package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
		"Retro 82":         "retro-82",
		"rosepine":         "rose-pine",
		"Tokyo Night":      "tokyo-night",
		"tokyo_night":      "tokyo-night",
		"catppuccin-mocha": "catppuccin",
		"gruvbox-dark":     "gruvbox",
		"onedark":          "one-dark",
		"totally-bogus":    "retro-82",
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
		got := loadHerdrTheme(in).Resolved
		if got != want {
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
	if c := parseThemeConfig(cfg); c.Name != "rose-pine" || c.Custom["accent"] != "#ff0000" {
		t.Fatalf("round-trip parse: name=%q custom=%v", c.Name, c.Custom)
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
	c := parseThemeConfig(cfg)
	if c.Name != "gruvbox" || c.Custom["accent"] != "#ff0000" {
		t.Fatalf("round-trip: name=%q custom=%v", c.Name, c.Custom)
	}
}

// herdrThemeNames is herdr 0.9's closed set of theme names (src/config/theme.rs
// THEME_NAMES). Anything else in [theme].name is a `herdr config check` error
// and silently falls its TUI back to catppuccin.
var herdrThemeNames = map[string]bool{
	"catppuccin": true, "catppuccin-latte": true, "terminal": true,
	"tokyo-night": true, "tokyo-night-day": true, "dracula": true, "nord": true,
	"gruvbox": true, "gruvbox-light": true, "one-dark": true, "one-light": true,
	"solarized": true, "solarized-light": true, "kanagawa": true,
	"kanagawa-lotus": true, "rose-pine": true, "rose-pine-dawn": true, "vesper": true,
}

// Whatever lasso writes into [theme].name must be a name herdr accepts: a theme
// it doesn't know costs the machine its palette AND a clean config check. A
// lasso-only theme has to declare a base; every other key has to BE a herdr name.
func TestThemeSpecBaseIsAlwaysAHerdrName(t *testing.T) {
	for name, def := range themes {
		spec := themeSpecFor(name)
		if !herdrThemeNames[spec.base] {
			t.Errorf("theme %q writes [theme].name = %q, which herdr rejects", name, spec.base)
		}
		if def.herdrBase == "" {
			if spec.lasso != "" || len(spec.tokens) > 0 {
				t.Errorf("built-in %q should be written as a bare name, got marker %q / %d tokens", name, spec.lasso, len(spec.tokens))
			}
			continue
		}
		if spec.lasso != name {
			t.Errorf("lasso-only theme %q wrote marker %q", name, spec.lasso)
		}
		if len(spec.tokens) == 0 {
			t.Errorf("lasso-only theme %q wrote no override block, so herdr would paint %q", name, def.herdrBase)
		}
	}
}

// herdrEnv is the process environment with herdr's config pointed at dir —
// HERDR_CONFIG_PATH removed rather than emptied, since herdr reads it as a path
// whenever it is set at all.
func herdrEnv(dir string) []string {
	env := []string{"XDG_CONFIG_HOME=" + dir}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); k != "XDG_CONFIG_HOME" && k != "HERDR_CONFIG_PATH" {
			env = append(env, kv)
		}
	}
	return env
}

