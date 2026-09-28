package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
)

// Appearance plugins: themes and fonts a plugin contributes as DATA.
//
// A plugin never ships CSS. One that could would restyle or hide lasso's own
// UI — make the approval dialog's warnings invisible, paint a fake prompt — so
// a plugin contributes OPTIONS and lasso composes every byte of CSS itself from
// validated values:
//
//   - A theme is an Omarchy-format theme directory inside the plugin. It joins
//     the SAME registry as the official and installed themes (omarchy.go), so
//     it paints the chrome, the terminals, herdr's config.toml, every agent
//     CLI's theme file and the fleet sync, with no parallel pipeline.
//   - A font is a family name plus font files. The family string ends up
//     inside a CSS @font-face rule the frontend writes, which is why
//     pluginFontFamilyRE is the injection guard and must never be loosened.
//
// Only enabled (approved) plugins contribute; disabling one withdraws its
// themes and fonts, and a selection naming one falls back the way a theme only
// a newer binary knows already does.

const (
	pluginThemesMax    = 32
	pluginFontsMax     = 16
	pluginFontFacesMax = 8
	pluginFontFileMax  = 5 << 20
	pluginLabelMax     = 64
	pluginLicenseMax   = 64
)

var (
	pluginThemeIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	pluginFontIDRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	// pluginFontFamilyRE is the CSS injection guard: the family is written into
	// a quoted font-family and an @font-face rule, so it may hold nothing that
	// can close a string or a declaration. Never loosen it.
	pluginFontFamilyRE = regexp.MustCompile(`^[A-Za-z0-9 _-]{1,64}$`)
	// fontGlobalIDRE is a font's global id as ui_state.typography stores it.
	fontGlobalIDRE = regexp.MustCompile(`^plugin:[a-z][a-z0-9-]{0,31}:[a-z][a-z0-9-]{0,31}$`)
)

var pluginFontCategories = []string{"sans", "serif", "display", "mono"}

// pluginFontTypes is every font file extension a face may name, with the
// Content-Type it is served as. Go's built-in mime table has none of them, and
// a system mime.types may spell them differently (application/font-woff2), so
// serveFiles sets these explicitly.
var pluginFontTypes = map[string]string{
	".woff2": "font/woff2",
	".woff":  "font/woff",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
}

type pluginThemeSpec struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Dir   string `json:"dir"`
}

type pluginFontSpec struct {
	ID       string           `json:"id"`
	Family   string           `json:"family"`
	Category string           `json:"category"`
	Faces    []pluginFontFace `json:"faces"`
	License  string           `json:"license"`
}

type pluginFontFace struct {
	File   string `json:"file"`
	Weight int    `json:"weight"`
	Style  string `json:"style"`
}

func fontGlobalID(plugin, font string) string { return "plugin:" + plugin + ":" + font }

// validateAppearance is the part of validation that needs no filesystem.
func (m *pluginManifest) validateAppearance() error {
	if len(m.Themes) > pluginThemesMax {
		return fmt.Errorf("at most %d themes (has %d)", pluginThemesMax, len(m.Themes))
	}
	seen := map[string]bool{}
	for i, t := range m.Themes {
		if !pluginThemeIDRE.MatchString(t.ID) {
			return fmt.Errorf("themes[%d]: id %q must match %s", i, t.ID, pluginThemeIDRE)
		}
		if seen[t.ID] {
			return fmt.Errorf("themes[%d]: duplicate id %q", i, t.ID)
		}
		seen[t.ID] = true
		if len(t.Label) > pluginLabelMax {
			return fmt.Errorf("themes[%d] (%s): label must be at most %d characters", i, t.ID, pluginLabelMax)
		}
		if _, err := cleanPluginPath(t.Dir); err != nil {
			return fmt.Errorf("themes[%d] (%s): dir: %v", i, t.ID, err)
		}
	}
	if len(m.Fonts) > pluginFontsMax {
		return fmt.Errorf("at most %d fonts (has %d)", pluginFontsMax, len(m.Fonts))
	}
	seen = map[string]bool{}
	for i, f := range m.Fonts {
		if !pluginFontIDRE.MatchString(f.ID) {
			return fmt.Errorf("fonts[%d]: id %q must match %s", i, f.ID, pluginFontIDRE)
		}
		if seen[f.ID] {
			return fmt.Errorf("fonts[%d]: duplicate id %q", i, f.ID)
		}
		seen[f.ID] = true
		if !pluginFontFamilyRE.MatchString(f.Family) || strings.TrimSpace(f.Family) == "" {
			return fmt.Errorf("fonts[%d] (%s): family %q must match %s", i, f.ID, f.Family, pluginFontFamilyRE)
		}
		if !slices.Contains(pluginFontCategories, f.Category) {
			return fmt.Errorf("fonts[%d] (%s): category must be one of %s", i, f.ID, strings.Join(pluginFontCategories, ", "))
		}
		if len(f.License) > pluginLicenseMax {
			return fmt.Errorf("fonts[%d] (%s): license must be at most %d characters", i, f.ID, pluginLicenseMax)
		}
		if len(f.Faces) == 0 || len(f.Faces) > pluginFontFacesMax {
			return fmt.Errorf("fonts[%d] (%s): needs 1-%d faces", i, f.ID, pluginFontFacesMax)
		}
		for j, face := range f.Faces {
			where := fmt.Sprintf("fonts[%d] (%s) faces[%d]", i, f.ID, j)
			if _, err := cleanPluginPath(face.File); err != nil {
				return fmt.Errorf("%s: file: %v", where, err)
			}
			if _, ok := pluginFontTypes[strings.ToLower(path.Ext(face.File))]; !ok {
				return fmt.Errorf("%s: file %q must be .woff2, .woff, .ttf or .otf", where, face.File)
			}
			if face.Weight < 100 || face.Weight > 900 || face.Weight%100 != 0 {
				return fmt.Errorf("%s: weight must be 100-900 in steps of 100", where)
			}
			if face.Style != "normal" && face.Style != "italic" {
				return fmt.Errorf("%s: style must be normal or italic", where)
			}
		}
	}
	return nil
}

