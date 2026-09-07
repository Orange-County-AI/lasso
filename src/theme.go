// Theme resolution: Luvus owns the theme; lasso only READS it.
//
// The active theme, its metadata and — for an installed/community theme — the
// path to its file all come from one UHP `theme.list` call on the host's own
// session socket. Nothing is guessed from a config file and nothing is ever
// written back: `luvus theme use <id>` is the only way the selection changes,
// so a stale palette in lasso can never override the user's choice.
//
// A Luvus theme (schema 1) is 18 SEMANTIC roles — crust..text, accent, sel_bg,
// the two border tiers, and four hues. It styles Luvus's own chrome, not the 16
// ANSI colors of terminals running inside panes, and UHP exposes no resolved
// color for a bundled theme. So two things happen here:
//
//   - the 17 bundled palettes are transcribed from Luvus's own theme registry
//     (the same data its Settings picker and luvus.dev render), keyed by the id
//     `theme.list` reports;
//   - an installed or community theme is resolved from ITS OWN file — the path
//     `theme.list` hands over — applied on top of its `extends` ancestor, so a
//     custom palette lands verbatim instead of collapsing to its parent.
//
// The 16 ANSI colors are DERIVED from those roles (deriveANSI) rather than
// transcribed per theme: a custom palette has no canonical Alacritty export to
// copy, and a derivation that reads the theme's own hues is the only mapping
// that can serve every theme a user installs.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// luvusPalette is a Luvus theme's 18 semantic color roles (schema 1). Every
// role is a "#rrggbb" once resolved; an empty one means the theme left it to
// the terminal (`reset`) and nothing could fill it.
type luvusPalette struct {
	// Backgrounds, darkest to lightest on a dark theme. Mantle is the pane
	// background — it is what Luvus answers an OSC 11 query with.
	Crust, Mantle, Base, Surface0, Surface1 string
	// Foreground tiers, dimmest to brightest.
	Overlay0, Overlay1, Subtext0, Subtext1, Text string
	// Signature color, selection wash, and the two border tiers.
	Accent, SelBg, Border, BorderFocus string
	// The four hues every theme carries.
	Green, Mint, Amber, Coral string
}

// luvusDefaultTheme is Luvus's bundled default — what it falls back to itself
// (removing the active installed theme switches here first), and therefore the
// palette to stand in for a theme whose colors this build cannot resolve.
const luvusDefaultTheme = "quattro-rally"

