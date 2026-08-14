package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The opencode sync writes a generated theme file — herdr's resolved palette
// mapped onto opencode's tokens as mode-invariant bare hexes — and pins it by
// name in tui.json, with the state kv.json mode lock as a fallback hint.
// opencode never writes the theme file or tui.json, so the pin survives
// running instances (which do rewrite kv.json from in-memory state — the
// reason the theme itself must not depend on the mode lock).
func TestSyncOpencodeTheme(t *testing.T) {
	home := t.TempDir()
	b := &localBackend{}
	tuiPath := filepath.Join(home, ".config", "opencode", "tui.json")
	themePath := filepath.Join(home, ".config", "opencode", "themes", "herdr.json")
	kvPath := filepath.Join(home, ".local", "state", "opencode", "kv.json")

	readTheme := func() map[string]string {
		var f struct {
			Schema string            `json:"$schema"`
			Theme  map[string]string `json:"theme"`
		}
		data, err := os.ReadFile(themePath)
		if err != nil {
			t.Fatalf("theme file missing: %v", err)
		}
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("theme file isn't json: %v", err)
		}
		if f.Schema == "" {
			t.Errorf("theme file lacks $schema: %s", data)
		}
		return f.Theme
	}

	// Fresh write: theme file with herdr's palette, tui.json pinning it by
	// name, kv.json with the mode hint pinned dark.
	dark := resolveThemeByName("catppuccin")
	if err := syncOpencodeTheme(b, home, dark); err != nil {
		t.Fatalf("create: %v", err)
	}
	toks := readTheme()
	if toks["background"] != dark.ui.PanelBg || toks["backgroundPanel"] != dark.ui.Surface0 || toks["text"] != dark.ui.Text {
		t.Errorf("theme file missing core tokens: %v", toks)
	}
	for tok, v := range toks {
		if !strings.HasPrefix(v, "#") {
			t.Errorf("token %q = %q, want a mode-invariant bare hex", tok, v)
		}
	}
	var got map[string]any
	data, _ := os.ReadFile(tuiPath)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("written file isn't json: %v", err)
	}
	if got["theme"] != "herdr" || got["$schema"] == nil {
		t.Fatalf("created file: %s", data)
	}
	var kv map[string]any
	data, _ = os.ReadFile(kvPath)
	if err := json.Unmarshal(data, &kv); err != nil {
		t.Fatalf("written kv isn't json: %v", err)
	}
	if kv["theme_mode_lock"] != "dark" || kv["theme_mode"] != "dark" {
		t.Fatalf("created kv: %s", data)
	}

	// Flip to light: the theme file is rewritten with the light palette, a
	// legacy "catppuccin" pin migrates to "herdr", existing keys in both
	// config files survive, and the kv hint flips.
	os.WriteFile(tuiPath, []byte(`{"theme": "catppuccin", "keybinds": {"leader": "ctrl+x"}}`), 0o644)
	os.WriteFile(kvPath, []byte(`{"sidebar":"auto","theme_mode_lock":"dark","theme_mode":"dark"}`), 0o644)
	light := resolveThemeByName("catppuccin-latte")
	if err := syncOpencodeTheme(b, home, light); err != nil {
		t.Fatalf("flip: %v", err)
	}
	toks = readTheme()
	if toks["background"] != light.ui.PanelBg || toks["text"] != light.ui.Text {
		t.Errorf("theme file not rewritten on flip: background=%q text=%q", toks["background"], toks["text"])
	}
	data, _ = os.ReadFile(tuiPath)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("after flip: %v", err)
	}
	if got["theme"] != "herdr" {
		t.Errorf("theme = %v, want herdr", got["theme"])
	}
	if _, ok := got["keybinds"]; !ok {
		t.Errorf("existing keys dropped: %s", data)
	}
	data, _ = os.ReadFile(kvPath)
	if err := json.Unmarshal(data, &kv); err != nil {
		t.Fatalf("kv after flip: %v", err)
	}
	if kv["theme_mode_lock"] != "light" || kv["theme_mode"] != "light" {
		t.Errorf("kv mode not flipped: %s", data)
	}
	if kv["sidebar"] != "auto" {
		t.Errorf("existing kv keys dropped: %s", data)
	}

	// No-op when already in step: content untouched in all three files.
	before, _ := os.ReadFile(tuiPath)
	themeBefore, _ := os.ReadFile(themePath)
	kvBefore, _ := os.ReadFile(kvPath)
	if err := syncOpencodeTheme(b, home, light); err != nil {
		t.Fatalf("noop: %v", err)
	}
	if after, _ := os.ReadFile(tuiPath); string(after) != string(before) {
		t.Errorf("no-op sync rewrote tui.json")
	}
	if after, _ := os.ReadFile(themePath); string(after) != string(themeBefore) {
		t.Errorf("no-op sync rewrote herdr.json")
	}
	if after, _ := os.ReadFile(kvPath); string(after) != string(kvBefore) {
		t.Errorf("no-op sync rewrote kv.json")
	}

	// Unparseable files (e.g. jsonc with comments) are left alone, not
	// clobbered.
	os.WriteFile(tuiPath, []byte("{\n  // comment\n}\n"), 0o644)
	os.WriteFile(kvPath, []byte("not json"), 0o644)
	if err := syncOpencodeTheme(b, home, dark); err != nil {
		t.Fatalf("malformed: %v", err)
	}
	if data, _ := os.ReadFile(tuiPath); !strings.Contains(string(data), "// comment") {
		t.Errorf("malformed tui.json was clobbered: %s", data)
	}
	if data, _ := os.ReadFile(kvPath); string(data) != "not json" {
		t.Errorf("malformed kv.json was clobbered: %s", data)
	}
}

