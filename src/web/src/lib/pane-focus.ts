import * as React from "react"
import { api, type GridPane, type GridPayload } from "@/lib/api"
import { qk, queryClient } from "@/lib/query"
import { focusHerdrTerminal } from "@/lib/terminal"
import { pushQueryParams } from "@/lib/url"

// In-flight counter for pane focus operations, which can take seconds when
// they switch the active host across the network. Views far from the click —
// the sidebar Files/Diff panel, which follows the focused pane's cwd — read it
// via usePaneFocusPending to show a loading state instead of silently keeping
// the previous pane's data on screen (which reads as desynchronized).
let focusInFlight = 0
const focusListeners = new Set<() => void>()
function trackFocusWork<T>(work: Promise<T>): Promise<T> {
  focusInFlight++
  for (const l of focusListeners) l()
  return work.finally(() => {
    focusInFlight--
    for (const l of focusListeners) l()
  })
}

// usePaneFocusPending reports whether any pane focus is currently in flight.
export function usePaneFocusPending(): boolean {
  return React.useSyncExternalStore(
    (cb) => {
      focusListeners.add(cb)
      return () => {
        focusListeners.delete(cb)
      }
    },
    () => focusInFlight > 0
  )
}

// focusPaneCore opens + focuses a pane in the Herdr tab, without touching
// browser history. If it's on another host, switch there first (which reloads
// the Herdr terminal onto that host), then focus its tab. Release the pane's
// grid terminal *before* surfacing Herdr so the only client left on the pane is
// the full-width Herdr terminal — otherwise herdr keeps the pane clamped to the
// grid cell's narrow width and a full-screen TUI renders thin. surfaceHerdr()
// switches the left view to the Herdr tab. Finally hand the keyboard to xterm so
// the user can type without clicking first.
function focusPaneCore(
  p: GridPane,
  activeHost: string | null,
  surfaceHerdr: () => void
) {
  return trackFocusWork(
    (async () => {
      if (p.host !== activeHost) await api.switchHost(p.host)
      if (p.workspace_id && p.tab_id) await api.focus(p.workspace_id, p.tab_id)
      await api.gridTermRelease(p.host, p.terminal_id)
      surfaceHerdr()
      focusHerdrTerminal()
    })()
  )
}

// focusPaneInPlace makes a pane herdr's focused pane WITHOUT leaving the
// current view: switch the active host if needed and focus the pane's tab, but
// push no history, release no grid terminal, and surface nothing. Used by the
// Grid tab so clicking a cell highlights it (via the SSE focus state) and the
// sidebar file viewer follows its cwd/host, while the user stays in the grid.
export function focusPaneInPlace(p: GridPane, activeHost: string | null) {
  return trackFocusWork(
    (async () => {
      if (p.host !== activeHost) await api.switchHost(p.host)
      if (p.workspace_id && p.tab_id) await api.focus(p.workspace_id, p.tab_id)
    })()
  )
}

// focusPaneInHerdr is the user-initiated focus path, shared by the Grid tab
// (header click) and the Cmd+K pane switcher. It pushes one browser history
// entry encoding the target (view + host + pane) so Back/Forward re-focus the
// pane you came from (see restorePaneFocus), then focuses the pane. The history
// push happens here — callers' surfaceHerdr should only set the tab, not push.
export async function focusPaneInHerdr(
  p: GridPane,
  activeHost: string | null,
  surfaceHerdr: () => void
) {
  pushQueryParams({
    view: "herdr",
    // Match HostSwitcher's convention of omitting ?host for the local machine.
    host: p.host === "local" ? null : p.host,
    pane: p.pane_id,
  })
  await focusPaneCore(p, activeHost, surfaceHerdr)
}

// restorePaneFocus re-focuses the pane named by a history entry (host+pane_id)
// on Back/Forward, *without* pushing a new entry. The full cross-host pane list
// is cached under qk.grid (prefetched on load); look the pane up there, fetching
// once if the cache is cold. If the pane is gone, fall back to at least
// restoring the host so the user lands somewhere sensible.
export async function restorePaneFocus(
  host: string,
  paneId: string,
  activeHost: string | null,
  surfaceHerdr: () => void
) {
  let data = queryClient.getQueryData<GridPayload>(qk.grid)
  if (!data) {
    data = await queryClient.fetchQuery<GridPayload>({
      queryKey: qk.grid,
      queryFn: () => api.gridPanes(),
    })
  }
  const p = data?.panes.find(
    (x: GridPane) => x.host === host && x.pane_id === paneId
  )
  if (p) {
    await focusPaneCore(p, activeHost, surfaceHerdr)
  } else if (host !== activeHost) {
    await trackFocusWork(api.switchHost(host))
  }
}
