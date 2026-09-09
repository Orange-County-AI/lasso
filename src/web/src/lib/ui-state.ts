import { useQuery } from "@tanstack/react-query"
import { toast } from "sonner"

import {
  type AtmospherePref,
  api,
  type UIState,
  type UIStatePatch,
  type UIStateResponse,
} from "@/lib/api"
import { qk, queryClient } from "@/lib/query"

// Persisted, SQLite-backed UI preferences (sidebar layout, the Files tab's
// click behavior, usage-footer settings, the appearance mode and its palettes,
// and the per-theme backdrop). One shared React Query cache is the source of
// truth in this tab; the server
// merges partial patches (so concurrent tabs can't clobber fields they didn't
// touch) and bumps ui_state_rev over SSE on every save, so every open tab —
// and every other browser on this lasso — converges on the same state (see
// syncUIState).
//
// The sidebar layout is the exception to "merge and converge": every tab
// re-persists it from its own panel group without anyone asking, so one stale
// client can reopen a sidebar the human just collapsed, over and over. Those
// two fields are therefore arbitrated by an ownership claim on the server
// (uilock.go) and each write says whether a human was behind it — see
// patchUIState's `intent`.

const DEFAULTS: UIState = {
  sidebar_collapsed: false,
  sidebar_pct: 0,
  files_click_navigates: true,
  usage_hidden: [],
  usage_order: [],
  usage_compact: false,
  theme_atmosphere: {},
  custom_backgrounds: [],
  // Mirrors the server's own defaults (getUIState in db.go) and the class
  // index.html paints pre-paint, so the instant before the first fetch lands
  // looks like a browser that has never been told anything — not like a
  // fourth appearance nobody chose.
  appearance_mode: "herdr",
  palette_light: "",
  palette_dark: "",
}

// The gallery cap, mirroring maxCustomBackgrounds in db.go. Only the optimistic
// copy needs it — the server trims what it stores either way — but a list that
// grows past the cap for one round trip and then snaps back is a flicker.
const MAX_CUSTOM_BACKGROUNDS = 24

// useUIState returns the persisted prefs (defaults until the first fetch lands).
// Kept fresh across tabs by syncUIState (SSE-driven), not by polling.
export function useUIState(): UIState {
  const q = useQuery({
    queryKey: qk.uiState,
    queryFn: () => api.uiState(),
    staleTime: Number.POSITIVE_INFINITY,
  })
  return q.data ?? DEFAULTS
}

// uiStateNow reads the current cached prefs synchronously (defaults if unfetched)
// — for non-component code and merges.
export function uiStateNow(): UIState {
  return queryClient.getQueryData<UIState>(qk.uiState) ?? DEFAULTS
}

// uiStateSettled says whether the server's copy has ARRIVED — or failed to.
// The atmosphere needs the distinction that uiStateNow's defaults erase: a
// theme with no stored entry wears lasso's default backdrop, so painting that
// default while the fetch is still in flight would drop a photograph on a
// browser whose owner turned it off, then take it away again a beat later.
// lib/wallpaper.ts therefore paints nothing until this is true (see
// atmosphereKnown), and the subscription repaints when it flips.
//
// A FAILED fetch settles too: an unreachable /api/ui-state must degrade to the
// documented defaults, not to a terminal that never gets its palette pinned.
// The query is mounted for the app's life (Shell's useUIState), so a state
// exists from the first render — `undefined` here is only the instant before.
export function uiStateSettled(): boolean {
  const state = queryClient.getQueryState<UIState>(qk.uiState)
  return !!state && state.status !== "pending"
}

// How long after a local write we hold off applying an SSE-triggered refetch.
// The rev bump echoes back to the writing tab; refetching immediately could
// land a response from BEFORE a rapid follow-up write and briefly revert the
// optimistic UI. The trailing sync after the window converges everything.
const ECHO_MS = 1000

let lastPatchAt = 0
let pendingSync: ReturnType<typeof setTimeout> | null = null

// clientID identifies this TAB to the server's sidebar-layout claim (see
// uilock.go). Per tab, not per browser: two tabs on one machine are two clients
// that can disagree about the sidebar, and sessionStorage is per tab by
// construction — the same reason the tab's host lives there (lib/host.ts). It
// survives a reload, which is what keeps a refreshing tab from silently
// dropping the lock it was holding.
const CLIENT_KEY = "lasso-client-id"

