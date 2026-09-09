// Omarchy themes: every official Omarchy palette, offline, plus any community
// theme installed from a git URL — resolved into the same resolvedTheme the
// rest of lasso already paints from (theme.go).
//
// Omarchy's own theme format is a single semantic palette file, colors.toml
// (see basecamp/omarchy docs/theming.md), which it renders every app's config
// from via default/themed/*.tpl. A theme predating that file ships only the
// generated ones, of which alacritty.toml is the palette of record — the same
// pair omarchy-theme-set handles (it converts the legacy file through
// omarchy-theme-colors-from-alacritty and stages only the result). Both are
// parsed here, and an installed legacy theme is normalized to a colors.toml on
// disk exactly like Omarchy does, so a reload never re-derives.
//
// Three rules this file exists to keep:
//
//   - A built-in ALWAYS wins. lasso's nineteen palettes (themes) are hand-tuned
//     for its own chrome and are what every existing test pins; an official
//     Omarchy theme of the same name (catppuccin, nord, retro-82, …) contributes
//     its backgrounds and nothing else. So `themes` stays a compile-time
//     constant map and the registry below holds only names it does not have.
//
//   - Nothing from a stranger's repo is executed, and almost none of it is even
//     kept. `git clone` runs with hooks and every non-https transport disabled,
//     and the working tree is then reduced to an ALLOWLIST — the palette plus
//     regular background images — so the *.lua / alacritty.toml / ghostty.conf /
//     vscode.json files Omarchy has to name programs for (its
//     INSTALLED_THEME_DENIED list) simply do not survive the install. A
//     denylist is only correct while it is maintained; there is nothing here
//     lasso needs from a theme besides colours and wallpapers.
//
//   - The registry is read on every theme resolution, from the hub poll, the
//     SSE feeds and each request, while an install rewrites it. It is therefore
//     a map behind an RWMutex that is REPLACED wholesale, never mutated in
//     place — the `themes` map is written once at init and read forever, and
//     adding runtime themes to it would be a data race on every tab.
//
// Every official theme is complete OFFLINE: its palette AND its wallpapers are
// vendored into the binary (~54 MB, byte-for-byte upstream images plus a 320px
// WebP thumbnail each, so a picker grid costs kilobytes a tile). A machine's
// own copy still wins over the vendored one — an install brings its own
// backgrounds, and an Omarchy box's themes dir is the one its user means.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// omarchyOfficialFS holds the vendored official themes — each one's
// colors.toml, its backgrounds/ and the thumbs/ generated from them,
// byte-for-byte from basecamp/omarchy (provenance in assets/omarchy/NOTICE.txt).
//
//go:embed assets/omarchy/themes
var omarchyOfficialFS embed.FS

const (
	omarchyAssetRoot = "assets/omarchy/themes"

	// omarchyBGPrefix is the URL space backgrounds are served from, and
	// omarchyThumbPrefix their 320px previews. Root relative, so the same
	// string resolves from the app page AND from a ttyd document served under
	// /terminal/<slug>/.
	omarchyBGPrefix    = "/omarchy/bg/"
	omarchyThumbPrefix = "/omarchy/thumb/"

	// Install bounds. A theme repo is colours and a handful of wallpapers; a
	// clone that wants more than this is not one.
	omarchyCloneTimeout   = 3 * time.Minute
	omarchyMaxBackground  = 32 << 20 // per image
	omarchyMaxThemeBytes  = 192 << 20
	omarchyMaxBackgrounds = 64

	// omarchyInstalledKey records what was installed and from where. The
	// directory is what makes an install survive a restart; this is its
	// provenance (and what the catalog reports as `url`).
	omarchyInstalledKey = "omarchy_installed"

	// herdr bases for a theme herdr has no name for: every token is overridden
	// by the generated [theme.custom] block (themeSpecFor), so the base only
	// decides what herdr's own Settings list highlights — but it must match the
	// theme's mode, or herdr renders light chrome for an instant on reload.
	omarchyDarkBase  = "vesper"
	omarchyLightBase = "catppuccin-latte"
)

// omarchyTheme is one theme this build can resolve that `themes` does not hold.
type omarchyTheme struct {
	Name   string // canonical lasso key
	Label  string
	Light  bool
	Source string // "official" (vendored) or "installed" (git URL)
	URL    string // git origin, installed themes only
	def    themeDef
}

var (
	omarchyMu     sync.RWMutex
	omarchyByName = map[string]omarchyTheme{}
	omarchyNames  []string // display order: official first, then installed
	// omarchyOfficial is every vendored official name, INCLUDING the ones a
	// built-in shadows: where a theme came from is a property of Omarchy's set,
	// not of which map ends up holding the palette.
	omarchyOfficial = map[string]bool{}
	omarchyLoadOnce sync.Once
)

// omarchyLoaded resolves the registry once, lazily: the CLI subcommands and
// most tests never touch a theme, and the scan reads the install directory.
func omarchyLoaded() {
	omarchyLoadOnce.Do(func() { reloadOmarchyThemes() })
}

