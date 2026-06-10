package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

// remoteBackend drives another host over the system `ssh` binary, so the user's
// full ~/.ssh/config (ProxyJump, IdentityFile, agent, known_hosts) is honored
// exactly as on the command line. One SSH ControlMaster connection is opened per
// host and everything multiplexes over it:
//
//   - tmux: per-command `ssh host 'tmux -S ~/.lasso/tmux.sock -f /dev/null …'`
//     (TmuxArgv) and the viewport's interactive `ssh -tt host 'tmux … attach'`
//     (TmuxAttachArgv) — the herdr daemon is gone; lasso drives the remote tmux
//     server directly.
//   - file ops: the SFTP subsystem (`ssh host -s sftp`), driven by pkg/sftp.
//   - git: `ssh host "git -C <dir> …"`.
type remoteBackend struct {
	alias   string // ssh-config host alias
	ctlPath string // ssh ControlMaster control socket (local)
	home    string // remote $HOME, for ~-expansion and the remote tmux socket

	cancel context.CancelFunc // tears down the control master
	done   chan struct{}      // closed once teardown completes

	sftpMu  sync.Mutex
	sftpCl  *sftp.Client
	sftpCmd *exec.Cmd
}

const sshConnectTimeout = 12 * time.Second

// ctlOpts are the options every short ssh command for this backend carries: fail
// fast rather than prompt, and ride the established control master.
func (b *remoteBackend) ctlOpts() []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ControlPath=" + b.ctlPath,
	}
}

// newRemoteBackend establishes the SSH control master for alias and resolves the
// remote home dir. parent is the root context: the backend tears itself down when
// parent is cancelled (process exit) or Close is called (host switch / reap).
func newRemoteBackend(parent context.Context, alias string) (*remoteBackend, error) {
	if alias == "" {
		return nil, fmt.Errorf("empty host alias")
	}
	if parent == nil {
		parent = context.Background()
	}
	b := &remoteBackend{
		alias:   alias,
		ctlPath: filepath.Join(os.TempDir(), fmt.Sprintf("lasso-ctl-%d-%s.sock", os.Getpid(), sanitizeAlias(alias))),
		done:    make(chan struct{}),
	}
	_ = os.Remove(b.ctlPath) // clear a stale socket a crashed prior run left

	// Open the control master. -fNT backgrounds it after authentication, so this
	// returns once the connection is up; later ssh commands reuse it (no re-auth).
	mctx, cancelDial := context.WithTimeout(parent, sshConnectTimeout)
	defer cancelDial()
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + b.ctlPath,
		"-o", "ControlPersist=60",
		// Drop any port forwards the user's ssh config attaches to this host —
		// lasso only runs commands, and an already-bound LocalForward would fail
		// the bind on every connection (noise at best).
		"-o", "ClearAllForwardings=yes",
		"-fNT",
		alias,
	}
	out, err := exec.CommandContext(mctx, "ssh", args...).CombinedOutput()
	if err != nil {
		b.killMaster()
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ssh %s: %s", alias, msg)
	}

	// Resolve the remote home (rides the master, cheap) — needed to build the
	// remote tmux socket path and to ~-expand paths.
	if home, herr := b.runOut(`printf %s "$HOME"`); herr == nil {
		b.home = strings.TrimSpace(home)
	}

	ctx, cancel := context.WithCancel(parent)
	b.cancel = cancel
	go func() {
		<-ctx.Done()
		b.teardown()
	}()
	log.Printf("host:     connected to %s (home %s)", alias, b.home)
	return b, nil
}

func (b *remoteBackend) Name() string { return b.alias }

// remoteTmuxSock is the lasso tmux socket path on the remote host. Resolved from
// the cached remote home; falls back to the literal "$HOME/…" (the remote shell
// expands it) when the home probe failed.
func (b *remoteBackend) remoteTmuxSock() string {
	if b.home != "" {
		return shellQuote(b.home + "/.lasso/tmux.sock")
	}
	return `"$HOME/.lasso/tmux.sock"`
}

