package main

import (
	"net/http"
	"testing"
)

// The Browser tab's mode rides the same patch endpoint as the sidebar drag, so
// it must survive a neighbour's write, default to live on every install that
// predates it, and refuse a value nobody could have picked.
func TestBrowserModePatch(t *testing.T) {
	openTestDB(t)

	got := postUIState(t, `{"client_id":"A","user_intent":false}`)
	if got.BrowserMode != browserModeLive {
		t.Fatalf("fresh install is not live: %+v", got.uiState)
	}

	postUIState(t, `{"browser_mode":"embed","client_id":"A","user_intent":true}`)
	got = postUIState(t, `{"files_click_navigates":false,"client_id":"B","user_intent":true}`)
	if got.BrowserMode != browserModeEmbed {
		t.Fatalf("mode lost by an unrelated patch: %+v", got.uiState)
	}

	w := postUIStateRaw(t, `{"browser_mode":"","client_id":"A","user_intent":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf(`browser_mode:"" accepted: %d`, w.Code)
	}
	w = postUIStateRaw(t, `{"browser_mode":"kiosk","files_click_navigates":true,"client_id":"A","user_intent":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown browser_mode accepted: %d", w.Code)
	}
	stored, err := getUIState()
	if err != nil {
		t.Fatalf("getUIState: %v", err)
	}
	if stored.BrowserMode != browserModeEmbed || stored.FilesClickNavigates {
		t.Fatalf("a refused patch landed anyway: %+v", stored)
	}

	// A hand-edited or future value already in the db reads as the default.
	if normalizeBrowserMode("kiosk") != browserModeLive {
		t.Fatal("unknown stored mode not normalized to live")
	}
}
