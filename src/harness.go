package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

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
	// opencode have a plan mode today; on other harnesses the flag is silently
	// ignored.
	SupportsPlanMode bool `json:"supports_plan_mode"`
	// ModelSuggestions seed the creator's free-text model field. They are
	// suggestions only — anything the user types is passed through, since
	// model names churn far faster than lasso releases.
	ModelSuggestions []string `json:"model_suggestions"`
	// DefaultModel is the model this harness's CLI is itself configured to use
	// on the target host (e.g. Claude Code's configured model) — the creator
	// seeds its model field with it so a new agent defaults to the same model
	// the harness would run on its own. Empty means "no pinned model, the CLI
	// picks its default" (for claude, the account/org default). It is NOT part
	// of the static table: it's resolved per host at serve time (see
	// resolveHarnesses / the defaultModel resolver), so the compiled-in value is
	// always "".
	DefaultModel string `json:"default_model,omitempty"`
	buildCmd     func(o launchOpts) string
	// defaultModel resolves DefaultModel for backend b (the host the agent will
	// run on). nil when the harness has no discoverable configured model.
	defaultModel func(b Backend) string
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
		ModelSuggestions: []string{"opus", "sonnet", "haiku", "claude-fable-5"},
		buildCmd:         claudeCommand,
		defaultModel:     claudeConfiguredModel,
	},
	{
		ID:               "codex",
		Label:            "Codex",
		SupportsPlanMode: false,
		ModelSuggestions: []string{"gpt-5.1-codex-max", "gpt-5.1-codex", "gpt-5.1-codex-mini"},
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

// resolveHarnesses returns a copy of the registry with each harness's
// DefaultModel filled in for backend b — the host the agent will run on — so
// the creator can seed its model field with the model that harness's CLI is
// itself configured to use there. The static table's DefaultModel is always
// empty (it's a per-host runtime value), so we copy rather than mutate the
// shared slice. Best-effort per harness: a resolver that can't read the host's
// config just yields "".
func resolveHarnesses(b Backend) []harnessDef {
	out := make([]harnessDef, len(harnesses))
	copy(out, harnesses)
	for i := range out {
		if out[i].defaultModel != nil {
			out[i].DefaultModel = out[i].defaultModel(b)
		}
	}
	return out
}

// claudeConfiguredModel returns the model Claude Code itself is configured to
// use on backend b — the model a bare `claude` (no --model) would run with —
// or "" when nothing is pinned (Claude Code then uses the account/org default,
// so no --model is the right thing to launch with). It mirrors Claude Code's
// own resolution, most-specific first:
//
//  1. ANTHROPIC_MODEL in the environment (local host only — we can't see a
//     remote daemon's env, and claudeCommand does not scrub this var, so a
//     spawned agent inherits it).
//  2. "model" in ~/.claude/settings.json (the user settings file).
//  3. top-level "model" in ~/.claude.json, where the interactive /model command
//     persists its choice.
//
// A "default" sentinel (Claude Code's marker for "use the account default") is
// treated as unset, so we surface/launch nothing rather than the literal word.
func claudeConfiguredModel(b Backend) string {
	if _, ok := b.(*localBackend); ok {
		if m := normalizeClaudeModel(os.Getenv("ANTHROPIC_MODEL")); m != "" {
			return m
		}
	}
	home, err := b.HomeDir()
	if err != nil || home == "" {
		return ""
	}
	if m := claudeModelFromJSON(b, filepath.Join(home, ".claude", "settings.json")); m != "" {
		return m
	}
	return claudeModelFromJSON(b, filepath.Join(home, ".claude.json"))
}

// claudeModelFromJSON reads a top-level string "model" key from a JSON file on
// backend b, returning "" if the file is missing/unreadable, isn't a JSON
// object, or the key is absent or non-string. Only that one key is decoded —
// ~/.claude.json in particular is large, so we avoid unmarshaling the whole
// document.
func claudeModelFromJSON(b Backend, path string) string {
	data, err := b.ReadFile(path)
	if err != nil {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		return ""
	}
	raw, ok := obj["model"]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return normalizeClaudeModel(s)
}

// normalizeClaudeModel trims a configured model value and maps Claude Code's
// "default" sentinel (and empty) to "" — meaning "no pinned model".
func normalizeClaudeModel(m string) string {
	m = strings.TrimSpace(m)
	if strings.EqualFold(m, "default") {
		return ""
	}
	return m
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

func opencodeCommand(o launchOpts) string {
	// --auto is opencode's analog of claude's --dangerously-skip-permissions:
	// it auto-approves every permission that isn't explicitly denied, so the
	// agent runs autonomously instead of prompting per action (lasso worktrees
	// are already isolated). Plan mode is opencode's built-in "plan" agent,
	// selected with --agent plan. Unlike claude/codex, opencode's TUI takes
	// the initial prompt via --prompt (not a positional arg), and models are
	// provider/model pairs. No boot-time trust dialog to auto-accept.
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
