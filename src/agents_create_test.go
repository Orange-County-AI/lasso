package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	existing := filepath.Join(lasso, "worktrees", "app", "fix-login-a1b2")
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
	want := filepath.Join(lasso, "worktrees", "app", "fix-login-a1b2-2")
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

// bootFake is a backend whose agent-boot RPCs are controllable: worktree/
// workspace create return a real root pane, but the pane reads that launchAgentInPane
// waits on block until the test releases them, and pane.send_text (the agent-launch
// write) fails. That lets the test prove createAgent returns before the boot runs,
// and that a boot failure is recorded on the persisted agent instead of being lost.
type bootFake struct {
	*memBackend
	release    chan struct{} // closed by the test to let the blocked boot proceed
	readSeen   chan struct{} // closed once the boot's first pane.read lands (boot started)
	readOnce   sync.Once
	releaseOne sync.Once
	sendErr    error // returned from pane.send_text to fail the launch
}

func (b *bootFake) HerdrCall(method string, _ any) (json.RawMessage, error) {
	switch method {
	case "worktree.create", "workspace.create":
		return json.RawMessage(`{"workspace":{"workspace_id":"ws"},"root_pane":{"pane_id":"p1"}}`), nil
	case "pane.read":
		b.readOnce.Do(func() { close(b.readSeen) })
		<-b.release // hold the boot here until the test lets it continue
		// Stable text so waitPaneReady settles quickly once released.
		return json.RawMessage(`{"read":{"text":"$ "}}`), nil
	case "pane.send_text":
		return nil, b.sendErr // the agent-launch write fails → boot fails
	default:
		return json.RawMessage(`{}`), nil
	}
}

func (b *bootFake) GitOut(string, ...string) (string, error) { return "", nil }

func (b *bootFake) releaseBoot() { b.releaseOne.Do(func() { close(b.release) }) }

// createAgent must return as soon as the durable facts exist (id, workspace, root
// pane, persisted record) — WITHOUT waiting for the slow boot (file copy, setup,
// CLI launch, pane readiness). And when that async boot fails, the failure must be
// recorded on the agent so a later get_agent/list_agents shows "failed" rather than
// a phantom healthy agent.
func TestCreateAgentReturnsBeforeBootAndRecordsBootFailure(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() {
		if db != nil {
			db.Close()
			db = nil
		}
	})

	b := &bootFake{
		memBackend: newMemBackend(),
		release:    make(chan struct{}),
		readSeen:   make(chan struct{}),
		sendErr:    errors.New("pane gone"),
	}
	// Always let the (possibly still-blocked) boot goroutine finish, so it can't
	// leak or write to a closing db. Registered after the db-close cleanup so it
	// runs first (LIFO).
	t.Cleanup(b.releaseBoot)

	prev := curBackend()
	setBackend(b)
	t.Cleanup(func() { setBackend(prev) })

	start := time.Now()
	rec, err := createAgent(b, createAgentReq{Type: "scratch", Title: "Boot test", Prompt: "boot test"})
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}
	// The boot is still blocked (release not yet closed), so a fast return here
	// proves create didn't wait on it.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("createAgent blocked on boot: took %v", elapsed)
	}
	// Durable facts the caller needs must be populated.
	if rec.ID == "" || rec.WorkspaceID != "ws" || rec.RootPane != "p1" || rec.WorkDir == "" {
		t.Fatalf("returned record missing durable facts: %+v", rec)
	}
	if rec.BootStatus != BootBooting {
		t.Errorf("returned BootStatus = %q, want %q", rec.BootStatus, BootBooting)
	}

	// Wait until the boot goroutine has actually started (and is now blocked in the
	// pane-readiness wait). The persisted record must still read "booting" — proof
	// the response landed while the boot was mid-flight, not after it.
	select {
	case <-b.readSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("boot goroutine never started")
	}
	if got, err := findAgentRecord("local", rec.ID); err != nil {
		t.Fatalf("findAgentRecord: %v", err)
	} else if got.BootStatus != BootBooting {
		t.Errorf("persisted BootStatus while booting = %q, want %q", got.BootStatus, BootBooting)
	}

	// Let the boot proceed; its pane.send_text fails, so it must record BootFailed.
	b.releaseBoot()
	deadline := time.Now().Add(5 * time.Second)
	var final AgentRecord
	for time.Now().Before(deadline) {
		final, err = findAgentRecord("local", rec.ID)
		if err != nil {
			t.Fatalf("findAgentRecord: %v", err)
		}
		if final.BootStatus == BootFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if final.BootStatus != BootFailed {
		t.Fatalf("boot failure not recorded: BootStatus = %q, want %q", final.BootStatus, BootFailed)
	}
	if !strings.Contains(final.BootError, "pane gone") {
		t.Errorf("BootError = %q, want it to mention the launch failure", final.BootError)
	}
	// A failed boot must surface as the agent's status, so get_agent/list_agents
	// don't report a phantom healthy agent.
	if info := agentInfoFrom("local", final, ""); info.Status != "failed" {
		t.Errorf("agentInfoFrom status = %q, want \"failed\"", info.Status)
	}
}

