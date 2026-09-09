package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestOmarchySidebarTextContrast(t *testing.T) {
	p := omarchyPalette{light: true, c: map[string]string{
		"background": "#f8f9fa", "foreground": "#5c6166",
		"bright_foreground": "#d1d1d1", "dark_foreground": "#8a9199",
		"muted": "#686868",
	}}
	def := p.themeDef()
	for _, token := range def.customTokens() {
		switch token.key {
		case "text", "subtext0", "overlay0", "overlay1":
			if ratio := contrastRatio(token.hex, p.at("background")); ratio < 4.5 {
				t.Errorf("%s contrast = %.2f, want at least 4.5", token.key, ratio)
			}
		}
	}
	if def.ansi.BrightWhite != p.at("bright_foreground") {
		t.Fatal("sidebar repair must not change the terminal ANSI palette")
	}
	if def.ui.Text != p.at("foreground") {
		t.Fatal("already readable primary text must remain unchanged")
	}
}

func TestOmarchyCatalogAndDerivedPalettes(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	reloadOmarchyThemes()

	cat := themeCatalog()
	byName := map[string]themeCatalogEntry{}
	for _, e := range cat {
		byName[e.Name] = e
	}
	t.Logf("catalog: %d entries", len(cat))
	for _, want := range []string{"retro-82", "osaka-jade", "everforest", "matte-black", "vantablack", "white", "catppuccin"} {
		e, ok := byName[want]
		if !ok {
			t.Fatalf("catalog missing %q", want)
		}
		t.Logf("%-14s src=%-9s light=%v accent=%s bg=%s images=%d", e.Name, e.Source, e.Light, e.Accent, e.Background, len(e.Backgrounds))
		// Every official theme is complete offline: the wallpapers ship in the
		// binary, so this holds on a machine that has never run Omarchy.
		if len(e.Backgrounds) == 0 {
			t.Errorf("%s: no wallpapers", e.Name)
		}
		if len(e.Thumbs) != len(e.Backgrounds) {
			t.Errorf("%s: %d thumbs for %d backgrounds", e.Name, len(e.Thumbs), len(e.Backgrounds))
		}
	}
	if byName["retro-82"].Source != "official" || byName["catppuccin"].Source != "builtin" {
		t.Errorf("catalog must distinguish Omarchy Retro 82 from native Herdr Catppuccin")
	}
	if byName["osaka-jade"].Source != "official" {
		t.Errorf("osaka-jade source = %q", byName["osaka-jade"].Source)
	}

	// The embedded image and its thumbnail both serve, and the thumbnail is the
	// small one — that is the whole reason it exists.
	full := byName["osaka-jade"].Backgrounds[0]
	thumb := byName["osaka-jade"].Thumbs[0]
	if !strings.HasPrefix(thumb, omarchyThumbPrefix) {
		t.Fatalf("no vendored thumbnail: %s", thumb)
	}
	recFull := httptest.NewRecorder()
	serveOmarchyBackground(recFull, httptest.NewRequest(http.MethodGet, full, nil))
	recThumb := httptest.NewRecorder()
	serveOmarchyBackground(recThumb, httptest.NewRequest(http.MethodGet, thumb, nil))
	t.Logf("%s -> %d (%d bytes); %s -> %d (%d bytes)",
		full, recFull.Code, recFull.Body.Len(), thumb, recThumb.Code, recThumb.Body.Len())
	if recFull.Code != 200 || recFull.Body.Len() < 10000 {
		t.Errorf("vendored wallpaper: %d (%d bytes)", recFull.Code, recFull.Body.Len())
	}
	if recThumb.Code != 200 || recThumb.Body.Len() == 0 || recThumb.Body.Len() >= recFull.Body.Len() {
		t.Errorf("thumbnail: %d (%d bytes)", recThumb.Code, recThumb.Body.Len())
	}

	// Every registered theme must produce a complete palette and a herdr base.
	omarchyMu.RLock()
	defer omarchyMu.RUnlock()
	for _, n := range omarchyNames {
		d := omarchyByName[n].def
		if d.herdrBase == "" {
			t.Errorf("%s: no herdr base", n)
		}
		for _, color := range []string{d.ui.Text, d.ui.Subtext0, d.ui.Overlay0, d.ui.Overlay1} {
			if contrastRatio(color, d.ui.PanelBg) < 4.5 {
				t.Errorf("%s: sidebar text %s lacks readable contrast", n, color)
			}
		}
		for tok, v := range map[string]string{
			"accent": d.ui.Accent, "panel_bg": d.ui.PanelBg, "text": d.ui.Text,
			"surface0": d.ui.Surface0, "surface1": d.ui.Surface1, "overlay1": d.ui.Overlay1,
			"peach": d.ui.Peach, "ansi.brightblue": d.ansi.BrightBlue,
		} {
			if len(v) != 7 || v[0] != '#' {
				t.Errorf("%s: %s = %q", n, tok, v)
			}
		}
		rt := resolveThemeByName(n)
		if rt.Resolved != n || !strings.Contains(rt.xtermJSON(), d.ui.PanelBg) {
			t.Errorf("%s: resolveThemeByName -> %q", n, rt.Resolved)
		}
		if spec := themeSpecFor(n); spec.lasso != n || len(spec.tokens) == 0 {
			t.Errorf("%s: config.toml spec = %+v", n, spec)
		}
	}
}

