package main

import "strings"

// Harness registry: the compiled-in table of AI agent CLIs lasso can launch.
// Each entry pairs the UI-facing metadata (label, plan-mode support, model
// suggestions — served to the creator via /api/agent-config) with the command
// builder that turns launch options into the shell line typed into the pane.
// Adding a harness (gemini, pi, …) means adding one entry here; the frontend
// and MCP schema pick it up without further plumbing (the remote-host
// provisioning script's herdr integration list is generated from it too).

// harnessDef describes one launchable agent CLI. The exported fields are
// serialized into the /api/agent-config response so the creator UI renders
// its choices from this table instead of a hardcoded list.
type harnessDef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// SupportsPlanMode gates the "Start in plan mode" checkbox — claude and
	// opencode are the harnesses with one lasso can request at launch. It is
	// about the LAUNCH LINE, not the CLI: omp has a plan mode reachable from
	// its TUI but no flag that starts a session in it, so it reads false here.
	// It is also the server-side gate: a plan
	// request for a harness without one is dropped by normalizePlanMode rather
	// than persisted, so the record can't claim an agent planned when it didn't.
	SupportsPlanMode bool `json:"supports_plan_mode"`
	// EffortLevels are the thinking/reasoning-effort levels this harness's CLI
	// accepts, cheapest first — they populate the creator's "Thinking effort"
	// select. Unlike ModelSuggestions this IS a closed set (the CLIs reject or
	// mis-handle anything else), so createAgent drops a level that isn't listed.
	// Empty means the harness has no effort knob and the select is hidden.
	EffortLevels []string `json:"effort_levels,omitempty"`
	// ModelSuggestions seed the creator's free-text model field. They are
	// suggestions only — anything the user types is passed through, since
	// model names churn far faster than lasso releases.
	ModelSuggestions []string `json:"model_suggestions"`
	buildCmd         func(o launchOpts) string
}

// launchOpts are the per-spawn knobs a harness builder consumes. model maps to
// the harness's model flag when non-empty; extraArgs is appended verbatim (an
// escape hatch for any flag lasso doesn't model — same trust level as a repo's
// setup script, which already runs arbitrary shell in the same pane).
type launchOpts struct {
	planMode bool
	model    string
	// effort is the thinking/reasoning-effort level, one of the harness's
	// EffortLevels; empty means "don't pass one" (the CLI's own default).
	effort    string
	extraArgs string
	prompt    string
	// promptFile, when set, is a file on the target host holding the prompt;
	// the launch line then carries `"$(cat <file>)"` in place of the inline
	// quoted prompt and prompt is ignored. Used when the prompt is too big or
	// multi-line to type into the pane's shell (see needsPromptFile).
	promptFile string
}

var harnesses = []harnessDef{
	{
		ID:               "claude",
		Label:            "Claude Code",
		SupportsPlanMode: true,
		EffortLevels:     []string{"low", "medium", "high", "xhigh", "max"},
		ModelSuggestions: []string{"fable", "opus", "sonnet", "haiku"},
		buildCmd:         claudeCommand,
	},
	{
		ID:               "codex",
		Label:            "Codex",
		SupportsPlanMode: false,
		EffortLevels:     []string{"minimal", "low", "medium", "high", "xhigh"},
		ModelSuggestions: []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"},
		buildCmd:         codexCommand,
	},
	{
		ID:               "opencode",
		Label:            "OpenCode",
		SupportsPlanMode: true,
		ModelSuggestions: []string{
			"kimi-for-coding/k3",
			"anthropic/claude-opus-4-5",
			"anthropic/claude-sonnet-4-5",
			"anthropic/claude-haiku-4-5",
			"openai/gpt-5.2",
			"openai/gpt-5.2-codex",
		},
		buildCmd: opencodeCommand,
	},
	{
		ID:               "omp",
		Label:            "Oh My Pi",
		SupportsPlanMode: false,
		// omp's --thinking ladder is pi-catalog's Effort enum plus two selectors
		// that aren't rungs on it: "off" disables reasoning outright, and "auto"
		// lets omp resolve a level per turn against the active model. Both are
		// listed (they're real choices a user wants) but "auto" goes last so the
		// ladder itself stays cheapest-first.
		EffortLevels: []string{"off", "minimal", "low", "medium", "high", "xhigh", "max", "auto"},
		// omp resolves --model as a pattern against the authenticated provider
		// catalogs — exact provider/id, exact bare id, then fuzzy/substring — so
		// a bare "opus" finds whichever claude-opus the user is logged into.
		ModelSuggestions: []string{
			"opus",
			"sonnet",
			"gpt-5.5",
			"gemini-3.1-pro",
			"anthropic/claude-opus-4-8",
		},
		buildCmd: ompCommand,
	},
	{
		ID:               "pi",
		Label:            "Pi",
		SupportsPlanMode: false,
		// Same ladder as omp (pi is omp's upstream), minus "auto" — pi's
		// --thinking takes concrete levels only.
		EffortLevels: []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"},
		ModelSuggestions: []string{
			"opus",
			"sonnet",
			"gpt-5.5",
			"gemini-3.1-pro",
			"anthropic/claude-opus-4-8",
		},
		buildCmd: piCommand,
	},
}

