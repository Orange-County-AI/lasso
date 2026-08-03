package main

import (
	"strings"
	"unicode/utf8"
)

// Recovering an agent herdr's own detection missed.
//
// herdr normally answers both "is there an agent in this pane" and "what is it
// doing" from its per-pane detector, which first identifies the agent *process*
// in the pane's foreground process group and only then reads the screen. That
// identification is where it can go blind:
//
//   - herdr walks the foreground process group on Linux through
//     /proc/<pid>/task/<tid>/children, a file that only exists when the kernel
//     was built with CONFIG_PROC_CHILDREN. On a kernel without it (several of
//     our Kubernetes workspace pods) the walk cannot see past the process-group
//     leader, so an agent started under a wrapper — `mise <task>`, npx, any
//     launcher script — is never identified. (0.8.0 adds an opt-in second mode,
//     HERDR_PROCESS_DETECTION=child-groups; "native" is still the default, so
//     the blind spot — and this recovery path — remain live.)
//   - the pane then has no detected agent at all: it is absent from agent.list,
//     pane.list reports no `agent` kind, and agent_status stays "unknown"
//     forever, because state detection is gated on knowing the agent first.
//
// herdr still knows one thing about such a pane: its agent_session, reported by
// the harness's own SessionStart hook rather than by process detection. That
// alone is not proof the agent is still *running* — herdr only clears a
// persisted session when it watches the agent process exit, which on these
// hosts it never does — so a session that outlived its process would otherwise
// leave a ghost in list_agents.
//
// The OSC terminal title settles it. Agent CLIs write their live state into it,
// and herdr's own detection manifests read exactly these patterns (the
// osc_title_working / osc_title_idle rules in its claude and codex manifests).
// A title still carrying that chrome means the CLI is live and rendering; once
// it exits the shell resets the title to its prompt ("dev@norm: ~"). So lasso
// treats a distinctive title as both the liveness proof for an unidentified
// agent_session AND the status for any agent pane herdr can only call
// "unknown".
//
// Deliberately narrow: only patterns unique to a harness's live UI count as
// proof. codex's plain-title idle rule is a status hint, not proof, because
// herdr only applies it once it already knows codex owns the pane. A harness
// with no title convention lasso knows simply keeps herdr's answer — better a
// missing agent than an invented one.

const (
	// claudeIdleGlyph is the "✳" claude prefixes its title with at rest.
	claudeIdleGlyph = '✳'
	// brailleFirst/brailleLast bound the Braille Patterns block, which claude
	// draws its working spinner from (one frame per animation step).
	brailleFirst = '⠀'
	brailleLast  = '⣿'
	// codexSpinnerGlyphs are the braille frames codex cycles through while
	// working; unlike claude's they can appear anywhere in the title.
	codexSpinnerGlyphs = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
)

// titleAgentStatus reads a pane's raw OSC title the way herdr's detection
// manifest for that agent would, returning the agent status it implies
// ("working"/"idle"/"blocked", or "" for a title that says nothing).
//
// distinctive reports whether the match came from chrome unique to that
// harness's live UI, and so proves an agent is running in the pane rather than
// merely being consistent with one.
func titleAgentStatus(agent, title string) (status string, distinctive bool) {
	switch agent {
	case "claude":
		// herdr's claude manifest: a braille spinner prefix means working, a
		// "✳ " prefix means idle. Both are followed by a space, which is what
		// keeps them from matching a title that merely happens to start with a
		// symbol.
		first, n := utf8.DecodeRuneInString(title)
		if !strings.HasPrefix(title[n:], " ") {
			return "", false
		}
		switch {
		case first >= brailleFirst && first <= brailleLast:
			return "working", true
		case first == claudeIdleGlyph:
			return "idle", true
		}
		return "", false
	case "codex":
		// herdr's codex manifest: "Action Required" means blocked, a spinner
		// frame anywhere means working, and any non-blank title otherwise reads
		// as idle — that last one only because herdr has already identified
		// codex, so it is a hint here, never proof.
		if strings.Contains(title, "Action Required") {
			return "blocked", true
		}
		if strings.ContainsAny(title, codexSpinnerGlyphs) {
			return "working", true
		}
		if strings.TrimSpace(title) != "" {
			return "idle", false
		}
	}
	return "", false
}

// paneAgentPresence answers "is an agent running in this pane, and what is it
// doing" from a single pane.list entry, filling in for herdr wherever its own
// detection came up empty.
//
// kind is the agent's harness ("claude"/"codex"/…) and is empty when the pane
// is a bare shell. status is herdr's agent_status, or the title-derived status
// when herdr's is missing or "unknown" — herdr's own answer always wins when it
// has one.
func paneAgentPresence(p pane) (kind, status string) {
	kind, status = p.Agent, p.AgentStatus
	if kind == "" && p.AgentSession != nil {
		// herdr did not identify an agent, but the harness reported a session.
		// Trust it only if the title still shows that harness's live chrome.
		if s, distinctive := titleAgentStatus(p.AgentSession.Agent, p.TerminalTitle); distinctive {
			return p.AgentSession.Agent, s
		}
		return "", status
	}
	if kind != "" && (status == "" || status == "unknown") {
		if s, _ := titleAgentStatus(kind, p.TerminalTitle); s != "" {
			status = s
		}
	}
	return kind, status
}

// paneHasLiveAgent reports whether an agent is running in the pane — herdr's own
// `agent` field, plus the sessions it never identified that paneAgentPresence
// recovers. The predicate every "is this an agent pane or a bare shell" decision
// should go through, so one host's blind detection can't make an agent look like
// a shell in some code paths and not others.
func paneHasLiveAgent(p pane) bool {
	kind, _ := paneAgentPresence(p)
	return kind != ""
}