// The acceptance test for the representation: a theme lasso synthesizes must
// leave `herdr config check` clean on the real binary. Only the synthesized
// ones are worth the process — every other key is a herdr name verbatim, which
// TestThemeSpecBaseIsAlwaysAHerdrName already pins without shelling out.
func TestHerdrConfigCheckAcceptsSynthesizedThemes(t *testing.T) {
	bin, err := exec.LookPath("herdr")
	if err != nil {
		t.Skip("herdr not installed")
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "herdr", "config.toml")
	check := func(label string) {
		t.Helper()
		cmd := exec.Command(bin, "config", "check")
		cmd.Env = herdrEnv(dir)
		out, err := cmd.CombinedOutput()
		if err != nil || strings.Contains(string(out), "unknown theme") {
			body, _ := os.ReadFile(cfg)
			t.Errorf("%s: herdr config check: %v\n%s\nconfig:\n%s", label, err, out, body)
		}
	}
	for name, def := range themes {
		if def.herdrBase == "" {
			continue
		}
		os.Remove(cfg)
		if err := setHerdrThemeName(cfg, name); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		check(name)

		// The generated block becomes a super-table AFTER an existing
		// [theme.custom.dark] sub-table, which is the one shape where "valid
		// TOML" is not obvious — and only herdr's own parser can settle it.
		os.Remove(cfg)
		os.MkdirAll(filepath.Dir(cfg), 0o755)
		os.WriteFile(cfg, []byte("[theme.custom.dark]\nred = \"#ff0000\"\n\n[theme]\nname = \"nord\"\n"), 0o644)
		if err := setHerdrThemeName(cfg, name); err != nil {
			t.Fatalf("%s: write under sub-table: %v", name, err)
		}
		body, _ := os.ReadFile(cfg)
		if !strings.Contains(string(body), "[theme.custom.dark]") || !strings.Contains(string(body), "red = \"#ff0000\"") {
			t.Errorf("%s: user sub-table lost:\n%s", name, body)
		}
		check(name + " under [theme.custom.dark]")

		// And switching away from that shape leaves it parseable and clean.
		if err := setHerdrThemeName(cfg, "nord"); err != nil {
			t.Fatalf("%s: switch away: %v", name, err)
		}
		if body, _ := os.ReadFile(cfg); strings.Contains(string(body), lassoThemeTag) {
			t.Errorf("%s: generated data survived beside a sub-table:\n%s", name, body)
		}
		check("switched away from " + name)
	}
}

// The full life of a lasso-only theme in someone else's config file: selected,
// re-selected, then switched away from. What must survive is everything the
// human wrote; what must not is anything lasso generated.
func TestRetro82RoundTripAndSwitchAway(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	const orig = `onboarding = false

[theme]
name = "nord"

[theme.custom]
accent = "#ff0000"

[keys]
prefix = "ctrl-a"
`
	os.WriteFile(cfg, []byte(orig), 0o644)
	if err := setHerdrThemeName(cfg, "retro-82"); err != nil {
		t.Fatalf("select: %v", err)
	}
	after, _ := os.ReadFile(cfg)

	// herdr sees a name it accepts; lasso still sees Retro 82.
	c := parseThemeConfig(cfg)
	if c.Name != "vesper" {
		t.Errorf("[theme].name = %q, want the herdr base vesper", c.Name)
	}
	if c.lassoTheme() != "retro-82" {
		t.Errorf("identity marker resolved to %q, want retro-82", c.lassoTheme())
	}
	t.Setenv("HERDR_CONFIG_PATH", cfg)
	rt := loadHerdrTheme("auto")
	if rt.Resolved != "retro-82" {
		t.Fatalf("resolved %q, want retro-82", rt.Resolved)
	}

	// The user's accent is theirs: not overwritten, not duplicated (which would
	// be invalid TOML), and still the color lasso paints.
	if got := strings.Count(string(after), "accent ="); got != 1 {
		t.Errorf("accent written %d times:\n%s", got, after)
	}
	if c.Custom["accent"] != "#ff0000" || rt.ui.Accent != "#ff0000" {
		t.Errorf("user accent lost: config %q, resolved %q", c.Custom["accent"], rt.ui.Accent)
	}
	// Every other token reproduces the palette, so herdr paints Retro 82 rather
	// than the base showing through.
	for _, tok := range themes["retro-82"].customTokens() {
		if tok.key == "accent" {
			continue
		}
		if c.Generated[tok.key] != tok.hex {
			t.Errorf("token %s = %q, want %q", tok.key, c.Generated[tok.key], tok.hex)
		}
	}
	if !strings.Contains(string(after), "[keys]") || !strings.Contains(string(after), "onboarding = false") {
		t.Errorf("unrelated config lost:\n%s", after)
	}

	// Re-selecting the same theme must be a no-op, or every write would stack
	// another copy of the block.
	if err := setHerdrThemeName(cfg, "retro-82"); err != nil {
		t.Fatalf("re-select: %v", err)
	}
	if again, _ := os.ReadFile(cfg); string(again) != string(after) {
		t.Errorf("re-selecting changed the file:\n%s\nwant:\n%s", again, after)
	}

	// Switching away takes the generated block with it — a leftover would paint
	// the next theme in Retro 82's colors.
	if err := setHerdrThemeName(cfg, "nord"); err != nil {
		t.Fatalf("switch away: %v", err)
	}
	back, _ := os.ReadFile(cfg)
	if strings.Contains(string(back), lassoThemeTag) {
		t.Errorf("generated data survived the switch:\n%s", back)
	}
	c = parseThemeConfig(cfg)
	if c.Name != "nord" || len(c.Generated) != 0 || c.Custom["accent"] != "#ff0000" {
		t.Errorf("after switch: name=%q generated=%v custom=%v", c.Name, c.Generated, c.Custom)
	}
	if rt := loadHerdrTheme("auto"); rt.Resolved != "nord" || rt.ui.PanelBg != themes["nord"].ui.PanelBg {
		t.Errorf("after switch resolved %q panel_bg %q", rt.Resolved, rt.ui.PanelBg)
	}
	if !strings.Contains(string(back), "[keys]") {
		t.Errorf("unrelated config lost on switch away:\n%s", back)
	}
}

