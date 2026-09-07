package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Luvus Bar limits: 16 segments and 256 display columns per widget. Every
// provider costs one name segment plus one per limit (and a separator), so the
// four providers with two limits each fit exactly; a fifth would overflow and is
// dropped rather than rendered half-way.
const usageBarMaxSegments = 16

// usageBarWidth is the glyph count of the drawn bar. Six cells at the bottom of
// the screen read at a glance; the percentage beside it carries the precision.
const usageBarWidth = 6

type barSegment struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Tone string `json:"tone,omitempty"`
}

// usageBarOptions is what the module's settings control: which providers are
// shown, in what order, and how compactly.
type usageBarOptions struct {
	Compact bool
	// Order lists provider names to show, in display order. Nil means every
	// provider in the payload's order.
	Order []string
}

// usageBarSegments renders the aggregated usage payload as Luvus Bar segments.
//
// Full: `Claude 5h ▰▰▱▱▱▱ 23%  7d ▰▰▱▱▱▱ 22%▲ ↻3h`. Compact: `Cl 23% 22%▲`.
// Every limit is shown, not just the worst, because the two windows fail
// differently: the 5-hour block is what stops an agent mid-task, the weekly one
// is what stops tomorrow. ▲ marks a limit whose usage is ahead of the elapsed
// share of its window — the pace notch of the old footer, in one glyph — and
// carries the warning tone; an exhausted limit is an error.
//
// Luvus 0.13.4 rejects its documented `progress` segment (its `value` field is
// deserialized as the click payload string), so the bar is drawn with block
// glyphs in a text segment. Revisit when a release accepts the typed shape.
func usageBarSegments(p usagePayload, opts usageBarOptions, now time.Time) []barSegment {
	var out []barSegment
	for _, prov := range orderedProviders(p.Providers, opts.Order) {
		if len(prov.Limits) == 0 {
			continue
		}
		need := 1 + len(prov.Limits)
		if len(out) > 0 {
			need++
		}
		if len(out)+need > usageBarMaxSegments {
			break
		}
		if len(out) > 0 {
			out = append(out, barSegment{Type: "separator"})
		}
		name := prov.Name
		if opts.Compact {
			name = compactProviderName(name)
		}
		out = append(out, barSegment{Type: "text", Text: name + " ", Tone: "muted"})
		for i, l := range prov.Limits {
			var b strings.Builder
			if !opts.Compact {
				if i > 0 {
					b.WriteString(" ")
				}
				b.WriteString(shortLimitLabel(l.Label))
				b.WriteString(" ")
				b.WriteString(drawBar(l.Percent))
				b.WriteString(" ")
			} else if i > 0 {
				b.WriteString(" ")
			}
			fmt.Fprintf(&b, "%d%%", l.Percent)
			if aheadOfPace(l) {
				b.WriteString("▲")
			}
			if !opts.Compact {
				if reset := resetHint(l, now); reset != "" {
					b.WriteString(" " + reset)
				}
			}
			out = append(out, barSegment{Type: "text", Text: b.String(), Tone: usageTone(l)})
		}
	}
	if len(out) == 0 {
		return []barSegment{{Type: "text", Text: "usage: no providers", Tone: "muted"}}
	}
	return out
}

// orderedProviders applies the module's provider allow-list and order. A nil
// order shows everything; providers named but absent from the payload (no
// credentials) are simply skipped.
func orderedProviders(all []usageProvider, order []string) []usageProvider {
	if order == nil {
		return all
	}
	byName := make(map[string]usageProvider, len(all))
	for _, p := range all {
		byName[p.Name] = p
	}
	out := make([]usageProvider, 0, len(order))
	for _, name := range order {
		if p, ok := byName[name]; ok {
			out = append(out, p)
		}
	}
	return out
}

