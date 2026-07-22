package main

import (
	"testing"
	"time"
)

// TestUsageStoreRoundTrip verifies last-good readings survive a "restart": saved
// to disk, then loaded back into a cleared in-memory map. This is what keeps the
// footer populated on a cold start that lands mid rate-limit window.
func TestUsageStoreRoundTrip(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // isolate from the real cache file

	usageLastGood.mu.Lock()
	usageLastGood.m = map[string]usageProvider{
		"Claude Code": {
			Name: "Claude Code",
			Limits: []usageLimit{
				{Label: "7-Day Rolling", Percent: 48, ElapsedPct: 35},
			},
		},
	}
	saveUsageStoreLocked()
	// Simulate a fresh process: empty map, not yet loaded.
	usageLastGood.m = map[string]usageProvider{}
	usageLastGood.loaded = false
	loadUsageStoreLocked()
	got, ok := usageLastGood.m["Claude Code"]
	usageLastGood.mu.Unlock()

	if !ok {
		t.Fatal("Claude Code reading did not survive the save/load round-trip")
	}
	if len(got.Limits) != 1 || got.Limits[0].Percent != 48 || got.Limits[0].ElapsedPct != 35 {
		t.Fatalf("round-tripped limit mangled: %+v", got.Limits)
	}
}

// TestUsageBackoff verifies a provider is skipped after a 429 and recovers once
// the cooldown elapses.
func TestUsageBackoff(t *testing.T) {
	const name = "Test Provider"
	usageBackoff.mu.Lock()
	delete(usageBackoff.until, name)
	usageBackoff.mu.Unlock()

	if usageInBackoff(name) {
		t.Fatal("provider should not start in backoff")
	}
	usageNoteRateLimited(name)
	if !usageInBackoff(name) {
		t.Fatal("provider should be in backoff right after a 429")
	}

	// Rewind the cooldown into the past to prove it lifts.
	usageBackoff.mu.Lock()
	usageBackoff.until[name] = time.Now().Add(-time.Second)
	usageBackoff.mu.Unlock()
	if usageInBackoff(name) {
		t.Fatal("backoff should have lifted once the cooldown passed")
	}
}

// TestElapsedPct checks the pace-notch math: half-way through a window reads ~50%.
func TestElapsedPct(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	got := elapsedPct(reset, 4*time.Hour) // 2h left of a 4h window → 50% elapsed
	if got < 49 || got > 51 {
		t.Fatalf("elapsedPct = %d, want ~50", got)
	}
	if elapsedPct("", time.Hour) != -1 {
		t.Fatal("empty resetsAt should yield -1")
	}
	if elapsedPct(reset, 0) != -1 {
		t.Fatal("zero window should yield -1")
	}
}