// herdr's own Settings UI writes [theme].name. When it moves off our base, the
// generated block is leftovers: lasso must paint what herdr paints (the new
// base), not keep claiming its own identity, and must clear the leftovers.
func TestStaleThemeMarkerIsIgnoredAndCleaned(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := setHerdrThemeName(cfg, "retro-82"); err != nil {
		t.Fatalf("select: %v", err)
	}
	body, _ := os.ReadFile(cfg)
	moved := strings.Replace(string(body), `name = "vesper"`, `name = "nord"`, 1)
	os.WriteFile(cfg, []byte(moved), 0o644)

	t.Setenv("HERDR_CONFIG_PATH", cfg)
	rt := loadHerdrTheme("auto")
	if rt.Resolved != "nord" {
		t.Fatalf("resolved %q, want nord — the marker no longer applies", rt.Resolved)
	}
	if rt.ui.PanelBg != themes["nord"].ui.PanelBg || rt.Customized {
		t.Errorf("stale generated tokens leaked: panel_bg %q customized %v", rt.ui.PanelBg, rt.Customized)
	}

	changed, err := migrateHerdrThemeConfig(cfg)
	if err != nil || !changed {
		t.Fatalf("migrate: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(cfg)
	if strings.Contains(string(got), lassoThemeTag) {
		t.Errorf("leftovers survived migration:\n%s", got)
	}
	if c := parseThemeConfig(cfg); c.Name != "nord" || len(c.Generated) != 0 {
		t.Errorf("cleanup changed the human's selection: name=%q generated=%v", c.Name, c.Generated)
	}

	// Same again with a name lasso does NOT recognize. The block is still
	// lasso's litter and herdr still applies it, but the name belongs to whoever
	// typed it — cleaning up must not overwrite it with a theme of our choosing.
	os.WriteFile(cfg, []byte(strings.Replace(moved, `name = "nord"`, `name = "moonfly"`, 1)), 0o644)
	if changed, err := migrateHerdrThemeConfig(cfg); err != nil || !changed {
		t.Fatalf("migrate unknown name: changed=%v err=%v", changed, err)
	}
	got, _ = os.ReadFile(cfg)
	if strings.Contains(string(got), lassoThemeTag) {
		t.Errorf("leftovers survived under an unknown name:\n%s", got)
	}
	if c := parseThemeConfig(cfg); c.Name != "moonfly" {
		t.Errorf("[theme].name = %q, want the human's moonfly untouched:\n%s", c.Name, got)
	}
	if changed, err := migrateHerdrThemeConfig(cfg); err != nil || changed {
		t.Errorf("second pass rewrote a clean file: changed=%v err=%v", changed, err)
	}
}

// The cleanup may not hang off "the theme changed". Select Retro 82 while on
// nord, then hand-edit its base straight back to nord: the resolved theme is
// nord before and after, so a poller comparing palettes sees nothing to do —
// while herdr is left applying Retro 82's colors over nord indefinitely.
func TestRefreshThemeClearsStrandedOverridesWithoutAThemeChange(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	os.WriteFile(cfg, []byte("[theme]\nname = \"nord\"\n"), 0o644)
	t.Setenv("HERDR_CONFIG_PATH", cfg)
	before := loadHerdrTheme("auto")

	if err := setHerdrThemeName(cfg, "retro-82"); err != nil {
		t.Fatalf("select: %v", err)
	}
	body, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, []byte(strings.Replace(string(body), `name = "vesper"`, `name = "nord"`, 1)), 0o644)

	rt, stranded := loadHerdrThemeConfig("auto")
	if rt != before {
		t.Fatalf("resolved theme moved (%v -> %v); this test no longer covers the same-resolved case", before.Resolved, rt.Resolved)
	}
	if !stranded {
		t.Fatal("stranded overrides not reported, so nothing would ever clear them")
	}

	// The hub already holds that same theme, so its change check short-circuits
	// — the cleanup has to run anyway.
	h := &hub{curTheme: before}
	h.refreshTheme()
	if h.themeRev != 0 {
		t.Errorf("themeRev bumped to %d without a theme change", h.themeRev)
	}
	got, _ := os.ReadFile(cfg)
	if strings.Contains(string(got), lassoThemeTag) {
		t.Errorf("stranded overrides survived the poll:\n%s", got)
	}
	if c := parseThemeConfig(cfg); c.Name != "nord" || len(c.Generated) != 0 {
		t.Errorf("after cleanup: name=%q generated=%v", c.Name, c.Generated)
	}
	if _, stranded := loadHerdrThemeConfig("auto"); stranded {
		t.Error("still reported as stranded after the cleanup: the poll would rewrite on every tick")
	}
}

// A machine that picked Retro 82 under an older build has a name herdr rejects
// and no way to fix itself: the migration is what repairs it at boot, without
// the human having to select some other theme and come back.
func TestMigrateLegacyRetro82Name(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	os.WriteFile(cfg, []byte("onboarding = false\n\n[theme]\nname = \"retro-82\"\n"), 0o644)

	changed, err := migrateHerdrThemeConfig(cfg)
	if err != nil || !changed {
		t.Fatalf("migrate: changed=%v err=%v", changed, err)
	}
	c := parseThemeConfig(cfg)
	if c.Name != "vesper" || c.lassoTheme() != "retro-82" {
		t.Fatalf("migrated to name=%q marker=%q", c.Name, c.Marker)
	}
	if got, _ := os.ReadFile(cfg); !strings.Contains(string(got), "onboarding = false") {
		t.Errorf("unrelated config lost:\n%s", got)
	}
	t.Setenv("HERDR_CONFIG_PATH", cfg)
	if rt := loadHerdrTheme("auto"); rt.Resolved != "retro-82" {
		t.Errorf("migrated config resolves to %q, want retro-82", rt.Resolved)
	}

	// Idempotent: a second boot must not rewrite (nor re-notify herdr).
	if changed, err := migrateHerdrThemeConfig(cfg); err != nil || changed {
		t.Errorf("second migrate: changed=%v err=%v", changed, err)
	}
}

// The migration may only touch a config it has something to fix. A file naming
// a plain built-in is herdr's business alone — reformatting it (and dropping the
// human's comments) at every boot would be the worse bug.
func TestMigrateLeavesOrdinaryConfigAlone(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	const orig = "[theme]\nname   = \"nord\"  # my favourite\n"
	os.WriteFile(cfg, []byte(orig), 0o644)
	if changed, err := migrateHerdrThemeConfig(cfg); err != nil || changed {
		t.Fatalf("migrate: changed=%v err=%v", changed, err)
	}
	if got, _ := os.ReadFile(cfg); string(got) != orig {
		t.Errorf("config rewritten:\n%s", got)
	}
	if changed, err := migrateHerdrThemeConfig(filepath.Join(dir, "absent.toml")); err != nil || changed {
		t.Errorf("missing config: changed=%v err=%v", changed, err)
	}
}

// A remote host gets the same representation over the Backend interface — a
// theme name only lasso knows must never reach another machine's config.toml.
func TestWriteHerdrThemeNameViaSynthesizes(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "sub", "config.toml")
	b := &localBackend{}
	if err := writeHerdrThemeNameVia(b, cfg, "retro-82"); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := parseThemeConfig(cfg)
	if c.Name != "vesper" || c.lassoTheme() != "retro-82" {
		t.Fatalf("remote config: name=%q marker=%q", c.Name, c.Marker)
	}
	if c.Generated["panel_bg"] != themes["retro-82"].ui.PanelBg {
		t.Errorf("panel_bg = %q, want %q", c.Generated["panel_bg"], themes["retro-82"].ui.PanelBg)
	}
	if err := writeHerdrThemeNameVia(b, cfg, "gruvbox"); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if got, _ := os.ReadFile(cfg); strings.Contains(string(got), lassoThemeTag) {
		t.Errorf("generated data survived on the remote:\n%s", got)
	}
}