func drawBar(percent int) string {
	filled := (percent*usageBarWidth + 50) / 100
	if filled > usageBarWidth {
		filled = usageBarWidth
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat("▰", filled) + strings.Repeat("▱", usageBarWidth-filled)
}

// aheadOfPace reports whether usage has outrun the clock: past the share of the
// window that has elapsed, so the quota will be hit before it resets.
func aheadOfPace(l usageLimit) bool {
	return l.ElapsedPct >= 0 && l.Percent > l.ElapsedPct
}

func usageTone(l usageLimit) string {
	switch {
	case l.Percent >= 100:
		return "error"
	case l.Percent >= 90 || aheadOfPace(l):
		return "warning"
	default:
		return "success"
	}
}

func compactProviderName(name string) string {
	switch name {
	case "Claude Code":
		return "Cl"
	case "Kimi Code":
		return "Ki"
	case "Codex":
		return "Cx"
	case "Z.ai":
		return "Z"
	}
	if len(name) > 2 {
		return name[:2]
	}
	return name
}

// shortLimitLabel reduces a provider's window label to the window itself.
func shortLimitLabel(label string) string {
	lower := strings.ToLower(strings.TrimSpace(label))
	// "5h Limit" / "30m Limit" (Kimi, Z.ai): the duration is the whole label.
	if i := strings.IndexAny(lower, "hm"); i > 0 && i+1 < len(lower) && lower[i+1] == ' ' {
		if _, err := fmt.Sscanf(lower[:i], "%d", new(int)); err == nil {
			return lower[:i+1]
		}
	}
	switch {
	case strings.HasSuffix(lower, " weekly") && !strings.HasPrefix(lower, "weekly"):
		// A model-scoped weekly window ("Fable Weekly"): the model is the label.
		return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(label), " Weekly"))
	case strings.Contains(lower, "week"), strings.Contains(lower, "7-day"):
		return "7d"
	case strings.Contains(lower, "month"):
		return "mo"
	case strings.Contains(lower, "hour"):
		return "5h"
	case strings.Contains(lower, "day"):
		return "day"
	}
	if len(lower) > 6 {
		return lower[:6]
	}
	return lower
}

func resetHint(l usageLimit, now time.Time) string {
	if l.ResetsAt == "" {
		return ""
	}
	at, err := time.Parse(time.RFC3339, l.ResetsAt)
	if err != nil {
		return ""
	}
	d := at.Sub(now)
	if d <= 0 {
		return ""
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("↻%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("↻%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("↻%dd", int(d.Hours()/24))
	}
}

// cliUsageBar prints `{"content":[…],"compact_content":[…]}` for the usage-bar
// module: the full form and the form Luvus falls back to when the row is too
// narrow — without a compact form Luvus hides the whole widget behind "… +N".
// It runs the same provider fetchers the footer did, sharing the on-disk
// last-good cache, so the widget works whether or not a lasso server is up.
//
//	lasso usage-bar [-compact] [-providers "Claude Code,Codex"]
//
// -compact makes the full form compact too (the module's setting). -providers
// is the visible-provider list in display order, assembled by the module script
// from its per-provider settings.
func cliUsageBar(args []string) {
	opts := usageBarOptions{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-compact", "--compact":
			opts.Compact = true
		case "-providers", "--providers":
			i++
			if i >= len(args) {
				fatal("usage-bar: -providers needs a comma-separated list")
			}
			opts.Order = []string{}
			for _, n := range strings.Split(args[i], ",") {
				if n = strings.TrimSpace(n); n != "" {
					opts.Order = append(opts.Order, n)
				}
			}
		default:
			fatal("usage-bar: unknown argument " + args[i])
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	payload := collectUsage(ctx)
	now := time.Now()
	compact := opts
	compact.Compact = true
	out, err := json.Marshal(struct {
		Content        []barSegment `json:"content"`
		CompactContent []barSegment `json:"compact_content"`
	}{usageBarSegments(payload, opts, now), usageBarSegments(payload, compact, now)})
	if err != nil {
		fatal("usage-bar: " + err.Error())
	}
	os.Stdout.Write(append(out, '\n'))
}