// resumeFake simulates the host state an interrupted create leaves behind: the
// branch exists in git, the worktree dir is on disk, and herdr already has a
// workspace rooted there. It records which herdr methods were called so the
// test can prove the retry adopted the orphan instead of re-creating.
type resumeFake struct {
	*memBackend
	branch  string // the git branch that "exists"
	workDir string // checkout path herdr's workspace reports
	mu      sync.Mutex
	calls   []string
}

func (b *resumeFake) HerdrCall(method string, _ any) (json.RawMessage, error) {
	b.mu.Lock()
	b.calls = append(b.calls, method)
	b.mu.Unlock()
	switch method {
	case "workspace.list":
		return json.RawMessage(fmt.Sprintf(
			`{"workspaces":[{"workspace_id":"wsX","worktree":{"checkout_path":%q}}]}`, b.workDir)), nil
	case "pane.list":
		return json.RawMessage(`{"panes":[{"pane_id":"pX","workspace_id":"wsX"}]}`), nil
	case "pane.read":
		// Stable text so the async boot's waitPaneReady settles quickly.
		return json.RawMessage(`{"read":{"text":"$ "}}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func (b *resumeFake) GitOut(_ string, args ...string) (string, error) {
	// `git branch --list <name>`: only the interrupted attempt's branch exists.
	if len(args) >= 2 && args[0] == "branch" && args[len(args)-1] == b.branch {
		return "  " + b.branch + "\n", nil
	}
	return "", nil
}

func (b *resumeFake) called(method string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.calls {
		if c == method {
			return true
		}
	}
	return false
}

// A retried create after a mid-flight failure (lasso restarted, response lost)
// must resume the interrupted attempt — same branch, same worktree dir, the
// herdr workspace it already has — rather than minting a -2 branch beside an
// orphan. This is the regression behind the 502-then-retry incident: the first
// attempt's worktree.create completed host-side, but its record was never
// saved, and the retry duplicated the whole tree.
func TestCreateAgentResumesInterruptedCreate(t *testing.T) {
	lasso := t.TempDir()
	t.Setenv("LASSO_DIR", lasso)
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() {
		if db != nil {
			db.Close()
			db = nil
		}
	})

	branch := "feature/fix-login-a1b2"
	workDir := filepath.Join(lasso, "worktrees", "app", "fix-login-a1b2")
	b := &resumeFake{memBackend: newMemBackend(), branch: branch, workDir: workDir}
	b.dirs[workDir] = true // the interrupted attempt's worktree is on disk

	// The interrupted attempt's write-ahead record: no workspace, still at
	// BootCreating (a sweep to BootFailed matches the same way — workspace_id
	// being empty is what marks it interrupted).
	if err := appendAgent("local", AgentRecord{
		ID: "old1", Type: "git", Title: "Fix login", Repo: "/repo/app",
		Branch: branch, WorkDir: workDir, BootStatus: BootCreating,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("appendAgent: %v", err)
	}

	prev := curBackend()
	setBackend(b)
	t.Cleanup(func() { setBackend(prev) })

	rec, err := createAgent(b, createAgentReq{
		Type: "git", Title: "Fix login", Prompt: "Fix login",
		Repo: "/repo/app", BaseBranch: "main",
		BranchPrefix: "feature", BranchName: "fix-login-a1b2",
	})
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}

	if rec.Branch != branch {
		t.Errorf("branch = %q, want %q (retry must not mint a -2 suffix)", rec.Branch, branch)
	}
	if rec.WorkDir != workDir {
		t.Errorf("work_dir = %q, want the interrupted attempt's %q", rec.WorkDir, workDir)
	}
	if rec.WorkspaceID != "wsX" || rec.RootPane != "pX" {
		t.Errorf("adoption: got workspace %q pane %q, want the live wsX/pX", rec.WorkspaceID, rec.RootPane)
	}
	if b.called("worktree.create") {
		t.Error("worktree.create was called — the resume must reattach, not re-create")
	}
	// The orphan record is superseded by this attempt's, not left as a duplicate.
	if _, err := findAgentRecord("local", "old1"); err == nil {
		t.Error("interrupted record still present — resume should delete it")
	}
	recs, err := listAgents("local")
	if err != nil {
		t.Fatalf("listAgents: %v", err)
	}
	n := 0
	for _, r := range recs {
		if r.Branch == branch {
			n++
		}
	}
	if n != 1 {
		t.Errorf("records for %s = %d, want exactly 1", branch, n)
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
