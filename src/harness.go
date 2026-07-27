package main

import "strings"

// Harness registry: the compiled-in table of AI agent CLIs lasso can launch.
// Each entry pairs the UI-facing metadata (label, plan-mode support, model
// suggestions — served to the creator via /api/agent-config) with the command
// builder that turns launch options into the shell line typed into the pane.
// Adding a harness (gemini, pi, …) means adding one entry here; the frontend
// and MCP schema pick it up without further plumbing.

// harnessDef describes one launchable agent CLI. The exported fields are
// serialized into the /api/agent-config response so the creator UI renders
// its choices from this table instead of a hardcoded list.
type harnessDef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// SupportsPlanMode gates the "Start in plan mode" checkbox — claude and
	// opencode have a plan mode today. It is also the server-side gate: a plan
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
		ModelSuggestions: []string{"opus", "sonnet", "haiku", "fable"},
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
	// --dangerously-skip-permissions forces bypass mode and silently overrides
	// --permission-mode plan, so plan agents never actually plan. In plan mode
	// use --allow-dangerously-skip-permissions instead, which only *enables*
	// bypassing and coexists with plan. Mirrors fulcrum's agent-commands.ts.
	const envScrub = "env -u CLAUDE_CODE_CHILD_SESSION -u CLAUDECODE -u CLAUDE_CODE_SESSION_ID "
	cmd := envScrub + "claude --dangerously-skip-permissions"
	if o.planMode {
		cmd = envScrub + "claude --allow-dangerously-skip-permissions --permission-mode plan"
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
