// Legible agent palettes: the contrast pass between a resolved theme and the
// words omp and Claude Code actually paint inside a pane.
//
// Every other palette lasso derives answers "what colour is this token?". This
// file answers a different question — "can the human read it?" — and it exists
// because the canvas an agent's glyphs land on is NOT the theme's background
// any more. A terminal under a backdrop is served transparent
// (transparentPanelBG), so a pane's default cells show a photograph washed by
// the scrim, and the theme's own accents are mapped against a background that
// is no longer there. Ayu Light under a wallpaper at 0.75 dimming was the
// report: omp's inline code (mdCode = Green, #86b300 at 1.9:1) and its
// blockquotes (mdQuote = Yellow) came out as pale text on a pale sky, while the
// same palette on a flat canvas reads fine.
//
// Three decisions carry the whole file:
//
//   - The canvas is the WORST one a glyph can land on, not the average. An
//     image is arbitrary pixels: lasso cannot know (and must not guess) what is
//     behind any given cell, and a picture the human hands it by URL is not
//     even readable from here. What it does know is that the scrim composites
//     every pixel toward the theme's own background, so the composite is
//     bounded — at scrim α the darkest possible cell is α·bg (a black pixel)
//     and the lightest is α·bg + (1−α)·white. One of those two bounds is the
//     one that hurts, and which one is decided by the THEME (see glyphCanvas).
//     Clear the bound and every pixel of every image is cleared with it.
//
//   - It scales with the dimming slider by construction. α = 1 (no image, or a
//     fully opaque wash) collapses the bound to the theme's own background, so
//     a flat theme is conditioned exactly as it is today; lowering α widens the
//     bound and drives the text further from it. No image data, no heuristics,
//     one monotone knob.
//
//   - Only words are touched, and only when they FAIL. Borders, rules and
//     background fills are left alone — a rule that fades under a photograph is
//     cosmetic, while a sentence that does is the bug — and a token already
//     clearing its target is returned unchanged, so a theme that reads well
//     keeps the colours its author chose.
package main

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

// The contrast a token has to clear against the worst canvas, in WCAG 2.x
// ratios. Two tiers, because a TUI has two kinds of text and collapsing them
// would cost the hierarchy: bring every muted label up to body contrast and
// nothing reads as secondary any more.
const (
	// legibleBody is the AA bar for body text, and what everything carrying
	// words gets: prose, code, headings, diff markers, syntax, status fields.
	legibleBody = 4.5
	// legibleQuiet is the AA large-text/non-text bar, and what the deliberately
	// secondary tier gets — muted metadata, tool output, comments. Still
	// legible, still visibly quieter than body text.
	legibleQuiet = 3.0
)

// bundledDefaultBackground mirrors lib/wallpaper.ts's BUNDLED_DEFAULT: the one
// theme that wears a still with nothing stored for it. Every other theme starts
// flat, so an absent entry there means a flat canvas.
var bundledDefaultBackground = map[string]string{
	"retro-82": "/wallpapers/retro-82/04-dusk-guardian.webp",
}

// glyphCanvas is what an agent's text is drawn on, reduced to the two numbers a
// contrast pass needs.
type glyphCanvas struct {
	// Bg is the theme's own terminal background — the canvas with no backdrop,
	// and what the bound below collapses to when there is none.
	Bg string
	// Lum is the relative luminance of the worst canvas a glyph can land on:
	// the composite that contrasts LEAST with text of this theme's polarity.
	Lum float64
	// Dark says text on this canvas is the dark end of the ramp. It is decided
	// by the theme's own background and never by Lum: a light theme under a dim
	// wallpaper must keep pushing its colours darker, not invert them halfway
	// through a drag of the dimming slider.
	Dark bool
}

