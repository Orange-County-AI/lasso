// Theme resolution: mirror herdr's own theming so the UI adapts to whichever
// theme the user has selected in ~/.config/herdr/config.toml — instead of
// hardcoding one palette.
//
// herdr has no socket method that exposes the resolved theme, so we read the
// config the same way it does: pick a built-in theme by name, apply any
// [theme.custom] per-token overrides, then the legacy [ui].accent override.
//
// herdr's theme is a 16-token *UI* palette (catppuccin-style names) — it styles
// herdr's own chrome, NOT the 16 ANSI colors of terminals running inside panes.
// So we use two derived things:
//
//   - the sidebar/file-viewer CSS  <- herdr's UI tokens (matches herdr's chrome)
//   - the embedded terminal theme  <- bg/fg/cursor from the UI tokens (so the
//     iframe blends with herdr) + the *canonical* 16-color ANSI palette for
//     that scheme (so colored output inside the terminal looks right) + a
//     translucent-accent selection highlight (visible on every theme — see
//     termSelectionAlpha).
//
// UI token values are transcribed verbatim from herdr's src/app/state.rs
// (v0.6.4); ANSI palettes from each scheme's canonical Alacritty/iTerm export.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// uiPalette is herdr's 16-token UI palette (see src/app/state.rs `Palette`).
type uiPalette struct {
	Accent, PanelBg, Surface0, Surface1, SurfaceDim, Overlay0, Overlay1 string
	Text, Subtext0, Mauve, Green, Yellow, Red, Blue, Teal, Peach        string
}

// ansiPalette is a canonical 16-color terminal palette. (The terminal's
// selection highlight is derived from the UI accent, not stored here — see
// xtermJSON.)
type ansiPalette struct {
	Black, Red, Green, Yellow, Blue, Magenta, Cyan     string
	White                                              string
	BrightBlack, BrightRed, BrightGreen, BrightYellow  string
	BrightBlue, BrightMagenta, BrightCyan, BrightWhite string
}

type themeDef struct {
	ui   uiPalette
	ansi ansiPalette
}