func TestOmarchyInstallLegacyThemeFromGit(t *testing.T) {
	if os.Getenv("LASSO_NET_TEST") == "" {
		t.Skip("set LASSO_NET_TEST=1 to clone from github")
	}
	openTestDB(t) // LASSO_DIR -> temp; the db is where provenance is recorded
	dir := os.Getenv("LASSO_DIR")
	reloadOmarchyThemes()

	name, err := installOmarchyTheme(context.Background(), "https://github.com/fdidron/omarchy-ayu-light-theme")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if name != "ayu-light" {
		t.Fatalf("installed as %q", name)
	}
	root := filepath.Join(dir, "omarchy", "themes", name)
	ents, _ := os.ReadDir(root)
	var kept []string
	for _, e := range ents {
		kept = append(kept, e.Name())
	}
	t.Logf("kept: %v", kept)
	for _, gone := range []string{".git", "alacritty.toml", "neovim.lua", "ghostty.conf", "README.md", "walker.css"} {
		if _, err := os.Stat(filepath.Join(root, gone)); err == nil {
			t.Errorf("%s survived the install", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "colors.toml")); err != nil {
		t.Fatalf("no colors.toml after install: %v", err)
	}

	def, ok := lookupThemeDef("ayu-light")
	if !ok {
		t.Fatal("installed theme does not resolve")
	}
	t.Logf("ayu-light: accent=%s bg=%s text=%s base=%s", def.ui.Accent, def.ui.PanelBg, def.ui.Text, def.herdrBase)
	if def.ui.PanelBg != "#f8f9fa" || def.ui.Text != "#5c6166" {
		t.Errorf("legacy palette misread: bg=%s text=%s", def.ui.PanelBg, def.ui.Text)
	}
	if def.herdrBase != omarchyLightBase {
		t.Errorf("light theme got base %q", def.herdrBase)
	}

	// Backgrounds are listed and served.
	urls := omarchyBackgroundURLs("ayu-light")
	t.Logf("backgrounds: %v", urls)
	if len(urls) != 3 {
		t.Fatalf("want the repo's 3 wallpapers, got %v", urls)
	}
	rec := httptest.NewRecorder()
	serveOmarchyBackground(rec, httptest.NewRequest(http.MethodGet, urls[0], nil))
	if rec.Code != 200 || rec.Body.Len() < 1000 {
		t.Errorf("serve %s: %d (%d bytes)", urls[0], rec.Code, rec.Body.Len())
	}
	for _, bad := range []string{"/omarchy/bg/ayu-light/..%2fcolors.toml", "/omarchy/bg/ayu-light/colors.toml", "/omarchy/bg/../../etc/passwd"} {
		rec := httptest.NewRecorder()
		serveOmarchyBackground(rec, httptest.NewRequest(http.MethodGet, bad, nil))
		if rec.Code != 404 {
			t.Errorf("%s: %d, want 404", bad, rec.Code)
		}
	}

	// The catalog reports it — including the provenance a restart reads back.
	reloadOmarchyThemes() // what a restart does: disk + the recorded URL
	rec = httptest.NewRecorder()
	serveOmarchyThemes(rec, httptest.NewRequest(http.MethodGet, "/api/omarchy-themes", nil))
	var got struct {
		Themes []themeCatalogEntry `json:"themes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range got.Themes {
		if e.Name == "ayu-light" {
			found = true
			t.Logf("catalog row: %+v", e)
			if !e.Installed || e.Source != "installed" || len(e.Backgrounds) != 3 || !e.Light {
				t.Errorf("bad catalog row: %+v", e)
			}
			if e.URL != "https://github.com/fdidron/omarchy-ayu-light-theme" {
				t.Errorf("provenance lost across a reload: %q", e.URL)
			}
		}
	}
	if !found {
		t.Fatal("installed theme missing from the catalog")
	}
	// A re-install of the same theme replaces it rather than failing.
	if _, err := installOmarchyTheme(context.Background(), "https://github.com/fdidron/omarchy-ayu-light-theme.git"); err != nil {
		t.Errorf("re-install: %v", err)
	}
}

func TestOmarchyInstallRefusesUnsafeSources(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	for _, bad := range []string{
		"", "file:///etc", "ext::sh -c whoami", "git@github.com:x/y.git",
		"https://user:pw@github.com/x/y", "http://github.com/x/y", "https://localhost/x",
	} {
		if _, err := installOmarchyTheme(context.Background(), bad); err == nil {
			t.Errorf("%q was accepted", bad)
		} else {
			t.Logf("%-32q -> %v", bad, err)
		}
	}
	if _, err := installOmarchyTheme(context.Background(), "https://github.com/x/omarchy-nord-theme"); err == nil {
		t.Error("a built-in name was accepted")
	}
}

// The registry is read by every theme resolution — the hub poll, each host
// feed, each request — while an install rebuilds it. Under -race this fails on
// any attempt to add runtime themes by mutating a shared map in place.
func TestOmarchyRegistryIsSafeUnderConcurrentReload(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	reloadOmarchyThemes()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, ok := lookupThemeDef("osaka-jade"); !ok {
					t.Error("osaka-jade vanished from the registry")
					return
				}
				_ = themeCatalog()
				_ = themeOptionsAll()
				_ = resolveThemeByName("everforest")
			}
		}()
	}
	for range 20 {
		reloadOmarchyThemes()
	}
	close(stop)
	wg.Wait()
}

func TestThemePreviewAndSelectOmarchyTheme(t *testing.T) {
	openTestDB(t)
	t.Setenv("HERDR_CONFIG_PATH", filepath.Join(t.TempDir(), "config.toml"))
	// Nothing on this machine may be written by this test. The theme fan-out
	// serveThemeSet kicks off is a goroutine that can outlive the test (and its
	// db, whose absence reads as "sync everything"), so HOME is redirected as
	// well as the settings flipped: a stray write then lands in the temp tree.
	t.Setenv("HOME", t.TempDir())
	reloadOmarchyThemes()
	if err := setSetting(syncAgentThemesKey, "false"); err != nil {
		t.Fatal(err)
	}
	if err := setThemeSyncFor("local", false); err != nil {
		t.Fatal(err)
	}
	prevHub := srvHub
	srvHub = newHub()
	t.Cleanup(func() { srvHub = prevHub })

	rec := httptest.NewRecorder()
	serveTheme(rec, httptest.NewRequest(http.MethodGet, "/api/theme?name=osaka-jade", nil))
	var p themePayload
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if p.Resolved != "osaka-jade" || !strings.Contains(p.CSS, "--bg:") {
		t.Errorf("preview payload: resolved=%q css=%.60s", p.Resolved, p.CSS)
	}
	t.Logf("preview xterm: %s", p.Xterm)
	names := map[string]bool{}
	for _, o := range p.Themes {
		names[o.Name] = true
	}
	if !names["osaka-jade"] || !names["retro-82"] {
		t.Errorf("themes[] = %d entries, missing omarchy or built-ins", len(p.Themes))
	}

	rec = httptest.NewRecorder()
	serveTheme(rec, httptest.NewRequest(http.MethodGet, "/api/theme?name=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown preview: %d", rec.Code)
	}

	// Selecting an omarchy theme writes herdr's config as base + override block.
	rec = httptest.NewRecorder()
	serveThemeSet(rec, httptest.NewRequest(http.MethodPost, "/api/theme-set", strings.NewReader(`{"name":"osaka-jade"}`)))
	if rec.Code != 200 {
		t.Fatalf("theme-set: %d %s", rec.Code, rec.Body.String())
	}
	body, _ := os.ReadFile(os.Getenv("HERDR_CONFIG_PATH"))
	t.Logf("config.toml:\n%s", body)
	if !strings.Contains(string(body), omarchyDarkBase) || !strings.Contains(string(body), `lasso-theme = "osaka-jade"`) {
		t.Errorf("config.toml does not express the theme")
	}
	if rt := loadHerdrTheme("auto"); rt.Resolved != "osaka-jade" {
		t.Errorf("reload resolved %q", rt.Resolved)
	}
}
