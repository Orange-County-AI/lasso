// Agent theme sync: when lasso's theme changes (via /api/theme-set, a host
// switch, or an out-of-band edit to herdr's config.toml picked up by the hub
// poll), mirror it into the agent CLIs' own theme files — a generated
// opencode theme (themes/herdr.json, pinned via tui.json), Claude Code's
// ~/.claude/themes/herdr.json, ghostty's themes/herdr, omp's
// ~/.omp/agent/themes/herdr.json (pinned in both of its mode slots, and the one
// mirror a running agent picks up live), and lasso's settings.json
// (.theme.resolved, read by claude-contextline) — so agents render in step with
// herdr. Writes go through the Backend interface, so the active remote host gets
// the same treatment over SFTP (see syncRemoteTheme).
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

	"gopkg.in/yaml.v3"
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
	if err := syncOpencodeTheme(b, home, rt); err != nil {
		log.Printf("theme:    opencode sync on %s: %v", b.Name(), err)
	}
	if err := syncClaudeTheme(b, home, rt, light); err != nil {
		log.Printf("theme:    claude sync on %s: %v", b.Name(), err)
	}
	if err := syncOmpTheme(b, home, rt); err != nil {
		log.Printf("theme:    omp sync on %s: %v", b.Name(), err)
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
// opencode — generated themes/herdr.json + tui.json "theme" pin + kv mode hint
// ---------------------------------------------------------------------------
//
// opencode 1.x resolves its light/dark mode as: kv "theme_mode_lock" ??
// the terminal-detected background ?? its default. Neither works for us:
// detection fails through the lasso/ttyd/xterm chain, and the lock lives in
// opencode's state kv.json, which any running instance rewrites wholesale from
// its in-memory state on every state change — so a pin written under it gets
// clobbered back (observed: a light-locked instance overwrote lasso's dark pin
// seconds later, and every fresh launch then came up light).
//
// So the mode lock can't be load-bearing. Instead lasso generates a theme from
// herdr's resolved palette and pins it by name — both in files opencode reads
// but never writes: ~/.config/opencode/themes/herdr.json (custom themes are
// discovered from <config>/themes/*.json) and the "theme" key in tui.json.
// It also writes legacy Catppuccin aliases: custom themes override built-ins,
// so an older lasso or dev build that later rewrites tui.json to "catppuccin"
// still resolves the generated, mode-invariant Herdr palette.
// Every token is emitted as a bare hex, which opencode's resolver treats as
// mode-invariant (only {dark,light} pair objects consult the mode), so the
// rendered colors are correct no matter what mode the lock/detection lands on.
// A theme flip rewrites herdr.json with the new palette; fresh launches pick
// it up. The kv mode lock is still written as a fallback hint (it keeps
// opencode's built-in adaptive themes in step if the pin is ever removed
// manually), but nothing correct depends on it anymore.

// opencodeThemeName is the generated theme lasso pins for opencode.
const opencodeThemeName = "herdr"

// opencodeThemeNames includes the generated theme and legacy names older lasso
// builds can write to tui.json or opencode's state. The custom files shadow
// opencode's built-ins, making those stale writers harmless.
var opencodeThemeNames = []string{
	opencodeThemeName,
	"catppuccin",
	"catppuccin-frappe",
	"catppuccin-macchiato",
}

func syncOpencodeTheme(b Backend, home string, rt resolvedTheme) error {
	if err := syncOpencodeThemeFile(b, home, rt); err != nil {
		return err
	}
	if err := syncOpencodeTui(b, home); err != nil {
		return err
	}
	return syncOpencodeMode(b, home, luminance(rt.ui.PanelBg) > 0.5)
}

// opencodeThemeBody renders rt as an opencode custom theme: herdr's UI tokens
// mapped onto the same semantic roles lasso uses for its own chrome, all as
// bare hex strings so the mode can't change the result.
func opencodeThemeBody(rt resolvedTheme) []byte {
	u, a := rt.ui, rt.ansi
	m := map[string]string{
		"primary":               u.Accent,
		"secondary":             u.Mauve,
		"accent":                a.Magenta, // the palette's pink slot (mocha pink, dracula pink, etc.)
		"error":                 u.Red,
		"warning":               u.Yellow,
		"success":               u.Green,
		"info":                  u.Teal,
		"text":                  u.Text,
		"textMuted":             u.Subtext0,
		"background":            u.PanelBg,
		"backgroundPanel":       u.Surface0,
		"backgroundElement":     u.Surface1,
		"border":                u.Surface1,
		"borderActive":          u.Accent,
		"borderSubtle":          u.SurfaceDim,
		"diffAdded":             u.Green,
		"diffRemoved":           u.Red,
		"diffContext":           u.Subtext0,
		"diffHunkHeader":        u.Peach,
		"diffHighlightAdded":    u.Green,
		"diffHighlightRemoved":  u.Red,
		"diffAddedBg":           blendHex(u.Green, u.PanelBg, 0.9),
		"diffRemovedBg":         blendHex(u.Red, u.PanelBg, 0.9),
		"diffContextBg":         u.Surface0,
		"diffLineNumber":        u.Subtext0,
		"diffAddedLineNumberBg": blendHex(u.Green, u.PanelBg, 0.95),
		// Removed line numbers sit on the same tinted rows.
		"diffRemovedLineNumberBg": blendHex(u.Red, u.PanelBg, 0.95),
		"markdownText":            u.Text,
		"markdownHeading":         u.Mauve,
		"markdownLink":            u.Blue,
		"markdownLinkText":        u.Teal,
		"markdownCode":            u.Green,
		"markdownBlockQuote":      u.Yellow,
		"markdownEmph":            u.Yellow,
		"markdownStrong":          u.Peach,
		"markdownHorizontalRule":  u.Subtext0,
		"markdownListItem":        u.Blue,
		"markdownListEnumeration": u.Teal,
		"markdownImage":           u.Blue,
		"markdownImageText":       u.Teal,
		"markdownCodeBlock":       u.Text,
		"syntaxComment":           u.Subtext0,
		"syntaxKeyword":           u.Mauve,
		"syntaxFunction":          u.Blue,
		"syntaxVariable":          u.Red,
		"syntaxString":            u.Green,
		"syntaxNumber":            u.Peach,
		"syntaxType":              u.Yellow,
		"syntaxOperator":          u.Teal,
		"syntaxPunctuation":       u.Text,
	}
	// Defensive: drop anything that isn't "#rrggbb" — a bare non-hex string
	// would be resolved as a color *reference* and throw at render time.
	for tok, v := range m {
		if _, _, _, ok := hexRGB(v); !ok {
			delete(m, tok)
		}
	}
	root := map[string]any{
		"$schema": "https://opencode.ai/theme.json",
		"theme":   m,
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil
	}
	return append(out, '\n')
}

func syncOpencodeThemeFile(b Backend, home string, rt resolvedTheme) error {
	dir := filepath.Join(home, ".config", "opencode", "themes")
	body := opencodeThemeBody(rt)
	for _, name := range opencodeThemeNames {
		path := filepath.Join(dir, name+".json")
		if cur, err := b.ReadFile(path); err == nil && string(cur) == string(body) {
			continue
		}
		if err := b.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		log.Printf("theme:    opencode theme file -> %s (%s) on %s", name, rt.Resolved, b.Name())
		if err := b.WriteFile(path, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func syncOpencodeTui(b Backend, home string) error {
	path := filepath.Join(home, ".config", "opencode", "tui.json")

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
	if cur == opencodeThemeName {
		return nil
	}
	root["theme"], _ = json.Marshal(opencodeThemeName)
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := b.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	log.Printf("theme:    opencode theme -> %s on %s", opencodeThemeName, b.Name())
	return b.WriteFile(path, out, 0o644)
}

// syncOpencodeMode pins opencode's light/dark mode as a fallback hint. With
// the generated herdr theme pinned (see above), every rendered token is
// mode-invariant, so the lock only matters if the tui.json pin is removed
// manually and opencode falls back to one of its built-in adaptive themes.
// Note the lock is best-effort by nature: a running opencode keeps its
// in-memory kv state and may write it back over ours.
func syncOpencodeMode(b Backend, home string, light bool) error {
	path := filepath.Join(home, ".local", "state", "opencode", "kv.json")
	want := "dark"
	if light {
		want = "light"
	}

	root := map[string]json.RawMessage{}
	data, err := b.ReadFile(path)
	switch {
	case err == nil:
		if json.Unmarshal(data, &root) != nil {
			return nil // don't clobber what we can't parse
		}
	case errors.Is(err, fs.ErrNotExist):
		// Create it — a first-ever opencode launch then starts in the right
		// mode instead of guessing.
	default:
		return err
	}

	var lock, cur string
	if raw, ok := root["theme_mode_lock"]; ok {
		_ = json.Unmarshal(raw, &lock)
	}
	if raw, ok := root["theme_mode"]; ok {
		_ = json.Unmarshal(raw, &cur)
	}
	if lock == want && cur == want {
		return nil
	}
	root["theme_mode_lock"], _ = json.Marshal(want)
	root["theme_mode"], _ = json.Marshal(want)
	out, err := json.Marshal(root) // kv.json is compact single-line JSON
	if err != nil {
		return err
	}
	if err := b.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	log.Printf("theme:    opencode mode -> %s on %s", want, b.Name())
	return b.WriteFile(path, out, 0o644)
}

// ---------------------------------------------------------------------------
// omp (Oh My Pi) — generated themes/herdr.json + both config.yml theme slots
// ---------------------------------------------------------------------------
//
// omp holds two named themes — settings theme.dark and theme.light — and picks
// between them from the TERMINAL's background (OSC 11 luminance, then
// COLORFGBG, then a hardcoded "dark"), re-deciding on terminal appearance
// reports and SIGWINCH. That detection is what fails through the
// lasso/ttyd/xterm chain, the same reason opencode's needs overriding.
//
// Pinning both slots at omp's own built-in "dark"/"light" (what lasso did
// before) only half-worked. It did make the SLOT choice irrelevant, but the pin
// lives in config.yml, which omp reads once at startup — so a herdr theme flip
// never reached a RUNNING omp, and an agent launched before the flip kept the
// mode its terminal happened to look like until someone restarted it.
//
// So omp gets opencode's treatment instead: a generated theme carrying herdr's
// palette at ~/.omp/agent/themes/herdr.json, pinned in BOTH slots. Two things
// follow:
//
//   - the light/dark branch cannot matter — both slots name one theme, and its
//     colors are herdr's rather than a mode's;
//   - it repaints LIVE. omp watches <themes dir>/<current theme>.json and
//     debounce-reloads it on change, so rewriting herdr.json restyles every
//     running omp launched with the pin in place. That watcher is deliberately
//     skipped when the current theme is named "dark" or "light" — one more
//     reason the old pin could never repaint anything.
//
// omp's lookup resolves built-ins BEFORE custom files, so the generated file
// must not be called dark.json/light.json (it would be shadowed); "herdr" is
// both unshadowed and watched. Every token is a literal hex: a token that is
// neither a hex nor a declared var throws during resolution, and omp answers a
// failed theme load by falling back to its built-in "dark" — i.e. one bad token
// costs the whole palette.
//
// The config pin stays best-effort, as opencode's is: omp rewrites config.yml
// wholesale from its in-memory settings whenever one changes, so a running
// instance can write back over the slots. That cannot undo a live repaint (the
// theme file is what running instances read), and fresh launches take the pin.

// ompThemeName is the generated theme lasso writes and pins in both of omp's
// mode slots. Must not collide with an omp built-in — see above.
const ompThemeName = "herdr"

// ompThemeSchema is the $schema omp's own built-in themes carry. Informational
// only (omp validates with an in-code ArkType schema), but it makes the
// generated file behave in an editor.
const ompThemeSchema = "https://raw.githubusercontent.com/can1357/oh-my-pi/main/packages/coding-agent/theme-schema.json"

// syncOmpTheme writes the generated theme and points both mode slots at it.
// Skipped entirely on hosts where omp has never run (no ~/.omp/agent), so a
// theme switch doesn't litter config for a CLI that isn't there.
func syncOmpTheme(b Backend, home string, rt resolvedTheme) error {
	dir := filepath.Join(home, ".omp", "agent")
	if _, err := b.Stat(dir); err != nil {
		return nil // omp not set up on this host
	}
	if err := syncOmpThemeFile(b, dir, rt); err != nil {
		return err
	}
	return syncOmpThemePin(b, dir)
}

// ompColors maps herdr's UI tokens onto omp's color tokens — the same semantic
// roles lasso uses for its own chrome and for opencode's generated theme. Every
// token omp requires is present; thinkingMax is the one optional token and is
// emitted too (omp falls back to thinkingXhigh without it).
func ompColors(u uiPalette) map[string]string {
	return map[string]string{
		"accent":       u.Accent,
		"border":       u.Surface1,
		"borderAccent": u.Accent,
		"borderMuted":  u.Surface0,
		"success":      u.Green,
		"error":        u.Red,
		"warning":      u.Yellow,
		"muted":        u.Subtext0,
		"dim":          u.Overlay0,
		"text":         u.Text,
		"thinkingText": u.Subtext0,

		"selectedBg":         u.Surface0,
		"userMessageBg":      u.SurfaceDim,
		"userMessageText":    u.Text,
		"customMessageBg":    u.Surface0,
		"customMessageText":  u.Text,
		"customMessageLabel": u.Mauve,
		// Tool result frames tint their background toward the outcome, the way
		// the opencode theme tints diff rows: a 10% wash over the panel bg, so
		// success/error read at a glance without fighting the text.
		"toolPendingBg": u.SurfaceDim,
		"toolSuccessBg": blendHex(u.Green, u.PanelBg, 0.9),
		"toolErrorBg":   blendHex(u.Red, u.PanelBg, 0.9),
		"toolTitle":     u.Text,
		"toolOutput":    u.Subtext0,

		"mdHeading":         u.Mauve,
		"mdLink":            u.Blue,
		"mdLinkUrl":         u.Subtext0,
		"mdCode":            u.Green,
		"mdCodeBlock":       u.Text,
		"mdCodeBlockBorder": u.Surface1,
		"mdQuote":           u.Yellow,
		"mdQuoteBorder":     u.Surface1,
		"mdHr":              u.Overlay0,
		"mdListBullet":      u.Blue,

		"toolDiffAdded":     u.Green,
		"toolDiffRemoved":   u.Red,
		"toolDiffContext":   u.Subtext0,
		"syntaxComment":     u.Subtext0,
		"syntaxKeyword":     u.Mauve,
		"syntaxFunction":    u.Blue,
		"syntaxVariable":    u.Red,
		"syntaxString":      u.Green,
		"syntaxNumber":      u.Peach,
		"syntaxType":        u.Yellow,
		"syntaxOperator":    u.Teal,
		"syntaxPunctuation": u.Text,

		// The thinking ladder colors the editor border by reasoning level, so it
		// climbs the palette rather than repeating one hue.
		"thinkingOff":     u.Overlay0,
		"thinkingMinimal": u.Overlay1,
		"thinkingLow":     u.Blue,
		"thinkingMedium":  u.Teal,
		"thinkingHigh":    u.Mauve,
		"thinkingXhigh":   u.Peach,
		"thinkingMax":     u.Red,
		"bashMode":        u.Teal,
		"pythonMode":      u.Yellow,

		"statusLineBg":        u.Surface0,
		"statusLineSep":       u.Overlay0,
		"statusLineModel":     u.Mauve,
		"statusLinePath":      u.Blue,
		"statusLineGitClean":  u.Green,
		"statusLineGitDirty":  u.Yellow,
		"statusLineContext":   u.Teal,
		"statusLineSpend":     u.Peach,
		"statusLineStaged":    u.Green,
		"statusLineDirty":     u.Yellow,
		"statusLineUntracked": u.Red,
		"statusLineOutput":    u.Text,
		"statusLineCost":      u.Peach,
		"statusLineSubagents": u.Accent,
	}
}

// ompThemeBody renders rt as an omp custom theme.
func ompThemeBody(rt resolvedTheme) []byte {
	colors := ompColors(rt.ui)
	// A [theme.custom] override reaches us already parsed to a hex, so this is
	// belt and braces — but omp treats a non-hex token as a var reference and
	// throws when it resolves nothing, and a theme that fails to load costs the
	// whole palette (omp falls back to built-in "dark"). So a malformed token
	// takes the same token off the unmodified built-in palette instead of being
	// emitted or, as in opencode's body, dropped: omp requires all of them.
	var base map[string]string
	for tok, v := range colors {
		if _, _, _, ok := hexRGB(v); !ok {
			if base == nil {
				base = ompColors(resolveThemeByName(rt.Resolved).ui)
			}
			colors[tok] = base[tok]
		}
	}
	root := map[string]any{
		"$schema": ompThemeSchema,
		"name":    ompThemeName,
		"colors":  colors,
		"export": map[string]string{
			"pageBg": rt.ui.PanelBg,
			"cardBg": rt.ui.SurfaceDim,
			"infoBg": rt.ui.Surface0,
		},
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil
	}
	return append(out, '\n')
}

// syncOmpThemeFile writes the generated theme into omp's custom themes dir
// (<agent dir>/themes, which omp discovers and watches). Unchanged content is
// left alone: the write is what a running omp reloads on, so a no-op rewrite
// would make every poll tick restyle live sessions for nothing.
func syncOmpThemeFile(b Backend, agentDir string, rt resolvedTheme) error {
	dir := filepath.Join(agentDir, "themes")
	path := filepath.Join(dir, ompThemeName+".json")
	body := ompThemeBody(rt)
	if body == nil {
		return fmt.Errorf("render omp theme %q", rt.Resolved)
	}
	if cur, err := b.ReadFile(path); err == nil && string(cur) == string(body) {
		return nil
	}
	if err := b.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	log.Printf("theme:    omp theme file -> %s (%s) on %s", ompThemeName, rt.Resolved, b.Name())
	return b.WriteFile(path, body, 0o644)
}

// syncOmpThemePin points both of omp's mode-selected theme slots at the
// generated theme, so whichever way its terminal detection lands it loads the
// same palette — and lands on the one name whose file omp watches.
func syncOmpThemePin(b Backend, agentDir string) error {
	path := filepath.Join(agentDir, "config.yml")

	root := map[string]any{}
	data, err := b.ReadFile(path)
	switch {
	case err == nil:
		if yaml.Unmarshal(data, &root) != nil {
			return nil // don't clobber what we can't parse
		}
		if root == nil {
			root = map[string]any{} // an empty/all-comment file decodes to nil
		}
	case errors.Is(err, fs.ErrNotExist):
		// Create it — omp's dir exists, so it has run; a config it hasn't
		// written yet still gets the pin on the next launch.
	default:
		return err
	}

	// A legacy flat `theme: "name"` (omp migrates it on read) is replaced by the
	// nested form rather than merged into.
	theme, _ := root["theme"].(map[string]any)
	if theme == nil {
		theme = map[string]any{}
	}
	dark, _ := theme["dark"].(string)
	light, _ := theme["light"].(string)
	if dark == ompThemeName && light == ompThemeName {
		return nil
	}
	theme["dark"], theme["light"] = ompThemeName, ompThemeName
	root["theme"] = theme

	out, err := yaml.Marshal(root)
	if err != nil {
		return err
	}
	log.Printf("theme:    omp theme pin -> %s on %s", ompThemeName, b.Name())
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