// glyphCanvasOf reduces a background, an image and a scrim to the bound the
// contrast pass works against. scrim is the wash's alpha (1 = opaque, i.e. no
// image at all); image says whether there is a picture under it.
//
// The composite is sRGB-space `α·bg + (1−α)·pixel`, which is what the browser
// does with `linear-gradient(rgba(bg,α))` over a `url()` layer — so the bound
// is the same arithmetic the eye is looking at, per channel, then luminance.
// The palette-derived shading layers are ignored: under an image they sit below
// the scrim and inside the bound already, and with no image they are two washes
// at ≤0.16 alpha of a hue, which moves the canvas by less than the rounding on
// a single channel.
func glyphCanvasOf(bg string, image bool, scrim float64) glyphCanvas {
	lum, _ := relLuminance(bg)
	// Light or dark is the theme's own question and is asked the way the rest
	// of the file asks it (luminance(), the cheap approximation every caller
	// compares against 0.5). Contrast is the other question, and it is asked in
	// WCAG's terms — see relLuminanceRGB for why the two cannot share one
	// number.
	c := glyphCanvas{Bg: bg, Lum: lum, Dark: luminance(bg) > 0.5}
	r, g, b, ok := hexRGB(bg)
	if !ok || !image {
		// A palette whose background is not a plain #rrggbb cannot be
		// composited, and one with no image has nothing to composite with:
		// either way the theme's own background is the canvas.
		return c
	}
	scrim = math.Min(1, math.Max(0, scrim))
	// The pixel that hurts: black under dark text, white under light text.
	pixel := 255.0
	if c.Dark {
		pixel = 0
	}
	mix := func(v int) int {
		return int(math.Round(scrim*float64(v) + (1-scrim)*pixel))
	}
	c.Lum = relLuminanceRGB(mix(r), mix(g), mix(b))
	return c
}

// contrast is the WCAG 2.x ratio between a colour and this canvas's bound.
func (c glyphCanvas) contrast(hex string) float64 {
	r, g, b, ok := hexRGB(hex)
	if !ok {
		return 21 // unparseable: not a colour this pass may move
	}
	hi, lo := relLuminanceRGB(r, g, b), c.Lum
	if lo > hi {
		hi, lo = lo, hi
	}
	return (hi + 0.05) / (lo + 0.05)
}

// legibleTravel caps how far a colour may be moved: a fraction of the distance
// from its own perceived lightness to the pole it is being pushed toward (white
// under a dark theme, black under a light one).
//
// It exists for the canvases the palette cannot win. A dark theme under a
// bright wallpaper at half its dimming has a worst-case canvas around 0.45
// luminance, where the target sits above what any colour can reach — the
// bisection lands on the clamp, and every token is driven to #ffffff: the
// palette gone, every hue identical, the text no more readable than before.
// With a cap each colour brightens as far as it can while remaining
// recognizably itself (retro-82's #faa968 reaches #ffeadb, 4.11:1, under a 0.6
// scrim rather than pure white), and the remedy for a backdrop that bright
// stays what it actually is — the dimming slider.
//
// It is also what the floor below is measured against: a colour the cap holds
// short of its tier still has to buy real legibility to be used at all.
//
// 0.75 was chosen against every theme this build resolves, at every dimming:
// no conditioning that REACHES its tier is blocked by it on a light theme —
// the reported case, where the deepest fix (ayu-light's yellow under a 0.75
// scrim) travels 0.53 — while the hopeless ones stop well short of the pole.
// There is no cliff either way: the travel grows smoothly as the slider moves.
const legibleTravel = 0.75

