import * as React from "react"
import { api, type GridPane } from "@/lib/api"
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
// entry for lasso's own view state (tab + host) so Back returns to where the
// jump started, then focuses the pane. The pane id is deliberately absent from
// that entry (see restoreHost), so a same-host jump produces an entry identical
// to the current one — pushQueryParams collapses that into a replace rather
// than a dead Back step. The push happens here — callers' surfaceHerdr should
// only set the tab, not push.
export async function focusPaneInHerdr(
  p: GridPane,
  activeHost: string | null,
  surfaceHerdr: () => void
) {
  pushQueryParams({
    view: "herdr",
    // Match HostSwitcher's convention of omitting ?host for the local machine.
    host: p.host === "local" ? null : p.host,
  })
  await focusPaneCore(p, activeHost, surfaceHerdr)
}

// restoreHost re-points lasso at the host a history entry names, on
// Back/Forward, without pushing an entry of its own. It is the whole of what a
// history entry restores, because which pane herdr focuses is herdr's state,
// not lasso's: it is global to the herdr session, shared with its TUI and every
// other lasso client, so a browser Back — which the user reads as "undo my
// navigation" — must not silently re-point it for everyone. The URL therefore
// carries ?view= and ?host= (lasso's own tab and active backend) and no ?pane=.
export async function restoreHost(host: string) {
  await trackFocusWork(api.switchHost(host))
}
