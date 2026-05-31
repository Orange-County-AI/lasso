import * as React from "react"

import { type ActiveState, api } from "@/lib/api"
import { refreshTheme } from "@/lib/theme"

// App-wide state derived from herdr, kept live over the /api/events SSE stream.
// Components read activeCwd/activePaneID/panesRev reactively and run their own
// effects off them (Files follows the cwd, Diff reloads, the grid re-highlights
// the focused pane and reloads on a layout change).
interface AppState {
  activeCwd: string | null
  activePaneID: string | null
  panesRev: number
  themeRev: number
  // Active host name ("local" or an alias), kept live off the SSE stream so the
  // footer reflects switches initiated anywhere.
  host: string | null
}

// Fired when the active host changes (term_rev bumped) so terminal iframes can
// reload onto the new host's ttyd session.
export const HOST_CHANGED_EVENT = "lasso:host-changed"

const AppContext = React.createContext<AppState | undefined>(undefined)

export function AppProvider({ children }: { children: React.ReactNode }) {
  const [state, setState] = React.useState<AppState>({
    activeCwd: null,
    activePaneID: null,
    panesRev: -1,
    themeRev: -1,
    host: null,
  })

  // Last seen term_rev — a change means the active host switched, so terminals
  // must reload. Tracked in a ref so the SSE handler stays referentially stable.
  const lastTermRev = React.useRef<number | null>(null)

  const apply = React.useCallback((a: ActiveState) => {
    if (typeof a.term_rev === "number") {
      if (lastTermRev.current !== null && a.term_rev !== lastTermRev.current) {
        window.dispatchEvent(new CustomEvent(HOST_CHANGED_EVENT))
      }
      lastTermRev.current = a.term_rev
    }
    setState((prev) => ({
      activeCwd: a.cwd || prev.activeCwd,
      activePaneID: a.pane_id || prev.activePaneID,
      panesRev: typeof a.panes_rev === "number" ? a.panes_rev : prev.panesRev,
      themeRev: typeof a.theme_rev === "number" ? a.theme_rev : prev.themeRev,
      host: a.host || prev.host,
    }))
  }, [])

  // Initial state + live SSE updates.
  React.useEffect(() => {
    let es: EventSource | null = null
    api
      .active()
      .then(apply)
      .catch(() => {
        /* SSE will populate */
      })
    es = new EventSource("/api/events")
    es.addEventListener("active", (e) =>
      apply(JSON.parse((e as MessageEvent).data))
    )
    return () => es?.close()
  }, [apply])

  // Repaint the UI + terminals whenever herdr's theme revision moves (including
  // the priming value, so a reload always converges to the current theme).
  React.useEffect(() => {
    refreshTheme()
  }, [])

  return <AppContext.Provider value={state}>{children}</AppContext.Provider>
}

export function useApp(): AppState {
  const ctx = React.useContext(AppContext)
  if (ctx === undefined)
    throw new Error("useApp must be used within an AppProvider")
  return ctx
}

// localStorage helpers that never throw (private-mode / disabled storage).
export function lsGet(key: string): string | null {
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}
export function lsSet(key: string, val: string) {
  try {
    localStorage.setItem(key, val)
  } catch {
    /* ignore */
  }
}
