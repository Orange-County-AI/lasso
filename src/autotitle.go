package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Auto-titling: an agent's title is the first line of its prompt (see
// promptTitle), which is what the branch, the work dir, the herdr workspace
// label and therefore the agents sidebar all get named after. That first line
// is written for the agent, not for a sidebar — a pasted paragraph gives every
// entry the same opening words, and anything long is simply clipped mid-word.
//
// So after a create (from the UI or the MCP create_agent tool) lasso asks a
// local agent CLI to distill the whole prompt into a few words and renames the
// workspace + record to that. Gated by the auto_title_agents setting (default
// on); the Settings tab exposes it as "Auto-title new agents".
//
// Three things are deliberate here:
//
//   - It runs on the LASSO host, never the host the agent was created on. The
//     titler is a one-shot local process (an API round-trip, no repo access),
//     so making it follow the agent to a remote box would mean shipping the
//     prompt over SSH and requiring a logged-in CLI on every host in the fleet
//     for a cosmetic rename. Only the resulting rename travels to the agent's
//     host, over the herdr RPC that host is already driven by.
//
//   - Only the DISPLAY name changes. The branch, work dir and prompt file were
//     all named from the original title before the CLI could answer, and
//     renaming a live worktree's branch out from under a booting agent is a far
//     bigger blast radius than a nicer label is worth.
//
//   - An explicit title is never overwritten. The web creator has no title
//     field (it always derives from the prompt), but an MCP caller that passed
//     one meant it.

// autoTitleKey is the settings-table key for the toggle. Unset means on.
const autoTitleKey = "auto_title_agents"

// autoTitleEnabled reports whether auto-titling is on (the default). A nil db
// (tests, shutdown) reads as on — the caller still has to be past createAgent
// for that to matter.
func autoTitleEnabled() bool {
	if db == nil {
		return true
	}
	v, err := getSetting(autoTitleKey)
	if err != nil || v == "" {
		return true
	}
	on, err := strconv.ParseBool(v)
	return err != nil || on
}

// ---------------------------------------------------------------------------
// GET/POST /api/auto-title — the Settings tab's toggle
// ---------------------------------------------------------------------------

func serveAutoTitle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{"enabled": autoTitleEnabled()})
	case http.MethodPost:
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Enabled == nil {
			http.Error(w, "enabled required", http.StatusBadRequest)
			return
		}
		if err := setSetting(autoTitleKey, strconv.FormatBool(*req.Enabled)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"enabled": autoTitleEnabled()})
	default:
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// the rename itself
// ---------------------------------------------------------------------------

// autoTitleAgent generates a title for rec from its prompt and applies it to
// the agent's herdr workspace (what the agents sidebar shows) and to the
// persisted record (the address list_agents / message_agent surface). Runs in
// its own goroutine off createAgent, alongside the boot: nothing downstream
// waits on the title, and the CLI takes seconds.
//
// Every failure past "the feature is off" is user-visible — a toast, not just a
// log line — because the whole feature is a rename the user is watching the
// sidebar for. b is the agent's own host backend (the rename travels there);
// the titler CLI runs locally regardless.
func autoTitleAgent(b Backend, host string, rec AgentRecord) {
	title, err := generateAgentTitle(rec.Description)
	if err != nil {
		log.Printf("agent %s on %s: auto-title failed: %v", rec.ID, host, err)
		notifyUI(notice{
			Level:  "error",
			Title:  fmt.Sprintf("Couldn't auto-title %q", rec.Title),
			Detail: "No local agent CLI could name it — " + err.Error(),
		})
		return
	}
	if title == rec.Title {
		return
	}
	if _, err := b.HerdrCall("workspace.rename", map[string]any{
		"workspace_id": rec.WorkspaceID,
		"label":        title,
	}); err != nil {
		log.Printf("agent %s on %s: auto-title rename failed: %v", rec.ID, host, err)
		notifyUI(notice{
			Level:  "error",
			Title:  fmt.Sprintf("Couldn't rename %q to %q", rec.Title, title),
			Detail: err.Error(),
		})
		return
	}
	// Only the workspace. The herdr TAB is deliberately left alone: it's the
	// user's own organization of the terminal, shared with whatever else they
	// put in it, and retitling it puts a generated sentence across the top of
	// the screen — a rename they never asked for in a place that isn't the
	// agent's.
	if err := updateAgentTitle(rec.ID, host, title); err != nil {
		log.Printf("agent %s on %s: auto-title record update failed: %v", rec.ID, host, err)
	}
	// The cross-host pane listing is served from a short-lived cache; drop it so
	// the next poll shows the new name instead of the old one.
	invalidatePanesCache()
	log.Printf("agent %s on %s: auto-titled %q -> %q", rec.ID, host, rec.Title, title)
}

