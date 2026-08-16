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

// Both claude launch lines carry --allow-dangerously-skip-permissions, which
// only makes bypass available (satisfying the accepted-disclaimer gate) without
// selecting it. Neither carries the forcing --dangerously-skip-permissions,
// which would override plan mode. Plan mode is therefore still the only thing
// that distinguishes the two, via --permission-mode plan.
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
		t.Errorf("plan command missing the allow-bypass flag: %q", plan)
	}
	// The forcing variant would override plan mode and leave the agent
	// bypassing instead of planning. Match it with its leading space so the
	// allow flag, which ends in the same token, doesn't count as a hit.
	if strings.Contains(plan, " --dangerously-skip-permissions") {
		t.Errorf("plan command must not force bypass mode: %q", plan)
	}

	def := agentCommand("claude", launchOpts{prompt: "do it"})
	if !strings.HasPrefix(def, envScrub) {
		t.Errorf("default command must scrub child-session env: %q", def)
	}
	if !strings.Contains(def, "--allow-dangerously-skip-permissions") {
		t.Errorf("default command missing the allow-bypass flag: %q", def)
	}
	// Bypass is made available, never forced, and plan mode is the only flagged
	// variant — so the default line selects no mode of its own.
	if strings.Contains(def, " --dangerously-skip-permissions") ||
		strings.Contains(def, "--permission-mode") {
		t.Errorf("default command should force no permission mode: %q", def)
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

	// omp and pi spell effort --thinking, so check the flag they'd actually
	// emit rather than the word "effort".
	for _, agent := range []string{"omp", "pi"} {
		cmd := agentCommand(agent, launchOpts{effort: "xhigh", prompt: "do it"})
		if !strings.Contains(cmd, "--thinking 'xhigh'") {
			t.Errorf("%s command missing thinking flag: %q", agent, cmd)
		}
		if strings.Index(cmd, "--thinking") > strings.Index(cmd, "'do it'") {
			t.Errorf("%s effort must precede the prompt: %q", agent, cmd)
		}
	}

	for _, agent := range []string{"claude", "codex", "opencode", "omp", "pi"} {
		bare := agentCommand(agent, launchOpts{prompt: "do it"})
		if strings.Contains(bare, "effort") || strings.Contains(bare, "--thinking") {
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
		{"omp", "auto", "auto"},
		{"omp", "off", "off"},
		{"pi", "off", "off"},
		{"pi", "auto", ""}, // omp-only selector; pi takes concrete levels
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
		// omp has no plan-mode FLAG, but plan.defaultOnStartup reaches the same
		// place through a --config overlay lasso stages itself, so the request is
		// honored and kept rather than dropped.
		{"omp", true, true},
		{"omp", false, false},
		{"codex", true, false}, // codex has no plan mode: drop it
		{"codex", false, false},
		{"pi", true, false}, // pi has no plan mode at all — not even a setting

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
	t.Cleanup(closeTestDB)
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

// omp must auto-approve its tool calls (--yolo, its analog of claude's
// skip-permissions) and take its prompt after a POSIX "--". The separator is
// what keeps a value-taking flag in the user's extra_args from swallowing the
// prompt: omp's parser lets a built-in string flag consume the very next token
// even when that token looks like a flag.
func TestAgentCommandOmp(t *testing.T) {
	cmd := agentCommand("omp", launchOpts{model: "opus", prompt: "do it"})
	for _, want := range []string{"omp --yolo", "--model 'opus'", "-- 'do it'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("omp command missing %q: %q", want, cmd)
		}
	}
	if !strings.HasSuffix(cmd, "-- 'do it'") {
		t.Errorf("prompt must be the final operand, after the separator: %q", cmd)
	}

	// A value-taking flag in extra_args must not be able to eat the prompt.
	extra := agentCommand("omp", launchOpts{extraArgs: "--plan", prompt: "do it"})
	if !strings.HasSuffix(extra, "--plan -- 'do it'") {
		t.Errorf("separator must sit between extra args and the prompt: %q", extra)
	}

	// No prompt, no separator — a bare "--" would be a pointless operand.
	if bare := agentCommand("omp", launchOpts{}); strings.Contains(bare, " -- ") {
		t.Errorf("promptless omp command should emit no separator: %q", bare)
	}
}

// omp's plan mode AND its theme pin are config settings, not flags, so the
// launch line carries a --config pointing at the overlay bootAgent staged. It
// must compose with the rest of the line, and — since the theme pin is on every
// omp agent — it rides whether or not plan mode was requested.
func TestAgentCommandOmpConfigOverlay(t *testing.T) {
	const overlay = "/home/u/.lasso/omp/a1.yml"
	plan := agentCommand("omp", launchOpts{
		planMode: true, configOverlay: overlay,
		model: "opus", effort: "high", extraArgs: "--add-dir /srv", prompt: "do it",
	})
	for _, want := range []string{
		"omp --yolo", "--config '" + overlay + "'",
		"--model 'opus'", "--thinking 'high'", "--add-dir /srv", "-- 'do it'",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("omp plan command missing %q: %q", want, plan)
		}
	}
	if !strings.HasSuffix(plan, "-- 'do it'") {
		t.Errorf("prompt must stay the final operand in plan mode: %q", plan)
	}
	// --config is repeatable and omp merges overlays LAST-WINS, so lasso's must
	// precede the user's extra_args or a user overlay could not refine it.
	if strings.Index(plan, "--config") > strings.Index(plan, "--add-dir") {
		t.Errorf("lasso's --config must precede extra_args: %q", plan)
	}

	// No plan mode, same overlay: it carries the theme pin, so it still rides.
	def := agentCommand("omp", launchOpts{configOverlay: overlay, prompt: "do it"})
	if !strings.Contains(def, "--config '"+overlay+"'") {
		t.Errorf("a non-plan omp command must still pass its overlay: %q", def)
	}
	// Nothing staged is not a launch path (bootAgent stages first); emitting a
	// bare --config with no operand would eat the next flag.
	if unstaged := agentCommand("omp", launchOpts{planMode: true, prompt: "do it"}); strings.Contains(unstaged, "--config") {
		t.Errorf("omp command must not emit --config without a staged overlay: %q", unstaged)
	}
}

// The overlay lasso ships must actually be the plan setting, and nothing else:
// every other omp default has to keep flowing through from the user's own
// config, which --config merges under (never over) this file.
func TestOmpPlanOverlayContents(t *testing.T) {
	// Comments are stripped: the rule is about what the overlay SETS, and the
	// header explains itself by naming the settings it deliberately leaves alone.
	var settings []string
	for _, line := range strings.Split(string(ompPlanOverlay), "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			settings = append(settings, strings.TrimRight(line, " \t"))
		}
	}
	got := strings.Join(settings, "\n")
	want := "plan:\n  enabled: true\n  defaultOnStartup: true"
	if got != want {
		t.Errorf("omp plan overlay must set the plan keys and nothing else\n got:\n%s\nwant:\n%s", got, want)
	}
}