let cachedClientID: string | null = null

// crypto.randomUUID exists only in a secure context, and lasso is routinely
// reached over plain http on a tailnet address — which is not one. Falling back
// to Math.random is fine here: this id only has to be unlikely to collide with
// the handful of other tabs open on the same server, and it authorizes nothing.
function newClientID(): string {
  try {
    return crypto.randomUUID()
  } catch {
    return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`
  }
}

function clientID(): string {
  if (cachedClientID) return cachedClientID
  let id = ""
  try {
    id = sessionStorage.getItem(CLIENT_KEY) ?? ""
    if (!id) {
      id = newClientID()
      sessionStorage.setItem(CLIENT_KEY, id)
    }
  } catch {
    // Private mode / storage disabled: a per-load id still distinguishes this
    // tab from the others for as long as it is open, which is all the claim
    // needs. It just can't survive a reload.
    id = newClientID()
  }
  cachedClientID = id
  return id
}

// How long a client that was refused the sidebar layout stops offering it. The
// panel group that produced the refused size will keep producing it — that is
// what makes a rogue client rogue — so without this a losing tab re-asks every
// time its debounce fires, forever. A human acting in the tab clears it
// immediately (see markSidebarIntent's caller below), so this costs a user
// nothing; it only quiets a tab nobody is using.
const DENY_BACKOFF_MS = 30_000

let deniedAt = 0

// releaseLayoutBackoff lets this tab offer the layout again. Called when the
// human acts here, because their intent is what wins the claim outright.
export function releaseLayoutBackoff() {
  deniedAt = 0
}

const LAYOUT_KEYS = ["sidebar_collapsed", "sidebar_pct"] as const

function touchesLayout(patch: UIStatePatch): boolean {
  return LAYOUT_KEYS.some((k) => k in patch)
}

// mergePatch folds one patch onto another, and mergeLocal folds a patch onto
// the cached state. Both mirror the server's merge (mergeThemeAtmosphere /
// mergeCustomBackgrounds in hostpanes.go), because the optimistic copy has to
// agree with the answer coming back: a shallow spread would replace the whole
// per-theme map with the one entry being written, blanking every other theme's
// backdrop until the next fetch.
function mergeAtmosphereInto(
  base: Record<string, AtmospherePref> | undefined,
  patch: Record<string, AtmospherePref>
): Record<string, AtmospherePref> {
  const out = { ...base }
  for (const [theme, pref] of Object.entries(patch))
    out[theme] = { ...out[theme], ...pref }
  return out
}

function mergePatch(a: UIStatePatch, b: UIStatePatch): UIStatePatch {
  const out: UIStatePatch = { ...a, ...b }
  if (a.theme_atmosphere && b.theme_atmosphere)
    out.theme_atmosphere = mergeAtmosphereInto(
      a.theme_atmosphere,
      b.theme_atmosphere
    )
  return out
}

function mergeLocal(cached: UIState, patch: UIStatePatch): UIState {
  const {
    theme_atmosphere: atmosphere,
    remember_background: remember,
    forget_background: forget,
    ...fields
  } = patch
  const out: UIState = { ...cached, ...fields }
  if (atmosphere)
    out.theme_atmosphere = mergeAtmosphereInto(
      cached.theme_atmosphere,
      atmosphere
    )
  if (remember || forget) {
    // Newest first, deduped, capped — the same three rules the server applies,
    // so re-adding a picture moves it to the front instead of doubling it.
    const rest = (cached.custom_backgrounds ?? []).filter(
      (u) => u !== remember && u !== forget
    )
    out.custom_backgrounds = (remember ? [remember, ...rest] : rest).slice(
      0,
      MAX_CUSTOM_BACKGROUNDS
    )
  }
  return out
}

// A pending write, either held back by `coalesceMs` or waiting for the request
// in flight to settle. One slot: writes are SERIALIZED (see sendPatch), and a
// queue of patches and a single merged patch reach the same stored state —
// while the merged one cannot arrive out of order. A differently-shaped patch
// arriving meanwhile is folded in rather than dropped or reordered, which is
// what makes the dimming slider and the reset button beside it safe: the reset
// merges over the tick still waiting on its timer instead of racing past it.
let pending: UIStatePatch | null = null
let pendingIntent = true
let pendingTimer: ReturnType<typeof setTimeout> | null = null

// Whether a save is on the wire. Only one is, ever: two concurrent saves settle
// in whatever order the network gives them, and since a response is now ADOPTED
// (below) the slower one would write the older merge over the newer.
let inFlight = false

// Bumped by every local write. A response may only be adopted while this still
// holds the value it had when the request left — anything newer means the cache
// already carries a change the server has not seen, and adopting would revert
// what the human is looking at.
let writeSeq = 0

// One toast id for every save failure, so a drag's worth of them is one line
// rather than a stack.
const SAVE_TOAST_ID = "ui-state-save"

// How long before re-reading the server's copy after a save failed AND the
// re-read failed too (i.e. lasso is unreachable). Only armed while a failure is
// outstanding, and disarmed by the first successful read — a tab that cannot
// reach lasso would otherwise sit on an optimistic value forever, since nothing
// bumps ui_state_rev for a write that never landed.
const RECOVER_MS = 5000

let recoverTimer: ReturnType<typeof setTimeout> | null = null

// Set by a failed save, cleared by the next successful one. The recovery read
// cannot be issued from the rejection handler itself: `inFlight` is still true
// there (it is cleared in the `finally`, which runs after), so the read would
// stand down against the very write that just failed and nothing would ever
// re-arm it. The settle handler below is where it fires.
let recoverWanted = false

// recoverUIState replaces the optimistic cache with what the server actually
// holds. Called when a save failed: the local copy then contains a change
// nothing persisted, so the honest state is the server's — and re-reading gets
// it without having to snapshot and unwind the patch, which would also throw
// away every OTHER field that changed meanwhile.
//
// It stands down while a write is queued or on the wire: that write's own
// settle is the newer answer, and reverting under it would flash a value the
// human has already replaced. The same reasoning applies to the read's own
// flight, hence the `writeSeq` ticket.
function recoverUIState() {
  recoverTimer = null
  if (inFlight || pending) return
  const seq = writeSeq
  void api.uiState().then(
    (state) => {
      if (seq === writeSeq && !inFlight && !pending)
        queryClient.setQueryData(qk.uiState, state)
    },
    () => {
      recoverWanted = true
      if (!recoverTimer) recoverTimer = setTimeout(recoverUIState, RECOVER_MS)
    }
  )
}

function flushPending() {
  pendingTimer = null
  // A save is on the wire: it will flush what is queued when it settles, so
  // the merged patch stays queued rather than becoming a second concurrent
  // write whose response could land out of order.
  if (inFlight || !pending) return
  const body = pending
  pending = null
  const intent = pendingIntent
  pendingIntent = true
  sendPatch(body, intent)
}

function sendPatch(body: UIStatePatch, intent: boolean) {
  lastPatchAt = Date.now()
  inFlight = true
  const seq = writeSeq
  void api
    .saveUIState({ ...body, client_id: clientID(), user_intent: intent })
    .then(
      (res) => {
        // Refused: another client owns the sidebar layout, and the body is the
        // owner's state. No toast — a human's change always wins the claim, so
        // the only writes that can be refused are ones nobody asked for.
        deniedAt = res.layout_denied ? Date.now() : 0
        // This write reached the server, so an earlier failure's pending
        // re-read is moot: the answer to it is in this very body.
        recoverWanted = false
        // Adopt the acknowledged whole. It is the server's merge of this patch
        // over everything else stored, so it is also how a field another
        // browser changed lands here without waiting for the SSE bump — and
        // how a refused layout is put back where the owner has it (App.tsx's
        // apply effect moves the panel). Skipped when anything was written
        // here since the request left: that change is not in this body, and
        // the follow-up write's own response carries both.
        if (seq === writeSeq && !pending)
          queryClient.setQueryData(qk.uiState, stripMeta(res))
      },
      () => {
        // A preference that silently failed to save is worse than one that
        // refused to change: the control keeps showing the new value and the
        // next reload undoes it. Say so, then converge on the server.
        toast.error("Couldn't save your preferences", {
          id: SAVE_TOAST_ID,
          description: "Restoring what lasso has stored.",
        })
        recoverWanted = true
      }
    )
    .finally(() => {
      inFlight = false
      // Anything that queued while this was on the wire goes out now, unless
      // its own coalescing timer is still running. Its own settle decides
      // whether a recovery is still needed, so this returns rather than
      // reading the server under a write that is about to change it.
      if (pending && !pendingTimer) {
        flushPending()
        return
      }
      if (recoverWanted) {
        recoverWanted = false
        recoverUIState()
      }
    })
}

// patchUIState applies a partial update optimistically to the cache and sends
// ONLY the patch — the server merges it into the stored state, so there is no
// whole-object clobber and no need to wait for a fetch before writing. The
// response is then adopted (see sendPatch), so a tab converges on the stored
// truth on every write rather than only on the next SSE bump.
//
// `intent` says a HUMAN just made this change here (a drag, ⌘\, the collapse
// chevron, the mobile dial) as opposed to this tab's panel group reporting a
// size it arrived at on its own (a mount, a window resize, applying a change
// that came in over SSE). The server needs it to arbitrate the sidebar layout:
// an intentional change takes ownership, an unattended echo is refused while
// another client owns it. Everything else in UIState only ever changes because
// someone clicked it, so those calls pass intent too.
//
// `coalesceMs` holds the network write back that long while the optimistic
// cache update (and therefore the repaint) still happens on the spot. The
// dimming slider fires on every pixel of a drag, and each save broadcasts a
// ui_state_rev bump to every open tab — a save per tick would turn one drag
// into a fleet-wide refetch storm. The timer is not restarted by a follow-up
// tick, so a long drag still lands a write every coalesceMs rather than only
// on release. An IMMEDIATE write (coalesceMs 0) does not jump the queue: it is
// merged over whatever is waiting and sent as one patch, so the slider's last
// tick can never land after the reset that followed it.
export function patchUIState(
  patch: UIStatePatch,
  intent = true,
  coalesceMs = 0
) {
  let body = patch
  if (
    !intent &&
    touchesLayout(patch) &&
    Date.now() - deniedAt < DENY_BACKOFF_MS
  ) {
    // Recently refused and still nobody driving here: drop the layout rather
    // than re-ask. Dropped BEFORE the optimistic cache write, so this tab
    // doesn't briefly render a layout it already knows the server won't take.
    const { sidebar_collapsed: _c, sidebar_pct: _p, ...rest } = patch
    body = rest
    if (Object.keys(body).length === 0) return
  }
  lastPatchAt = Date.now()
  writeSeq++
  const cached = queryClient.getQueryData<UIState>(qk.uiState)
  if (cached) queryClient.setQueryData(qk.uiState, mergeLocal(cached, body))
  pending = pending ? mergePatch(pending, body) : body
  pendingIntent = pendingIntent && intent
  if (coalesceMs > 0) {
    if (!pendingTimer) pendingTimer = setTimeout(flushPending, coalesceMs)
    return
  }
  // Immediate: cancel the coalescing timer this patch now rides in front of,
  // and send the merged body (flushPending stands down if a save is already on
  // the wire — its settle sends this one).
  if (pendingTimer) {
    clearTimeout(pendingTimer)
    pendingTimer = null
  }
  flushPending()
}

// stripMeta drops the response-only fields so nothing but preferences reaches
// the cache the UI renders from.
function stripMeta(res: UIStateResponse): UIState {
  const { layout_denied: _denied, ...state } = res
  return state
}

// syncUIState refetches the persisted prefs — called when the SSE ui_state_rev
// moves (some tab, possibly this one, saved). Recent local writes defer the
// refetch past the echo window so it can't briefly revert an optimistic update.
export function syncUIState() {
  const since = Date.now() - lastPatchAt
  if (since < ECHO_MS) {
    if (!pendingSync) {
      pendingSync = setTimeout(
        () => {
          pendingSync = null
          syncUIState()
        },
        ECHO_MS - since + 50
      )
    }
    return
  }
  void queryClient.invalidateQueries({ queryKey: qk.uiState })
}
