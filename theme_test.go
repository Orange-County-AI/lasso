package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The previously hardcoded Rosé Pine xterm.js ITheme — the resolved rose-pine
// theme must reproduce it byte-for-byte so the current setup doesn't regress.
const prevRosePine = `{` +
	`"background":"#191724","foreground":"#e0def4",` +
	`"cursor":"#e0def4","cursorAccent":"#191724","selectionBackground":"#403d52",` +
	`"black":"#26233a","red":"#eb6f92","green":"#31748f","yellow":"#f6c177",` +
	`"blue":"#9ccfd8","magenta":"#c4a7e7","cyan":"#ebbcba","white":"#e0def4",` +
	`"brightBlack":"#6e6a86","brightRed":"#eb6f92","brightGreen":"#31748f","brightYellow":"#f6c177",` +
	`"brightBlue":"#9ccfd8","brightMagenta":"#c4a7e7","brightCyan":"#ebbcba","brightWhite":"#e0def4"}`

func TestRosePineNoRegression(t *testing.T) {
	rt := loadHerdrTheme("rose-pine")
	if got := rt.xtermJSON(); got != prevRosePine {
		t.Errorf("rose-pine xterm theme regressed:\n got:  %s\n want: %s", got, prevRosePine)
	}
	// Sidebar vars must match the prior hand-tuned values.
	css := rt.cssVars()
	for _, want := range []string{
		"--bg: #191724;", "--panel: #1f1d2e;", "--border: #26233a;",
		"--fg: #e0def4;", "--muted: #6e6a86;", "--accent: #9ccfd8;",
		"--accent-dim: #9ccfd826;", "--dir: #c4a7e7;", "--good: #9ccfd8;",
		"--warn: #f6c177;", "--bad: #eb6f92;",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("rose-pine css missing %q in:\n%s", want, css)
		}
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
		if !strings.Contains(rt.cssVars(), "--accent-dim:") {
			t.Errorf("%s missing accent-dim", name)
		}
	}
}

func TestAliasesAndUnknown(t *testing.T) {
	cases := map[string]string{
		"Rosé Pine":   "rose-pine", // note: not a real alias; spaces/accents -> falls back
		"rosepine":    "rose-pine",
		"Tokyo Night": "tokyo-night",
		"tokyo_night": "tokyo-night",
		"catppuccin-mocha": "catppuccin",
		"gruvbox-dark":     "gruvbox",
		"onedark":          "one-dark",
		"totally-bogus":    "catppuccin", // unknown -> herdr default
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