// themes is keyed by canonical (normalized) herdr theme name.
var themes = map[string]themeDef{
	"catppuccin": {
		ui: uiPalette{
			Accent: "#89b4fa", PanelBg: "#181825", Surface0: "#313244", Surface1: "#45475a",
			SurfaceDim: "#1e1e2e", Overlay0: "#6c7086", Overlay1: "#7f849c", Text: "#cdd6f4",
			Subtext0: "#a6adc8", Mauve: "#cba6f7", Green: "#a6e3a1", Yellow: "#f9e2af",
			Red: "#f38ba8", Blue: "#89b4fa", Teal: "#94e2d5", Peach: "#fab387",
		},
		ansi: ansiPalette{
			Black: "#45475a", Red: "#f38ba8", Green: "#a6e3a1", Yellow: "#f9e2af",
			Blue: "#89b4fa", Magenta: "#f5c2e7", Cyan: "#94e2d5", White: "#a6adc8",
			BrightBlack: "#585b70", BrightRed: "#f37799", BrightGreen: "#89d88b", BrightYellow: "#ebd391",
			BrightBlue: "#74a8fc", BrightMagenta: "#f2aede", BrightCyan: "#6bd7ca", BrightWhite: "#bac2de",
		},
	},
	"tokyo-night": {
		ui: uiPalette{
			Accent: "#7aa2f7", PanelBg: "#1a1b26", Surface0: "#24283b", Surface1: "#414868",
			SurfaceDim: "#1a1b26", Overlay0: "#565f89", Overlay1: "#697196", Text: "#c0caf5",
			Subtext0: "#a9b1d6", Mauve: "#bb9af7", Green: "#9ece6a", Yellow: "#e0af68",
			Red: "#f7768e", Blue: "#7aa2f7", Teal: "#7dcfff", Peach: "#ff9e64",
		},
		ansi: ansiPalette{
			Black: "#15161e", Red: "#f7768e", Green: "#9ece6a", Yellow: "#e0af68",
			Blue: "#7aa2f7", Magenta: "#bb9af7", Cyan: "#7dcfff", White: "#a9b1d6",
			BrightBlack: "#414868", BrightRed: "#ff899d", BrightGreen: "#9fe044", BrightYellow: "#faba4a",
			BrightBlue: "#8db0ff", BrightMagenta: "#c7a9ff", BrightCyan: "#a4daff", BrightWhite: "#c0caf5",
		},
	},
	"dracula": {
		ui: uiPalette{
			Accent: "#bd93f9", PanelBg: "#282a36", Surface0: "#44475a", Surface1: "#6272a4",
			SurfaceDim: "#282a36", Overlay0: "#6272a4", Overlay1: "#828cb4", Text: "#f8f8f2",
			Subtext0: "#d2d2dc", Mauve: "#ff79c6", Green: "#50fa7b", Yellow: "#f1fa8c",
			Red: "#ff5555", Blue: "#8be9fd", Teal: "#8be9fd", Peach: "#ffb86c",
		},
		ansi: ansiPalette{
			Black: "#21222c", Red: "#ff5555", Green: "#50fa7b", Yellow: "#f1fa8c",
			Blue: "#bd93f9", Magenta: "#ff79c6", Cyan: "#8be9fd", White: "#f8f8f2",
			BrightBlack: "#6272a4", BrightRed: "#ff6e6e", BrightGreen: "#69ff94", BrightYellow: "#ffffa5",
			BrightBlue: "#d6acff", BrightMagenta: "#ff92df", BrightCyan: "#a4ffff", BrightWhite: "#ffffff",
		},
	},
	"nord": {
		ui: uiPalette{
			Accent: "#88c0d0", PanelBg: "#2e3440", Surface0: "#3b4252", Surface1: "#434c5e",
			SurfaceDim: "#2e3440", Overlay0: "#4c566a", Overlay1: "#646e82", Text: "#eceff4",
			Subtext0: "#d8dee9", Mauve: "#b48ead", Green: "#a3be8c", Yellow: "#ebcb8b",
			Red: "#bf616a", Blue: "#81a1c1", Teal: "#8fbcbb", Peach: "#d08770",
		},
		ansi: ansiPalette{
			Black: "#3b4252", Red: "#bf616a", Green: "#a3be8c", Yellow: "#ebcb8b",
			Blue: "#81a1c1", Magenta: "#b48ead", Cyan: "#88c0d0", White: "#e5e9f0",
			BrightBlack: "#596377", BrightRed: "#bf616a", BrightGreen: "#a3be8c", BrightYellow: "#ebcb8b",
			BrightBlue: "#81a1c1", BrightMagenta: "#b48ead", BrightCyan: "#8fbcbb", BrightWhite: "#eceff4",
		},
	},
	"gruvbox": {
		ui: uiPalette{
			Accent: "#d79921", PanelBg: "#282828", Surface0: "#3c3836", Surface1: "#504945",
			SurfaceDim: "#282828", Overlay0: "#928374", Overlay1: "#a89984", Text: "#ebdbb2",
			Subtext0: "#d5c4a1", Mauve: "#d3869b", Green: "#b8bb26", Yellow: "#fabd2f",
			Red: "#fb4934", Blue: "#83a598", Teal: "#8ec07c", Peach: "#fe8019",
		},
		ansi: ansiPalette{
			Black: "#282828", Red: "#cc241d", Green: "#98971a", Yellow: "#d79921",
			Blue: "#458588", Magenta: "#b16286", Cyan: "#689d6a", White: "#a89984",
			BrightBlack: "#928374", BrightRed: "#fb4934", BrightGreen: "#b8bb26", BrightYellow: "#fabd2f",
			BrightBlue: "#83a598", BrightMagenta: "#d3869b", BrightCyan: "#8ec07c", BrightWhite: "#ebdbb2",
		},
	},
	"one-dark": {
		ui: uiPalette{
			Accent: "#61afef", PanelBg: "#282c34", Surface0: "#2c313a", Surface1: "#3e4451",
			SurfaceDim: "#282c34", Overlay0: "#5c6370", Overlay1: "#737a87", Text: "#abb2bf",
			Subtext0: "#969ca8", Mauve: "#c678dd", Green: "#98c379", Yellow: "#e5c07b",
			Red: "#e06c75", Blue: "#61afef", Teal: "#56b6c2", Peach: "#d19a66",
		},
		ansi: ansiPalette{
			Black: "#21252b", Red: "#e06c75", Green: "#98c379", Yellow: "#e5c07b",
			Blue: "#61afef", Magenta: "#c678dd", Cyan: "#56b6c2", White: "#abb2bf",
			BrightBlack: "#767676", BrightRed: "#e06c75", BrightGreen: "#98c379", BrightYellow: "#e5c07b",
			BrightBlue: "#61afef", BrightMagenta: "#c678dd", BrightCyan: "#56b6c2", BrightWhite: "#abb2bf",
		},
	},
	"solarized": {
		ui: uiPalette{
			Accent: "#268bd2", PanelBg: "#002b36", Surface0: "#073642", Surface1: "#586e75",
			SurfaceDim: "#002b36", Overlay0: "#586e75", Overlay1: "#657b83", Text: "#93a1a1",
			Subtext0: "#839496", Mauve: "#d33682", Green: "#859900", Yellow: "#b58900",
			Red: "#dc322f", Blue: "#268bd2", Teal: "#2aa198", Peach: "#cb4b16",
		},
		ansi: ansiPalette{
			Black: "#073642", Red: "#dc322f", Green: "#859900", Yellow: "#b58900",
			Blue: "#268bd2", Magenta: "#d33682", Cyan: "#2aa198", White: "#eee8d5",
			BrightBlack: "#586e75", BrightRed: "#cb4b16", BrightGreen: "#586e75", BrightYellow: "#657b83",
			BrightBlue: "#839496", BrightMagenta: "#6c71c4", BrightCyan: "#93a1a1", BrightWhite: "#fdf6e3",
		},
	},
	"kanagawa": {
		ui: uiPalette{
			Accent: "#7e9cd8", PanelBg: "#1f1f28", Surface0: "#2a2a37", Surface1: "#363646",
			SurfaceDim: "#1f1f28", Overlay0: "#727169", Overlay1: "#87867d", Text: "#dcd7ba",
			Subtext0: "#c8c3aa", Mauve: "#957fb8", Green: "#76946a", Yellow: "#c0a36e",
			Red: "#c34043", Blue: "#7e9cd8", Teal: "#7fb4ca", Peach: "#ffa066",
		},
		ansi: ansiPalette{
			Black: "#090618", Red: "#c34043", Green: "#76946a", Yellow: "#c0a36e",
			Blue: "#7e9cd8", Magenta: "#957fb8", Cyan: "#6a9589", White: "#c8c093",
			BrightBlack: "#727169", BrightRed: "#e82424", BrightGreen: "#98bb6c", BrightYellow: "#e6c384",
			BrightBlue: "#7fb4ca", BrightMagenta: "#938aa9", BrightCyan: "#7aa89f", BrightWhite: "#dcd7ba",
		},
	},
	"rose-pine": {
		ui: uiPalette{
			Accent: "#c4a7e7", PanelBg: "#191724", Surface0: "#1f1d2e", Surface1: "#26233a",
			SurfaceDim: "#191724", Overlay0: "#6e6a86", Overlay1: "#908caa", Text: "#e0def4",
			Subtext0: "#c8c5dc", Mauve: "#c4a7e7", Green: "#31748f", Yellow: "#f6c177",
			Red: "#eb6f92", Blue: "#31748f", Teal: "#9ccfd8", Peach: "#ea9a97",
		},
		ansi: ansiPalette{
			Black: "#26233a", Red: "#eb6f92", Green: "#31748f", Yellow: "#f6c177",
			Blue: "#9ccfd8", Magenta: "#c4a7e7", Cyan: "#ebbcba", White: "#e0def4",
			BrightBlack: "#6e6a86", BrightRed: "#eb6f92", BrightGreen: "#31748f", BrightYellow: "#f6c177",
			BrightBlue: "#9ccfd8", BrightMagenta: "#c4a7e7", BrightCyan: "#ebbcba", BrightWhite: "#e0def4",
		},
	},
	"vesper": {
		ui: uiPalette{
			Accent: "#ffc799", PanelBg: "#1a1a1a", Surface0: "#232323", Surface1: "#282828",
			SurfaceDim: "#101010", Overlay0: "#5c5c5c", Overlay1: "#7e7e7e", Text: "#ffffff",
			Subtext0: "#a0a0a0", Mauve: "#ffd1a8", Green: "#99ffe4", Yellow: "#ffc799",
			Red: "#ff8080", Blue: "#b0b0b0", Teal: "#66ddcc", Peach: "#ffc799",
		},
		ansi: ansiPalette{
			Black: "#101010", Red: "#f5a191", Green: "#90b99f", Yellow: "#e6b99d",
			Blue: "#aca1cf", Magenta: "#e29eca", Cyan: "#ea83a5", White: "#a0a0a0",
			BrightBlack: "#7e7e7e", BrightRed: "#ff8080", BrightGreen: "#99ffe4", BrightYellow: "#ffc799",
			BrightBlue: "#b9aeda", BrightMagenta: "#ecaad6", BrightCyan: "#f591b2", BrightWhite: "#ffffff",
		},
	},
	// herdr's "terminal" theme inherits the host terminal's palette via ANSI
	// named colors / Reset. There's no host palette to inherit inside the
	// iframe, so we fall back to a neutral dark scheme + the standard xterm 16.
	"terminal": {
		ui: uiPalette{
			Accent: "#5f87d7", PanelBg: "#141414", Surface0: "#1c1c1c", Surface1: "#303030",
			SurfaceDim: "#0a0a0a", Overlay0: "#6c6c6c", Overlay1: "#9e9e9e", Text: "#d0d0d0",
			Subtext0: "#a8a8a8", Mauve: "#af87d7", Green: "#5faf5f", Yellow: "#d7af5f",
			Red: "#d75f5f", Blue: "#5f87d7", Teal: "#5fd7d7", Peach: "#d7875f",
		},
		ansi: ansiPalette{
			Black: "#000000", Red: "#cd0000", Green: "#00cd00", Yellow: "#cdcd00",
			Blue: "#1e90ff", Magenta: "#cd00cd", Cyan: "#00cdcd", White: "#e5e5e5",
			BrightBlack: "#7f7f7f", BrightRed: "#ff0000", BrightGreen: "#00ff00", BrightYellow: "#ffff00",
			BrightBlue: "#5c5cff", BrightMagenta: "#ff00ff", BrightCyan: "#00ffff", BrightWhite: "#ffffff",
		},
	},

	// Light variants (herdr 0.6.4). UI tokens transcribed verbatim from herdr's
	// src/app/state.rs (the *_latte/_day/_light/_lotus/_dawn Palette fns); ANSI
	// 16 from each scheme's canonical Alacritty export — same provenance as the
	// dark themes above. On these the bg is light and the text is dark.
	"catppuccin-latte": {
		ui: uiPalette{
			Accent: "#1e66f5", PanelBg: "#eff1f5", Surface0: "#ccd0da", Surface1: "#bcc0cc",
			SurfaceDim: "#e6e9ef", Overlay0: "#9ca0b0", Overlay1: "#8c8fa1", Text: "#4c4f69",
			Subtext0: "#6c6f85", Mauve: "#8839ef", Green: "#40a02b", Yellow: "#df8e1d",
			Red: "#d20f39", Blue: "#1e66f5", Teal: "#179299", Peach: "#fe640b",
		},
		ansi: ansiPalette{
			Black: "#5c5f77", Red: "#d20f39", Green: "#40a02b", Yellow: "#df8e1d",
			Blue: "#1e66f5", Magenta: "#ea76cb", Cyan: "#179299", White: "#acb0be",
			BrightBlack: "#6c6f85", BrightRed: "#de293e", BrightGreen: "#49af3d", BrightYellow: "#eea02d",
			BrightBlue: "#456eff", BrightMagenta: "#fe85d8", BrightCyan: "#2d9fa8", BrightWhite: "#bcc0cc",
		},
	},
	"tokyo-night-day": {
		ui: uiPalette{
			Accent: "#2e7de9", PanelBg: "#e1e2e7", Surface0: "#c4c8da", Surface1: "#a8aecb",
			SurfaceDim: "#d2d3da", Overlay0: "#8990b3", Overlay1: "#68709a", Text: "#3760bf",
			Subtext0: "#6172b0", Mauve: "#7847bd", Green: "#587539", Yellow: "#8c6c3e",
			Red: "#f52a65", Blue: "#2e7de9", Teal: "#118c74", Peach: "#b15c00",
		},
		ansi: ansiPalette{
			Black: "#e9e9ed", Red: "#f52a65", Green: "#587539", Yellow: "#8c6c3e",
			Blue: "#2e7de9", Magenta: "#9854f1", Cyan: "#007197", White: "#6172b0",
			BrightBlack: "#a1a6c5", BrightRed: "#f52a65", BrightGreen: "#587539", BrightYellow: "#8c6c3e",
			BrightBlue: "#2e7de9", BrightMagenta: "#9854f1", BrightCyan: "#007197", BrightWhite: "#3760bf",
		},
	},
	"gruvbox-light": {
		ui: uiPalette{
			Accent: "#076678", PanelBg: "#fbf1c7", Surface0: "#ebdbb2", Surface1: "#d5c4a1",
			SurfaceDim: "#f2e5bc", Overlay0: "#928374", Overlay1: "#7c6f64", Text: "#3c3836",
			Subtext0: "#504945", Mauve: "#8f3f71", Green: "#79740e", Yellow: "#b57614",
			Red: "#9d0006", Blue: "#076678", Teal: "#427b58", Peach: "#af3a03",
		},
		ansi: ansiPalette{
			Black: "#fbf1c7", Red: "#cc241d", Green: "#98971a", Yellow: "#d79921",
			Blue: "#458588", Magenta: "#b16286", Cyan: "#689d6a", White: "#7c6f64",
			BrightBlack: "#928374", BrightRed: "#9d0006", BrightGreen: "#79740e", BrightYellow: "#b57614",
			BrightBlue: "#076678", BrightMagenta: "#8f3f71", BrightCyan: "#427b58", BrightWhite: "#3c3836",
		},
	},
	"one-light": {
		ui: uiPalette{
			Accent: "#4078f2", PanelBg: "#fafafa", Surface0: "#f0f0f1", Surface1: "#e5e5e6",
			SurfaceDim: "#f5f5f6", Overlay0: "#a0a1a7", Overlay1: "#686b77", Text: "#383a42",
			Subtext0: "#686b77", Mauve: "#a626a4", Green: "#50a14f", Yellow: "#c18401",
			Red: "#e45649", Blue: "#4078f2", Teal: "#0184bc", Peach: "#986801",
		},
		ansi: ansiPalette{
			Black: "#000000", Red: "#de3e35", Green: "#3f953a", Yellow: "#d2b67c",
			Blue: "#2f5af3", Magenta: "#950095", Cyan: "#3f953a", White: "#bbbbbb",
			BrightBlack: "#000000", BrightRed: "#de3e35", BrightGreen: "#3f953a", BrightYellow: "#d2b67c",
			BrightBlue: "#2f5af3", BrightMagenta: "#a00095", BrightCyan: "#3f953a", BrightWhite: "#ffffff",
		},
	},
	"solarized-light": {
		ui: uiPalette{
			Accent: "#268bd2", PanelBg: "#fdf6e3", Surface0: "#eee8d5", Surface1: "#93a1a1",
			SurfaceDim: "#eee8d5", Overlay0: "#93a1a1", Overlay1: "#586e75", Text: "#657b83",
			Subtext0: "#839496", Mauve: "#d33682", Green: "#859900", Yellow: "#b58900",
			Red: "#dc322f", Blue: "#268bd2", Teal: "#2aa198", Peach: "#cb4b16",
		},
		ansi: ansiPalette{
			Black: "#073642", Red: "#dc322f", Green: "#859900", Yellow: "#b58900",
			Blue: "#268bd2", Magenta: "#d33682", Cyan: "#2aa198", White: "#bbb5a2",
			BrightBlack: "#002b36", BrightRed: "#cb4b16", BrightGreen: "#586e75", BrightYellow: "#657b83",
			BrightBlue: "#839496", BrightMagenta: "#6c71c4", BrightCyan: "#93a1a1", BrightWhite: "#fdf6e3",
		},
	},
	"kanagawa-lotus": {
		ui: uiPalette{
			Accent: "#4d699b", PanelBg: "#f2ecbc", Surface0: "#dcd5ac", Surface1: "#c9cbd1",
			SurfaceDim: "#d5cea3", Overlay0: "#a09cac", Overlay1: "#8a8980", Text: "#545464",
			Subtext0: "#43436c", Mauve: "#624c83", Green: "#6f894e", Yellow: "#77713f",
			Red: "#c84053", Blue: "#4d699b", Teal: "#4e8ca2", Peach: "#cc6d00",
		},
		ansi: ansiPalette{
			Black: "#1f1f28", Red: "#c84053", Green: "#6f894e", Yellow: "#77713f",
			Blue: "#4d699b", Magenta: "#b35b79", Cyan: "#597b75", White: "#545464",
			BrightBlack: "#8a8980", BrightRed: "#d7474b", BrightGreen: "#6e915f", BrightYellow: "#836f4a",
			BrightBlue: "#6693bf", BrightMagenta: "#624c83", BrightCyan: "#5e857a", BrightWhite: "#43436c",
		},
	},
	"rose-pine-dawn": {
		ui: uiPalette{
			Accent: "#907aa9", PanelBg: "#faf4ed", Surface0: "#f2e9e1", Surface1: "#fffaf3",
			SurfaceDim: "#f2e9e1", Overlay0: "#9893a5", Overlay1: "#797593", Text: "#464261",
			Subtext0: "#797593", Mauve: "#907aa9", Green: "#286983", Yellow: "#ea9d34",
			Red: "#b4637a", Blue: "#286983", Teal: "#56949f", Peach: "#d7827e",
		},
		ansi: ansiPalette{
			Black: "#f2e9e1", Red: "#b4637a", Green: "#286983", Yellow: "#ea9d34",
			Blue: "#56949f", Magenta: "#907aa9", Cyan: "#d7827e", White: "#575279",
			BrightBlack: "#9893a5", BrightRed: "#b4637a", BrightGreen: "#286983", BrightYellow: "#ea9d34",
			BrightBlue: "#56949f", BrightMagenta: "#907aa9", BrightCyan: "#d7827e", BrightWhite: "#575279",
		},
	},
}

