package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testHarborColors = `mode = "dark"
accent = "#f2b35e"
background = "#0f1b24"
foreground = "#c3d2da"
red = "#ef7f7a"
yellow = "#f2b35e"
green = "#8fcf9c"
cyan = "#6cc6c9"
blue = "#79aee8"
magenta = "#c39ae6"
`

// appearanceManifest is a valid themes+fonts manifest for plugin name.
func appearanceManifest(name, themeID string) map[string]any {
	return map[string]any{
		"name": name, "version": "0.1.0",
		"themes": []any{map[string]any{"id": themeID, "label": "Harbor Night", "dir": "themes/night"}},
		"fonts": []any{map[string]any{
			"id": "mono", "family": "Space Mono", "category": "mono", "license": "OFL-1.1",
			"faces": []any{
				map[string]any{"file": "fonts/regular.woff2", "weight": 400, "style": "normal"},
				map[string]any{"file": "fonts/bold.woff2", "weight": 700, "style": "italic"},
			},
		}},
	}
}

func appearanceFiles() map[string]string {
	return map[string]string{
		"themes/night/colors.toml":          testHarborColors,
		"themes/night/backgrounds/dusk.png": "\x89PNG fake",
		"fonts/regular.woff2":               "wOF2 regular",
		"fonts/bold.woff2":                  "wOF2 bold",
	}
}

func TestPluginAppearanceValidation(t *testing.T) {
	type mut func(m map[string]any, files map[string]string)
	font := func(m map[string]any) map[string]any { return m["fonts"].([]any)[0].(map[string]any) }
	face := func(m map[string]any) map[string]any { return font(m)["faces"].([]any)[0].(map[string]any) }
	theme := func(m map[string]any) map[string]any { return m["themes"].([]any)[0].(map[string]any) }
	cases := []struct {
		name    string
		mut     mut
		wantErr string
	}{
		{"ok", func(map[string]any, map[string]string) {}, ""},
		{"appearance only, no label", func(m map[string]any, _ map[string]string) { delete(theme(m), "label") }, ""},
		{"css injection in family", func(m map[string]any, _ map[string]string) {
			font(m)["family"] = `Inter"; } body { display: none } x {`
		}, "family"},
		{"family with a quote", func(m map[string]any, _ map[string]string) { font(m)["family"] = `Inter'` }, "family"},
		{"blank family", func(m map[string]any, _ map[string]string) { font(m)["family"] = "   " }, "family"},
		{"bad category", func(m map[string]any, _ map[string]string) { font(m)["category"] = "cursive" }, "category"},
		{"bad font id", func(m map[string]any, _ map[string]string) { font(m)["id"] = "Mono!" }, "id"},
		{"traversal face", func(m map[string]any, _ map[string]string) { face(m)["file"] = "../../etc/passwd.woff2" }, ".."},
		{"absolute face", func(m map[string]any, _ map[string]string) { face(m)["file"] = "/etc/x.woff2" }, "relative"},
		{"bad extension", func(m map[string]any, f map[string]string) {
			face(m)["file"] = "fonts/x.svg"
			f["fonts/x.svg"] = "<svg/>"
		}, ".woff2"},
		{"missing face file", func(m map[string]any, _ map[string]string) { face(m)["file"] = "fonts/gone.woff2" }, "not a file"},
		{"oversized face", func(m map[string]any, f map[string]string) {
			f["fonts/regular.woff2"] = strings.Repeat("x", pluginFontFileMax+1)
		}, "larger than"},
		{"bad weight", func(m map[string]any, _ map[string]string) { face(m)["weight"] = 450 }, "weight"},
		{"weight too high", func(m map[string]any, _ map[string]string) { face(m)["weight"] = 1000 }, "weight"},
		{"bad style", func(m map[string]any, _ map[string]string) { face(m)["style"] = "oblique" }, "style"},
		{"no faces", func(m map[string]any, _ map[string]string) { font(m)["faces"] = []any{} }, "faces"},
		{"too many faces", func(m map[string]any, _ map[string]string) {
			fs := []any{}
			for range 9 {
				fs = append(fs, map[string]any{"file": "fonts/regular.woff2", "weight": 400, "style": "normal"})
			}
			font(m)["faces"] = fs
		}, "faces"},
		{"bad theme id", func(m map[string]any, _ map[string]string) { theme(m)["id"] = "Harbor Night" }, "id"},
		{"theme dir escapes", func(m map[string]any, _ map[string]string) { theme(m)["dir"] = "../elsewhere" }, ".."},
		{"theme dir missing", func(m map[string]any, _ map[string]string) { theme(m)["dir"] = "themes/nope" }, "not a directory"},
		{"palette unparseable", func(_ map[string]any, f map[string]string) { f["themes/night/colors.toml"] = "background = \"#000\"\n" }, "no usable palette"},
		{"dup theme id", func(m map[string]any, _ map[string]string) {
			m["themes"] = append(m["themes"].([]any), theme(m))
		}, "duplicate id"},
		{"too many fonts", func(m map[string]any, _ map[string]string) {
			fs := []any{}
			for i := range 17 {
				f := map[string]any{}
				for k, v := range font(m) {
					f[k] = v
				}
				f["id"] = "f" + strings.Repeat("x", i)
				fs = append(fs, f)
			}
			m["fonts"] = fs
		}, "at most 16 fonts"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, files := appearanceManifest("harbor", "harbor-night"), appearanceFiles()
			c.mut(m, files)
			pd := writePlugin(t, t.TempDir(), "harbor", m, files)
			_, err := loadPluginManifest(pd, "harbor")
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, c.wantErr)
			}
		})
	}

	// A face symlinked out of the plugin is refused by the os.Root, not
	// merely by the string checks.
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "evil.woff2")
	_ = os.WriteFile(outside, []byte("x"), 0o644)
	files := appearanceFiles()
	delete(files, "fonts/regular.woff2")
	pd := writePlugin(t, dir, "harbor", appearanceManifest("harbor", "harbor-night"), files)
	if err := os.Symlink(outside, filepath.Join(pd, "fonts", "regular.woff2")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPluginManifest(pd, "harbor"); err == nil {
		t.Error("a face symlinked outside the plugin was accepted")
	}
}

