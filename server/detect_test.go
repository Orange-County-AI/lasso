package main

import (
	"testing"
	"time"
)

// Fixtures approximate what `tmux capture-pane -p` yields for each agent in each
// state. They key off the structural chrome the heuristics match
// (prompt box rules, spinner glyph + ellipsis, "esc to interrupt", codex's "›"
// prompt and "• Working (" markers, permission phrases).

const claudeIdle = `
 Some earlier assistant output here.

────────────────────────────────────────
❯
────────────────────────────────────────
  ? for shortcuts
`

const claudeWorking = `
 Reading files…

✻ Pondering… (esc to interrupt)

────────────────────────────────────────
❯
────────────────────────────────────────
`

const claudeWorkingInterrupt = `
 Running a tool

  Doing the thing (esc to interrupt)

────────────────────────────────────────
❯
────────────────────────────────────────
`

const claudeBlocked = `
 Bash(rm -rf /tmp/x)

 Do you want to proceed?
 ❯ 1. Yes
   2. No, and tell Claude what to do differently

 tab to amend · ctrl+e to explain
`

// An at-rest agent (e.g. the 52 Labs Orchestrator) whose last assistant turn
// happened to ask a question. The transcript scrollback contains "Do you want…"
// prose, and the live empty composer renders a bare "❯" caret below it. The old
// whole-screen heuristic matched "do you want" + a trailing "❯" (the empty
// caret) and mis-reported this idle agent as "blocked". A live empty prompt box
// means the agent is at rest waiting for input — never blocked.
const claudeIdleAskedQuestion = `
  I've finished wiring up the webhook handler and it's green.
  Do you want me to also add a retry on the channel publish?

※ recap: handler done; offered to add a publish retry next.

────────────────────────────────────────
❯
────────────────────────────────────────
  ? for shortcuts
`

func TestDetectClaude(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		want   AgentStatus
	}{
		{"idle", claudeIdle, StatusIdle},
		{"working-spinner", claudeWorking, StatusWorking},
		{"working-interrupt", claudeWorkingInterrupt, StatusWorking},
		{"blocked", claudeBlocked, StatusBlocked},
		{"idle-asked-question", claudeIdleAskedQuestion, StatusIdle},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectClaudeRaw(c.screen); got != c.want {
				t.Errorf("detectClaudeRaw = %q, want %q", got, c.want)
			}
		})
	}
}

// The "esc to interrupt" footer BELOW the prompt box must not count as working —
// only chrome ABOVE the box does (otherwise an idle prompt with a help footer
// would read as working).
func TestClaudeFooterNotWorking(t *testing.T) {
	screen := `
 output

────────────────────────────────────────
❯
────────────────────────────────────────
 esc to interrupt
`
	if got := detectClaudeRaw(screen); got != StatusIdle {
		t.Errorf("footer-only interrupt = %q, want idle", got)
	}
}

const codexIdle = `
• Ran a command
  └ done

› `

const codexWorking = `
• Working (12s • esc to interrupt)

› `

const codexBlockedStrong = `
  $ rm -rf build

  Allow command?
  press enter to confirm or esc to cancel
`

const codexBlockedWeak = `
 Continue? [y/n]
`

func TestDetectCodex(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		want   AgentStatus
	}{
		{"idle", codexIdle, StatusIdle},
		{"working", codexWorking, StatusWorking},
		{"blocked-strong", codexBlockedStrong, StatusBlocked},
		{"blocked-weak", codexBlockedWeak, StatusBlocked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectCodex(c.screen); got != c.want {
				t.Errorf("detectCodex = %q, want %q", got, c.want)
			}
		})
	}
}

// A codex working marker that sits BEFORE the current prompt (no block marker
// after the prompt) reads working; once a new block marker appears after the
// prompt the region is no longer "current" and it's not idle via that path.
func TestCodexWorkingBeforePrompt(t *testing.T) {
	screen := "• Working (3s • esc to interrupt)\n\n› "
	if got := detectCodex(screen); got != StatusWorking {
		t.Errorf("= %q, want working", got)
	}
}

func TestStabilizeClaudeWorkingHold(t *testing.T) {
	now := time.Now()
	// Brief idle right after working → held at working.
	if got := stabilizeClaude(StatusWorking, StatusIdle, now, now.Add(-500*time.Millisecond)); got != StatusWorking {
		t.Errorf("within hold = %q, want working", got)
	}
	// Idle long after working → genuinely idle.
	if got := stabilizeClaude(StatusWorking, StatusIdle, now, now.Add(-2*time.Second)); got != StatusIdle {
		t.Errorf("past hold = %q, want idle", got)
	}
	// Raw working always working (and stamps lastWorking via caller).
	if got := stabilizeClaude(StatusIdle, StatusWorking, now, time.Time{}); got != StatusWorking {
		t.Errorf("raw working = %q, want working", got)
	}
}

func TestDetectAgentStatusDispatch(t *testing.T) {
	now := time.Now()
	var lw time.Time
	if got := detectAgentStatus("claude", claudeWorking, StatusIdle, &lw, now); got != StatusWorking {
		t.Errorf("claude dispatch = %q, want working", got)
	}
	if lw.IsZero() {
		t.Error("lastWorking should be stamped when raw working")
	}
	if got := detectAgentStatus("codex", codexIdle, StatusWorking, nil, now); got != StatusIdle {
		t.Errorf("codex dispatch = %q, want idle", got)
	}
	if got := detectAgentStatus("unknown-agent", "whatever", StatusIdle, nil, now); got != StatusUnknown {
		t.Errorf("unknown dispatch = %q, want unknown", got)
	}
}