// pi has no permission gate to bypass and no plan mode. --approve is not a
// bypass flag: it settles pi's project-trust question so an interactive launch
// in a repo carrying .pi resources doesn't block on a dialog at boot.
func TestAgentCommandPi(t *testing.T) {
	cmd := agentCommand("pi", launchOpts{model: "sonnet", prompt: "do it"})
	for _, want := range []string{"pi --approve", "--model 'sonnet'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("pi command missing %q: %q", want, cmd)
		}
	}
	// pi's parser has no "--" separator — it would land in unknownFlags — so
	// the prompt is a plain trailing positional.
	if !strings.HasSuffix(cmd, "'do it'") || strings.Contains(cmd, " -- ") {
		t.Errorf("pi prompt must be a bare trailing positional: %q", cmd)
	}
}

// Every harness id must be distinct: harnessByID resolves first-match and the
// provisioning script installs one herdr integration per id, so a duplicate
// would silently shadow a harness.
func TestHarnessIDsUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, h := range harnesses {
		if seen[h.ID] {
			t.Errorf("duplicate harness id %q", h.ID)
		}
		seen[h.ID] = true
		if h.Label == "" || h.buildCmd == nil {
			t.Errorf("harness %q needs a label and a command builder", h.ID)
		}
	}
}

// The remote provisioning script installs herdr's agent-state integration for
// every harness lasso can spawn. The list is substituted from the registry, so
// a newly added harness can't be left screen-scraped on remote hosts.
func TestProvisionScriptCoversEveryHarness(t *testing.T) {
	if strings.Contains(provisionScript, harnessIDsPlaceholder) {
		t.Fatalf("provisionScript still carries the unsubstituted placeholder")
	}
	want := "for agent in " + strings.Join(harnessIDs(), " ") + "; do"
	if !strings.Contains(provisionScript, want) {
		t.Errorf("provisionScript missing %q", want)
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