func TestPluginAppearanceFingerprint(t *testing.T) {
	load := func(m map[string]any, files map[string]string) *pluginManifest {
		t.Helper()
		pd := writePlugin(t, t.TempDir(), "harbor", m, files)
		man, err := loadPluginManifest(pd, "harbor")
		if err != nil {
			t.Fatal(err)
		}
		return man
	}
	base := load(appearanceManifest("harbor", "harbor-night"), appearanceFiles())
	fp := base.fingerprint()

	// A palette edit is not a permission change.
	files := appearanceFiles()
	files["themes/night/colors.toml"] = strings.Replace(testHarborColors, "#f2b35e", "#ff0000", -1)
	edited := load(appearanceManifest("harbor", "harbor-night"), files)
	if edited.fingerprint() != fp {
		t.Error("editing a palette's colours changed the fingerprint")
	}
	if edited.themeSig == base.themeSig {
		t.Error("editing a palette did not change the theme digest the registry rebuild keys on")
	}
	// Nor is a label, a license, or a face's file.
	m := appearanceManifest("harbor", "harbor-night")
	m["themes"].([]any)[0].(map[string]any)["label"] = "Other"
	m["fonts"].([]any)[0].(map[string]any)["license"] = "MIT"
	if load(m, appearanceFiles()).fingerprint() != fp {
		t.Error("a label or license edit changed the fingerprint")
	}

	// The theme id, and a font's id, family or category, are.
	for name, f := range map[string]func(map[string]any){
		"theme id":      func(m map[string]any) { m["themes"].([]any)[0].(map[string]any)["id"] = "harbor-day" },
		"font family":   func(m map[string]any) { m["fonts"].([]any)[0].(map[string]any)["family"] = "Other Mono" },
		"font category": func(m map[string]any) { m["fonts"].([]any)[0].(map[string]any)["category"] = "sans" },
		"font id":       func(m map[string]any) { m["fonts"].([]any)[0].(map[string]any)["id"] = "other" },
		"no fonts":      func(m map[string]any) { delete(m, "fonts") },
	} {
		m := appearanceManifest("harbor", "harbor-night")
		f(m)
		if load(m, appearanceFiles()).fingerprint() == fp {
			t.Errorf("%s did not change the fingerprint", name)
		}
	}

	// A plugin with no themes or fonts keeps the fingerprint it was approved
	// under before appearance existed.
	plain := &pluginManifest{Name: "hello", Tabs: []pluginTabSpec{{ID: "main", Label: "H", Entry: "ui/index.html"}}}
	withEmpty := &pluginManifest{Name: "hello", Tabs: plain.Tabs, Themes: []pluginThemeSpec{}, Fonts: []pluginFontSpec{}}
	if plain.fingerprint() != withEmpty.fingerprint() {
		t.Error("an empty themes/fonts list changed the fingerprint")
	}
}

