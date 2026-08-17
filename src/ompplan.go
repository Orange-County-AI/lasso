package main

import (
	_ "embed"
	"path/filepath"
	"strings"
)

// omp's config overlay: plan mode and the theme pin.
//
// Two settings lasso must reach on omp are settings, not flags, so both ride in
// on `omp --config <path>` — an extra config.yml-style overlay merged on top of
// the user's ~/.omp/agent/config.yml for that run alone. The user's config is
// never written to by this path, and every setting lasso doesn't name still
// flows through from it.
//
//  1. Plan mode. omp HAS one, but the launch line reaches it only through
//     plan.defaultOnStartup, gated by plan.enabled — and the two flags that look
//     like they'd do it don't: `--plan` names the model the "plan" ROLE resolves
//     to (a sibling of --smol/--slow), and `--plan-yolo` enters plan mode and
//     then auto-approves the plan on the model's first resolve call, which is the
//     opposite of the blocking gate lasso's plan_mode promises.
//
//  2. The theme pin. theme.dark/theme.light name the themes omp picks between
//     from the terminal's background; lasso points both at its generated
//     themes/herdr.json so the pick can't matter and the file omp watches is the
//     one lasso repaints (see agentsync.go). agentsync also pins them in
//     config.yml, for omp runs lasso didn't launch — but that pin is best-effort
//     by nature: omp rewrites config.yml wholesale from its in-memory settings
//     whenever one changes, and a single running instance doing so drops every
//     later launch back onto a built-in theme, which is watched by nothing.
//     Observed on this box: the slots were clobbered within ~16 minutes of being
//     written. In the overlay they cannot be — it is a file only lasso writes.
//
// Two constraints from omp itself:
//
//   - Interactive only. Under -p/print mode omp deliberately IGNORES
//     plan.defaultOnStartup ("no interactive surface to review the plan") and
//     points you at --plan-yolo instead. lasso always launches omp interactively
//     in a herdr pane, so this is fine — but don't reuse the plan keys on any
//     headless omp path.
//   - `--config` is accepted on the main `omp` invocation only, never on a
//     subcommand (`omp config --config …` is an unknown-option error).

// ompPlanOverlay is the plan half of the overlay, compiled into the binary so a
// plan-mode omp agent depends on no file in anyone's home — not the user's omp
// config, and not a lasso checkout.
//
//go:embed assets/omp-plan.yml
var ompPlanOverlay []byte

// ompThemeOverlay is the theme half. Built from ompThemeName rather than shipped
// as an asset so the pin and the generated theme file cannot drift apart.
var ompThemeOverlay = []byte("# lasso's theme pin: both of omp's mode-selected slots name the theme\n" +
	"# lasso generates from herdr's palette (~/.omp/agent/themes/" + ompThemeName + ".json),\n" +
	"# so the light/dark pick can't matter and omp watches the file lasso rewrites.\n" +
	"theme:\n" +
	"  dark: " + ompThemeName + "\n" +
	"  light: " + ompThemeName + "\n")

// ompOverlayBody is the overlay one omp agent launches with: the theme pin
// always, plus the plan keys when it is launching in plan mode. Both halves are
// top-level YAML maps with disjoint keys, so concatenation IS the merge.
func ompOverlayBody(planMode bool) []byte {
	if !planMode {
		return ompThemeOverlay
	}
	body := make([]byte, 0, len(ompThemeOverlay)+len(ompPlanOverlay)+1)
	body = append(body, ompThemeOverlay...)
	if n := len(body); n > 0 && body[n-1] != '\n' {
		body = append(body, '\n')
	}
	return append(body, ompPlanOverlay...)
}

// ompConfigPath is the staged overlay for one agent on backend b. Under
// <lasso dir>/omp, NOT the work dir, so it never dirties the agent's worktree —
// same placement rule as the staged prompt. Deterministic (lasso dir + agent id)
// so closeAgentRecord can remove it without the path being persisted anywhere.
//
// Per agent rather than one shared file: the content is a constant, but a shared
// path would be truncated-and-rewritten under a second agent booting at the same
// instant, and the atomic write that fixes that (write-temp-then-rename) isn't
// available on every backend — SFTP's rename refuses an existing destination.
func ompConfigPath(b Backend, agentID string) string {
	return filepath.Join(lassoDirFor(b), "omp", agentID+".yml")
}

