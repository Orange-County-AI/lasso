package main

import (
	"math"
	"testing"
)

// hueOf is the angle of a colour in Oklab, in degrees — the thing a human calls
// "which colour is it". Only meaningful for a colour with some chroma.
func hueOf(hex string) float64 {
	_, a, b, _ := oklabOf(hex)
	h := math.Atan2(b, a) * 180 / math.Pi
	if h < 0 {
		h += 360
	}
	return h
}

func chromaOf(hex string) float64 {
	_, a, b, _ := oklabOf(hex)
	return math.Hypot(a, b)
}

// Everything in this file rests on being able to put a colour back together
// from its Oklab coordinates, so that is the first thing worth proving: a
// round trip through the inverse has to land on the colour it started from.
func TestOklabRoundTrip(t *testing.T) {
	for _, hex := range []string{
		"#000000", "#ffffff", "#f8f9fa", "#00172e", "#6cbf43", "#eca944",
		"#ea6c6d", "#3199e1", "#8839ef", "#028391", "#717171",
	} {
		l, a, b, ok := oklabOf(hex)
		if !ok {
			t.Fatalf("oklabOf(%s) failed", hex)
		}
		if got := oklabHex(l, a, b); got != hex {
			t.Errorf("round trip %s -> %s", hex, got)
		}
	}
	// A colour outside sRGB at that lightness keeps its lightness and hue and
	// pays in chroma, rather than coming back as a clipped mess.
	const vivid = "#1e66f5" // catppuccin-latte's blue: high chroma, mid lightness
	_, a, b, _ := oklabOf(vivid)
	dark := oklabHex(0.25, a, b)
	if d := math.Abs(hueOf(dark) - hueOf(vivid)); d > 3 {
		t.Errorf("gamut clip moved the hue by %.1f°: %s -> %s", d, vivid, dark)
	}
	if got := perceivedL(dark); math.Abs(got-0.25) > 0.01 {
		t.Errorf("gamut clip moved the lightness: asked 0.25, got %.3f (%s)", got, dark)
	}
}

// The canvas is the whole model: with no picture it is the theme's own
// background, and with one it is the worst composite the scrim can leave —
// darker as the dimming comes off under a light theme, brighter under a dark
// one. That monotonicity is what makes the palette follow the slider.
func TestGlyphCanvasFollowsTheScrim(t *testing.T) {
	light := "#f8f9fa" // ayu-light
	dark := "#00172e"  // retro-82

	lumOf := func(hex string) float64 { l, _ := relLuminance(hex); return l }

	flat := glyphCanvasOf(light, false, 0.3) // no image: the scrim is moot
	if flat.Lum != lumOf(light) || !flat.Dark {
		t.Errorf("flat light canvas = %+v, want the theme's own background and dark text", flat)
	}
	if c := glyphCanvasOf(dark, false, 0.3); c.Lum != lumOf(dark) || c.Dark {
		t.Errorf("flat dark canvas = %+v, want the theme's own background and light text", c)
	}

	// Under a picture, less dimming means a worse canvas — and which way
	// "worse" runs is decided by the theme, not by the composite.
	var prev float64 = 2
	for _, scrim := range []float64{1, 0.9, 0.75, 0.5, 0.25} {
		c := glyphCanvasOf(light, true, scrim)
		if !c.Dark {
			t.Errorf("light theme at scrim %.2f: text polarity flipped", scrim)
		}
		if c.Lum >= prev {
			t.Errorf("light theme at scrim %.2f: bound %.3f did not fall below %.3f", scrim, c.Lum, prev)
		}
		prev = c.Lum
	}
	prev = -1
	for _, scrim := range []float64{1, 0.9, 0.75, 0.5, 0.25} {
		c := glyphCanvasOf(dark, true, scrim)
		if c.Dark {
			t.Errorf("dark theme at scrim %.2f: text polarity flipped", scrim)
		}
		if c.Lum <= prev {
			t.Errorf("dark theme at scrim %.2f: bound %.3f did not rise above %.3f", scrim, c.Lum, prev)
		}
		prev = c.Lum
	}
}

