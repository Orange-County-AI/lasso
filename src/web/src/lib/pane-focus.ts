import * as React from "react"
import { api, type HostPane } from "@/lib/api"
import { focusHerdrTerminal } from "@/lib/terminal"
import { pushQueryParam } from "@/lib/url"

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

// focusPaneInHerdr makes a pane herdr's focused pane and hands the keyboard to
// the terminal showing it — the ⌘K switcher's open path. If the pane is on
// another host, switch there first (which reloads the herdr terminal onto that
// host), then focus its tab.
//
// It pushes one browser history entry for the host it lands on, so Back returns
// to the host the jump started from. The pane id is deliberately absent from
// that entry (see restoreHost), so a same-host jump produces an entry identical
// to the current one — pushQueryParam collapses that into a replace rather than
// a dead Back step.
export async function focusPaneInHerdr(p: HostPane, activeHost: string | null) {
  // Match HostSwitcher's convention of omitting ?host for the local machine.
  pushQueryParam("host", p.host === "local" ? null : p.host)
  await trackFocusWork(
    (async () => {
      if (p.host !== activeHost) await api.switchHost(p.host)
      if (p.workspace_id && p.tab_id) await api.focus(p.workspace_id, p.tab_id)
      focusHerdrTerminal()
    })()
  )
}

// restoreHost re-points lasso at the host a history entry names, on
// Back/Forward, without pushing an entry of its own. It is the whole of what a
// history entry restores, because which pane herdr focuses is herdr's state,
// not lasso's: it is global to the herdr session, shared with its TUI and every
// other lasso client, so a browser Back — which the user reads as "undo my
// navigation" — must not silently re-point it for everyone. The URL therefore
// carries only ?host= (lasso's active backend) and no ?pane=.
export async function restoreHost(host: string) {
  await trackFocusWork(api.switchHost(host))
}
