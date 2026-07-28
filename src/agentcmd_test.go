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

// Neither claude launch line carries a skip-permissions flag: bypass mode is
// the host's ~/.claude/settings.json setting. Plan mode is therefore the only
// thing that distinguishes the two, via --permission-mode plan.
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
	// Either skip-permissions variant on the line would override plan mode and
	// leave the agent bypassing instead of planning.
	if strings.Contains(plan, "dangerously-skip-permissions") {
		t.Errorf("plan command must not force bypass mode: %q", plan)
	}

	def := agentCommand("claude", launchOpts{prompt: "do it"})
	if !strings.HasPrefix(def, envScrub) {
		t.Errorf("default command must scrub child-session env: %q", def)
	}
	// The default line carries no permission flags at all — bypass comes from
	// the host's settings.json, and plan mode is the only flagged variant.
	if strings.Contains(def, "dangerously-skip-permissions") ||
		strings.Contains(def, "--permission-mode") {
		t.Errorf("default command should carry no permission flags: %q", def)
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

// Thinking effort is harness-specific: claude takes a --effort flag, codex has
// none and needs the config key overridden with -c. Either way it lands before
// the prompt, and an empty effort emits nothing.
func TestAgentCommandEffort(t *testing.T) {
	cmd := agentCommand("claude", launchOpts{model: "opus", effort: "xhigh", prompt: "do it"})
	if !strings.Contains(cmd, "--effort 'xhigh'") {
		t.Errorf("claude command missing effort flag: %q", cmd)
	}
	if strings.Index(cmd, "--effort") > strings.Index(cmd, "'do it'") {
		t.Errorf("effort must precede the prompt: %q", cmd)
	}

	codex := agentCommand("codex", launchOpts{effort: "high", prompt: "do it"})
	if !strings.Contains(codex, "-c 'model_reasoning_effort=high'") {
		t.Errorf("codex command missing reasoning-effort override: %q", codex)
	}

	for _, agent := range []string{"claude", "codex", "opencode"} {
		bare := agentCommand(agent, launchOpts{prompt: "do it"})
		if strings.Contains(bare, "effort") {
			t.Errorf("empty effort must not emit a flag for %s: %q", agent, bare)
		}
	}
}

// Effort is a closed set per harness: a level the CLI doesn't know would abort
// the launch, so anything unlisted (including a level from another harness) is
// dropped rather than passed through. Case and padding are forgiving.
func TestNormalizeEffort(t *testing.T) {
	cases := []struct{ agent, in, want string }{
		{"claude", "high", "high"},
		{"claude", " High ", "high"},
		{"claude", "max", "max"},
		{"claude", "minimal", ""}, // codex-only level
		{"codex", "minimal", "minimal"},
		{"codex", "max", ""},     // claude-only level
		{"opencode", "high", ""}, // no effort knob at all
		{"claude", "", ""},
		{"claude", "turbo", ""},
	}
	for _, c := range cases {
		if got := normalizeEffort(c.agent, c.in); got != c.want {
			t.Errorf("normalizeEffort(%q, %q) = %q, want %q", c.agent, c.in, got, c.want)
		}
	}
}

// A plan request for a harness with no plan mode must be DROPPED, not recorded.
// codexCommand builds no plan flag, so persisting plan_mode:true would make the
// creator's badge, get_agent and list_agents all report planning that never
// happened — with the launch line long gone by the time anyone doubts it.
func TestNormalizePlanMode(t *testing.T) {
	cases := []struct {
		agent string
		in    bool
		want  bool
	}{
		{"claude", true, true},
		{"opencode", true, true},
		{"codex", true, false}, // codex has no plan mode: drop it
		{"codex", false, false},
		{"claude", false, false},
		{"", true, true}, // unknown ids default to claude, per harnessByID
		{"nonesuch", true, true},
	}
	for _, c := range cases {
		if got := normalizePlanMode(c.agent, c.in); got != c.want {
			t.Errorf("normalizePlanMode(%q, %v) = %v, want %v", c.agent, c.in, got, c.want)
		}
	}
}

// The drop has to happen where the record is built, not just in the helper —
// otherwise the launch is right and the persisted row still lies.
func TestCreateAgentDropsPlanModeForHarnessWithoutOne(t *testing.T) {
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
	b := &createAgentBackend{memBackend: newMemBackend()}
	prev := curBackend()
	setBackend(b)
	t.Cleanup(func() { setBackend(prev) })

	rec, err := createAgent(b, createAgentReq{
		Type: "scratch", Title: "codex planner", Prompt: "plan it",
		Agent: "codex", PlanMode: true, NoFocus: true,
	})
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}
	if rec.PlanMode {
		t.Error("codex agent persisted plan_mode:true, but codexCommand builds no plan flag — the record claims planning that never happened")
	}
}

// opencode must auto-approve permissions (--auto, its analog of claude's
// skip-permissions) so it runs autonomously, take its prompt via --prompt
// (the TUI has no positional prompt arg), and select plan mode via its
// built-in plan agent.
func TestAgentCommandOpencode(t *testing.T) {
	cmd := agentCommand("opencode", launchOpts{model: "kimi-for-coding/k3", prompt: "do it"})
	for _, want := range []string{"opencode --auto", "--model 'kimi-for-coding/k3'", "--prompt 'do it'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("opencode command missing %q: %q", want, cmd)
		}
	}
	if strings.Contains(cmd, "'do it' --prompt") || !strings.HasSuffix(cmd, "--prompt 'do it'") {
		t.Errorf("prompt must ride in via --prompt: %q", cmd)
	}

	plan := agentCommand("opencode", launchOpts{planMode: true, prompt: "do it"})
	if !strings.Contains(plan, "--agent plan") {
		t.Errorf("opencode plan command missing --agent plan: %q", plan)
	}

	def := agentCommand("opencode", launchOpts{prompt: "do it"})
	if strings.Contains(def, "--agent") {
		t.Errorf("non-plan opencode command must not pin an agent: %q", def)
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
