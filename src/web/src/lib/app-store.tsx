import * as React from "react"
import { toast } from "sonner"

import { type ActiveState, api } from "@/lib/api"
import { setTabHost, tabHost, withTabHost } from "@/lib/host"
import { applyMode, watchSystemMode } from "@/lib/mode"
import { invalidateHostScoped } from "@/lib/query"
import { refreshTheme } from "@/lib/theme"
import { syncUIState } from "@/lib/ui-state"

// App-wide state derived from herdr, kept live over the /api/events SSE stream.
// Components read activeCwd/activePaneID/panesRev reactively and run their own
// effects off them (Files follows the cwd, Diff reloads, the pane switcher's
// cached listing is refreshed on a layout change).
interface AppState {
  activeCwd: string | null
  activePaneID: string | null
  panesRev: number
  themeRev: number
  // The host THIS tab is on ("local" or an alias) and its URL path segment, both
  // straight off this tab's own SSE stream. Another tab moving to another host
  // no longer shows up here — that is the point.
  host: string | null
  hostSlug: string | null
  // The host the focused pane's cwd lives on — normally `host`, but a pane
  // ssh-attached to another host's herdr reports that host instead. Sticky like
  // activeCwd: null only until the first /api/active answer lands.
  cwdHost: string | null
}

// Fired when THIS tab moves to another host, so terminal iframes reload onto
// that host's ttyd session and host-scoped caches are dropped.
export const HOST_CHANGED_EVENT = "lasso:host-changed"

// moveTabToHost points this tab at another host: attach it server-side (pooling
// the connection and spawning its terminals), then record the choice so every
// later request carries it, then reconnect the SSE stream onto that host.
//
// The order matters. Recording the host before the attach returns would leave
// requests addressing a host whose terminals are not up yet; reconnecting the
// stream before recording it would re-subscribe to the old one.
export async function moveTabToHost(host: string) {
  if (tabHost() === host) return
  await api.attachHost(host)
  setTabHost(host)
  window.dispatchEvent(new CustomEvent(HOST_MOVED_EVENT))
}

// Internal: the SSE stream and host-scoped caches listen for this to re-subscribe.
export const HOST_MOVED_EVENT = "lasso:host-moved"

// A server-pushed toast (SSE "notice"): how background work that outlives its
// request — auto-titling a new agent, which finishes long after create-agent
// answered — reports a failure the user would otherwise only see in the log.
interface Notice {
  level?: "error" | "info" | "success"
  title: string
  detail?: string
}

function showNotice(raw: string) {
  let n: Notice
  try {
    n = JSON.parse(raw)
  } catch {
    return
  }
  if (!n?.title) return
  const opts = n.detail ? { description: n.detail } : undefined
  if (n.level === "error") toast.error(n.title, opts)
  else if (n.level === "success") toast.success(n.title, opts)
  else toast.info(n.title, opts)
}

const AppContext = React.createContext<AppState | undefined>(undefined)

export function AppProvider({ children }: { children: React.ReactNode }) {
  const [state, setState] = React.useState<AppState>({
    activeCwd: null,
    activePaneID: null,
    panesRev: -1,
    themeRev: -1,
    host: null,
    hostSlug: null,
    cwdHost: null,
  })

  // Last host seen on this tab's stream — a change means the tab moved, so the
  // terminals reload and the host-scoped caches are dropped. Tracked in a ref so
  // the SSE handler stays referentially stable.
  const lastHost = React.useRef<string | null>(null)
  // Last seen ui_state_rev — a change means some tab saved the persisted UI
  // prefs; refetch so every open tab converges (see syncUIState's echo guard).
  const lastUIStateRev = React.useRef<number | null>(null)

  const apply = React.useCallback((a: ActiveState) => {
    if (a.host) {
      if (lastHost.current !== null && a.host !== lastHost.current) {
        window.dispatchEvent(new CustomEvent(HOST_CHANGED_EVENT))
        // The new host has its own remembered repo/branch/agent + repo list, so
        // drop the cached host-scoped queries; the creator reloads them on open.
        invalidateHostScoped()
      }
      lastHost.current = a.host
    }
    if (typeof a.ui_state_rev === "number") {
      if (
        lastUIStateRev.current !== null &&
        a.ui_state_rev !== lastUIStateRev.current
      ) {
        syncUIState()
      }
      lastUIStateRev.current = a.ui_state_rev
    }
    setState((prev) => ({
      activeCwd: a.cwd || prev.activeCwd,
      activePaneID: a.pane_id || prev.activePaneID,
      panesRev: typeof a.panes_rev === "number" ? a.panes_rev : prev.panesRev,
      themeRev: typeof a.theme_rev === "number" ? a.theme_rev : prev.themeRev,
      host: a.host || prev.host,
      hostSlug: a.host_slug || prev.hostSlug,
      cwdHost: a.cwd_host || prev.cwdHost,
    }))
  }, [])

  // Initial state + live SSE updates, re-subscribed whenever this tab moves to
  // another host. An EventSource cannot send a header, so the host rides in the
  // query string; the stream then carries that host's frames only.
  React.useEffect(() => {
    let es: EventSource | null = null
    let cancelled = false

    const connect = () => {
      es?.close()
      api
        .active()
        .then((a) => {
          if (!cancelled) apply(a)
        })
        .catch(() => {
          /* SSE will populate */
        })
      es = new EventSource(withTabHost("/api/events"))
      es.addEventListener("active", (e) =>
        apply(JSON.parse((e as MessageEvent).data))
      )
      es.addEventListener("notice", (e) => showNotice((e as MessageEvent).data))
    }

    // A tab that remembers a host from a previous page load must re-attach
    // before subscribing: its terminals may have been retired (or lasso
    // restarted) while it was away, and the iframes are about to ask for them.
    const remembered = tabHost()
    if (remembered) {
      api
        .attachHost(remembered)
        .catch(() => {
          /* the host may be gone; the stream reports it down */
        })
        .then(() => {
          if (!cancelled) connect()
        })
    } else {
      connect()
    }

    window.addEventListener(HOST_MOVED_EVENT, connect)
    return () => {
      cancelled = true
      window.removeEventListener(HOST_MOVED_EVENT, connect)
      es?.close()
    }
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
