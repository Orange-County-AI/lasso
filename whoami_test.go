package main

import (
	"testing"
	"time"
)

func whoamiRecs() []AgentRecord {
	return []AgentRecord{
		{ID: "other", TabID: "other", CreatedAt: time.Now()},
		{ID: "self", Title: "whoami", Type: "scratch", TabID: "self", Agent: "claude", CreatedAt: time.Now()},
	}
}

// The headline case: an agent passes its $LASSO_TAB_ID and whoami resolves it to
// its own lasso record. (Status comes from a live tmux scrape; with no session it
// reads idle — we only assert the identity here.)
func TestResolveWhoamiMapsTabToAgent(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir()) // agentStatusNow scrapes tmux on its own socket
	out := resolveWhoami("local", whoamiRecs(), "self")
	if !out.Found || out.Agent == nil {
		t.Fatalf("expected found, got %+v", out)
	}
	if out.Agent.ID != "self" {
		t.Errorf("resolved to wrong agent: %q", out.Agent.ID)
	}
}

// A legacy record whose TabID is empty still matches by agent id.
func TestResolveWhoamiMatchesByAgentID(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	recs := []AgentRecord{{ID: "legacy", Title: "x", CreatedAt: time.Now()}}
	out := resolveWhoami("local", recs, "legacy")
	if !out.Found || out.Agent == nil || out.Agent.ID != "legacy" {
		t.Fatalf("expected to match by agent id, got %+v", out)
	}
}

// No tab_id: a structured answer telling the caller to pass $LASSO_TAB_ID, not an
// opaque error.
func TestResolveWhoamiEmptyTabID(t *testing.T) {
	out := resolveWhoami("local", whoamiRecs(), "  ")
	if out.Found || out.Agent != nil {
		t.Fatalf("expected not found, got %+v", out)
	}
	if out.Detail == "" {
		t.Error("expected a detail explaining LASSO_TAB_ID is required")
	}
}

// A tab lasso doesn't manage: found:false with an explanation, no error.
func TestResolveWhoamiUnknownTab(t *testing.T) {
	out := resolveWhoami("local", whoamiRecs(), "nope")
	if out.Found || out.Agent != nil {
		t.Fatalf("expected not found, got %+v", out)
	}
	if out.Detail == "" {
		t.Error("expected a detail for an unmanaged tab")
	}
}