// reloadOmarchyThemes rebuilds the registry from the embedded palettes and the
// install directory, then swaps it in. Called at boot (via omarchyLoaded) and
// after an install.
func reloadOmarchyThemes() {
	byName := map[string]omarchyTheme{}
	vendored := map[string]bool{}
	var official, installed []string

	urls := installedThemeURLs()

	add := func(name string, t omarchyTheme, list *[]string) {
		if _, builtin := themes[name]; builtin {
			return // a built-in always wins; the catalog still lists its backgrounds
		}
		if _, dup := byName[name]; dup {
			return
		}
		byName[name] = t
		*list = append(*list, name)
	}

	ents, err := omarchyOfficialFS.ReadDir(omarchyAssetRoot)
	if err != nil {
		log.Printf("omarchy: vendored themes unreadable: %v", err)
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		body, err := omarchyOfficialFS.ReadFile(omarchyAssetRoot + "/" + e.Name() + "/colors.toml")
		if err != nil {
			log.Printf("omarchy: vendored %s: %v", e.Name(), err)
			continue
		}
		p, err := paletteFromColorsTOML(string(body))
		if err != nil {
			log.Printf("omarchy: vendored %s: %v", e.Name(), err)
			continue
		}
		name := normalizeThemeName(e.Name())
		// Recorded even when a built-in shadows it below: this is the set of
		// names Omarchy itself ships, which is what says where a theme came
		// from (see themeCatalog).
		vendored[name] = true
		add(name, omarchyTheme{
			Name:   name,
			Label:  omarchyLabel(name),
			Light:  p.light,
			Source: "official",
			def:    p.themeDef(),
		}, &official)
	}

	for _, dir := range installedThemeDirs() {
		name := normalizeThemeName(filepath.Base(dir))
		p, err := readThemePalette(dir)
		if err != nil {
			log.Printf("omarchy: installed %s: %v", name, err)
			continue
		}
		add(name, omarchyTheme{
			Name:   name,
			Label:  omarchyLabel(name),
			Light:  p.light,
			Source: "installed",
			URL:    urls[name],
			def:    p.themeDef(),
		}, &installed)
	}

	sort.Strings(official)
	sort.Strings(installed)

	omarchyMu.Lock()
	omarchyByName, omarchyNames, omarchyOfficial = byName, append(official, installed...), vendored
	omarchyMu.Unlock()
}

// lookupThemeDef resolves a canonical theme key to its palette: lasso's
// built-ins first, then the Omarchy registry. Every theme resolution goes
// through here (theme.go, agentsync.go), which is what makes an installed
// theme a first-class one — it paints the chrome, the terminal, herdr's own
// config.toml and every agent CLI's theme file, like any other.
func lookupThemeDef(key string) (themeDef, bool) {
	if def, ok := themes[key]; ok {
		return def, true
	}
	omarchyLoaded()
	omarchyMu.RLock()
	defer omarchyMu.RUnlock()
	t, ok := omarchyByName[key]
	return t.def, ok
}

// themeResolvable reports whether this build has a palette for a canonical key
// — the question that decides what lasso may REWRITE, never what it may keep:
// a selection whose theme only a newer binary (or an install since removed)
// knows is still that human's selection (see lassoTokenTag in theme.go).
func themeResolvable(key string) bool {
	_, ok := lookupThemeDef(key)
	return ok
}

// logOmarchyThemes resolves the registry and says what it found, once, at boot.
func logOmarchyThemes() {
	omarchyLoaded()
	omarchyMu.RLock()
	defer omarchyMu.RUnlock()
	official, installed := 0, []string{}
	for _, n := range omarchyNames {
		if omarchyByName[n].Source == "installed" {
			installed = append(installed, n)
			continue
		}
		official++
	}
	if len(installed) == 0 {
		log.Printf("themes:   %d built-in + %d omarchy", len(themeOptions), official)
		return
	}
	log.Printf("themes:   %d built-in + %d omarchy + %d installed (%s)",
		len(themeOptions), official, len(installed), strings.Join(installed, ", "))
}

// themeOptionsAll is every selectable theme: the built-ins in their curated
// order, then the Omarchy ones. Served as /api/theme's `themes`, so the
// existing Settings dropdown offers installs with no client-side merge.
func themeOptionsAll() []themeOption {
	omarchyLoaded()
	omarchyMu.RLock()
	defer omarchyMu.RUnlock()
	out := make([]themeOption, 0, len(themeOptions)+len(omarchyNames))
	out = append(out, themeOptions...)
	for _, n := range omarchyNames {
		t := omarchyByName[n]
		out = append(out, themeOption{Name: t.Name, Label: t.Label, Light: t.Light})
	}
	return out
}

// ---------------------------------------------------------------------------
// Catalog (GET /api/omarchy-themes)
// ---------------------------------------------------------------------------

// themeCatalogEntry is one row of the Themes settings subtab: everything a
// theme picker needs in one call — including the built-ins, so the subtab has
// a single source and a built-in that also exists as an Omarchy theme still
// offers that theme's wallpapers.
type themeCatalogEntry struct {
	Name       string `json:"name"`
	Label      string `json:"label"`
	Light      bool   `json:"light"`
	Source     string `json:"source"` // builtin | official | installed (see builtinSource)
	Installed  bool   `json:"installed"`
	URL        string `json:"url,omitempty"`
	Accent     string `json:"accent"`
	Background string `json:"background"`
	// Backgrounds are root-relative URLs of this theme's wallpapers: the ones
	// vendored with the binary, plus any this machine has of its own (an
	// install, or an Omarchy box's themes dir, which shadow a vendored file of
	// the same name). Never nil.
	Backgrounds []string `json:"backgrounds"`
	// Thumbs runs parallel to Backgrounds — same length, same order — carrying
	// the 320px preview where one is vendored and the full image otherwise, so
	// a picker grid loads ~20 KB a tile instead of ~1 MB.
	Thumbs []string `json:"thumbs"`
}

func themeCatalog() []themeCatalogEntry {
	omarchyLoaded()
	omarchyMu.RLock()
	defer omarchyMu.RUnlock()
	out := make([]themeCatalogEntry, 0, len(themeOptions)+8)
	for _, o := range themeOptions {
		def := themes[o.Name]
		out = append(out, themeCatalogEntry{
			Name: o.Name, Label: o.Label, Light: o.Light, Source: builtinSource(o.Name, def),
			Accent: def.ui.Accent, Background: def.ui.PanelBg,
			Backgrounds: omarchyBackgroundURLs(o.Name), Thumbs: omarchyThumbURLs(o.Name),
		})
	}
	for _, n := range omarchyNames {
		t := omarchyByName[n]
		out = append(out, themeCatalogEntry{
			Name: t.Name, Label: t.Label, Light: t.Light, Source: t.Source,
			Installed: t.Source == "installed", URL: t.URL,
			Accent: t.def.ui.Accent, Background: t.def.ui.PanelBg,
			Backgrounds: omarchyBackgroundURLs(t.Name), Thumbs: omarchyThumbURLs(t.Name),
		})
	}
	return out
}