// The report: Ayu Light under a wallpaper, where omp's inline code and its
// blockquotes came out as pale text on a pale sky. Both are palette accents
// (green, yellow) that never had text contrast to begin with, and the pass has
// to fix them without turning them into some other colour.
func TestOmpTextLegibleUnderBackdrop(t *testing.T) {
	rt := resolveThemeByName("ayu-light")
	cv := glyphCanvasOf(rt.ui.PanelBg, true, 0.75)
	got := ompColors(rt.ui, cv)

	for _, tc := range []struct {
		tok, was string
	}{
		{"mdCode", rt.ui.Green},
		{"mdQuote", rt.ui.Yellow},
		{"text", rt.ui.Text},
		{"error", rt.ui.Red},
		{"mdHeading", rt.ui.Mauve},
	} {
		if c := cv.contrast(tc.was); c >= legibleBody {
			t.Fatalf("%s: premise broken — %s already reads at %.2f:1", tc.tok, tc.was, c)
		}
		if c := cv.contrast(got[tc.tok]); c < legibleBody {
			t.Errorf("%s = %s at %.2f:1, want >= %.1f", tc.tok, got[tc.tok], c, legibleBody)
		}
		// Still the theme's colour: a green that reads as brown is a different
		// bug from a green nobody can read.
		if chromaOf(tc.was) > 0.02 {
			if d := math.Abs(hueOf(got[tc.tok]) - hueOf(tc.was)); d > 5 && d < 355 {
				t.Errorf("%s: hue moved %.1f° (%s -> %s)", tc.tok, d, tc.was, got[tc.tok])
			}
		}
	}

	// The secondary tier stays secondary: legible, and still quieter than body
	// text, or the hierarchy a TUI leans on is gone.
	quiet, body := cv.contrast(got["muted"]), cv.contrast(got["text"])
	if quiet < legibleQuiet {
		t.Errorf("muted = %s at %.2f:1, want >= %.1f", got["muted"], quiet, legibleQuiet)
	}
	if quiet >= body {
		t.Errorf("muted (%.2f:1) is not quieter than text (%.2f:1)", quiet, body)
	}

	// Shape is not words: fills and borders are the theme's own, whatever the
	// backdrop does, or a re-theme would be redecorating omp's frames too.
	for tok, want := range map[string]string{
		"border":        rt.ui.Surface1,
		"borderMuted":   rt.ui.Surface0,
		"userMessageBg": rt.ui.SurfaceDim,
		"statusLineBg":  rt.ui.Surface0,
		"mdHr":          rt.ui.Overlay0,
		"thinkingHigh":  rt.ui.Mauve,
	} {
		if got[tok] != want {
			t.Errorf("%s = %s, want the mapped %s untouched", tok, got[tok], want)
		}
	}
}

// Claude Code's half of the same fix, and the tokens that must stay out of it:
// the diff bands are backgrounds it draws text ON, and inverseText is the
// theme's background painted on an accent fill.
func TestClaudeTextLegibleUnderBackdrop(t *testing.T) {
	rt := resolveThemeByName("ayu-light")
	cv := glyphCanvasOf(rt.ui.PanelBg, true, 0.75)
	got := claudeOverrides(rt.ui, cv)

	for _, tok := range []string{"text", "warning", "success", "error", "planMode", "permission"} {
		if c := cv.contrast(got[tok]); c < legibleBody {
			t.Errorf("%s = %s at %.2f:1, want >= %.1f", tok, got[tok], c, legibleBody)
		}
	}
	if c := cv.contrast(got["subtle"]); c < legibleQuiet {
		t.Errorf("subtle = %s at %.2f:1, want >= %.1f", got["subtle"], c, legibleQuiet)
	}
	if got["inverseText"] != rt.ui.PanelBg {
		t.Errorf("inverseText = %s, want the panel background %s", got["inverseText"], rt.ui.PanelBg)
	}
	if got["background"] != rt.ui.PanelBg {
		t.Errorf("background = %s, want %s", got["background"], rt.ui.PanelBg)
	}
	// The diff bands keep their own derivation (a fixed lift over the panel),
	// which the contrast pass must not have walked over.
	for _, tok := range []string{"diffAdded", "diffAddedWord", "diffRemoved", "diffRemovedWord"} {
		if got[tok] != claudeOverrides(rt.ui, glyphCanvasOf(rt.ui.PanelBg, false, 1))[tok] {
			t.Errorf("%s moved with the backdrop; diff bands are backgrounds", tok)
		}
	}
}

// Two invariants across every theme this build can resolve, at every dimming a
// human can dial: the pass never makes text harder to read, and it never
// repaints a colour for a gain that leaves it unreadable anyway (which is what
// turned retro-82 under a 0.33 scrim into pastels exactly as illegible as the
// palette they replaced).
func TestLegibleNeverWorsensAnyTheme(t *testing.T) {
	for _, opt := range themeOptions {
		rt := resolveThemeByName(opt.Name)
		u := rt.ui
		for _, scrim := range []float64{1, 0.9, 0.75, 0.6, 0.33} {
			for _, image := range []bool{false, true} {
				cv := glyphCanvasOf(u.PanelBg, image, scrim)
				for _, was := range []string{
					u.Text, u.Subtext0, u.Overlay0, u.Overlay1, u.Accent,
					u.Green, u.Yellow, u.Red, u.Blue, u.Teal, u.Mauve, u.Peach,
				} {
					for _, tier := range []float64{legibleBody, legibleQuiet} {
						got := cv.legible(was, tier)
						before, after := cv.contrast(was), cv.contrast(got)
						if after < before-0.001 {
							t.Errorf("%s scrim=%.2f image=%v tier=%.1f: %s (%.2f:1) -> %s (%.2f:1) is worse",
								opt.Name, scrim, image, tier, was, before, got, after)
						}
						if got != was && after < legibleQuiet {
							t.Errorf("%s scrim=%.2f image=%v tier=%.1f: repainted %s -> %s for %.2f:1, still unreadable",
								opt.Name, scrim, image, tier, was, got, after)
						}
						if _, _, _, ok := hexRGB(got); !ok {
							t.Fatalf("%s: %q is not a hex colour", opt.Name, got)
						}
					}
				}
			}
		}
	}
}

