package main

import (
	"strings"
	"testing"
)

// targetRecords are the host's lasso-created agents.
func targetRecords() []AgentRecord {
	return []AgentRecord{
		{ID: "a1", Title: "clem", RootPane: "w1-1"},
		{ID: "a2", Title: "builder", RootPane: "w2-1"},
	}
}

// targetPanes are the host's live herdr panes: two lasso-owned (w1-1, w2-1) and
// two foreign sessions lasso never created (a bot "Clem (OCAI)" and "Ticket
// 500"), plus a bare shell that carries no agent.
func targetPanes() []gridPane {
	return []gridPane{
		{PaneID: "w1-1", WorkspaceID: "w1", WorkspaceLabel: "clem", Agent: "claude", AgentStatus: "idle", HasAgent: true},
		{PaneID: "w2-1", WorkspaceID: "w2", WorkspaceLabel: "builder", Agent: "codex", AgentStatus: "working", HasAgent: true},
		{PaneID: "w9-1", WorkspaceID: "w9", WorkspaceLabel: "Clem (OCAI)", Agent: "claude", AgentStatus: "working", HasAgent: true},
		{PaneID: "w8-1", WorkspaceID: "w8", WorkspaceLabel: "Ticket 500", Agent: "claude", AgentStatus: "idle", HasAgent: true},
		{PaneID: "w7-1", WorkspaceID: "w7", WorkspaceLabel: "scratch", HasAgent: false},
	}
}

func TestResolveTargetByID(t *testing.T) {
	// Exact lasso id resolves to its record (the pre-existing id-based path).
	got, err := resolveTarget("local", "a1", targetRecords(), targetPanes())
	if err != nil || got.Record == nil || got.Record.ID != "a1" || got.PaneID != "w1-1" {
		t.Fatalf("a1 -> %+v (err %v), want lasso a1 on w1-1", got, err)
	}
	if got.Pane != nil {
		t.Errorf("id match should not carry a foreign pane: %+v", got.Pane)
	}
}

func TestResolveTargetByPaneID(t *testing.T) {
	// A lasso-owned pane id maps back to its record...
	got, err := resolveTarget("local", "w2-1", targetRecords(), targetPanes())
	if err != nil || got.Record == nil || got.Record.ID != "a2" {
		t.Fatalf("w2-1 -> %+v (err %v), want lasso a2", got, err)
	}
	// ...a foreign pane id resolves to the session, with no lasso record.
	got, err = resolveTarget("local", "w9-1", targetRecords(), targetPanes())
	if err != nil || got.Record != nil || got.Pane == nil || got.PaneID != "w9-1" {
		t.Fatalf("w9-1 -> %+v (err %v), want foreign pane w9-1", got, err)
	}
}

func TestResolveTargetByName(t *testing.T) {
	// A lasso agent's title resolves to its record, case-insensitively.
	got, err := resolveTarget("local", "BUILDER", targetRecords(), targetPanes())
	if err != nil || got.Record == nil || got.Record.ID != "a2" {
		t.Fatalf("BUILDER -> %+v (err %v), want lasso a2", got, err)
	}
	// A foreign session's sidebar name resolves to its pane — this is the clem
	// bug: "Clem (OCAI)" is a herdr session lasso did not create.
	got, err = resolveTarget("local", "clem (ocai)", targetRecords(), targetPanes())
	if err != nil || got.Record != nil || got.Pane == nil || got.PaneID != "w9-1" {
		t.Fatalf("clem (ocai) -> %+v (err %v), want foreign pane w9-1", got, err)
	}
}

func TestResolveTargetNameAmbiguous(t *testing.T) {
	// A foreign session sharing a lasso agent's name makes "clem" ambiguous: it
	// must be refused with BOTH candidates listed, never guessed.
	panes := append(targetPanes(), gridPane{PaneID: "w5-1", WorkspaceLabel: "clem", Agent: "claude", AgentStatus: "idle", HasAgent: true})
	_, err := resolveTarget("local", "clem", targetRecords(), panes)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("clem ambiguity: err = %v, want an ambiguous-match error", err)
	}
	if !strings.Contains(err.Error(), "a1@local") || !strings.Contains(err.Error(), "w5-1") {
		t.Fatalf("ambiguity error should list both candidates: %v", err)
	}
}

func TestResolveTargetNotFoundAndBareShell(t *testing.T) {
	if _, err := resolveTarget("local", "nobody", targetRecords(), targetPanes()); err == nil {
		t.Error("unknown target resolved, want error")
	}
	// A bare shell (no agent) is not addressable by its sidebar name.
	if _, err := resolveTarget("local", "scratch", targetRecords(), targetPanes()); err == nil {
		t.Error("bare-shell name resolved, want error")
	}
	if _, err := resolveTarget("local", "  ", targetRecords(), targetPanes()); err == nil {
		t.Error("blank needle resolved, want error")
	}
}

func TestSidebarNameFallback(t *testing.T) {
	// Workspace label wins; then the pane's own label; then the tab label.
	if got := sidebarName(gridPane{WorkspaceLabel: "ws", PaneLabel: "pn", TabLabel: "tb"}); got != "ws" {
		t.Errorf("sidebarName = %q, want ws", got)
	}
	if got := sidebarName(gridPane{PaneLabel: "pn", TabLabel: "tb"}); got != "pn" {
		t.Errorf("sidebarName = %q, want pn", got)
	}
	if got := sidebarName(gridPane{TabLabel: "tb"}); got != "tb" {
		t.Errorf("sidebarName = %q, want tb", got)
	}
	if got := sidebarName(gridPane{}); got != "" {
		t.Errorf("sidebarName = %q, want empty", got)
	}
}

func TestAgentInfoLassoCreatedFlag(t *testing.T) {
	// A lasso record is flagged created; a foreign pane is not, and carries its
	// sidebar name as both the address and the human title.
	if ai := agentInfoFrom("local", AgentRecord{ID: "a1", Title: "clem"}, "idle"); !ai.LassoCreated {
		t.Error("agentInfoFrom should set lasso_created=true")
	}
	ai := agentInfoFromPane("local", gridPane{PaneID: "w9-1", WorkspaceLabel: "Clem (OCAI)", Agent: "claude", AgentStatus: "working"})
	if ai.LassoCreated {
		t.Error("agentInfoFromPane should set lasso_created=false")
	}
	if ai.SidebarName != "Clem (OCAI)" || ai.Title != "Clem (OCAI)" || ai.RootPane != "w9-1" || ai.ID != "" {
		t.Errorf("foreign pane info = %+v, want name/pane populated and no lasso id", ai)
	}
}
