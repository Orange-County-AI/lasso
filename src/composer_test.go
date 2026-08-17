package main

import (
	"os"
	"path/filepath"
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