// builtinSource is where one of lasso's own nineteen palettes came from, which
// is not the same question as which map holds it.
//
// herdr's closed set of theme names is the evidence: a key herdr accepts on
// [theme].name (herdrBase == "") is a palette herdr ships, and stays "builtin"
// even when Omarchy vendors a theme of the same name (catppuccin, nord,
// gruvbox, …) — the palette lasso, herdr and every agent actually paint is
// herdr's, so calling it Omarchy's would be wrong in the one direction that
// matters. A key herdr REJECTS exists here only because Omarchy has it, so a
// name vendored under assets/omarchy/themes is reported as the official
// Omarchy theme it is. Retro 82 is the only one today.
//
// Callers hold omarchyMu.
func builtinSource(name string, def themeDef) string {
	if def.herdrBase != "" && omarchyOfficial[name] {
		return "official"
	}
	return "builtin"
}

// ---------------------------------------------------------------------------
// Backgrounds
// ---------------------------------------------------------------------------

var omarchyImageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".avif": true,
}

// omarchyInstallRoot is where lasso keeps the themes it installed.
func omarchyInstallRoot() string { return filepath.Join(lassoDir(), "omarchy", "themes") }

// omarchyBackgroundRoots lists, in precedence order, the directories that may
// hold this theme's wallpapers: lasso's own install first, then the three
// places Omarchy itself keeps them on a machine running it (a user overlay, the
// user themes dir, the system themes dir). Searched in the same order when
// listing and when serving, so a name always resolves to the file it named.
func omarchyBackgroundRoots(name string) []string {
	if !validOmarchyName(name) {
		return nil
	}
	home, _ := os.UserHomeDir()
	roots := []string{filepath.Join(omarchyInstallRoot(), name, "backgrounds")}
	if home != "" {
		roots = append(roots,
			filepath.Join(home, ".config", "omarchy", "backgrounds", name),
			filepath.Join(home, ".config", "omarchy", "themes", name, "backgrounds"),
			filepath.Join(home, ".local", "share", "omarchy", "themes", name, "backgrounds"),
		)
	}
	return append(roots, filepath.Join("/usr/share/omarchy/themes", name, "backgrounds"))
}

// omarchyBackgroundFiles is the theme's wallpaper FILE NAMES, sorted, deduped
// across every source (a machine's own copy shadows the vendored one of the
// same name). Second return value: whether each name has a vendored thumbnail.
func omarchyBackgroundFiles(name string) []string {
	if !validOmarchyName(name) {
		return nil
	}
	seen := map[string]bool{}
	var files []string
	keep := func(n string, isDir bool) {
		if isDir || seen[n] || len(files) >= omarchyMaxBackgrounds {
			return
		}
		if !omarchyImageExts[strings.ToLower(filepath.Ext(n))] {
			return
		}
		seen[n] = true
		files = append(files, n)
	}
	for _, root := range omarchyBackgroundRoots(name) {
		ents, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range ents {
			keep(e.Name(), e.IsDir())
		}
	}
	for _, e := range embeddedThemeDir(name, "backgrounds") {
		keep(e.Name(), e.IsDir())
	}
	sort.Strings(files)
	return files
}

// omarchyBackgroundURLs is the theme's wallpapers as root-relative URLs. Never
// nil, so a client can index it beside the thumbnails without a nil check.
func omarchyBackgroundURLs(name string) []string {
	files := omarchyBackgroundFiles(name)
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, omarchyBGPrefix+url.PathEscape(name)+"/"+url.PathEscape(f))
	}
	return out
}

// omarchyThumbURLs runs parallel to omarchyBackgroundURLs: the 320px vendored
// thumbnail where there is one, else the full image, so a picker grid can use
// the array positionally and never has to decide. A theme's own copy on disk
// (an install, an Omarchy box) has no thumbnail — vendoring one for a file this
// build has never seen is not possible — but a vendored thumb of the SAME name
// still stands in for it, which is exactly the Omarchy-box case.
func omarchyThumbURLs(name string) []string {
	files := omarchyBackgroundFiles(name)
	out := make([]string, 0, len(files))
	for _, f := range files {
		if _, ok := embeddedThumbPath(name, f); ok {
			out = append(out, omarchyThumbPrefix+url.PathEscape(name)+"/"+url.PathEscape(f))
			continue
		}
		out = append(out, omarchyBGPrefix+url.PathEscape(name)+"/"+url.PathEscape(f))
	}
	return out
}

// embeddedThemeDir lists one subdirectory of a vendored theme.
func embeddedThemeDir(name, sub string) []fs.DirEntry {
	if !validOmarchyName(name) {
		return nil
	}
	ents, err := omarchyOfficialFS.ReadDir(omarchyAssetRoot + "/" + name + "/" + sub)
	if err != nil {
		return nil
	}
	return ents
}

// embeddedBackground returns a vendored wallpaper's bytes.
func embeddedBackground(name, file string) ([]byte, bool) {
	if !validOmarchyName(name) || !validBackgroundFile(file) {
		return nil, false
	}
	b, err := omarchyOfficialFS.ReadFile(omarchyAssetRoot + "/" + name + "/backgrounds/" + file)
	return b, err == nil
}

