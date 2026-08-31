package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"syscall"
	"time"

	"github.com/pkg/sftp"
)

// remoteBackend drives a herdr daemon on another host, reached entirely through
// the system `ssh` binary so the user's full ~/.ssh/config (ProxyJump,
// IdentityFile, agent, known_hosts) is honored exactly as it would be on the
// command line. One SSH ControlMaster connection is opened per remote host and
// everything multiplexes over it:
//
//   - herdr RPC + events: the remote herdr unix socket is forwarded (-L) to a
//     local socket, which the shared dial code (herdrCallSock / subscribeEvents)
//     points at — identical to the local case.
//   - file ops: the SFTP subsystem (`ssh host -s sftp`), driven by pkg/sftp.
//   - git: `ssh host "git -C <dir> ..."`.
//
// The left terminal and shell tab use herdr's own `herdr --remote <host>` and
// `ssh <host>` (wired in startTtyd via the active backend's TermCmd/ShellCmd).
type remoteBackend struct {
	alias      string // ssh-config host alias
	remoteSock string // absolute herdr socket path on the remote host
	ctlPath    string // ssh ControlMaster control socket (local)
	localSock  string // local end of the forwarded herdr socket
	home       string // remote $HOME, for ~-expansion
	herdrCfg   string // where herdr on the remote reads config.toml
	protocol   int    // remote herdr protocol (verified == local at connect)
	version    string // remote herdr version (for display)

	cancel context.CancelFunc // tears down the control master + sockets
	done   chan struct{}      // closed once teardown completes

	sftpMu  sync.Mutex
	sftpCl  *sftp.Client
	sftpCmd *exec.Cmd
}

// sshConnectTimeout bounds the initial control-master handshake; sshOpFastFail
// the per-op timeout for short commands that ride the established master.
const (
	sshConnectTimeout = 10 * time.Second
	sshForwardReady   = 4 * time.Second
	// sshOpTimeout bounds a command riding an established master (git, sqlite3,
	// $HOME lookups, control-socket ops). Every one of them is a sub-second call
	// on a healthy path; the bound exists for the unhealthy one, where ssh with
	// no ConnectTimeout and no ServerAlive of its own waits on a wedged master
	// forever. Unbounded, one such call parks its caller permanently: four
	// `git for-each-ref`s to a sleeping laptop sat in ssh for 13 hours and took
	// the cache warmer's wg.Wait — and so every later warm cycle — with them.
	sshOpTimeout = 30 * time.Second
	// sshWaitDelay bounds how long Wait blocks on I/O pipes after the ssh process
	// itself is gone. See sshCmd.
	sshWaitDelay = 2 * time.Second
)

// sshCmd builds an ssh command bounded by ctx. Two details make it safe to run
// from a request path, and neither is the default:
//
//   - The child gets its own process group and Cancel kills the GROUP, so a
//     ProxyCommand (ProxyJump, `cloudflared access ssh`) dies with the ssh that
//     spawned it instead of surviving as an orphan.
//   - WaitDelay caps how long Wait blocks after that. exec's output copiers read
//     until EOF, and EOF needs every holder of the pipe to close it — including
//     grandchildren ssh forked. Without it a timeout is a promise the context
//     cannot keep: the process is killed on schedule and CombinedOutput blocks
//     forever anyway, waiting on a pipe a surviving ProxyCommand still holds.
func sshCmd(ctx context.Context, args ...string) *exec.Cmd {
	return boundedCmd(ctx, "ssh", args...)
}

// boundedCmd is sshCmd's body, over any binary, so the guarantee can be tested
// without an ssh server.
func boundedCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = sshWaitDelay
	return cmd
}

// killProcessGroup SIGKILLs cmd's whole process group (see boundedCmd's
// Setpgid), so a ProxyCommand goes down with the ssh it was spawned for.
//
// The leader is killed through os.Process first, deliberately: that call refuses
// once Wait has reaped the pid, which is what keeps the raw group kill below
// from landing on a pgid the OS has since handed to someone else.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil {
		return err
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return nil
}

