package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo makes a git repo at dir with a single commit stamped at the given
// ISO date (controls %ct so we can assert latest-commit ordering).
func initRepo(t *testing.T, dir, date string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
}

func TestServeTreeOrderingAndPin(t *testing.T) {
	requireTmux(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() {
		if db != nil {
			db.Close()
			db = nil
		}
	})
	prev := curBackend()
	setBackend(&localBackend{})
	t.Cleanup(func() { setBackend(prev) })

	root := t.TempDir()
	oldPath := filepath.Join(root, "old")
	newPath := filepath.Join(root, "new")
	initRepo(t, oldPath, "2020-01-01T00:00:00")
	initRepo(t, newPath, "2024-01-01T00:00:00")
	if err := setSetting("repos_root", root); err != nil {
		t.Fatal(err)
	}
	// Repos appear in the tree only when they have a workspace; give
	// each a main-checkout workspace (work_dir == repo root) so ordering + pinning
	// are exercised.
	_ = insertWorkspace(Workspace{ID: "wold", Host: "local", Title: "old", Repo: oldPath, WorkDir: oldPath, Kind: "git"})
	_ = insertWorkspace(Workspace{ID: "wnew", Host: "local", Title: "new", Repo: newPath, WorkDir: newPath, Kind: "git"})

	get := func() treePayload {
		req := httptest.NewRequest(http.MethodGet, "/api/tree", nil)
		rec := httptest.NewRecorder()
		serveTree(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("serveTree status %d: %s", rec.Code, rec.Body.String())
		}
		var p treePayload
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return p
	}

	p := get()
	if len(p.Repos) != 2 {
		t.Fatalf("repos = %d, want 2", len(p.Repos))
	}
	// Newer commit first.
	if p.Repos[0].Name != "new" || p.Repos[1].Name != "old" {
		t.Fatalf("order = [%s, %s], want [new, old]", p.Repos[0].Name, p.Repos[1].Name)
	}
	if p.Repos[0].PrimaryBranch != "main" {
		t.Errorf("primary branch = %q, want main", p.Repos[0].PrimaryBranch)
	}

	// Pinning the older repo floats it to the top.
	if err := pinRepo(sbHost(), filepath.Join(root, "old"), true); err != nil {
		t.Fatal(err)
	}
	p = get()
	if p.Repos[0].Name != "old" || !p.Repos[0].Pinned {
		t.Fatalf("after pin, first = %q pinned=%v, want old/true", p.Repos[0].Name, p.Repos[0].Pinned)
	}
}

func TestServeNewTabAndClose(t *testing.T) {
	requireTmux(t)
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() {
		if db != nil {
			db.Close()
			db = nil
		}
	})
	dir := t.TempDir()
	_ = insertWorkspace(Workspace{ID: "w1", Host: "local", Title: "ws", WorkDir: dir, Kind: "scratch"})

	// New shell tab.
	body := `{"workspace_id":"w1","title":"logs"}`
	req := httptest.NewRequest(http.MethodPost, "/api/tab/new", strings.NewReader(body))
	rec := httptest.NewRecorder()
	serveNewTab(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("serveNewTab status %d: %s", rec.Code, rec.Body.String())
	}
	var tab Tab
	_ = json.Unmarshal(rec.Body.Bytes(), &tab)
	if tab.ID == "" || tab.Kind != "shell" || tab.Title != "logs" {
		t.Fatalf("new tab = %+v", tab)
	}
	if !tmuxHasSession(tabSession(tab.ID)) {
		t.Fatal("new tab's tmux session should exist")
	}

	// Close it: session gone, row retired.
	close := `{"tab_id":"` + tab.ID + `"}`
	creq := httptest.NewRequest(http.MethodPost, "/api/tab/close", strings.NewReader(close))
	crec := httptest.NewRecorder()
	serveTabClose(crec, creq)
	if crec.Code != http.StatusOK {
		t.Fatalf("serveTabClose status %d", crec.Code)
	}
	if tmuxHasSession(tabSession(tab.ID)) {
		t.Error("session should be killed on tab close")
	}
	if tabs, _ := listTabs("w1"); len(tabs) != 0 {
		t.Errorf("live tabs after close = %d, want 0", len(tabs))
	}
}
