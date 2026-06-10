package main

import (
	"path/filepath"
	"testing"
)

func TestReconcileTabs(t *testing.T) {
	requireTmux(t) // LASSO_DIR temp + kills our tmux server on cleanup
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() {
		if db != nil {
			db.Close()
			db = nil
		}
	})

	// A live tab whose cwd exists.
	good := t.TempDir()
	_ = insertWorkspace(Workspace{ID: "wg", Host: "local", Title: "g", WorkDir: good, Kind: "scratch"})
	_ = insertTab(Tab{ID: "good", WorkspaceID: "wg", Cwd: good, Kind: "shell"})
	if err := tmuxNewSession(tabSession("good"), good, nil); err != nil {
		t.Fatalf("new-session good: %v", err)
	}

	// A tab whose cwd is gone → should be retired (closed + session killed).
	gone := filepath.Join(t.TempDir(), "deleted")
	_ = insertWorkspace(Workspace{ID: "wd", Host: "local", Title: "d", WorkDir: gone, Kind: "git"})
	_ = insertTab(Tab{ID: "dead", WorkspaceID: "wd", Cwd: gone, Kind: "agent", AgentID: "dead"})

	// An orphan session with no live tab row → should be killed.
	if err := tmuxNewSession("lasso_orphan", good, nil); err != nil {
		t.Fatalf("new-session orphan: %v", err)
	}

	reconcileTabs()

	if !tmuxHasSession(tabSession("good")) {
		t.Error("live tab's session should survive reconcile")
	}
	if tmuxHasSession("lasso_orphan") {
		t.Error("orphan session should be killed")
	}
	if tabs, _ := listTabs("wd"); len(tabs) != 0 {
		t.Errorf("tab with missing cwd should be retired, live tabs = %d", len(tabs))
	}
}

func TestEnsureTabSessionRecreates(t *testing.T) {
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
	_ = insertWorkspace(Workspace{ID: "w1", Host: "local", Title: "x", WorkDir: dir, Kind: "scratch"})
	_ = insertTab(Tab{ID: "t1", WorkspaceID: "w1", Cwd: dir, Kind: "shell"})

	// No session exists yet (simulating post-reboot); ensureTabSession creates it.
	session, created, err := ensureTabSession("t1")
	if err != nil {
		t.Fatalf("ensureTabSession: %v", err)
	}
	if !created {
		t.Fatal("first ensure should report created=true")
	}
	if !tmuxHasSession(session) {
		t.Fatal("session should be created on first ensure")
	}
	// Idempotent: a second call returns the same live session, created=false.
	if s2, created2, err := ensureTabSession("t1"); err != nil || s2 != session || created2 {
		t.Fatalf("ensureTabSession second call = %q,%v,%v want %q,false", s2, created2, err, session)
	}
}
