// Agent theme sync: when lasso's theme changes (via /api/theme-set, a host
// switch, or an out-of-band edit to herdr's config.toml picked up by the hub
// poll), mirror it into the agent CLIs' own theme files — opencode's tui.json,
// Claude Code's ~/.claude/themes/herdr.json, ghostty's themes/herdr, and
// lasso's settings.json (.theme.resolved, read by claude-contextline) — so
// agents render in step with herdr. Writes go through the Backend interface, so
// the active remote host gets the same treatment over SFTP (see
// syncRemoteTheme).
//
// This subsumes the old per-machine herdr-theme-sync watcher daemons: lasso is
// the single writer of herdr's [theme].name in practice, and its hub poll
// catches edits it didn't make, so no file-watching service is needed.
//
// Gated by the sync_agent_themes setting (default on); the Settings tab
// exposes it as "Sync agent themes".
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// syncAgentThemesKey is the settings-table key for the toggle. Unset means on.
const syncAgentThemesKey = "sync_agent_themes"

// syncAgentThemesEnabled reports whether agent theme sync is on (the default).
func syncAgentThemesEnabled() bool {
	v, err := getSetting(syncAgentThemesKey)
	if err != nil || v == "" {
		return true
	}
	on, err := strconv.ParseBool(v)
	return err != nil || on
}

// syncAgentThemesVia mirrors rt into the agent theme files on backend b.
// Best-effort like syncRemoteTheme: every failure is logged, never propagated —
// a theme switch must not fail because an agent's config dir is missing.
func syncAgentThemesVia(b Backend, rt resolvedTheme) {
	if b == nil || !syncAgentThemesEnabled() {
		return
	}
	home, err := b.HomeDir()
	if err != nil || home == "" {
		log.Printf("theme:    agent sync on %s: no home dir: %v", b.Name(), err)
		return
	}
	light := luminance(rt.ui.PanelBg) > 0.5
	if err := syncOpencodeTheme(b, home, light); err != nil {
		log.Printf("theme:    opencode sync on %s: %v", b.Name(), err)
	}
	if err := syncClaudeTheme(b, home, rt, light); err != nil {
		log.Printf("theme:    claude sync on %s: %v", b.Name(), err)
	}
	if err := syncGhosttyTheme(b, home, rt); err != nil {
		log.Printf("theme:    ghostty sync on %s: %v", b.Name(), err)
	}
	if err := syncLassoResolved(b, home, light); err != nil {
		log.Printf("theme:    lasso appearance sync on %s: %v", b.Name(), err)
	}
}

// resolveThemeByName resolves a canonical theme key (no custom overrides — used
// for the remote mirror, where lasso only writes [theme].name and the remote
// herdr owns any [theme.custom]).
func resolveThemeByName(name string) resolvedTheme {
	key := normalizeThemeName(name)
	def, ok := themes[key]
	if !ok {
		key, def = "catppuccin", themes["catppuccin"]
	}
	return resolvedTheme{Name: name, Resolved: key, ui: def.ui, ansi: def.ansi}
}

// ---------------------------------------------------------------------------
// opencode — ~/.config/opencode/tui.json "theme"
// ---------------------------------------------------------------------------

// opencodeThemeFor maps lasso's resolved light/dark onto the opencode built-in
// catppuccin variants (opencode can't detect the terminal background through
// the lasso/ttyd/xterm chain, so it can't pick its own variant — see the OSC 11
// discussion in the docs; we pin it instead). Latte is catppuccin's light
// variant; frappé/macchiato/mocha are all dark, so light must NOT map to any
// of them (it used to map to frappé, which painted opencode dark inside a
// light lasso).
func opencodeThemeFor(light bool) string {
	if light {
		return "catppuccin-latte"
	}
	return "catppuccin"
}

func syncOpencodeTheme(b Backend, home string, light bool) error {
	path := filepath.Join(home, ".config", "opencode", "tui.json")
	want := opencodeThemeFor(light)

	root := map[string]json.RawMessage{}
	data, err := b.ReadFile(path)
	switch {
	case err == nil:
		if json.Unmarshal(data, &root) != nil {
			// Malformed (or jsonc with comments) — don't clobber what we
			// can't parse.
			return nil
		}
	case errors.Is(err, fs.ErrNotExist):
		root["$schema"], _ = json.Marshal("https://opencode.ai/tui.json")
	default:
		return err
	}

	var cur string
	if raw, ok := root["theme"]; ok {
		_ = json.Unmarshal(raw, &cur)
	}
	if cur == want {
		return nil
	}
	root["theme"], _ = json.Marshal(want)
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := b.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	log.Printf("theme:    opencode theme -> %s on %s", want, b.Name())
	return b.WriteFile(path, out, 0o644)
}