// validateAppearanceAssets checks what the manifest names on disk — every
// theme's palette parses, every font file is a regular file within the size
// cap — all through the plugin directory's os.Root, so nothing a symlink
// points outside the plugin at is read. It returns a digest of the theme
// inputs (palette bytes, light.mode, background names) so the manager can tell
// an in-place palette edit from a rescan that found nothing new.
func (m *pluginManifest) validateAppearanceAssets(dir string) (string, error) {
	if len(m.Themes) == 0 && len(m.Fonts) == 0 {
		return "", nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	fsys := root.FS()
	h := sha256.New()
	for i, t := range m.Themes {
		rel, _ := cleanPluginPath(t.Dir) // validated
		st, err := fs.Stat(fsys, rel)
		if err != nil || !st.IsDir() {
			return "", fmt.Errorf("themes[%d] (%s): dir %q is not a directory in the plugin", i, t.ID, t.Dir)
		}
		sub, err := fs.Sub(fsys, rel)
		if err != nil {
			return "", fmt.Errorf("themes[%d] (%s): %v", i, t.ID, err)
		}
		if _, err := readThemePaletteFS(sub); err != nil {
			return "", fmt.Errorf("themes[%d] (%s): %v", i, t.ID, err)
		}
		fmt.Fprintf(h, "theme\x00%s\x00%s\x00%s\x00", t.ID, t.Label, rel)
		for _, f := range []string{"colors.toml", "alacritty.toml", "light.mode"} {
			b, err := fs.ReadFile(sub, f)
			if err != nil {
				fmt.Fprintf(h, "%s\x00-\x00", f)
				continue
			}
			sum := sha256.Sum256(b)
			fmt.Fprintf(h, "%s\x00%x\x00", f, sum)
		}
		if ents, err := fs.ReadDir(sub, "backgrounds"); err == nil {
			for _, e := range ents {
				if info, err := e.Info(); err == nil {
					fmt.Fprintf(h, "bg\x00%s\x00%d\x00%d\x00", e.Name(), info.Size(), info.ModTime().UnixNano())
				}
			}
		}
	}
	for i, f := range m.Fonts {
		for j, face := range f.Faces {
			rel, _ := cleanPluginPath(face.File) // validated
			st, err := fs.Stat(fsys, rel)
			if err != nil || !st.Mode().IsRegular() {
				return "", fmt.Errorf("fonts[%d] (%s) faces[%d]: %q is not a file in the plugin", i, f.ID, j, face.File)
			}
			if st.Size() > pluginFontFileMax {
				return "", fmt.Errorf("fonts[%d] (%s) faces[%d]: %q is larger than %d MB", i, f.ID, j, face.File, pluginFontFileMax>>20)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------------------------------------------------------------------------
// the registry side (read by omarchy.go)
// ---------------------------------------------------------------------------

// pluginThemeRef is one theme an ENABLED plugin contributes, as the registry
// needs it.
type pluginThemeRef struct {
	Plugin    string
	ID        string
	Label     string
	PluginDir string
	Dir       string // cleaned, relative to PluginDir
}

// palette reads the theme through the plugin directory's os.Root.
func (r pluginThemeRef) palette() (omarchyPalette, error) {
	root, err := os.OpenRoot(r.PluginDir)
	if err != nil {
		return omarchyPalette{}, err
	}
	defer root.Close()
	sub, err := fs.Sub(root.FS(), r.Dir)
	if err != nil {
		return omarchyPalette{}, err
	}
	return readThemePaletteFS(sub)
}

// pluginThemeRefs is what the plugin manager last said is contributed, ordered
// by plugin name then manifest order. A value the registry reads rather than a
// call into the manager, so omarchy.go never takes the manager's locks (it is
// read under omarchyMu, and the manager reloads the registry while holding
// its own).
var pluginThemeRefs atomic.Pointer[[]pluginThemeRef]

func setPluginThemeRefs(r []pluginThemeRef) { pluginThemeRefs.Store(&r) }

func pluginThemeRefsNow() []pluginThemeRef {
	if p := pluginThemeRefs.Load(); p != nil {
		return *p
	}
	return nil
}

// pluginBGRoot is a registered plugin theme's backgrounds directory: the
// plugin directory (the os.Root every read goes through) and the path under it.
type pluginBGRoot struct {
	dir string
	rel string
}

// omarchyPluginBG maps a registered plugin theme key to its backgrounds. An
// atomic map swapped by reloadOmarchyThemes rather than a field under
// omarchyMu: themeCatalog lists backgrounds while HOLDING that read lock, and
// a recursive RLock deadlocks as soon as a writer is waiting.
var omarchyPluginBG atomic.Pointer[map[string]pluginBGRoot]

func pluginBackgroundOf(name string) (pluginBGRoot, bool) {
	m := omarchyPluginBG.Load()
	if m == nil {
		return pluginBGRoot{}, false
	}
	b, ok := (*m)[name]
	return b, ok
}

// list is the wallpaper file names: regular files with an image extension.
// A symlink is skipped even when it points inside the plugin — the served set
// must be exactly what a directory listing of the plugin shows.
func (b pluginBGRoot) list() []string {
	root, err := os.OpenRoot(b.dir)
	if err != nil {
		return nil
	}
	defer root.Close()
	ents, err := fs.ReadDir(root.FS(), b.rel)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.Type().IsRegular() && validBackgroundFile(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// open opens one wallpaper through the os.Root, which refuses any path that
// resolves outside the plugin directory.
func (b pluginBGRoot) open(file string) (*os.File, fs.FileInfo, error) {
	if !validBackgroundFile(file) {
		return nil, nil, fs.ErrNotExist
	}
	root, err := os.OpenRoot(b.dir)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	f, err := root.Open(path.Join(b.rel, file))
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, fs.ErrNotExist
	}
	return f, st, nil
}

// ---------------------------------------------------------------------------
// the manager side
// ---------------------------------------------------------------------------

// syncThemes rebuilds the theme registry when the set of contributed themes —
// or any of their palettes — changed, and reports whether it did. Called from
// noteIfChanged, i.e. after every rescan, enable, disable and re-approval, so
// the 10s rescan is also what picks up a palette edited in place.
func (m *pluginManager) syncThemes() bool {
	grants := loadPluginGrants()
	m.mu.Lock()
	names := make([]string, 0, len(m.entries))
	for n := range m.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var refs []pluginThemeRef
	sig := sha256.New()
	for _, n := range names {
		e := m.entries[n]
		if e.state(grants[n]) != pluginStateEnabled || len(e.Man.Themes) == 0 {
			continue
		}
		fmt.Fprintf(sig, "%s\x00%s\x00%s\n", n, e.Dir, e.Man.themeSig)
		for _, t := range e.Man.Themes {
			rel, _ := cleanPluginPath(t.Dir)
			refs = append(refs, pluginThemeRef{Plugin: n, ID: t.ID, Label: strings.TrimSpace(t.Label), PluginDir: e.Dir, Dir: rel})
		}
	}
	m.mu.Unlock()
	now := hex.EncodeToString(sig.Sum(nil))

	m.themeMu.Lock()
	defer m.themeMu.Unlock()
	if now == m.themeSig {
		return false
	}
	first := m.themeSig == ""
	m.themeSig = now
	if first && len(refs) == 0 {
		return false // boot with no plugin themes: the registry already says so
	}

	// What the fleet wears right now, and its palette before the swap — so a
	// palette edited under the CURRENT theme can be carried to herdr and the
	// agents like a re-theme, and nothing else pays for it.
	cur := ""
	if srvHub != nil {
		if rt := srvHub.themeSnapshot(); !rt.Foreign {
			cur = rt.Resolved
		}
	}
	oldDef, hadOld := lookupThemeDef(cur)

	setPluginThemeRefs(refs)
	reloadOmarchyThemes()
	log.Printf("plugins:  theme registry rebuilt (%d plugin theme%s)", len(refs), plural(len(refs)))

	if srvHub == nil || cur == "" {
		return true
	}
	newDef, ok := lookupThemeDef(cur)
	forced := *themeName != "" && *themeName != "auto"
	// A dev lasso may not rewrite the file (themeWritesAllowed), so it only
	// re-resolves below, repainting its own tabs.
	if hadOld && ok && newDef != oldDef && themeSourceOf(cur) == "plugin" && !forced && themeWritesAllowed() {
		// herdr's config.toml spells a lasso-only theme as base + a generated
		// override block holding the palette, so the file has to be rewritten
		// for the local TUI to repaint; setLocalHerdrTheme does that, marks it
		// as lasso's own write and re-resolves, which fans the new palette out.
		go func() {
			if err := setLocalHerdrTheme(cur); err != nil {
				log.Printf("plugins:  re-theme %s after a palette edit: %v", cur, err)
			}
		}()
		return true
	}
	// Anything else — the current theme appearing or disappearing with its
	// plugin — is what the hub's own poll resolves; asking now just saves the
	// wait.
	go srvHub.refreshTheme()
	return true
}

// resetPluginThemes withdraws every plugin theme from the registry (tests).
func resetPluginThemes() {
	setPluginThemeRefs(nil)
	reloadOmarchyThemes()
}

// ---------------------------------------------------------------------------
// the listing
// ---------------------------------------------------------------------------

type pluginFontPerm struct {
	ID       string `json:"id"`
	Family   string `json:"family"`
	Category string `json:"category"`
}

type pluginThemeOut struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	KeyTaken bool   `json:"key_taken,omitempty"`
}

type pluginFontOut struct {
	ID       string          `json:"id"`
	GlobalID string          `json:"global_id"`
	Family   string          `json:"family"`
	Category string          `json:"category"`
	License  string          `json:"license,omitempty"`
	Faces    []pluginFaceOut `json:"faces,omitempty"`
}

type pluginFaceOut struct {
	URL    string `json:"url"`
	Weight int    `json:"weight"`
	Style  string `json:"style"`
}

// pluginFileURL is where /plugins/ serves one of a plugin's files, each
// segment escaped since the path came from a manifest.
func pluginFileURL(name, rel string) string {
	c, _ := cleanPluginPath(rel)
	segs := strings.Split(c, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "/plugins/" + name + "/" + strings.Join(segs, "/")
}

// themeKeyTakenOutsidePlugins reports whether a key is already a built-in,
// official or installed theme (or an alias of one) — i.e. whether a plugin
// theme by this id would be skipped whatever other plugins do. Used for a
// plugin that is not enabled yet, so the approval dialog can say so up front.
func themeKeyTakenOutsidePlugins(key string) bool {
	if normalizeThemeName(key) != key {
		return true
	}
	if _, ok := themes[key]; ok {
		return true
	}
	src := themeSourceOf(key)
	return src != "" && src != "plugin"
}

// appearanceListing fills a plugin's themes, fonts and warnings.
func appearanceListing(p *pluginPayload, e *pluginEntry, enabled bool) {
	var skips map[string]string
	if enabled {
		skips = pluginThemeSkips(e.Name)
	}
	for _, t := range e.Man.Themes {
		label := strings.TrimSpace(t.Label)
		if label == "" {
			label = omarchyLabel(t.ID)
		}
		out := pluginThemeOut{ID: t.ID, Label: label}
		if enabled {
			if why, skipped := skips[t.ID]; skipped {
				out.KeyTaken = true
				p.Warnings = append(p.Warnings, why)
			}
		} else if themeKeyTakenOutsidePlugins(t.ID) {
			out.KeyTaken = true
			p.Warnings = append(p.Warnings, fmt.Sprintf("theme %q is taken by an existing theme and will be skipped", t.ID))
		}
		p.Themes = append(p.Themes, out)
	}
	for _, f := range e.Man.Fonts {
		out := pluginFontOut{ID: f.ID, GlobalID: fontGlobalID(e.Name, f.ID), Family: f.Family, Category: f.Category, License: f.License}
		if enabled {
			for _, face := range f.Faces {
				out.Faces = append(out.Faces, pluginFaceOut{URL: pluginFileURL(e.Name, face.File), Weight: face.Weight, Style: face.Style})
			}
		}
		p.Fonts = append(p.Fonts, out)
	}
}
