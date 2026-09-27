package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectComposerScreens(t *testing.T) {
	tests := []struct {
		name string
		kind string
		want ComposerState
	}{
		{"omp-empty", "omp", ComposerEmpty},
		{"omp-empty-working", "omp", ComposerEmpty},
		{"omp-draft", "omp", ComposerDraft},
		{"omp-draft-wrapped", "omp", ComposerDraft},
		{"omp-draft-palette", "omp", ComposerDraft},
		{"omp-draft-working", "omp", ComposerDraft},
		{"claude-empty", "claude", ComposerEmpty},
		{"claude-empty-working", "claude", ComposerEmpty},
		{"claude-draft", "claude", ComposerDraft},
		{"claude-draft-working", "claude", ComposerDraft},
		{"claude-empty-named", "claude", ComposerEmpty},
		{"claude-draft-named", "claude", ComposerDraft},
		{"shell", "claude", ComposerUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			screen, err := os.ReadFile(filepath.Join("testdata", "screens", tt.name+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			if got := detectComposer(tt.kind, string(screen)); got != tt.want {
				t.Fatalf("detectComposer(%q, %s) = %v, want %v", tt.kind, tt.name, got, tt.want)
			}
		})
	}
}

func TestComposerGuardEnabled(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  bool
	}{
		{"", true},
		{"0", false},
		{"false", false},
		{"TRUE", true},
		{"unparseable", true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("LASSO_COMPOSER_GUARD", tt.value)
			if got := composerGuardEnabled(); got != tt.want {
				t.Fatalf("composerGuardEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Claude Code draws a suggested next prompt, dim, in an empty composer. Only
// the ANSI screen tells it from a typed draft.
func TestClaudeComposerIgnoresDimSuggestion(t *testing.T) {
	rule := "\x1b[0m\x1b[38;2;162;53;22m" + strings.Repeat("─", 40) + "\r"
	screen := func(prompt string) string {
		return strings.Join([]string{
			"● done",
			"",
			rule,
			"❯ " + prompt + "\r",
			rule,
			"  \x1b[0m\x1b[38;5;6mOpus 5.5\x1b[0m",
			"  ⏵⏵ bypass permissions on",
		}, "\n")
	}
	for _, tt := range []struct {
		name   string
		prompt string
		want   ComposerState
	}{
		{"suggestion", "\x1b[0m\x1b[2mcommit it\x1b[0m", ComposerEmpty},
		{"empty", "", ComposerEmpty},
		{"draft", "\x1b[0mcommit it", ComposerDraft},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectClaudeComposer(undimmed(screen(tt.prompt))); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