// bundledPalettes holds Luvus's 17 built-in palettes, keyed by the id
// `theme.list` reports. It is a VERSION-MATCHED transcription of Luvus's own
// theme registry (0.13.4), and it exists because UHP publishes a bundled
// theme's identity but not its colors: theme.list, config.get, theme.path,
// uhp.capabilities and session.snapshot all carry the active theme's NAME and
// nothing about its palette, and `luvus theme init` writes a fixed starter
// rather than exporting the active one. So there is no runtime source to read.
//
// The values come from the registry Luvus publishes for its own docs site
// (luvus.dev/_astro/themes.*.css, "the colours come straight from luvus's own
// registry"), cross-checked against the installed binary. The consequence to
// know: a new bundled ID is unresolved until this table is refreshed. Lasso
// preserves the last good palette and skips downstream writes in that case.
// Installed/community themes carry their own colors and are read from their file.
var bundledPalettes = map[string]luvusPalette{
	"quattro-rally": {
		Crust: "#181926", Mantle: "#1e2030", Base: "#24273a", Surface0: "#363a4f", Surface1: "#494d64",
		Overlay0: "#6e738d", Overlay1: "#8087a2", Subtext0: "#a5adcb", Subtext1: "#b8c0e0", Text: "#cad3f5",
		Accent: "#dbc66f", SelBg: "#3a3416", Border: "#5a5130", BorderFocus: "#9a8a50",
		Green: "#94a143", Mint: "#b8cf6a", Amber: "#e0a154", Coral: "#cf5a44",
	},
	"noir": {
		Crust: "#070709", Mantle: "#111116", Base: "#202028", Surface0: "#1a1a20", Surface1: "#25252d",
		Overlay0: "#4a4a54", Overlay1: "#686873", Subtext0: "#93939f", Subtext1: "#b6b6c0", Text: "#e7e7ed",
		Accent: "#c6ff1a", SelBg: "#33450e", Border: "#383840", BorderFocus: "#8c8c96",
		Green: "#8fbc7a", Mint: "#6fc6a3", Amber: "#e09a4d", Coral: "#e06c66",
	},
	"ocean": {
		Crust: "#02102a", Mantle: "#061d42", Base: "#0c2e5e", Surface0: "#05224a", Surface1: "#123a6e",
		Overlay0: "#355886", Overlay1: "#5276a4", Subtext0: "#8fa8c8", Subtext1: "#b8cce6", Text: "#e8f2ff",
		Accent: "#46c6ff", SelBg: "#123e76", Border: "#2a4e80", BorderFocus: "#5a86c0",
		Green: "#6fcf97", Mint: "#4fd6c8", Amber: "#f2c14e", Coral: "#f06a6a",
	},
	"dracula": {
		Crust: "#1a1b23", Mantle: "#282a36", Base: "#333647", Surface0: "#21222c", Surface1: "#3c3f52",
		Overlay0: "#565869", Overlay1: "#6272a4", Subtext0: "#a0a4c0", Subtext1: "#cccee0", Text: "#f8f8f2",
		Accent: "#bd93f9", SelBg: "#44475a", Border: "#44475a", BorderFocus: "#6272a4",
		Green: "#50fa7b", Mint: "#8be9fd", Amber: "#ffb86c", Coral: "#ff5555",
	},
	"nord": {
		Crust: "#242933", Mantle: "#2e3440", Base: "#3b4252", Surface0: "#292f3a", Surface1: "#434c5e",
		Overlay0: "#4c566a", Overlay1: "#616e88", Subtext0: "#b0b8c8", Subtext1: "#d8dee9", Text: "#eceff4",
		Accent: "#88c0d0", SelBg: "#3b4a5e", Border: "#434c5e", BorderFocus: "#6a7690",
		Green: "#a3be8c", Mint: "#8fbcbb", Amber: "#ebcb8b", Coral: "#bf616a",
	},
	"sky": {
		Crust: "#f5f8fc", Mantle: "#ebf1f9", Base: "#e1eaf5", Surface0: "#d6e1f0", Surface1: "#c4d3e8",
		Overlay0: "#93a4bd", Overlay1: "#74849e", Subtext0: "#566579", Subtext1: "#42505f", Text: "#2f3a48",
		Accent: "#2477c7", SelBg: "#cbe2f7", Border: "#c0cde0", BorderFocus: "#6a7a92",
		Green: "#3f8f56", Mint: "#1c8f88", Amber: "#c07d12", Coral: "#cc3b52",
	},
	"catppuccin-mocha": {
		Crust: "#11111b", Mantle: "#181825", Base: "#1e1e2e", Surface0: "#313244", Surface1: "#45475a",
		Overlay0: "#6c7086", Overlay1: "#7f849c", Subtext0: "#a6adc8", Subtext1: "#bac2de", Text: "#cdd6f4",
		Accent: "#cba6f7", SelBg: "#3f3359", Border: "#45475a", BorderFocus: "#7f849c",
		Green: "#a6e3a1", Mint: "#94e2d5", Amber: "#fab387", Coral: "#f38ba8",
	},
	"catppuccin-macchiato": {
		Crust: "#181926", Mantle: "#1e2030", Base: "#24273a", Surface0: "#363a4f", Surface1: "#494d64",
		Overlay0: "#6e738d", Overlay1: "#8087a2", Subtext0: "#a5adcb", Subtext1: "#b8c0e0", Text: "#cad3f5",
		Accent: "#c6a0f6", SelBg: "#3b3254", Border: "#494d64", BorderFocus: "#8087a2",
		Green: "#a6da95", Mint: "#8bd5ca", Amber: "#f5a97f", Coral: "#ed8796",
	},
	"catppuccin-frappe": {
		Crust: "#232634", Mantle: "#292c3c", Base: "#303446", Surface0: "#414559", Surface1: "#51576d",
		Overlay0: "#737994", Overlay1: "#838ba7", Subtext0: "#a5adce", Subtext1: "#b5bfe2", Text: "#c6d0f5",
		Accent: "#ca9ee6", SelBg: "#463e5e", Border: "#51576d", BorderFocus: "#838ba7",
		Green: "#a6d189", Mint: "#81c8be", Amber: "#ef9f76", Coral: "#e78284",
	},
	"gruvbox": {
		Crust: "#1d2021", Mantle: "#282828", Base: "#3c3836", Surface0: "#32302f", Surface1: "#504945",
		Overlay0: "#665c54", Overlay1: "#7c6f64", Subtext0: "#a89984", Subtext1: "#bdae93", Text: "#ebdbb2",
		Accent: "#fabd2f", SelBg: "#453d21", Border: "#504945", BorderFocus: "#928374",
		Green: "#b8bb26", Mint: "#8ec07c", Amber: "#fe8019", Coral: "#fb4934",
	},
	"sunset": {
		Crust: "#160a1e", Mantle: "#221030", Base: "#2e1640", Surface0: "#1c0d2a", Surface1: "#3a1d50",
		Overlay0: "#5a3a70", Overlay1: "#7a5a90", Subtext0: "#b89ad0", Subtext1: "#d6bce8", Text: "#f6e8ff",
		Accent: "#ff5fd0", SelBg: "#4a2466", Border: "#4a2a64", BorderFocus: "#8a5aa8",
		Green: "#5fe0a8", Mint: "#5fd6e0", Amber: "#ffb54f", Coral: "#ff5f8f",
	},
	"homebrew": {
		Crust: "#000000", Mantle: "#040a04", Base: "#081608", Surface0: "#061006", Surface1: "#0e240e",
		Overlay0: "#1e4c1e", Overlay1: "#2e6c2e", Subtext0: "#22b422", Subtext1: "#2ee02e", Text: "#3cff3c",
		Accent: "#00ff41", SelBg: "#0a3a0a", Border: "#1a4e1a", BorderFocus: "#2ea82e",
		Green: "#35e035", Mint: "#5effb0", Amber: "#c8ff3c", Coral: "#ff6050",
	},
	"grass": {
		Crust: "#052012", Mantle: "#0a361d", Base: "#104c28", Surface0: "#082c18", Surface1: "#155830",
		Overlay0: "#367850", Overlay1: "#54966c", Subtext0: "#a8c898", Subtext1: "#c8e0ae", Text: "#fff0a5",
		Accent: "#bee632", SelBg: "#1a6c3a", Border: "#287648", BorderFocus: "#56a66c",
		Green: "#8fd07a", Mint: "#5ed6b0", Amber: "#f2c84e", Coral: "#f06a5a",
	},
	"redsands": {
		Crust: "#1f0a06", Mantle: "#38120c", Base: "#4e1c12", Surface0: "#2c0e08", Surface1: "#5c261a",
		Overlay0: "#8a4a38", Overlay1: "#a86854", Subtext0: "#c89a80", Subtext1: "#dcba9c", Text: "#f2daba",
		Accent: "#ff8a3c", SelBg: "#702e1e", Border: "#7c3c2a", BorderFocus: "#ba6e4c",
		Green: "#a6bf5e", Mint: "#5ec8a8", Amber: "#ffb454", Coral: "#ff6a5a",
	},
	"catppuccin-latte": {
		Crust: "#eff1f5", Mantle: "#e6e9ef", Base: "#dce0e8", Surface0: "#ccd0da", Surface1: "#bcc0cc",
		Overlay0: "#9ca0b0", Overlay1: "#7c8090", Subtext0: "#6c6f85", Subtext1: "#50526c", Text: "#4c4f69",
		Accent: "#40a02b", SelBg: "#c6e8a8", Border: "#acb0be", BorderFocus: "#7c8090",
		Green: "#40a02b", Mint: "#179299", Amber: "#df8e1d", Coral: "#d20f39",
	},
	"gruvbox-light": {
		Crust: "#fbf1c7", Mantle: "#f2e5bc", Base: "#ebdbb2", Surface0: "#d5c4a1", Surface1: "#bdae93",
		Overlay0: "#a89984", Overlay1: "#928374", Subtext0: "#665c54", Subtext1: "#504945", Text: "#3c3836",
		Accent: "#af3a03", SelBg: "#e0c68a", Border: "#bdae93", BorderFocus: "#7c6f64",
		Green: "#79740e", Mint: "#427b58", Amber: "#b57614", Coral: "#9d0006",
	},
	"mono": {
		Crust: "#070707", Mantle: "#121212", Base: "#1e1e1e", Surface0: "#181818", Surface1: "#282828",
		Overlay0: "#4a4a4a", Overlay1: "#686868", Subtext0: "#939393", Subtext1: "#b6b6b6", Text: "#ececec",
		Accent: "#eaeaea", SelBg: "#333333", Border: "#525252", BorderFocus: "#909090",
		Green: "#828282", Mint: "#a6a6a6", Amber: "#c8c8c8", Coral: "#e6e6e6",
	},
}

