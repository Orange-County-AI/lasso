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

// A drag is attributed from the RESIZE, not from a pointerdown on the handle.
// The separator is 1px wide, but react-resizable-panels hit-tests a 10px band
// around it (resizeTargetMinimumSize.fine) from its own document-level
// listener, so a grab a few pixels to either side starts a drag whose
// pointerdown lands on the neighbouring panel and never reaches the handle's
// own onPointerDown. That drag used to persist with no intent, the server
// refused it unless this tab happened to hold a fresh lease, and the adopted
// reply snapped the sidebar back the moment the user let go.
//
// So: track whether a pointer is held anywhere in this document, and treat a
// resize reported while one is held as the human's. Nothing unattended resizes
// the panel mid-press (a remount or an SSE apply does not wait for a click),
// and an apply that did coincide writes nothing, since the persist only sends
// what differs from the synced state.
let pointersHeld = 0
let draggedThisPress = false

if (typeof window !== "undefined") {
  // Capture phase on window, so it runs before the panel library's own
  // document-capture handlers and whatever they do to the event.
  window.addEventListener(
    "pointerdown",
    () => {
      pointersHeld++
    },
    true
  )
  const release = () => {
    pointersHeld = Math.max(0, pointersHeld - 1)
    if (pointersHeld > 0) return
    // Re-stamp on release: a slow drag can outlast INTENT_MS, and the size is
    // persisted a debounce AFTER the pointer comes up.
    if (draggedThisPress) markSidebarIntent()
    draggedThisPress = false
  }
  window.addEventListener("pointerup", release, true)
  window.addEventListener("pointercancel", release, true)
  // A release over another document (the terminal iframe) never reaches this
  // one; losing focus to it is the closest signal, and it must not leave the
  // count stuck above zero.
  window.addEventListener("blur", () => {
    if (pointersHeld > 0) {
      pointersHeld = 1
      release()
    }
  })
}

// noteSidebarResize is called for every size the panel reports. A resize while
// a pointer is held is a drag, whichever element the press started on.
export function noteSidebarResize() {
  if (pointersHeld === 0) return
  draggedThisPress = true
  markSidebarIntent()
}

// sidebarIntentFresh reports whether the layout change now being persisted can
// be attributed to a human in this tab.
export function sidebarIntentFresh(): boolean {
  return Date.now() - intentAt < INTENT_MS
}