// themeAliases maps herdr's alternate theme spellings to our canonical keys,
// mirroring herdr's from_name match arms (src/app/state.rs) so every name herdr
// accepts resolves to the same palette here. Unknown names fall back to
// catppuccin, matching herdr (its from_name returns None → default).
var themeAliases = map[string]string{
	"catppuccin-mocha": "catppuccin",
	"mocha":            "catppuccin",
	"latte":            "catppuccin-latte",
	"light":            "catppuccin-latte",
	"tokyonight":       "tokyo-night",
	"tokyo-day":        "tokyo-night-day",
	"tokyonight-day":   "tokyo-night-day",
	"gruvbox-dark":     "gruvbox",
	"onedark":          "one-dark",
	"onelight":         "one-light",
	"solarized-dark":   "solarized",
	"rosepine":         "rose-pine",
	"rosepine-dawn":    "rose-pine-dawn",
	"dawn":             "rose-pine-dawn",
	"lotus":            "kanagawa-lotus",
}

// resolvedTheme is a concrete palette after applying config overrides.
type resolvedTheme struct {
	Name       string // the name as found in config (for logging)
	Resolved   string // canonical key we resolved to
	Customized bool   // whether any [theme.custom]/legacy accent override applied
	ui         uiPalette
	ansi       ansiPalette
}

