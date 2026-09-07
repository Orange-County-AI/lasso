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

// The full form shows each provider's worst limit as a bar with its figure; the
// pace marker and tone follow that limit.
func TestUsageBarFullFormBarsTheWorstLimit(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	p := sampleUsage(now)
	p.Providers[0].Limits[1].Percent = 60 // weekly now the worse of Claude's two
	segs := usageBarSegments(p, usageBarOptions{}, now)
	// Claude: name + bar; separator; Codex: name + bar. Kimi has no limits.
	if len(segs) != 5 {
		t.Fatalf("segments = %+v", segs)
	}
	if segs[0].Text != "Cl " || segs[1].Text != "▰▰▰▱▱ 60%▲" || segs[1].Tone != "warning" {
		t.Errorf("claude = %+v, want the ahead-of-pace weekly bar in warning tone", segs[:2])
	}
	if codex := segs[4]; codex.Tone != "error" || codex.Text != "▰▰▰▰▰ 100%▲" {
		t.Errorf("exhausted limit = %+v, want a full bar in error tone", codex)
	}
	for _, s := range segs {
		if s.Type != "text" && s.Type != "separator" {
			t.Errorf("unexpected segment type %q", s.Type)
		}
	}
	// The whole point: four two-limit providers must stay inside the ~66
	// columns Luvus leaves the bottom-right region, or it never shows the form.
	var four usagePayload
	for _, n := range []string{"Claude Code", "Kimi Code", "Codex", "Z.ai"} {
		four.Providers = append(four.Providers, usageProvider{Name: n, Limits: []usageLimit{{Percent: 47, ElapsedPct: 60}, {Percent: 28, ElapsedPct: 10}}})
	}
	if w := displayWidth(usageBarSegments(four, usageBarOptions{}, now)); w > 66 {
		t.Errorf("four-provider full form is %d columns, wider than the region budget", w)
	}
}

// displayWidth approximates Luvus's segment_width: text runes, bar cells for
// progress, three cells for a separator (" · ").
func displayWidth(segs []barSegment) int {
	w := 0
	for _, s := range segs {
		switch s.Type {
		case "text":
			w += len([]rune(s.Text))
		case "progress":
			w += s.Width
		case "separator":
			w += 3
		}
	}
	return w
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
	for _, opts := range []usageBarOptions{{}, {Native: true}, {Compact: true}} {
		if n := len(usageBarSegments(p, opts, time.Now())); n > usageBarMaxSegments {
			t.Fatalf("%+v: %d segments exceeds Luvus's cap of %d", opts, n, usageBarMaxSegments)
		}
	}
}

// The native shape is the full form with the bar as a `progress` segment: name,
// progress, figure per provider.
func TestUsageBarNativeProgressOnWorstLimit(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	p := sampleUsage(now)
	p.Providers[0].Limits[1].Percent = 60 // weekly now the worse of Claude's two
	segs := usageBarSegments(p, usageBarOptions{Native: true}, now)
	// Claude: name, progress, figure; separator; Codex: name, progress, figure.
	if len(segs) != 7 {
		t.Fatalf("segments = %+v", segs)
	}
	bar := segs[1]
	if segs[0].Text != "Cl " || bar.Type != "progress" || bar.Value == nil || *bar.Value != 60 || bar.Total != 100 || bar.Width != usageBarWidth || bar.Tone != "warning" {
		t.Errorf("claude = %+v", segs[:3])
	}
	if segs[2].Text != " 60%▲" || segs[2].Tone != "warning" {
		t.Errorf("figure = %+v", segs[2])
	}
	if codex := segs[5]; codex.Type != "progress" || *codex.Value != 100 || codex.Tone != "error" {
		t.Errorf("exhausted limit = %+v", codex)
	}
	// A value past the cap is clamped to the total: Luvus refuses value > total.
	p.Providers[1].Limits[0].Percent = 130
	if segs := usageBarSegments(p, usageBarOptions{Native: true}, now); *segs[5].Value != 100 {
		t.Errorf("over-cap value = %d, want 100", *segs[5].Value)
	}
	// Four two-limit providers: 3 each + 3 separators = 15 <= cap.
	var four usagePayload
	for i := 0; i < 4; i++ {
		four.Providers = append(four.Providers, usageProvider{Name: strings.Repeat("P", i+1), Limits: []usageLimit{{Percent: 1}, {Percent: 2}}})
	}
	if n := len(usageBarSegments(four, usageBarOptions{Native: true}, now)); n != 15 {
		t.Errorf("four providers rendered %d native segments, want 15", n)
	}
	// Text-only marshalling never leaks empty progress fields.
	b, _ := json.Marshal(segs[0])
	if strings.Contains(string(b), "value") || strings.Contains(string(b), "width") {
		t.Errorf("text segment carries progress fields: %s", b)
	}
}
