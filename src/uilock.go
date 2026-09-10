package main

import (
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Sidebar-layout ownership — a mutex over the one piece of UI state that
// several clients write UNPROMPTED.
// ---------------------------------------------------------------------------
//
// The rest of ui_state (usage footer, Files click behavior) only ever changes
// because a human clicked something, so patch-merge is enough. The sidebar
// layout is different: every tab re-persists it from its own panel group, which
// fires on mount, on a window resize, and on the programmatic apply of a change
// that arrived over SSE. That makes the layout a shared variable that any
// number of clients write on their own initiative, and a single stale client is
// enough to reopen a sidebar the human just collapsed, forever:
//
//	tab A collapses -> ui_state_rev -> tab B applies -> B's panel bounces back
//	open (its own remembered layout, its own viewport) -> B persists "open" ->
//	ui_state_rev -> A reopens.
//
// So writes to sidebar_collapsed/sidebar_pct take a lock. A client that says a
// HUMAN just acted on the sidebar takes the lock outright — "the most recent
// active client owns it" — and everyone else's spontaneous echo is refused for
// the duration of the lease.
//
// The lock is deliberately in memory, not in the db: it is a property of the
// clients currently connected, and a lasso restart should leave nobody holding
// it rather than let a client that has since gone away keep writing through a
// resurrected claim.

// layoutLease is how long a claim stands without being renewed. Long enough to
// cover a drag, an SSE round trip and the loser's snap-back; short enough that
// a human moving from their desktop to their phone doesn't wait on a tab that
// is no longer in front of anyone. Renewal is free (any accepted write from the
// owner renews), so an actively-used tab never loses the lock mid-session.
const layoutLease = 45 * time.Second

// layoutClaim is who holds the sidebar-layout lock and when they last wrote
// through it. Guarded by uiStateMu, which the POST handler already holds across
// its whole read-modify-write.
type layoutClaim struct {
	client string
	at     time.Time
}

var layoutOwner layoutClaim

// claimLayout decides whether client may CHANGE the sidebar-layout fields, and
// records the claim when it may. userIntent means the client is reporting a
// change a HUMAN just made in it (a drag of the handle, ⌘\, the collapse
// chevron, the mobile dial) rather than its panel group reporting a size it
// arrived at on its own.
//
// Only two writers may move the sidebar:
//
//   - a client whose human just acted. It takes the lock from whoever held it —
//     this is the whole feature: the sidebar answers to whoever is actually
//     using it, and the most recent one wins.
//   - the current owner, within its lease, settling the change it just made
//     (the debounced write that follows a drag).
//
// Everything else is refused, and note what is deliberately NOT in that list: a
// free lock. An expired lease does not promote an unattended echo into an
// authority, because "nobody has touched a tab for a while" is exactly the
// state the rogue client writes in. A lock nobody holds simply means the next
// human to act gets it uncontested.
//
// A client that sends no id is a tab built before this handshake existed. It
// can never move the sidebar for anyone else, which is the correct reading: its
// writes are indistinguishable from the unattended ones this exists to stop.
// Its own view still works; it just stops syncing until it is reloaded.
func claimLayout(client string, userIntent bool, now time.Time) bool {
	if client == "" {
		return false
	}
	ownerFresh := layoutOwner.client == client && now.Sub(layoutOwner.at) <= layoutLease
	if !userIntent && !ownerFresh {
		return false
	}
	layoutOwner = layoutClaim{client: client, at: now}
	return true
}

// releaseLayoutOwner drops the lock. Only tests need it — in a running server
// the lease is what ends a claim.
func releaseLayoutOwner() { layoutOwner = layoutClaim{} }

// ---------------------------------------------------------------------------
// Terminal-size ownership — the same idea, over the shared pty
// ---------------------------------------------------------------------------
//
// Every tab on a host proxies to the SAME ttyd, and ttyd resizes the one pty to
// whichever client last asked. herdr then reflows every pane in the session, so
// a background tab that refits — an OS window resize, a phone rotating, a tab
// reviving after a reload — clamps the width AND rewraps the scrollback of the
// human actually working in it. That is the "terminal shrinks as though the
// sidebar opened" jank, and the scroll jump that rides along with it.
//
// lib/terminal.ts already gates this on the tab's OWN focus, which is as far as
// a browser can see: two windows on two machines can both be visible and
// focused, and neither can observe the other. The claim is the missing half —
// the one question only the server can answer.
//
// It differs from the sidebar claim in one deliberate way: an UNHELD terminal
// claim is grantable. The sidebar refuses a free lock because a rogue client's
// echo would otherwise be promoted into authority once everyone went idle, and
// the cost of refusing is only that a layout stays put. Here the cost is a
// terminal nobody may size: a lone tab opening on a host with no other client
// would render its herdr at the pty default (80x24) forever. So "nobody holds
// it" means the next asker gets it, and only a LIVE owner's lease can refuse.

// termLease is how long a terminal claim stands without renewal. Matches the
// sidebar's, and for the same reason: long enough to span a window resize
// settling, short enough that walking from a desk to a phone doesn't wait on a
// tab nobody is looking at. It is a backstop rather than the usual path — an
// owner whose tab closes frees the claim at once (clientConnected).
const termLease = 45 * time.Second

// termOwners is the terminal-size claim, per host: ttyd, and so the pty, is per
// host, so two tabs on two machines are not in contention at all.
//
// Its own mutex, deliberately NOT uiStateMu: that one is held across a db
// read-modify-write, and this is taken on interaction — a keystroke must not
// queue behind someone else's settings save.
var (
	termMu     sync.Mutex
	termOwners = map[string]layoutClaim{}
)

// termHeld reports whether host's claim currently stands, i.e. is held by a
// client that is both within its lease and still connected. Callers hold termMu.
func termHeld(host string, now time.Time) bool {
	c, ok := termOwners[host]
	if !ok || c.client == "" {
		return false
	}
	if now.Sub(c.at) > termLease {
		return false
	}
	return clientConnected(c.client)
}

// claimTerm gives client the right to resize host's terminal, and reports who
// holds it afterwards.
//
// Every call is a report that a human just acted in that tab — the frontend asks
// only on real interaction (a keystroke or a pointer in the terminal, the window
// taking focus), never on a reflow — so this is the "most recently active client
// wins" rule with no intent flag to carry: asking IS the intent. A renewal by
// the current owner and a steal by a new one are the same operation.
//
// A client that sends no id gets no claim (and, being a build that predates the
// gate, does not consult one either — it resizes as it always did).
func claimTerm(host, client string, now time.Time) string {
	termMu.Lock()
	defer termMu.Unlock()
	if client == "" {
		if termHeld(host, now) {
			return termOwners[host].client
		}
		return ""
	}
	termOwners[host] = layoutClaim{client: client, at: now}
	return client
}

// termOwner is who may resize host's terminal right now, or "" when the claim is
// free — which the browser reads as "I may take it", not "nobody may resize".
func termOwner(host string, now time.Time) string {
	termMu.Lock()
	defer termMu.Unlock()
	if !termHeld(host, now) {
		return ""
	}
	return termOwners[host].client
}

// releaseTermOwners drops every terminal claim. Only tests need it.
func releaseTermOwners() {
	termMu.Lock()
	defer termMu.Unlock()
	termOwners = map[string]layoutClaim{}
}
