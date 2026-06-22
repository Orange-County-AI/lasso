package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