// embeddedThumbPath is the embedded path of the 320px thumbnail vendored for a
// wallpaper, if there is one. Thumbs are WebP whatever the original is, so the
// file is the stem plus .webp — the URL still names the ORIGINAL image, since
// that is the identity a client holds. Existence is a Stat, not a read: the
// catalog asks for all 92 of them on every call.
func embeddedThumbPath(name, file string) (string, bool) {
	if !validOmarchyName(name) || !validBackgroundFile(file) {
		return "", false
	}
	stem := strings.TrimSuffix(file, filepath.Ext(file))
	p := omarchyAssetRoot + "/" + name + "/thumbs/" + stem + ".webp"
	if _, err := fs.Stat(omarchyOfficialFS, p); err != nil {
		return "", false
	}
	return p, true
}

// embeddedThumb returns that thumbnail's bytes.
func embeddedThumb(name, file string) ([]byte, bool) {
	p, ok := embeddedThumbPath(name, file)
	if !ok {
		return nil, false
	}
	b, err := omarchyOfficialFS.ReadFile(p)
	return b, err == nil
}

// omarchyBackgroundPath resolves one wallpaper to a readable regular file.
func omarchyBackgroundPath(name, file string) (string, bool) {
	if !validOmarchyName(name) || !validBackgroundFile(file) {
		return "", false
	}
	for _, root := range omarchyBackgroundRoots(name) {
		p := filepath.Join(root, file)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, true
		}
	}
	return "", false
}

// validBackgroundFile accepts one path element with an image extension — no
// separators, no dot segments, so the join below cannot escape the root.
func validBackgroundFile(f string) bool {
	if f == "" || f == "." || f == ".." || len(f) > 128 {
		return false
	}
	if strings.ContainsAny(f, "/\\") || strings.Contains(f, "..") {
		return false
	}
	return omarchyImageExts[strings.ToLower(filepath.Ext(f))]
}

// validOmarchyName is the theme-name grammar: lowercase, no separators, so a
// name is always exactly one directory under a root.
func validOmarchyName(n string) bool {
	if n == "" || len(n) > 64 || n == "." || n == ".." {
		return false
	}
	for _, c := range n {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// serveOmarchyBackground serves /omarchy/bg/<theme>/<file> (the full wallpaper)
// and /omarchy/thumb/<theme>/<file> (its 320px vendored preview, named after
// the ORIGINAL file). Behind the same auth as the rest of the app; both URLs
// are root-relative, so a ttyd document can use them verbatim.
//
// A copy ON THIS MACHINE wins over the vendored one of the same name — an
// Omarchy box's own themes dir, or a background the user dropped into their
// overlay, is the one they mean.
func serveOmarchyBackground(w http.ResponseWriter, r *http.Request) {
	thumb := strings.HasPrefix(r.URL.Path, omarchyThumbPrefix)
	prefix := omarchyBGPrefix
	if thumb {
		prefix = omarchyThumbPrefix
	}
	theme, file, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	theme, err1 := url.PathUnescape(theme)
	file, err2 := url.PathUnescape(file)
	if err1 != nil || err2 != nil {
		http.NotFound(w, r)
		return
	}
	// Immutable in practice: the vendored set changes only with the binary, and
	// an install writes its directory once. A wallpaper re-fetched on every
	// repaint would flash the chrome on each theme change.
	w.Header().Set("Cache-Control", "public, max-age=86400")

	if thumb {
		body, ok := embeddedThumb(theme, file)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/webp")
		http.ServeContent(w, r, "thumb.webp", time.Time{}, bytes.NewReader(body))
		return
	}
	if path, ok := omarchyBackgroundPath(theme, file); ok {
		f, err := os.Open(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
		return
	}
	body, ok := embeddedBackground(theme, file)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(file))); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, file, time.Time{}, bytes.NewReader(body))
}

// ---------------------------------------------------------------------------
// Palette parsing: colors.toml (current) and alacritty.toml (legacy)
// ---------------------------------------------------------------------------

// omarchyPalette is one theme's semantic palette — Omarchy's colors.toml keys,
// with every default already filled in (see fillOmarchyDefaults).
type omarchyPalette struct {
	light bool
	c     map[string]string
}

func (p omarchyPalette) at(k string) string { return p.c[k] }

// tomlColorTable parses the flat "<key> = <value>" pairs of a TOML file into
// dotted paths ([colors.normal] black -> colors.normal.black), first occurrence
// winning. Deliberately not a TOML library: two shapes of colour table is the
// whole requirement, and theme.go already reads herdr's config the same way.
func tomlColorTable(text string) map[string]string {
	out := map[string]string{}
	section := ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if s, ok := tomlSection(line); ok {
				section = s
			} else {
				section = ""
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		if section != "" {
			key = section + "." + key
		}
		if _, dup := out[key]; !dup {
			out[key] = unquote(strings.TrimSpace(v))
		}
	}
	return out
}

// omarchyHex accepts the colour spellings Omarchy's own parser does — "#rgb",
// "#rrggbb", "0xrrggbb", bare "rrggbb" — and returns "#rrggbb".
func omarchyHex(v string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(unquote(strings.TrimSpace(v))))
	s = strings.TrimPrefix(s, "0x")
	if !strings.HasPrefix(s, "#") {
		s = "#" + s
	}
	if len(s) != 4 && len(s) != 7 {
		return "", false
	}
	for _, c := range s[1:] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return "", false
		}
	}
	return normalizeHex(s)
}

// paletteFromColorsTOML reads a theme's colors.toml.
func paletteFromColorsTOML(text string) (omarchyPalette, error) {
	tbl := tomlColorTable(text)
	c := map[string]string{}
	for k, v := range tbl {
		if strings.Contains(k, ".") { // colors.toml is flat; a table is not a palette key
			continue
		}
		if hex, ok := omarchyHex(v); ok {
			c[k] = hex
		}
	}
	p := omarchyPalette{c: c, light: strings.EqualFold(strings.TrimSpace(unquote(tbl["mode"])), "light")}
	if !fillOmarchyDefaults(p.c) {
		return omarchyPalette{}, errors.New("colors.toml has no usable palette (needs background/foreground and the named colours)")
	}
	if _, hasMode := tbl["mode"]; !hasMode {
		p.light = isLightHex(p.c["background"])
	}
	return p, nil
}

