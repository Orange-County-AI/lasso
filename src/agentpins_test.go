package main

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// Pins are written by ops rather than as a list, so the things that can go
// wrong are: a pin lost to a neighbouring write, two pins in one coalesced
// patch clobbering each other, a re-pin moving a card, and the list growing
// without bound.
func TestAgentPinsOps(t *testing.T) {
	openTestDB(t)

	got := postUIState(t, `{"client_id":"A","user_intent":false}`)
	if got.PinnedAgents == nil || len(got.PinnedAgents) != 0 {
		t.Fatalf("fresh install has pins (or null): %#v", got.PinnedAgents)
	}

	// Two pins in one patch both land, in a stable order.
	got = postUIState(t, `{"agent_pins":{"titan\u0000w2:p1":true,"titan\u0000w1:p1":true},"client_id":"A","user_intent":true}`)
	want := []string{"titan\x00w1:p1", "titan\x00w2:p1"}
	if !slices.Equal(got.PinnedAgents, want) {
		t.Fatalf("pins = %q, want %q", got.PinnedAgents, want)
	}

	// A later pin goes last; re-pinning an existing one does not move it.
	got = postUIState(t, `{"agent_pins":{"minime\u0000w1:p1":true,"titan\u0000w1:p1":true},"client_id":"B","user_intent":true}`)
	want = []string{"titan\x00w1:p1", "titan\x00w2:p1", "minime\x00w1:p1"}
	if !slices.Equal(got.PinnedAgents, want) {
		t.Fatalf("pins = %q, want %q", got.PinnedAgents, want)
	}

	// A neighbouring write leaves pins alone, and a client sending the list
	// itself cannot replace it.
	got = postUIState(t, `{"files_click_navigates":false,"pinned_agents":[],"client_id":"B","user_intent":true}`)
	if !slices.Equal(got.PinnedAgents, want) {
		t.Fatalf("pins changed by an unrelated patch: %q", got.PinnedAgents)
	}

	got = postUIState(t, `{"agent_pins":{"titan\u0000w2:p1":false},"client_id":"A","user_intent":true}`)
	want = []string{"titan\x00w1:p1", "minime\x00w1:p1"}
	if !slices.Equal(got.PinnedAgents, want) {
		t.Fatalf("unpin: pins = %q, want %q", got.PinnedAgents, want)
	}
	stored, err := getUIState()
	if err != nil {
		t.Fatalf("getUIState: %v", err)
	}
	if !slices.Equal(stored.PinnedAgents, want) {
		t.Fatalf("pins not persisted: %q", stored.PinnedAgents)
	}

	if w := postUIStateRaw(t, `{"agent_pins":{"":true},"client_id":"A","user_intent":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty pin key accepted: %d", w.Code)
	}
}

func TestMergePinnedAgentsCap(t *testing.T) {
	var stored []string
	for i := range maxPinnedAgents {
		stored = append(stored, fmt.Sprintf("h\x00p%03d", i))
	}
	got := mergePinnedAgents(stored, map[string]bool{"h\x00new": true})
	if len(got) != maxPinnedAgents || got[0] != "h\x00p001" || got[len(got)-1] != "h\x00new" {
		t.Fatalf("cap did not drop the oldest pin: first %q last %q len %d", got[0], got[len(got)-1], len(got))
	}
	if got := mergePinnedAgents([]string{"a", "", "a", "b"}, nil); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("repair = %q", got)
	}
}
