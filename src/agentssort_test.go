package main

import (
	"net/http"
	"testing"
)

// The grid's sort order is a preference several other controls share a patch
// with (the sidebar drag writes on the same endpoint dozens of times a
// second), so the two things that can go wrong are that it is dropped by a
// neighbour's write, or that a client sending garbage persists an order nobody
// chose.
func TestAgentsSortPatch(t *testing.T) {
	openTestDB(t)

	// Every install that predates this field reads as the order the grid
	// always had, not as an empty string the frontend would have to guess at.
	got := postUIState(t, `{"client_id":"A","user_intent":false}`)
	if got.AgentsSort != agentsSortPriority {
		t.Fatalf("fresh install is not priority-sorted: %+v", got.uiState)
	}

	postUIState(t, `{"agents_sort":"alpha","client_id":"A","user_intent":true}`)
	// A neighbouring write that names no sort must leave it alone: the whole
	// value of choosing alpha is that the grid stops rearranging itself, and a
	// sidebar drag silently reverting it would be that bug wearing a disguise.
	got = postUIState(t, `{"files_click_navigates":false,"client_id":"B","user_intent":true}`)
	if got.AgentsSort != agentsSortAlpha {
		t.Fatalf("sort lost by an unrelated patch: %+v", got.uiState)
	}

	stored, err := getUIState()
	if err != nil {
		t.Fatalf("getUIState: %v", err)
	}
	if stored.AgentsSort != agentsSortAlpha {
		t.Fatalf("sort not persisted: %+v", stored)
	}

	// Refused rather than coerced, exactly as appearance_mode is: coercing
	// would persist an order nobody picked and hide the client bug that sent
	// it. The whole patch drops with it, so the caller cannot half-land.
	w := postUIStateRaw(t, `{"agents_sort":"","client_id":"A","user_intent":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf(`agents_sort:"" accepted: %d`, w.Code)
	}
	w = postUIStateRaw(t, `{"agents_sort":"newest","files_click_navigates":true,"client_id":"A","user_intent":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown agents_sort accepted: %d", w.Code)
	}
	stored, err = getUIState()
	if err != nil {
		t.Fatalf("getUIState: %v", err)
	}
	if stored.AgentsSort != agentsSortAlpha || stored.FilesClickNavigates {
		t.Fatalf("a refused patch landed anyway: %+v", stored)
	}
}
