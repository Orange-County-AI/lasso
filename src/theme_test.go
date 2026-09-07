package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// themeStubBackend answers theme.list with a canned payload and theme files
// from a map, which is exactly the pair loadLuvusThemeFrom needs — no ssh, no
// running Luvus. It records the paths it was asked for, because "which files
// did resolution touch" is itself part of the contract.
type themeStubBackend struct {
	Backend
	list     string
	listErr  error
	files    map[string]string
	readErr  error
	readSeen []string
}

func (s *themeStubBackend) Name() string { return "stub" }

func (s *themeStubBackend) LuvusCall(method string, _ any) (json.RawMessage, error) {
	if method != "theme.list" {
		return nil, fmt.Errorf("unexpected method %q — theme resolution must not call anything else", method)
	}
	if s.listErr != nil {
		return nil, s.listErr
	}
	return json.RawMessage(s.list), nil
}

func (s *themeStubBackend) ReadFile(path string) ([]byte, error) {
	s.readSeen = append(s.readSeen, path)
	if s.readErr != nil {
		return nil, s.readErr
	}
	body, ok := s.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(body), nil
}

// themeListJSON renders a theme.list result. Each row is "<id>|<appearance>|
// <source>|<extends>", where source is "built_in", "virtual", or a file path.
// The first row is the active one.
func themeListJSON(revision int, rows ...string) string {
	var out []string
	for i, row := range rows {
		f := strings.Split(row, "|")
		src := `"built_in"`
		switch {
		case f[2] == "built_in" || f[2] == "virtual":
			src = `"` + f[2] + `"`
		default:
			src = `{"local":{"path":` + strconv.Quote(f[2]) + `,"installed_from":"/tmp/x.toml"}}`
		}
		ext := "null"
		if len(f) > 3 && f[3] != "" {
			ext = `"` + f[3] + `"`
		}
		out = append(out, fmt.Sprintf(
			`{"id":%q,"display_name":%q,"appearance":%q,"source":%s,"extends":%s,"active":%t}`,
			f[0], strings.ToUpper(f[0][:1])+f[0][1:], f[1], src, ext, i == 0))
	}
	return fmt.Sprintf(`{"revision":%d,"problems":[],"themes":[%s]}`, revision, strings.Join(out, ","))
}

// A bundled theme resolves to its transcribed palette, and every field the API
// serves comes from Luvus rather than from a lasso-side guess.
func TestBundledThemeResolves(t *testing.T) {
	b := &themeStubBackend{list: themeListJSON(7, "nord|dark|built_in", "sky|light|built_in")}
	rt, err := loadLuvusThemeFrom(b)
	if err != nil {
		t.Fatalf("loadLuvusThemeFrom: %v", err)
	}
	if rt.Name != "nord" || rt.Resolved != "nord" || rt.Label != "Nord" || rt.Appearance != "dark" {
		t.Errorf("got name=%q resolved=%q label=%q appearance=%q", rt.Name, rt.Resolved, rt.Label, rt.Appearance)
	}
	if !rt.Available || rt.Customized {
		t.Errorf("available=%v customized=%v", rt.Available, rt.Customized)
	}
	if rt.p != bundledPalettes["nord"] {
		t.Errorf("palette mismatch:\n got %+v\nwant %+v", rt.p, bundledPalettes["nord"])
	}
	// A bundled theme's colors are known without touching the filesystem.
	if len(b.readSeen) != 0 {
		t.Errorf("read %v for a built-in theme", b.readSeen)
	}

	// theme.list's `revision` is the server's GLOBAL session revision — a
	// workspace.open or tab.new moves it with no theme change — so it must not
	// reach resolvedTheme. h.refreshTheme compares two of these for equality to
	// decide "did the theme change", and a revision inside would make every
	// pane mutation look like a new palette and re-sync the whole fleet.
	moved := &themeStubBackend{list: themeListJSON(4096, "nord|dark|built_in", "sky|light|built_in")}
	bumped, err := loadLuvusThemeFrom(moved)
	if err != nil {
		t.Fatalf("loadLuvusThemeFrom: %v", err)
	}
	if bumped != rt {
		t.Errorf("a bumped session revision changed the theme:\n got %+v\nwant %+v", bumped, rt)
	}
}