// neutralPalette is deliberately NOT one of Luvus's: it is what lasso paints
// before it has ever managed to read a theme (lasso starts before ttyd
// autostarts Luvus). Standing in a real bundled palette there would state a
// selection the user may not have made, so this is a plain slate that reads as
// "not yet known" — see unavailableTheme.
var neutralPalette = luvusPalette{
	Crust: "#101114", Mantle: "#16171b", Base: "#1c1e23", Surface0: "#22242a", Surface1: "#2c2f36",
	Overlay0: "#4b4f59", Overlay1: "#666b77", Subtext0: "#9096a1", Subtext1: "#b4bac4", Text: "#dfe3ea",
	Accent: "#6f9dc4", SelBg: "#25303a", Border: "#31353d", BorderFocus: "#7f8794",
	Green: "#7fa76a", Mint: "#6bab9c", Amber: "#c8a15c", Coral: "#c2685f",
}

// ansiPalette is the 16-color terminal palette lasso hands to xterm.js and
// ghostty. (The selection highlight is derived from the accent, not stored
// here — see xtermJSON.)
type ansiPalette struct {
	Black, Red, Green, Yellow, Blue, Magenta, Cyan     string
	White                                              string
	BrightBlack, BrightRed, BrightGreen, BrightYellow  string
	BrightBlue, BrightMagenta, BrightCyan, BrightWhite string
}

