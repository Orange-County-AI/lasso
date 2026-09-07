package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestUsageBarSegmentsShowWorstLimitPerProvider(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	payload := usagePayload{Providers: []usageProvider{
		{Name: "Claude Code", Limits: []usageLimit{
			{Label: "5-hour", Percent: 20, ElapsedPct: 50},
			{Label: "Weekly", Percent: 95, ElapsedPct: 40, ResetsAt: now.Add(30 * time.Hour).Format(time.RFC3339)},
		}},
		{Name: "Codex", Limits: []usageLimit{{Label: "5-hour", Percent: 100, ElapsedPct: 10}}},
		{Name: "Kimi Code", Err: "no credentials"},
	}}
	segs := usageBarSegments(payload, false, now)
	if len(segs) != 5 { // Claude name+value, separator, Codex name+value
		t.Fatalf("segments = %+v", segs)
	}
	if segs[1].Text != "95% wk ↻30h" || segs[1].Tone != "warning" {
		t.Errorf("claude segment = %+v, want the weekly limit ahead of pace", segs[1])
	}
	if segs[4].Tone != "error" {
		t.Errorf("exhausted quota tone = %q, want error", segs[4].Tone)
	}
	// Every segment must be a shape Luvus accepts (text or separator only).
	for _, s := range segs {
		if s.Type != "text" && s.Type != "separator" {
			t.Errorf("unexpected segment type %q", s.Type)
		}
	}
	compact := usageBarSegments(payload, true, now)
	if compact[0].Text != "Cl " || compact[1].Text != "95%" {
		t.Errorf("compact = %+v", compact[:2])
	}
	if _, err := json.Marshal(compact); err != nil {
		t.Fatal(err)
	}
}

func TestUsageBarSegmentsWithoutProviders(t *testing.T) {
	segs := usageBarSegments(usagePayload{}, false, time.Now())
	if len(segs) != 1 || segs[0].Tone != "muted" {
		t.Fatalf("empty payload rendered %+v", segs)
	}
}
