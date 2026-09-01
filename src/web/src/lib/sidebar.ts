import { lsGet, lsSet } from "@/lib/app-store"
import { qk, queryClient } from "@/lib/query"
import { releaseLayoutBackoff } from "@/lib/ui-state"

// The sidebar's last-open width (% of the panel group), persisted to
// localStorage and cached in React Query so it survives a page reload / lasso
// restart. react-resizable-panels' own expand() only remembers a size within
// the current session, so without this a sidebar reopened after a reload snaps
// back to the default width instead of where the user left it. The React Query
// cache is the in-memory source of truth (mirrors ui-state.ts); localStorage is
// the durable backing store.

const KEY = "lasso-sidebar-pct"
const DEFAULT_PCT = 40
const MIN_PCT = 15 // matches the right panel's minSize

// readSidebarPct parses the persisted width, clamping out garbage / sub-minSize
// values so a corrupt entry can't wedge the sidebar open thin.
function readSidebarPct(): number {
  const n = Number.parseFloat(lsGet(KEY) ?? "")
  return Number.isFinite(n) && n >= MIN_PCT ? n : DEFAULT_PCT
}

// sidebarPctNow reads the current open width synchronously (cache first, falling
// back to localStorage on a cold load) — for the expand callback, which needs
// the value imperatively rather than reactively.
export function sidebarPctNow(): number {
  return queryClient.getQueryData<number>(qk.sidebarPct) ?? readSidebarPct()
}

// setSidebarPct caches the width and persists it. Called as the user drags and
// before collapsing, so the next expand restores the true open width.
export function setSidebarPct(pct: number) {
  queryClient.setQueryData(qk.sidebarPct, pct)
  lsSet(KEY, String(pct))
}

// ---------------------------------------------------------------------------
// Sidebar intent — "a human just did this"
// ---------------------------------------------------------------------------
//
// The panel group reports a drag and a remount through the same onResize
// callback, so the persist path cannot tell a change someone MADE from one this
// tab merely arrived at. The server's layout claim (uilock.go) turns on exactly
// that distinction: an intentional change takes ownership of the sidebar,
// an unattended echo is refused while another client owns it.
//
// So the user-driven entry points stamp a timestamp on the way in, and the
// (debounced) persist asks whether one landed recently enough to be the cause
// of the size it is about to write. A window slightly longer than the persist
// debounce is enough to span a drag's settling frames without letting an
// unrelated resize minutes later inherit the claim.
const INTENT_MS = 2000

let intentAt = 0

// markSidebarIntent records that the human acted on the sidebar in this tab —
// ⌘\, the collapse/expand chevron, the mobile dial.
export function markSidebarIntent() {
  intentAt = Date.now()
  // A human acting here wins the claim outright, so lift any backoff this tab
  // fell into while it was the one being ignored.
  releaseLayoutBackoff()
}

// beginSidebarDrag marks the start of a handle drag and re-marks it at the end.
// A drag is the one gesture that can outlast the intent window: the stamp is
// laid down on pointerdown, but the size is not persisted until the debounce
// after the user lets go, which for a slow drag is seconds later. Stamping
// again on release puts the claim right next to the write it belongs to.
export function beginSidebarDrag() {
  markSidebarIntent()
  const end = () => {
    markSidebarIntent()
    window.removeEventListener("pointerup", end)
    window.removeEventListener("pointercancel", end)
  }
  // On window, not the handle: a drag routinely ends with the pointer somewhere
  // else entirely, and a missed release would leave the claim stamped at the
  // start of the gesture.
  window.addEventListener("pointerup", end)
  window.addEventListener("pointercancel", end)
}

// sidebarIntentFresh reports whether the layout change now being persisted can
// be attributed to a human in this tab.
export function sidebarIntentFresh(): boolean {
  return Date.now() - intentAt < INTENT_MS
}
