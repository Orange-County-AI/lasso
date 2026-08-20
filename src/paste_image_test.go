package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type recordingPasteBackend struct {
	Backend
	name      string
	dir       string
	mkdirPath string
	mkdirMode fs.FileMode
	writePath string
	writeBody []byte
	writeMode fs.FileMode
}

func (b *recordingPasteBackend) Name() string          { return b.name }
func (b *recordingPasteBackend) PasteImageDir() string { return b.dir }
func (b *recordingPasteBackend) MkdirAll(path string, mode fs.FileMode) error {
	b.mkdirPath, b.mkdirMode = path, mode
	return nil
}
func (b *recordingPasteBackend) WriteFile(path string, body []byte, mode fs.FileMode) error {
	b.writePath, b.writeBody, b.writeMode = path, append([]byte(nil), body...), mode
	return nil
}

func TestServePasteImageWritesThroughTargetBackend(t *testing.T) {
	for _, tc := range []struct {
		name       string
		backend    string
		dir        string
		requestURL string
	}{
		{
			name:       "local terminal stays local",
			backend:    "local",
			dir:        "/home/local/.lasso/uploads/pasted-images",
			requestURL: "/api/paste-image",
		},
		{
			name:       "native attach target writes remotely",
			backend:    "ticket500",
			dir:        "/home/stephan/.lasso/uploads/pasted-images",
			requestURL: "/api/paste-image?host=ticket500",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := &recordingPasteBackend{name: tc.backend, dir: tc.dir}
			swapBackend(t, be)
			image := []byte("png bytes")
			req := httptest.NewRequest(http.MethodPost, tc.requestURL, bytes.NewReader(image))
			req.Header.Set("Content-Type", "image/png")
			rec := httptest.NewRecorder()

			servePasteImage(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			var response struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if filepath.Dir(response.Path) != tc.dir || !strings.HasSuffix(response.Path, ".png") {
				t.Errorf("response path = %q, want a png in %q", response.Path, tc.dir)
			}
			if be.mkdirPath != tc.dir || be.mkdirMode != 0o755 {
				t.Errorf("MkdirAll = (%q, %o), want (%q, 755)", be.mkdirPath, be.mkdirMode, tc.dir)
			}
			if be.writePath != response.Path || !bytes.Equal(be.writeBody, image) || be.writeMode != 0o644 {
				t.Errorf("WriteFile = (%q, %q, %o), want (%q, %q, 644)", be.writePath, be.writeBody, be.writeMode, response.Path, image)
			}
		})
	}
}

func TestServePasteImageRejectsUnsupportedMIMEBeforeWrite(t *testing.T) {
	be := &recordingPasteBackend{name: "local", dir: "/should-not-be-used"}
	swapBackend(t, be)
	req := httptest.NewRequest(http.MethodPost, "/api/paste-image", strings.NewReader("not an image"))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()

	servePasteImage(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnsupportedMediaType)
	}
	if be.mkdirPath != "" || be.writePath != "" {
		t.Fatalf("unsupported MIME touched backend: mkdir %q, write %q", be.mkdirPath, be.writePath)
	}
}
