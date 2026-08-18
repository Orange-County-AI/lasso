package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The resolved Rosé Pine xterm.js ITheme — a regression guard on the derivation.
// selectionBackground is the theme accent (#c4a7e7) at termSelectionAlpha (0x66),
// a translucent highlight that stays visible on every theme.
const prevRosePine = `{` +
	`"background":"#191724","foreground":"#e0def4",` +
	`"cursor":"#e0def4","cursorAccent":"#191724","selectionBackground":"#c4a7e766",` +
	`"black":"#26233a","red":"#eb6f92","green":"#31748f","yellow":"#f6c177",` +
	`"blue":"#9ccfd8","magenta":"#c4a7e7","cyan":"#ebbcba","white":"#e0def4",` +
	`"brightBlack":"#6e6a86","brightRed":"#eb6f92","brightGreen":"#31748f","brightYellow":"#f6c177",` +
	`"brightBlue":"#9ccfd8","brightMagenta":"#c4a7e7","brightCyan":"#ebbcba","brightWhite":"#e0def4"}`

func TestRosePineNoRegression(t *testing.T) {
	rt := loadHerdrTheme("rose-pine")
	if got := rt.xtermJSON(); got != prevRosePine {
		t.Errorf("rose-pine xterm theme regressed:\n got:  %s\n want: %s", got, prevRosePine)
	}
	// Sidebar vars. --accent maps to the theme's own Accent (mauve), not Teal,
	// so the New Agent button reads purple; --muted is the Subtext0 tier (not the
	// dimmer Overlay0) so form labels keep readable contrast. --good stays Teal.
	css := rt.cssVars()
	for _, want := range []string{
		"--bg: #191724;", "--panel: #1f1d2e;", "--border: #26233a;",
		"--fg: #e0def4;", "--muted: #c8c5dc;", "--accent: #c4a7e7;",
		"--accent-dim: #c4a7e726;", "--dir: #c4a7e7;", "--good: #9ccfd8;",
		"--warn: #f6c177;", "--bad: #eb6f92;",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("rose-pine css missing %q in:\n%s", want, css)
		}
	}
}

// cssVarsRoot is injected into the served index.html for a flash-free first
// paint, so it must wrap the palette in a :root{} rule keyed by the same --h-*
// names index.css and applyCSSVars use (not the bare --bg names cssVars emits).
func TestCSSVarsRoot(t *testing.T) {
	root := loadHerdrTheme("tokyo-night").cssVarsRoot()
	if !strings.HasPrefix(root, ":root{") {
		t.Errorf("cssVarsRoot must start with %q, got:\n%s", ":root{", root)
	}
	for _, want := range []string{"--h-bg: #1a1b26;", "--h-accent-dim:"} {
		if !strings.Contains(root, want) {
			t.Errorf("cssVarsRoot missing %q in:\n%s", want, root)
		}
	}
	// Every property must be --h-* prefixed; a bare "--bg:" means the prefixing
	// broke (index.css's :root fallback wouldn't be overridden).
	if strings.Contains(root, "--bg:") {
		t.Errorf("cssVarsRoot has an unprefixed var (expected only --h-*):\n%s", root)
	}
}

func TestEveryThemeResolves(t *testing.T) {
	for name := range themes {
		rt := loadHerdrTheme(name)
		if rt.Resolved != name {
			t.Errorf("%s resolved to %s", name, rt.Resolved)
		}
		j := rt.xtermJSON()
		if strings.Count(j, "#") < 19 { // bg,fg,cursor,cursorAccent,sel + 16 ansi... but some share
			t.Errorf("%s xterm json looks short: %s", name, j)
		}
		// The selection highlight must be a *translucent* accent wash (8-digit
		// #rrggbbaa), so it composites over cell content and stays visible on
		// every theme rather than an opaque near-bg color that disappears.
		wantSel := `"selectionBackground":"` + rgba(rt.ui.Accent, termSelectionAlpha) + `"`
		if !strings.Contains(j, wantSel) {
			t.Errorf("%s selection not translucent accent (want %s) in:\n%s", name, wantSel, j)
		}
		if !strings.Contains(rt.cssVars(), "--accent-dim:") {
			t.Errorf("%s missing accent-dim", name)
		}
	}
}

