import { api } from "@/lib/api"
import { clientID } from "@/lib/client-id"

// ---------------------------------------------------------------------------
// Who may resize the shared terminal
// ---------------------------------------------------------------------------
//
// Every tab on a host proxies to the same ttyd, so they all drive one pty, and
// herdr reflows every pane in the session whenever it changes size. A tab that
// refits while nobody is looking at it therefore clamps the width and rewraps
// the scrollback of the human actually working — the "terminal shrinks as though
// the sidebar opened" jank.
//
// lib/terminal.ts used to gate this on the tab's own focus, which is as far as a
// browser can see. It is not far enough: two windows on two machines can both be
// visible and focused at once, and a tab reviving after a reload passed its
// first resize unconditionally. So the arbiter is the server (uilock.go's
// claimTerm) and this module is the browser's half of it —
//
//   - CLAIM when a human acts here: a key or a pointer in this tab, or the
//     window taking focus. Never on a reflow, which is the thing being gated.
//   - OBEY the owner the server reports, which arrives on every SSE frame
//     (`term_owner`) and, sooner, in the claim's own response.
//
// A free claim ("") means "I may take it", not "nobody may resize": a lone tab
// on a quiet host must still be able to size its terminal, or herdr sits at the
// pty default forever. That is the deliberate difference from the sidebar claim,
// which refuses an unheld lock.

let owner = ""
let claiming = false
let lastClaimAt = 0

// How long a held claim is left alone before this tab re-asserts it. The server
// lease is 45s; renewing at a third of that keeps an actively-used tab from ever
// timing out mid-session while costing one small POST per interaction burst
// rather than one per keystroke.
const RENEW_MS = 15_000

type Listener = () => void
const listeners = new Set<Listener>()

function emit() {
  for (const fn of listeners) fn()
}

// onTermOwnerChange subscribes to the owner moving. The resize gate uses it to
// flush a resize it had to drop, the moment this tab is allowed to send one.
export function onTermOwnerChange(fn: Listener): () => void {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

// setTermOwner records the owner the server reported. Called from the SSE apply
// (lib/app-store.tsx) and from a claim's own response.
export function setTermOwner(next: string | undefined) {
  const v = next ?? ""
  if (v === owner) return
  owner = v
  emit()
}

// mayResizeTerminal is the gate's question: this tab owns the pty, or nobody
// does and it is free to take.
export function mayResizeTerminal(): boolean {
  return owner === "" || owner === clientID()
}

export function termOwner(): string {
  return owner
}

// claimTerminal reports that a human just acted in this tab. Throttled while
// this tab already owns the claim (a renewal is not urgent, and a keystroke must
// not cost a round trip), immediate when it does not — that is the case the
// human is waiting on.
export function claimTerminal() {
  const mine = owner === clientID()
  if (mine && Date.now() - lastClaimAt < RENEW_MS) return
  if (claiming) return
  claiming = true
  lastClaimAt = Date.now()
  api
    .claimTerminal(clientID())
    .then((r) => setTermOwner(r.term_owner))
    .catch(() => {
      // An older server has no /api/term-claim. Leave the owner empty, which
      // reads as "free" — i.e. degrade to the pre-claim behavior rather than to
      // a terminal this tab may never resize.
      setTermOwner("")
    })
    .finally(() => {
      claiming = false
    })
}

// watchTermIntent wires the events that count as "a human acted here". Mounted
// once by AppProvider; returns its own teardown.
//
// Focus counts, and deliberately so: alt-tabbing to a window is the human saying
// which screen they are working on, and it is the moment the size should follow
// them. A mere visibility change does not, on its own — a tab can become visible
// because the OS restored a window nobody asked for — so it is gated on the
// document actually having focus too.
export function watchTermIntent(): () => void {
  const act = () => {
    if (document.visibilityState === "visible" && document.hasFocus()) {
      claimTerminal()
    }
  }
  const opts = { capture: true, passive: true } as const
  window.addEventListener("focus", act)
  document.addEventListener("visibilitychange", act)
  document.addEventListener("keydown", act, opts)
  document.addEventListener("pointerdown", act, opts)
  document.addEventListener("touchstart", act, opts)
  return () => {
    window.removeEventListener("focus", act)
    document.removeEventListener("visibilitychange", act)
    document.removeEventListener("keydown", act, opts)
    document.removeEventListener("pointerdown", act, opts)
    document.removeEventListener("touchstart", act, opts)
  }
}

// watchTermIntentIn wires the same events inside a terminal iframe. Typing into
// a terminal never reaches the parent document — the events fire in the iframe's
// own realm — and typing into a terminal is the single clearest statement that
// this is the tab being used, so without this the most important signal is the
// one that is missed.
export function watchTermIntentIn(w: Window): () => void {
  const act = () => {
    if (document.visibilityState === "visible") claimTerminal()
  }
  const opts = { capture: true, passive: true } as const
  try {
    w.addEventListener("keydown", act, opts)
    w.addEventListener("pointerdown", act, opts)
    w.addEventListener("touchstart", act, opts)
    w.addEventListener("focus", act)
  } catch {
    return () => {}
  }
  return () => {
    try {
      w.removeEventListener("keydown", act, opts)
      w.removeEventListener("pointerdown", act, opts)
      w.removeEventListener("touchstart", act, opts)
      w.removeEventListener("focus", act)
    } catch {
      /* the iframe's realm is gone */
    }
  }
}
