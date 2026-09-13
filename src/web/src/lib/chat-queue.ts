import type { ChatItem } from "@/lib/api"

// Echoes of messages the pane has ACCEPTED but the transcript has not written
// yet.
//
// The composer clears its draft the moment a send is confirmed, and the row
// exists only once the harness records it — a completed message, not a stream
// (see src/chatview.go). So without an echo the message vanishes for the
// seconds in between, which reads as "my prompt didn't send".
//
// Reconciliation is on the transcript's newest USER TURN, not on the text: a
// harness records its own rendering of what it received, and an echo that
// outlives its row is one message shown twice.
export type QueuedEcho = {
  // Assigned by the caller, and the only reason it exists is that a message has
  // no identity here yet — it is not in the transcript — so two sends of the
  // same text would otherwise be the same row to React.
  id: number
  // host\0pane the message was sent to. A pane change must not carry one
  // conversation's echo into another's.
  target: string
  text: string
  // The transcript's newest user turn when the send STARTED.
  after: string
}

// newestUserID is the id of the transcript's newest user turn, or "" when it
// has none.
export function newestUserID(items: ChatItem[]): string {
  for (let i = items.length - 1; i >= 0; i--) {
    if (items[i].kind === "user") return items[i].id
  }
  return ""
}

// reconcileQueued retires one echo for each message the transcript has recorded
// since. The transcript is append-only, so once its newest user turn is no
// longer the one an echo was sent against, a message has landed.
//
// Echoes still waiting are re-pointed at the turn that landed, so two sends
// before either lands retire ONE APART instead of both at once — the case that
// would otherwise hide the second message until the transcript caught up with
// it, which is the bug this exists to fix.
export function reconcileQueued(
  prev: QueuedEcho[],
  target: string,
  newestUser: string
): QueuedEcho[] {
  const mine = prev.filter((q) => q.target === target)
  if (mine.length === 0 || newestUser === mine[0].after) return prev
  const rest = mine.slice(1).map((q) => ({ ...q, after: newestUser }))
  return [...prev.filter((q) => q.target !== target), ...rest]
}
