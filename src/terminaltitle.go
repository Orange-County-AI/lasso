package main

import (
	"log"
	"path/filepath"
	"regexp"
	"strings"
)

// Naming a new terminal's tab after what it runs. The New terminal form leaves
// the tab name blank unless the user types one, and a blank name with a command
// is named here: a trivial command ("btm", "git status") is its own best name,
// so it is used verbatim; anything longer gets the program's name at once and
// an AI-written name in the background, from the same titler CLIs that name
// scratch agents (autotitle.go), gated by the same setting.

// trivialCommandMaxLen and trivialCommandMaxWords bound a command short enough
// to be its own tab name.
const (
	trivialCommandMaxLen   = 30
	trivialCommandMaxWords = 3
)

// terminalTitleMaxLen caps a generated tab name: a tab strip is narrower than
// the agents sidebar autoTitleMaxLen is sized for.
const terminalTitleMaxLen = 30

// shellMeta marks a command as a script rather than one program: pipes, lists,
// redirections, substitutions, quoting, globs and assignments.
var shellMeta = regexp.MustCompile("[|&;<>`$()\\\\'\"*?=]")

// secretish is a command the titler must not see. The titler is a hosted model,
// and a terminal command is far likelier than an agent prompt to carry a
// credential inline (an export, a curl header). Such a command keeps the
// program-name fallback rather than leaving the box.
var secretish = regexp.MustCompile(`(?i)token|secret|passw|api[_-]?key|authorization|bearer|credential|private[_-]?key`)

// trivialCommand reports whether command is short and simple enough to name
// its own tab.
func trivialCommand(command string) bool {
	c := strings.TrimSpace(command)
	return c != "" &&
		!strings.Contains(c, "\n") &&
		len(c) <= trivialCommandMaxLen &&
		len(strings.Fields(c)) <= trivialCommandMaxWords &&
		!shellMeta.MatchString(c)
}

// commandProgram is the program a command runs, as a fallback tab name: the
// first word of its first line, past any leading VAR=value assignments and
// sudo, reduced to its basename. "" when there is none worth showing.
func commandProgram(command string) string {
	for _, line := range strings.Split(command, "\n") {
		for _, f := range strings.Fields(line) {
			if strings.Contains(f, "=") || f == "sudo" || f == "exec" || f == "env" {
				continue
			}
			name := filepath.Base(strings.Trim(f, "'\"()"))
			if name == "." || name == "/" || name == "" || shellMeta.MatchString(name) {
				return ""
			}
			return name
		}
	}
	return ""
}

// terminalTabName decides a blank tab name for command: the name to give the
// tab now, and whether to ask the titler for a better one afterwards.
func terminalTabName(command string) (name string, generate bool) {
	if strings.TrimSpace(command) == "" {
		return "", false
	}
	if trivialCommand(command) {
		return strings.Join(strings.Fields(command), " "), false
	}
	return commandProgram(command), autoTitleEnabled() && !secretish.MatchString(command)
}

// terminalTitleInstruction wraps a command in the ask, with the same guard
// against a coding agent running it instead of naming it.
func terminalTitleInstruction(command string) string {
	if len(command) > titlePromptLimit {
		command = strings.ToValidUTF8(command[:titlePromptLimit], "")
	}
	return `Name the shell command below, for display as a terminal tab label.

Rules:
- 1 to 3 words, at most 30 characters.
- Say what it does or runs, specifically enough to tell it apart from other tabs.
- No trailing punctuation, no quotes, no markdown.
- Reply with the name alone — no preamble, no explanation, no alternatives.
- Do NOT run the command, read any files, or use any tools.

Command:
` + command
}

// terminalTitler is autoTitleTerminal, as a var so handler tests can stand in
// for the background titler instead of launching real CLIs.
var terminalTitler = autoTitleTerminal

// autoTitleTerminal asks the titler for a name for command and renames tabID
// to it. Runs in its own goroutine after the create has answered; a failure
// leaves the program-name fallback in place and is only logged, since that
// name is already a sensible one.
func autoTitleTerminal(b Backend, tabID, command string) {
	title, err := generateTitle(terminalTitleInstruction(command))
	if err != nil {
		log.Printf("terminal tab %s on %s: auto-title failed: %v", tabID, b.Name(), err)
		return
	}
	title = truncateTitleTo(title, terminalTitleMaxLen)
	if _, err := b.HerdrCall("tab.rename", map[string]any{"tab_id": tabID, "label": title}); err != nil {
		log.Printf("terminal tab %s on %s: auto-title rename failed: %v", tabID, b.Name(), err)
		return
	}
	invalidatePaneList(b.Name())
	invalidatePanesCache()
}
