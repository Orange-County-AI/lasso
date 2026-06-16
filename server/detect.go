package main

import (
	"strings"
	"time"
	"unicode"
)

// Agent status detection. We determine whether an agent is idle, working, or
// blocked (waiting on the user) by SCREEN-SCRAPING the tmux pane. These are
// per-agent screen-scraping heuristics for claude + codex (the only agents
// supported). The status poller (statusPoller) feeds tmuxCapture() output here
// on a tick.

type AgentStatus string

const (
	StatusIdle    AgentStatus = "idle"
	StatusWorking AgentStatus = "working"
	StatusBlocked AgentStatus = "blocked"
	StatusUnknown AgentStatus = "unknown"
)

// claudeWorkingHold is how long a Claude agent's status is held at "working"
// after the screen flips to an idle prompt, to deglitch the rapid Working→Idle→
// Working flicker between tool calls.
const claudeWorkingHold = 1200 * time.Millisecond

// detectAgentStatus is the entry point the poller calls. It returns the agent's
// current status, applying Claude's working-hold deglitch. lastWorking is
// read+updated in place: it's stamped whenever the raw screen reads "working" so
// a subsequent brief idle within claudeWorkingHold is suppressed.
func detectAgentStatus(agent, screen string, prev AgentStatus, lastWorking *time.Time, now time.Time) AgentStatus {
	switch strings.ToLower(strings.TrimPrefix(agent, ".")) {
	case "claude", "claude-code":
		raw := detectClaudeRaw(screen)
		if raw == StatusWorking && lastWorking != nil {
			*lastWorking = now
		}
		var lw time.Time
		if lastWorking != nil {
			lw = *lastWorking
		}
		return stabilizeClaude(prev, raw, now, lw)
	case "codex":
		return detectCodex(screen)
	default:
		return StatusUnknown
	}
}

// stabilizeClaude holds "working" across a brief idle blip (between tool calls).
func stabilizeClaude(prev, raw AgentStatus, now, lastWorking time.Time) AgentStatus {
	if raw == StatusIdle && prev == StatusWorking && !lastWorking.IsZero() &&
		now.Sub(lastWorking) < claudeWorkingHold {
		return StatusWorking
	}
	return raw
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// hasConfirmationPrompt reports a "do you want…/would you like…" prompt that's
// followed by a yes option or a "❯" selection caret.
func hasConfirmationPrompt(lower string) bool {
	pos := strings.Index(lower, "do you want")
	if pos < 0 {
		pos = strings.Index(lower, "would you like")
	}
	if pos < 0 {
		return false
	}
	after := lower[pos:]
	return strings.Contains(after, "yes") || strings.Contains(after, "❯")
}

// hasSelectionPrompt reports a numbered selection menu line ("❯ 1. …").
func hasSelectionPrompt(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "❯") &&
			strings.ContainsFunc(trimmed, func(r rune) bool { return r >= '0' && r <= '9' }) &&
			strings.Contains(trimmed, ".") {
			return true
		}
	}
	return false
}

// hasInterruptPattern reports the "esc to interrupt" working footer.
func hasInterruptPattern(lower string) bool {
	return strings.Contains(lower, "esc to interrupt") ||
		strings.Contains(lower, "ctrl+c to interrupt") ||
		strings.Contains(lower, "press esc to interrupt")
}

// ---------------------------------------------------------------------------
// Claude Code
// ---------------------------------------------------------------------------