// paletteFromAlacritty converts a pre-colors.toml theme, mirroring Omarchy's
// own omarchy-theme-colors-from-alacritty: the eight normal colours are
// required, bright ones fall back to their normal, black/white are pinned to
// background/foreground, and the accent is blue.
func paletteFromAlacritty(text string) (omarchyPalette, error) {
	tbl := tomlColorTable(text)
	get := func(k string) string {
		if hex, ok := omarchyHex(tbl[k]); ok {
			return hex
		}
		return ""
	}
	names := []string{"black", "red", "green", "yellow", "blue", "magenta", "cyan", "white"}
	c := map[string]string{}
	for i, n := range names {
		v := get("colors.normal." + n)
		if v == "" {
			return omarchyPalette{}, fmt.Errorf("alacritty.toml is missing [colors.normal].%s", n)
		}
		c["color"+strconv.Itoa(i)] = v
	}
	for i, n := range names {
		v := get("colors.bright." + n)
		if v == "" {
			v = c["color"+strconv.Itoa(i)]
		}
		c["color"+strconv.Itoa(i+8)] = v
	}
	bg, fg := get("colors.primary.background"), get("colors.primary.foreground")
	if bg == "" {
		bg = c["color0"]
	}
	if fg == "" {
		fg = c["color7"]
	}
	c["background"], c["foreground"] = bg, fg
	c["color0"], c["color7"] = bg, fg
	if sel := get("colors.selection.background"); sel != "" {
		c["selection"] = sel
	} else {
		c["selection"] = fg
	}
	c["accent"] = c["color4"]
	if !fillOmarchyDefaults(c) {
		return omarchyPalette{}, errors.New("alacritty.toml has no usable palette")
	}
	return omarchyPalette{c: c, light: isLightHex(bg)}, nil
}

// fillOmarchyDefaults derives every palette key a template may read from the
// ones a theme actually wrote — the same order and the same mixes as Omarchy's
// bin/omarchy-theme-color, so a theme looks here the way it looks there.
// Reports whether the result is a usable palette.
func fillOmarchyDefaults(c map[string]string) bool {
	// Legacy ANSI names -> semantic ones.
	alias := func(dst, src string) {
		if c[dst] == "" && c[src] != "" {
			c[dst] = c[src]
		}
	}
	if c["background"] == "" {
		alias("background", "color0")
	}
	if c["foreground"] == "" {
		alias("foreground", "color7")
	}
	if c["background"] == "" || c["foreground"] == "" {
		return false
	}
	c["color0"], c["color7"] = c["background"], c["foreground"]

	for _, pair := range [][2]string{
		{"red", "color1"}, {"green", "color2"}, {"yellow", "color3"}, {"blue", "color4"},
		{"magenta", "color5"}, {"cyan", "color6"},
		{"bright_red", "color9"}, {"bright_green", "color10"}, {"bright_yellow", "color11"},
		{"bright_blue", "color12"}, {"bright_magenta", "color13"}, {"bright_cyan", "color14"},
	} {
		alias(pair[0], pair[1])
	}
	alias("magenta", "purple")
	alias("bright_magenta", "bright_purple")

	alias("light_foreground", "color7")
	alias("light_foreground", "foreground")
	alias("bright_foreground", "color15")
	alias("bright_foreground", "foreground")
	alias("lighter_background", "color0")
	alias("lighter_background", "background")
	alias("dark_foreground", "color8")
	alias("dark_foreground", "foreground")
	alias("muted", "color8")
	alias("muted", "dark_foreground")
	alias("selection", "selection_background")
	alias("selection", "color8")
	alias("selection", "background")
	alias("selection_background", "selection")
	alias("selection_foreground", "bright_foreground")
	alias("accent", "blue")
	alias("accent", "color4")
	alias("orange", "yellow")

	for _, hue := range []string{"red", "green", "yellow", "blue", "magenta", "cyan"} {
		if c[hue] == "" {
			return false
		}
	}
	if c["brown"] == "" {
		c["brown"] = mixHex(c["orange"], "#000000", 0.5)
	}
	if c["dark_background"] == "" {
		c["dark_background"] = mixHex(c["background"], "#000000", 0.25)
	}
	if c["darker_background"] == "" {
		c["darker_background"] = mixHex(c["background"], "#000000", 0.5)
	}
	for _, hue := range []string{"red", "yellow", "green", "cyan", "blue", "magenta"} {
		if c["bright_"+hue] == "" {
			c["bright_"+hue] = mixHex(c[hue], "#ffffff", 0.2)
		}
	}
	return c["accent"] != ""
}

// Herdr uses the secondary and overlay tokens for sidebar labels, not merely
// decorative terminal gray. Keep readable palette values unchanged; move a
// failing foreground toward whichever endpoint contrasts most with the canvas.
func legibleThemeText(color, bg string) string {
	if contrastRatio(color, bg) >= 4.5 {
		return color
	}
	pole := "#000000"
	if contrastRatio("#ffffff", bg) > contrastRatio(pole, bg) {
		pole = "#ffffff"
	}
	for step := 1; step <= 100; step++ {
		candidate := mixHex(color, pole, float64(step)/100)
		if contrastRatio(candidate, bg) >= 4.5 {
			return candidate
		}
	}
	return pole
}

