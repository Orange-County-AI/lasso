package main

// The blocked-agent watcher — the one producer of notifications today.
//
// It answers a question nothing else in lasso does: an agent that stops to ask
// for approval is invisible until someone looks at a screen. The pane feeds
// (hostfeed.go) only run for hosts a tab is watching, and the ⌘K aggregation
// only runs when a browser polls it — both are properties of somebody already
// looking. So this is a poll of its own, deliberately cheap and deliberately
// off by default.
//
// It reuses panesSnapshot (hostpanes.go) rather than enumerating hosts itself,
// for three reasons: that aggregation already fans out with per-host deadlines
// and degrades to last-good panes; it already resolves the agent status lasso
// considers authoritative (herdr's, corrected by title reads and omp's plan
// gate — see enumerateHostPanes); and its 1.5s cache means a poll that lands
// near a browser's ⌘K refresh costs nothing at all.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// notifyPollEvery is how often the fleet is checked for newly blocked agents.
// It gates a pane.list per host (herdr's most expensive method), which is why
// the loop first asks notifyEnabled(): with no device subscribed this costs one
// COUNT(*) against a local sqlite file every ten seconds and nothing else.
//
// A var, not a const, so a test can shrink the wait (same reason as the
// bulk-close pacing knobs in main.go).
var notifyPollEvery = 10 * time.Second

// notifyRenotify is the minimum gap between two notifications about the same
// pane. Detection is a transition (working → blocked), so a steadily blocked
// agent is announced once regardless; this bounds an agent that flaps —
// answering one approval only to raise the next — to one buzz per window.
const notifyRenotify = 5 * time.Minute

// notifyForget is how long a pane's remembered status outlives the pane
// vanishing from the listing. Long enough that an unreachable host coming back
// is not read as every one of its agents having just blocked; short enough that
// the map tracks the fleet rather than growing with every pane ever seen.
const notifyForget = 10 * time.Minute

// paneNotifyState is what the watcher remembers per pane.
type paneNotifyState struct {
	status     string
	notifiedAt time.Time
	seenAt     time.Time
}

// blockedWatcher holds the previous observation. Its whole job is telling a
// transition from a steady state, so it is the only thing here with memory.
type blockedWatcher struct {
	mu   sync.Mutex
	seen map[string]paneNotifyState
}

// startBlockedWatcher runs the watcher for the life of the server.
func startBlockedWatcher(ctx context.Context) {
	w := &blockedWatcher{}
	t := time.NewTicker(notifyPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !notifyEnabled() {
			continue
		}
		for _, n := range w.observe(panesSnapshot(ctx).Panes, time.Now()) {
			publishNotification(n)
		}
	}
}

// observe records this poll's pane statuses and returns a notification for each
// agent that has just become blocked.
//
// Deliberately NOT suppressed on the first poll. A silent baseline would mean an
// agent that blocked while lasso was restarting — or while the user was
// switching notifications on — is never announced at all, which is the exact
// hole this feature exists to close. The cost is that a restart with several
// agents already parked on approvals sends several notifications; they collapse
// per pane on the device, and notifyRenotify bounds anything worse.
func (w *blockedWatcher) observe(panes []hostPane, now time.Time) []notification {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen == nil {
		w.seen = map[string]paneNotifyState{}
	}
	ids := notifyPaneIdentities(panes)
	// Which names are shared. Two agents in one workspace inherit its label and
	// would otherwise produce notifications identical down to the body — measured
	// in the field: a workspace called "cheese" with two parked claudes sent two
	// indistinguishable buzzes. Counted across every agent pane in the snapshot,
	// not just the ones notifying now, so the sibling that blocks five minutes
	// later is disambiguated the same way.
	shared := map[string]int{}
	for _, p := range ids {
		if p.HasAgent {
			shared[blockedTitle(p)]++
		}
	}
	var out []notification
	for key, p := range ids {
		if !p.HasAgent {
			// A pane that stopped being an agent (the CLI exited, leaving a bare
			// shell) keeps its slot so the status it had is not read as a
			// transition when an agent lands there again, but says nothing now.
			prev := w.seen[key]
			prev.status, prev.seenAt = "", now
			w.seen[key] = prev
			continue
		}
		prev := w.seen[key]
		blocked := p.AgentStatus == "blocked"
		fresh := blocked && prev.status != "blocked" &&
			(prev.notifiedAt.IsZero() || now.Sub(prev.notifiedAt) >= notifyRenotify)
		next := paneNotifyState{status: p.AgentStatus, notifiedAt: prev.notifiedAt, seenAt: now}
		if fresh {
			next.notifiedAt = now
			out = append(out, blockedNotification(p, key, shared[blockedTitle(p)] > 1))
		}
		w.seen[key] = next
	}
	for key, st := range w.seen {
		if now.Sub(st.seenAt) > notifyForget {
			delete(w.seen, key)
		}
	}
	return out
}