// normalizeThemeName mirrors herdr's from_name normalization.
func normalizeThemeName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.NewReplacer(" ", "-", "_", "-").Replace(n)
	if c, ok := themeAliases[n]; ok {
		return c
	}
	return n
}

// herdrConfigPath returns the path to config.toml, honoring HERDR_CONFIG_PATH
// and otherwise sitting alongside the socket.
func herdrConfigPath() string {
	if p := os.Getenv("HERDR_CONFIG_PATH"); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(*herdrSock), "config.toml")
}

// loadHerdrTheme resolves the active theme. If forceName != "" and != "auto" it
// is used directly; otherwise the name (and overrides) come from config.toml.
// Falls back to catppuccin (herdr's default) on anything unreadable/unknown.
func loadHerdrTheme(forceName string) resolvedTheme {
	name, custom, legacyAccent := "", map[string]string{}, ""
	if forceName != "" && forceName != "auto" {
		name = forceName
	} else {
		name, custom, legacyAccent = parseThemeConfig(herdrConfigPath())
	}

	rawName := name
	if name == "" {
		name = "catppuccin"
	}
	key := normalizeThemeName(name)
	def, ok := themes[key]
	if !ok {
		key, def = "catppuccin", themes["catppuccin"]
	}

	rt := resolvedTheme{Name: rawName, Resolved: key, ui: def.ui, ansi: def.ansi}
	if rt.Name == "" {
		rt.Name = "(default catppuccin)"
	}

	// [theme.custom] per-token overrides, then legacy [ui].accent (only if
	// [theme.custom].accent is unset and it's not the old "cyan" default).
	for tok, raw := range custom {
		if hex, ok := parseColor(raw); ok {
			rt.applyToken(tok, hex)
			rt.Customized = true
		}
	}
	if _, hasCustomAccent := custom["accent"]; !hasCustomAccent && legacyAccent != "" && legacyAccent != "cyan" {
		if hex, ok := parseColor(legacyAccent); ok {
			rt.ui.Accent = hex
			rt.Customized = true
		}
	}
	return rt
}