// resolvedTheme is the theme lasso is painting: what Luvus says is active, plus
// the concrete palette that resolves to.
//
// Every field is semantic state a caller may compare for equality to decide
// "has the theme changed", so nothing incidental belongs here. In particular
// theme.list's `revision` is deliberately NOT kept: it is the server's GLOBAL
// session revision, which a workspace.open or tab.new moves without any theme
// change — carrying it would make every pane mutation look like a new palette
// and re-sync the whole fleet.
type resolvedTheme struct {
	Name       string // active Luvus theme id ("" when unavailable)
	Label      string // Luvus's display_name
	Appearance string // "dark", "light" or "terminal"
	Resolved   string // id of the palette actually used (an ancestor, for a child theme)
	Customized bool   // a theme FILE contributed colors (installed/community theme)
	Available  bool   // Luvus answered; false means this is a stand-in, do not sync it

	p    luvusPalette
	ansi ansiPalette
}

// unavailableTheme is the stand-in for "Luvus has not answered yet". It carries
// a complete, valid palette so every surface renders, and an empty Name/Resolved
// so no caller mistakes it for a selection or writes it to a host.
func unavailableTheme() resolvedTheme {
	rt := resolvedTheme{p: neutralPalette}
	rt.ansi = deriveANSI(rt.p, false)
	return rt
}

// bundledTheme resolves one of Luvus's built-in palettes by id, falling back to
// the bundled default. Used for the omp fallback (a malformed token must not
// cost the whole palette) and by tests.
func bundledTheme(id string) resolvedTheme {
	p, ok := bundledPalettes[id]
	if !ok {
		id, p = luvusDefaultTheme, bundledPalettes[luvusDefaultTheme]
	}
	rt := resolvedTheme{Name: id, Label: id, Resolved: id, Available: true, p: p}
	rt.Appearance = "dark"
	if luminance(p.Mantle) > 0.5 {
		rt.Appearance = "light"
	}
	rt.ansi = deriveANSI(p, rt.light())
	return rt
}

// light reports whether this is a light palette. Luvus's own `appearance` is
// authoritative when it says so; the virtual Terminal theme reports neither, so
// the background decides.
func (rt resolvedTheme) light() bool {
	switch rt.Appearance {
	case "light":
		return true
	case "dark":
		return false
	}
	return luminance(rt.p.Mantle) > 0.5
}

// fingerprint identifies the exact palette lasso would write to a host. It
// covers the colors, not just the name: an installed theme's file can be edited
// and reinstalled under the same id, and a convergence check keyed on the name
// alone would call every host up to date while they all render the old palette.
func (rt resolvedTheme) fingerprint() string {
	if !rt.Available {
		return ""
	}
	return rt.Name + "|" + fmt.Sprintf("%v", rt.p)
}

// ---------------------------------------------------------------------------
// theme.list: what Luvus says is active
// ---------------------------------------------------------------------------

// luvusThemeRow is one row of `theme.list`. Source is an enum: the strings
// "built_in" and "virtual", or an object {"local":{"path":…}} whose path is the
// installed theme's file ON THAT HOST — which is why nothing here needs
// `theme.path` or a guess at $LUVUS_HOME.
type luvusThemeRow struct {
	ID          string          `json:"id"`
	DisplayName string          `json:"display_name"`
	Appearance  string          `json:"appearance"`
	Extends     string          `json:"extends"`
	Active      bool            `json:"active"`
	Source      json.RawMessage `json:"source"`
}

// localPath returns the row's theme file path, or "" for a bundled/virtual one.
func (r luvusThemeRow) localPath() string {
	var obj struct {
		Local struct {
			Path string `json:"path"`
		} `json:"local"`
	}
	if json.Unmarshal(r.Source, &obj) != nil {
		return "" // "built_in" / "virtual" — a bare string, not an object
	}
	return obj.Local.Path
}