// An installed theme extending a bundled one is the case the migration exists
// for: the file's own colors must win and the rest must come from the parent,
// so a Theme Maker palette lands verbatim instead of collapsing to its parent.
func TestInstalledThemeOverridesParent(t *testing.T) {
	path := "/luvus/themes/warm.toml"
	b := &themeStubBackend{
		list: themeListJSON(3, "warm|dark|"+path+"|nord", "nord|dark|built_in"),
		files: map[string]string{path: `schema = 1
id = "warm"
extends = "nord"   # comment must not be read as part of the value

[colors]
accent = '#ff00ff'
sel_bg = "\u0023330033"
coral = "reset"
`},
	}
	rt, err := loadLuvusThemeFrom(b)
	if err != nil {
		t.Fatalf("loadLuvusThemeFrom: %v", err)
	}
	if rt.p.Accent != "#ff00ff" || rt.p.SelBg != "#330033" {
		t.Errorf("overrides lost: accent=%q sel_bg=%q", rt.p.Accent, rt.p.SelBg)
	}
	// `reset` names no color, so the parent's role must survive it.
	if rt.p.Coral != bundledPalettes["nord"].Coral || rt.p.Text != bundledPalettes["nord"].Text {
		t.Errorf("parent roles lost: coral=%q text=%q", rt.p.Coral, rt.p.Text)
	}
	if rt.Name != "warm" || rt.Resolved != "nord" || !rt.Customized {
		t.Errorf("name=%q resolved=%q customized=%v", rt.Name, rt.Resolved, rt.Customized)
	}
	// The palette drives both surfaces, not just the CSS.
	if !strings.Contains(rt.cssVars(), "--accent: #ff00ff;") {
		t.Errorf("css did not follow the custom accent:\n%s", rt.cssVars())
	}
	if !strings.Contains(rt.xtermJSON(), `"selectionBackground":"#ff00ff66"`) {
		t.Errorf("terminal selection did not follow the custom accent:\n%s", rt.xtermJSON())
	}
}

// A complete installed theme (no parent) is resolved entirely from its file.
func TestCompleteInstalledTheme(t *testing.T) {
	path := "/luvus/themes/full.toml"
	var body strings.Builder
	body.WriteString("schema = 1\nid = \"full\"\n\n[colors]\n")
	for role, hex := range map[string]string{
		"crust": "#010101", "mantle": "#020202", "base": "#030303", "surface0": "#040404",
		"surface1": "#050505", "overlay0": "#060606", "overlay1": "#070707",
		"subtext0": "#080808", "subtext1": "#090909", "text": "#0a0a0a",
		"accent": "#0b0b0b", "sel_bg": "#0c0c0c", "border": "#0d0d0d",
		"border_focus": "#0e0e0e", "green": "#0f0f0f", "mint": "#101010",
		"amber": "#111111", "coral": "#121212",
	} {
		fmt.Fprintf(&body, "%s = %q\n", role, hex)
	}
	b := &themeStubBackend{
		list:  themeListJSON(1, "full|dark|"+path),
		files: map[string]string{path: body.String()},
	}
	rt, err := loadLuvusThemeFrom(b)
	if err != nil {
		t.Fatalf("loadLuvusThemeFrom: %v", err)
	}
	if rt.p.Text != "#0a0a0a" || rt.p.Accent != "#0b0b0b" || rt.p.Coral != "#121212" {
		t.Errorf("palette not taken from the file: %+v", rt.p)
	}
	if rt.Resolved != "full" || !rt.Customized {
		t.Errorf("resolved=%q customized=%v", rt.Resolved, rt.Customized)
	}
}

// The virtual Terminal theme has no colors of its own — it is derived from the
// host terminal, which lasso's embedded terminals are not — so it must render
// Luvus's bundled fallback instead of a blank palette.
func TestVirtualTerminalTheme(t *testing.T) {
	b := &themeStubBackend{list: themeListJSON(1, "terminal|terminal|virtual")}
	rt, err := loadLuvusThemeFrom(b)
	if err != nil {
		t.Fatalf("loadLuvusThemeFrom: %v", err)
	}
	if rt.Name != "terminal" || rt.Resolved != luvusDefaultTheme {
		t.Errorf("name=%q resolved=%q", rt.Name, rt.Resolved)
	}
	if rt.p != bundledPalettes[luvusDefaultTheme] {
		t.Errorf("terminal theme did not fall back to %s", luvusDefaultTheme)
	}
	// "terminal" is neither dark nor light, so the background decides.
	if rt.light() {
		t.Error("a dark fallback palette read as light")
	}
}

