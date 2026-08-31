package main

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// recordingPasteBackend stands in for the host a drop targets: it records the
// directory made and the bytes streamed, and never touches a real filesystem.
type recordingPasteBackend struct {
	Backend
	name       string
	dir        string
	mkdirPath  string
	mkdirMode  fs.FileMode
	createPath string
	created    *closeRecorder
	removed    string
}

type closeRecorder struct {
	bytes.Buffer
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

func (b *recordingPasteBackend) Name() string         { return b.name }
func (b *recordingPasteBackend) PasteFileDir() string { return b.dir }
func (b *recordingPasteBackend) MkdirAll(path string, mode fs.FileMode) error {
	b.mkdirPath, b.mkdirMode = path, mode
	return nil
}
func (b *recordingPasteBackend) Create(path string) (io.WriteCloser, error) {
	b.createPath, b.created = path, &closeRecorder{}
	return b.created, nil
}
func (b *recordingPasteBackend) RemoveAll(path string) error {
	b.removed = path
	return nil
}

func TestServePasteFileWritesThroughTargetBackend(t *testing.T) {
	for _, tc := range []struct {
		name        string
		backend     string
		dir         string
		requestURL  string
		contentType string
		wantSuffix  string
	}{
		{
			name:        "clipboard image stays local and is named by its type",
			backend:     "local",
			dir:         "/home/local/.lasso/uploads/dropped-files",
			requestURL:  "/api/paste-file",
			contentType: "image/png",
			wantSuffix:  ".png",
		},
		{
			name:        "native attach target writes remotely",
			backend:     "ticket500",
			dir:         "/home/stephan/.lasso/uploads/dropped-files",
			requestURL:  "/api/paste-file?host=ticket500",
			contentType: "image/png",
			wantSuffix:  ".png",
		},
		{
			// The whole point of the drop: a picked file is not an image, and
			// keeps the name that makes its path readable in a prompt.
			name:        "a picked document keeps its own filename",
			backend:     "local",
			dir:         "/home/local/.lasso/uploads/dropped-files",
			requestURL:  "/api/paste-file?name=Q3%20notes.pdf",
			contentType: "application/pdf",
			wantSuffix:  "-Q3 notes.pdf",
		},
		{
			// A filename is client-supplied: only its base may reach the path.
			name:        "a traversing filename cannot escape the drop dir",
			backend:     "local",
			dir:         "/home/local/.lasso/uploads/dropped-files",
			requestURL:  "/api/paste-file?name=..%2F..%2F.ssh%2Fauthorized_keys",
			contentType: "text/plain",
			wantSuffix:  "-authorized_keys",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := &recordingPasteBackend{name: tc.backend, dir: tc.dir}
			swapBackend(t, be)
			body := []byte("dropped bytes")
			req := httptest.NewRequest(http.MethodPost, tc.requestURL, bytes.NewReader(body))
			req.Header.Set("Content-Type", tc.contentType)
			rec := httptest.NewRecorder()

			servePasteFile(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			var response struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if filepath.Dir(response.Path) != tc.dir ||
				!strings.HasSuffix(response.Path, tc.wantSuffix) {
				t.Errorf("response path = %q, want %q in %q", response.Path, tc.wantSuffix, tc.dir)
			}
			if be.mkdirPath != tc.dir || be.mkdirMode != 0o755 {
				t.Errorf("MkdirAll = (%q, %o), want (%q, 755)", be.mkdirPath, be.mkdirMode, tc.dir)
			}
			if be.createPath != response.Path {
				t.Errorf("Create = %q, want %q", be.createPath, response.Path)
			}
			if be.created == nil || !bytes.Equal(be.created.Bytes(), body) || !be.created.closed {
				t.Errorf("streamed %q (closed %v), want %q closed", be.created.Bytes(), be.created.closed, body)
			}
			if be.removed != "" {
				t.Errorf("removed %q on a good write", be.removed)
			}
		})
	}
}

func TestServePasteFileDropsAnEmptyBody(t *testing.T) {
	be := &recordingPasteBackend{name: "local", dir: "/home/local/.lasso/uploads/dropped-files"}
	swapBackend(t, be)
	req := httptest.NewRequest(http.MethodPost, "/api/paste-file?name=empty.txt", strings.NewReader(""))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()

	servePasteFile(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	// A zero-byte file left behind would be a dead path in someone's prompt.
	if be.removed != be.createPath {
		t.Errorf("removed %q, want the created %q", be.removed, be.createPath)
	}
}