func catalogEntry(name string) *themeCatalogEntry {
	for _, e := range themeCatalog() {
		if e.Name == name {
			return &e
		}
	}
	return nil
}

func TestPluginThemeRegistryFollowsEnable(t *testing.T) {
	m, dir := testPluginManager(t)
	pd := writePlugin(t, dir, "harbor", appearanceManifest("harbor", "harbor-night"), appearanceFiles())
	bumps := 0
	m.onChange = func() { bumps++ }
	m.rescan()

	if themeResolvable("harbor-night") {
		t.Fatal("a disabled plugin's theme is in the registry")
	}
	p := pluginByName(t, m.listing(), "harbor")
	if len(p.Themes) != 1 || p.Themes[0].ID != "harbor-night" || p.Themes[0].KeyTaken {
		t.Errorf("disabled listing themes = %+v", p.Themes)
	}
	if len(p.Fonts) != 1 || len(p.Fonts[0].Faces) != 0 || p.Fonts[0].GlobalID != "plugin:harbor:mono" {
		t.Errorf("a disabled plugin's fonts must be listed without faces: %+v", p.Fonts)
	}
	if !slices.Equal(p.Permissions.Themes, []string{"harbor-night"}) || len(p.Permissions.Fonts) != 1 || p.Permissions.Fonts[0].Family != "Space Mono" {
		t.Errorf("permissions = %+v", p.Permissions)
	}

	if err := m.enable("harbor", p.Fingerprint); err != nil {
		t.Fatal(err)
	}
	def, ok := lookupThemeDef("harbor-night")
	if !ok || def.ui.PanelBg != "#0f1b24" {
		t.Fatalf("enabled plugin theme = %+v %v", def.ui, ok)
	}
	e := catalogEntry("harbor-night")
	if e == nil || e.Source != "plugin" || e.Plugin != "harbor" || e.Label != "Harbor Night" || e.Installed {
		t.Fatalf("catalog row = %+v", e)
	}
	if len(e.Backgrounds) != 1 || e.Backgrounds[0] != "/omarchy/bg/harbor-night/dusk.png" || len(e.Thumbs) != 1 {
		t.Fatalf("backgrounds = %v thumbs = %v", e.Backgrounds, e.Thumbs)
	}
	rec := httptest.NewRecorder()
	serveOmarchyBackground(rec, httptest.NewRequest(http.MethodGet, e.Backgrounds[0], nil))
	if rec.Code != 200 || rec.Body.String() != "\x89PNG fake" || rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("serve plugin background: %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	for _, bad := range []string{"/omarchy/bg/harbor-night/..%2fcolors.toml", "/omarchy/bg/harbor-night/colors.toml", "/omarchy/bg/harbor-night/missing.png"} {
		rec := httptest.NewRecorder()
		serveOmarchyBackground(rec, httptest.NewRequest(http.MethodGet, bad, nil))
		if rec.Code != 404 {
			t.Errorf("%s: %d, want 404", bad, rec.Code)
		}
	}
	found := false
	for _, o := range themeOptionsAll() {
		found = found || o.Name == "harbor-night"
	}
	if !found {
		t.Error("the enabled plugin theme is not selectable in /api/theme's themes")
	}
	if rt := resolveThemeByName("harbor-night"); rt.Resolved != "harbor-night" || rt.Foreign {
		t.Errorf("resolveThemeByName = %+v", rt)
	}

	// A palette edited in place rebuilds the registry on the next rescan and
	// announces it (plugins_rev) even though the listing reads the same.
	before := bumps
	_ = os.WriteFile(filepath.Join(pd, "themes/night/colors.toml"), []byte(strings.Replace(testHarborColors, "#0f1b24", "#101010", 1)), 0o644)
	m.rescan()
	if def, _ := lookupThemeDef("harbor-night"); def.ui.PanelBg != "#101010" {
		t.Errorf("palette edit not picked up: bg = %s", def.ui.PanelBg)
	}
	if bumps == before {
		t.Error("a palette edit did not bump plugins_rev")
	}
	if pluginByName(t, m.listing(), "harbor").State != pluginStateEnabled {
		t.Error("a palette edit needed re-approval")
	}
	before = bumps
	m.rescan()
	if bumps != before {
		t.Error("a rescan that found nothing new bumped plugins_rev")
	}

	// Disabling withdraws it everywhere.
	if err := m.disable("harbor"); err != nil {
		t.Fatal(err)
	}
	if themeResolvable("harbor-night") || catalogEntry("harbor-night") != nil {
		t.Error("a disabled plugin's theme is still in the registry")
	}
	rec = httptest.NewRecorder()
	serveOmarchyBackground(rec, httptest.NewRequest(http.MethodGet, "/omarchy/bg/harbor-night/dusk.png", nil))
	if rec.Code != 404 {
		t.Errorf("a disabled plugin's background is still served: %d", rec.Code)
	}
}

