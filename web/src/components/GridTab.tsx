import { useQuery } from "@tanstack/react-query"
import { Loader2 } from "lucide-react"
import * as React from "react"
import { toast } from "sonner"

import { type GridPane, api } from "@/lib/api"
import { lsGet, lsSet, useApp } from "@/lib/app-store"
import { tilde } from "@/lib/format"
import { qk } from "@/lib/query"
import { bootTermFrame } from "@/lib/terminal"
import { cn } from "@/lib/utils"

// How often the grid re-lists panes across hosts while the tab is open. The
// backend coalesces overlapping fetches behind a short cache, so this only needs
// to be brisk enough to feel live.
const POLL_MS = 2500

// How often a mounted cell re-pings its terminal endpoint so the server keeps the
// ttyd alive (the server reaps idle attaches after ~30s). Comfortably under that.
const KEEPALIVE_MS = 18_000

const AGENTS_ONLY_KEY = "grid-agents-only"

// The Grid tab: a wall of live terminals, one per herdr pane, spanning every
// reachable + protocol-compatible host. Each cell body is an interactive
// terminal (a ttyd attached to that pane); clicking a cell's header focuses the
// pane in the Herdr tab — switching the active host first when it lives
// elsewhere. A toggle filters to panes that have an associated agent.
export function GridTab({
  active,
  onFocusInHerdr,
}: {
  active: boolean
  onFocusInHerdr: () => void
}) {
  const { activePaneID, host: activeHost } = useApp()
  const [agentsOnly, setAgentsOnly] = React.useState(
    () => lsGet(AGENTS_ONLY_KEY) === "1"
  )

  const toggleAgentsOnly = () =>
    setAgentsOnly((v) => {
      lsSet(AGENTS_ONLY_KEY, v ? "0" : "1")
      return !v
    })

  const gridQuery = useQuery({
    queryKey: qk.grid,
    queryFn: () => api.gridPanes(),
    enabled: active,
    refetchInterval: active ? POLL_MS : false,
  })
  const data = gridQuery.data
  const error = gridQuery.isError ? (gridQuery.error as Error).message : null

  const all = data?.panes ?? null
  const panes = React.useMemo(
    () => (all && agentsOnly ? all.filter((p) => p.has_agent) : all),
    [all, agentsOnly]
  )
  const hostErrors = data?.errors ?? null

  // Focus a pane in the Herdr tab. If it's on another host, switch there first
  // (which reloads the Herdr terminal onto that host), then focus its tab.
  const focusPane = async (p: GridPane) => {
    try {
      if (p.host !== activeHost) await api.switchHost(p.host)
      if (p.workspace_id && p.tab_id) await api.focus(p.workspace_id, p.tab_id)
      onFocusInHerdr()
    } catch (e) {
      toast.error(`focus failed: ${(e as Error).message}`)
    }
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex shrink-0 flex-wrap items-center gap-2 border-border border-b px-2 py-1.5">
        <span className="text-muted-foreground text-xs">
          {panes
            ? `${panes.length} pane${panes.length === 1 ? "" : "s"}${
                agentsOnly && all ? ` of ${all.length}` : ""
              }`
            : ""}
        </span>
        {gridQuery.isFetching && (
          <Loader2 className="size-3 animate-spin text-muted-foreground" />
        )}
        <button
          type="button"
          onClick={toggleAgentsOnly}
          aria-pressed={agentsOnly}
          className={cn(
            "ml-auto flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-[11px] transition-colors",
            agentsOnly
              ? "border-primary/40 bg-accent text-foreground"
              : "border-border text-muted-foreground hover:text-foreground"
          )}
          title="Show only panes with an associated agent"
        >
          <span
            className={cn(
              "size-2 rounded-full",
              agentsOnly ? "bg-primary" : "bg-muted-foreground/40"
            )}
          />
          Agents only
        </button>
      </div>

      {/* Per-host failures (unreachable, protocol drift) — the rest still renders. */}
      {hostErrors && Object.keys(hostErrors).length > 0 && (
        <div className="flex shrink-0 flex-wrap gap-1.5 border-border border-b px-2 py-1">
          {Object.entries(hostErrors).map(([host, err]) => (
            <span
              key={host}
              className="rounded border border-warn/40 px-1.5 py-0.5 text-[10px] text-warn"
              title={err}
            >
              {host}: {err}
            </span>
          ))}
        </div>
      )}

      <div className="termgrid">
        {error ? (
          <div className="empty">
            cannot list panes
            <br />
            {error}
          </div>
        ) : !panes ? (
          <div className="empty">loading panes…</div>
        ) : panes.length === 0 ? (
          <div className="empty">
            {agentsOnly ? "no agent panes" : "no panes"}
          </div>
        ) : (
          panes.map((p) => (
            <GridCell
              key={`${p.host}|${p.pane_id}`}
              pane={p}
              focused={
                p.host === activeHost
                  ? activePaneID
                    ? p.pane_id === activePaneID
                    : !!p.focused
                  : false
              }
              onFocus={() => focusPane(p)}
            />
          ))
        )}
      </div>

      <div className="hint">
        each cell is a live terminal · click a header to focus it in Herdr
      </div>
    </div>
  )
}

