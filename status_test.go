package main

import (
	"testing"
	"time"
)

// TestPollOnceIgnoresDeadAgents verifies the new process-based model: a tab
// marked kind=agent in the DB but with no running agent process is NOT tracked
// (agent-ness is live now — see procscan.go). pollOnce keys off
// tabAgentKinds(), which finds no agent here, so the cache stays empty.
func TestPollOnceIgnoresDeadAgents(t *testing.T) {
	openTestDB(t)
	_ = appendAgent("local", AgentRecord{ID: "a1", Title: "T", Type: "scratch", Agent: "claude", WorkDir: "/x", CreatedAt: time.Now()})
	_ = insertWorkspace(Workspace{ID: "wa1", Host: "local", Title: "T", WorkDir: "/x", Kind: "scratch"})
	_ = insertTab(Tab{ID: "a1", WorkspaceID: "wa1", Title: "T", Cwd: "/x", Kind: "agent", AgentID: "a1"})

	agentStatuses.pollOnce()
	if got := agentStatuses.status("", "a1"); got != StatusUnknown {
		t.Errorf("status = %q, want unknown (no live agent process)", got)
	}
	if snap := agentStatuses.snapshot(); len(snap) != 0 {
		t.Errorf("snapshot = %+v, want empty (dead agent not tracked)", snap)
	}
}

func TestTreeSignatureChanges(t *testing.T) {
	openTestDB(t)
	base := treeSignature()
	_ = insertWorkspace(Workspace{ID: "w1", Host: "local", Title: "A", Kind: "scratch"})
	if treeSignature() == base {
		t.Error("signature should change after adding a workspace")
	}
	withWs := treeSignature()
	_ = renameWorkspace("w1", "renamed")
	if treeSignature() == withWs {
		t.Error("signature should change after a rename")
	}
}