// An unresolved source must not become a guessed palette eligible for sync.
func TestUnresolvablePaletteRefusesSync(t *testing.T) {
	cases := map[string]*themeStubBackend{
		// A bundled theme added by a newer Luvus than this build knows.
		"unknown built-in": {list: themeListJSON(1, "aurora-new|dark|built_in")},
		// An installed theme whose file has gone.
		"missing file": {list: themeListJSON(1, "gone|dark|/luvus/themes/gone.toml")},
		// A child naming a parent that is not registered.
		"orphan parent": {
			list:  themeListJSON(1, "child|dark|/luvus/themes/child.toml|nope"),
			files: map[string]string{"/luvus/themes/child.toml": "[colors]\naccent = \"#123456\"\n"},
		},
	}
	for name, b := range cases {
		rt, err := loadLuvusThemeFrom(b)
		if err == nil || rt.Available {
			t.Errorf("%s: unresolved source became syncable: available=%v err=%v", name, rt.Available, err)
		}
	}
}

// A cyclic inheritance graph must fail rather than syncing a partial palette.
func TestExtendsCycleTerminates(t *testing.T) {
	a, z := "/luvus/themes/a.toml", "/luvus/themes/z.toml"
	b := &themeStubBackend{
		list: themeListJSON(1, "a|dark|"+a+"|z", "z|dark|"+z+"|a"),
		files: map[string]string{
			a: "extends = \"z\"\n[colors]\naccent = \"#aaaaaa\"\n",
			z: "extends = \"a\"\n[colors]\ntext = \"#zzz-not-a-color\"\n",
		},
	}
	rt, err := loadLuvusThemeFrom(b)
	if err == nil || rt.Available {
		t.Fatalf("cyclic palette became syncable: available=%v err=%v", rt.Available, err)
	}
}

// A theme lasso cannot READ must not be reported as a palette. Painting a
// stand-in as if it were the selection — and worse, syncing it to every host —
// is the failure unavailableTheme exists to keep visible.
func TestUnreachableLuvusIsAnError(t *testing.T) {
	for name, b := range map[string]*themeStubBackend{
		"transport failure": {listErr: errors.New("dial: no such file")},
		"garbage reply":     {list: `not json`},
		"no active theme":   {list: themeListJSON(1)},
	} {
		rt, err := loadLuvusThemeFrom(b)
		if err == nil {
			t.Errorf("%s: want an error, got theme %q", name, rt.Name)
		}
		if rt.Available || rt.Name != "" || rt.Resolved != "" {
			t.Errorf("%s: stand-in claims a selection: available=%v name=%q resolved=%q",
				name, rt.Available, rt.Name, rt.Resolved)
		}
		if _, _, _, ok := hexRGB(rt.p.Mantle); !ok {
			t.Errorf("%s: stand-in palette is not renderable: %+v", name, rt.p)
		}
	}
	if rt, err := loadLuvusThemeFrom(nil); err == nil || rt.Available {
		t.Errorf("a nil backend must not resolve a theme (err=%v)", err)
	}
}

// fingerprint is what decides whether a host is in step. An installed theme can
// be edited and reinstalled under the SAME id, so a name-keyed check would call
// the fleet up to date while every host renders the old colors.
func TestFingerprintFollowsColorsNotName(t *testing.T) {
	path := "/luvus/themes/edit.toml"
	load := func(accent string) resolvedTheme {
		b := &themeStubBackend{
			list:  themeListJSON(1, "edit|dark|"+path+"|nord", "nord|dark|built_in"),
			files: map[string]string{path: "[colors]\naccent = \"" + accent + "\"\n"},
		}
		rt, err := loadLuvusThemeFrom(b)
		if err != nil {
			t.Fatalf("loadLuvusThemeFrom: %v", err)
		}
		return rt
	}
	before, after := load("#111111"), load("#222222")
	if before.Name != after.Name {
		t.Fatalf("test setup: names differ (%q vs %q)", before.Name, after.Name)
	}
	if before.fingerprint() == after.fingerprint() {
		t.Errorf("edited palette kept fingerprint %q", before.fingerprint())
	}
	if load("#111111").fingerprint() != before.fingerprint() {
		t.Error("an unchanged palette must fingerprint identically, or every probe rewrites the fleet")
	}
	if unavailableTheme().fingerprint() != "" {
		t.Error("the stand-in must not fingerprint, or it would be recorded as synced")
	}
}

