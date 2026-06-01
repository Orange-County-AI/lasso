package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type createAgentBackend struct {
	*memBackend
	worktreePath string
}

func (b *createAgentBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	if method == "worktree.create" {
		if p, ok := params.(map[string]any); ok {
			if path, ok := p["path"].(string); ok {
				b.worktreePath = path
			}
		}
	}
	return json.RawMessage(`{"workspace":{"workspace_id":"ws"},"root_pane":{"pane_id":""}}`), nil
}

func (b *createAgentBackend) GitOut(dir string, args ...string) (string, error) {
	return "", nil
}

func TestCreateGitAgentUsesUniqueBranchLeafForWorktreeDir(t *testing.T) {
	lasso := t.TempDir()
	t.Setenv("LASSO_DIR", lasso)
	// serveCreateAgent persists the host's remembered selections + agent log, so
	// it needs the state DB open.
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() {
		if db != nil {
			db.Close()
			db = nil
		}
	})

	b := &createAgentBackend{memBackend: newMemBackend()}
	existing := filepath.Join(lasso, "worktrees", "fix-login-a1b2")
	b.dirs[existing] = true

	prev := curBackend()
	setBackend(b)
	t.Cleanup(func() { setBackend(prev) })

	reqBody := `{
		"type": "git",
		"title": "Fix login",
		"repo": "/repo/app",
		"base_branch": "main",
		"branch_prefix": "feature",
		"branch_name": "fix-login-a1b2",
		"agent": "codex",
		"plan_mode": false
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/create-agent", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()

	serveCreateAgent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("serveCreateAgent status = %d, body = %s", rec.Code, rec.Body.String())
	}
	want := filepath.Join(lasso, "worktrees", "fix-login-a1b2-2")
	if b.worktreePath != want {
		t.Fatalf("worktree path = %q, want %q", b.worktreePath, want)
	}

	var agent AgentRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &agent); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if agent.WorkDir != want {
		t.Errorf("response work_dir = %q, want %q", agent.WorkDir, want)
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