// virtual reports the client-derived Terminal theme, which carries no palette
// of its own by design (as opposed to one lasso merely failed to resolve).
func (r luvusThemeRow) virtual() bool {
	var s string
	return json.Unmarshal(r.Source, &s) == nil && s == "virtual"
}

type luvusThemeList struct {
	Themes []luvusThemeRow `json:"themes"`
}

// errNoActiveTheme means Luvus answered but named no active theme, which no
// healthy server does. Kept distinct from a transport failure so a caller can
// tell "cannot reach Luvus" from "Luvus is confused".
var errNoActiveTheme = errors.New("luvus reported no active theme")

// loadLuvusTheme resolves the theme from the DEFAULT host's Luvus — the one
// lasso booted on. The theme is a property of this lasso, not of whichever host
// a tab is looking at (see the per-tab hosts note in CLAUDE.md), so this is
// deliberately not per-request.
func loadLuvusTheme() (resolvedTheme, error) {
	return loadLuvusThemeFrom(defaultBackend())
}

// loadLuvusThemeFrom resolves the active theme on one host. It returns an error
// rather than a fallback palette: the caller keeps its last good theme and
// skips syncing, because painting a stand-in — or worse, writing it into every
// host's agent config — would misreport the user's selection.
func loadLuvusThemeFrom(b Backend) (resolvedTheme, error) {
	if b == nil {
		return unavailableTheme(), errors.New("no backend to read the theme from")
	}
	raw, err := b.LuvusCall("theme.list", map[string]any{})
	if err != nil {
		return unavailableTheme(), fmt.Errorf("theme.list: %w", err)
	}
	var list luvusThemeList
	if err := json.Unmarshal(raw, &list); err != nil {
		return unavailableTheme(), fmt.Errorf("theme.list: %w", err)
	}
	var active luvusThemeRow
	for _, row := range list.Themes {
		if row.Active {
			active = row
			break
		}
	}
	if active.ID == "" {
		return unavailableTheme(), errNoActiveTheme
	}

	rt := resolvedTheme{
		Name:       active.ID,
		Label:      active.DisplayName,
		Appearance: active.Appearance,
		Available:  true,
	}
	var resolveErr error
	rt.p, rt.Resolved, rt.Customized, resolveErr = resolveLuvusPalette(b, list.Themes, active)
	if resolveErr != nil {
		return unavailableTheme(), resolveErr
	}
	if rt.Label == "" {
		rt.Label = rt.Name
	}
	rt.ansi = deriveANSI(rt.p, rt.light())
	return rt, nil
}

// maxThemeExtends is Luvus's own inheritance depth limit (documented as eight
// levels), which also bounds the walk below against a cycle its validator would
// have rejected but a hand-copied file may still contain.
const maxThemeExtends = 8

// resolveLuvusPalette resolves a row's palette, returning it with the id of the
// palette it is BASED on and whether a theme file contributed colors.
//
// A bundled theme is a table lookup. An installed one is its file's [colors]
// over its ancestor's palette, walked through the rows `theme.list` already
// returned — so the whole chain costs one ReadFile per installed ancestor and
// no directory scan. The virtual Terminal theme has no colors of its own (it is
// client-derived from the host terminal, which lasso's embedded terminals are
// not), so it resolves to Luvus's bundled fallback exactly as Luvus does for a
// `reset` role it cannot probe.
func resolveLuvusPalette(b Backend, rows []luvusThemeRow, row luvusThemeRow) (luvusPalette, string, bool, error) {
	byID := make(map[string]luvusThemeRow, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}

	// Collect the child-first chain of installed themes down to an ancestor
	// whose palette is known (bundled), or to a complete installed theme.
	var chain []map[string]string
	seen := map[string]bool{}
	cur := row
	base, baseID := luvusPalette{}, ""
	for depth := 0; ; depth++ {
		if cur.ID == "" || seen[cur.ID] || depth > maxThemeExtends {
			return luvusPalette{}, "", false, fmt.Errorf("invalid theme inheritance for %q", row.ID)
		}
		seen[cur.ID] = true
		path := cur.localPath()
		if path == "" {
			if p, ok := bundledPalettes[cur.ID]; ok {
				base, baseID = p, cur.ID
				break
			}
			if cur.virtual() {
				break
			}
			return luvusPalette{}, "", false, fmt.Errorf("unresolved Luvus palette %q", cur.ID)
		}
		data, err := b.ReadFile(path)
		if err != nil {
			return luvusPalette{}, "", false, fmt.Errorf("read Luvus theme %q: %w", cur.ID, err)
		}
		colors, extends, err := parseLuvusThemeTOML(data)
		if err != nil {
			return luvusPalette{}, "", false, fmt.Errorf("parse Luvus theme %q: %w", cur.ID, err)
		}
		chain = append(chain, colors)
		if extends == "" {
			extends = cur.Extends
		}
		if extends == "" {
			break
		}
		next, ok := byID[extends]
		if !ok {
			next = luvusThemeRow{ID: extends}
		}
		cur = next
	}

	// Apply the chain parent-first so a child's override wins.
	custom := false
	for i := len(chain) - 1; i >= 0; i-- {
		for role, rawVal := range chain[i] {
			hex, ok := parseThemeColor(rawVal)
			if !ok {
				continue // `reset`, or unparseable: the ancestor's role stands
			}
			if base.applyRole(role, hex) {
				custom = true
			}
		}
	}
	if baseID != "" {
		return base, baseID, custom, nil
	}

	// No bundled ancestor. Either the files covered all 18 roles — a complete
	// installed theme, which genuinely IS its own palette — or they did not,
	// and Luvus's bundled default backs whatever is left, which is what Luvus
	// itself falls back to. The distinction has to reach Resolved: reporting a
	// half-filled palette under the theme's own name would state that lasso is
	// painting a theme it is mostly not.
	if missing := base.backfill(bundledPalettes[luvusDefaultTheme]); missing == 0 && custom {
		return base, row.ID, custom, nil
	}
	return base, luvusDefaultTheme, custom, nil
}