// sshBaseOpts are the options every ssh invocation for a remote backend carries.
// BatchMode makes anything that would prompt (password, unknown host key) fail
// fast instead of hanging the server. accept-new trusts a first-seen host key
// (the master command records it) without prompting.
//
// ControlMaster=auto matters for what happens when the master ISN'T up. The
// default (no) reuses a live socket but silently opens a full handshake when
// there's none — so a dead master turns one warm cycle into a burst of
// independent dials (warmHost fans out warmRepoConcurrency branch listings at
// once, on top of the repo listing and any probe). Whichever of those get
// cancelled die mid-authentication, and OpenSSH >= 9.8 scores an aborted
// handshake as authfail under PerSourcePenalties: the remote then drops every
// new connection from this IP for 15-600s, which surfaces as herdr's "remote
// platform detection failed: Connection closed by <ip> port 22". With auto, the
// first op to find no master becomes one and the rest of the cycle rides it.
//
// ControlPersist is bounded here, unlike the yes on the forward-carrying master
// in newRemoteBackend: a master created by this path has no -L forwards, so it
// can't serve the backend. The health check (herdrPing over the forwarded
// socket, see hostBackend) fails against it and redials, and newRemoteBackend
// unlinks the socket before dialing — leaving the ad-hoc master orphaned with no
// socket for -O exit to reach. A timed persist bounds that to a minute instead
// of leaking it until the sshreap loop or process exit.
func (b *remoteBackend) ctlOpts() []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ControlPath=" + b.ctlPath,
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist=60",
	}
}

// newRemoteBackend establishes the SSH control master + forwarded herdr socket
// for alias (whose remote herdr socket path is remoteSock, from the probe),
// verifies the forwarded socket answers `ping` with a protocol matching the
// local one, and resolves the remote home dir. parent is the root context: the
// backend tears itself down when parent is cancelled (process exit) or when
// Close is called (an idle-reaped or unhealthy pool entry). On any failure it
// cleans up and returns the error so the caller can roll back.
//
// The socket filenames carry lasso's pid and the alias, and nothing else: there
// is exactly one connection per host now that a host switch adopts the pooled
// one (see hostBackend), so no second backend to the same alias exists to
// clobber these paths.
func newRemoteBackend(parent context.Context, alias, remoteSock string, wantProtocol int) (*remoteBackend, error) {
	if remoteSock == "" {
		return nil, fmt.Errorf("no remote herdr socket for %s", alias)
	}
	return dialRemote(parent, alias, remoteSock, wantProtocol)
}

// newRemoteFileBackend opens a FILES-ONLY connection to alias: the same control
// master, SFTP and remote-command plumbing as newRemoteBackend, but no forwarded
// herdr socket and no protocol check. HerdrCall on it fails by construction, so
// a caller must treat talking to that host's herdr as optional.
//
// It exists for work that is pure file I/O on a host lasso cannot DRIVE: today
// the theme sync, which has to keep a machine's ghostty/Claude/opencode/omp
// theme files in step with herdr even when that machine runs a herdr whose
// protocol this build refuses to speak (see agentsync.go).
//
// It is never pooled and never becomes the active backend — the caller Closes
// it — and its control socket carries a "-files" suffix so it cannot collide
// with the one pooled master per host that hostBackend owns.
func newRemoteFileBackend(parent context.Context, alias string) (*remoteBackend, error) {
	return dialRemote(parent, alias, "", 0)
}

