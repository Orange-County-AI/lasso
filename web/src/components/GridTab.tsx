import { useQuery } from "@tanstack/react-query"
import * as React from "react"
import { toast } from "sonner"

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuTrigger,
} from "@/components/ui/context-menu"
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { type GridPane, api } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { tilde } from "@/lib/format"
import { focusPaneInHerdr } from "@/lib/pane-focus"
import { qk } from "@/lib/query"
import { bootTermFrame, whenTerminalReady } from "@/lib/terminal"
import { GRID_FRAME_CLASS } from "@/lib/theme"
import { patchUIState, useUIState } from "@/lib/ui-state"
import { cn } from "@/lib/utils"

// How often the grid re-lists panes across hosts while the tab is open. The
// backend coalesces overlapping fetches behind a short cache, so this only needs
// to be brisk enough to feel live.
const POLL_MS = 2500

// How often a mounted cell re-pings its terminal endpoint so the server keeps the
// ttyd alive (the server reaps idle attaches after ~30s). Comfortably under that.
const KEEPALIVE_MS = 18_000

// Grid layout constants — must match .termgrid in index.css (gap) and .termcell
// (flex-basis) so the measured column count matches what flexbox actually packs.
const GRID_GAP = 14
const GRID_MIN_CELL = 360

// cellKey uniquely identifies a pane across hosts (pane ids are only unique
// within a host).
const cellKey = (p: GridPane) => `${p.host}|${p.pane_id}`