// remotePathExport widens PATH at the start of every remote command. A
// non-interactive ssh shell gets the sshd default PATH, which misses Homebrew
// (/opt/homebrew/bin on macOS hosts like minime/gigachad) and ~/.local/bin —
// where tmux and the agent CLIs actually live — so a bare `tmux` would be
// "command not found" there. The remote shell expands $PATH/$HOME; this string
// is never interpreted locally (exec.Command, no local shell).
const remotePathExport = `export PATH="$PATH:/opt/homebrew/bin:/usr/local/bin:$HOME/.local/bin"; `

// TmuxArgv builds a non-interactive remote tmux command, multiplexed over the
// control master. Each arg is shell-quoted; the tmux command-sequence separator
// ";" survives as its own quoted token (the remote shell hands tmux a literal ";"
// argv element, which tmux treats as a separator).
func (b *remoteBackend) TmuxArgv(args []string) []string {
	cmd := remotePathExport + "tmux -S " + b.remoteTmuxSock() + " -f /dev/null"
	for _, a := range args {
		cmd += " " + shellQuote(a)
	}
	argv := append([]string{"ssh"}, b.ctlOpts()...)
	return append(argv, b.alias, cmd)
}

// TmuxAttachArgv is the interactive viewport attach: ssh -tt forces a remote PTY
// (over the control master) so the attached TUI renders, then attaches to the
// session on the remote lasso tmux server.
func (b *remoteBackend) TmuxAttachArgv(session string) []string {
	cmd := remotePathExport + "tmux -S " + b.remoteTmuxSock() + " -f /dev/null attach -t " + shellQuote(session)
	return []string{
		"ssh", "-tt",
		"-o", "BatchMode=yes",
		"-o", "ControlPath=" + b.ctlPath,
		b.alias, cmd,
	}
}

// runOut runs a shell command string on the remote host over the control master
// and returns stdout, surfacing stderr in the error.
func (b *remoteBackend) runOut(remoteCmd string) (string, error) {
	args := append(b.ctlOpts(), b.alias, remotePathExport+remoteCmd)
	out, err := exec.Command("ssh", args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
				return "", fmt.Errorf("%s", msg)
			}
		}
		return "", err
	}
	return string(out), nil
}

func (b *remoteBackend) GitOut(dir string, args ...string) (string, error) {
	// Build a single shell-quoted command so paths with spaces survive ssh's
	// argv-join into one remote shell string.
	parts := make([]string, 0, len(args)+3)
	parts = append(parts, "git", "-C", shellQuote(dir))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return b.runOut(strings.Join(parts, " "))
}

func (b *remoteBackend) HomeDir() (string, error) {
	if b.home != "" {
		return b.home, nil
	}
	return "", fmt.Errorf("remote home unknown")
}

// PasteImageDir mirrors the local uploads dir but on the remote host, so the path
// typed into the remote terminal resolves there.
func (b *remoteBackend) PasteImageDir() string {
	home := b.home
	if home == "" {
		home = "/tmp"
	}
	return path.Join(home, ".lasso", "uploads", "pasted-images")
}

// ---------------------------------------------------------------------------
// SFTP-backed filesystem ops
// ---------------------------------------------------------------------------

// sftpClient lazily opens the SFTP subsystem over the control master and caches
// the client for the backend's lifetime.
func (b *remoteBackend) sftpClient() (*sftp.Client, error) {
	b.sftpMu.Lock()
	defer b.sftpMu.Unlock()
	if b.sftpCl != nil {
		return b.sftpCl, nil
	}
	args := append(b.ctlOpts(), b.alias, "-s", "sftp")
	cmd := exec.Command("ssh", args...)
	wr, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	rd, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	cl, err := sftp.NewClientPipe(rd, wr)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("sftp on %s: %w", b.alias, err)
	}
	b.sftpCl, b.sftpCmd = cl, cmd
	return cl, nil
}

