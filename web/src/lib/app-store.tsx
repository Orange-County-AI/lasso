import * as React from "react"

import { type ActiveState, api } from "@/lib/api"
import { applyMode, watchSystemMode } from "@/lib/mode"
import { invalidateHostScoped } from "@/lib/query"
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
        // The new host has its own remembered repo/branch/agent + repo list, so
        // drop the cached host-scoped queries; the creator reloads them on open.
        invalidateHostScoped()
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

  // Chrome light/dark follows the system color scheme (same as the main branch).
  // The inline script in index.html sets the class pre-paint; here we re-assert
  // it on mount and keep it live as the OS theme flips. The terminal palette is
  // herdr's and is handled separately (refreshTheme), so this never touches it.
  React.useEffect(() => {
    applyMode()
    watchSystemMode()
  }, [])

  // Re-pin the terminals to herdr's theme whenever its theme revision moves
  // (including the priming value, so a reload always converges). The chrome is
  // not repainted here — it's the system-driven Nothing palette. themeRev is a
  // trigger-only dep: refreshTheme() re-fetches /api/theme on each bump (SSE)
  // rather than reading the rev itself.
  // biome-ignore lint/correctness/useExhaustiveDependencies: themeRev is the intentional re-theme trigger
  React.useEffect(() => {
    refreshTheme()
  }, [state.themeRev])

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