// applyToken overwrites a single UI token by its config name.
func (rt *resolvedTheme) applyToken(tok, hex string) {
	switch tok {
	case "accent":
		rt.ui.Accent = hex
	case "panel_bg":
		rt.ui.PanelBg = hex
	case "surface0":
		rt.ui.Surface0 = hex
	case "surface1":
		rt.ui.Surface1 = hex
	case "surface_dim":
		rt.ui.SurfaceDim = hex
	case "overlay0":
		rt.ui.Overlay0 = hex
	case "overlay1":
		rt.ui.Overlay1 = hex
	case "text":
		rt.ui.Text = hex
	case "subtext0":
		rt.ui.Subtext0 = hex
	case "mauve":
		rt.ui.Mauve = hex
	case "green":
		rt.ui.Green = hex
	case "yellow":
		rt.ui.Yellow = hex
	case "red":
		rt.ui.Red = hex
	case "blue":
		rt.ui.Blue = hex
	case "teal":
		rt.ui.Teal = hex
	case "peach":
		rt.ui.Peach = hex
	}
}

// termSelectionAlpha is the opacity of the terminal's selection/highlight tint.
// Selection is a *translucent* wash of the theme accent (see xtermJSON): unlike
// an opaque color it composites over whatever cell content is underneath, so it
// stays visible on every theme instead of vanishing when the canonical selection
// color sits a shade off the background (e.g. tokyo-night's #283457 over
// #1a1b26). ~40% reads as a clear band while the text below stays legible.
const termSelectionAlpha = 0x66

