import * as React from "react"
import { api } from "@/lib/api"
import { moveTabToHost } from "@/lib/app-store"

// In-flight counter for pane focus operations, which can take seconds when
// they move this tab to another host across the network. Views far from the
// click —
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

// focusCreatedAgent lands herdr on an agent the creator just made. The pane id
// the create returns is not enough on its own: focusing a PANE means focusing
// its tab, and only pane.list knows which tab that is — a listing that can still
// be answering from a call issued before the pane existed (it takes 0.5-1.5s on
// a busy session; the server invalidates its cache on create, but a remote
// host's herdr can also lag its own create). A single miss used to abandon the
// navigation entirely, which is what made landing on a new agent a coin flip.
//
// So it retries the lookup briefly, and then falls back to focusing the
// WORKSPACE: landing on the new agent is the point, and its workspace has one
// tab at this stage anyway.
export async function focusCreatedAgent(
  workspaceID: string | undefined,
  rootPane: string | undefined
) {
  if (!workspaceID && !rootPane) {
    throw new Error("The created pane is not available in Herdr")
  }
  const deadline = Date.now() + 4000
  let lastErr: unknown
  while (rootPane) {
    try {
      const { panes } = await api.panes()
      const pane = panes?.find((p) => p.pane_id === rootPane)
      if (pane?.workspace_id && pane.tab_id) {
        await api.focus(pane.workspace_id, pane.tab_id)
        return
      }
    } catch (err) {
      lastErr = err
    }
    if (Date.now() >= deadline) break
    await new Promise((r) => setTimeout(r, 300))
  }
  if (!workspaceID) {
    throw lastErr instanceof Error
      ? lastErr
      : new Error("The created pane is not available in Herdr")
  }
  await api.focus(workspaceID)
}

// restoreHost re-points THIS TAB at the host a history entry names, on
// Back/Forward, without pushing an entry of its own. It is the whole of what a
// history entry restores, because which pane herdr focuses is herdr's state,
// not lasso's: it is global to the herdr session, shared with its TUI and every
// other lasso client, so a browser Back — which the user reads as "undo my
// navigation" — must not silently re-point it for everyone. The URL therefore
// carries only ?host= (this tab's host) and no ?pane=.
export async function restoreHost(host: string) {
  await trackFocusWork(moveTabToHost(host))
}