// foreignSelection writes the config a NEWER lasso leaves behind: a real
// generated block for a theme this build knows, with the identity marker moved
// to a name it does not. That is byte-for-byte what an older binary sees when a
// newer one selects a theme it has never heard of.
func foreignSelection(t *testing.T, cfg, known, unknown string) (base string) {
	t.Helper()
	if def, ok := lookupThemeDef(known); !ok || def.herdrBase == "" {
		t.Skipf("%s is not a lasso-only theme in this build", known)
	}
	if err := setHerdrThemeName(cfg, known); err != nil {
		t.Fatalf("select %s: %v", known, err)
	}
	body, _ := os.ReadFile(cfg)
	swapped := strings.Replace(string(body),
		lassoThemeTag+" = "+strconv.Quote(known),
		lassoThemeTag+" = "+strconv.Quote(unknown), 1)
	if swapped == string(body) {
		t.Fatalf("no identity marker for %s in:\n%s", known, body)
	}
	os.WriteFile(cfg, []byte(swapped), 0o644)
	return parseThemeConfig(cfg).Name
}

// A theme this build cannot resolve is still the human's selection, and the
// generated block beside it is that theme's palette. Deleting it is what made a
// theme revert on its own: a released lasso and a newer one share one
// config.toml, and the older one used to strip every block it could not claim,
// leaving [theme].name holding the bare herdr base (Vesper, or Catppuccin Latte
// for a light theme). So: paint the block, report nothing stranded, rewrite
// nothing.
func TestForeignThemeSelectionIsPaintedAndNeverRewritten(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	base := foreignSelection(t, cfg, "matte-black", "moonveil-1999")
	want, ok := lookupThemeDef("matte-black")
	if !ok {
		t.Skip("matte-black is not vendored in this build")
	}
	before, _ := os.ReadFile(cfg)

	t.Setenv("HERDR_CONFIG_PATH", cfg)
	rt, stranded := loadHerdrThemeConfig("auto")
	if stranded {
		t.Error("reported stranded: the poll would delete another build's selection")
	}
	if rt.ui.PanelBg != want.ui.PanelBg {
		t.Errorf("panel_bg %q, want the generated block's %q (base %s paints %q)",
			rt.ui.PanelBg, want.ui.PanelBg, base, themes[base].ui.PanelBg)
	}
	if rt.Name != "moonveil-1999" {
		t.Errorf("selection reported as %q, want the marker's moonveil-1999", rt.Name)
	}

	if changed, err := migrateHerdrThemeConfig(cfg); err != nil || changed {
		t.Errorf("migrate rewrote a selection it cannot resolve: changed=%v err=%v", changed, err)
	}
	if got, _ := os.ReadFile(cfg); string(got) != string(before) {
		t.Errorf("config rewritten:\n%s\nwant:\n%s", got, before)
	}
}