// backfill copies every role donor has and p does not, returning how many it
// had to fill.
func (p *luvusPalette) backfill(donor luvusPalette) int {
	n := 0
	for _, role := range themeRoles {
		if slot := p.roleRef(role); *slot == "" {
			*slot = *donor.roleRef(role)
			n++
		}
	}
	return n
}

// themeRoles is schema 1's role set, in the order a theme file declares them.
var themeRoles = []string{
	"crust", "mantle", "base", "surface0", "surface1",
	"overlay0", "overlay1", "subtext0", "subtext1", "text",
	"accent", "sel_bg", "border", "border_focus",
	"green", "mint", "amber", "coral",
}

// roleRef addresses one role by its schema-1 name, or nil for a name lasso does
// not know — a later schema may add one, and the rest of the palette is still
// correct, so an unknown role is ignored rather than refused.
func (p *luvusPalette) roleRef(role string) *string {
	switch role {
	case "crust":
		return &p.Crust
	case "mantle":
		return &p.Mantle
	case "base":
		return &p.Base
	case "surface0":
		return &p.Surface0
	case "surface1":
		return &p.Surface1
	case "overlay0":
		return &p.Overlay0
	case "overlay1":
		return &p.Overlay1
	case "subtext0":
		return &p.Subtext0
	case "subtext1":
		return &p.Subtext1
	case "text":
		return &p.Text
	case "accent":
		return &p.Accent
	case "sel_bg":
		return &p.SelBg
	case "border":
		return &p.Border
	case "border_focus":
		return &p.BorderFocus
	case "green":
		return &p.Green
	case "mint":
		return &p.Mint
	case "amber":
		return &p.Amber
	case "coral":
		return &p.Coral
	}
	return nil
}

// applyRole overwrites one role, reporting whether the name was one lasso knows.
func (p *luvusPalette) applyRole(role, hex string) bool {
	slot := p.roleRef(role)
	if slot == nil {
		return false
	}
	*slot = hex
	return true
}

// ---------------------------------------------------------------------------
// parseLuvusThemeTOML uses the TOML grammar Luvus accepts, including literal
// strings, quoted keys, inline tables and Unicode escapes.
func parseLuvusThemeTOML(data []byte) (map[string]string, string, error) {
	var file struct {
		Schema  int               `toml:"schema"`
		Extends string            `toml:"extends"`
		Colors  map[string]string `toml:"colors"`
	}
	if err := toml.Unmarshal(data, &file); err != nil {
		return nil, "", err
	}
	if file.Schema != 0 && file.Schema != 1 {
		return nil, "", fmt.Errorf("unsupported theme schema %d", file.Schema)
	}
	return file.Colors, file.Extends, nil
}

// ---------------------------------------------------------------------------
// parseThemeColor: schema 1 accepts exactly three forms.
// ---------------------------------------------------------------------------

// parseThemeColor resolves a schema-1 color to "#rrggbb". ok=false means the
// value names no color of its own — `reset` (defer to the terminal) or a
// malformed value — and the role it was written to must keep what it inherited.
func parseThemeColor(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "" || s == "reset":
		return "", false
	case strings.HasPrefix(s, "#"):
		if _, _, _, ok := hexRGB(s); ok {
			return s, true
		}
		return "", false
	case strings.HasPrefix(s, "ansi(") && strings.HasSuffix(s, ")"):
		n, err := strconv.Atoi(strings.TrimSpace(s[5 : len(s)-1]))
		if err != nil || n < 0 || n > 255 {
			return "", false
		}
		return ansi256Hex(n), true
	}
	return "", false
}