// dialRemote is the body both constructors share. An empty remoteSock means
// files-only: no -L forward, no readiness ping, no version/protocol.
func dialRemote(parent context.Context, alias, remoteSock string, wantProtocol int) (*remoteBackend, error) {
	filesOnly := remoteSock == ""
	tag := sanitizeAlias(alias)
	if filesOnly {
		tag += "-files"
	}
	b := &remoteBackend{
		alias:      alias,
		remoteSock: remoteSock,
		ctlPath:    filepath.Join(os.TempDir(), fmt.Sprintf("lasso-ctl-%d-%s.sock", os.Getpid(), tag)),
		done:       make(chan struct{}),
	}
	if !filesOnly {
		b.localSock = filepath.Join(os.TempDir(), fmt.Sprintf("lasso-herdr-%d-%s.sock", os.Getpid(), tag))
	}
	// Clear stale sockets a crashed prior run may have left so ssh can bind.
	_ = os.Remove(b.ctlPath)
	if b.localSock != "" {
		_ = os.Remove(b.localSock)
	}

	// Open the control master and the forwarded herdr socket. -fNT backgrounds
	// the master after authentication, so this returns once the forward is up.
	mctx, cancelDial := context.WithTimeout(parent, sshConnectTimeout)
	defer cancelDial()
	// ExitOnForwardFailure=no so a conflicting forward the user's config attaches
	// to this host (e.g. a busy-port tunnel) can't abort our master — our own
	// herdr-socket forward is verified separately by the ping readiness check.
	// ControlPersist=yes: the master's lifetime is the backend's lifetime —
	// Close/teardown kills it explicitly, and the sshreap loop cleans up after a
	// crashed lasso. A timed persist (formerly 60) let the master exit during any
	// quiet minute: the host pool holds no persistent channel (unlike the active
	// backend, whose event subscription reconnects every second), so pooled
	// masters routinely died between polls, taking the forwarded socket — and
	// every SFTP session on it — down with them.
	// ServerAlive* makes a genuinely dead path (network drop, sleep) kill the
	// master within ~60s so liveness checks fail fast and trigger a redial,
	// instead of every op hanging on a wedged TCP connection.
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ExitOnForwardFailure=no",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + b.ctlPath,
		"-o", "ControlPersist=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-fNT",
	}
	if !filesOnly {
		args = append(args, "-L", b.localSock+":"+b.remoteSock)
	}
	args = append(args, alias)
	out, err := sshCmd(mctx, args...).CombinedOutput()
	if err != nil {
		b.killMaster()
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ssh %s: %s", alias, msg)
	}

	// Wait for the forwarded socket to accept connections and answer ping with a
	// matching protocol (doubles as the compatibility re-check).
	if !filesOnly {
		ver, proto, perr := b.waitForSocket(parent, wantProtocol)
		if perr != nil {
			b.killMaster()
			return nil, perr
		}
		b.version, b.protocol = ver, proto
	}

	// Resolve the remote $HOME (for ~-expansion) and the env that decides where
	// herdr on that host reads config.toml, in ONE round trip on the fresh
	// master (both are cheap; see herdrConfigPath below).
	if out, herr := b.runOut(`printf '%s\n%s\n%s\n' "$HOME" "$XDG_CONFIG_HOME" "$HERDR_CONFIG_PATH"`); herr == nil {
		lines := strings.Split(out, "\n")
		for len(lines) < 3 {
			lines = append(lines, "")
		}
		b.home = strings.TrimSpace(lines[0])
		b.herdrCfg = herdrConfigIn(strings.TrimSpace(lines[2]), strings.TrimSpace(lines[1]), b.home)
	}

	// Tie teardown to the root context so process exit (Ctrl-C) cleans up every
	// remote backend's master + sockets, mirroring the ttyd cleanup discipline.
	ctx, cancel := context.WithCancel(parent)
	b.cancel = cancel
	go func() {
		<-ctx.Done()
		b.teardown()
	}()
	if filesOnly {
		log.Printf("host:     connected to %s for files only (no herdr socket)", alias)
	} else {
		log.Printf("host:     connected to %s (herdr %s, protocol %d) via %s", alias, b.version, b.protocol, b.localSock)
	}
	return b, nil
}

