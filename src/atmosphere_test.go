package main

import "testing"

// The atmosphere is the first part of ui_state that two browsers edit at the
// same time without either one having seen the other's copy: it is written from
// a Settings pane that may be open on a laptop and a phone, and each pick names
// only the theme that browser happens to be wearing. A whole-object write — or
// a decode that replaces a map's named entry wholesale — silently drops the
// other browser's backdrop, which reads as a theme spontaneously going flat.
func TestAtmosphereMergesPerThemeAndPerField(t *testing.T) {
	openTestDB(t)

	// One browser dresses the theme it is wearing.
	postUIState(t, `{"theme_atmosphere":{"retro-82":{"background":"/wallpapers/retro-82/05-gateway.webp"}},"client_id":"A","user_intent":true}`)
	// Another, which has never heard of that pick, dresses a different theme.
	postUIState(t, `{"theme_atmosphere":{"rose-pine":{"shading":false}},"client_id":"B","user_intent":true}`)
	// And the first nudges one knob of its own theme, saying nothing about the
	// picture it chose a moment ago.
	got := postUIState(t, `{"theme_atmosphere":{"retro-82":{"scrim":0.42}},"client_id":"A","user_intent":true}`)

	retro := got.ThemeAtmosphere["retro-82"]
	if retro.Background != "/wallpapers/retro-82/05-gateway.webp" {
		t.Fatalf("picture lost by a later scrim write: %+v", retro)
	}
	if retro.Scrim == nil || *retro.Scrim != 0.42 {
		t.Fatalf("scrim not stored: %+v", retro)
	}
	rose, ok := got.ThemeAtmosphere["rose-pine"]
	if !ok || rose.Shading == nil || *rose.Shading {
		t.Fatalf("another theme's entry clobbered: %+v", got.ThemeAtmosphere)
	}

	// A pick made before /api/theme resolved has no theme to belong to; storing
	// it under "" would both lose it and leave an entry nothing ever reads.
	got = postUIState(t, `{"theme_atmosphere":{"":{"shading":true}},"client_id":"A","user_intent":true}`)
	if _, ok := got.ThemeAtmosphere[""]; ok {
		t.Fatalf("stored an entry under no theme: %+v", got.ThemeAtmosphere)
	}

	// Out-of-range dimming is clamped rather than stored: the wash is an alpha,
	// and a CSS rgba() built from 4.2 paints nothing.
	got = postUIState(t, `{"theme_atmosphere":{"retro-82":{"scrim":4.2}},"client_id":"A","user_intent":true}`)
	if s := got.ThemeAtmosphere["retro-82"].Scrim; s == nil || *s != 1 {
		t.Fatalf("scrim not clamped: %+v", got.ThemeAtmosphere["retro-82"])
	}
}

// The gallery of hand-given pictures is written by op, not as a list, so a tab
// holding a copy from ten minutes ago cannot resurrect a picture somebody just
// forgot — or drop one they just added.
func TestCustomBackgroundGalleryOps(t *testing.T) {
	openTestDB(t)

	postUIState(t, `{"remember_background":"/api/file?path=/tmp/one.png","client_id":"A","user_intent":true}`)
	got := postUIState(t, `{"remember_background":"/api/file?path=/tmp/two.png","client_id":"B","user_intent":true}`)
	if len(got.CustomBackgrounds) != 2 || got.CustomBackgrounds[0] != "/api/file?path=/tmp/two.png" {
		t.Fatalf("newest first: %v", got.CustomBackgrounds)
	}

	// Re-adding a picture moves it to the front instead of doubling it, which is
	// what makes pasting the same URL twice harmless.
	got = postUIState(t, `{"remember_background":"/api/file?path=/tmp/one.png","client_id":"A","user_intent":true}`)
	if len(got.CustomBackgrounds) != 2 || got.CustomBackgrounds[0] != "/api/file?path=/tmp/one.png" {
		t.Fatalf("re-add duplicated or did not promote: %v", got.CustomBackgrounds)
	}

	got = postUIState(t, `{"forget_background":"/api/file?path=/tmp/one.png","client_id":"B","user_intent":true}`)
	if len(got.CustomBackgrounds) != 1 || got.CustomBackgrounds[0] != "/api/file?path=/tmp/two.png" {
		t.Fatalf("forget removed the wrong picture: %v", got.CustomBackgrounds)
	}
}

// Two collections share one blob with the sidebar and the usage footer, and the
// no-op check that guards the rev bump compares the merge against the stored
// state. A decode writing through the map `stored` still points at would make
// that comparison compare a value with itself: the write is answered 200, the
// db never changes, and the pick is gone on the next reload.
func TestAtmospherePatchIsActuallyPersisted(t *testing.T) {
	openTestDB(t)

	postUIState(t, `{"theme_atmosphere":{"retro-82":{"background":"none"}},"client_id":"A","user_intent":true}`)
	stored, err := getUIState()
	if err != nil {
		t.Fatalf("getUIState: %v", err)
	}
	if stored.ThemeAtmosphere["retro-82"].Background != atmosphereNoBackground {
		t.Fatalf("choice not persisted: %+v", stored.ThemeAtmosphere)
	}
}
