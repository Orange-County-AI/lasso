package main

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"os"
	"sync"
	"time"
)

// Backend is one host lasso can drive: the local machine (localBackend) or a
// herdr daemon on another box reached over SSH (remoteBackend). Every
// per-request handler — herdr RPC, file browsing/editing, git diff, the paste
// scratch dir — runs against the backend its REQUEST resolves to (reqBackend,
// reqhost.go), because "which host" is now a property of the calling browser tab
// rather than of the process. hostBackend/namedHostBackend own the one
// connection per host that they all share.
type Backend interface {
	// Name is "local" or the ssh-config host alias.
	Name() string

	// HerdrSock is the unix socket to dial for herdr RPC and the event stream:
	// the local socket for localBackend, the SSH-forwarded local socket for
	// remoteBackend. subscribeEvents reads it each time it (re)connects.
	HerdrSock() string
	// HerdrCall does one request/response round-trip against HerdrSock.
	HerdrCall(method string, params any) (json.RawMessage, error)

	// Filesystem ops mirror the os.* calls the file handlers used to make.
	// Local impls hit os directly; remote impls go over SFTP.
	ReadDir(path string) ([]fileEntry, error)
	Stat(path string) (fs.FileInfo, error)
	Lstat(path string) (fs.FileInfo, error)
	Open(path string) (io.ReadSeekCloser, error) // http.ServeContent needs Seek
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm fs.FileMode) error
	Create(path string) (io.WriteCloser, error) // truncating create, for streamed uploads
	MkdirAll(path string, perm fs.FileMode) error
	RemoveAll(path string) error
	Rename(oldpath, newpath string) error

	// GitOut runs `git -C dir args...` on the host and returns stdout.
	GitOut(dir string, args ...string) (string, error)

	// TermCmd / ShellCmd are the commands the two ttyd terminals run for this
	// host (left "Herdr" terminal and the right shell tab). TermEnv overrides the
	// left terminal's environment (nil = inherit the viewer's env).
	TermCmd() string
	ShellCmd() string
	TermEnv() []string

	// HomeDir is the host's home directory, for ~-expansion in path inputs.
	HomeDir() (string, error)
	// PasteImageDir is where a pasted clipboard image is written so the path
	// typed into the (possibly remote) terminal resolves on the same host.
	PasteImageDir() string

	// Close releases any resources (SSH control master, sftp client, forwarded
	// socket). A no-op for localBackend.
	Close() error
}

// active holds the DEFAULT backend: the host lasso booted on (local, or -host),
// which answers for any caller that names none — a fresh browser tab before it
// has chosen, an MCP whoami, a background job. It is immutable at runtime; a tab
// selecting another host changes only that tab, never this. (Tests swap it via
// setDefaultBackend to stand a fake host up as the default.)
//
// It used to be "the active host", swapped by POST /api/host, which is precisely
// what made two tabs on two machines impossible: the second tab's switch moved
// the first tab's terminal, pane list, and file viewer out from under it.
var active struct {
	mu sync.RWMutex
	b  Backend
}

func defaultBackend() Backend {
	active.mu.RLock()
	defer active.mu.RUnlock()
	return active.b
}

// defaultHostName is the default host's name, or "local" before one is
// installed (a CLI subcommand, a test) — for callers that want a name to report
// rather than a connection to use.
func defaultHostName() string {
	if b := defaultBackend(); b != nil {
		return b.Name()
	}
	return "local"
}

func setDefaultBackend(b Backend) {
	active.mu.Lock()
	active.b = b
	active.mu.Unlock()
}

// herdrReadTimeout is how long we wait for a herdr method's response. Most calls
// are cheap reads that should fail fast so the UI stays snappy, but the worktree
// and workspace mutations shell out to git — `git worktree add` alone runs several
// seconds on a large repo, more once it's crossing an ssh-forwarded socket to a
// remote host. Holding those to the read default made createAgent's worktree.create
// time out and surface as a 502 from the New Agent modal.
const herdrReadTimeout = 3 * time.Second

var herdrSlowMethods = map[string]time.Duration{
	"worktree.create":  120 * time.Second,
	"worktree.remove":  120 * time.Second,
	"workspace.create": 120 * time.Second,
	"workspace.rename": 30 * time.Second,
	"pane.close":       30 * time.Second,
}