// themeDef maps an Omarchy palette onto lasso's two palettes.
//
// The ANSI half is Omarchy's own mapping, verbatim from its alacritty/ghostty
// templates (black = background, white = foreground, bright black = muted,
// bright white = bright_foreground) — anything else would render a theme's
// terminal output differently here than under Omarchy itself.
//
// The UI half follows what the hand-written built-ins already do: checked
// against themes["tokyo-night"], whose accent/panel/surface0/surface1/overlay0/
// text/subtext0 are exactly this theme's accent/background/lighter_background/
// muted/dark_foreground/bright_foreground/foreground. overlay1 is the one token
// with no palette key — it is the "slightly brighter than overlay0" tier, so it
// is mixed a quarter of the way to the foreground.
//
// text/subtext0 are the two foregrounds ordered BY CONTRAST rather than by
// name, because `bright_foreground` is not always the more legible of the two.
// A theme that never declared it gets it filled from ANSI bright white (the
// omarchy-theme-color default chain, right for a cursor and wrong for a page:
// Ayu Light would render #d1d1d1 text on #f8f9fa), and two of the official
// themes — last-horizon, solitude — dim it deliberately. Ordering by contrast
// is the same answer as the naming wherever the naming is right (verified
// across all 22 vendored palettes) and a legible one where it isn't.

func (p omarchyPalette) themeDef() themeDef {
	base := omarchyDarkBase
	if p.light {
		base = omarchyLightBase
	}
	bg := p.at("background")
	text, subtext := p.at("bright_foreground"), p.at("foreground")
	if contrastRatio(bg, subtext) > contrastRatio(bg, text) {
		text, subtext = subtext, text
	}
	if text == subtext {
		// One foreground: the label tier has to be derived, or every form
		// label would render at full text weight.
		subtext = mixHex(text, bg, 0.3)
	}
	text = legibleThemeText(text, bg)
	subtext = legibleThemeText(subtext, bg)
	return themeDef{
		ui: uiPalette{
			Accent:     p.at("accent"),
			PanelBg:    bg,
			Surface0:   p.at("lighter_background"),
			Surface1:   p.at("muted"),
			SurfaceDim: p.at("dark_background"),
			Overlay0:   legibleThemeText(p.at("dark_foreground"), bg),
			Overlay1:   legibleThemeText(mixHex(p.at("dark_foreground"), p.at("foreground"), 0.25), bg),
			Text:       text,
			Subtext0:   subtext,
			Mauve:      p.at("magenta"),
			Green:      p.at("green"),
			Yellow:     p.at("yellow"),
			Red:        p.at("red"),
			Blue:       p.at("blue"),
			Teal:       p.at("cyan"),
			Peach:      p.at("orange"),
		},
		ansi: ansiPalette{
			Black: p.at("background"), Red: p.at("red"), Green: p.at("green"),
			Yellow: p.at("yellow"), Blue: p.at("blue"), Magenta: p.at("magenta"),
			Cyan: p.at("cyan"), White: p.at("foreground"),
			BrightBlack: p.at("muted"), BrightRed: p.at("bright_red"),
			BrightGreen: p.at("bright_green"), BrightYellow: p.at("bright_yellow"),
			BrightBlue: p.at("bright_blue"), BrightMagenta: p.at("bright_magenta"),
			BrightCyan: p.at("bright_cyan"), BrightWhite: p.at("bright_foreground"),
		},
		herdrBase: base,
	}
}

// colorsTOML renders the palette back out in Omarchy's own format. An installed
// legacy theme is normalized to this on disk (exactly what omarchy-theme-set
// does with its scratch conversion), so a reload never re-derives and the
// directory says what lasso actually resolved.
func (p omarchyPalette) colorsTOML(srcURL string) []byte {
	mode := "dark"
	if p.light {
		mode = "light"
	}
	var b strings.Builder
	b.WriteString("# Generated by lasso from " + srcURL + "\n")
	b.WriteString("# (converted from the theme's alacritty.toml, the way\n")
	b.WriteString("#  omarchy-theme-colors-from-alacritty does).\n\n")
	fmt.Fprintf(&b, "mode = %q\n\n", mode)
	keys := make([]string, 0, len(p.c))
	for k := range p.c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %q\n", k, p.c[k])
	}
	return []byte(b.String())
}

// readThemePalette reads a theme directory: colors.toml when present, else the
// legacy alacritty.toml. A `light.mode` marker file (Omarchy's own) overrides
// the mode either way.
func readThemePalette(dir string) (omarchyPalette, error) {
	var p omarchyPalette
	body, err := os.ReadFile(filepath.Join(dir, "colors.toml"))
	switch {
	case err == nil:
		if p, err = paletteFromColorsTOML(string(body)); err != nil {
			return omarchyPalette{}, err
		}
	case errors.Is(err, fs.ErrNotExist):
		legacy, lerr := os.ReadFile(filepath.Join(dir, "alacritty.toml"))
		if lerr != nil {
			return omarchyPalette{}, errors.New("no colors.toml and no alacritty.toml — not an Omarchy theme")
		}
		if p, err = paletteFromAlacritty(string(legacy)); err != nil {
			return omarchyPalette{}, err
		}
	default:
		return omarchyPalette{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, "light.mode")); err == nil {
		p.light = true
	}
	return p, nil
}

// isLightHex is the luminance test used when a theme states no mode.
func isLightHex(hex string) bool {
	r, g, b, ok := rgbOf(hex)
	if !ok {
		return false
	}
	return (0.2126*float64(r)+0.7152*float64(g)+0.0722*float64(b))/255 > 0.5
}

