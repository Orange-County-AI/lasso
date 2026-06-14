import { useQuery } from "@tanstack/react-query"
import { Maximize2, Minimize2, X } from "lucide-react"
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

// Grid layout constants — must match .termgrid. The wall spans the full width
// (no right strip: hold Shift to scroll the grid past a terminal). Every cell is
// the same width via 1fr grid columns; the column count is "as many
// GRID_MIN_CELL-wide cells as fit across the measured width".
const GRID_MIN_CELL = 360
// Floor for one row-unit (a shell tile's height; an agent tile is two units, so
// twice as tall). Tracks grow to fill the grid's vertical space; once there are
// enough row-units that they'd shrink past this, the grid scrolls instead.
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
  selectTabInGrid,
}: {
  active: boolean
  // Header-click focus: drop out of the grid onto the pane in the main viewport.
  selectTab: (tabId: string) => void
  // Select a tab WITHOUT leaving the grid — used to point the right sidebar at
  // the active cell (the Shell's cwd poller follows the selected tab).
  selectTabInGrid: (tabId: string) => void
}) {
  const { activePaneID, host: activeHost } = useApp()
  const ui = useUIState()
  const agentsOnly = ui.grid_agents_only
  const hidden = React.useMemo(
    () => new Set(ui.grid_hidden_hosts),
    [ui.grid_hidden_hosts]
  )

  const gridRef = React.useRef<HTMLDivElement>(null)
  const [gridW, setGridW] = React.useState(0)
  const [gridH, setGridH] = React.useState(0)
  React.useLayoutEffect(() => {
    const el = gridRef.current
    if (!el) return
    const measure = () => {
      setGridW(el.clientWidth)
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
  // Which pane (by tab id — globally unique) is expanded to fill the whole grid,
  // and which cell's terminal the user last focused (the ⌘⇧U expand target).
  // Keyed by tab id rather than host|tab so a freshly-created agent can be
  // expanded from its select-tab event (which carries only the tab id) before
  // the grid poll has even listed it.
  const [expandedTab, setExpandedTab] = React.useState<string | null>(null)
  const [focusedTab, setFocusedTab] = React.useState<string | null>(null)

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

  // Column count: as many min-width cells as fit across the measured width. The
  // grid's 1fr columns then make every cell the same width.
  const cols = gridW > 0 ? Math.max(1, Math.floor(gridW / GRID_MIN_CELL)) : 1

  // Agent panes are twice as tall as shell panes: the wall is a dense masonry of
  // single-column tiles — two row-units for an agent, one for a shell — packed so
  // a tall tile's neighbours fill in beside it. packGrid returns each pane's
  // explicit grid placement (the layout is fully deterministic — it doesn't lean
  // on browser auto-flow) plus the total row-unit count.
  const layout = React.useMemo(() => {
    const spans = (panes ?? []).map((p) => (p.has_agent ? 2 : 1))
    return packGrid(spans, cols)
  }, [panes, cols])

  // Grow the row tracks to fill the grid's vertical space: divide the measured
  // height by the row-unit count, but never shrink a unit below GRID_MIN_CELL_H
  // (past that the grid scrolls). Falls back to the floor until the first measure
  // lands. Spare vertical space becomes tile height; an agent tile, spanning two
  // tracks, stays exactly twice a shell tile.
  const cellH =
    gridH > 0
      ? Math.max(GRID_MIN_CELL_H, Math.floor(gridH / layout.rows))
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

  // Expand a cell to fill the whole grid (a header button or ⌘⇧U toggles it).
  const toggleExpand = (tabId: string) =>
    setExpandedTab((cur) => (cur === tabId ? null : tabId))

  // The cell the main viewport is pointed at — the visually-focused (.focused)
  // cell — used as the ⌘⇧U target when the user hasn't clicked into a grid
  // terminal yet.
  const activeMatchTab = React.useMemo(
    () =>
      (panes ?? []).find(
        (p) => p.host === activeHost && p.tab_id === activePaneID
      )?.tab_id ?? null,
    [panes, activeHost, activePaneID]
  )

  // ⌘⇧U in grid mode toggles expand/restore: collapse if a cell is expanded,
  // else expand the focused cell (the terminal the user last clicked into, or
  // the .focused cell). App routes the key here as this event while in grid.
  React.useEffect(() => {
    if (!active) return
    const onToggle = () => {
      // Target the focused cell (the terminal the user last clicked into), else
      // the .focused cell the main viewport points at, else the first pane — so
      // the key always does something even before any cell has been focused.
      const target = focusedTab ?? activeMatchTab ?? panes?.[0]?.tab_id ?? null
      setExpandedTab((cur) => (cur ? null : target))
    }
    window.addEventListener("lasso:grid-expand-toggle", onToggle)
    return () =>
      window.removeEventListener("lasso:grid-expand-toggle", onToggle)
  }, [active, focusedTab, activeMatchTab, panes])

  // A new agent created while the grid is up opens fullscreen in the grid (App
  // forwards its tab id here instead of leaving the grid). Reload so the just
  // -created pane lands in the wall as promptly as the backend can list it.
  React.useEffect(() => {
    if (!active) return
    const onExpandTab = (e: Event) => {
      const tabId = (e as CustomEvent).detail as string
      if (!tabId) return
      setExpandedTab(tabId)
      reload()
    }
    window.addEventListener("lasso:grid-expand-tab", onExpandTab)
    return () =>
      window.removeEventListener("lasso:grid-expand-tab", onExpandTab)
  }, [active, reload])

  // Track which cell's terminal the user is in, so the sidebar (with "follow
  // active pane" on) points at it. Each grid iframe reports its own
  // pointerdown/focus up to us via this event (see wireGridFocus): a click
  // *anywhere* in a terminal — including straight from one terminal to another —
  // updates the focused cell, which a window-level blur can't catch (it only sees
  // the first hop from the page into a terminal, never iframe→iframe).
  React.useEffect(() => {
    if (!active) return
    const onCellFocus = (e: Event) => {
      const id = (e as CustomEvent).detail as string
      const hit = (panes ?? []).find((p) => frameId(p.host, p.tab_id) === id)
      if (hit) setFocusedTab(hit.tab_id)
    }
    window.addEventListener("lasso:grid-cell-focus", onCellFocus)
    return () =>
      window.removeEventListener("lasso:grid-cell-focus", onCellFocus)
  }, [active, panes])

  // Tab ids we've actually seen in the wall, so the cleanup below can tell a
  // pane that's *gone* (was listed, now closed → drop the expand) from one that
  // merely *hasn't arrived yet* (a freshly-created agent the poll hasn't listed
  // → keep it expanded so it lands fullscreen once it appears).
  const seenTabs = React.useRef<Set<string>>(new Set())
  React.useEffect(() => {
    if (!panes) return
    const live = new Set(panes.map((p) => p.tab_id))
    for (const id of live) seenTabs.current.add(id)
    if (
      expandedTab &&
      seenTabs.current.has(expandedTab) &&
      !live.has(expandedTab)
    )
      setExpandedTab(null)
    if (focusedTab && !live.has(focusedTab)) setFocusedTab(null)
  }, [panes, expandedTab, focusedTab])

  const expandedPane = expandedTab
    ? (panes?.find((p) => p.tab_id === expandedTab) ?? null)
    : null
  const expanding = expandedTab != null && expandedPane == null

  const focusedPane = focusedTab
    ? (panes?.find((p) => p.tab_id === focusedTab) ?? null)
    : null

  // Point the right sidebar (Files/Diff) at the active cell — the one the user
  // fullscreened or last clicked into. Switch the active host to it so /api/file
  // & /api/diff read the right filesystem, and select its tab (without leaving
  // the grid) so the Shell's cwd poller tracks its live directory. Without this
  // the sidebar keeps following whatever tab was selected before the grid opened
  // — often a tab on another host, whose path then 404s on the active host.
  const activeCell = expandedPane ?? focusedPane
  const activeCellTab = activeCell?.tab_id ?? null
  const activeCellHost = activeCell?.host ?? null
  const switchedHost = React.useRef<string | null>(null)
  React.useEffect(() => {
    if (!active || !activeCellTab) return
    if (
      activeCellHost &&
      activeCellHost !== (activeHost ?? "local") &&
      switchedHost.current !== activeCellHost
    ) {
      switchedHost.current = activeCellHost
      void api.switchHost(activeCellHost)
    }
    selectTabInGrid(activeCellTab)
  }, [active, activeCellTab, activeCellHost, activeHost, selectTabInGrid])

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
        className={cn("termgrid", expandedTab && "termgrid-expanded")}
        style={
          {
            "--termcell-h": `${cellH}px`,
            "--grid-cols": cols,
          } as React.CSSProperties
        }
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
          <>
            {panes.map((p, i) => (
              <GridCell
                key={cellKey(p)}
                pane={p}
                active={active}
                selected={selected.has(cellKey(p))}
                selectionCount={selected.size}
                focused={p.host === activeHost && p.tab_id === activePaneID}
                expanded={p.tab_id === expandedTab}
                placement={expandedTab ? null : layout.cells[i]}
                onClick={(e) => onCellClick(e, p)}
                onToggleExpand={() => toggleExpand(p.tab_id)}
                onRename={() => requestRename(p)}
                onClose={() => requestClose(p)}
              />
            ))}
            {/* The expand target isn't in the wall yet (a just-created agent the
                poll hasn't listed) — hold the fullscreen space rather than
                flashing the whole grid blank. */}
            {expanding && (
              <div className="termcell termcell-expanded">
                <div className="termcell-placeholder">opening terminal…</div>
              </div>
            )}
          </>
        )}
      </div>

      <div className="hint">
        {expandedTab
          ? "⤢ or ⌘⇧U restores the cell to the grid · click a header to focus in the main terminal"
          : "click a header to focus in the main terminal · ⌘/Ctrl-click to select · ⤢ expands a cell (⌘⇧U)"}
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

// One pane's explicit grid position: a single column, starting at `row`, spanning
// `span` row-units (1 for a shell, 2 for an agent). Columns/rows are 0-based here;
// the style maps them to 1-based grid lines.
type GridCellPlacement = { col: number; row: number; span: number }

// packGrid lays the panes out as a dense masonry of single-column tiles —
// `spans[i]` row-units tall (2 for an agent, 1 for a shell) — by placing each at
// the first free slot scanning row-major, so a tall tile's gaps fill with later
// tiles. It then grows any shell tile sitting directly above a blank bottom slot
// to cover it, so a short last row doesn't leave a blank rectangle. Returns each
// pane's explicit placement (so the layout is deterministic and never depends on
// the browser's auto-placement) plus the total row-unit count for track sizing.
function packGrid(
  spans: number[],
  cols: number
): { cells: GridCellPlacement[]; rows: number } {
  const occ: (number | undefined)[][] = [] // occ[row][col] = pane index, or undefined
  const ensure = (r: number) => {
    while (occ.length <= r) occ.push(new Array(cols).fill(undefined))
  }
  const cells: GridCellPlacement[] = []
  for (let i = 0; i < spans.length; i++) {
    const s = spans[i]
    let done = false
    for (let r = 0; !done; r++) {
      ensure(r + s - 1)
      for (let c = 0; c < cols; c++) {
        let free = true
        for (let k = 0; k < s; k++)
          if (occ[r + k][c] !== undefined) {
            free = false
            break
          }
        if (!free) continue
        for (let k = 0; k < s; k++) occ[r + k][c] = i
        cells[i] = { col: c, row: r, span: s }
        done = true
        break
      }
    }
  }
  const rows = Math.max(1, occ.length)
  // Grow a shell tile down into a blank slot directly beneath it (only when the
  // tile starts in the row above, so it can't stretch past two units).
  for (let c = 0; c < cols; c++)
    for (let r = 1; r < rows; r++) {
      const above = occ[r - 1][c]
      if (occ[r][c] === undefined && above !== undefined) {
        const cell = cells[above]
        if (cell.span === 1 && cell.row === r - 1) {
          cell.span = 2
          occ[r][c] = above
        }
      }
    }
  return { cells, rows }
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
  expanded,
  placement,
  onClick,
  onToggleExpand,
  onRename,
  onClose,
}: {
  pane: GridPane
  active: boolean
  selected: boolean
  selectionCount: number
  focused: boolean
  expanded: boolean
  // Explicit grid position (column + row span) from the masonry packer, or null
  // when a cell is expanded to fill the grid (CSS owns the layout then).
  placement: GridCellPlacement | null
  onClick: (e: React.MouseEvent) => void
  onToggleExpand: () => void
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
    const cleanup = bootTermFrame(id, true, true)
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
      className={cn(
        "termcell",
        focused && "focused",
        selected && "selected",
        expanded && "termcell-expanded"
      )}
      style={
        placement
          ? {
              gridColumn: placement.col + 1,
              gridRow: `${placement.row + 1} / span ${placement.span}`,
            }
          : undefined
      }
    >
      <div className="termcell-head">
        <ContextMenu>
          <ContextMenuTrigger asChild>
            <button
              type="button"
              className="termcell-head-main"
              title={`${tip}\n\nclick to focus · ⌘/Ctrl-click to select`}
              onClick={onClick}
            >
              {p.git && (
                <span
                  className={cn("termcell-git", p.dirty ? "dirty" : "clean")}
                  title={
                    p.dirty
                      ? `${p.dirty} uncommitted change${
                          p.dirty === 1 ? "" : "s"
                        }`
                      : "clean working tree"
                  }
                >
                  ● {p.dirty || ""}
                </span>
              )}
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
        <button
          type="button"
          className="termcell-btn"
          title={
            expanded ? "Restore to grid (⌘⇧U)" : "Expand to fill grid (⌘⇧U)"
          }
          aria-label={expanded ? "restore to grid" : "expand to fill grid"}
          onClick={onToggleExpand}
        >
          {expanded ? (
            <Minimize2 className="size-3.5" />
          ) : (
            <Maximize2 className="size-3.5" />
          )}
        </button>
        <button
          type="button"
          className="termcell-btn termcell-btn-close"
          title={closeLabel}
          aria-label={closeLabel}
          onClick={onClose}
        >
          <X className="size-3.5" />
        </button>
      </div>
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