func TestAliasesAndUnknown(t *testing.T) {
	cases := map[string]string{
		"Rosé Pine":        "rose-pine", // note: not a real alias; spaces/accents -> falls back
		"rosepine":         "rose-pine",
		"Tokyo Night":      "tokyo-night",
		"tokyo_night":      "tokyo-night",
		"catppuccin-mocha": "catppuccin",
		"gruvbox-dark":     "gruvbox",
		"onedark":          "one-dark",
		"totally-bogus":    "catppuccin", // unknown -> herdr default
		// light variants + herdr's alternate spellings for them
		"tokyo-night-day": "tokyo-night-day",
		"Tokyo Night Day": "tokyo-night-day",
		"tokyonight-day":  "tokyo-night-day",
		"latte":           "catppuccin-latte",
		"dawn":            "rose-pine-dawn",
		"rosepine-dawn":   "rose-pine-dawn",
		"lotus":           "kanagawa-lotus",
		"gruvbox-light":   "gruvbox-light",
		"one-light":       "one-light",
		"onelight":        "one-light",
		"solarized-light": "solarized-light",
	}
	for in, want := range cases {
		got := normalizeThemeName(in)
		if _, ok := themes[got]; !ok {
			got = "catppuccin"
		}
		if got != want && !(in == "Rosé Pine") { // accented form legitimately won't match
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLightThemesAreLight guards the light variants: their background (panel_bg)
// must be clearly brighter than their text, and the xterm bg/fg must agree — so a
// swapped or mis-transcribed value (e.g. a dark bg) is caught.
func TestLightThemesAreLight(t *testing.T) {
	lum := func(hex string) float64 {
		if len(hex) != 7 || hex[0] != '#' {
			t.Fatalf("bad hex %q", hex)
		}
		var r, g, b int
		_, err := fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b)
		if err != nil {
			t.Fatalf("parse %q: %v", hex, err)
		}
		return (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)) / 255
	}
	for _, name := range []string{
		"catppuccin-latte", "tokyo-night-day", "gruvbox-light", "one-light",
		"solarized-light", "kanagawa-lotus", "rose-pine-dawn",
	} {
		rt := loadHerdrTheme(name)
		bg, fg := lum(rt.ui.PanelBg), lum(rt.ui.Text)
		if bg < 0.6 {
			t.Errorf("%s: panel_bg %s not light (luma %.2f)", name, rt.ui.PanelBg, bg)
		}
		if bg <= fg {
			t.Errorf("%s: panel_bg %s should be brighter than text %s (%.2f <= %.2f)", name, rt.ui.PanelBg, rt.ui.Text, bg, fg)
		}
	}
}

// herdrConfigIn must answer what herdr itself reads, from the environment of the
// machine that reads it — and must never fall back to the socket's directory,
// the guess that silently misdirected every theme write to a host whose herdr
// keeps its socket outside its config dir (workspace boxes: /dev/shm/herdr/).
func TestHerdrConfigIn(t *testing.T) {
	cases := []struct {
		name                      string
		configPath, xdgHome, home string
		want                      string
	}{
		{"explicit path wins", "/etc/herdr.toml", "/x/config", "/home/dev", "/etc/herdr.toml"},
		{"xdg beats home", "", "/x/config", "/home/dev", "/x/config/herdr/config.toml"},
		{"home default", "", "", "/home/dev", "/home/dev/.config/herdr/config.toml"},
		{"mac home", "", "", "/Users/stephanfitzpatrick", "/Users/stephanfitzpatrick/.config/herdr/config.toml"},
	}
	for _, c := range cases {
		if got := herdrConfigIn(c.configPath, c.xdgHome, c.home); got != c.want {
			t.Errorf("%s: herdrConfigIn(%q, %q, %q) = %q, want %q", c.name, c.configPath, c.xdgHome, c.home, got, c.want)
		}
	}
}

func TestConfigParseAndCustomOverride(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	os.WriteFile(cfg, []byte(`onboarding = false
[theme]
name = "nord"   # inline comment
[theme.custom]
accent = "#ff0000"
red = "rgb(0, 255, 0)"
green = "default"
[ui]
accent = "cyan"
`), 0o644)
	t.Setenv("HERDR_CONFIG_PATH", cfg)
	rt := loadHerdrTheme("auto")
	if rt.Resolved != "nord" {
		t.Fatalf("resolved %s, want nord", rt.Resolved)
	}
	if rt.ui.Accent != "#ff0000" {
		t.Errorf("accent override not applied: %s", rt.ui.Accent)
	}
	if rt.ui.Red != "#00ff00" {
		t.Errorf("rgb() override not applied: %s", rt.ui.Red)
	}
	// "default" is a reset alias -> leaves the base nord green untouched.
	if rt.ui.Green != themes["nord"].ui.Green {
		t.Errorf("reset override should keep base green, got %s", rt.ui.Green)
	}
	if !rt.Customized {
		t.Error("expected Customized=true")
	}
}

// Every dropdown option must resolve to itself (it's a canonical key), and
// every built-in theme must be offered — the dropdown is the themes map's UI.
func TestThemeOptionsCoverThemes(t *testing.T) {
	seen := map[string]bool{}
	for _, o := range themeOptions {
		if _, ok := themes[o.Name]; !ok {
			t.Errorf("option %q is not a canonical theme key", o.Name)
		}
		if normalizeThemeName(o.Name) != o.Name {
			t.Errorf("option %q is not canonical (normalizes to %q)", o.Name, normalizeThemeName(o.Name))
		}
		if seen[o.Name] {
			t.Errorf("option %q listed twice", o.Name)
		}
		seen[o.Name] = true
	}
	for name := range themes {
		if !seen[name] {
			t.Errorf("theme %q missing from themeOptions", name)
		}
	}
}

func TestSetHerdrThemeName(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")

	// No file: creates one with just the theme section.
	if err := setHerdrThemeName(cfg, "nord"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, _ := os.ReadFile(cfg); string(got) != "[theme]\nname = \"nord\"\n" {
		t.Fatalf("created file:\n%s", got)
	}

	// Existing name key: replaced in place, everything else untouched —
	// including [theme.custom] and later sections.
	os.WriteFile(cfg, []byte(`onboarding = false

[theme]
name = "nord"   # inline comment
[theme.custom]
accent = "#ff0000"

[ui]
accent = "cyan"
`), 0o644)
	if err := setHerdrThemeName(cfg, "rose-pine"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _ := os.ReadFile(cfg)
	want := `onboarding = false

[theme]
name = "rose-pine"
[theme.custom]
accent = "#ff0000"

[ui]
accent = "cyan"
`
	if string(got) != want {
		t.Fatalf("replace result:\n%s\nwant:\n%s", got, want)
	}
	if name, custom, _ := parseThemeConfig(cfg); name != "rose-pine" || custom["accent"] != "#ff0000" {
		t.Fatalf("round-trip parse: name=%q custom=%v", name, custom)
	}

	// [theme] section without a name key: inserted right after the header, not
	// into [theme.custom].
	os.WriteFile(cfg, []byte("[theme]\n[theme.custom]\naccent = \"#ff0000\"\n"), 0o644)
	if err := setHerdrThemeName(cfg, "gruvbox"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, _ = os.ReadFile(cfg)
	if string(got) != "[theme]\nname = \"gruvbox\"\n[theme.custom]\naccent = \"#ff0000\"\n" {
		t.Fatalf("insert result:\n%s", got)
	}

	// File with other sections but no [theme]: section appended.
	os.WriteFile(cfg, []byte("onboarding = false\n"), 0o644)
	if err := setHerdrThemeName(cfg, "dracula"); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, _ = os.ReadFile(cfg)
	if string(got) != "onboarding = false\n\n[theme]\nname = \"dracula\"\n" {
		t.Fatalf("append result:\n%s", got)
	}
}

// writeHerdrThemeNameVia drives the same rewrite through the Backend interface
// (localBackend here; the remote path is the same code over SFTP). Covers the
// create-when-missing and preserve-and-replace cases plus fs.ErrNotExist
// handling through the interface's ReadFile.
func TestWriteHerdrThemeNameVia(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "sub", "config.toml") // sub/ exercises MkdirAll
	b := &localBackend{}

	if err := writeHerdrThemeNameVia(b, cfg, "nord"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, _ := os.ReadFile(cfg); string(got) != "[theme]\nname = \"nord\"\n" {
		t.Fatalf("created file:\n%s", got)
	}

	os.WriteFile(cfg, []byte("onboarding = false\n\n[theme]\nname = \"nord\"\n[theme.custom]\naccent = \"#ff0000\"\n"), 0o644)
	if err := writeHerdrThemeNameVia(b, cfg, "gruvbox"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	name, custom, _ := parseThemeConfig(cfg)
	if name != "gruvbox" || custom["accent"] != "#ff0000" {
		t.Fatalf("round-trip: name=%q custom=%v", name, custom)
	}
}