// harnessByID resolves an agent id to its definition, defaulting to claude —
// mirroring createAgent's historical `default:` behavior for unknown ids.
func harnessByID(id string) harnessDef {
	for _, h := range harnesses {
		if h.ID == id {
			return h
		}
	}
	return harnesses[0] // claude
}

// normalizeEffort trims/lowercases an effort level and keeps it only when the
// harness actually offers it. A level from an older/newer UI (or a harness the
// user switched away from) is dropped rather than passed through: an unknown
// --effort makes the CLI exit at launch, which would fail the whole boot.
func normalizeEffort(agent, effort string) string {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" {
		return ""
	}
	for _, lvl := range harnessByID(agent).EffortLevels {
		if lvl == e {
			return e
		}
	}
	return ""
}

// normalizePlanMode keeps a plan-mode request only when the harness actually
// has a plan mode. The failure it prevents is quieter than normalizeEffort's:
// an unknown --effort kills the CLI at launch, whereas a plan request codex
// can't honor is harmless AT BOOT — codexCommand simply builds no plan flag.
// The damage is to the record. Persisting plan_mode:true for an agent that
// launched in the default mode makes the creator's badge, get_agent and
// list_agents all report planning that never happened, and the launch line is
// long gone by the time anyone doubts it. Drop it so the record matches what
// actually ran.
func normalizePlanMode(agent string, planMode bool) bool {
	return planMode && harnessByID(agent).SupportsPlanMode
}

// agentCommand builds the shell command that launches the chosen agent. A
// non-empty prompt is passed as the agent's initial instruction; plan mode is
// requested when the harness supports it.
func agentCommand(agent string, o launchOpts) string {
	return harnessByID(agent).buildCmd(o)
}

// appendCommonArgs adds the harness-agnostic tail shared by every builder:
// the user's verbatim extra flags, then the prompt as the final positional
// arg. extraArgs is deliberately NOT quoted — it's free-form flags, and
// quoting would collapse them into one argument.
func appendCommonArgs(cmd string, o launchOpts) string {
	if e := strings.TrimSpace(o.extraArgs); e != "" {
		cmd += " " + e
	}
	if o.prompt != "" || o.promptFile != "" {
		cmd += " " + promptArg(o)
	}
	return cmd
}

// promptArg renders the launch line's prompt operand: the shell-quoted prompt
// itself, or — when it was staged to a file on the host (stageAgentPrompt) — a
// double-quoted command substitution the shell expands to a single argv
// argument at exec time. The substitution keeps the typed line short and
// newline-free no matter how large the prompt is; the only byte-level delta is
// that $() strips trailing newlines, which carry no meaning in a prompt.
func promptArg(o launchOpts) string {
	if o.promptFile != "" {
		return `"$(cat ` + shellQuote(o.promptFile) + `)"`
	}
	return shellQuote(o.prompt)
}

func claudeCommand(o launchOpts) string {
	// env -u scrubs the three CLAUDE_CODE_* session markers the lasso (and
	// herdr) daemon leaks because it was itself launched from inside a Claude
	// Code session. Claude Code 2.1.193+ treats their presence as "this is a
	// child session" and SUPPRESSES transcript persistence for an INTERACTIVE
	// agent — so the spawned agent writes no ~/.claude/projects/.../*.jsonl,
	// breaking resume/recovery and leaving nothing for restic to back up.
	// Scrubbing them per-launch restores normal transcript writing. This is
	// claude-specific (the codex builder needs no scrub); do not "clean it up".
	//
	// --allow-dangerously-skip-permissions rides on EVERY claude line, plan mode
	// included. It only MAKES bypass available — satisfying the accepted-disclaimer
	// gate that ~/.claude/settings.json's skipDangerousModePermissionPrompt
	// otherwise has to cover — without selecting it, so the effective mode still
	// comes from the host's settings (permissions.defaultMode) or from
	// --permission-mode below, and a plan agent still plans.
	//
	// The forcing variant, --dangerously-skip-permissions, stays off the line:
	// it FORCED bypass mode and silently overrode --permission-mode plan, so
	// plan agents never actually planned. Do not "simplify" the allow-variant
	// back into it.
	const envScrub = "env -u CLAUDE_CODE_CHILD_SESSION -u CLAUDECODE -u CLAUDE_CODE_SESSION_ID "
	cmd := envScrub + "claude --allow-dangerously-skip-permissions"
	if o.planMode {
		cmd += " --permission-mode plan"
	}
	if m := strings.TrimSpace(o.model); m != "" {
		cmd += " --model " + shellQuote(m)
	}
	if e := strings.TrimSpace(o.effort); e != "" {
		cmd += " --effort " + shellQuote(e)
	}
	return appendCommonArgs(cmd, o)
}

