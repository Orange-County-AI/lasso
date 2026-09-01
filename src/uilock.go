package main

import "time"

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