// The Grid tab: a wall of live terminals, one per herdr pane, spanning every
// reachable + protocol-compatible host. Each cell body is an interactive
// terminal (a ttyd attached to that pane). Click a header to focus the pane in
// the Herdr tab (switching host first when it lives elsewhere); ⌘/Ctrl-click to
// multi-select; right-click for rename / close. Filters (agents-only, per-host)
// are persisted server-side so the view is the same every visit.
export function GridTab({
  active,
  onFocusInHerdr,
}: {
  active: boolean
  onFocusInHerdr: () => void
}) {
  const { activePaneID, host: activeHost } = useApp()
  const ui = useUIState()
  const agentsOnly = ui.grid_agents_only
  const hidden = React.useMemo(
    () => new Set(ui.grid_hidden_hosts),
    [ui.grid_hidden_hosts]
  )

  // Measure how many columns flexbox will pack so we can place the remainder
  // (the newest panes, since they sort first) in the top row — which then grows
  // to fill the full width — rather than leaving a stretched, oversized cell in
  // the bottom row.
  const gridRef = React.useRef<HTMLDivElement>(null)
  const [cols, setCols] = React.useState(1)
  React.useLayoutEffect(() => {
    const el = gridRef.current
    if (!el) return
    const measure = () => {
      const content = el.clientWidth - GRID_GAP * 2 // .termgrid horizontal padding
      setCols(
        Math.max(
          1,
          Math.floor((content + GRID_GAP) / (GRID_MIN_CELL + GRID_GAP))
        )
      )
    }
    measure()
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  // Multi-selection, persisted server-side (host|pane_id keys) so it survives
  // navigating away and back or reloading.
  const selected = React.useMemo(
    () => new Set(ui.grid_selected),
    [ui.grid_selected]
  )
  const [renameTarget, setRenameTarget] = React.useState<GridPane | null>(null)
  const [renameValue, setRenameValue] = React.useState("")
  const [closeTargets, setCloseTargets] = React.useState<GridPane[] | null>(
    null
  )

  const gridQuery = useQuery({
    queryKey: qk.grid,
    queryFn: () => api.gridPanes(),
    enabled: active,
    refetchInterval: active ? POLL_MS : false,
  })
  const data = gridQuery.data
  const error = gridQuery.isError ? (gridQuery.error as Error).message : null
  const reload = gridQuery.refetch

  // Authoritative teardown: whenever the Grid view isn't active (and on unmount),
  // release every grid terminal. Each cell already releases its own attach, but
  // this backstops a dropped or keepalive-raced per-cell release so no thin grid
  // attach lingers in the background and clamps a pane viewed full-size in Herdr.
  React.useEffect(() => {
    if (active) return
    void api.gridTermReleaseAll()
  }, [active])

  const all = data?.panes ?? null
  const hostErrors = data?.errors ?? null

  // The distinct hosts present, for the per-host filter chips (label kept for
  // display). Only worth showing when more than one host is in play.
  const hosts = React.useMemo(() => {
    const m = new Map<string, string>()
    for (const p of all ?? []) if (!m.has(p.host)) m.set(p.host, p.host_label)
    return Array.from(m, ([host, label]) => ({ host, label }))
  }, [all])

  const panes = React.useMemo(
    () =>
      all
        ? all.filter((p) => (!agentsOnly || p.has_agent) && !hidden.has(p.host))
        : null,
    [all, agentsOnly, hidden]
  )

  const toggleAgentsOnly = () => patchUIState({ grid_agents_only: !agentsOnly })

  const toggleHost = (host: string) => {
    const next = new Set(hidden)
    if (next.has(host)) next.delete(host)
    else next.add(host)
    patchUIState({ grid_hidden_hosts: Array.from(next) })
  }

  const clearSelection = () => patchUIState({ grid_selected: [] })
  const toggleSelect = (key: string) => {
    const next = new Set(selected)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    patchUIState({ grid_selected: Array.from(next) })
  }

  // Header click: plain click focuses the pane in Herdr; ⌘/Ctrl/Shift-click
  // toggles selection instead.
  const onCellClick = (e: React.MouseEvent, p: GridPane) => {
    if (e.metaKey || e.ctrlKey || e.shiftKey) {
      toggleSelect(cellKey(p))
      return
    }
    clearSelection()
    void focusPane(p)
  }

  // Focus a pane in the Herdr tab (see focusPaneInHerdr for the sequence; shared
  // with the Cmd+U pane switcher).
  const focusPane = async (p: GridPane) => {
    try {
      await focusPaneInHerdr(p, activeHost, onFocusInHerdr)
    } catch (e) {
      toast.error(`focus failed: ${(e as Error).message}`)
    }
  }

  // Close targets: the whole selection if the right-clicked pane is part of it,
  // otherwise just that pane.
  const requestClose = (p: GridPane) => {
    if (selected.has(cellKey(p)) && all) {
      setCloseTargets(all.filter((x) => selected.has(cellKey(x))))
    } else {
      setCloseTargets([p])
    }
  }

  const requestRename = (p: GridPane) => {
    setRenameTarget(p)
    setRenameValue(p.workspace_label || "")
  }

  const submitRename = async () => {
    const p = renameTarget
    if (!p) return
    const label = renameValue.trim()
    setRenameTarget(null)
    if (!p.workspace_id || !label || label === p.workspace_label) return
    try {
      await api.gridRename(p.host, p.workspace_id, label)
      reload()
    } catch (e) {
      toast.error(`rename failed: ${(e as Error).message}`)
    }
  }

  const doClose = async (targets: GridPane[]) => {
    setCloseTargets(null)
    clearSelection()
    try {
      const res = await api.gridClose(
        targets.map((t) => ({ host: t.host, pane_id: t.pane_id }))
      )
      reload()
      const nErr = res.errors ? Object.keys(res.errors).length : 0
      if (nErr)
        toast.error(`close failed for ${nErr} pane${nErr === 1 ? "" : "s"}`)
    } catch (e) {
      toast.error(`close failed: ${(e as Error).message}`)
    }
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex shrink-0 flex-wrap items-center gap-2 border-border border-b px-2 py-1.5">
        {selected.size > 0 ? (
          <>
            <span className="text-foreground text-xs">
              {selected.size} selected
            </span>
            <button
              type="button"
              onClick={() =>
                all &&
                setCloseTargets(all.filter((x) => selected.has(cellKey(x))))
              }
              className="rounded border border-bad/50 px-1.5 py-0.5 text-[11px] text-bad hover:bg-accent"
            >
              Close
            </button>
            <button
              type="button"
              onClick={clearSelection}
              className="rounded border border-border px-1.5 py-0.5 text-[11px] text-muted-foreground hover:text-foreground"
            >
              Clear
            </button>
          </>
        ) : (
          <span className="text-muted-foreground text-xs">
            {panes
              ? `${panes.length} pane${panes.length === 1 ? "" : "s"}${
                  panes.length !== (all?.length ?? 0)
                    ? ` of ${all?.length}`
                    : ""
                }`
              : ""}
          </span>
        )}

        {/* Per-host filter chips (only when more than one host is present). */}
        {hosts.length > 1 &&
          hosts.map((h) => {
            const on = !hidden.has(h.host)
            return (
              <button
                key={h.host}
                type="button"
                onClick={() => toggleHost(h.host)}
                aria-pressed={on}
                className={cn(
                  "flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-[11px] transition-colors",
                  on
                    ? "border-primary/40 bg-accent text-foreground"
                    : "border-border text-muted-foreground hover:text-foreground"
                )}
                title={on ? `Hide ${h.label} panes` : `Show ${h.label} panes`}
              >
                <span
                  className={cn(
                    "size-2 rounded-full",
                    on ? "bg-primary" : "bg-muted-foreground/40"
                  )}
                />
                {h.label}
              </button>
            )
          })}

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

      <div ref={gridRef} className="termgrid">
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
            {agentsOnly || hidden.size
              ? "no panes match the filters"
              : "no panes"}
          </div>
        ) : (
          panes.map((p, i) => {
            // A flex break after the first `remainder` cells puts them in the
            // top row (where they grow to fill the full width); the older panes
            // pack into full rows below. No break when the count divides evenly.
            const remainder = cols > 0 ? panes.length % cols : 0
            const breakHere =
              remainder > 0 && remainder < panes.length && i === remainder - 1
            return (
              <React.Fragment key={cellKey(p)}>
                <GridCell
                  pane={p}
                  active={active}
                  selected={selected.has(cellKey(p))}
                  selectionCount={selected.size}
                  focused={
                    p.host === activeHost
                      ? activePaneID
                        ? p.pane_id === activePaneID
                        : !!p.focused
                      : false
                  }
                  onClick={(e) => onCellClick(e, p)}
                  onRename={() => requestRename(p)}
                  onClose={() => requestClose(p)}
                />
                {breakHere && <div className="termbreak" aria-hidden="true" />}
              </React.Fragment>
            )
          })
        )}
      </div>

      <div className="hint">
        click a header to focus in Herdr · ⌘/Ctrl-click to select · right-click
        for rename / close
      </div>

      {/* rename the workspace (relabels every pane grouped under it on that host) */}
      <Dialog
        open={renameTarget != null}
        onOpenChange={(o) => !o && setRenameTarget(null)}
      >
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>Rename workspace</DialogTitle>
          </DialogHeader>
          <Input
            autoFocus
            placeholder={renameTarget?.workspace_id || "workspace"}
            value={renameValue}
            onChange={(e) => setRenameValue(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") submitRename()
            }}
          />
          <DialogFooter>
            <Button variant="outline" onClick={() => setRenameTarget(null)}>
              Cancel
            </Button>
            <Button onClick={submitRename}>Rename</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* close confirmation — terminates the pane(s) and any agent in them */}
      <AlertDialog
        open={closeTargets != null}
        onOpenChange={(o) => !o && setCloseTargets(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {closeTargets && closeTargets.length > 1
                ? `Close ${closeTargets.length} panes?`
                : closeTargets?.[0]?.has_agent
                  ? "Close agent?"
                  : "Close pane?"}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {closeTargets && closeTargets.length === 1
                ? [
                    closeTargets[0].host_label,
                    closeTargets[0].workspace_label,
                    closeTargets[0].agent,
                  ]
                    .filter(Boolean)
                    .join(" · ") || closeTargets[0].pane_id
                : "This terminates the selected terminals and any agents running in them."}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => closeTargets && doClose(closeTargets)}
            >
              Close
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

// frameId derives a stable, DOM-safe iframe id from the host + terminal so the
// shared terminal wiring (bootTermFrame) can find it by id.
function frameId(host: string, terminalID: string) {
  return `gridterm-${host.replace(/[^a-zA-Z0-9_-]/g, "_")}-${terminalID}`
}

// GridCell renders one pane: a clickable header (right-click for actions) plus a
// lazily-mounted terminal. The terminal iframe is only created once the cell
// scrolls into view (so a long grid doesn't spawn dozens of ttyds at once) and
// only while the Grid tab is active.
function GridCell({
  pane: p,
  active,
  selected,
  selectionCount,
  focused,
  onClick,
  onRename,
  onClose,
}: {
  pane: GridPane
  active: boolean
  selected: boolean
  selectionCount: number
  focused: boolean
  onClick: (e: React.MouseEvent) => void
  onRename: () => void
  onClose: () => void
}) {
  const id = frameId(p.host, p.terminal_id)
  const bodyRef = React.useRef<HTMLDivElement>(null)
  const [src, setSrc] = React.useState<string | null>(null)
  const [failed, setFailed] = React.useState(false)
  // The ttyd iframe flashes its own connect/reconnect chrome before herdr
  // repaints the pane, so we keep a loading overlay on top until xterm has
  // actually rendered content (see whenTerminalReady below).
  const [ready, setReady] = React.useState(false)

  // Lazy-mount: attach the terminal once the cell is on screen — but only while
  // the Grid tab is active. (Keeping it attached when hidden would clamp the
  // pane's width to this thin cell while it's viewed full-size in Herdr.)
  React.useEffect(() => {
    if (!active || src) return
    const el = bodyRef.current
    if (!el) return
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
  }, [active, src, p.host, p.terminal_id])

  // When the Grid tab is hidden, detach this pane's terminal (drop the iframe and
  // kill its server-side ttyd) so herdr stops sizing the pane to this cell — the
  // focused pane then resizes to the full-width Herdr terminal. Re-attaches when
  // the tab is shown again.
  React.useEffect(() => {
    if (active) return
    setSrc(null)
    setFailed(false)
    setReady(false)
    void api.gridTermRelease(p.host, p.terminal_id)
  }, [active, p.host, p.terminal_id])

  // Release the server-side attach when the cell goes away entirely.
  React.useEffect(
    () => () => {
      void api.gridTermRelease(p.host, p.terminal_id)
    },
    [p.host, p.terminal_id]
  )

  // Wire xterm (shift+enter, image paste, …) once the iframe exists, and keep the
  // server-side attach alive while the cell is mounted.
  React.useEffect(() => {
    if (!src) return
    setReady(false)
    const cleanup = bootTermFrame(id, true)
    // Hold the loading overlay until xterm has painted real pane content, so the
    // ttyd connect/reconnect flash never shows through.
    const cancelReady = whenTerminalReady(id, () => setReady(true))
    // Touch-only keepalive: bumps the server idle timer but never (re)creates the
    // attach, so an in-flight keepalive landing after this cell releases can't
    // resurrect a thin attach that would clamp the pane in the wide Herdr terminal.
    const ka = setInterval(() => {
      void api.gridTermTouch(p.host, p.terminal_id).catch(() => {})
    }, KEEPALIVE_MS)
    return () => {
      cleanup()
      cancelReady()
      clearInterval(ka)
    }
  }, [src, id, p.host, p.terminal_id])

  const title = p.workspace_label || p.workspace_id || p.pane_id
  const tabLabel = p.tab_label && p.tab_label !== title ? p.tab_label : ""
  const tip = [
    p.host_label,
    p.workspace_label,
    p.tab_label,
    p.agent,
    tilde(p.cwd),
  ]
    .filter(Boolean)
    .join("\n")
  const closeLabel =
    selected && selectionCount > 1
      ? `Close ${selectionCount} panes`
      : p.has_agent
        ? "Close agent"
        : "Close pane"

  return (
    <div
      className={cn("termcell", focused && "focused", selected && "selected")}
    >
      <ContextMenu>
        <ContextMenuTrigger asChild>
          <button
            type="button"
            className="termcell-head"
            title={`${tip}\n\nclick to focus in Herdr · ⌘/Ctrl-click to select`}
            onClick={onClick}
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
        </ContextMenuTrigger>
        <ContextMenuContent>
          <ContextMenuItem onSelect={onRename}>
            Rename workspace…
          </ContextMenuItem>
          <ContextMenuItem variant="destructive" onSelect={onClose}>
            {closeLabel}
          </ContextMenuItem>
        </ContextMenuContent>
      </ContextMenu>
      <div ref={bodyRef} className="termcell-body">
        {/* Keep the iframe mounted (so ttyd connects + paints) but hidden under a
            loading overlay until xterm has rendered, masking ttyd's own connect
            churn. */}
        {src && (
          <iframe
            id={id}
            className={cn(GRID_FRAME_CLASS, !ready && "is-loading")}
            src={src}
            title={`${p.host_label} ${title}`}
          />
        )}
        {(failed || !ready) && (
          <div className="termcell-placeholder">
            {failed ? (
              "terminal unavailable"
            ) : (
              <span
                className="termcell-spinner"
                role="status"
                aria-label="loading"
              />
            )}
          </div>
        )}
      </div>
    </div>
  )
}
