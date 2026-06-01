import { lsGet, lsSet } from "@/lib/app-store"
import { qk, queryClient } from "@/lib/query"

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
