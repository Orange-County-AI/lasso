package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Luvus Bar limits: 16 segments and 256 display columns per widget; a bottom
// widget realistically gets far less. Both layouts fit one provider per pair of
// segments and rely on Luvus's own compaction for narrow clients.
const usageBarMaxSegments = 16

type barSegment struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Tone string `json:"tone,omitempty"`
}

// usageBarSegments renders the aggregated usage payload as Luvus Bar segments.
// Full: "Claude 42% · 5h 18m"; compact: "Cl 42%". The worst limit per provider
// is what is shown: the quota you hit first is the one worth reading.
func usageBarSegments(p usagePayload, compact bool, now time.Time) []barSegment {
	var out []barSegment
	for _, prov := range p.Providers {
		limit, ok := worstLimit(prov)
		if !ok {
			continue
		}
		if len(out) > 0 {
			out = append(out, barSegment{Type: "separator"})
		}
		name := prov.Name
		if compact {
			name = compactProviderName(name)
		}
		out = append(out, barSegment{Type: "text", Text: name + " ", Tone: "muted"})
		value := fmt.Sprintf("%d%%", limit.Percent)
		if !compact {
			value += " " + shortLimitLabel(limit.Label)
			if reset := resetHint(limit, now); reset != "" {
				value += " " + reset
			}
		}
		out = append(out, barSegment{Type: "text", Text: value, Tone: usageTone(limit)})
		if len(out) >= usageBarMaxSegments-2 {
			break
		}
	}
	if len(out) == 0 {
		return []barSegment{{Type: "text", Text: "usage: no providers", Tone: "muted"}}
	}
	return out
}

func worstLimit(p usageProvider) (usageLimit, bool) {
	var best usageLimit
	found := false
	for _, l := range p.Limits {
		if !found || l.Percent > best.Percent {
			best, found = l, true
		}
	}
	return best, found
}

// usageTone compares usage against the elapsed share of the window: ahead of
// the clock is a warning, exhausted is an error.
func usageTone(l usageLimit) string {
	switch {
	case l.Percent >= 100:
		return "error"
	case l.Percent >= 90 || (l.ElapsedPct >= 0 && l.Percent > l.ElapsedPct+15):
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

// shortLimitLabel keeps provider labels to the window ("5h", "week") since the
// provider name already occupies its own segment.
func shortLimitLabel(label string) string {
	label = strings.ToLower(strings.TrimSpace(label))
	switch {
	case strings.Contains(label, "week"), strings.Contains(label, "7-day"):
		return "wk"
	case strings.Contains(label, "month"):
		return "mo"
	case strings.Contains(label, "hour"):
		return "5h"
	case strings.Contains(label, "day"):
		return "day"
	}
	if len(label) > 6 {
		return label[:6]
	}
	return label
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

// cliUsageBar prints Luvus Bar content JSON for the usage-bar module. It runs
// the same provider fetchers as the server did, sharing the on-disk last-good
// cache, so the module works whether or not a lasso server is up.
func cliUsageBar(args []string) {
	compact := false
	for _, a := range args {
		switch a {
		case "-compact", "--compact":
			compact = true
		default:
			fatal("usage-bar: unknown argument " + a)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	payload := collectUsage(ctx)
	out, err := json.Marshal(usageBarSegments(payload, compact, time.Now()))
	if err != nil {
		fatal("usage-bar: " + err.Error())
	}
	os.Stdout.Write(append(out, '\n'))
}
