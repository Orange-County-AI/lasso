import { useQuery } from "@tanstack/react-query"

import { api, type UIState, type UIStateResponse } from "@/lib/api"
import { qk, queryClient } from "@/lib/query"

// Persisted, SQLite-backed UI preferences (sidebar layout, the Files tab's
// click behavior, and usage-footer settings). One shared React Query cache is
// the source of truth in this tab; the server merges partial patches (so
// concurrent tabs can't clobber fields they didn't touch) and bumps
// ui_state_rev over SSE on every save, so every open tab converges on the same
// state (see syncUIState).
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
}

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

function touchesLayout(patch: Partial<UIState>): boolean {
  return LAYOUT_KEYS.some((k) => k in patch)
}

// patchUIState applies a partial update optimistically to the cache and sends
// ONLY the patch — the server merges it into the stored state, so there is no
// whole-object clobber and no need to wait for a fetch before writing.
//
// `intent` says a HUMAN just made this change here (a drag, ⌘\, the collapse
// chevron, the mobile dial) as opposed to this tab's panel group reporting a
// size it arrived at on its own (a mount, a window resize, applying a change
// that came in over SSE). The server needs it to arbitrate the sidebar layout:
// an intentional change takes ownership, an unattended echo is refused while
// another client owns it. Everything else in UIState only ever changes because
// someone clicked it, so those calls pass intent too.
export function patchUIState(patch: Partial<UIState>, intent = true) {
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
  const cached = queryClient.getQueryData<UIState>(qk.uiState)
  if (cached) queryClient.setQueryData(qk.uiState, { ...cached, ...body })
  void api
    .saveUIState({ ...body, client_id: clientID(), user_intent: intent })
    .then((res) => {
      // Refused: another client owns the sidebar layout. Adopt what the server
      // actually holds instead of sitting on the optimistic value — the apply
      // effect in App.tsx puts the panel back where the owner has it. No toast:
      // a human's change always wins the claim, so the only writes that can be
      // refused are ones nobody asked for.
      deniedAt = res.layout_denied ? Date.now() : 0
      if (res.layout_denied)
        queryClient.setQueryData(qk.uiState, stripMeta(res))
    })
    .catch(() => {})
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