func detectClaudeRaw(content string) AgentStatus {
	lower := strings.ToLower(content)
	if strings.Contains(content, "⌕ Search…") {
		return StatusIdle
	}
	if strings.Contains(lower, "ctrl+r to toggle") {
		return StatusIdle
	}
	// A live, empty composer (a bare "❯" caret line) means the agent is at rest
	// waiting for input — idle, or working if there's working chrome above, but
	// never blocked. A genuine permission/selection prompt replaces the empty
	// composer with a choice menu ("❯ 1. Yes …"), so it has no bare caret. This
	// guard mirrors herdr (which suppresses its loose whole-screen blocker rules
	// when `^\s*❯\s*$` is present): without it, ordinary transcript scrollback —
	// e.g. an at-rest orchestrator whose last turn asked "Do you want me to…" —
	// satisfies the whole-screen "do you want" + trailing "❯" heuristic and an
	// idle agent is mis-reported as blocked.
	if !hasEmptyClaudePromptBox(content) && hasClaudeBlockedPrompt(content, lower) {
		return StatusBlocked
	}
	if hasClaudeWorkingChrome(content) {
		return StatusWorking
	}
	return StatusIdle
}

// hasEmptyClaudePromptBox reports a live, ready-for-input composer: a line that
// is just Claude's "❯" caret (optionally surrounded by whitespace). Present when
// the agent is idle or working; absent when a permission/selection dialog has
// taken over the input area.
func hasEmptyClaudePromptBox(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "❯" {
			return true
		}
	}
	return false
}

func hasClaudeBlockedPrompt(content, lower string) bool {
	return hasConfirmationPrompt(lower) ||
		strings.Contains(lower, "do you want to proceed?") ||
		strings.Contains(lower, "would you like to proceed?") ||
		strings.Contains(lower, "waiting for permission") ||
		strings.Contains(lower, "do you want to allow this connection?") ||
		strings.Contains(lower, "tab to amend") ||
		strings.Contains(lower, "ctrl+e to explain") ||
		strings.Contains(lower, "chat about this") ||
		strings.Contains(lower, "review your answers") ||
		strings.Contains(lower, "skip interview and plan immediately") ||
		(hasSelectionPrompt(content) && hasClaudeYesNoChoice(content))
}

func hasClaudeYesNoChoice(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		t := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "❯")))
		t = strings.TrimSpace(t)
		if t == "yes" || t == "no" ||
			strings.HasPrefix(t, "1. yes") || strings.HasPrefix(t, "2. no") ||
			strings.HasPrefix(t, "yes, and ") || strings.HasPrefix(t, "no, and tell claude") {
			return true
		}
	}
	return false
}

func hasClaudeWorkingChrome(content string) bool {
	above := strings.ToLower(claudeContentAbovePromptBox(content))
	return strings.Contains(above, "esc to interrupt") ||
		strings.Contains(above, "ctrl+c to interrupt") ||
		hasSpinnerActivity(claudeContentAbovePromptBox(content))
}

// claudeSpinnerChars are the spinner glyphs Claude cycles while working; the
// verb after them changes, so we key off glyph + trailing ellipsis.
const claudeSpinnerChars = "·✱✲✳✴✵✶✷✸✹✺✻✼✽✾✿❀❁❂❃❇❈❉❊❋✢✣✤✥✦✧✨⊛⊕⊙◉◎◍⁂⁕※⍟☼★☆"

func hasSpinnerActivity(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		runes := []rune(trimmed)
		if len(runes) == 0 {
			continue
		}
		if !strings.ContainsRune(claudeSpinnerChars, runes[0]) {
			continue
		}
		rest := string(runes[1:])
		if strings.HasPrefix(rest, " ") && strings.ContainsRune(rest, '…') &&
			strings.ContainsFunc(rest, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			return true
		}
	}
	return false
}

// claudeContentAbovePromptBox returns the text above Claude's prompt box (the two
// "────" rules with "❯" between them); the whole content if no box is found.
func claudeContentAbovePromptBox(content string) string {
	lines := strings.Split(content, "\n")
	i := claudePromptBoxTopBorderIndex(lines)
	if i < 0 {
		return content
	}
	return strings.Join(lines[:i], "\n")
}

// claudePromptBoxTopBorderIndex finds the 2nd horizontal rule scanning upward
// from the bottom (the prompt box's top border), or -1.
func claudePromptBoxTopBorderIndex(lines []string) int {
	count := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if isHorizontalRule(lines[i]) {
			count++
			if count == 2 {
				return i
			}
		}
	}
	return -1
}