// Schema 1 accepts exactly three color forms, and `reset` deliberately resolves
// to nothing so the role it names keeps what it inherited.
func TestParseThemeColor(t *testing.T) {
	for in, want := range map[string]string{
		"#ff0000":   "#ff0000",
		"#FF0000":   "#ff0000",
		"ansi(1)":   "#800000",
		"ansi(16)":  "#000000",
		"ansi(46)":  "#00ff00",
		"ansi(255)": "#eeeeee",
	} {
		if got, ok := parseThemeColor(in); !ok || got != want {
			t.Errorf("parseThemeColor(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"reset", "", "  ", "puce", "ansi(256)", "ansi(-1)", "ansi()", "#12345", "#gggggg"} {
		if got, ok := parseThemeColor(in); ok {
			t.Errorf("parseThemeColor(%q) = %q, want no color", in, got)
		}
	}
}

// Every transcribed palette must be complete and well-formed: one mistyped role
// paints a blank surface, and there is no runtime check that would catch it.
func TestBundledPalettesComplete(t *testing.T) {
	if len(bundledPalettes) != 17 {
		t.Errorf("got %d bundled palettes, want Luvus's 17", len(bundledPalettes))
	}
	if _, ok := bundledPalettes[luvusDefaultTheme]; !ok {
		t.Fatalf("the default theme %q is not in the table", luvusDefaultTheme)
	}
	for id, p := range bundledPalettes {
		for role, hex := range map[string]string{
			"crust": p.Crust, "mantle": p.Mantle, "base": p.Base, "surface0": p.Surface0,
			"surface1": p.Surface1, "overlay0": p.Overlay0, "overlay1": p.Overlay1,
			"subtext0": p.Subtext0, "subtext1": p.Subtext1, "text": p.Text,
			"accent": p.Accent, "sel_bg": p.SelBg, "border": p.Border,
			"border_focus": p.BorderFocus, "green": p.Green, "mint": p.Mint,
			"amber": p.Amber, "coral": p.Coral,
		} {
			if _, _, _, ok := hexRGB(hex); !ok {
				t.Errorf("%s.%s = %q, want #rrggbb", id, role, hex)
			}
		}
	}
}

// A few canonical values, so a wholesale re-transcription of the registry can't
// quietly shift a palette people recognize.
func TestBundledPalettesMatchLuvus(t *testing.T) {
	for id, want := range map[string]luvusPalette{
		"nord":          {Mantle: "#2e3440", Text: "#eceff4", Accent: "#88c0d0", Green: "#a3be8c", Coral: "#bf616a"},
		"quattro-rally": {Mantle: "#1e2030", Text: "#cad3f5", Accent: "#dbc66f", Green: "#94a143", Coral: "#cf5a44"},
		"noir":          {Mantle: "#111116", Text: "#e7e7ed", Accent: "#c6ff1a", Green: "#8fbc7a", Coral: "#e06c66"},
	} {
		got := bundledPalettes[id]
		if got.Mantle != want.Mantle || got.Text != want.Text || got.Accent != want.Accent ||
			got.Green != want.Green || got.Coral != want.Coral {
			t.Errorf("%s: got mantle=%s text=%s accent=%s green=%s coral=%s",
				id, got.Mantle, got.Text, got.Accent, got.Green, got.Coral)
		}
	}
}

// Luvus's `appearance` is authoritative, so the light variants must read light
// and their derived terminal palette must not invert: a "black" brighter than
// its "white" makes ordinary output unreadable on a light background.
func TestLightThemesDeriveLightTerminals(t *testing.T) {
	light := map[string]bool{"sky": true, "catppuccin-latte": true, "gruvbox-light": true}
	for id := range bundledPalettes {
		rt := bundledTheme(id)
		if rt.light() != light[id] {
			t.Errorf("%s: light()=%v, want %v", id, rt.light(), light[id])
		}
		if luminance(rt.ansi.Black) >= luminance(rt.ansi.White) {
			t.Errorf("%s: ansi black %s is not darker than white %s", id, rt.ansi.Black, rt.ansi.White)
		}
		if luminance(rt.ansi.BrightWhite) == luminance(rt.ansi.White) && rt.ansi.BrightWhite == rt.ansi.White {
			t.Errorf("%s: brightWhite and white collapsed to %s", id, rt.ansi.White)
		}
	}
}

// The 16 ANSI colors are derived, and every slot must be a real color: xterm.js
// silently ignores a malformed one and falls back to its own default, which is
// how a terminal ends up half-themed.
func TestXtermThemeIsComplete(t *testing.T) {
	rt := bundledTheme("gruvbox")
	var got map[string]string
	if err := json.Unmarshal([]byte(rt.xtermJSON()), &got); err != nil {
		t.Fatalf("xtermJSON is not valid JSON: %v\n%s", err, rt.xtermJSON())
	}
	for _, key := range []string{
		"background", "foreground", "cursor", "cursorAccent",
		"black", "red", "green", "yellow", "blue", "magenta", "cyan", "white",
		"brightBlack", "brightRed", "brightGreen", "brightYellow",
		"brightBlue", "brightMagenta", "brightCyan", "brightWhite",
	} {
		if _, _, _, ok := hexRGB(got[key]); !ok {
			t.Errorf("xterm %s = %q, want #rrggbb", key, got[key])
		}
	}
	// Selection is a translucent wash of the accent, so it stays visible on
	// every theme instead of vanishing against a near-background sel_bg.
	if want := rt.p.Accent + "66"; got["selectionBackground"] != want {
		t.Errorf("selectionBackground = %q, want %q", got["selectionBackground"], want)
	}
	if got["background"] != rt.p.Mantle {
		t.Errorf("terminal background %q must be the pane background %q", got["background"], rt.p.Mantle)
	}
}

// The sidebar's semantic vars must come off distinct roles: --good is the
// success color and stays on mint whatever the accent is, --muted is the
// secondary-text tier (subtext0, not the dimmest overlay0, which reads at ~2:1
// against the panel and is too low for form labels).
func TestCSSVarsUseDistinctRoles(t *testing.T) {
	rt := bundledTheme("quattro-rally")
	css := rt.cssVars()
	for _, want := range []string{
		"--bg: " + rt.p.Mantle + ";", "--panel: " + rt.p.Surface0 + ";",
		"--border: " + rt.p.Border + ";", "--hover: " + rt.p.Surface1 + ";",
		"--fg: " + rt.p.Text + ";", "--muted: " + rt.p.Subtext0 + ";",
		"--accent: " + rt.p.Accent + ";", "--accent-dim: " + rt.p.Accent + "26;",
		"--good: " + rt.p.Mint + ";", "--warn: " + rt.p.Amber + ";", "--bad: " + rt.p.Coral + ";",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("css missing %q in:\n%s", want, css)
		}
	}
	if rt.p.Accent == rt.p.Mint {
		t.Skip("this theme's accent is its mint; the distinction is untestable here")
	}
	if strings.Contains(css, "--good: "+rt.p.Accent+";") {
		t.Error("--good followed the accent; it must stay the success color")
	}
}

// A theme file is read from the path theme.list reports, and only that path —
// lasso must never guess $LUVUS_HOME or scan the themes directory, which is
// what makes the resolution work identically on a remote host.
func TestThemeFileReadFromReportedPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.toml")
	if err := os.WriteFile(path, []byte("[colors]\naccent = \"#abcdef\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &themeStubBackend{
		list:  themeListJSON(1, "custom|dark|"+path+"|nord", "nord|dark|built_in"),
		files: map[string]string{path: "[colors]\naccent = \"#abcdef\"\n"},
	}
	rt, err := loadLuvusThemeFrom(b)
	if err != nil {
		t.Fatalf("loadLuvusThemeFrom: %v", err)
	}
	if rt.p.Accent != "#abcdef" {
		t.Errorf("accent = %q", rt.p.Accent)
	}
	if len(b.readSeen) != 1 || b.readSeen[0] != path {
		t.Errorf("read %v, want exactly [%s]", b.readSeen, path)
	}
}