// stageOmpConfig materializes the overlay on backend b and returns its path for
// the launch line's --config. Rewritten on every boot rather than written once,
// so an overlay that changes across a lasso upgrade (or a theme flip between two
// boots) takes effect without anyone having to clear the old one.
func stageOmpConfig(b Backend, agentID string, planMode bool) (string, error) {
	path := ompConfigPath(b, agentID)
	if err := b.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := b.WriteFile(path, ompOverlayBody(planMode), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Reporting the plan gate as "blocked".
//
// lasso's plan_mode contract is that the agent's status goes "blocked" when it
// wants to execute. claude and opencode satisfy it for free: their gate is a
// prompt herdr's own detection recognizes. omp's does not.
//
// herdr learns an omp pane's state from an event-driven integration extension
// (herdr-omp-agent-state.ts) that emits "blocked" for exactly two things — a
// tool_approval_requested event and the `ask` tool. omp's plan gate is neither:
// it is a TUI overlay (showPlanReview) raised after the turn ends, so the
// extension has already published idle. Measured on omp 17.3.4: an omp agent
// parked on the Plan Review overlay reports agent_status "done".
//
// So lasso reads the screen for it. ompGateStatus asks paneShowsOmpPlanReview
// when herdr says an omp pane is at rest, and reports "blocked" when the overlay
// is up. It is applied on the three MCP surfaces the plan_mode contract names —
// wait_agent (via paneAgentStatus), get_agent and list_agents — and on the
// cross-host pane enumeration (enumerateHostPanes) behind /api/all-panes, which
// feeds lasso's ⌘K pane switcher. That enumeration runs on a poll, so there it
// is narrowed to the panes whose RECORD says lasso launched them in plan mode:
// that is the only population that can be at the gate lasso promised, and it is
// usually empty.
//
// What this cannot reach is HERDR's own sidebar, which renders herdr's answer
// and not lasso's. herdr accepts a state report only from the source that
// already owns the pane's agent lifecycle: a report from a "lasso" source is
// dropped, and reporting as "herdr:omp" instead outranks the real integration's
// seq counter — that counter is seeded at the omp process's START time, so a
// report stamped with the current time wins forever and the pane's status
// freezes at whatever lasso last said. Both were measured, the second by
// wedging a probe pane at "blocked" while omp was demonstrably working. The
// integration does expose a `herdr:blocked` event another omp extension could
// emit, and omp takes a per-run `-e <file>` so lasso could stage one — but omp
// fires no extension event when the plan review opens (a probe extension
// subscribed to every documented event sees `agent_end` and then nothing), so
// there is nothing for such an extension to hang off. Making herdr's sidebar
// agree needs a change in omp or in herdr's omp integration, not here.

// ompGateStatus is that upgrade: given the agent kind and status herdr reported
// for a pane, it returns "blocked" when the pane is an omp parked on its plan
// gate, and the status unchanged otherwise. Both guards come first so the screen
// read never happens for another harness, for a working agent, or (b nil) for a
// host whose enumeration failed — the status is then whatever herdr managed to
// say, not a gate we could not check for.
func ompGateStatus(b Backend, paneID, agent, status string) string {
	if b == nil || agent != "omp" || paneID == "" || !agentAtRest(status) {
		return status
	}
	if paneShowsOmpPlanReview(b, paneID) {
		return "blocked"
	}
	return status
}

// agentAtRest reports whether a herdr agent status means "not currently doing
// anything" — the states omp's plan gate can be hiding behind. herdr publishes
// "done" for a pane whose agent finished a turn it hasn't been looked at since,
// which is idle wearing a badge.
func agentAtRest(status string) bool {
	return status == "idle" || status == "done"
}

// ompPlanReviewMarkers are the strings omp's plan-approval overlay draws: the
// review box's title and the title of the picker beneath it. Both are required,
// because either alone is a phrase an agent could plausibly print into its own
// output — and a false "blocked" is worse than a missed one, since it makes a
// wait for idle never match. There is exactly one showPlanReview call site in
// the binary, and this is its chrome.
var ompPlanReviewMarkers = []string{"Plan Review", "Plan mode - next step"}

// paneShowsOmpPlanReview reports whether the pane's visible screen is omp's plan
// approval overlay. Deliberately reads "visible" and not scrollback: the gate is
// a thing the pane is showing NOW, and a finished plan review scrolled up in the
// history would otherwise pin the agent at "blocked" forever.
//
// A read failure answers false — herdr's own status stands rather than an
// invented gate.
func paneShowsOmpPlanReview(b Backend, paneID string) bool {
	text, ok := paneVisibleText(b, paneID)
	if !ok {
		return false
	}
	for _, m := range ompPlanReviewMarkers {
		if !strings.Contains(text, m) {
			return false
		}
	}
	return true
}
