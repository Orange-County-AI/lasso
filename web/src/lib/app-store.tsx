import * as React from "react"

import { type ActiveState, api } from "@/lib/api"
import { applyMode, watchSystemMode } from "@/lib/mode"
import { invalidateHostScoped } from "@/lib/query"

// App-wide state from the backend, kept live over the /api/events SSE stream.
// Components read activeCwd/activePaneID/panesRev reactively and run their own
// effects off them (Files follows the cwd, Diff reloads).
interface AppState {
  activeCwd: string | null
  activePaneID: string | null
  panesRev: number
  // The local host label, kept live off the SSE stream.
  host: string | null
  // Bumps when the server-persisted UI layout (/api/ui-state) is written, so
  // the Shell re-pulls it and applies another client's panel widths.
  uiRev: number
  // tab id → agent status (idle|working|blocked), pushed by the status poller.
  agentStatuses: Record<string, string>
}

// Fired when term_rev bumps so terminal iframes can reload onto a fresh ttyd
// session.
export const HOST_CHANGED_EVENT = "lasso:host-changed"

const AppContext = React.createContext<AppState | undefined>(undefined)

export function AppProvider({ children }: { children: React.ReactNode }) {
  const [state, setState] = React.useState<AppState>({
    activeCwd: null,
    activePaneID: null,
    panesRev: -1,
    host: null,
    uiRev: -1,
    agentStatuses: {},
  })

  // Last seen term_rev — a change means terminals must reload. Tracked in a ref
  // so the SSE handler stays referentially stable.
  const lastTermRev = React.useRef<number | null>(null)

  const apply = React.useCallback((a: ActiveState) => {
    if (typeof a.term_rev === "number") {
      if (lastTermRev.current !== null && a.term_rev !== lastTermRev.current) {
        window.dispatchEvent(new CustomEvent(HOST_CHANGED_EVENT))
        // Drop the cached creator queries so they reload on next open.
        invalidateHostScoped()
      }
      lastTermRev.current = a.term_rev
    }
    setState((prev) => ({
      activeCwd: a.cwd || prev.activeCwd,
      activePaneID: a.pane_id || prev.activePaneID,
      panesRev: typeof a.panes_rev === "number" ? a.panes_rev : prev.panesRev,
      host: a.host || prev.host,
      uiRev: typeof a.ui_rev === "number" ? a.ui_rev : prev.uiRev,
      agentStatuses: a.agent_statuses ?? prev.agentStatuses,
    }))
  }, [])

  // The Files/Diff panel follows the selected tab's working directory. Selection
  // lives in the Shell, so it tells the store which cwd to track via this event
  // (keeping the existing useApp().activeCwd consumers — git.ts, FilesPanel —
  // unchanged).
  React.useEffect(() => {
    const onCwd = (e: Event) => {
      const cwd = (e as CustomEvent).detail as string
      setState((prev) =>
        cwd && cwd !== prev.activeCwd ? { ...prev, activeCwd: cwd } : prev
      )
    }
    window.addEventListener("lasso:cwd", onCwd)
    return () => window.removeEventListener("lasso:cwd", onCwd)
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

  // Apply the saved appearance mode on mount (sets the html dark/light class +
  // pins the matching xterm palette) and keep it in sync with the OS while on
  // "system". The reconciler re-pins terminals across ttyd reconnects.
  React.useEffect(() => {
    applyMode()
    watchSystemMode()
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