// xtermJSON builds an xterm.js ITheme: chrome (bg/fg/cursor) from the herdr UI
// tokens so the terminal blends with herdr's own theme, a translucent-accent
// selection highlight (theme-matched yet always visible — see
// termSelectionAlpha), and the 16 ANSI colors from the scheme's canonical palette.
func (rt resolvedTheme) xtermJSON() string {
	a := rt.ansi
	return `{` +
		q("background", rt.ui.PanelBg) + "," + q("foreground", rt.ui.Text) + "," +
		q("cursor", rt.ui.Text) + "," + q("cursorAccent", rt.ui.PanelBg) + "," +
		q("selectionBackground", rgba(rt.ui.Accent, termSelectionAlpha)) + "," +
		q("black", a.Black) + "," + q("red", a.Red) + "," + q("green", a.Green) + "," +
		q("yellow", a.Yellow) + "," + q("blue", a.Blue) + "," + q("magenta", a.Magenta) + "," +
		q("cyan", a.Cyan) + "," + q("white", a.White) + "," +
		q("brightBlack", a.BrightBlack) + "," + q("brightRed", a.BrightRed) + "," +
		q("brightGreen", a.BrightGreen) + "," + q("brightYellow", a.BrightYellow) + "," +
		q("brightBlue", a.BrightBlue) + "," + q("brightMagenta", a.BrightMagenta) + "," +
		q("brightCyan", a.BrightCyan) + "," + q("brightWhite", a.BrightWhite) + `}`
}

