import { lsGet, lsSet } from "@/lib/app-store"

// Device-local record of which panes (host|pane_id keys) this browser has
// already seen, backing the Grid tab's "+N new" badge in Watch mode. Kept in
// localStorage rather than the server-side UIState blob: "seen" models this
// device's eyeballs (a second browser genuinely hasn't seen the panes), and
// the UIState client POSTs the whole object, so a high-churn field there
// would invite cross-device last-write-wins clobbering of grid_watched.
//
// The set stays bounded: reconcileSeen prunes keys for panes that no longer
// exist on hosts that listed successfully, so it can never exceed the live
// pane count (plus keys for hosts currently unreachable).

const KEY = "lasso-grid-seen"

// loadSeen reads the stored set; null means the feature has never initialized
// on this device (distinct from an empty set).
export function loadSeen(): Set<string> | null {
  const raw = lsGet(KEY)
  if (raw == null) return null
  try {
    const arr: unknown = JSON.parse(raw)
    return new Set(
      Array.isArray(arr)
        ? arr.filter((x): x is string => typeof x === "string")
        : []
    )
  } catch {
    return new Set()
  }
}

function save(seen: Set<string>) {
  lsSet(KEY, JSON.stringify(Array.from(seen)))
}

// markSeen unions keys into the stored set.
export function markSeen(keys: Iterable<string>) {
  const seen = loadSeen() ?? new Set<string>()
  let changed = false
  for (const k of keys) {
    if (!seen.has(k)) {
      seen.add(k)
      changed = true
    }
  }
  if (changed) save(seen)
}

// reconcileSeen syncs the stored set against the live pane keys and returns
// the result. First-ever call seeds with all live keys, so nothing reads as
// "new" the moment the feature ships (or on a fresh device). Otherwise it
// prunes keys for panes that are gone — but only when the pane's host is
// present among liveKeys, since a host missing from the payload (unreachable,
// listing failed) says nothing about its panes.
export function reconcileSeen(liveKeys: Set<string>): Set<string> {
  const seen = loadSeen()
  if (seen == null) {
    const seeded = new Set(liveKeys)
    save(seeded)
    return seeded
  }
  const liveHosts = new Set<string>()
  for (const k of liveKeys) liveHosts.add(k.slice(0, k.indexOf("|")))
  let changed = false
  for (const k of seen) {
    if (!liveKeys.has(k) && liveHosts.has(k.slice(0, k.indexOf("|")))) {
      seen.delete(k)
      changed = true
    }
  }
  if (changed) save(seen)
  return seen
}