// legible returns hex moved along its own lightness axis until its contrast
// against this canvas reaches ratio, keeping the colour's hue and chroma.
//
// It works in Oklab, and that is what makes the result still look like the
// theme's colour. The obvious implementations do not: mixing toward the
// background (or toward black/white) desaturates by however far the two hues
// sit apart, so a red collapses to brown while a teal keeps its chroma; moving
// HSL lightness keeps the hue but bleeds chroma as it approaches either end,
// which turned ayu-light's accents into near-neutral mud (measured: 28% of the
// original chroma left). Holding Oklab's (a, b) fixed and moving only L is the
// literal statement of "the same colour, darker", with the sRGB gamut the only
// thing that may take chroma away (see oklabHex).
//
// A colour already clearing the ratio comes back untouched, so this is a floor
// on legibility rather than a restyling: on a theme whose author already had
// the contrast right, nothing here changes a single token.
func (c glyphCanvas) legible(hex string, ratio float64) string {
	l0, a, b, ok := oklabOf(hex)
	if !ok || c.contrast(hex) >= ratio {
		return hex
	}
	// The luminance the ratio demands, on the side the theme's polarity puts
	// the text. Clamped, because a canvas can be bright enough (or dark enough)
	// that no colour clears the ratio at all; the cap below is what keeps that
	// case from ending at the pole.
	target := ratio*(c.Lum+0.05) - 0.05
	pole := 1.0
	if c.Dark {
		target = (c.Lum+0.05)/ratio - 0.05
		pole = 0
	}
	target = math.Min(1, math.Max(0, target))
	// Luminance is monotonic in Oklab lightness at a fixed hue and chroma, so
	// bisection converges on the lightness that just meets the target; 24
	// rounds is well past 8-bit resolution. The half that satisfies the target
	// is kept, so rounding lands on the legible side rather than a hair short.
	lo, hi := 0.0, 1.0
	for range 24 {
		mid := (lo + hi) / 2
		if l, _ := relLuminance(oklabHex(mid, a, b)); l < target {
			lo = mid
		} else {
			hi = mid
		}
	}
	want, limit := hi, l0+legibleTravel*(pole-l0)
	if c.Dark {
		want = math.Max(lo, limit) // darker, but not past the cap
	} else {
		want = math.Min(want, limit)
	}
	out := oklabHex(want, a, b)
	// Two things the move has to be worth, and both were measured on real
	// settings rather than imagined:
	//
	//   - It must not make text HARDER to read. In the futile regime the canvas
	//     bound can sit BETWEEN the colour and the pole — a dark theme's
	//     mid-tone teal against a bound at 0.69 luminance — so moving toward
	//     the pole crosses it and lands closer than it started (retro-82's
	//     green, 1.62:1 → 1.21:1 at a 0.33 scrim).
	//   - It must buy real legibility. Below legibleQuiet nothing a palette can
	//     do makes the words readable, so repainting every accent for a gain
	//     from 1.0:1 to 1.3:1 is a theme thrown away for nothing — retro-82
	//     under a wallpaper at a third of its dimming came out in pastels that
	//     were exactly as unreadable as the colours they replaced. The remedy
	//     there is the dimming slider, and leaving the palette alone is what
	//     says so.
	if got := c.contrast(out); got <= c.contrast(hex) || got < legibleQuiet {
		return hex
	}
	return out
}

// glyphCanvasFor resolves the canvas for a theme lasso is about to write agent
// themes for: the theme's own background, plus whatever backdrop this lasso's
// UI state says that theme wears.
//
// The backdrop is lasso's own state and global (one pick per theme, shared by
// every browser), so the same canvas applies to a remote host's agents as to
// local ones — the terminal those panes are shown in is served by THIS lasso.
func glyphCanvasFor(rt resolvedTheme) glyphCanvas {
	image, scrim := themeBackdrop(rt.Resolved)
	return glyphCanvasOf(rt.ui.PanelBg, image, scrim)
}

// themeBackdrop reports whether a theme wears an image and at what scrim,
// mirroring lib/wallpaper.ts (backgroundFor / getScrim) — the two are one
// decision and the browser's copy is the one the human is looking at.
//
// Only presence is answered, never which picture: the bound above holds for any
// pixels at all, which is also what makes a URL lasso cannot read (a hand-given
// one on another machine, an /api/file upload) as safe as a bundled still.
//
// Deliberately tolerant. A stored URL the frontend would reject falls back
// there to the theme's default, and lasso cannot replay that check without the
// gallery; assuming an image is the conservative half of the mistake — it
// conditions text for a backdrop that is not painted, which costs contrast a
// human never sees, where the other way costs legibility they do.
func themeBackdrop(theme string) (bool, float64) {
	if theme == "" || db == nil {
		return false, defaultScrim
	}
	us, err := getUIState()
	if err != nil {
		log.Printf("theme:    backdrop for %s unreadable (%v) — conditioning agent text for a flat canvas", theme, err)
		return false, defaultScrim
	}
	return backdropOf(theme, us.ThemeAtmosphere[theme])
}

