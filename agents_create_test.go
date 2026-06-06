package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

type createAgentBackend struct {
	*memBackend
	worktreePath string
}

// HerdrCall is retained only so createAgentBackend still satisfies the (local-
// only) Backend interface during the migration; createWorktree never calls it.
func (b *createAgentBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// GitOut captures the worktree path from `git worktree add -b <branch> <path> <base>`.
func (b *createAgentBackend) GitOut(dir string, args ...string) (string, error) {
	if len(args) >= 5 && args[0] == "worktree" && args[1] == "add" {
		b.worktreePath = args[4]
	}
	return "", nil
}

func TestCreateGitAgentUsesUniqueBranchLeafForWorktreeDir(t *testing.T) {
	lasso := t.TempDir()
	t.Setenv("LASSO_DIR", lasso)

	b := &createAgentBackend{memBackend: newMemBackend()}
	existing := filepath.Join(lasso, "worktrees", "app", "fix-login-a1b2")
	b.dirs[existing] = true

	prev := curBackend()
	setBackend(b)
	t.Cleanup(func() { setBackend(prev) })

	// createWorktree is the agent-less core of a git workspace (shared by the New
	// Agent flow and the sidebar's "create worktree"); it derives the worktree dir
	// from the branch leaf, suffixing -2 when the name is taken.
	want := filepath.Join(lasso, "worktrees", "app", "fix-login-a1b2-2")
	workDir, err := createWorktree(b, "/repo/app", "main", "feature/fix-login-a1b2", "fix-login")
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	if workDir != want {
		t.Fatalf("worktree path = %q, want %q", workDir, want)
	}
	if b.worktreePath != want {
		t.Fatalf("git worktree add path = %q, want %q", b.worktreePath, want)
	}
}

// TestCreateScratchAgentPersists exercises the full createAgent flow for a
// scratch agent against real tmux + a real ~/.lasso: it must mkdir the scratch
// dir, create the backing tmux session, and persist a workspace + agent tab.
func TestCreateScratchAgentPersists(t *testing.T) {
	requireTmux(t) // sets LASSO_DIR to a temp dir + kills our tmux server on cleanup
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
	setBackend(&localBackend{sock: filepath.Join(t.TempDir(), "nope.sock")})
	t.Cleanup(func() { setBackend(prev) })

	rec, err := createAgent(curBackend(), createAgentReq{
		Type: "scratch", Title: "Hey boss", Prompt: "do a thing", Agent: "claude", NoFocus: true,
	})
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}
	if rec.TabID == "" || rec.WorkspaceID == "" {
		t.Fatalf("record missing tab/workspace ids: %+v", rec)
	}
	if !tmuxHasSession(tabSession(rec.TabID)) {
		t.Fatalf("tmux session %s not created", tabSession(rec.TabID))
	}
	if ws, err := getWorkspace(rec.WorkspaceID); err != nil || ws.Kind != "scratch" {
		t.Fatalf("workspace = %+v err=%v", ws, err)
	}
	tab, err := getTab(rec.TabID)
	if err != nil || tab.Kind != "agent" || tab.WorkspaceID != rec.WorkspaceID {
		t.Fatalf("agent tab not persisted: %+v err=%v", tab, err)
	}
}

func TestWorktreeDirSlug(t *testing.T) {
	cases := []struct {
		branch   string
		fallback string
		want     string
	}{
		{branch: "feature/add-dark-mode-a1b2", fallback: "add-dark-mode", want: "add-dark-mode-a1b2"},
		{branch: "fix/#42", fallback: "agent", want: "42"},
		{branch: "////", fallback: "agent", want: "agent"},
	}
	for _, c := range cases {
		if got := worktreeDirSlug(c.branch, c.fallback); got != c.want {
			t.Errorf("worktreeDirSlug(%q, %q) = %q, want %q", c.branch, c.fallback, got, c.want)
		}
	}
}