func q(k, v string) string { return `"` + k + `":"` + v + `"` }

// cssVars renders the :root custom-property declarations for the sidebar.
// --accent maps to each theme's own Accent token (the signature color that
// drives --primary in the chrome), so e.g. rose-pine reads purple, not teal.
// --good stays on Teal since it's the success color, independent of the accent.
// --muted is the secondary-text tier (form labels, metadata), so it maps to
// Subtext0, not Overlay0 — Overlay0 is the dimmest "subtle line/disabled" tier
// and reads at ~2:1 against the panel, too low for labels.
func (rt resolvedTheme) cssVars() string {
	u := rt.ui
	var b strings.Builder
	put := func(name, val string) { fmt.Fprintf(&b, "    %s: %s;\n", name, val) }
	put("--bg", u.PanelBg)
	put("--panel", u.Surface0)
	put("--border", u.Surface1)
	put("--hover", u.Surface1)
	put("--fg", u.Text)
	put("--muted", u.Subtext0)
	put("--accent", u.Accent)
	put("--accent-dim", rgba(u.Accent, 0x26))
	put("--dir", u.Mauve)
	put("--good", u.Teal)
	put("--warn", u.Yellow)
	put("--bad", u.Red)
	return b.String()
}

// cssVarsRoot returns the resolved palette as a ":root{…}" rule using the same
// --h-* custom-property names the stylesheet (web/src/index.css) and the
// runtime applier (lib/theme.ts:applyCSSVars) use. It's injected into the
// served index.html so the first paint matches herdr's theme instead of
// flashing the stylesheet's fallback palette before /api/theme lands.
func (rt resolvedTheme) cssVarsRoot() string {
	// cssVars() emits bare "--bg: …;" declarations; the live theme is keyed by
	// "--h-bg" (applyCSSVars prefixes them). Prefix the leading "--" of each
	// property to "--h-" — the palette values contain no other "--" sequences.
	return ":root{\n" + strings.ReplaceAll(rt.cssVars(), "--", "--h-") + "}"
}