// A backdrop is stored per theme, and only the two fields that change what
// "legible" means may cost a fleet-wide re-mirror.
func TestBackdropChangeDetection(t *testing.T) {
	scrim := func(v float64) *float64 { return &v }
	with := func(m map[string]atmospherePref) uiState {
		return uiState{ThemeAtmosphere: m}
	}
	flat := with(nil)
	pic := with(map[string]atmospherePref{"ayu-light": {Background: "/x.jpg"}})
	dimmer := with(map[string]atmospherePref{"ayu-light": {Background: "/x.jpg", Scrim: scrim(0.4)}})
	other := with(map[string]atmospherePref{"nord": {Background: "/y.jpg"}})
	off := with(map[string]atmospherePref{"ayu-light": {Background: atmosphereNoBackground}})
	shade := with(map[string]atmospherePref{"ayu-light": {Background: "/x.jpg", Shading: new(bool)}})

	for _, tc := range []struct {
		name         string
		before, arg2 uiState
		want         bool
	}{
		{"picture added", flat, pic, true},
		{"picture turned off", pic, off, true},
		{"dimming moved", pic, dimmer, true},
		{"nothing moved", pic, pic, false},
		{"another theme dressed", flat, other, false},
		{"shading toggled", pic, shade, false},
		{"flat stays flat", flat, off, false},
	} {
		if got := backdropChanged(tc.before, tc.arg2, "ayu-light"); got != tc.want {
			t.Errorf("%s: backdropChanged = %v, want %v", tc.name, got, tc.want)
		}
	}
	if backdropChanged(flat, pic, "") {
		t.Error("a write with no theme resolved must not trigger a re-mirror")
	}
	// retro-82 is the one theme that wears a still with nothing stored, so an
	// absent entry there is an image — and a stored picture is not a change.
	if image, _ := backdropOf("retro-82", atmospherePref{}); !image {
		t.Error("retro-82 with no stored entry should wear its bundled default")
	}
	if image, _ := backdropOf("nord", atmospherePref{}); image {
		t.Error("a theme with no bundled default should start flat")
	}
}

// The convergence record is what catches a machine up, so it has to notice the
// half of the state that carries no theme name: a host that slept through a
// dimming change is behind, and a host that was never written to at all must
// not be told it is in step by a pass that only rewrote agent theme files.
func TestThemeStampCarriesTheBackdrop(t *testing.T) {
	t.Cleanup(func() { forgetThemeSynced("box") })
	forgetThemeSynced("box")

	first := themeStamp{name: "ayu-light", legibility: "img/0.75"}
	if !claimThemeConverge("box", first) {
		t.Fatal("first push refused")
	}
	releaseThemeConverge("box")
	markThemeSynced("box", first)

	if claimThemeConverge("box", first) {
		t.Error("a host already in step was pushed again")
	}
	releaseThemeConverge("box")

	dimmed := themeStamp{name: "ayu-light", legibility: "img/0.40"}
	if !claimThemeConverge("box", dimmed) {
		t.Error("a host that slept through a dimming change was not caught up")
	}
	releaseThemeConverge("box")

	// A re-fingerprint applies to the host's own theme only.
	markThemeSynced("box", first)
	restampLegibility("box", "nord", "flat")
	if name, _ := themeSyncedFor("box"); name != "ayu-light" {
		t.Errorf("record moved to another theme: %q", name)
	}
	if claimThemeConverge("box", first) {
		t.Error("restamping another theme's fingerprint disturbed this one")
	}
	releaseThemeConverge("box")
	restampLegibility("box", "ayu-light", "img/0.40")
	if claimThemeConverge("box", dimmed) {
		t.Error("the new fingerprint was not recorded")
	}
	releaseThemeConverge("box")

	// A host lasso has never written to stays behind: its next probe owes it
	// herdr's config.toml too, not just the files a re-mirror touched.
	forgetThemeSynced("fresh")
	restampLegibility("fresh", "ayu-light", "img/0.40")
	if _, ok := themeSyncedFor("fresh"); ok {
		t.Error("a re-mirror invented a record for a host it never wrote a theme to")
	}
}