func TestPluginThemeCollisions(t *testing.T) {
	m, dir := testPluginManager(t)
	writePlugin(t, dir, "alpha", appearanceManifest("alpha", "shared-sea"), appearanceFiles())
	betaFiles := appearanceFiles()
	betaFiles["themes/night/colors.toml"] = strings.Replace(testHarborColors, "#0f1b24", "#222222", 1)
	writePlugin(t, dir, "beta", appearanceManifest("beta", "shared-sea"), betaFiles)
	writePlugin(t, dir, "gamma", appearanceManifest("gamma", "nord"), appearanceFiles())
	writePlugin(t, dir, "delta", appearanceManifest("delta", "retro-82"), appearanceFiles())
	m.rescan()

	// Before approval a collision with an existing theme is already visible.
	if p := pluginByName(t, m.listing(), "gamma"); !p.Themes[0].KeyTaken || len(p.Warnings) != 1 {
		t.Errorf("disabled gamma = %+v / %v", p.Themes, p.Warnings)
	}
	// Enabled in the "wrong" order on purpose: precedence is by name, not by
	// who was approved first.
	for _, n := range []string{"beta", "alpha", "gamma", "delta"} {
		if err := m.enable(n, ""); err != nil {
			t.Fatalf("enable %s: %v (a taken key must be a warning, never an invalid manifest)", n, err)
		}
	}
	if def, _ := lookupThemeDef("shared-sea"); def.ui.PanelBg != "#0f1b24" {
		t.Errorf("shared-sea resolved to %s; the plugin that sorts first (alpha) must win", def.ui.PanelBg)
	}
	if e := catalogEntry("shared-sea"); e == nil || e.Plugin != "alpha" {
		t.Errorf("catalog shared-sea = %+v", e)
	}
	l := m.listing()
	if p := pluginByName(t, l, "alpha"); p.Themes[0].KeyTaken || len(p.Warnings) != 0 {
		t.Errorf("alpha = %+v / %v", p.Themes, p.Warnings)
	}
	if p := pluginByName(t, l, "beta"); !p.Themes[0].KeyTaken || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], `plugin "alpha"`) {
		t.Errorf("beta = %+v / %v", p.Themes, p.Warnings)
	}
	if p := pluginByName(t, l, "gamma"); !p.Themes[0].KeyTaken || !strings.Contains(strings.Join(p.Warnings, ""), "built-in") || p.State != pluginStateEnabled {
		t.Errorf("gamma = %+v / %v / %s", p.Themes, p.Warnings, p.State)
	}
	if def, _ := lookupThemeDef("nord"); def != themes["nord"] {
		t.Error("a plugin redefined a built-in theme")
	}
	if p := pluginByName(t, l, "delta"); !p.Themes[0].KeyTaken {
		t.Errorf("delta (retro-82) = %+v", p.Themes)
	}
	if e := catalogEntry("retro-82"); e == nil || e.Source == "plugin" {
		t.Errorf("retro-82 catalog row = %+v", e)
	}

	// Alpha leaving hands the key to beta.
	if err := m.disable("alpha"); err != nil {
		t.Fatal(err)
	}
	if def, _ := lookupThemeDef("shared-sea"); def.ui.PanelBg != "#222222" {
		t.Errorf("after alpha left, shared-sea = %s; want beta's", def.ui.PanelBg)
	}
	if p := pluginByName(t, m.listing(), "beta"); p.Themes[0].KeyTaken || len(p.Warnings) != 0 {
		t.Errorf("beta after alpha left = %+v / %v", p.Themes, p.Warnings)
	}
}

