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

// claudeComposerRule accepts a bare rule and one carrying a label: a named
// session draws its name into the upper fence ("──── my-session ─"). Missing
// that made every named pane read as Unknown, so the chat never saw its own
// paste land and stopped short of pressing Enter. The label must sit between
// two runs of rule, with at least 8 glyphs of rule leading it, so a transcript
// line that merely starts with a dash cannot pass.
func claudeComposerRule(line string) bool {
	trimmed := []rune(strings.TrimSpace(line))
	lead := 0
	for lead < len(trimmed) && trimmed[lead] == '─' {
		lead++
	}
	if lead < 8 {
		return false
	}
	return lead == len(trimmed) || trimmed[len(trimmed)-1] == '─'
}

// codexComposerReach is how far above the screen's last line the composer may
// sit. Codex draws it just above its footer, with at most a slash-command popup
// below; a `›` row higher up is a past turn in the history, which Codex draws
// with the same prompt glyph.
const codexComposerReach = 12

// detectCodexComposer reads Codex's composer from an ANSI screen
// (pane.read format=ansi). Plain text cannot answer it: an empty composer
// shows a placeholder ("Ask Codex to do anything", or one of several others),
// and the only thing telling that apart from a draft is that the placeholder is
// drawn DIM. So the prompt row is a draft exactly when it holds text that is
// not dim.
func detectCodexComposer(screen string) ComposerState {
	lines := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(stripSGR(lines[last])) == "" {
		last--
	}
	for i := last; i >= 0 && i >= last-codexComposerReach; i-- {
		runes, dim := sgrCells(lines[i])
		j := 0
		for j < len(runes) && runes[j] == ' ' {
			j++
		}
		if j >= len(runes) || runes[j] != '›' {
			continue
		}
		for k := j + 1; k < len(runes); k++ {
			if runes[k] != ' ' && !dim[k] {
				return ComposerDraft
			}
		}
		return ComposerEmpty
	}
	return ComposerUnknown
}

// sgrCells splits an ANSI line into its printed runes and whether each was drawn
// dim (SGR 2). Every other escape is dropped.
func sgrCells(line string) (runes []rune, dim []bool) {
	faint := false
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '\x1b' {
			runes = append(runes, rs[i])
			dim = append(dim, faint)
			continue
		}
		if i+1 >= len(rs) || rs[i+1] != '[' {
			i++
			continue
		}
		j := i + 2
		for j < len(rs) && (rs[j] < 0x40 || rs[j] > 0x7e) {
			j++
		}
		if j < len(rs) && rs[j] == 'm' {
			params := strings.Split(string(rs[i+2:j]), ";")
			for p := 0; p < len(params); p++ {
				switch params[p] {
				case "", "0", "22":
					faint = false
				case "2":
					faint = true
				case "38", "48", "58":
					// Extended colour: its arguments are not attributes.
					if p+1 < len(params) && params[p+1] == "5" {
						p += 2
					} else if p+1 < len(params) && params[p+1] == "2" {
						p += 4
					}
				}
			}
		}
		i = j
	}
	return runes, dim
}

// undimmed renders an ANSI screen as plain text with every dim cell blanked.
// Claude Code puts a suggested next prompt in its empty composer ("❯ commit
// it"), drawn dim; left in, it reads as an unsent draft and the chat refuses
// every send to an idle pane.
func undimmed(screen string) string {
	lines := strings.Split(screen, "\n")
	for i, line := range lines {
		runes, dim := sgrCells(line)
		for j := range runes {
			if dim[j] {
				runes[j] = ' '
			}
		}
		lines[i] = string(runes)
	}
	return strings.Join(lines, "\n")
}

func stripSGR(line string) string {
	runes, _ := sgrCells(line)
	return string(runes)
}
