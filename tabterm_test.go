package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestTmuxAttachArgv(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	argv := tmuxAttachArgv("lasso_abc")
	joined := strings.Join(argv, " ")
	// Must always carry our private socket + no-user-config, and attach the session.
	if argv[0] != "tmux" || !strings.Contains(joined, "-S "+lassoTmuxSock()) ||
		!strings.Contains(joined, "-f /dev/null") || !strings.Contains(joined, "attach -t lasso_abc") {
		t.Fatalf("attach argv missing required parts: %q", joined)
	}
}

// TestViewportSpawns spawns the real persistent viewport ttyd (attached to the
// per-instance park session) and checks the proxy base is stable across calls,
// and that pointing it at a tab lazily creates that tab's session.
func TestViewportSpawns(t *testing.T) {
	requireTmux(t)
	if _, err := exec.LookPath("ttyd"); err != nil {
		t.Skip("ttyd not installed")
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
	ctx, cancel := context.WithCancel(context.Background())
	srvCtx = ctx
	t.Cleanup(func() {
		cancel()
		_ = tmuxKillSession(tmuxParkSession())
		viewport.token, viewport.base, viewport.proxy = "", "", nil
	})

	base, err := ensureViewport()
	if err != nil {
		t.Fatalf("ensureViewport: %v", err)
	}
	if !strings.HasPrefix(base, "/tab-term/") {
		t.Fatalf("base = %q, want /tab-term/ prefix", base)
	}
	if !tmuxHasSession(tmuxParkSession()) {
		t.Fatal("park session should exist after ensureViewport")
	}
	// Idempotent: the base is stable (one viewport, no per-tab churn).
	if b2, _ := ensureViewport(); b2 != base {
		t.Fatalf("second ensureViewport = %q, want %q", b2, base)
	}

	// Pointing at a tab lazily creates its session (the viewport then survives it).
	dir := t.TempDir()
	_ = insertWorkspace(Workspace{ID: "w1", Host: "local", Title: "x", WorkDir: dir, Kind: "scratch"})
	_ = insertTab(Tab{ID: "tt1", WorkspaceID: "w1", Cwd: dir, Kind: "shell"})
	session, created, err := ensureTabSession("tt1")
	if err != nil || !created {
		t.Fatalf("ensureTabSession = %q,%v,%v", session, created, err)
	}
	if !tmuxHasSession(session) {
		t.Fatal("tab session should exist after ensureTabSession")
	}
}
