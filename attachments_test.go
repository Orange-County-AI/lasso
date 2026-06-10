package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// fakeCopyBackend implements the file ops moveAttachments needs (Open to read
// staging, Create to write the dest, RemoveAll to clear staging), delegating to
// os.* — staging and dest both resolve under LASSO_DIR (a local temp dir) here,
// standing in for "both on the selected host". It records what it touched so the
// test can assert the move went through the backend, not raw os.* on lasso.
type fakeCopyBackend struct {
	Backend
	opened  map[string]bool
	created map[string]bool
	removed map[string]bool
}

func (f *fakeCopyBackend) Open(p string) (io.ReadSeekCloser, error) {
	f.opened[p] = true
	return os.Open(p)
}
func (f *fakeCopyBackend) Create(p string) (io.WriteCloser, error) {
	f.created[p] = true
	return os.Create(p)
}
func (f *fakeCopyBackend) RemoveAll(p string) error {
	f.removed[p] = true
	return os.RemoveAll(p)
}

// Attachments are staged on the selected host's ~/.lasso/uploads (serveAgentUpload
// writes them through the backend) and moved into the work dir on that SAME host.
// This guards that moveAttachments reads the staged file and writes the dest both
// through the backend (so a remote host's staging+workdir never round-trip through
// some other host) and clears staging afterward.
func TestMoveAttachmentsWritesThroughBackend(t *testing.T) {
	base := t.TempDir()
	t.Setenv("LASSO_DIR", base)

	uploadID := "upl123"
	staging := filepath.Join(lassoUploadsDir(), uploadID)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"spec.md": "# spec", "diagram.txt": "a->b"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A destination standing in for the agent's work dir on the selected host.
	dest := filepath.Join(base, "workdir")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	fake := &fakeCopyBackend{
		opened:  map[string]bool{},
		created: map[string]bool{},
		removed: map[string]bool{},
	}
	moveAttachments(fake, uploadID, []string{"spec.md", "diagram.txt"}, dest)

	for name, want := range files {
		src := filepath.Join(staging, name)
		dst := filepath.Join(dest, name)
		if !fake.opened[src] {
			t.Errorf("%s was not read through the backend's Open", name)
		}
		if !fake.created[dst] {
			t.Errorf("%s was not written through the backend's Create", name)
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// Staging is cleared (through the backend) after a successful move.
	if !fake.removed[staging] {
		t.Errorf("staging dir should be removed through the backend")
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging dir should be gone, stat err = %v", err)
	}
}

// A nil/empty upload must be a no-op (and must not panic on the nil backend).
func TestMoveAttachmentsNoopWhenEmpty(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	fake := &fakeCopyBackend{
		opened:  map[string]bool{},
		created: map[string]bool{},
		removed: map[string]bool{},
	}
	moveAttachments(fake, "", nil, t.TempDir())
	moveAttachments(fake, "upl", nil, t.TempDir())
	if len(fake.created) != 0 || len(fake.opened) != 0 {
		t.Errorf("expected no file ops, got opened=%v created=%v", fake.opened, fake.created)
	}
}