// frameId derives a stable, DOM-safe iframe id from the host + terminal so the
// shared terminal wiring (bootTermFrame) can find it by id.
function frameId(host: string, terminalID: string) {
  return `gridterm-${host.replace(/[^a-zA-Z0-9_-]/g, "_")}-${terminalID}`
}

// GridCell renders one pane: a clickable header plus a lazily-mounted terminal.
// The terminal iframe is only created once the cell scrolls into view (so a long
// grid doesn't spawn dozens of ttyds at once), and a keepalive ping keeps it
// from being reaped while visible.
function GridCell({
  pane: p,
  focused,
  onFocus,
}: {
  pane: GridPane
  focused: boolean
  onFocus: () => void
}) {
  const id = frameId(p.host, p.terminal_id)
  const bodyRef = React.useRef<HTMLDivElement>(null)
  const [src, setSrc] = React.useState<string | null>(null)
  const [failed, setFailed] = React.useState(false)

  // Lazy-mount: attach the terminal once the cell is on screen.
  React.useEffect(() => {
    const el = bodyRef.current
    if (!el || src) return
    let cancelled = false
    const attach = async () => {
      try {
        const { base } = await api.gridTerm(p.host, p.terminal_id)
        if (!cancelled) setSrc(base)
      } catch {
        if (!cancelled) setFailed(true)
      }
    }
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          io.disconnect()
          void attach()
        }
      },
      { root: null, rootMargin: "200px" }
    )
    io.observe(el)
    return () => {
      cancelled = true
      io.disconnect()
    }
  }, [p.host, p.terminal_id, src])

  // Wire xterm (shift+enter, image paste, …) once the iframe exists, and keep the
  // server-side attach alive while the cell is mounted.
  React.useEffect(() => {
    if (!src) return
    const cleanup = bootTermFrame(id, true)
    const ka = setInterval(() => {
      void api.gridTerm(p.host, p.terminal_id).catch(() => {})
    }, KEEPALIVE_MS)
    return () => {
      cleanup()
      clearInterval(ka)
    }
  }, [src, id, p.host, p.terminal_id])

  const title = p.workspace_label || p.workspace_id || p.pane_id
  const tabLabel = p.tab_label && p.tab_label !== title ? p.tab_label : ""
  const tip = [p.host_label, p.workspace_label, p.tab_label, p.agent, tilde(p.cwd)]
    .filter(Boolean)
    .join("\n")

  return (
    <div className={cn("termcell", focused && "focused")}>
      <button
        type="button"
        className="termcell-head"
        title={`${tip}\n\nclick to focus in Herdr`}
        onClick={onFocus}
      >
        <span className="termcell-host">{p.host_label}</span>
        <span className="termcell-title">
          {title}
          {tabLabel ? ` · ${tabLabel}` : ""}
        </span>
        {p.has_agent && (
          <span className={cn("termcell-agent", p.agent_status)}>
            ● {p.agent || "agent"}
          </span>
        )}
      </button>
      <div ref={bodyRef} className="termcell-body">
        {src ? (
          <iframe id={id} src={src} title={`${p.host_label} ${title}`} />
        ) : (
          <div className="termcell-placeholder">
            {failed ? "terminal unavailable" : "…"}
          </div>
        )}
      </div>
    </div>
  )
}