// waitForSocket polls the forwarded local socket until it answers ping (or the
// readiness window elapses), returning the remote herdr version/protocol. It
// fails if the protocol doesn't match wantProtocol — a host that changed or
// downgraded since discovery.
func (b *remoteBackend) waitForSocket(ctx context.Context, wantProtocol int) (string, int, error) {
	deadline := time.Now().Add(sshForwardReady)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		ver, proto, err := herdrPing(b.localSock)
		if err == nil {
			if wantProtocol != 0 && proto != wantProtocol {
				return "", 0, fmt.Errorf("protocol mismatch: %s speaks %d, this lasso speaks %d", b.alias, proto, wantProtocol)
			}
			return ver, proto, nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out")
	}
	return "", 0, fmt.Errorf("herdr socket on %s not reachable: %v", b.alias, lastErr)
}

func (b *remoteBackend) Name() string { return b.alias }

// HerdrSock is empty on a files-only connection (newRemoteFileBackend), which is
// how a caller tells there is no herdr on the far end to talk to.
func (b *remoteBackend) HerdrSock() string { return b.localSock }

func (b *remoteBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	if b.localSock == "" {
		return nil, fmt.Errorf("no herdr connection to %s (files-only)", b.alias)
	}
	return herdrCallSock(b.localSock, method, params)
}

// runOut runs a shell command string on the remote host over the control master
// and returns stdout, surfacing stderr in the error.
func (b *remoteBackend) runOut(remoteCmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sshOpTimeout)
	defer cancel()
	args := append(b.ctlOpts(), b.alias, remoteCmd)
	cmd := sshCmd(ctx, args...)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("ssh %s: timed out after %v", b.alias, sshOpTimeout)
		}
		if ee, ok := err.(*exec.ExitError); ok {
			if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
				return "", fmt.Errorf("%s", msg)
			}
		}
		return "", err
	}
	return string(out), nil
}

// runStdin runs remoteCmd on the host over the control master, piping stdin and
// returning stdout; stderr is surfaced on error. Used by the per-host settings
// provider to drive the remote's sqlite3 against its own ~/.lasso/lasso.db (the
// SQL rides stdin, never the shell). remoteCmd is the full remote command line
// (callers wrap it in a login shell when PATH matters).
func (b *remoteBackend) runStdin(remoteCmd string, stdin []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sshOpTimeout)
	defer cancel()
	sshArgs := append(b.ctlOpts(), b.alias, remoteCmd)
	cmd := sshCmd(ctx, sshArgs...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ssh %s: timed out after %v", b.alias, sshOpTimeout)
		}
		if msg := strings.TrimSpace(errBuf.String()); msg != "" {
			return nil, fmt.Errorf("%s: %s", b.alias, msg)
		}
		return nil, fmt.Errorf("ssh %s: %w", b.alias, err)
	}
	return out, nil
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

// herdrConfigPath is the config.toml the remote herdr READS — resolved from that
// host's own environment at connect (see newRemoteBackend), because the answer
// is the remote's, not ours. Empty only if the probe failed, in which case the
// caller has nothing safe to write and should skip.
func (b *remoteBackend) herdrConfigPath() string { return b.herdrCfg }

func (b *remoteBackend) HomeDir() (string, error) {
	if b.home != "" {
		return b.home, nil
	}
	return "", fmt.Errorf("remote home unknown")
}

// PasteFileDir mirrors the local drop dir but on the remote host, so the path
// typed into the remote terminal resolves there.
func (b *remoteBackend) PasteFileDir() string {
	home := b.home
	if home == "" {
		home = "/tmp"
	}
	return path.Join(home, ".lasso", "uploads", "dropped-files")
}

// ---------------------------------------------------------------------------
// SFTP-backed filesystem ops
// ---------------------------------------------------------------------------