// backdropOf is themeBackdrop's arithmetic, over one stored entry. An ABSENT
// entry is the zero value and lands on the same branch as one holding no
// picture, which is the frontend's rule too: no opinion means this theme's
// default (a still for retro-82, nothing for every other theme), while the
// explicit atmosphereNoBackground means a human turned it off.
func backdropOf(theme string, pref atmospherePref) (bool, float64) {
	scrim := defaultScrim
	if pref.Scrim != nil {
		scrim = math.Min(1, math.Max(0, *pref.Scrim))
	}
	switch pref.Background {
	case "":
		return bundledDefaultBackground[theme] != "", scrim
	case atmosphereNoBackground:
		return false, scrim
	}
	return true, scrim
}

// defaultScrim mirrors lib/wallpaper.ts's DEFAULT_SCRIM: the wash a theme wears
// when a picture is picked and the slider never touched.
const defaultScrim = 0.7

// backdropSig is the part of a theme's backdrop that changes what a legible
// palette looks like: whether there is an image and how much of it shows
// through. It is the fingerprint the convergence record carries beside the
// theme name (see themeStamp), so a host asleep through a dimming change is
// caught up on its next probe instead of keeping a palette conditioned for a
// backdrop nobody is wearing.
func backdropSig(theme string) string {
	image, scrim := themeBackdrop(theme)
	return backdropSigOf(image, scrim)
}

// backdropSigOf renders the fingerprint. A flat canvas collapses to one value
// whatever the stored scrim says — with no picture there is nothing to wash, so
// the slider changes no colour — and a scrim is quantized to whole percent, so
// a drag settles into one re-mirror rather than one per sub-pixel.
func backdropSigOf(image bool, scrim float64) string {
	if !image {
		return "flat"
	}
	return fmt.Sprintf("img/%.2f", scrim)
}

// backdropChanged reports whether a ui_state write moved the part of theme's
// backdrop that decides what a legible palette looks like. Everything else in
// the same POST — the sidebar width, the usage footer, another theme's picture —
// leaves the agents' text alone and must not cost a fleet-wide re-mirror.
func backdropChanged(before, after uiState, theme string) bool {
	if theme == "" {
		return false
	}
	return backdropSigOf(backdropOf(theme, before.ThemeAtmosphere[theme])) !=
		backdropSigOf(backdropOf(theme, after.ThemeAtmosphere[theme]))
}

// ---------------------------------------------------------------------------
// The per-CLI token tables
// ---------------------------------------------------------------------------
//
// Keyed by the CLI's OWN token names rather than by herdr's palette tokens,
// because one palette colour lands in both kinds of place: Claude Code's accent
// is its `permission` text AND its `promptBorder`, omp's yellow is `mdQuote` and
// `statusLineGitDirty`. Conditioning the palette would drag the borders along;
// conditioning the emitted map states, token by token, which ones spell words.
//
// A token absent from these tables is left exactly as mapped. That is the
// answer for every background fill, every border and rule, and for Claude's
// `inverseText` — which is the theme's background painted ON an accent fill, so
// the canvas it needs contrast against is not this one.

