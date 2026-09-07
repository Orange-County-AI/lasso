package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"sync"
	"time"
)

// Backend is one host lasso can drive: the local machine (localBackend) or a
// luvus daemon on another box reached over SSH (remoteBackend). Every
// per-request handler — luvus RPC, file browsing/editing, git diff, the paste
// scratch dir — runs against the backend its REQUEST resolves to (reqBackend,
// reqhost.go), because "which host" is now a property of the calling browser tab
// rather than of the process. hostBackend/namedHostBackend own the one
// connection per host that they all share.
type Backend interface {
	// Name is "local" or the ssh-config host alias.
	Name() string

	// LuvusSock is the unix socket to dial for luvus RPC and the event stream:
	// the local socket for localBackend, the SSH-forwarded local socket for
	// remoteBackend. subscribeEvents reads it each time it (re)connects.
	LuvusSock() string
	// LuvusCall does one request/response round-trip against LuvusSock.
	LuvusCall(method string, params any) (json.RawMessage, error)

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
	// host (left "Luvus" terminal and the right shell tab). TermEnv overrides the
	// left terminal's environment (nil = inherit the viewer's env).
	TermCmd() string
	ShellCmd() string
	TermEnv() []string

	// HomeDir is the host's home directory, for ~-expansion in path inputs.
	HomeDir() (string, error)
	// PasteFileDir is where a file handed over by the browser — a pasted
	// screenshot, a photo or document picked on a phone — is written, so the
	// path typed into the (possibly remote) terminal resolves on the same host.
	PasteFileDir() string

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

// Cheap reads fail fast; workspace/PTY creation and teardown have separate
// deadlines so a slow host does not turn a successful mutation into a retry.
const luvusReadTimeout = 3 * time.Second

var luvusSlowMethods = map[string]time.Duration{
	"workspace.open":          120 * time.Second,
	"terminal.backend.create": 120 * time.Second,
	"workspace.rename":        30 * time.Second,
	"pane.close":              30 * time.Second,
}

func luvusTimeoutFor(method string) time.Duration {
	if d, ok := luvusSlowMethods[method]; ok {
		return d
	}
	return luvusReadTimeout
}

// luvusCallSock does one newline-delimited JSON request/response round-trip on a
// fresh connection to sock. This is the body the old package-level luvusCall
// used; both backends share it (local socket vs forwarded remote socket).
func luvusCallSock(sock, method string, params any) (json.RawMessage, error) {
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
	_ = conn.SetReadDeadline(time.Now().Add(luvusTimeoutFor(method)))
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
		he := &luvusError{}
		if json.Unmarshal(resp.Error, he) != nil || he.Code == "" {
			he.Message = string(resp.Error) // non-structured error: keep the raw payload
		}
		return nil, he
	}
	return resp.Result, nil
}

// luvusPing verifies the public UHP contract, not the private TUI protocol.
func luvusPing(sock string) (version string, protocol int, err error) {
	res, err := luvusCallSock(sock, "uhp.capabilities", map[string]any{})
	if err != nil {
		return "", 0, err
	}
	var caps uhpCaps
	if err := json.Unmarshal(res, &caps); err != nil {
		return "", 0, err
	}
	if err := validateLuvusCaps(caps); err != nil {
		return "", caps.Protocol.Major, err
	}
	res, err = luvusCallSock(sock, "ping", map[string]any{})
	if err != nil {
		return "", caps.Protocol.Major, err
	}
	var pong struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(res, &pong); err != nil {
		return "", caps.Protocol.Major, err
	}
	return pong.Version, caps.Protocol.Major, nil
}

var requiredLuvusMethods = []string{
	"session.snapshot", "workspace.list", "workspace.open", "workspace.rename",
	"workspace.focus", "workspace.close", "tab.new", "tab.rename",
	"pane.get", "pane.current", "pane.focus", "pane.run", "pane.send_input", "pane.read",
	"pane.close", "terminal.backend.create", "events.subscribe", "theme.list",
}

type uhpCaps struct {
	Protocol struct {
		Name  string `json:"name"`
		Major int    `json:"major"`
		Minor int    `json:"minor"`
	} `json:"protocol"`
	Methods []string `json:"methods"`
}

func validateLuvusCaps(c uhpCaps) error {
	if c.Protocol.Name != "luvus-uhp" || c.Protocol.Major != lassoLuvusProtocol {
		return fmt.Errorf("incompatible UHP protocol %q major %d", c.Protocol.Name, c.Protocol.Major)
	}
	methods := make(map[string]bool, len(c.Methods))
	for _, m := range c.Methods {
		methods[m] = true
	}
	for _, m := range requiredLuvusMethods {
		if !methods[m] {
			return fmt.Errorf("Luvus lacks required UHP method %s", m)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// localBackend — the machine lasso runs on (the historical, default behavior)
// ---------------------------------------------------------------------------

type localBackend struct {
	sock string // luvus unix socket (defaults to *luvusSock)
}

func (b *localBackend) Name() string      { return "local" }
func (b *localBackend) LuvusSock() string { return b.sock }

func (b *localBackend) LuvusCall(method string, params any) (json.RawMessage, error) {
	return luvusCallSock(b.sock, method, params)
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
func (b *localBackend) PasteFileDir() string     { return pasteFileDir() }

// TermCmd/ShellCmd/TermEnv reproduce the historical local terminal wiring: the
// left terminal runs *termCmd inheriting the viewer's env (so it joins the same
// luvus session); the shell tab runs the resolved shell. TermEnv is nil here —
// startTtyd inherits the viewer env. (The shell tab's env-stripping is applied
// by the caller via outsideLuvusEnv, unchanged.)
func (b *localBackend) TermCmd() string  { return *termCmd }
func (b *localBackend) ShellCmd() string { return shellCommand() }
func (b *localBackend) TermEnv() []string {
	env := outsideLuvusEnv()
	if home := os.Getenv("LUVUS_HOME"); home != "" {
		env = append(env, "LUVUS_HOME="+home)
	}
	return append(env, "LUVUS_SOCKET_PATH="+b.sock, "LUVUS_API_ADDRESS="+b.sock)
}

func (b *localBackend) Close() error { return nil }

var _ Backend = (*localBackend)(nil)