// rgba appends an 8-bit alpha to a #rrggbb hex (-> #rrggbbaa); passes other
// formats through unchanged.
func rgba(hex string, alpha int) string {
	if len(hex) == 7 && hex[0] == '#' {
		return fmt.Sprintf("%s%02x", hex, alpha&0xff)
	}
	return hex
}

// ---------------------------------------------------------------------------
// minimal config.toml reader (only what we need: [theme].name,
// [theme.custom].*, legacy [ui].accent) — avoids a TOML dependency.
// ---------------------------------------------------------------------------

// parseThemeConfig returns (theme name, [theme.custom] token map, legacy
// [ui].accent). Missing/unreadable config yields zero values.
func parseThemeConfig(path string) (name string, custom map[string]string, legacyAccent string) {
	custom = map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", custom, ""
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(stripComment(line))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		val = unquote(strings.TrimSpace(val))
		switch section {
		case "theme":
			if key == "name" {
				name = val
			}
		case "theme.custom":
			custom[key] = val
		case "ui":
			if key == "accent" {
				legacyAccent = val
			}
		}
	}
	return name, custom, legacyAccent
}

// stripComment removes a trailing TOML comment that lies outside of quotes.
func stripComment(line string) string {
	inStr := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inStr = !inStr
		case '#':
			if !inStr {
				return line[:i]
			}
		}
	}
	return line
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// ---------------------------------------------------------------------------
// parse_color: mirrors herdr's src/config/theme.rs parser, returning #rrggbb.
// ok=false means "leave the base token unchanged" (used for the reset aliases).
// ---------------------------------------------------------------------------

func parseColor(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "reset", "default", "none", "transparent":
		return "", false // transparent / inherit — no hex to apply to our chrome
	}
	if strings.HasPrefix(s, "#") {
		return normalizeHex(s)
	}
	if strings.HasPrefix(s, "rgb(") && strings.HasSuffix(s, ")") {
		parts := strings.Split(s[4:len(s)-1], ",")
		if len(parts) == 3 {
			r, e1 := strconv.Atoi(strings.TrimSpace(parts[0]))
			g, e2 := strconv.Atoi(strings.TrimSpace(parts[1]))
			b, e3 := strconv.Atoi(strings.TrimSpace(parts[2]))
			if e1 == nil && e2 == nil && e3 == nil &&
				r >= 0 && r <= 255 && g >= 0 && g <= 255 && b >= 0 && b <= 255 {
				return fmt.Sprintf("#%02x%02x%02x", r, g, b), true
			}
		}
		return "", false
	}
	if hex, ok := namedColors[s]; ok {
		return hex, true
	}
	return "#00ffff", true // herdr's fallback: unknown -> cyan
}

// normalizeHex accepts #rgb or #rrggbb and returns #rrggbb.
func normalizeHex(s string) (string, bool) {
	h := s[1:]
	switch len(h) {
	case 3:
		return fmt.Sprintf("#%c%c%c%c%c%c", h[0], h[0], h[1], h[1], h[2], h[2]), true
	case 6:
		return s, true
	}
	return "", false
}

// namedColors maps herdr's accepted color names to representative hex values.
var namedColors = map[string]string{
	"black": "#000000", "red": "#cc0000", "green": "#4e9a06", "yellow": "#c4a000",
	"blue": "#3465a4", "magenta": "#75507b", "purple": "#75507b", "cyan": "#06989a",
	"white": "#d3d7cf", "gray": "#808080", "grey": "#808080", "darkgray": "#555753",
	"darkgrey": "#555753", "lightred": "#ef2929", "lightgreen": "#8ae234",
	"lightyellow": "#fce94f", "lightblue": "#729fcf", "lightmagenta": "#ad7fa8",
	"lightcyan": "#34e2e2",
}
