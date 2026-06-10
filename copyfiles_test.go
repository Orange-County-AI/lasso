package main

import (
	"bytes"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// memBackend is a virtual filesystem standing in for a remote host: its paths
// exist only in these maps, never on the local disk. That's the whole point —
// if backendGlob/copyRepoFiles reached for the local os (the old filepath.Glob
// bug) they'd find nothing here. It embeds Backend so only the methods these
// helpers actually use need implementing.
type memBackend struct {
	Backend
	dirs  map[string]bool
	files map[string]string
	wrote map[string]string // captures everything Create-d, for assertions
}

func newMemBackend() *memBackend {
	return &memBackend{
		dirs:  map[string]bool{},
		files: map[string]string{},
		wrote: map[string]string{},
	}
}

// Name identifies the (single) fake host. The embedded nil Backend would panic
// here, and host-scoped DB lookups key off curBackend().Name().
func (b *memBackend) Name() string { return "local" }

func (b *memBackend) mkdirAllAncestors(p string) {
	for d := p; d != "/" && d != "." && d != ""; d = filepath.Dir(d) {
		b.dirs[d] = true
	}
}

func (b *memBackend) addFile(p, content string) {
	b.files[p] = content
	b.mkdirAllAncestors(filepath.Dir(p))
}

func (b *memBackend) ReadDir(p string) ([]fileEntry, error) {
	var out []fileEntry
	for d := range b.dirs {
		if d != p && filepath.Dir(d) == p {
			out = append(out, fileEntry{Name: filepath.Base(d), Dir: true})
		}
	}
	for f, c := range b.files {
		if filepath.Dir(f) == p {
			out = append(out, fileEntry{Name: filepath.Base(f), Size: int64(len(c))})
		}
	}
	return out, nil
}

func (b *memBackend) Stat(p string) (fs.FileInfo, error) {
	if b.dirs[p] {
		return memInfo{name: filepath.Base(p), dir: true}, nil
	}
	if c, ok := b.files[p]; ok {
		return memInfo{name: filepath.Base(p), size: int64(len(c))}, nil
	}
	return nil, fs.ErrNotExist
}

func (b *memBackend) Lstat(p string) (fs.FileInfo, error) { return b.Stat(p) }

func (b *memBackend) Open(p string) (io.ReadSeekCloser, error) {
	c, ok := b.files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return memReader{strings.NewReader(c)}, nil
}

func (b *memBackend) Create(p string) (io.WriteCloser, error) {
	return &memWriter{b: b, p: p}, nil
}

func (b *memBackend) MkdirAll(p string, _ fs.FileMode) error {
	b.mkdirAllAncestors(p)
	return nil
}

type memInfo struct {
	name string
	size int64
	dir  bool
}

func (m memInfo) Name() string { return m.name }
func (m memInfo) Size() int64  { return m.size }
func (m memInfo) Mode() fs.FileMode {
	if m.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (m memInfo) ModTime() time.Time { return time.Time{} }
func (m memInfo) IsDir() bool        { return m.dir }
func (m memInfo) Sys() any           { return nil }

type memReader struct{ *strings.Reader }

func (memReader) Close() error { return nil }

type memWriter struct {
	b   *memBackend
	p   string
	buf bytes.Buffer
}

func (w *memWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *memWriter) Close() error {
	w.b.files[w.p] = w.buf.String()
	w.b.wrote[w.p] = w.buf.String()
	return nil
}

func TestBackendGlobMatchesOnBackend(t *testing.T) {
	b := newMemBackend()
	b.addFile("/remote/proj/.env", "X")
	b.addFile("/remote/proj/.env.local", "Y")
	b.addFile("/remote/proj/README.md", "Z")
	b.addFile("/remote/proj/config/a.json", "A")
	b.addFile("/remote/proj/config/b.json", "B")

	cases := []struct {
		pattern string
		want    []string
	}{
		{"/remote/proj/.env", []string{"/remote/proj/.env"}},
		{"/remote/proj/missing", nil},
		{"/remote/proj/*.json", nil}, // no json at the top level
		{"/remote/proj/.env*", []string{"/remote/proj/.env", "/remote/proj/.env.local"}},
		{"/remote/proj/config/*.json", []string{"/remote/proj/config/a.json", "/remote/proj/config/b.json"}},
		{"/remote/proj/*/*.json", []string{"/remote/proj/config/a.json", "/remote/proj/config/b.json"}},
	}
	for _, c := range cases {
		got := backendGlob(b, c.pattern)
		sort.Strings(got)
		if !equalStrings(got, c.want) {
			t.Errorf("backendGlob(%q) = %v, want %v", c.pattern, got, c.want)
		}
	}
}

// copyRepoFiles must glob + copy entirely on the active backend, and skip files
// that already exist in the destination.
func TestCopyRepoFilesOnBackend(t *testing.T) {
	b := newMemBackend()
	repo := "/remote/proj"
	dest := "/remote/proj-wt"
	b.addFile(repo+"/.env", "SECRET")
	b.addFile(repo+"/config/a.json", "AAA")
	b.addFile(repo+"/config/b.json", "BBB")
	b.mkdirAllAncestors(dest)
	// A pre-existing dest file must not be overwritten.
	b.addFile(dest+"/.env", "KEEP")

	copyRepoFiles(b, repo, dest, ".env, config/*.json")

	// .env already existed → untouched, never written.
	if _, ok := b.wrote[dest+"/.env"]; ok {
		t.Error(".env should not have been overwritten")
	}
	if b.files[dest+"/.env"] != "KEEP" {
		t.Errorf("dest .env = %q, want KEEP", b.files[dest+"/.env"])
	}
	// The json files copied into the worktree's config subdir.
	for name, want := range map[string]string{"a.json": "AAA", "b.json": "BBB"} {
		dst := dest + "/config/" + name
		if b.wrote[dst] != want {
			t.Errorf("%s = %q, want %q", dst, b.wrote[dst], want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