// contrastRatio is WCAG 2.x contrast between two hexes (1 … 21). Used to order
// a theme's two foregrounds; an unparseable colour ranks last.
func contrastRatio(a, b string) float64 {
	la, ok1 := relLuminance(a)
	lb, ok2 := relLuminance(b)
	if !ok1 || !ok2 {
		return 0
	}
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func relLuminance(hex string) (float64, bool) {
	r, g, b, ok := rgbOf(hex)
	if !ok {
		return 0, false
	}
	lin := func(v int) float64 {
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b), true
}

func rgbOf(hex string) (int, int, int, bool) {
	h, ok := omarchyHex(hex)
	if !ok {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(h[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return int(v>>16) & 0xff, int(v>>8) & 0xff, int(v) & 0xff, true
}

// mixHex blends pct of b into a (Omarchy's mix_color).
func mixHex(a, b string, pct float64) string {
	ar, ag, ab, ok1 := rgbOf(a)
	br, bg, bb, ok2 := rgbOf(b)
	if !ok1 {
		return b
	}
	if !ok2 {
		return a
	}
	mix := func(x, y int) int { return int(float64(x) + (float64(y)-float64(x))*pct + 0.5) }
	return fmt.Sprintf("#%02x%02x%02x", mix(ar, br), mix(ag, bg), mix(ab, bb))
}

// omarchyLabel turns a theme key into a display label ("osaka-jade" -> "Osaka
// Jade"), with the spellings lasso already uses for the names it shares.
func omarchyLabel(name string) string {
	if l, ok := omarchyLabels[name]; ok {
		return l
	}
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// omarchyLabels are the labels title-casing gets wrong.
var omarchyLabels = map[string]string{
	"retro-82":    "Retro 82",
	"rose-pine":   "Rosé Pine",
	"tokyo-night": "Tokyo Night",
	"osaka-jade":  "Osaka Jade",
	"matte-black": "Matte Black",
}

// ---------------------------------------------------------------------------
// Install
// ---------------------------------------------------------------------------

// themeInstallError carries the HTTP status a failure deserves — a bad URL, a
// taken name and an unreachable remote are three different things to a human
// looking at the toast.
type themeInstallError struct {
	status int
	msg    string
}

func (e themeInstallError) Error() string { return e.msg }

func installErr(status int, format string, a ...any) error {
	return themeInstallError{status: status, msg: fmt.Sprintf(format, a...)}
}

// installedThemeDirs lists the theme directories under the install root.
func installedThemeDirs() []string {
	ents, err := os.ReadDir(omarchyInstallRoot())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || !validOmarchyName(e.Name()) {
			continue
		}
		out = append(out, filepath.Join(omarchyInstallRoot(), e.Name()))
	}
	return out
}

// installedThemeURLs is the recorded provenance, name -> git URL. The DIRECTORY
// is what makes an install survive a restart; this only says where it came
// from, so a db that is closed (tests, CLI) costs the catalog a `url` and
// nothing else.
func installedThemeURLs() map[string]string {
	out := map[string]string{}
	if db == nil {
		return out
	}
	v, err := getSetting(omarchyInstalledKey)
	if err != nil || v == "" {
		return out
	}
	var rec map[string]struct {
		URL string `json:"url"`
	}
	if json.Unmarshal([]byte(v), &rec) != nil {
		return out
	}
	for k, r := range rec {
		out[k] = r.URL
	}
	return out
}

// recordInstalledTheme records where one installed theme came from.
func recordInstalledTheme(name, url string) {
	if db == nil {
		return
	}
	rec := map[string]map[string]string{}
	if v, err := getSetting(omarchyInstalledKey); err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &rec)
	}
	rec[name] = map[string]string{"url": url, "installed_at": time.Now().UTC().Format(time.RFC3339)}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := setSetting(omarchyInstalledKey, string(b)); err != nil {
		log.Printf("omarchy: record install %s: %v", name, err)
	}
}

// themeRepoURL validates a theme's git URL and returns it cleaned. Only public
// https is accepted: a local path, an ssh remote or git's `ext::` transport
// would either run a command or read this machine's own filesystem, and
// embedded credentials would be stored in the catalog.
func themeRepoURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", installErr(http.StatusBadRequest, "no url")
	}
	if len(s) > 512 || strings.ContainsAny(s, " \t\r\n") {
		return "", installErr(http.StatusBadRequest, "not a usable git URL")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", installErr(http.StatusBadRequest, "not a URL: %v", err)
	}
	if u.Scheme != "https" {
		return "", installErr(http.StatusBadRequest, "only https:// theme repositories can be installed (got %q)", u.Scheme)
	}
	if u.User != nil {
		return "", installErr(http.StatusBadRequest, "the URL must not carry credentials")
	}
	if !strings.Contains(u.Host, ".") {
		return "", installErr(http.StatusBadRequest, "%q is not a public host", u.Host)
	}
	if strings.Trim(u.Path, "/") == "" {
		return "", installErr(http.StatusBadRequest, "the URL names no repository")
	}
	u.Fragment = ""
	return u.String(), nil
}

// themeNameFromURL derives the theme key from the repository name, stripping
// the conventional omarchy-<name>-theme wrapping.
func themeNameFromURL(u string) (string, error) {
	base := strings.TrimSuffix(strings.Trim(u, "/"), ".git")
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	name := normalizeThemeName(base)
	name = strings.TrimPrefix(name, "omarchy-")
	name = strings.TrimSuffix(name, "-theme")
	name = strings.TrimSuffix(name, "-themes")
	if !validOmarchyName(name) {
		return "", installErr(http.StatusBadRequest, "cannot derive a theme name from %q", u)
	}
	return name, nil
}

