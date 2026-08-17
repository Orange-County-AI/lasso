package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// listBackend is one fake host's herdr for the list_agents path: pane.list
// answers with a fixture, or fails wholesale when down is set (herdr restarting,
// host briefly unreachable).
type listBackend struct {
	*memBackend
	panes []pane
	down  bool
}

func (b *listBackend) Name() string { return "local" }

func (b *listBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	if b.down {
		return nil, &herdrError{Code: "internal", Message: "herdr is restarting"}
	}
	if method == "pane.list" {
		out, err := json.Marshal(map[string]any{"panes": b.panes})
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	return json.RawMessage(`{}`), nil
}

// stubListBackend points the MCP tools' backend resolution at b.
func stubListBackend(t *testing.T, b Backend) {
	t.Helper()
	stubSSHHosts(t)
	prev := resolveBackend
	resolveBackend = func(host string) (Backend, error) {
		if host != "" && !isLocalHost(host) {
			return nil, fmt.Errorf("host %q not available", host)
		}
		return b, nil
	}
	t.Cleanup(func() { resolveBackend = prev })
}

func listAgentsIDs(out listAgentsOut) map[string]agentInfo {
	m := map[string]agentInfo{}
	for _, a := range out.Agents {
		key := a.ID
		if key == "" { // a foreign session has no lasso id — key it by its pane
			key = a.RootPane
		}
		m[key] = a
	}
	return m
}

// The headline case: list_agents returns only agents herdr still has a pane for,
// and it reconciles the rest away as it goes — while the foreign herdr session
// (a pane lasso never created, e.g. a long-lived bot) keeps being listed.
func TestListAgentsReconcilesAndKeepsForeignSessions(t *testing.T) {
	openTestDB(t)
	resetAgentReapMiss(t)
	if err := appendAgent("local", reapRec("livesone", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	if err := appendAgent("local", reapRec("deadone", "w2:p1")); err != nil {
		t.Fatal(err)
	}
	b := &listBackend{memBackend: newMemBackend(), panes: []pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", AgentStatus: "idle"},
		// A pane lasso did not create: a bot in its own workspace.
		{PaneID: "w5:p1", WorkspaceID: "w5", Agent: "claude", AgentStatus: "working",
			TerminalTitle: "Clem (OCAI)", TerminalTitleStripped: "Clem (OCAI)"},
	}}
	stubListBackend(t, b)

	var out listAgentsOut
	// The reaper wants agentReapMisses consecutive misses before it condemns.
	for i := 0; i < agentReapMisses; i++ {
		var err error
		if _, out, err = listAgentsTool(context.Background(), nil, listAgentsIn{}); err != nil {
			t.Fatalf("list_agents: %v", err)
		}
	}
	if out.HerdrError != "" {
		t.Fatalf("unexpected herdr_error: %s", out.HerdrError)
	}
	got := listAgentsIDs(out)
	if _, ok := got["livesone"]; !ok {
		t.Error("agent with a live pane is missing from the listing")
	}
	if _, ok := got["deadone"]; ok {
		t.Error("agent whose pane herdr no longer has is still listed")
	}
	foreign, ok := got["w5:p1"]
	if !ok {
		t.Fatal("foreign herdr session was dropped — reconciliation must not touch panes lasso did not create")
	}
	if foreign.LassoCreated {
		t.Error("foreign session claims lasso_created")
	}
	if len(out.Agents) != 2 {
		t.Errorf("listed %d agents, want 2", len(out.Agents))
	}
}

// A herdr that cannot be enumerated is not evidence that anything died: the
// listing says so (herdr_error, i.e. PARTIAL) and every record survives, however
// many times the call is repeated.
func TestListAgentsDoesNotReapWhenHerdrIsDown(t *testing.T) {
	openTestDB(t)
	resetAgentReapMiss(t)
	if err := appendAgent("local", reapRec("alive", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	b := &listBackend{memBackend: newMemBackend(), down: true}
	stubListBackend(t, b)

	for i := 0; i < agentReapMisses+2; i++ {
		_, out, err := listAgentsTool(context.Background(), nil, listAgentsIn{})
		if err != nil {
			t.Fatalf("list_agents: %v", err)
		}
		if out.HerdrError == "" {
			t.Fatal("a failed herdr enumeration did not report herdr_error")
		}
		if len(out.Agents) != 1 || out.Agents[0].ID != "alive" {
			t.Fatalf("partial listing lost its records: %+v", out.Agents)
		}
	}
	if !liveIDs(t, "local")["alive"] {
		t.Error("an unreachable herdr reaped a record")
	}
}

// Every id list_agents hands back must resolve for the interaction tools —
// the property that failed on titan, where 136 records were listed as "ready"
// and every close_agent against them answered pane_not_found.
func TestListAgentsIDsAllResolve(t *testing.T) {
	openTestDB(t)
	resetAgentReapMiss(t)
	for i, pane := range []string{"w1:p1", "w2:p1", "w3:p1"} {
		if err := appendAgent("local", reapRec(fmt.Sprintf("a%d", i), pane)); err != nil {
			t.Fatal(err)
		}
	}
	b := &listBackend{memBackend: newMemBackend(), panes: []pane{
		{PaneID: "w2:p1", WorkspaceID: "w2", Agent: "claude", AgentStatus: "idle"},
	}}
	stubListBackend(t, b)

	var out listAgentsOut
	for i := 0; i < agentReapMisses; i++ {
		var err error
		if _, out, err = listAgentsTool(context.Background(), nil, listAgentsIn{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(out.Agents) == 0 {
		t.Fatal("listing is empty")
	}
	recs, err := listAgents("local")
	if err != nil {
		t.Fatal(err)
	}
	panes, err := hostHerdrPanesErr(b, "local")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range out.Agents {
		if !a.LassoCreated {
			continue
		}
		tgt, err := resolveTarget("local", a.ID, recs, panes)
		if err != nil {
			t.Errorf("listed agent %q does not resolve: %v", a.ID, err)
			continue
		}
		if _, ok := findPane(panes, tgt.PaneID); !ok {
			t.Errorf("listed agent %q resolves to pane %q that herdr does not have", a.ID, tgt.PaneID)
		}
	}
}
