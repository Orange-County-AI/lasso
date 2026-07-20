package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The opencode theme pins catppuccin for dark herdr themes and
// catppuccin-frappe for light ones (opencode can't detect the terminal
// background through the lasso/ttyd chain, so we choose for it).
func TestOpencodeThemeFor(t *testing.T) {
	if got := opencodeThemeFor(false); got != "catppuccin" {
		t.Errorf("dark = %q, want catppuccin", got)
	}
	if got := opencodeThemeFor(true); got != "catppuccin-frappe" {
		t.Errorf("light = %q, want catppuccin-frappe", got)
	}
}

func TestSyncOpencodeTheme(t *testing.T) {
	home := t.TempDir()
	b := &localBackend{}
	path := filepath.Join(home, ".config", "opencode", "tui.json")

	// Fresh write: creates the file with schema + theme.
	if err := syncOpencodeTheme(b, home, false); err != nil {
		t.Fatalf("create: %v", err)
	}
	var got map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("written file isn't json: %v", err)
	}
	if got["theme"] != "catppuccin" || got["$schema"] == nil {
		t.Fatalf("created file: %s", data)
	}

	// Existing keys survive a flip to light.
	os.WriteFile(path, []byte(`{"theme": "catppuccin", "keybinds": {"leader": "ctrl+x"}}`), 0o644)
	if err := syncOpencodeTheme(b, home, true); err != nil {
		t.Fatalf("flip: %v", err)
	}
	data, _ = os.ReadFile(path)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("after flip: %v", err)
	}
	if got["theme"] != "catppuccin-frappe" {
		t.Errorf("theme = %v, want catppuccin-frappe", got["theme"])
	}
	if _, ok := got["keybinds"]; !ok {
		t.Errorf("existing keys dropped: %s", data)
	}

	// No-op when already in step: mtime/content untouched.
	before, _ := os.ReadFile(path)
	if err := syncOpencodeTheme(b, home, true); err != nil {
		t.Fatalf("noop: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("no-op sync rewrote the file")
	}

	// Unparseable (e.g. jsonc with comments) is left alone, not clobbered.
	os.WriteFile(path, []byte("{\n  // comment\n}\n"), 0o644)
	if err := syncOpencodeTheme(b, home, false); err != nil {
		t.Fatalf("malformed: %v", err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "// comment") {
		t.Errorf("malformed file was clobbered: %s", data)
	}
}

// The Claude theme file maps herdr's UI tokens onto Claude's, with the base
// (dark/light) following the theme's background luminance.
func TestSyncClaudeTheme(t *testing.T) {
	home := t.TempDir()
	b := &localBackend{}
	path := filepath.Join(home, ".claude", "themes", "herdr.json")

	dark := resolveThemeByName("catppuccin")
	if err := syncClaudeTheme(b, home, dark, false); err != nil {
		t.Fatalf("dark: %v", err)
	}
	var got claudeThemeFile
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Base != "dark" || got.Name != "herdr (catppuccin)" {
		t.Errorf("dark file: base=%q name=%q", got.Base, got.Name)
	}
	if got.Overrides["background"] != dark.ui.PanelBg || got.Overrides["text"] != dark.ui.Text {
		t.Errorf("overrides missing core tokens: %v", got.Overrides)
	}
	for _, tok := range []string{"diffAdded", "diffRemoved", "diffAddedDimmed", "diffRemovedDimmed", "planMode", "warning"} {
		if got.Overrides[tok] == "" {
			t.Errorf("missing override %q", tok)
		}
	}

	light := resolveThemeByName("catppuccin-latte")
	if err := syncClaudeTheme(b, home, light, true); err != nil {
		t.Fatalf("light: %v", err)
	}
	data, _ = os.ReadFile(path)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse light: %v", err)
	}
	if got.Base != "light" || got.Overrides["background"] != light.ui.PanelBg {
		t.Errorf("light file: base=%q bg=%q", got.Base, got.Overrides["background"])
	}
}

func TestSyncLassoResolved(t *testing.T) {
	home := t.TempDir()
	b := &localBackend{}
	path := filepath.Join(home, ".lasso", "settings.json")

	if err := syncLassoResolved(b, home, true); err != nil {
		t.Fatalf("create: %v", err)
	}
	var root map[string]map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if root["theme"]["resolved"] != "light" || root["theme"]["mode"] != "herdr" {
		t.Fatalf("created: %s", data)
	}

	// Existing unrelated keys survive; unchanged resolved is a no-op.
	os.WriteFile(path, []byte(`{"other": 1, "theme": {"resolved": "light"}}`), 0o644)
	if err := syncLassoResolved(b, home, true); err != nil {
		t.Fatalf("noop: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != `{"other": 1, "theme": {"resolved": "light"}}` {
		t.Errorf("no-op sync rewrote the file: %s", data)
	}
	if err := syncLassoResolved(b, home, false); err != nil {
		t.Fatalf("flip: %v", err)
	}
	var after map[string]any
	data, _ = os.ReadFile(path)
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatalf("parse flip: %v", err)
	}
	if _, ok := after["other"]; !ok {
		t.Errorf("unrelated key dropped: %s", data)
	}
}

// resolveThemeByName normalizes aliases and falls back to catppuccin.
func TestResolveThemeByName(t *testing.T) {
	if got := resolveThemeByName("mocha").Resolved; got != "catppuccin" {
		t.Errorf("alias mocha -> %q, want catppuccin", got)
	}
	if got := resolveThemeByName("nonsense").Resolved; got != "catppuccin" {
		t.Errorf("unknown -> %q, want catppuccin", got)
	}
	if got := resolveThemeByName("catppuccin-latte").Resolved; got != "catppuccin-latte" {
		t.Errorf("latte -> %q", got)
	}
}

// blendHex mixes toward the background; luminance classifies light vs dark.
func TestColorMath(t *testing.T) {
	if got := blendHex("#a6e3a1", "#181825", 0.6); got != "#516957" {
		t.Errorf("blendHex = %q", got)
	}
	if luminance("#eff1f5") <= 0.5 {
		t.Errorf("latte bg should read as light")
	}
	if luminance("#181825") > 0.5 {
		t.Errorf("mocha bg should read as dark")
	}
}
