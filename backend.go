package main

import (
	"io"
	"io/fs"
	"os"
	"sync"
)

// Backend abstracts the filesystem + git surface the handlers operate on. lasso
// is local-only, so there is a single implementation (localBackend) that hits
// os.* / git directly; the interface remains as a seam handlers route through
// (curBackend()) and tests can substitute.
type Backend interface {
	// Name is the host identity — always "local".
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

	// HomeDir is the home directory, for ~-expansion in path inputs.
	HomeDir() (string, error)
	// PasteImageDir is where a pasted clipboard image is written.
	PasteImageDir() string
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

var _ Backend = (*localBackend)(nil)