// ---------------------------------------------------------------------------
// Claude Code — ~/.claude/themes/herdr.json (mapped palette, as herdr-theme-sync wrote)
// ---------------------------------------------------------------------------

// claudeThemeFile is the on-disk shape Claude Code expects for a custom theme:
// a base (its own dark/light palette) plus per-token hex overrides. Only tokens
// Claude knows are honored; unknown keys are ignored, so a superset is safe.
type claudeThemeFile struct {
	Name      string            `json:"name"`
	Base      string            `json:"base"`
	Overrides map[string]string `json:"overrides"`
}

// claudeOverrides maps herdr's UI tokens onto Claude Code's theme tokens
// (ported from herdr-theme-sync's mapping).
func claudeOverrides(p uiPalette) map[string]string {
	m := map[string]string{}
	put := func(hex string, toks ...string) {
		if hex == "" {
			return // let Claude's base show through
		}
		for _, t := range toks {
			m[t] = hex
		}
	}
	put(p.Text, "text")
	put(p.PanelBg, "background", "inverseText")
	put(p.Surface0, "userMessageBackground", "bashMessageBackgroundColor")
	put(p.Surface1, "userMessageBackgroundHover", "selectionBg")
	put(p.SurfaceDim, "composerSidebarBackground", "memoryBackgroundColor")
	put(p.Overlay0, "subtle", "inactive")
	put(p.Overlay1, "secondaryBorder", "suggestion")
	put(p.Accent, "permission", "ide", "promptBorder", "bashBorder")
	put(p.Mauve, "planMode", "thinking", "merged")
	put(p.Teal, "remember")
	put(p.Green, "success", "autoAccept", "diffAdded")
	put(p.Red, "error", "diffRemoved")
	put(p.Yellow, "warning")

	// Dimmed diff variants: blend the accent toward the background.
	bg := p.PanelBg
	if bg == "" {
		if luminance(p.Text) > 0.5 {
			bg = "#000000"
		} else {
			bg = "#ffffff"
		}
	}
	if p.Green != "" {
		m["diffAddedDimmed"] = blendHex(p.Green, bg, 0.6)
	}
	if p.Red != "" {
		m["diffRemovedDimmed"] = blendHex(p.Red, bg, 0.6)
	}
	return m
}

