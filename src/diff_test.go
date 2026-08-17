package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"testing"
)

type gitFailBackend struct{ *localBackend }

func (gitFailBackend) GitOut(string, ...string) (string, error) {
	return "", fmt.Errorf("ssh gigachad: timed out after 20s")
}

func getDiff(t *testing.T, dir string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	serveDiff(rec, httptest.NewRequest(http.MethodGet,
		"/api/diff?mode=auto&ignoreWhitespace=true&path="+url.QueryEscape(dir), nil))
	return rec
}

func decodeDiff(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode diff response: %v (%s)", err, rec.Body.String())
	}
	return payload
}

func TestServeDiffPlainDirectoryIsNotAnError(t *testing.T) {
	prev := curBackend()
	setBackend(&localBackend{})
	t.Cleanup(func() { setBackend(prev) })

	// A pane parked in /home/stephan is a normal plain-directory case.
	rec := getDiff(t, t.TempDir())
	if want := http.StatusOK; rec.Code != want {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, want, rec.Body.String())
	}
	payload := decodeDiff(t, rec)
	if got, want := payload["isRepo"], false; got != want {
		t.Fatalf("isRepo = %v, want %v", got, want)
	}
	if got, want := payload["dirty"], float64(0); got != want {
		t.Fatalf("dirty = %v, want %v", got, want)
	}
	files, ok := payload["files"].([]any)
	if !ok || files == nil || len(files) != 0 {
		t.Fatalf("files = %#v, want an empty non-nil array", payload["files"])
	}
}

func TestServeDiffRepoReportsIsRepo(t *testing.T) {
	prev := curBackend()
	setBackend(&localBackend{})
	t.Cleanup(func() { setBackend(prev) })

	dir := t.TempDir()
	if err := exec.Command("git", "init", "-q", dir).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	rec := getDiff(t, dir)
	if want := http.StatusOK; rec.Code != want {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, want, rec.Body.String())
	}
	if got, want := decodeDiff(t, rec)["isRepo"], true; got != want {
		t.Fatalf("isRepo = %v, want %v", got, want)
	}
}

func TestServeDiffBackendFailureStays502(t *testing.T) {
	prev := curBackend()
	setBackend(gitFailBackend{&localBackend{}})
	t.Cleanup(func() { setBackend(prev) })

	// An unreachable host must not masquerade as a clean non-repo; that made this
	// bug look like an SSH problem.
	rec := getDiff(t, "/tmp")
	if want := http.StatusBadGateway; rec.Code != want {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, want, rec.Body.String())
	}
}

func TestServeDiffMissingDirectoryIsNotAnError(t *testing.T) {
	prev := curBackend()
	setBackend(&localBackend{})
	t.Cleanup(func() { setBackend(prev) })

	dir := filepath.Join(t.TempDir(), "gone")
	rec := getDiff(t, dir)
	if want := http.StatusOK; rec.Code != want {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, want, rec.Body.String())
	}
	if got, want := decodeDiff(t, rec)["isRepo"], false; got != want {
		t.Fatalf("isRepo = %v, want %v", got, want)
	}
}