// sftpClient lazily opens the SFTP subsystem over the control master and caches
// the client. The cache self-clears when the underlying ssh process dies (a
// watcher goroutine reaps it), so a master teardown or network drop costs one
// failed op at worst — the next op dials a fresh subsystem instead of failing
// forever with "file already closed".
func (b *remoteBackend) sftpClient() (*sftp.Client, error) {
	b.sftpMu.Lock()
	defer b.sftpMu.Unlock()
	if b.sftpCl != nil {
		return b.sftpCl, nil
	}
	args := append(b.ctlOpts(), b.alias, "-s", "sftp")
	cmd := exec.Command("ssh", args...)
	// Own process group, as sshCmd does: this ssh outlives the call, and killing
	// it must take any ProxyCommand with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	// NewClientPipe reads the server's SSH_FXP_VERSION, which on a wedged master
	// never arrives — and this runs under sftpMu, the same lock teardown takes.
	// Unbounded, one hung handshake blocks teardown, which blocks Close, which
	// blocks hostPoolDrop while it holds the host's dial mutex, which hangs every
	// later connection attempt to that host. Bound it and the wedge is one failed
	// op instead.
	type dialed struct {
		cl  *sftp.Client
		err error
	}
	ch := make(chan dialed, 1)
	go func() {
		cl, err := sftp.NewClientPipe(rd, wr)
		ch <- dialed{cl, err}
	}()
	var cl *sftp.Client
	select {
	case d := <-ch:
		cl, err = d.cl, d.err
	case <-time.After(sshOpTimeout):
		err = fmt.Errorf("timed out after %v", sshOpTimeout)
	}
	if err != nil {
		_ = killProcessGroup(cmd)
		_ = cmd.Wait()
		return nil, fmt.Errorf("sftp on %s: %w", b.alias, err)
	}
	b.sftpCl, b.sftpCmd = cl, cmd
	// Sole reaper for this cmd (teardown/dropSFTP only Kill): waits for the ssh
	// subprocess to exit — however it dies — and drops it from the cache if it's
	// still the cached one.
	go func() {
		_ = cmd.Wait()
		b.sftpMu.Lock()
		if b.sftpCmd == cmd {
			_ = b.sftpCl.Close()
			b.sftpCl, b.sftpCmd = nil, nil
		}
		b.sftpMu.Unlock()
	}()
	return cl, nil
}

// dropSFTP evicts cl from the cache (if still cached) and kills its transport so
// the watcher reaps it. Called when an op fails at the connection level while
// the process hasn't exited yet (e.g. a wedged path the ServerAlive probes
// haven't condemned).
func (b *remoteBackend) dropSFTP(cl *sftp.Client) {
	b.sftpMu.Lock()
	if b.sftpCl == cl {
		_ = b.sftpCl.Close()
		if b.sftpCmd != nil {
			_ = killProcessGroup(b.sftpCmd)
		}
		b.sftpCl, b.sftpCmd = nil, nil
	}
	b.sftpMu.Unlock()
}

// sftpDo runs op against the cached SFTP client, reopening and retrying once
// when the failure is connection-level (the transport died mid-use) rather than
// a real filesystem error. Ops routed through it must be safe to re-run.
func (b *remoteBackend) sftpDo(op func(cl *sftp.Client) error) error {
	for attempt := 0; ; attempt++ {
		cl, err := b.sftpClient()
		if err != nil {
			return err
		}
		err = op(cl)
		if err == nil || attempt > 0 || !sftpConnDead(err) {
			return err
		}
		b.dropSFTP(cl)
	}
}

// sftpConnDead reports whether err means the SFTP transport itself is gone (as
// opposed to a normal filesystem error like ENOENT). The string checks cover
// the errors pkg/sftp wraps without a matchable sentinel: writes to the closed
// stdin pipe surface as "file already closed" / "broken pipe", dead reads as
// "connection lost".
func sftpConnDead(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sftp.ErrSSHFxConnectionLost) || errors.Is(err, os.ErrClosed) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connection lost") ||
		strings.Contains(s, "file already closed") ||
		strings.Contains(s, "broken pipe")
}

