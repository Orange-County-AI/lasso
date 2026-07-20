import { useQuery } from "@tanstack/react-query"

import { api, type UIState } from "@/lib/api"
import { qk, queryClient } from "@/lib/query"

// Persisted, SQLite-backed UI preferences (Grid filters/watch set + sidebar
// layout). One shared React Query cache is the single source of truth in this
// tab; the server merges partial patches (so concurrent tabs can't clobber
// fields they didn't touch) and bumps ui_state_rev over SSE on every save, so
// every open tab converges on the same state (see syncUIState).

const DEFAULTS: UIState = {
  grid_agents_only: false,
  grid_hidden_hosts: [],
  grid_selected: [],
  grid_mode: "all",
  grid_watched: [],
  grid_rail_agents_only: false,
  sidebar_collapsed: false,
  sidebar_pct: 0,
  files_click_navigates: true,
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

// patchUIState applies a partial update optimistically to the cache and sends
// ONLY the patch — the server merges it into the stored state, so there is no
// whole-object clobber and no need to wait for a fetch before writing.
export function patchUIState(patch: Partial<UIState>) {
  lastPatchAt = Date.now()
  const cached = queryClient.getQueryData<UIState>(qk.uiState)
  if (cached) queryClient.setQueryData(qk.uiState, { ...cached, ...patch })
  void api.saveUIState(patch).catch(() => {})
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
