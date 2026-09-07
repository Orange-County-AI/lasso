package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampleUsage(now time.Time) usagePayload {
	return usagePayload{Providers: []usageProvider{
		{Name: "Claude Code", Limits: []usageLimit{
			{Label: "5-Hour Block", Percent: 23, ElapsedPct: 41, ResetsAt: now.Add(2 * time.Hour).Format(time.RFC3339)},
			{Label: "7-Day Rolling", Percent: 22, ElapsedPct: 12, ResetsAt: now.Add(30 * time.Hour).Format(time.RFC3339)},
		}},
		{Name: "Codex", Limits: []usageLimit{{Label: "Weekly", Percent: 100, ElapsedPct: 10}}},
		{Name: "Kimi Code", Err: "no credentials"},
	}}
}

func TestUsageBarSegmentsRenderEveryLimitWithPace(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	segs := usageBarSegments(sampleUsage(now), usageBarOptions{}, now)
	// Claude: name + 2 limits; separator; Codex: name + 1 limit. Kimi has no limits.
	if len(segs) != 6 {
		t.Fatalf("segments = %+v", segs)
	}
	fiveHour, weekly, codex := segs[1], segs[2], segs[5]
	if !strings.HasPrefix(fiveHour.Text, "5h ") || !strings.Contains(fiveHour.Text, "23%") || strings.Contains(fiveHour.Text, "▲") || fiveHour.Tone != "success" {
		t.Errorf("on-pace limit = %+v", fiveHour)
	}
	if !strings.Contains(weekly.Text, "22%▲") || !strings.Contains(weekly.Text, "↻30h") || weekly.Tone != "warning" {
		t.Errorf("ahead-of-pace limit = %+v, want ▲ + reset + warning", weekly)
	}
	if codex.Tone != "error" || !strings.Contains(codex.Text, "▰▰▰▰▰▰") {
		t.Errorf("exhausted limit = %+v, want a full bar in error tone", codex)
	}
	for _, s := range segs {
		if s.Type != "text" && s.Type != "separator" {
			t.Errorf("unexpected segment type %q", s.Type)
		}
	}
	if _, err := json.Marshal(segs); err != nil {
		t.Fatal(err)
	}
}

func TestUsageBarCompactAndProviderOrder(t *testing.T) {
	now := time.Now()
	segs := usageBarSegments(sampleUsage(now), usageBarOptions{Compact: true, Order: []string{"Codex", "Claude Code"}}, now)
	if segs[0].Text != "Cx " || segs[1].Text != "100%▲" {
		t.Fatalf("order/compact not applied: %+v", segs[:2])
	}
	if segs[3].Text != "Cl " || segs[4].Text != "23%" || segs[5].Text != " 22%▲" {
		t.Fatalf("compact claude = %+v", segs[3:])
	}
	// An explicit empty order hides everything.
	if got := usageBarSegments(sampleUsage(now), usageBarOptions{Order: []string{}}, now); got[0].Tone != "muted" {
		t.Fatalf("empty allow-list rendered %+v", got)
	}
}

func TestUsageBarStaysWithinSegmentLimit(t *testing.T) {
	var p usagePayload
	for i := 0; i < 10; i++ {
		p.Providers = append(p.Providers, usageProvider{Name: strings.Repeat("P", i+1), Limits: []usageLimit{{Percent: 1}, {Percent: 2}}})
	}
	if n := len(usageBarSegments(p, usageBarOptions{}, time.Now())); n > usageBarMaxSegments {
		t.Fatalf("%d segments exceeds Luvus's cap of %d", n, usageBarMaxSegments)
	}
}