// ansi256Hex renders an xterm 256-color index. A theme asking for ansi(0..15)
// means "whatever the terminal calls color N", which lasso's own embedded
// terminals cannot answer for; xterm's classic defaults are the honest stand-in.
func ansi256Hex(n int) string {
	switch {
	case n < 16:
		return [16]string{
			"#000000", "#800000", "#008000", "#808000", "#000080", "#800080", "#008080", "#c0c0c0",
			"#808080", "#ff0000", "#00ff00", "#ffff00", "#0000ff", "#ff00ff", "#00ffff", "#ffffff",
		}[n]
	case n < 232:
		steps := [6]int{0, 95, 135, 175, 215, 255}
		n -= 16
		return fmt.Sprintf("#%02x%02x%02x", steps[n/36], steps[n/6%6], steps[n%6])
	default:
		v := 8 + (n-232)*10
		return fmt.Sprintf("#%02x%02x%02x", v, v, v)
	}
}

// ---------------------------------------------------------------------------
// ANSI derivation
// ---------------------------------------------------------------------------

// deriveANSI builds the 16 terminal colors from a theme's semantic roles.
//
// It is a derivation rather than a per-theme transcription because a Luvus theme
// has no ANSI palette at all — Luvus renders its own chrome and leaves the 16
// colors to the host terminal — and because a palette a user just wrote in the
// Theme Maker has no canonical export to copy. Reading the theme's own hues is
// the only mapping that serves every theme, including the greyscale ones (mono
// derives grey blues, which is what a monochrome theme should look like).
//
//   - red/green/yellow/cyan are the theme's four hues verbatim;
//   - blue and magenta are the two hues schema 1 has no role for, synthesized at
//     their canonical angles with the SATURATION AND LIGHTNESS this theme uses
//     for its own hues, so they belong to the palette instead of being imported
//     from somewhere else;
//   - black/white are the two neutral tiers, ordered by luminance so a light
//     theme does not end up with a white "black";
//   - the brights are each color pushed toward the theme's text color, i.e.
//     toward more contrast against the background — which is what "bright"
//     means on a light theme too.
func deriveANSI(p luvusPalette, light bool) ansiPalette {
	sat, lum := chromaProfile(p)
	blue := hslHex(214, sat, lum)
	magenta := hslHex(300, sat, lum)

	black, white := p.Surface1, p.Subtext1
	if luminance(black) > luminance(white) {
		black, white = white, black
	}
	dim := p.Overlay1
	if light {
		dim = p.Overlay0
	}

	bright := func(hex string) string { return blendHex(hex, p.Text, ansiBrightLift) }
	return ansiPalette{
		Black: black, Red: p.Coral, Green: p.Green, Yellow: p.Amber,
		Blue: blue, Magenta: magenta, Cyan: p.Mint, White: white,
		BrightBlack: dim, BrightRed: bright(p.Coral), BrightGreen: bright(p.Green),
		BrightYellow: bright(p.Amber), BrightBlue: bright(blue),
		BrightMagenta: bright(magenta), BrightCyan: bright(p.Mint), BrightWhite: p.Text,
	}
}

// ansiBrightLift is how far a bright color is pushed toward the theme's text
// color. Enough to read as a distinct second tier beside its normal, small
// enough that eight brights don't collapse into the foreground.
const ansiBrightLift = 0.28

// chromaProfile averages the saturation and lightness of the four hue roles, so
// a synthesized color sits at the same intensity as the ones the theme chose.
// A palette whose hues are all grey yields a grey profile, by design.
func chromaProfile(p luvusPalette) (sat, lum float64) {
	n := 0
	for _, hex := range []string{p.Green, p.Mint, p.Amber, p.Coral} {
		_, s, l, ok := hexHSL(hex)
		if !ok {
			continue
		}
		sat, lum, n = sat+s, lum+l, n+1
	}
	if n == 0 {
		return 0.5, 0.6
	}
	return sat / float64(n), lum / float64(n)
}

