package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// serveAgentHistory should return every recorded agent (across hosts) shaped as a
// gridPane: the title in WorkspaceLabel, the work dir as Cwd, and the record id in
// AgentID so the client can reopen it.
func TestServeAgentHistory(t *testing.T) {
	openTestDB(t)
	if err := appendAgent("local", AgentRecord{
		ID: "a1", Title: "Fix the bug", Type: "git", Agent: "claude",
		Description: "please fix the login bug", WorkDir: "/w/a1",
		WorkspaceID: "ws-a1", RootPane: "p-a1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	serveAgentHistory(rr, httptest.NewRequest(http.MethodGet, "/api/agent-history", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var out struct {
		Agents []gridPane `json:"agents"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Agents) != 1 {
		t.Fatalf("agents len = %d, want 1", len(out.Agents))
	}
	g := out.Agents[0]
	if g.AgentID != "a1" {
		t.Errorf("agent_id = %q, want a1", g.AgentID)
	}
	if g.WorkspaceLabel != "Fix the bug" {
		t.Errorf("workspace_label = %q, want the title", g.WorkspaceLabel)
	}
	if g.Cwd != "/w/a1" {
		t.Errorf("cwd = %q, want the work dir", g.Cwd)
	}
	if g.Prompt != "please fix the login bug" {
		t.Errorf("prompt = %q, want the description", g.Prompt)
	}
	if !g.HasAgent || g.Agent != "claude" {
		t.Errorf("has_agent/agent = %v/%q, want true/claude", g.HasAgent, g.Agent)
	}
}

// serveAgentHistory should also fold in orphan directories under scratch/ and
// worktrees/<repo>/ that have no agent record, shaped as reopen-by-path rows (Cwd
// set, AgentID empty), so a session whose record was never written is still
// findable by its directory name. A dir that *does* have a record isn't doubled.
func TestServeAgentHistoryOrphanDirs(t *testing.T) {
	openTestDB(t) // points LASSO_DIR at a fresh temp dir

	// One orphan scratch dir, one orphan worktree dir, and one scratch dir that has
	// a matching agent record (so it must come through as the record, not an orphan).
	scratch := lassoScratchDir()
	worktrees := lassoWorktreesDir()
	orphanScratch := filepath.Join(scratch, "ksa-boilerplate-engagement-odoo-sign-1i5t")
	orphanWorktree := filepath.Join(worktrees, "lasso", "fix-the-thing-abcd")
	recordedScratch := filepath.Join(scratch, "has-a-record-zzzz")
	for _, d := range []string{orphanScratch, orphanWorktree, recordedScratch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := appendAgent("local", AgentRecord{
		ID: "rec1", Title: "Has a record", Type: "scratch",
		WorkDir: recordedScratch, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	serveAgentHistory(rr, httptest.NewRequest(http.MethodGet, "/api/agent-history", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var out struct {
		Agents []gridPane `json:"agents"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byCwd := map[string]gridPane{}
	for _, g := range out.Agents {
		byCwd[g.Cwd] = g
	}

	// Orphans: present, no AgentID, label humanized from the dir basename.
	for _, dir := range []string{orphanScratch, orphanWorktree} {
		g, ok := byCwd[dir]
		if !ok {
			t.Fatalf("orphan dir %s not surfaced", dir)
		}
		if g.AgentID != "" {
			t.Errorf("orphan %s has agent_id %q, want empty", dir, g.AgentID)
		}
		if g.WorkspaceLabel != humanizeSlug(filepath.Base(dir)) {
			t.Errorf("orphan %s label = %q, want %q", dir, g.WorkspaceLabel, humanizeSlug(filepath.Base(dir)))
		}
	}

	// The recorded dir comes through exactly once, as its record (AgentID set).
	n := 0
	for _, g := range out.Agents {
		if g.Cwd == recordedScratch {
			n++
			if g.AgentID != "rec1" {
				t.Errorf("recorded dir agent_id = %q, want rec1", g.AgentID)
			}
		}
	}
	if n != 1 {
		t.Errorf("recorded dir appeared %d times, want 1 (not doubled as an orphan)", n)
	}
}

// scanOrphanWorkDirs reaches two levels into worktrees/ (worktrees/<repo>/<dir>)
// but only one level into scratch/, and skips files. Guard that shape.
func TestScanOrphanWorkDirsShape(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	b := &localBackend{}
	scratch := lassoScratchDir()
	worktrees := lassoWorktreesDir()
	mustMkdir(t, filepath.Join(scratch, "scratch-dir-aaaa"))
	mustMkdir(t, filepath.Join(worktrees, "myrepo", "wt-dir-bbbb"))
	// A bare file in scratch/ and a bare repo dir with no children contribute nothing.
	if err := os.WriteFile(filepath.Join(scratch, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(worktrees, "emptyrepo"))

	rows := scanOrphanWorkDirs(b, "local", "host", nil)
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Cwd] = true
	}
	want := []string{
		filepath.Join(scratch, "scratch-dir-aaaa"),
		filepath.Join(worktrees, "myrepo", "wt-dir-bbbb"),
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing orphan row for %s", w)
		}
	}
	if got[filepath.Join(scratch, "stray.txt")] {
		t.Error("stray file surfaced as an orphan dir")
	}
	if len(rows) != len(want) {
		t.Errorf("got %d rows, want %d: %v", len(rows), len(want), got)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}