// ---------------------------------------------------------------------------
// asking a local agent CLI for the title
// ---------------------------------------------------------------------------

// titler is one CLI lasso will ask for a title, in the order listed: claude
// first, then codex, opencode, omp, pi. Each runs one non-interactive turn with the
// instruction as its last argument (argv, so no quoting is involved) and prints
// the answer — and only the answer — on stdout; their progress chatter,
// sandbox warnings and session banners all go to stderr.
type titler struct {
	id string
	// args precede the instruction on the command line.
	args []string
}

var titlers = []titler{
	{id: "claude", args: []string{"-p"}},
	// --skip-git-repo-check: the titler runs from the home dir, which usually
	// isn't a repo, and codex exec otherwise refuses to start there.
	{id: "codex", args: []string{"exec", "--skip-git-repo-check"}},
	{id: "opencode", args: []string{"run"}},
	// -p is print mode for both: one turn, answer on stdout, exit. Neither
	// needs a repo (they run from the home dir), and pi skips its project-trust
	// prompt entirely in non-interactive modes.
	{id: "omp", args: []string{"-p"}},
	{id: "pi", args: []string{"-p"}},
}

// titlerTimeout caps one CLI's turn. Generous — a first launch pays for config
// loading and MCP handshakes — but bounded, so a hung or rate-limited CLI falls
// through to the next one instead of pinning the goroutine forever. A var so
// tests can shrink it.
var titlerTimeout = 90 * time.Second

// titlerRunner runs one titler and returns its stdout. A package var so tests
// can substitute a fake instead of shelling out to a real agent CLI.
var titlerRunner = runTitler

// generateAgentTitle asks each titler in turn for a title, returning the first
// clean answer. The error, when every one fails, names what each did — the
// toast's whole job is telling the user WHICH part of the chain is missing
// (a CLI that isn't installed reads very differently from one that isn't
// logged in).
func generateAgentTitle(prompt string) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", errors.New("no prompt to summarize")
	}
	instruction := titleInstruction(prompt)
	var fails []string
	for _, t := range titlers {
		out, err := titlerRunner(t, instruction)
		if err != nil {
			fails = append(fails, t.id+": "+err.Error())
			continue
		}
		if title := cleanTitle(out); title != "" {
			return title, nil
		}
		fails = append(fails, t.id+": answered with no usable title")
	}
	return "", errors.New(strings.Join(fails, "; "))
}

// titlePromptLimit caps how much of the prompt rides into the instruction. A
// title comes out of the opening paragraphs; the rest is context the titler
// would pay tokens (and latency) to read past.
const titlePromptLimit = 4000

// titleInstruction wraps the agent's prompt in the ask. The rules exist because
// the titlers are CODING agents: without "do not attempt the task" a capable
// one will happily start editing files instead of naming the work.
func titleInstruction(prompt string) string {
	if len(prompt) > titlePromptLimit {
		prompt = strings.ToValidUTF8(prompt[:titlePromptLimit], "")
	}
	return `Name the coding task below, for display in a sidebar list of running agents.

Rules:
- 3 to 6 words, at most ` + strconv.Itoa(autoTitleMaxLen) + ` characters.
- Say what the task IS, specifically enough to tell it apart from similar work.
- No trailing punctuation, no quotes, no markdown.
- Reply with the title alone — no preamble, no explanation, no alternatives.
- Do NOT attempt the task, read any files, or use any tools.

Task:
` + prompt
}