// hexHSL converts "#rrggbb" to hue (degrees), saturation and lightness (0..1).
func hexHSL(hex string) (h, s, l float64, ok bool) {
	ri, gi, bi, ok := hexRGB(hex)
	if !ok {
		return 0, 0, 0, false
	}
	r, g, b := float64(ri)/255, float64(gi)/255, float64(bi)/255
	max, min := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	l = (max + min) / 2
	d := max - min
	if d == 0 {
		return 0, 0, l, true
	}
	s = d / (1 - math.Abs(2*l-1))
	switch max {
	case r:
		h = math.Mod((g-b)/d, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	h *= 60
	if h < 0 {
		h += 360
	}
	return h, s, l, true
}

// hslHex is hexHSL's inverse.
func hslHex(h, s, l float64) string {
	h = math.Mod(math.Mod(h, 360)+360, 360)
	s, l = clamp01(s), clamp01(l)
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := l - c/2
	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	to := func(v float64) int { return int(math.Round(clamp01(v+m) * 255)) }
	return fmt.Sprintf("#%02x%02x%02x", to(r), to(g), to(b))
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// ---------------------------------------------------------------------------
// rendering
// ---------------------------------------------------------------------------

// termSelectionAlpha is the opacity of the terminal's selection/highlight tint.
// Selection is a *translucent* wash of the theme accent (see xtermJSON): unlike
// an opaque color it composites over whatever cell content is underneath, so it
// stays visible on every theme instead of vanishing when the theme's own sel_bg
// sits a shade off the background. ~40% reads as a clear band while the text
// below stays legible.
const termSelectionAlpha = 0x66

// xtermJSON builds an xterm.js ITheme: chrome from the Luvus roles (mantle is
// the pane background Luvus itself reports over OSC 11, so the iframe blends
// with Luvus), a translucent-accent selection highlight (theme-matched yet
// always visible — see termSelectionAlpha), and the derived 16 ANSI colors.
func (rt resolvedTheme) xtermJSON() string {
	a := rt.ansi
	return `{` +
		q("background", rt.p.Mantle) + "," + q("foreground", rt.p.Text) + "," +
		q("cursor", rt.p.Text) + "," + q("cursorAccent", rt.p.Mantle) + "," +
		q("selectionBackground", rgba(rt.p.Accent, termSelectionAlpha)) + "," +
		q("black", a.Black) + "," + q("red", a.Red) + "," + q("green", a.Green) + "," +
		q("yellow", a.Yellow) + "," + q("blue", a.Blue) + "," + q("magenta", a.Magenta) + "," +
		q("cyan", a.Cyan) + "," + q("white", a.White) + "," +
		q("brightBlack", a.BrightBlack) + "," + q("brightRed", a.BrightRed) + "," +
		q("brightGreen", a.BrightGreen) + "," + q("brightYellow", a.BrightYellow) + "," +
		q("brightBlue", a.BrightBlue) + "," + q("brightMagenta", a.BrightMagenta) + "," +
		q("brightCyan", a.BrightCyan) + "," + q("brightWhite", a.BrightWhite) + `}`
}

func q(k, v string) string { return `"` + k + `":"` + v + `"` }

// peach is the warm tier between amber and coral. Schema 1 has no orange role,
// and the agent themes want one distinct from their warning color, so it is
// mixed from the two hues that bracket it instead of repeating amber twice.
func (rt resolvedTheme) peach() string { return blendHex(rt.p.Amber, rt.p.Coral, 0.35) }

// mauve is the palette's purple tier — the synthesized magenta, which is a
// theme-consistent stand-in for the role schema 1 does not carry (used for
// directories, headings and keywords).
func (rt resolvedTheme) mauve() string { return rt.ansi.Magenta }

// cssVars renders the :root custom-property declarations for the sidebar.
// --accent is the theme's own Accent role (the signature color that drives
// --primary in the chrome), so e.g. ocean reads cyan and quattro-rally gold.
// --good stays on Mint since it is the success color, independent of the accent.
// --muted is the secondary-text tier (form labels, metadata), so it maps to
// Subtext0, not Overlay0 — Overlay0 is the dimmest "subtle line/disabled" tier
// and reads at ~2:1 against the panel, too low for labels.
func (rt resolvedTheme) cssVars() string {
	p := rt.p
	var b strings.Builder
	put := func(name, val string) { fmt.Fprintf(&b, "    %s: %s;\n", name, val) }
	put("--bg", p.Mantle)
	put("--panel", p.Surface0)
	put("--border", p.Border)
	put("--hover", p.Surface1)
	put("--fg", p.Text)
	put("--muted", p.Subtext0)
	put("--accent", p.Accent)
	put("--accent-dim", rgba(p.Accent, 0x26))
	put("--dir", rt.mauve())
	put("--good", p.Mint)
	put("--warn", p.Amber)
	put("--bad", p.Coral)
	return b.String()
}

// rgba appends an 8-bit alpha to a #rrggbb hex (-> #rrggbbaa); passes other
// formats through unchanged.
func rgba(hex string, alpha int) string {
	if len(hex) == 7 && hex[0] == '#' {
		return fmt.Sprintf("%s%02x", hex, alpha&0xff)
	}
	return hex
}