// Not claiming a block must not mean never clearing one: herdr's own theme
// popup writes [theme].name, and once it moves off the base the block records,
// the block is leftovers herdr would keep applying — whether or not this build
// knows the theme it was generated for.
func TestOutsideRethemeClearsAForeignBlock(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	base := foreignSelection(t, cfg, "matte-black", "moonveil-1999")
	body, _ := os.ReadFile(cfg)
	moved := strings.Replace(string(body), "name = "+strconv.Quote(base), `name = "nord"`, 1)
	os.WriteFile(cfg, []byte(moved), 0o644)

	t.Setenv("HERDR_CONFIG_PATH", cfg)
	rt, stranded := loadHerdrThemeConfig("auto")
	if !stranded {
		t.Fatal("leftovers not reported: nothing would ever clear them")
	}
	if rt.Resolved != "nord" || rt.ui.PanelBg != themes["nord"].ui.PanelBg {
		t.Errorf("resolved %q panel_bg %q, want nord's — herdr paints what the human picked", rt.Resolved, rt.ui.PanelBg)
	}
	if changed, err := migrateHerdrThemeConfig(cfg); err != nil || !changed {
		t.Fatalf("migrate: changed=%v err=%v", changed, err)
	}
	if got, _ := os.ReadFile(cfg); strings.Contains(string(got), lassoThemeTag) {
		t.Errorf("leftovers survived:\n%s", got)
	}
}