func (b *remoteBackend) ReadDir(p string) ([]fileEntry, error) {
	var out []fileEntry
	err := b.sftpDo(func(cl *sftp.Client) error {
		infos, err := cl.ReadDir(p)
		if err != nil {
			return err
		}
		out = make([]fileEntry, 0, len(infos))
		for _, info := range infos {
			fe := fileEntry{Name: info.Name(), Dir: info.IsDir()}
			if !info.IsDir() {
				fe.Size = info.Size()
			}
			out = append(out, fe)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (b *remoteBackend) Stat(p string) (fs.FileInfo, error) {
	var info fs.FileInfo
	err := b.sftpDo(func(cl *sftp.Client) error {
		var err error
		info, err = cl.Stat(p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

func (b *remoteBackend) Lstat(p string) (fs.FileInfo, error) {
	var info fs.FileInfo
	err := b.sftpDo(func(cl *sftp.Client) error {
		var err error
		info, err = cl.Lstat(p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

func (b *remoteBackend) Open(p string) (io.ReadSeekCloser, error) {
	var f *sftp.File
	err := b.sftpDo(func(cl *sftp.Client) error {
		var err error
		f, err = cl.Open(p)
		return err
	})
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

// WriteFile re-runs the whole create+write+chmod sequence on a connection-level
// failure — Create truncates, so the retry rewrites the file from the start.
func (b *remoteBackend) WriteFile(p string, data []byte, perm fs.FileMode) error {
	return b.sftpDo(func(cl *sftp.Client) error {
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
	})
}

func (b *remoteBackend) Create(p string) (io.WriteCloser, error) {
	var f *sftp.File
	err := b.sftpDo(func(cl *sftp.Client) error {
		var err error
		f, err = cl.Create(p) // *sftp.File is an io.WriteCloser
		return err
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (b *remoteBackend) MkdirAll(p string, _ fs.FileMode) error {
	return b.sftpDo(func(cl *sftp.Client) error { return cl.MkdirAll(p) })
}

func (b *remoteBackend) Rename(oldpath, newpath string) error {
	return b.sftpDo(func(cl *sftp.Client) error { return cl.Rename(oldpath, newpath) })
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
		<-b.done   // wait for cleanup so callers can rely on sockets being gone
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
	if b.sftpCmd != nil {
		// Kill only — the watcher goroutine in sftpClient owns the Wait.
		_ = killProcessGroup(b.sftpCmd)
		b.sftpCmd = nil
	}
	b.sftpMu.Unlock()
	b.killMaster()
	log.Printf("host:     disconnected from %s", b.alias)
}

// killMaster gracefully stops the SSH control master and removes the local
// socket files. Safe to call even if the master never came up. Bounded, because
// teardown runs on paths that hold locks — hostPoolDrop calls Close with the
// host's dial mutex held — and `-O exit` against a wedged control socket has no
// timeout of its own.
func (b *remoteBackend) killMaster() {
	ctx, cancel := context.WithTimeout(context.Background(), sshOpTimeout)
	defer cancel()
	_ = sshCmd(ctx, "-o", "ControlPath="+b.ctlPath, "-O", "exit", b.alias).Run()
	_ = os.Remove(b.ctlPath)
	_ = os.Remove(b.localSock)
}

// TermCmd / ShellCmd / TermEnv give the per-host commands the two ttyd
// terminals run. A remote backend attaches the left terminal through herdr's own
// SSH remote mode and opens the right shell tab as a plain ssh session. The left
// terminal runs with the HERDR_* session markers stripped (outsideHerdrEnv) so
// `herdr --remote` doesn't think it's nested inside the local session.
func (b *remoteBackend) TermCmd() string   { return herdrBinary() + " --remote " + b.alias }
func (b *remoteBackend) ShellCmd() string  { return "ssh " + b.alias }
func (b *remoteBackend) TermEnv() []string { return outsideHerdrEnv() }

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