func TestPluginFontServedAndListed(t *testing.T) {
	m, dir := testPluginManager(t)
	writePlugin(t, dir, "harbor", appearanceManifest("harbor", "harbor-night"), appearanceFiles())
	m.rescan()
	if err := m.enable("harbor", ""); err != nil {
		t.Fatal(err)
	}
	p := pluginByName(t, m.listing(), "harbor")
	if len(p.Fonts) != 1 {
		t.Fatalf("fonts = %+v", p.Fonts)
	}
	f := p.Fonts[0]
	if f.GlobalID != "plugin:harbor:mono" || f.Family != "Space Mono" || f.Category != "mono" || f.License != "OFL-1.1" || len(f.Faces) != 2 {
		t.Fatalf("font = %+v", f)
	}
	if f.Faces[0].URL != "/plugins/harbor/fonts/regular.woff2" || f.Faces[0].Weight != 400 || f.Faces[1].Style != "italic" {
		t.Errorf("faces = %+v", f.Faces)
	}
	r := httptest.NewRequest(http.MethodGet, f.Faces[0].URL, nil)
	w := httptest.NewRecorder()
	m.serveFiles(w, r)
	if w.Code != 200 || w.Body.String() != "wOF2 regular" {
		t.Fatalf("font file = %d %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "font/woff2" {
		t.Errorf("Content-Type = %q, want font/woff2", ct)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("headers = %v", w.Header())
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"warnings":[]`) {
		t.Errorf("warnings must be an array, never null: %s", b)
	}
}

func TestTypographyUIState(t *testing.T) {
	openTestDB(t)
	us, _ := getUIState()
	if us.Typography == nil || len(us.Typography) != 0 {
		t.Fatalf("default typography = %#v; want {}", us.Typography)
	}
	postUIState(t, `{"typography":{"sans":"plugin:harbor:inter"}}`)
	// A second device editing a different slot must not drop the first.
	got := postUIState(t, `{"typography":{"terminal":"plugin:harbor:mono"}}`)
	if got.Typography["sans"] != "plugin:harbor:inter" || got.Typography["terminal"] != "plugin:harbor:mono" {
		t.Fatalf("per-slot merge lost a slot: %v", got.Typography)
	}
	// An unrelated patch leaves it alone.
	got = postUIState(t, `{"usage_compact":true}`)
	if len(got.Typography) != 2 {
		t.Fatalf("an unrelated patch touched typography: %v", got.Typography)
	}
	// "" returns a slot to lasso's default, which is its absence.
	got = postUIState(t, `{"typography":{"sans":""}}`)
	if _, ok := got.Typography["sans"]; ok || got.Typography["terminal"] != "plugin:harbor:mono" {
		t.Fatalf("clearing sans = %v", got.Typography)
	}
	// A well-formed id for a plugin that is not there is kept.
	got = postUIState(t, `{"typography":{"display":"plugin:gone:font"}}`)
	if got.Typography["display"] != "plugin:gone:font" {
		t.Errorf("an absent plugin's font id was dropped: %v", got.Typography)
	}

	for name, body := range map[string]string{
		"unknown slot":  `{"typography":{"body":"plugin:a:b"}}`,
		"not an id":     `{"typography":{"sans":"Inter"}}`,
		"css injection": `{"typography":{"sans":"plugin:a:b\"; } x {"}}`,
		"too long":      `{"typography":{"sans":"plugin:` + strings.Repeat("a", 100) + `:b"}}`,
		"not a string":  `{"typography":{"sans":7}}`,
		"not an object": `{"typography":"plugin:a:b"}`,
	} {
		if w := postUIStateRaw(t, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s; want 400", name, w.Code, w.Body.String())
		}
	}
	us, _ = getUIState()
	if len(us.Typography) != 2 || us.Typography["terminal"] != "plugin:harbor:mono" {
		t.Errorf("a refused patch changed what is stored: %v", us.Typography)
	}
}

// The shipped example must stay a valid plugin whose theme and font load.
func TestHarborExamplePlugin(t *testing.T) {
	src := filepath.Join("..", "examples", "plugins", "harbor")
	man, err := loadPluginManifest(src, "harbor")
	if err != nil {
		t.Fatalf("examples/plugins/harbor: %v", err)
	}
	if len(man.Themes) != 1 || len(man.Fonts) != 1 || man.MCP != nil || len(man.Tabs) != 0 {
		t.Errorf("harbor should be appearance-only: %+v", man)
	}
	ref := pluginThemeRef{Plugin: "harbor", ID: man.Themes[0].ID, PluginDir: src, Dir: man.Themes[0].Dir}
	p, err := ref.palette()
	if err != nil || p.light {
		t.Fatalf("harbor-night palette: %v (light=%v)", err, p.light)
	}
	def := p.themeDef()
	if r := contrastRatio(def.ui.Text, def.ui.PanelBg); r < 7 {
		t.Errorf("harbor-night body text contrast %.2f:1; want a comfortable 7:1", r)
	}
	if r := contrastRatio(def.ui.Subtext0, def.ui.PanelBg); r < 4.5 {
		t.Errorf("harbor-night subtext contrast %.2f:1", r)
	}
}
