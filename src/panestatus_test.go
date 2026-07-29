package main

import "testing"

func TestTitleAgentStatusClaude(t *testing.T) {
	// The two title shapes herdr's own claude manifest matches on: a braille
	// spinner frame while working, "✳ " at rest. Both prove claude is live.
	for _, tc := range []struct {
		title  string
		status string
	}{
		{"⠐ Rename agent titles based on prompt content", "working"},
		{"⣿ still working", "working"},
		{"✳ Check Norm outline wiki connection", "idle"},
	} {
		status, distinctive := titleAgentStatus("claude", tc.title)
		if status != tc.status || !distinctive {
			t.Errorf("titleAgentStatus(claude, %q) = %q/%v, want %q/true", tc.title, status, distinctive, tc.status)
		}
	}
	// A shell prompt is the title a pane wears once claude exits — it must not
	// read as a live agent, or a stale session would leave a ghost behind.
	for _, title := range []string{"dev@norm: ~", "", "✳no space after the glyph", "⠐", "nvim"} {
		if status, distinctive := titleAgentStatus("claude", title); status != "" || distinctive {
			t.Errorf("titleAgentStatus(claude, %q) = %q/%v, want empty/false", title, status, distinctive)
		}
	}
}

func TestTitleAgentStatusCodex(t *testing.T) {
	// codex's spinner and its "Action Required" blocker are distinctive; a plain
	// title only hints at idle, since any shell would match it too.
	if status, distinctive := titleAgentStatus("codex", "⠹ building"); status != "working" || !distinctive {
		t.Errorf("codex spinner = %q/%v, want working/true", status, distinctive)
	}
	if status, distinctive := titleAgentStatus("codex", "Action Required: approve command"); status != "blocked" || !distinctive {
		t.Errorf("codex blocker = %q/%v, want blocked/true", status, distinctive)
	}
	if status, distinctive := titleAgentStatus("codex", "some task"); status != "idle" || distinctive {
		t.Errorf("codex plain title = %q/%v, want idle/false", status, distinctive)
	}
	// An unknown harness has no title convention lasso can read.
	if status, distinctive := titleAgentStatus("opencode", "✳ anything"); status != "" || distinctive {
		t.Errorf("unknown agent = %q/%v, want empty/false", status, distinctive)
	}
}

func TestPaneAgentPresenceHerdrIdentified(t *testing.T) {
	// herdr identified the agent and has a state for it: both are taken as-is,
	// title or no title.
	kind, status := paneAgentPresence(pane{
		Agent: "claude", AgentStatus: "working", TerminalTitle: "✳ says idle",
	})
	if kind != "claude" || status != "working" {
		t.Errorf("presence = %q/%q, want claude/working", kind, status)
	}
	// Identified but stateless — the title fills the gap rather than leaving the
	// pane reading "unknown".
	kind, status = paneAgentPresence(pane{
		Agent: "claude", AgentStatus: "unknown", TerminalTitle: "⠐ Working on it",
	})
	if kind != "claude" || status != "working" {
		t.Errorf("presence = %q/%q, want claude/working", kind, status)
	}
}

func TestPaneAgentPresenceRecoversUnidentifiedAgent(t *testing.T) {
	// The norm reproduction: herdr never identified the agent (no kind, status
	// "unknown", absent from agent.list) but the harness reported its session and
	// the title still shows claude's live chrome.
	kind, status := paneAgentPresence(pane{
		PaneID:        "w3:p1",
		AgentStatus:   "unknown",
		TerminalTitle: "✳ Check Norm outline wiki connection",
		AgentSession:  &agentSession{Source: "herdr:claude", Agent: "claude", Kind: "id", Value: "98bbd260"},
	})
	if kind != "claude" || status != "idle" {
		t.Errorf("presence = %q/%q, want claude/idle", kind, status)
	}
}

func TestPaneAgentPresenceIgnoresStaleSession(t *testing.T) {
	// herdr only clears a persisted session when it watches the agent process
	// exit — which on a host where it never identified the agent it never does.
	// A session whose pane is back to a shell prompt must not resurrect it.
	kind, status := paneAgentPresence(pane{
		PaneID:        "w3:p1",
		AgentStatus:   "unknown",
		TerminalTitle: "dev@norm: ~/projects/norm",
		AgentSession:  &agentSession{Source: "herdr:claude", Agent: "claude", Kind: "id", Value: "98bbd260"},
	})
	if kind != "" {
		t.Errorf("presence kind = %q, want empty for a session with no live agent", kind)
	}
	if status != "unknown" {
		t.Errorf("presence status = %q, want herdr's own answer preserved", status)
	}
	// A bare shell stays a bare shell.
	if kind, _ := paneAgentPresence(pane{PaneID: "w1:p4", TerminalTitle: "dev@norm: ~"}); kind != "" {
		t.Errorf("presence kind = %q, want empty for a plain shell", kind)
	}
}
