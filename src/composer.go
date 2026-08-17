package main

import (
	"os"
	"strconv"
	"strings"
)

// ComposerState is the state of an agent harness's visible input composer.
// Unknown is deliberately fail-open: a screen layout that is not positively
// recognized must not starve a queued message.
type ComposerState int

const (
	ComposerUnknown ComposerState = iota
	ComposerEmpty
	ComposerDraft
)

// composerGuardEnabled defaults on. An explicit false/0 is the escape hatch
// for a harness layout regression; malformed values remain on rather than
// silently weakening delivery protection.
func composerGuardEnabled() bool {
	value := strings.TrimSpace(os.Getenv("LASSO_COMPOSER_GUARD"))
	if value == "" {
		return true
	}
	enabled, err := strconv.ParseBool(value)
	return err != nil || enabled
}

// detectComposer reads a harness composer from a pane.read source=visible
// screen. It returns ComposerDraft only for positive evidence; callers must
// treat unknown layouts and failed reads exactly as they did before this guard.
func detectComposer(agentKind, screen string) ComposerState {
	if strings.TrimSpace(screen) == "" {
		return ComposerUnknown
	}
	switch strings.ToLower(strings.TrimSpace(agentKind)) {
	case "omp", "pi":
		return detectOmpComposer(screen)
	case "claude":
		return detectClaudeComposer(screen)
	default:
		return ComposerUnknown
	}
}

// detectOmpComposer anchors bottom-up on omp's closing rounded border because
// slash-command palettes are drawn below the composer. The footer holds the
// final input row; wrapped rows use │…│ above it.
func detectOmpComposer(screen string) ComposerState {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		interior, ok := ompComposerFooter(lines[i])
		if !ok {
			continue
		}
		if strings.TrimSpace(interior) != "" {
			return ComposerDraft
		}
		for above := i - 1; above >= 0; above-- {
			body, ok := ompComposerBody(lines[above])
			if !ok {
				break
			}
			if strings.TrimSpace(body) != "" {
				return ComposerDraft
			}
		}
		return ComposerEmpty
	}
	return ComposerUnknown
}

// ompComposerFooter rejects ordinary box rules: an omp composer footer's
// interior always begins with its input padding space.
func ompComposerFooter(line string) (string, bool) {
	trimmed := strings.TrimRight(line, " \t\r")
	interior, ok := strings.CutPrefix(trimmed, "╰─")
	if !ok {
		return "", false
	}
	interior, ok = strings.CutSuffix(interior, "─╯")
	if !ok || !strings.HasPrefix(interior, " ") {
		return "", false
	}
	return interior, true
}

func ompComposerBody(line string) (string, bool) {
	trimmed := strings.TrimRight(line, " \t\r")
	interior, ok := strings.CutPrefix(trimmed, "│")
	if !ok {
		return "", false
	}
	return strings.CutSuffix(interior, "│")
}

// detectClaudeComposer requires both horizontal fences. Transcript echoes and
// shell prompts can contain ❯, but cannot be mistaken for the composer without
// its preceding rule.
func detectClaudeComposer(screen string) ComposerState {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 1; i-- {
		text, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "❯")
		if !ok || !claudeComposerRule(lines[i-1]) {
			continue
		}
		if strings.TrimSpace(text) != "" {
			return ComposerDraft
		}
		for below := i + 1; below < len(lines); below++ {
			row := strings.TrimSpace(lines[below])
			if claudeComposerRule(row) {
				break
			}
			if row != "" {
				return ComposerDraft
			}
		}
		return ComposerEmpty
	}
	return ComposerUnknown
}

func claudeComposerRule(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len([]rune(trimmed)) < 8 {
		return false
	}
	for _, glyph := range trimmed {
		if glyph != '─' {
			return false
		}
	}
	return true
}
