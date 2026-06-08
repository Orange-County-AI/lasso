package main

import (
	"io"
	"io/fs"
	"os"
	"sync"
)

// Backend abstracts the filesystem + git surface the handlers operate on. The
// local implementation (localBackend) hits os.* / git directly; remoteBackend
// drives another host over SSH (SFTP for files, `ssh host git …`, and per-command
// tmux over the SSH control master). Handlers route through curBackend() (the
// active host); tmux/grid code can target any host via hostBackend()/resolveBackend().
type Backend interface {
	// Name is the host identity — "local" or an ssh-config alias.
	Name() string

	// Filesystem ops mirror the os.* calls the file handlers make.
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

	// GitOut runs `git -C dir args...` and returns stdout.
	GitOut(dir string, args ...string) (string, error)

	// TmuxArgv builds the full argv that runs a tmux command against THIS host's
	// dedicated lasso tmux server. Local: `tmux -S <sock> -f /dev/null args…`.
	// Remote: `ssh <ctlOpts> <alias> 'tmux -S <remoteSock> -f /dev/null <quoted args…>'`,
	// multiplexed over the host's SSH control master. The tmux command-sequence
	// separator ";" passes through as its own (quoted) argv token either way.
	TmuxArgv(args []string) []string

	// TmuxAttachArgv is the argv ttyd runs to attach a browser terminal to a tmux
	// session interactively. Local: `tmux … attach -t <session>`. Remote: an
	// `ssh -tt … 'tmux … attach -t <session>'` that forces a remote PTY (over the
	// control master) so the TUI renders. Distinct from TmuxArgv (non-interactive).
	TmuxAttachArgv(session string) []string

	// HomeDir is the home directory, for ~-expansion in path inputs.
	HomeDir() (string, error)
	// PasteImageDir is where a pasted clipboard image is written.
	PasteImageDir() string

	// Close tears down any resources (the SSH control master for a remote
	// backend). The local backend is a no-op.
	Close() error
}

// active holds the backend every handler currently routes through.
var active struct {
	mu sync.RWMutex
	b  Backend
}

func curBackend() Backend {
	active.mu.RLock()
	defer active.mu.RUnlock()
	return active.b
}

func setBackend(b Backend) {
	active.mu.Lock()
	active.b = b
	active.mu.Unlock()
}

// ---------------------------------------------------------------------------
// localBackend — the machine lasso runs on
// ---------------------------------------------------------------------------

type localBackend struct{}

func (b *localBackend) Name() string { return "local" }

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

// TmuxArgv runs tmux locally on lasso's private socket (the historical path).
func (b *localBackend) TmuxArgv(args []string) []string {
	return append([]string{"tmux"}, append(tmuxPrefix(), args...)...)
}

// TmuxAttachArgv attaches ttyd to a local session (the historical viewport path).
func (b *localBackend) TmuxAttachArgv(session string) []string {
	return b.TmuxArgv([]string{"attach", "-t", session})
}

func (b *localBackend) Close() error { return nil }

var _ Backend = (*localBackend)(nil)

// localBE is the shared local backend singleton, so tmux/file routing for the
// local host never allocates or holds a connection.
var localBE Backend = &localBackend{}
