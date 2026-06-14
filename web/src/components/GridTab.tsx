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
import { api, type GridPane } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { tilde } from "@/lib/format"
import { focusGridPane } from "@/lib/pane-focus"
import { qk } from "@/lib/query"
import { bootTermFrame, whenTerminalReady } from "@/lib/terminal"
import { GRID_FRAME_CLASS } from "@/lib/theme"
import { patchUIState, useUIState } from "@/lib/ui-state"
import { cn } from "@/lib/utils"

// How often the grid re-lists panes across hosts while the tab is open. The
// backend coalesces overlapping fetches behind a short cache.
const POLL_MS = 2500

// How often a mounted cell re-pings its terminal so the server keeps the ttyd
// alive (reaped after ~60s idle). Comfortably under that.
const KEEPALIVE_MS = 18_000

// Grid layout constants — must match .termgrid (gap + right padding) and
// .termcell (flex-basis).
const GRID_GAP = 0
const GRID_PAD_RIGHT = 16
const GRID_MIN_CELL = 360
// Floor for a cell's height. Rows grow to fill the grid's vertical space; once
// there are enough rows that they'd shrink past this, the grid scrolls instead.
const GRID_MIN_CELL_H = 260

// cellKey uniquely identifies a pane across hosts (tab ids are globally unique,
// but key by host too to match the server's grid_selected convention).
const cellKey = (p: GridPane) => `${p.host}|${p.tab_id}`

// The Grid tab: a wall of live terminals, one per tab, spanning every reachable
// host. Each cell body is an interactive terminal (ttyd attached to that tab's
// session). Click a header to focus the tab in the main viewport (switching host
// first when it lives elsewhere); ⌘/Ctrl-click to multi-select; right-click for
// rename / close. Filters (agents-only, per-host) persist server-side.
export function GridTab({
  active,
  selectTab,
}: {
  active: boolean
  selectTab: (tabId: string) => void
}) {
  const { activePaneID, host: activeHost } = useApp()
  const ui = useUIState()
  const agentsOnly = ui.grid_agents_only
  const hidden = React.useMemo(
    () => new Set(ui.grid_hidden_hosts),
    [ui.grid_hidden_hosts]
  )

  const gridRef = React.useRef<HTMLDivElement>(null)
  const [cols, setCols] = React.useState(1)
  const [gridH, setGridH] = React.useState(0)
  React.useLayoutEffect(() => {
    const el = gridRef.current
    if (!el) return
    const measure = () => {
      const content = el.clientWidth - GRID_PAD_RIGHT
      setCols(
        Math.max(
          1,
          Math.floor((content + GRID_GAP) / (GRID_MIN_CELL + GRID_GAP))
        )
      )
      setGridH(el.clientHeight)
    }
    measure()
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

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

  const all = data?.panes ?? null
  const hostErrors = data?.errors ?? null

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

  // Grow rows to fill the grid's vertical space: divide the measured height by
  // the row count, but never shrink a cell below GRID_MIN_CELL_H (past that the
  // grid scrolls). Falls back to the floor until the first measure lands.
  const rows = panes && panes.length ? Math.ceil(panes.length / cols) : 1
  const cellH =
    gridH > 0
      ? Math.max(GRID_MIN_CELL_H, Math.floor(gridH / rows))
      : GRID_MIN_CELL_H

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

  // Header click: plain click focuses the pane in the main viewport; modifier
  // click toggles selection instead.
  const onCellClick = (e: React.MouseEvent, p: GridPane) => {
    if (e.metaKey || e.ctrlKey || e.shiftKey) {
      toggleSelect(cellKey(p))
      return
    }
    clearSelection()
    void focusPane(p)
  }

  const focusPane = async (p: GridPane) => {
    try {
      await focusGridPane(p, activeHost, selectTab)
    } catch (e) {
      toast.error(`focus failed: ${(e as Error).message}`)
    }
  }

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
      await api.gridClose(
        targets.map((t) => ({ host: t.host, tab_id: t.tab_id }))
      )
      reload()
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

      <div
        ref={gridRef}
        className="termgrid"
        style={{ "--termcell-h": `${cellH}px` } as React.CSSProperties}
      >
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
                  focused={p.host === activeHost && p.tab_id === activePaneID}
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
        click a header to focus in the main terminal · ⌘/Ctrl-click to select ·
        right-click for rename / close
      </div>

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
                    .join(" · ") || closeTargets[0].tab_id
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

// frameId derives a stable, DOM-safe iframe id from the host + tab.
function frameId(host: string, tabID: string) {
  return `gridterm-${host.replace(/[^a-zA-Z0-9_-]/g, "_")}-${tabID}`
}

// GridCell renders one pane: a clickable header (right-click for actions) plus a
// lazily-mounted terminal. The iframe is created once the cell scrolls into view
// (so a long grid doesn't spawn dozens of ttyds at once) and only while the Grid
// tab is active.
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
  const id = frameId(p.host, p.tab_id)
  const bodyRef = React.useRef<HTMLDivElement>(null)
  const [src, setSrc] = React.useState<string | null>(null)
  const [failed, setFailed] = React.useState(false)
  const [ready, setReady] = React.useState(false)

  React.useEffect(() => {
    if (!active || src) return
    const el = bodyRef.current
    if (!el) return
    let cancelled = false
    const attach = async () => {
      try {
        const { base } = await api.gridTerm(p.host, p.tab_id)
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
  }, [active, src, p.host, p.tab_id])

  // When the Grid tab is hidden, detach this pane's terminal so its ttyd is
  // released; re-attaches when shown again.
  React.useEffect(() => {
    if (active) return
    setSrc(null)
    setFailed(false)
    setReady(false)
    void api.gridTermRelease(p.host, p.tab_id)
  }, [active, p.host, p.tab_id])

  React.useEffect(
    () => () => {
      void api.gridTermRelease(p.host, p.tab_id)
    },
    [p.host, p.tab_id]
  )

  React.useEffect(() => {
    if (!src) return
    setReady(false)
    const cleanup = bootTermFrame(id, true)
    const cancelReady = whenTerminalReady(id, () => setReady(true))
    const ka = setInterval(() => {
      void api.gridTermTouch(p.host, p.tab_id).catch(() => {})
    }, KEEPALIVE_MS)
    return () => {
      cleanup()
      cancelReady()
      clearInterval(ka)
    }
  }, [src, id, p.host, p.tab_id])

  const title = p.workspace_label || p.workspace_id || p.tab_id
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
            title={`${tip}\n\nclick to focus · ⌘/Ctrl-click to select`}
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