// notifyPaneIdentities keys this poll's panes by the pane they actually ARE.
//
// The dedupe matters because of herdr-mirror (mirror.go): a mirrored pane is a
// real local pane streaming another machine's, so a blocked agent on norm can
// appear twice in one snapshot — once as norm's own pane, once as titan's
// mirror of it. Keying a mirror row by the REMOTE host and pane collapses the
// pair, and the direct row wins when both are present (it is the machine the
// work is on, and its status came from that machine's own herdr).
//
// It also protects the state map from the aggregation's last-good fallback,
// where a host that failed this poll contributes its previous panes: those keys
// are the same keys, so a stale listing re-states what the watcher already
// recorded instead of reading as new.
func notifyPaneIdentities(panes []hostPane) map[string]hostPane {
	out := make(map[string]hostPane, len(panes))
	for _, p := range panes {
		key := p.Host + "\x00" + p.PaneID
		if p.MirrorHost != "" {
			key = p.MirrorHost + "\x00" + p.MirrorPane
			if _, ok := out[key]; ok {
				continue // the direct row (or an earlier mirror of it) is already in
			}
			out[key] = p
			continue
		}
		out[key] = p // a direct row always outranks a mirror of the same pane
	}
	return out
}

// blockedTitle is the agent's NAME: the workspace label, which auto-titling
// (autotitle.go) turns into a real description of the task. That is what a phone
// shows on the lock screen and the only line guaranteed to be read, so it comes
// before anything lasso could say itself.
//
// Its own function because the watcher needs the answer twice: once to render,
// once to find out whether two agents would render the same.
func blockedTitle(p hostPane) string {
	if t := firstNonEmpty(p.WorkspaceLabel, p.MirrorLabel, p.PaneLabel, p.TerminalTitle); t != "" {
		return t
	}
	return "Agent"
}

// paneDiscriminator tells sibling agents apart when their shared name cannot.
// herdr's tab label is what the human sees beside them in the sidebar; the pane
// id is the fallback for a tab carrying no label at all.
func paneDiscriminator(p hostPane) string {
	if tab := strings.TrimSpace(p.TabLabel); tab != "" {
		return "tab " + tab
	}
	if p.MirrorPane != "" {
		return p.MirrorPane
	}
	return p.PaneID
}

// blockedNotification renders one blocked agent. ambiguous means another agent
// in the same snapshot carries the same name, so the title must say which pane
// this is — two agents in one workspace inherit its label, and without this the
// pair is identical down to the body.
//
// The body says which harness, on which machine, and what it was last doing.
func blockedNotification(p hostPane, key string, ambiguous bool) notification {
	host := p.Host
	hostLabel := p.HostLabel
	if p.MirrorHost != "" {
		host, hostLabel = p.MirrorHost, p.MirrorHost
	}
	title := blockedTitle(p)
	if d := paneDiscriminator(p); ambiguous && d != "" {
		// Clip the shared part, never the discriminator: it is the only thing
		// telling this notification from its sibling, so a long workspace label
		// must not push it off the end.
		title = clipNotifyText(title, max(20, 70-len([]rune(d))-3)) + " · " + d
	}
	agent := p.Agent
	if agent == "" {
		agent = "An agent"
	}
	where := firstNonEmpty(hostLabel, host)
	// The terminal title is what the agent was working on when it stopped — the
	// most useful thing to add, unless it is already the headline, in which case
	// saying what lasso wants from the reader is worth more.
	tail := "needs your input"
	if task := strings.TrimSpace(p.TerminalTitle); task != "" && task != blockedTitle(p) {
		tail = clipNotifyText(task, 90)
	}
	body := fmt.Sprintf("%s is blocked on %s — %s", agent, where, tail)
	return notification{
		Kind:  notifAgentBlocked,
		Title: clipNotifyText(title, 70),
		Body:  body,
		Tag:   "blocked:" + key,
		Host:  host,
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// clipNotifyText keeps a notification line to a length a phone will actually
// render, cutting on a rune boundary so a clipped multi-byte character can't
// produce mojibake on the lock screen.
func clipNotifyText(s string, max int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= max {
		return s
	}
	return strings.TrimRight(string([]rune(s)[:max-1]), " ") + "…"
}
