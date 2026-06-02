import { useQuery } from "@tanstack/react-query"

import { type UIState, api } from "@/lib/api"
import { qk, queryClient } from "@/lib/query"

// Persisted, SQLite-backed UI preferences (Grid filters + sidebar collapse).
// One shared React Query cache is the single source of truth so the components
// that write different slices (App for the sidebar, GridTab for the filters)
// never clobber each other's fields — patchUIState merges into the cached object
// before persisting the whole thing.

const DEFAULTS: UIState = {
  grid_agents_only: false,
  grid_hidden_hosts: [],
  grid_selected: [],
  sidebar_collapsed: false,
}

// useUIState returns the persisted prefs (defaults until the first fetch lands).
// The data rarely changes, so it's effectively cached for the session.
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

// patchUIState merges a partial update into the cached prefs, updates the cache
// optimistically (so the UI reflects it immediately), and persists the whole
// object. Fire-and-forget; a failed save just leaves the server on its prior
// value, which the next fetch would reconcile.
export function patchUIState(patch: Partial<UIState>) {
  const next: UIState = { ...uiStateNow(), ...patch }
  queryClient.setQueryData(qk.uiState, next)
  void api.saveUIState(next).catch(() => {})
}
