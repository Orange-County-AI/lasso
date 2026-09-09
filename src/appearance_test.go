package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// postUIStateRaw drives the handler without asserting the status, which is the
// half postUIState cannot cover: a rejected write is part of this contract.
func postUIStateRaw(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/ui-state", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	serveUIState(w, r)
	return w
}

// Appearance is three fields written by three different Settings controls (the
// mode segmented control, and one palette picker per scheme), so a patch
// naming one must leave the other two exactly as they were — otherwise picking
// a light palette silently drops the dark one, or flips the mode back to herdr.
func TestAppearancePatchPreservesSiblingFields(t *testing.T) {
	openTestDB(t)

	// A fresh install follows herdr with no palette of its own: the behavior
	// that existed before these fields did.
	got := postUIState(t, `{"client_id":"A","user_intent":false}`)
	if got.AppearanceMode != appearanceModeHerdr || got.PaletteLight != "" || got.PaletteDark != "" {
		t.Fatalf("fresh install is not herdr/flat: %+v", got.uiState)
	}

	postUIState(t, `{"appearance_mode":"system","client_id":"A","user_intent":true}`)
	postUIState(t, `{"palette_dark":"rose-pine","client_id":"A","user_intent":true}`)
	got = postUIState(t, `{"palette_light":"rose-pine-dawn","client_id":"B","user_intent":true}`)

	if got.AppearanceMode != appearanceModeSystem {
		t.Fatalf("mode lost by a palette write: %+v", got.uiState)
	}
	if got.PaletteDark != "rose-pine" || got.PaletteLight != "rose-pine-dawn" {
		t.Fatalf("palettes clobbered each other: %+v", got.uiState)
	}

	// And it is really in the db, not merely in the response echo.
	stored, err := getUIState()
	if err != nil {
		t.Fatalf("getUIState: %v", err)
	}
	if stored.AppearanceMode != appearanceModeSystem || stored.PaletteDark != "rose-pine" || stored.PaletteLight != "rose-pine-dawn" {
		t.Fatalf("appearance not persisted: %+v", stored)
	}

	// "" is a real choice — back to the shared herdr palette for that scheme —
	// and must not read as "field absent, keep what you had".
	got = postUIState(t, `{"palette_dark":"","client_id":"A","user_intent":true}`)
	if got.PaletteDark != "" {
		t.Fatalf("explicit clear ignored: %+v", got.uiState)
	}
	if got.PaletteLight != "rose-pine-dawn" || got.AppearanceMode != appearanceModeSystem {
		t.Fatalf("clearing one palette disturbed the rest: %+v", got.uiState)
	}
	stored, err = getUIState()
	if err != nil {
		t.Fatalf("getUIState: %v", err)
	}
	if stored.PaletteDark != "" {
		t.Fatalf("clear not persisted: %+v", stored)
	}
}

// A mode the frontend cannot switch on is worse than a refused write: it would
// be persisted, broadcast to every other tab over ui_state_rev, and read back
// forever. So it is a client error, and it takes the whole patch down with it
// rather than landing the rest around a preference nobody chose.
func TestAppearanceModeRejectsUnknownValue(t *testing.T) {
	openTestDB(t)

	postUIState(t, `{"appearance_mode":"dark","palette_dark":"rose-pine","client_id":"A","user_intent":true}`)

	for _, bad := range []string{`"sepia"`, `""`, `"HERDR"`} {
		w := postUIStateRaw(t, `{"appearance_mode":`+bad+`,"palette_dark":"gruvbox","client_id":"A","user_intent":true}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("appearance_mode %s answered %d: %s", bad, w.Code, w.Body.String())
		}
	}

	stored, err := getUIState()
	if err != nil {
		t.Fatalf("getUIState: %v", err)
	}
	if stored.AppearanceMode != appearanceModeDark || stored.PaletteDark != "rose-pine" {
		t.Fatalf("a refused write corrupted the stored preferences: %+v", stored)
	}
}

// Every existing install has a ui_state blob that predates these fields, and a
// hand-edited (or downgraded-then-upgraded) one can hold a mode this build
// does not know. Reads must answer something the frontend can switch on, and a
// later patch must not carry the junk forward.
func TestAppearanceModeNormalizedOnRead(t *testing.T) {
	openTestDB(t)

	// A blob from before appearance existed.
	if err := setSetting("ui_state", `{"sidebar_pct":33,"usage_compact":true}`); err != nil {
		t.Fatalf("setSetting: %v", err)
	}
	us, err := getUIState()
	if err != nil {
		t.Fatalf("getUIState: %v", err)
	}
	if us.AppearanceMode != appearanceModeHerdr {
		t.Fatalf("legacy blob did not default to herdr: %q", us.AppearanceMode)
	}
	if us.SidebarPct != 33 || !us.UsageCompact {
		t.Fatalf("normalization disturbed the rest of the blob: %+v", us)
	}

	// And a stored mode this build does not recognize.
	if err := setSetting("ui_state", `{"appearance_mode":"sepia"}`); err != nil {
		t.Fatalf("setSetting: %v", err)
	}
	us, err = getUIState()
	if err != nil {
		t.Fatalf("getUIState: %v", err)
	}
	if us.AppearanceMode != appearanceModeHerdr {
		t.Fatalf("unknown stored mode not repaired: %q", us.AppearanceMode)
	}
	// A patch about something else must not be refused because of what was
	// already in the db, and must write the repaired mode back.
	got := postUIState(t, `{"usage_compact":true,"client_id":"A","user_intent":true}`)
	if got.AppearanceMode != appearanceModeHerdr {
		t.Fatalf("unrelated patch carried the junk mode forward: %+v", got.uiState)
	}
}