// runTitler shells out to one titler CLI and returns its stdout.
func runTitler(t titler, instruction string) (string, error) {
	bin, ok := titlerBin(t.id)
	if !ok {
		return "", errors.New("not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), titlerTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append(append([]string{}, t.args...), instruction)...)
	// Run from the home dir: it's the one directory these CLIs can be expected
	// to already trust, and the titler has no business in any repo.
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	cmd.Env = titlerEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("timed out after %s", titlerTimeout)
	}
	if err != nil {
		if detail := lastLine(stderr.String()); detail != "" {
			return "", fmt.Errorf("%v (%s)", err, detail)
		}
		return "", err
	}
	return stdout.String(), nil
}

// titlerEnv is the environment the titler CLIs run with: lasso's own, minus the
// three Claude Code session markers a lasso launched from inside a Claude Code
// session leaks. Claude Code reads their presence as "this is a child session"
// and changes its behavior accordingly (see claudeCommand, which scrubs the
// same three for the agents it launches).
func titlerEnv() []string {
	drop := map[string]bool{
		"CLAUDE_CODE_CHILD_SESSION": true,
		"CLAUDECODE":                true,
		"CLAUDE_CODE_SESSION_ID":    true,
	}
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && drop[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// titlerInstallDirs are the home-relative directories these CLIs install into,
// searched when PATH doesn't have them. lasso commonly runs as a systemd user
// unit with a hand-written PATH (see the unit's Environment=PATH), which is how
// an opencode in ~/.opencode/bin ends up invisible to a lasso that can see
// claude in ~/.local/bin. Mirrors tailscaleBin's fallback list.
var titlerInstallDirs = []string{
	".local/bin",
	".local/share/mise/shims",
	".bun/bin",
	".opencode/bin",
	".cargo/bin",
	"go/bin",
}

// titlerBin resolves a titler's executable, preferring PATH.
func titlerBin(name string) (string, bool) {
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	for _, dir := range titlerInstallDirs {
		p := filepath.Join(home, dir, name)
		if _, err := exec.LookPath(p); err == nil {
			return p, true
		}
	}
	for _, p := range []string{"/usr/local/bin/" + name, "/opt/homebrew/bin/" + name} {
		if _, err := exec.LookPath(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// cleaning up what the CLI said
// ---------------------------------------------------------------------------

// autoTitleMaxLen caps a generated title. It's a display name in a narrow
// sidebar column — past this it's clipped anyway, which is the problem the
// feature exists to fix.
const autoTitleMaxLen = 60

// cleanTitle turns a titler's raw stdout into a title, or "" if there's nothing
// usable in it. The instruction asks for a bare title and the CLIs generally
// comply, but "comply" is not a guarantee: a model that prefixes a lead-in,
// wraps the answer in quotes or dresses it in markdown has still answered, so
// take the answer rather than throwing the turn away.
func cleanTitle(raw string) string {
	line := titleLine(raw)
	line = trimTitleDecoration(line)
	// "Title: Fix the login flow" — drop the label, keep the title. Only an
	// exact "title" prefix, so a real colon ("Login: fix the redirect") stays.
	if pre, rest, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(pre), "title") {
		line = trimTitleDecoration(rest)
	}
	line = strings.Join(strings.Fields(line), " ")
	line = strings.TrimRight(line, ".!,;")
	return truncateTitle(line)
}

// titleLine picks the answer out of the CLI's output: the first non-empty line
// that isn't a lead-in ("Here's a short title:").
func titleLine(raw string) string {
	for _, ln := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasSuffix(t, ":") {
			continue
		}
		return t
	}
	return ""
}

// trimTitleDecoration strips the quoting and markdown a model may wrap its
// answer in.
func trimTitleDecoration(s string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "`*_#\"'“”‘’"))
}

// truncateTitle caps a title at autoTitleMaxLen, cutting back to the last space
// so it ends on a whole word (mirroring titleSlug's word-boundary cut).
func truncateTitle(s string) string {
	if len(s) <= autoTitleMaxLen {
		return s
	}
	s = strings.ToValidUTF8(s[:autoTitleMaxLen], "")
	if i := strings.LastIndex(s, " "); i > 0 {
		s = s[:i]
	}
	return strings.TrimRight(strings.TrimSpace(s), ".,;:!-")
}

// lastLine is the final non-empty line of s, trimmed — the useful part of a
// failed CLI's stderr (the error, past whatever banner preceded it).
func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}