func herdrTimeoutFor(method string) time.Duration {
	if d, ok := herdrSlowMethods[method]; ok {
		return d
	}
	return herdrReadTimeout
}

// herdrCallSock does one newline-delimited JSON request/response round-trip on a
// fresh connection to sock. This is the body the old package-level herdrCall
// used; both backends share it (local socket vs forwarded remote socket).
func herdrCallSock(sock, method string, params any) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	req := map[string]any{"id": "ui", "method": method, "params": params}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(herdrTimeoutFor(method)))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		he := &herdrError{}
		if json.Unmarshal(resp.Error, he) != nil || he.Code == "" {
			he.Message = string(resp.Error) // non-structured error: keep the raw payload
		}
		return nil, he
	}
	return resp.Result, nil
}

// herdrPing dials sock and issues the `ping` method, returning herdr's protocol
// version. Used to confirm a (local or forwarded) socket is a live, compatible
// herdr server before declaring a host switch successful.
func herdrPing(sock string) (version string, protocol int, err error) {
	res, err := herdrCallSock(sock, "ping", map[string]any{})
	if err != nil {
		return "", 0, err
	}
	var pong struct {
		Version  string `json:"version"`
		Protocol int    `json:"protocol"`
	}
	if err := json.Unmarshal(res, &pong); err != nil {
		return "", 0, err
	}
	return pong.Version, pong.Protocol, nil
}

// ---------------------------------------------------------------------------
// localBackend — the machine lasso runs on (the historical, default behavior)
// ---------------------------------------------------------------------------

type localBackend struct {
	sock string // herdr unix socket (defaults to *herdrSock)
}

func (b *localBackend) Name() string      { return "local" }
func (b *localBackend) HerdrSock() string { return b.sock }

func (b *localBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	return herdrCallSock(b.sock, method, params)
}

func (b *localBackend) ReadDir(path string) ([]fileEntry, error) {
	ents, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]fileEntry, 0, len(ents))
	for _, e := range ents {
		fe := fileEntry{Name: e.Name(), Dir: e.IsDir()}
		if !e.IsDir() {
			if info, err := e.Info(); err == nil {
				fe.Size = info.Size()
			}
		}
		out = append(out, fe)
	}
	return out, nil
}

func (b *localBackend) Stat(path string) (fs.FileInfo, error)  { return os.Stat(path) }
func (b *localBackend) Lstat(path string) (fs.FileInfo, error) { return os.Lstat(path) }

func (b *localBackend) Open(path string) (io.ReadSeekCloser, error) { return os.Open(path) }
func (b *localBackend) ReadFile(path string) ([]byte, error)        { return os.ReadFile(path) }

func (b *localBackend) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(path, data, perm)
}
func (b *localBackend) Create(path string) (io.WriteCloser, error)   { return os.Create(path) }
func (b *localBackend) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }
func (b *localBackend) RemoveAll(path string) error                  { return os.RemoveAll(path) }
func (b *localBackend) Rename(oldpath, newpath string) error         { return os.Rename(oldpath, newpath) }

func (b *localBackend) GitOut(dir string, args ...string) (string, error) {
	return gitOutLocal(dir, args...) // the local `git -C dir ...` exec lives in main.go
}

func (b *localBackend) HomeDir() (string, error) { return os.UserHomeDir() }
func (b *localBackend) PasteImageDir() string    { return pasteImageDir() }

// TermCmd/ShellCmd/TermEnv reproduce the historical local terminal wiring: the
// left terminal runs *termCmd inheriting the viewer's env (so it joins the same
// herdr session); the shell tab runs the resolved shell. TermEnv is nil here —
// startTtyd inherits the viewer env. (The shell tab's env-stripping is applied
// by the caller via outsideHerdrEnv, unchanged.)
func (b *localBackend) TermCmd() string   { return *termCmd }
func (b *localBackend) ShellCmd() string  { return shellCommand() }
func (b *localBackend) TermEnv() []string { return nil }

func (b *localBackend) Close() error { return nil }

var _ Backend = (*localBackend)(nil)