func syncClaudeTheme(b Backend, home string, rt resolvedTheme, light bool) error {
	path := filepath.Join(home, ".claude", "themes", "herdr.json")
	base := "dark"
	if light {
		base = "light"
	}
	theme := claudeThemeFile{
		Name:      "herdr (" + rt.Resolved + ")",
		Base:      base,
		Overrides: claudeOverrides(rt.ui),
	}
	out, err := json.MarshalIndent(theme, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	if cur, err := b.ReadFile(path); err == nil && string(cur) == string(out) {
		return nil
	}
	if err := b.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return b.WriteFile(path, out, 0o644)
}

// ---------------------------------------------------------------------------
// Ghostty — ~/.config/ghostty/themes/herdr + `theme = herdr` in the config
// ---------------------------------------------------------------------------

// ghosttyConfigPaths are the config files ghostty may read, relative to home.
// We only ever rewrite ones that already exist — a host without ghostty should
// not sprout a ghostty config. macOS keeps its config under Application Support
// (both bare and .ghostty-suffixed spellings are in the wild); the XDG path
// works on both platforms.
var ghosttyConfigPaths = [][]string{
	{".config", "ghostty", "config"},
	{"Library", "Application Support", "com.mitchellh.ghostty", "config"},
	{"Library", "Application Support", "com.mitchellh.ghostty", "config.ghostty"},
}

// ghosttyThemeBody renders rt as a ghostty theme file: the same chrome + ANSI
// split xtermJSON uses, so a ghostty window matches lasso's embedded terminal.
// Ghostty has no alpha on selection-background, so the translucent accent wash
// (see termSelectionAlpha) is pre-composited over the panel background instead.
func ghosttyThemeBody(rt resolvedTheme) []byte {
	u, a := rt.ui, rt.ansi
	var b []byte
	put := func(k, v string) {
		if v != "" {
			b = append(b, (k + " = " + v + "\n")...)
		}
	}
	b = append(b, ("# Generated by lasso — herdr theme " + rt.Resolved + ". Edits are overwritten.\n")...)
	put("background", u.PanelBg)
	put("foreground", u.Text)
	put("cursor-color", u.Text)
	put("cursor-text", u.PanelBg)
	put("selection-background", blendHex(u.Accent, u.PanelBg, 1-float64(termSelectionAlpha)/255))
	put("selection-foreground", u.Text)
	for i, hex := range []string{
		a.Black, a.Red, a.Green, a.Yellow, a.Blue, a.Magenta, a.Cyan, a.White,
		a.BrightBlack, a.BrightRed, a.BrightGreen, a.BrightYellow,
		a.BrightBlue, a.BrightMagenta, a.BrightCyan, a.BrightWhite,
	} {
		put("palette", strconv.Itoa(i)+"="+hex)
	}
	return b
}

// ghosttySetTheme points a ghostty config's `theme` key at our generated theme,
// returning the new body and whether anything changed. Existing assignments are
// rewritten in place (rather than appended past, which ghostty would also honor)
// so the file doesn't grow a line per theme change and keeps the user's
// ordering; a config with no theme key gets one appended. Any prior value —
// including a `light:X,dark:Y` pair — is replaced: once lasso syncs ghostty it
// owns the choice.
func ghosttySetTheme(body []byte, name string) ([]byte, bool) {
	want := "theme = " + name
	lines := strings.Split(string(body), "\n")
	found := false
	for i, ln := range lines {
		k, _, ok := strings.Cut(ln, "=")
		if !ok || strings.TrimSpace(k) != "theme" {
			continue // not an assignment, or not ours (comments have no bare key)
		}
		lines[i], found = want, true
	}
	if !found {
		// Keep exactly one trailing newline whether or not the file had one.
		lines = nil
		if trimmed := strings.TrimRight(string(body), "\n"); trimmed != "" {
			lines = strings.Split(trimmed, "\n")
		}
		lines = append(lines, want, "")
	}
	out := strings.Join(lines, "\n")
	if out == string(body) {
		return nil, false
	}
	return []byte(out), true
}

// ghosttyThemeName is the theme file lasso writes and points ghostty's config at.
const ghosttyThemeName = "herdr"

func syncGhosttyTheme(b Backend, home string, rt resolvedTheme) error {
	// Ghostty resolves bare theme names against ~/.config/ghostty/themes on
	// every platform, so one location covers macOS and Linux.
	path := filepath.Join(home, ".config", "ghostty", "themes", ghosttyThemeName)
	body := ghosttyThemeBody(rt)
	if cur, err := b.ReadFile(path); err != nil || string(cur) != string(body) {
		if err := b.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := b.WriteFile(path, body, 0o644); err != nil {
			return err
		}
	}

	for _, parts := range ghosttyConfigPaths {
		cfg := filepath.Join(append([]string{home}, parts...)...)
		cur, err := b.ReadFile(cfg)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		next, changed := ghosttySetTheme(cur, ghosttyThemeName)
		if !changed {
			continue
		}
		log.Printf("theme:    ghostty theme -> %s in %s on %s", ghosttyThemeName, cfg, b.Name())
		if err := b.WriteFile(cfg, next, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// lasso settings.json — .theme.resolved (claude-contextline's light/dark cue)
// ---------------------------------------------------------------------------

func syncLassoResolved(b Backend, home string, light bool) error {
	path := filepath.Join(home, ".lasso", "settings.json")
	resolved := "dark"
	if light {
		resolved = "light"
	}

	root := map[string]json.RawMessage{}
	if data, err := b.ReadFile(path); err == nil {
		if json.Unmarshal(data, &root) != nil {
			return nil // don't clobber an unparseable file
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	theme := map[string]json.RawMessage{}
	if raw, ok := root["theme"]; ok {
		_ = json.Unmarshal(raw, &theme)
	}
	var cur string
	if raw, ok := theme["resolved"]; ok {
		_ = json.Unmarshal(raw, &cur)
	}
	if cur == resolved {
		return nil // already in step; leave the file (and its mtime) untouched
	}
	set := func(m map[string]json.RawMessage, k string, v any) {
		b, _ := json.Marshal(v)
		m[k] = b
	}
	set(theme, "mode", "herdr")
	set(theme, "resolved", resolved)
	set(theme, "updatedAt", time.Now().UTC().Format(time.RFC3339))
	set(root, "theme", theme)

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := b.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return b.WriteFile(path, out, 0o644)
}

// ---------------------------------------------------------------------------
// color math (ported from herdr-theme-sync)
// ---------------------------------------------------------------------------

func hexRGB(hex string) (r, g, b int, ok bool) {
	if len(hex) != 7 || hex[0] != '#' {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff), true
}

// luminance returns the relative luminance (0..1) of a "#rrggbb" color.
func luminance(hex string) float64 {
	r, g, b, ok := hexRGB(hex)
	if !ok {
		return 0
	}
	return (0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)) / 255.0
}

// blendHex mixes color a toward color b by fraction t (0..1).
func blendHex(a, b string, t float64) string {
	ar, ag, ab, ok1 := hexRGB(a)
	br, bg, bb, ok2 := hexRGB(b)
	if !ok1 || !ok2 {
		return a
	}
	mix := func(x, y int) int { return int(float64(x)*(1-t) + float64(y)*t + 0.5) }
	return fmt.Sprintf("#%02x%02x%02x", mix(ar, br), mix(ag, bg), mix(ab, bb))
}