func isHorizontalRule(line string) bool {
	t := strings.TrimSpace(line)
	return t != "" && strings.Trim(t, "─") == ""
}

// ---------------------------------------------------------------------------
// Codex
// ---------------------------------------------------------------------------

func detectCodex(content string) AgentStatus {
	lower := strings.ToLower(content)
	if hasCodexStrongBlocked(lower) {
		return StatusBlocked
	}
	if hasCodexWorkingAtCurrentPrompt(content) {
		return StatusWorking
	}
	if hasCodexCurrentPrompt(content) {
		return StatusIdle
	}
	if hasCodexWeakBlocked(lower) {
		return StatusBlocked
	}
	if hasInterruptPattern(lower) || hasCodexWorkingHeader(content) {
		return StatusWorking
	}
	return StatusIdle
}

func hasCodexStrongBlocked(lower string) bool {
	return strings.Contains(lower, "press enter to confirm or esc to cancel") ||
		strings.Contains(lower, "enter to submit answer") ||
		strings.Contains(lower, "enter to submit all") ||
		strings.Contains(lower, "allow command?")
}

func hasCodexWeakBlocked(lower string) bool {
	return strings.Contains(lower, "[y/n]") ||
		strings.Contains(lower, "yes (y)") ||
		hasConfirmationPrompt(lower)
}

func hasCodexWorkingAtCurrentPrompt(content string) bool {
	m, ok := codexLastBlockMarkerBeforeCurrentPrompt(content)
	return ok && codexWorkingStatusLine(m)
}

func hasCodexWorkingHeader(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		if codexWorkingStatusLine(line) {
			return true
		}
	}
	return false
}

func codexLastBlockMarkerBeforeCurrentPrompt(content string) (string, bool) {
	lines, promptIndex, ok := codexCurrentPromptRegion(content)
	if !ok {
		return "", false
	}
	for i := promptIndex - 1; i >= 0; i-- {
		if codexBlockMarkerLine(lines[i]) {
			return lines[i], true
		}
	}
	return "", false
}

func codexWorkingStatusLine(line string) bool {
	if codexQueuedInputHeaderLine(line) {
		return true
	}
	trimmed := strings.TrimLeft(line, " \t")
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(trimmed, "•") &&
		(strings.Contains(trimmed, "Working (") ||
			strings.Contains(trimmed, "Waiting for background terminal (") ||
			strings.Contains(lower, "reviewing approval request (") ||
			(strings.Contains(lower, "reviewing ") && strings.Contains(lower, " approval requests (")) ||
			strings.Contains(trimmed, "Booting MCP server:"))
}

func hasCodexCurrentPrompt(content string) bool {
	_, _, ok := codexCurrentPromptRegion(content)
	return ok
}

// codexCurrentPromptRegion finds the last "›" prompt line with no block-marker
// line after it (the live, editable prompt). Returns the lines, its index, ok.
func codexCurrentPromptRegion(content string) ([]string, int, bool) {
	lines := strings.Split(content, "\n")
	promptIndex := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if codexPromptLine(lines[i]) {
			promptIndex = i
			break
		}
	}
	if promptIndex < 0 {
		return nil, 0, false
	}
	for _, line := range lines[promptIndex+1:] {
		if codexBlockMarkerLine(line) {
			return nil, 0, false
		}
	}
	return lines, promptIndex, true
}

func codexPromptLine(line string) bool {
	return line == "›" || strings.HasPrefix(line, "› ")
}

func codexBlockMarkerLine(line string) bool {
	return strings.HasPrefix(line, "•") || strings.HasPrefix(line, "■") ||
		strings.HasPrefix(line, "✗") || strings.HasPrefix(line, "✓")
}

func codexQueuedInputHeaderLine(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "•") {
		return false
	}
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "• queued follow-up inputs") ||
		strings.HasPrefix(lower, "• messages to be submitted after next tool call")
}
