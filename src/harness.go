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
	// SupportsPlanMode gates the "Start in plan mode" checkbox — only claude
	// has a plan mode today; on other harnesses the flag is silently ignored.
	SupportsPlanMode bool `json:"supports_plan_mode"`
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
	planMode  bool
	model     string
	extraArgs string
	prompt    string
}

var harnesses = []harnessDef{
	{
		ID:               "claude",
		Label:            "Claude Code",
		SupportsPlanMode: true,
		ModelSuggestions: []string{"opus", "sonnet", "haiku", "claude-fable-5"},
		buildCmd:         claudeCommand,
	},
	{
		ID:               "codex",
		Label:            "Codex",
		SupportsPlanMode: false,
		ModelSuggestions: []string{"gpt-5.1-codex-max", "gpt-5.1-codex", "gpt-5.1-codex-mini"},
		buildCmd:         codexCommand,
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
	if o.prompt != "" {
		cmd += " " + shellQuote(o.prompt)
	}
	return cmd
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
	return appendCommonArgs(cmd, o)
}