func codexCommand(o launchOpts) string {
	// --dangerously-bypass-approvals-and-sandbox is codex's analog of claude's
	// --dangerously-skip-permissions (lasso worktrees are already isolated), so
	// the agent runs autonomously instead of prompting per command. It does NOT
	// skip codex's boot-time "Do you trust this directory?" gate, though — that
	// dialog is auto-accepted via the trust goroutine in launchAgentInPane (a
	// config-file/-c pre-trust is fragile across the pane's shell). No
	// documented plan-mode flag, so plan agents launch in the default mode.
	cmd := "codex --dangerously-bypass-approvals-and-sandbox"
	if m := strings.TrimSpace(o.model); m != "" {
		cmd += " --model " + shellQuote(m)
	}
	// Codex has no --effort flag; reasoning effort is a config key, overridden
	// per-invocation with -c (the value parses as TOML, so a bare word is taken
	// as the literal string it is).
	if e := strings.TrimSpace(o.effort); e != "" {
		cmd += " -c " + shellQuote("model_reasoning_effort="+e)
	}
	return appendCommonArgs(cmd, o)
}

func opencodeCommand(o launchOpts) string {
	// --auto is opencode's analog of claude's --dangerously-skip-permissions:
	// it auto-approves every permission that isn't explicitly denied, so the
	// agent runs autonomously instead of prompting per action (lasso worktrees
	// are already isolated). Plan mode is opencode's built-in "plan" agent,
	// selected with --agent plan. Unlike claude/codex, opencode's TUI takes
	// the initial prompt via --prompt (not a positional arg), and models are
	// provider/model pairs. No reasoning-effort flag either (it's a per-model
	// setting in opencode's config), so EffortLevels stays empty and the
	// creator hides the select. No boot-time trust dialog to auto-accept.
	cmd := "opencode --auto"
	if o.planMode {
		cmd += " --agent plan"
	}
	if m := strings.TrimSpace(o.model); m != "" {
		cmd += " --model " + shellQuote(m)
	}
	if e := strings.TrimSpace(o.extraArgs); e != "" {
		cmd += " " + e
	}
	if o.prompt != "" || o.promptFile != "" {
		cmd += " --prompt " + promptArg(o)
	}
	return cmd
}

func ompCommand(o launchOpts) string {
	// --yolo (alias --auto-approve) overrides tools.approvalMode to "yolo" for
	// the run — omp's analog of claude's skip-permissions, so it never stops on
	// the `ask` picker (lasso worktrees are already isolated). Unlike claude and
	// codex there is no boot-time directory-trust dialog: omp dropped the
	// project-trust flow its upstream pi still has, so confirmAgentTrust has
	// nothing to accept here.
	//
	// No plan mode on the launch line. omp HAS one, but it is reached through
	// the TUI's /plan or the plan.defaultOnStartup setting — the `--plan` flag
	// only names the model the "plan" ROLE resolves to, and `--plan-yolo` starts
	// in plan mode and then auto-approves its own plan, which is the opposite of
	// the blocking gate lasso's plan_mode promises. SupportsPlanMode:false so a
	// plan request is dropped rather than recorded as planning that never
	// happened; wiring a real one means staging a settings file for --config.
	cmd := "omp --yolo"
	if m := strings.TrimSpace(o.model); m != "" {
		cmd += " --model " + shellQuote(m)
	}
	if e := strings.TrimSpace(o.effort); e != "" {
		cmd += " --thinking " + shellQuote(e)
	}
	if e := strings.TrimSpace(o.extraArgs); e != "" {
		cmd += " " + e
	}
	if o.prompt != "" || o.promptFile != "" {
		// The prompt rides after a POSIX "--" rather than as a bare trailing
		// positional. omp's parser lets a built-in string flag consume the very
		// next token even when it looks like a flag, so a value-taking flag in
		// the user's extra_args (`--plan`, `--fork`, …) would otherwise swallow
		// the prompt and boot the agent with no task at all. "--" ends option
		// parsing, so everything after it is message text either way. pi's
		// parser has no such separator, hence only omp gets this.
		cmd += " -- " + promptArg(o)
	}
	return cmd
}

func piCommand(o launchOpts) string {
	// pi needs no bypass flag: it has no permission gate to bypass — the tools
	// just run, and the README is explicit that confirmation flows are something
	// you add via an extension. So there is no analog of claude's
	// skip-permissions to pass, and no plan mode either (SupportsPlanMode:false).
	//
	// --approve is NOT that bypass. It settles pi's project-trust question for
	// this run — whether .pi/settings.json, project resources and project
	// extensions load — which interactive pi otherwise asks about at boot in any
	// repo carrying them, blocking the launch behind a dialog confirmAgentTrust
	// doesn't know. Trusting matches what lasso already does by spawning agents
	// with full permissions in the same worktree.
	cmd := "pi --approve"
	if m := strings.TrimSpace(o.model); m != "" {
		cmd += " --model " + shellQuote(m)
	}
	if e := strings.TrimSpace(o.effort); e != "" {
		cmd += " --thinking " + shellQuote(e)
	}
	return appendCommonArgs(cmd, o)
}