// The tag on a generated token line is a delete right, and lasso 3.0.2 and
// earlier take it for any `# lasso-theme` line — including one written for a
// theme they cannot resolve, which is why they stripped Omarchy selections off
// a shared config.toml. Nothing this build generates may carry that tag; the
// identity marker, which those builds only read, still must.
func TestGeneratedTokensAreNotTaggedForOlderBuilds(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := setHerdrThemeName(cfg, "retro-82"); err != nil {
		t.Fatalf("select: %v", err)
	}
	body, _ := os.ReadFile(cfg)
	for _, line := range strings.Split(string(body), "\n") {
		// 3.0.2's own rule: a line whose trailing comment is exactly the tag.
		code, comment, ok := lineComment(line)
		if ok && comment == lassoThemeTag && strings.TrimSpace(code) != "" {
			t.Errorf("an older lasso would delete this line: %q", line)
		}
	}
	c := parseThemeConfig(cfg)
	if c.Marker != "retro-82" || c.MarkerBase != "vesper" {
		t.Errorf("marker=%q base=%q, want retro-82 on vesper", c.Marker, c.MarkerBase)
	}
	if len(c.Generated) == 0 {
		t.Error("no generated tokens: this build no longer reads its own block")
	}
}

// The upgrade path off the old representation (marker with no recorded base,
// tokens under the legacy tag): it has to migrate exactly once, keep resolving
// to the same theme, and not stack a second block beside the first.
func TestMigrateLegacyTaggedBlock(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := setHerdrThemeName(cfg, "retro-82"); err != nil {
		t.Fatalf("select: %v", err)
	}
	body, _ := os.ReadFile(cfg)
	legacy := strings.ReplaceAll(string(body), "# "+lassoTokenTag, "# "+lassoThemeTag)
	var kept []string
	for _, line := range strings.Split(legacy, "\n") {
		if markerBase(line) == "" {
			kept = append(kept, line)
		}
	}
	os.WriteFile(cfg, []byte(strings.Join(kept, "\n")), 0o644)

	if changed, err := migrateHerdrThemeConfig(cfg); err != nil || !changed {
		t.Fatalf("migrate: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(cfg)
	c := parseThemeConfig(cfg)
	if c.Name != "vesper" || c.lassoTheme() != "retro-82" || c.MarkerBase != "vesper" {
		t.Fatalf("migrated to name=%q marker=%q base=%q", c.Name, c.Marker, c.MarkerBase)
	}
	if n := strings.Count(string(got), "panel_bg ="); n != 1 {
		t.Errorf("panel_bg written %d times:\n%s", n, got)
	}
	t.Setenv("HERDR_CONFIG_PATH", cfg)
	if rt := loadHerdrTheme("auto"); rt.Resolved != "retro-82" || rt.Customized {
		t.Errorf("after migration resolved %q customized %v", rt.Resolved, rt.Customized)
	}
	if changed, err := migrateHerdrThemeConfig(cfg); err != nil || changed {
		t.Errorf("second migrate rewrote a current file: changed=%v err=%v", changed, err)
	}
}
