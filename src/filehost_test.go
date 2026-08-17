package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sidebar's file endpoints take an explicit host selector (?host= / a JSON
// or form `host` field) and default to the active host. These tests pin the
// routing contract: a host lasso may not drive is refused with 502 before any
// filesystem is touched, the default still serves the active host, and tilde
// expansion runs against the RESOLVED backend, not the active one.

// fsSpyBackend wraps a backend and counts filesystem reads, so a test can prove
// a request never reached ANY backend's filesystem.
type fsSpyBackend struct {
	Backend
	fsReads int
}

func (b *fsSpyBackend) ReadDir(path string) ([]fileEntry, error) {
	b.fsReads++
	return b.Backend.ReadDir(path)
}

func (b *fsSpyBackend) Stat(path string) (fi os.FileInfo, err error) {
	b.fsReads++
	return b.Backend.Stat(path)
}

// fakeHomeBackend names itself "local" but resolves ~ to a sentinel directory,
// so a test can tell whether ~ was expanded against the resolved backend (the
// real local one) or against the active backend (this fake).
type fakeHomeBackend struct {
	Backend
	home string
}

func (b *fakeHomeBackend) Name() string             { return "local" }
func (b *fakeHomeBackend) HomeDir() (string, error) { return b.home, nil }

// swapBackend installs b as the active backend for one test, restoring the
// previous one on cleanup (the convention the other handler tests use).
func swapBackend(t *testing.T, b Backend) {
	t.Helper()
	prev := curBackend()
	setBackend(b)
	t.Cleanup(func() { setBackend(prev) })
}

// A ?host= we may not drive is refused with 502 before any filesystem read: the
// spy active backend must stay untouched (the old code read the active
// backend's dir regardless of host).
func TestServeFilesBogusHostRefusedBeforeRead(t *testing.T) {
	spy := &fsSpyBackend{Backend: &localBackend{}}
	swapBackend(t, spy)

	req := httptest.NewRequest(http.MethodGet, "/api/files?path=/&host=no-such-host-xyz", nil)
	rec := httptest.NewRecorder()
	serveFiles(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if want := `host "no-such-host-xyz" not available`; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body %q does not mention %q", rec.Body.String(), want)
	}
	if spy.fsReads != 0 {
		t.Errorf("filesystem was read %d time(s) for a refused host; want 0", spy.fsReads)
	}
}

// file-write with a bogus host refuses with 502 and leaves the target file
// untouched (a handler that ignored the host field would overwrite it).
func TestFileWriteBogusHostLeavesFileUntouched(t *testing.T) {
	swapBackend(t, &localBackend{})
	dir := t.TempDir()
	file := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(file, []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := `{"path": "` + file + `", "content": "NEW", "host": "no-such-host-xyz"}`
	req := httptest.NewRequest(http.MethodPost, "/api/file-write", strings.NewReader(body))
	rec := httptest.NewRecorder()
	serveFileWrite(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "OLD" {
		t.Errorf("file content = %q after refused write; want %q", got, "OLD")
	}
}

// With no host selector the file endpoints still serve the active (local)
// host's paths — the default is the active host, not an error.
func TestFileEndpointsDefaultToLocal(t *testing.T) {
	swapBackend(t, &localBackend{})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	serveFiles(rec, httptest.NewRequest(http.MethodGet, "/api/files?path="+dir, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("serveFiles status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var listed struct {
		Entries []fileEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode serveFiles response: %v", err)
	}
	aFile, subDir := false, false
	for _, e := range listed.Entries {
		switch e.Name {
		case "a.txt":
			aFile = !e.Dir
		case "sub":
			subDir = e.Dir
		}
	}
	if !aFile || !subDir {
		t.Errorf("entries = %+v; want a.txt (file) and sub (dir)", listed.Entries)
	}

	rec = httptest.NewRecorder()
	serveFile(rec, httptest.NewRequest(http.MethodGet, "/api/file?path="+filepath.Join(dir, "a.txt"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("serveFile status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello" {
		t.Errorf("serveFile body = %q, want %q", rec.Body.String(), "hello")
	}
}

// ~ expands against the RESOLVED backend, not the active one: the active
// backend here claims a sentinel home, but with no host selector the resolved
// backend is the real local one — so the 404 for ~/missing must name the real
// home, never the sentinel. (The old code expanded before resolving, which on
// a remote-active session turned a remote ~/x into the active host's home.)
func TestServeFilesTildeExpandsOnResolvedBackend(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("no local home dir to expand against: %v", err)
	}
	sentinel := t.TempDir()
	swapBackend(t, &fakeHomeBackend{home: sentinel})

	rec := httptest.NewRecorder()
	serveFiles(rec, httptest.NewRequest(http.MethodGet, "/api/files?path=~/no-such-entry-xyz", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), filepath.Join(home, "no-such-entry-xyz")) {
		t.Errorf("body %q does not name the resolved (local) home %q", rec.Body.String(), home)
	}
	if strings.Contains(rec.Body.String(), sentinel) {
		t.Errorf("body %q expanded ~ against the ACTIVE backend's home %q; want the resolved backend's", rec.Body.String(), sentinel)
	}
}
