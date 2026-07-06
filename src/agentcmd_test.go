package main

import (
	"strings"
	"testing"
)

// agentPrompt hands the agent the full prompt verbatim (stored in Description;
// its first line is also the title), falling back to the title when no prompt
// body was stored, plus pointers to any notes/attachments.
func TestAgentPromptLeadsWithTitle(t *testing.T) {
	cases := []struct {
		name string
		rec  AgentRecord
		want string
	}{
		{
			name: "title only (no prompt body)",
			rec:  AgentRecord{Title: "Add dark mode"},
			want: "Add dark mode",
		},
		{
			name: "full prompt verbatim",
			rec: AgentRecord{
				Title:       "Add dark mode",
				Description: "Add dark mode\ntoggle in settings",
			},
			want: "Add dark mode\ntoggle in settings",
		},
		{
			name: "notes + attachments appended",
			rec: AgentRecord{
				Title:       "Add dark mode",
				Notes:       "see thread",
				Attachments: []string{"a.png", "b.png"},
			},
			want: "Add dark mode\n\nSee NOTES.md for additional notes.\n\nAttachments: a.png, b.png",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentPrompt(c.rec); got != c.want {
				t.Errorf("agentPrompt = %q, want %q", got, c.want)
			}
		})
	}
}

// In plan mode claude must get --allow-dangerously-skip-permissions, NOT the
// plain --dangerously-skip-permissions: the latter forces bypass mode and
// silently overrides --permission-mode plan, so the agent never plans.
func TestAgentCommandPlanModeFlags(t *testing.T) {
	// Both claude variants must scrub the leaked CLAUDE_CODE_* session markers so
	// 2.1.193+ doesn't treat the interactive agent as a child session and suppress
	// transcript persistence. The prefix must lead the command (it's an env wrapper
	// around the claude exec, not a claude flag).
	const envScrub = "env -u CLAUDE_CODE_CHILD_SESSION -u CLAUDECODE -u CLAUDE_CODE_SESSION_ID claude "

	plan := agentCommand("claude", launchOpts{planMode: true, prompt: "do it"})
	if !strings.HasPrefix(plan, envScrub) {
		t.Errorf("plan command must scrub child-session env: %q", plan)
	}
	if !strings.Contains(plan, "--permission-mode plan") {
		t.Errorf("plan command missing --permission-mode plan: %q", plan)
	}
	if !strings.Contains(plan, "--allow-dangerously-skip-permissions") {
		t.Errorf("plan command must use --allow-dangerously-skip-permissions: %q", plan)
	}
	// The bypass-forcing flag would override plan mode; it must not appear as a
	// standalone token (it's a prefix of the --allow- variant, so match a space).
	if strings.Contains(plan, " --dangerously-skip-permissions") {
		t.Errorf("plan command must not force bypass mode: %q", plan)
	}

	def := agentCommand("claude", launchOpts{prompt: "do it"})
	if !strings.HasPrefix(def, envScrub) {
		t.Errorf("default command must scrub child-session env: %q", def)
	}
	if !strings.Contains(def, "--dangerously-skip-permissions") ||
		strings.Contains(def, "--permission-mode") {
		t.Errorf("default command should bypass permissions without plan: %q", def)
	}
}

// codex must bypass approvals/sandbox (its analog of claude's skip-permissions)
// so it runs autonomously. Its boot-time trust dialog is handled separately by
// the trust goroutine, not a launch flag.
func TestAgentCommandCodexBypassesApprovals(t *testing.T) {
	cmd := agentCommand("codex", launchOpts{prompt: "do it"})
	if !strings.Contains(cmd, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("codex command missing bypass flag: %q", cmd)
	}
}

// A chosen model rides in via --model (shell-quoted — it's free user text),
// extra args are appended verbatim, and both land BEFORE the prompt so the
// prompt stays the final positional argument. Empty model/extra args add
// nothing at all.
func TestAgentCommandModelAndExtraArgs(t *testing.T) {
	cmd := agentCommand("claude", launchOpts{
		model:     "opus",
		extraArgs: "--append-system-prompt hi",
		prompt:    "do it",
	})
	wantOrder := []string{"--model 'opus'", "--append-system-prompt hi", "'do it'"}
	last := -1
	for _, w := range wantOrder {
		i := strings.Index(cmd, w)
		if i < 0 {
			t.Fatalf("command missing %q: %q", w, cmd)
		}
		if i < last {
			t.Fatalf("%q out of order (flags must precede the prompt): %q", w, cmd)
		}
		last = i
	}

	codex := agentCommand("codex", launchOpts{model: "gpt-5.1-codex", prompt: "do it"})
	if !strings.Contains(codex, "--model 'gpt-5.1-codex'") {
		t.Errorf("codex command missing model flag: %q", codex)
	}

	bare := agentCommand("claude", launchOpts{prompt: "do it"})
	if strings.Contains(bare, "--model") {
		t.Errorf("empty model must not emit a --model flag: %q", bare)
	}

	// Plan mode and model compose.
	planned := agentCommand("claude", launchOpts{planMode: true, model: "sonnet"})
	if !strings.Contains(planned, "--permission-mode plan") || !strings.Contains(planned, "--model 'sonnet'") {
		t.Errorf("plan mode + model should compose: %q", planned)
	}
}

// Unknown harness ids fall back to claude — the historical default for a
// createAgentReq with a bogus agent value.
func TestHarnessByIDDefaultsToClaude(t *testing.T) {
	if got := harnessByID("gemini-someday").ID; got != "claude" {
		t.Errorf("harnessByID(unknown) = %q, want claude", got)
	}
}

// titleSlug must cap a long single-paragraph prompt so the scratch dir / branch
// name built from it doesn't blow past the filesystem's 255-byte component limit
// (mkdir would fail with ENAMETOOLONG). It should also end on a whole word.
func TestTitleSlug(t *testing.T) {
	long := "Ticket 500 Tech Stack. See the imessage conversatoin I have with Ray Peters earlier today. track the need to get her the Ticket 500 Tech stack in todoist and let's start putting that together."
	slug := titleSlug(long)
	if len(slug) > maxSlugLen {
		t.Errorf("titleSlug len = %d, want <= %d (%q)", len(slug), maxSlugLen, slug)
	}
	if strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") {
		t.Errorf("titleSlug %q should not start/end with a dash", slug)
	}
	// A short title passes through unchanged.
	if got := titleSlug("Fix the bug"); got != "fix-the-bug" {
		t.Errorf("titleSlug(short) = %q, want fix-the-bug", got)
	}
}

// randSuffix tags scratch dirs to keep same-titled scratch agents distinct.
func TestRandSuffix(t *testing.T) {
	const ok = "abcdefghijklmnopqrstuvwxyz0123456789"
	s := randSuffix()
	if len(s) != 4 {
		t.Fatalf("randSuffix len = %d, want 4 (%q)", len(s), s)
	}
	if strings.Trim(s, ok) != "" {
		t.Errorf("randSuffix %q has chars outside %q", s, ok)
	}
}