// ompLegibility is omp's text tokens (see ompColors).
//
// The tokens that sit on one of omp's own opaque fills — a user message, a tool
// frame, the status line — are conditioned against the canvas too, not against
// that fill. Those fills are the theme's own surfaces, a few percent from the
// background it already failed against, so the canvas bound is the harsher of
// the two questions and clearing it clears both.
var ompLegibility = map[string]float64{
	"text":               legibleBody,
	"accent":             legibleBody,
	"success":            legibleBody,
	"error":              legibleBody,
	"warning":            legibleBody,
	"userMessageText":    legibleBody,
	"customMessageText":  legibleBody,
	"customMessageLabel": legibleBody,
	"toolTitle":          legibleBody,

	"mdHeading":    legibleBody,
	"mdLink":       legibleBody,
	"mdCode":       legibleBody,
	"mdCodeBlock":  legibleBody,
	"mdQuote":      legibleBody,
	"mdListBullet": legibleBody,

	"toolDiffAdded":   legibleBody,
	"toolDiffRemoved": legibleBody,

	"syntaxKeyword":     legibleBody,
	"syntaxFunction":    legibleBody,
	"syntaxVariable":    legibleBody,
	"syntaxString":      legibleBody,
	"syntaxNumber":      legibleBody,
	"syntaxType":        legibleBody,
	"syntaxOperator":    legibleBody,
	"syntaxPunctuation": legibleBody,

	"bashMode":   legibleBody,
	"pythonMode": legibleBody,

	"statusLineModel":     legibleBody,
	"statusLinePath":      legibleBody,
	"statusLineGitClean":  legibleBody,
	"statusLineGitDirty":  legibleBody,
	"statusLineContext":   legibleBody,
	"statusLineSpend":     legibleBody,
	"statusLineStaged":    legibleBody,
	"statusLineDirty":     legibleBody,
	"statusLineUntracked": legibleBody,
	"statusLineOutput":    legibleBody,
	"statusLineCost":      legibleBody,
	"statusLineSubagents": legibleBody,

	"muted":           legibleQuiet,
	"dim":             legibleQuiet,
	"thinkingText":    legibleQuiet,
	"toolOutput":      legibleQuiet,
	"mdLinkUrl":       legibleQuiet,
	"syntaxComment":   legibleQuiet,
	"toolDiffContext": legibleQuiet,
}

// claudeLegibility is Claude Code's text tokens (see claudeOverrides). The six
// diff tokens are backgrounds and stay out of it — they are already derived as
// a fixed lift over the panel background, and Claude draws its own text on
// them.
var claudeLegibility = map[string]float64{
	"text":       legibleBody,
	"permission": legibleBody,
	"ide":        legibleBody,
	"planMode":   legibleBody,
	"thinking":   legibleBody,
	"merged":     legibleBody,
	"remember":   legibleBody,
	"success":    legibleBody,
	"autoAccept": legibleBody,
	"error":      legibleBody,
	"warning":    legibleBody,

	"subtle":     legibleQuiet,
	"inactive":   legibleQuiet,
	"suggestion": legibleQuiet,
}

// legibleTokens raises every token named in tiers to its contrast floor, in
// place. Tokens the table does not name, and tokens already clearing their
// floor, are left as they are.
func legibleTokens(m map[string]string, tiers map[string]float64, c glyphCanvas) {
	for tok, ratio := range tiers {
		if hex, ok := m[tok]; ok {
			m[tok] = c.legible(hex, ratio)
		}
	}
}

// ---------------------------------------------------------------------------
// Re-mirroring when the backdrop moves
// ---------------------------------------------------------------------------
//
// A theme change fans out on its own (syncThemeEverywhere) and a host that
// missed one catches up on its next probe. The backdrop had neither: it is
// lasso's own UI state, it changes no theme name, and until now it changed
// nothing an agent could read. Now it decides what "legible" means, so picking
// a picture or moving the dimming slider has to reach the same files a
// re-theme does — which is also what makes a running omp repaint, since omp
// watches its theme file and reloads it.

// backdropResyncDelay coalesces a run of backdrop writes into one fan-out. The
// dimming slider fires per pixel of a drag (the client already coalesces its
// saves at 250ms), and a fan-out is SFTP to every reachable host: a write per
// tick would mean an ssh burst per drag.
const backdropResyncDelay = 1500 * time.Millisecond

var backdropResync struct {
	mu    sync.Mutex
	timer *time.Timer
}

// scheduleBackdropResync re-mirrors the agent theme files after the backdrop
// settles. Debounced, best-effort, and off the request path — the browser
// repaints itself from its own state; this is only the agents catching up.
func scheduleBackdropResync(why string) {
	backdropResync.mu.Lock()
	defer backdropResync.mu.Unlock()
	if backdropResync.timer != nil {
		backdropResync.timer.Reset(backdropResyncDelay)
		return
	}
	backdropResync.timer = time.AfterFunc(backdropResyncDelay, func() {
		rt := liveTheme()
		log.Printf("theme:    %s -> re-mirroring agent themes for %s (%s)", why, rt.Resolved, backdropSig(rt.Resolved))
		syncAgentThemesEverywhere(rt)
	})
}