func (b *remoteBackend) ReadDir(p string) ([]fileEntry, error) {
	cl, err := b.sftpClient()
	if err != nil {
		return nil, err
	}
	infos, err := cl.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]fileEntry, 0, len(infos))
	for _, info := range infos {
		fe := fileEntry{Name: info.Name(), Dir: info.IsDir()}
		if !info.IsDir() {
			fe.Size = info.Size()
		}
		out = append(out, fe)
	}
	return out, nil
}

func (b *remoteBackend) Stat(p string) (fs.FileInfo, error) {
	cl, err := b.sftpClient()
	if err != nil {
		return nil, err
	}
	return cl.Stat(p)
}

func (b *remoteBackend) Lstat(p string) (fs.FileInfo, error) {
	cl, err := b.sftpClient()
	if err != nil {
		return nil, err
	}
	return cl.Lstat(p)
}

func (b *remoteBackend) Open(p string) (io.ReadSeekCloser, error) {
	cl, err := b.sftpClient()
	if err != nil {
		return nil, err
	}
	f, err := cl.Open(p)
	if err != nil {
		return nil, err
	}
	return f, nil // *sftp.File implements io.ReadSeekCloser
}

func (b *remoteBackend) ReadFile(p string) ([]byte, error) {
	f, err := b.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (b *remoteBackend) WriteFile(p string, data []byte, perm fs.FileMode) error {
	cl, err := b.sftpClient()
	if err != nil {
		return err
	}
	f, err := cl.Create(p) // O_RDWR|O_CREATE|O_TRUNC
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return cl.Chmod(p, perm)
}

func (b *remoteBackend) Create(p string) (io.WriteCloser, error) {
	cl, err := b.sftpClient()
	if err != nil {
		return nil, err
	}
	return cl.Create(p) // *sftp.File is an io.WriteCloser
}

func (b *remoteBackend) MkdirAll(p string, _ fs.FileMode) error {
	cl, err := b.sftpClient()
	if err != nil {
		return err
	}
	return cl.MkdirAll(p)
}

func (b *remoteBackend) Rename(oldpath, newpath string) error {
	cl, err := b.sftpClient()
	if err != nil {
		return err
	}
	return cl.Rename(oldpath, newpath)
}

// RemoveAll removes recursively. SFTP has no recursive delete, so shell out to
// `rm -rf` over the master — robust and matches os.RemoveAll's semantics.
func (b *remoteBackend) RemoveAll(p string) error {
	_, err := b.runOut("rm -rf -- " + shellQuote(p))
	return err
}

// ---------------------------------------------------------------------------
// lifecycle
// ---------------------------------------------------------------------------

func (b *remoteBackend) Close() error {
	if b.cancel != nil {
		b.cancel() // triggers teardown via the parent-context goroutine
		<-b.done   // wait for cleanup so callers can rely on the master being gone
	}
	return nil
}

func (b *remoteBackend) teardown() {
	defer close(b.done)
	b.sftpMu.Lock()
	if b.sftpCl != nil {
		_ = b.sftpCl.Close()
		b.sftpCl = nil
	}
	if b.sftpCmd != nil && b.sftpCmd.Process != nil {
		_ = b.sftpCmd.Process.Kill()
		_ = b.sftpCmd.Wait()
		b.sftpCmd = nil
	}
	b.sftpMu.Unlock()
	b.killMaster()
	log.Printf("host:     disconnected from %s", b.alias)
}

// killMaster gracefully stops the SSH control master and removes the socket file.
// Safe to call even if the master never came up.
func (b *remoteBackend) killMaster() {
	_ = exec.Command("ssh", "-o", "ControlPath="+b.ctlPath, "-O", "exit", b.alias).Run()
	_ = os.Remove(b.ctlPath)
}

var _ Backend = (*remoteBackend)(nil)

// shellQuote wraps s in single quotes, escaping embedded single quotes, so it
// survives the remote shell as one argument.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sanitizeAlias makes a host alias safe to embed in a socket filename.
func sanitizeAlias(alias string) string {
	var b strings.Builder
	for _, r := range alias {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	s := b.String()
	if s == "" {
		s = "host"
	}
	return s
}