// installOmarchyTheme clones a theme repository and keeps its palette and
// wallpapers. Nothing from the repo is executed, and nothing else from it is
// even kept — see the allowlist in pruneThemeTree.
func installOmarchyTheme(ctx context.Context, rawURL string) (string, error) {
	repo, err := themeRepoURL(rawURL)
	if err != nil {
		return "", err
	}
	name, err := themeNameFromURL(repo)
	if err != nil {
		return "", err
	}
	if _, builtin := themes[name]; builtin {
		return "", installErr(http.StatusConflict, "%q is one of lasso's own themes — rename the repository to install it alongside", name)
	}
	omarchyLoaded()
	omarchyMu.RLock()
	existing, taken := omarchyByName[name]
	omarchyMu.RUnlock()
	if taken && existing.Source == "official" {
		return "", installErr(http.StatusConflict, "%q is an official Omarchy theme lasso already ships", name)
	}

	root := omarchyInstallRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", installErr(http.StatusInternalServerError, "create %s: %v", root, err)
	}
	tmp, err := os.MkdirTemp(root, ".install-")
	if err != nil {
		return "", installErr(http.StatusInternalServerError, "temp dir: %v", err)
	}
	defer os.RemoveAll(tmp)

	work := filepath.Join(tmp, "repo")
	if out, err := gitCloneTheme(ctx, repo, work); err != nil {
		return "", installErr(http.StatusBadGateway, "clone %s: %v%s", repo, err, out)
	}

	p, err := readThemePalette(work)
	if err != nil {
		return "", installErr(http.StatusBadRequest, "%s: %v", repo, err)
	}
	if err := pruneThemeTree(work); err != nil {
		return "", installErr(http.StatusBadRequest, "%v", err)
	}
	// Normalize to colors.toml so a legacy theme is stored the way Omarchy
	// stores it, and so nothing on the reload path has to convert again.
	if err := os.WriteFile(filepath.Join(work, "colors.toml"), p.colorsTOML(repo), 0o644); err != nil {
		return "", installErr(http.StatusInternalServerError, "write colors.toml: %v", err)
	}

	dst := filepath.Join(root, name)
	// A re-install of the same theme replaces it: the old tree is moved aside
	// and dropped only once the new one is in place, so a failed rename never
	// leaves the machine with neither.
	old := ""
	if _, err := os.Stat(dst); err == nil {
		old = filepath.Join(tmp, "old")
		if err := os.Rename(dst, old); err != nil {
			return "", installErr(http.StatusInternalServerError, "replace %s: %v", dst, err)
		}
	}
	if err := os.Rename(work, dst); err != nil {
		if old != "" {
			_ = os.Rename(old, dst)
		}
		return "", installErr(http.StatusInternalServerError, "install %s: %v", dst, err)
	}

	recordInstalledTheme(name, repo)
	reloadOmarchyThemes()
	log.Printf("omarchy: installed theme %q from %s", name, repo)
	return name, nil
}

// gitCloneTheme clones with every code path git offers switched off: no hooks,
// no non-https transport, no credential helper or terminal prompt, no
// submodules, one commit of one branch.
func gitCloneTheme(ctx context.Context, repo, dst string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, omarchyCloneTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git",
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "credential.helper=",
		"-c", "advice.detachedHead=false",
		"clone", "--depth", "1", "--single-branch", "--no-tags",
		"--recurse-submodules=no", "--config", "core.symlinks=false",
		"--", repo, dst)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_CONFIG_COUNT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return ": " + lastLine(msg), err
		}
		return "", err
	}
	return "", nil
}

// pruneThemeTree reduces a cloned repository to what lasso serves: the palette
// files it just read, and regular background images. Everything else — .git,
// the generated per-app configs Omarchy has to name programs for (*.lua,
// alacritty.toml, ghostty.conf, kitty.conf, foot.ini, vscode.json), READMEs,
// previews, and every symlink at any depth — is deleted.
//
// An allowlist rather than Omarchy's denylist because lasso needs strictly less
// from a theme than Omarchy does: colours and wallpapers. Nothing here is ever
// executed, sourced or handed to another program, so anything kept beyond those
// two is a file a stranger chose that lasso has no use for.
func pruneThemeTree(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		switch {
		case e.Name() == "colors.toml" && e.Type().IsRegular():
		case e.Name() == "light.mode" && e.Type().IsRegular():
		case e.Name() == "backgrounds" && e.IsDir():
		default:
			if err := os.RemoveAll(p); err != nil {
				return err
			}
		}
	}
	bgDir := filepath.Join(dir, "backgrounds")
	bgs, err := os.ReadDir(bgDir)
	if err != nil {
		return nil // no wallpapers is fine; a palette is the theme
	}
	total, kept := int64(0), 0
	for _, e := range bgs {
		p := filepath.Join(bgDir, e.Name())
		info, err := e.Info()
		bad := err != nil ||
			!info.Mode().IsRegular() || // directories and symlinks
			!omarchyImageExts[strings.ToLower(filepath.Ext(e.Name()))] ||
			!validBackgroundFile(e.Name()) ||
			info.Size() > omarchyMaxBackground ||
			kept >= omarchyMaxBackgrounds
		if bad {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
			continue
		}
		total += info.Size()
		if total > omarchyMaxThemeBytes {
			return fmt.Errorf("theme backgrounds exceed %d MB", omarchyMaxThemeBytes>>20)
		}
		kept++
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// serveOmarchyThemes is the theme catalog: GET lists every selectable theme
// with its wallpapers, POST {"url"} installs one from a git repository and
// answers with the whole catalog, so the client never has to guess what the
// install produced.
func serveOmarchyThemes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{"themes": themeCatalog()})
	case http.MethodPost:
		var req struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeThemeError(w, installErr(http.StatusBadRequest, "bad json"))
			return
		}
		if req.URL == "" {
			writeThemeError(w, installErr(http.StatusBadRequest, `send {"url":"https://…"} to install a theme`))
			return
		}
		name, err := installOmarchyTheme(r.Context(), req.URL)
		if err != nil {
			writeThemeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"installed": name, "themes": themeCatalog()})
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// writeThemeError answers with JSON {"error"} so a client can toast the reason
// rather than a status code.
func writeThemeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var te themeInstallError
	if errors.As(err, &te) {
		status = te.status
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