// The generated opencode theme mirrors herdr's palette: core tokens come
// straight from the theme's UI tokens, diff backgrounds are blends, and
// every value is a bare hex so opencode's
// light/dark mode can't change the result.
func TestOpencodeThemeBody(t *testing.T) {
	rt := resolveThemeByName("tokyo-night")
	var got struct {
		Schema string            `json:"$schema"`
		Theme  map[string]string `json:"theme"`
	}
	if err := json.Unmarshal(opencodeThemeBody(rt), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Schema == "" {
		t.Errorf("missing $schema")
	}
	for tok, want := range map[string]string{
		"primary":         "#7aa2f7", // Accent
		"secondary":       "#bb9af7", // Mauve
		"accent":          "#bb9af7", // ansi Magenta
		"text":            "#c0caf5", // Text
		"background":      "#1a1b26", // PanelBg
		"backgroundPanel": "#24283b", // Surface0
		"syntaxString":    "#9ece6a", // Green
		"error":           "#f7768e", // Red
	} {
		if got.Theme[tok] != want {
			t.Errorf("%s = %q, want %q", tok, got.Theme[tok], want)
		}
	}
	// Blended tokens must be neither parent color.
	for _, tok := range []string{"diffAddedBg", "diffRemovedBg"} {
		v := got.Theme[tok]
		if v == "" || v == rt.ui.Green || v == rt.ui.Red || v == rt.ui.PanelBg {
			t.Errorf("%s = %q, want a blend", tok, v)
		}
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

// The ghostty theme file mirrors the embedded terminal: chrome from the herdr
// UI tokens, the 16 ANSI colors from the scheme's canonical palette.
func TestGhosttyThemeBody(t *testing.T) {
	rt := resolveThemeByName("tokyo-night")
	body := string(ghosttyThemeBody(rt))
	for _, want := range []string{
		"background = #1a1b26",
		"foreground = #c0caf5",
		"palette = 0=#15161e",
		"palette = 15=#c0caf5",
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	// Selection is the accent pre-composited over the bg (ghostty has no alpha),
	// so it must be neither the raw accent nor the raw background.
	sel := ""
	for _, ln := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(ln, "selection-background = "); ok {
			sel = v
		}
	}
	if sel == "" || sel == rt.ui.Accent || sel == rt.ui.PanelBg {
		t.Errorf("selection-background = %q, want a blend", sel)
	}
}

func TestGhosttySetTheme(t *testing.T) {
	cases := []struct {
		name, in, want string
		changed        bool
	}{
		{
			name:    "replaces an auto light/dark pair",
			in:      "font-size = 18\ntheme = light:Catppuccin Latte,dark:Catppuccin Frappe\n",
			want:    "font-size = 18\ntheme = herdr\n",
			changed: true,
		},
		{
			name:    "appends when the key is absent",
			in:      "font-size = 18\n",
			want:    "font-size = 18\ntheme = herdr\n",
			changed: true,
		},
		{
			name:    "leaves comments alone",
			in:      "# theme = Dracula\ntheme = Dracula\n",
			want:    "# theme = Dracula\ntheme = herdr\n",
			changed: true,
		},
		{name: "no-op when already in step", in: "theme = herdr\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := ghosttySetTheme([]byte(c.in), "herdr")
			if changed != c.changed {
				t.Fatalf("changed = %v, want %v", changed, c.changed)
			}
			if changed && string(got) != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A host without a ghostty config gets the theme file but no config conjured
// out of thin air; one with a config gets its theme key repointed.
func TestSyncGhosttyTheme(t *testing.T) {
	home := t.TempDir()
	b := &localBackend{}
	rt := resolveThemeByName("catppuccin")

	if err := syncGhosttyTheme(b, home, rt); err != nil {
		t.Fatalf("no config: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(home, ".config", "ghostty", "themes", "herdr")); err != nil {
		t.Fatalf("theme file not written: %v", err)
	}
	for _, parts := range ghosttyConfigPaths {
		if _, err := os.Stat(filepath.Join(append([]string{home}, parts...)...)); err == nil {
			t.Errorf("conjured a ghostty config at %v", parts)
		}
	}

	// macOS-style config: the theme key gets repointed, other keys survive.
	cfg := filepath.Join(home, "Library", "Application Support", "com.mitchellh.ghostty", "config.ghostty")
	os.MkdirAll(filepath.Dir(cfg), 0o755)
	os.WriteFile(cfg, []byte("font-size = 18\ntheme = Dracula\n"), 0o644)
	if err := syncGhosttyTheme(b, home, rt); err != nil {
		t.Fatalf("with config: %v", err)
	}
	got, _ := os.ReadFile(cfg)
	if string(got) != "font-size = 18\ntheme = herdr\n" {
		t.Errorf("config = %q", got)
	}

	// A theme flip rewrites the theme file, not the (already-correct) config.
	before, _ := os.Stat(cfg)
	if err := syncGhosttyTheme(b, home, resolveThemeByName("gruvbox")); err != nil {
		t.Fatalf("flip: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(home, ".config", "ghostty", "themes", "herdr"))
	if !strings.Contains(string(body), resolveThemeByName("gruvbox").ui.PanelBg) {
		t.Errorf("theme file not updated on flip: %s", body)
	}
	if after, _ := os.Stat(cfg); !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("config rewritten though its theme key was already in step")
	}
}
