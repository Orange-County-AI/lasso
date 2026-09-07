package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Luvus Bar limits: 16 segments and 256 display columns per widget (16 accepted,
// 17 refused — probed), and the tighter region budget described on
// usageBarSegments. Per provider the compact form costs one name segment plus
// one per limit, the glyph form two, the native form three (name, progress,
// figure), plus a separator between providers: four providers fit every form.
// A fifth overflows and is dropped rather than rendered half-way.
const usageBarMaxSegments = 16

// usageBarWidth is the cell count of the drawn bar. Five cells keep four
// providers inside the region budget; the percentage beside it carries the
// precision.
const usageBarWidth = 5

type barSegment struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Tone string `json:"tone,omitempty"`
	// progress fields. Value is numeric: Luvus 0.13.4 release deserializes it as
	// the click-payload string and refuses the segment; the fix
	// (luvus fix/bar-progress-segment) takes the number.
	Value *int `json:"value,omitempty"`
	Total int  `json:"total,omitempty"`
	Width int  `json:"width,omitempty"`
}

// usageBarOptions is what the module's settings control: which providers are
// shown, in what order, and how compactly — plus which bar shape to draw.
type usageBarOptions struct {
	Compact bool
	// Order lists provider names to show, in display order. Nil means every
	// provider in the payload's order.
	Order []string
	// Native draws Luvus `progress` segments instead of block glyphs in a text
	// segment. Only a patched Luvus accepts them; the module script pushes
	// this form first and falls back to the glyph form when it is refused.
	Native bool
}

// usageBarSegments renders the aggregated usage payload as Luvus Bar segments.
//
// Full: `Cl ▰▰▱▱▱ 47% · Ki ▰▰▰▱▱ 51% · Cx ▱▱▱▱▱ 6%▲ · Z ▱▱▱▱▱ 1%` — each
// provider's bar is its worst limit (the one nearer its cap is what stops
// tomorrow); Native draws the bar as a `progress` segment instead of glyphs.
// Compact: `Cl 47% 28% 16%▲ · Ki 0% 51%`, every limit's figure and no bar.
// ▲ marks a limit whose usage is ahead of the elapsed share of its window and
// carries the warning tone; an exhausted limit is an error. Reset times and
// limit labels are the Usage tab's; there is no room for them here.
//
// Width is the constraint, not the segment count: Luvus caps the bottom-right
// region at 100 columns (MAX_BAR_REGION_WIDTH) shared with its own runtime
// status widget, so a widget wider than ~66 columns is shown compact on any
// terminal. Four providers at ~14 columns each fit; the compact form is what
// Luvus falls back to when they do not.
func usageBarSegments(p usagePayload, opts usageBarOptions, now time.Time) []barSegment {
	var out []barSegment
	for _, prov := range orderedProviders(p.Providers, opts.Order) {
		if len(prov.Limits) == 0 {
			continue
		}
		need := 2
		if opts.Compact {
			need = 1 + len(prov.Limits)
		} else if opts.Native {
			need = 3
		}
		if len(out) > 0 {
			need++
		}
		if len(out)+need > usageBarMaxSegments {
			break
		}
		if len(out) > 0 {
			out = append(out, barSegment{Type: "separator"})
		}
		out = append(out, barSegment{Type: "text", Text: compactProviderName(prov.Name) + " ", Tone: "muted"})
		if opts.Compact {
			for i, l := range prov.Limits {
				figure := limitFigure(l)
				if i > 0 {
					figure = " " + figure
				}
				out = append(out, barSegment{Type: "text", Text: figure, Tone: usageTone(l)})
			}
			continue
		}
		w := prov.Limits[worstLimit(prov.Limits)]
		if opts.Native {
			value := min(max(w.Percent, 0), 100)
			out = append(out,
				barSegment{Type: "progress", Value: &value, Total: 100, Width: usageBarWidth, Tone: usageTone(w)},
				barSegment{Type: "text", Text: " " + limitFigure(w), Tone: usageTone(w)})
		} else {
			out = append(out, barSegment{Type: "text", Text: drawBar(w.Percent) + " " + limitFigure(w), Tone: usageTone(w)})
		}
	}
	if len(out) == 0 {
		return []barSegment{{Type: "text", Text: "usage: no providers", Tone: "muted"}}
	}
	return out
}

// worstLimit is the index of the limit nearest its cap.
func worstLimit(limits []usageLimit) int {
	worst := 0
	for i, l := range limits {
		if l.Percent > limits[worst].Percent {
			worst = i
		}
	}
	return worst
}

// limitFigure is a limit's percentage with its pace marker.
func limitFigure(l usageLimit) string {
	if aheadOfPace(l) {
		return fmt.Sprintf("%d%%▲", l.Percent)
	}
	return fmt.Sprintf("%d%%", l.Percent)
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

// cliUsageBar prints `{"content":[…],"compact_content":[…],"glyph_content":[…]}`
// for the usage-bar module: the full form with native `progress` bars, the form
// Luvus falls back to when the row is too narrow — without a compact form Luvus
// hides the whole widget behind "… +N" — and the full form with block-glyph
// bars, which the script pushes instead when the server refuses `progress`
// (Luvus 0.13.4 release does). It runs the same provider fetchers the footer
// did, sharing the on-disk last-good cache, so the widget works whether or not
// a lasso server is up.
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
	native := opts
	native.Native = true
	out, err := json.Marshal(struct {
		Content        []barSegment `json:"content"`
		CompactContent []barSegment `json:"compact_content"`
		GlyphContent   []barSegment `json:"glyph_content"`
	}{usageBarSegments(payload, native, now), usageBarSegments(payload, compact, now), usageBarSegments(payload, opts, now)})
	if err != nil {
		fatal("usage-bar: " + err.Error())
	}
	os.Stdout.Write(append(out, '\n'))
}
