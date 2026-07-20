import { useQuery } from "@tanstack/react-query"
import * as React from "react"
import { toast } from "sonner"
import { GridRail } from "@/components/GridRail"
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
import { lsGet, lsSet, useApp } from "@/lib/app-store"
import { tilde } from "@/lib/format"
import { loadSeen, markSeen, reconcileSeen } from "@/lib/grid-seen"
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

// Grid layout constants — gap/padding must match .termgrid in index.css. The
// column count and row height are computed from the viewport (tall-first: as
// many columns as fit at GRID_MIN_CELL_W, cells stretched to fill the height,
// never shorter than GRID_MIN_CELL_H) and applied via CSS vars.
const GRID_GAP = 14
const GRID_PAD = 14
const GRID_MIN_CELL_W = 360
const GRID_MIN_CELL_H = 260

// cellKey uniquely identifies a pane across hosts (pane ids are only unique
// within a host).
const cellKey = (p: GridPane) => `${p.host}|${p.pane_id}`

// Whether the pane rail is open — device-local (like the sidebar width), so a
// small laptop can keep it collapsed while a big monitor leaves it open.
const RAIL_OPEN_KEY = "lasso-grid-rail-open"

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
  const mode = ui.grid_mode
  const hidden = React.useMemo(
    () => new Set(ui.grid_hidden_hosts),
    [ui.grid_hidden_hosts]
  )
  // Starred panes (host|pane_id keys) — the only panes shown in Watch mode.
  const watched = React.useMemo(
    () => new Set(ui.grid_watched),
    [ui.grid_watched]
  )

  // Pane rail (the picker): default collapsed so the grid keeps the full
  // width; open state persists per device.
  const [railOpen, setRailOpen] = React.useState(
    () => lsGet(RAIL_OPEN_KEY) === "1"
  )
  const toggleRail = (next?: boolean) => {
    setRailOpen((cur) => {
      const v = next ?? !cur
      lsSet(RAIL_OPEN_KEY, v ? "1" : "0")
      return v
    })
  }
  // Keys highlighted as "new" in the rail (snapshotted when the +N badge opens
  // it, cleared when it closes — see the seen-tracking below).
  const [railHighlight, setRailHighlight] = React.useState<Set<string>>(
    () => new Set()
  )
  React.useEffect(() => {
    if (!railOpen) setRailHighlight((cur) => (cur.size ? new Set() : cur))
  }, [railOpen])

  // Which panes this device has already seen (null until first reconcile on a
  // fresh device, so the "+N new" badge can't flash before seeding). Mirrors
  // the localStorage set (grid-seen.ts) into state so the badge is reactive.
  const [seen, setSeen] = React.useState<Set<string> | null>(() => loadSeen())
  const mark = React.useCallback((keys: Iterable<string>) => {
    markSeen(keys)
    setSeen((cur) => {
      if (!cur) return cur
      let next: Set<string> | null = null
      for (const k of keys) {
        if (!cur.has(k)) {
          next ??= new Set(cur)
          next.add(k)
        }
      }
      return next ?? cur
    })
  }, [])

  // Measure the grid viewport; the tall-first column/row math derives from it
  // in render (clientHeight of the scroll container is the viewport height, not
  // the content height, which is exactly what the row math wants).
  const gridRef = React.useRef<HTMLDivElement>(null)
  const [box, setBox] = React.useState({ w: 0, h: 0 })
  React.useLayoutEffect(() => {
    const el = gridRef.current
    if (!el) return
    const measure = () => setBox({ w: el.clientWidth, h: el.clientHeight })
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

  // Reconcile the seen set against each payload (seeds on first ever load,
  // prunes keys for dead panes so the set stays bounded).
  React.useEffect(() => {
    if (!all) return
    const next = reconcileSeen(new Set(all.map(cellKey)))
    setSeen((cur) => {
      if (
        cur &&
        cur.size === next.size &&
        Array.from(next).every((k) => cur.has(k))
      )
        return cur
      return next
    })
  }, [all])

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
        ? all.filter(
            (p) =>
              (!agentsOnly || p.has_agent) &&
              !hidden.has(p.host) &&
              (mode !== "watch" || watched.has(cellKey(p)))
          )
        : null,
    [all, agentsOnly, hidden, mode, watched]
  )

  // A pane counts as seen once it's been on screen: rendered in All mode,
  // listed while the rail is open, or deliberately starred.
  React.useEffect(() => {
    if (active && mode === "all" && panes) mark(panes.map(cellKey))
  }, [active, mode, panes, mark])
  React.useEffect(() => {
    if (railOpen && all) mark(all.map(cellKey))
  }, [railOpen, all, mark])
  React.useEffect(() => {
    if (watched.size) mark(watched)
  }, [watched, mark])

  // The unseen, unstarred panes backing the Watch-mode "+N new" badge.
  const newKeys = React.useMemo(() => {
    const s = new Set<string>()
    if (mode !== "watch" || !all || !seen) return s
    for (const p of all) {
      const k = cellKey(p)
      if (!seen.has(k) && !watched.has(k)) s.add(k)
    }
    return s
  }, [mode, all, seen, watched])

  // Tall-first layout: as many columns as fit at the min cell width (so a
  // handful of panes becomes one row of tall columns), rows stretched to fill
  // the viewport height down to a floor — past which the grid scrolls like the
  // old fixed-height wall.
  const n = panes?.length ?? 0
  const availW = box.w - GRID_PAD * 2
  const availH = box.h - GRID_PAD * 2
  const maxCols = Math.max(
    1,
    Math.floor((availW + GRID_GAP) / (GRID_MIN_CELL_W + GRID_GAP))
  )
  const cols = Math.max(1, Math.min(n || 1, maxCols))
  const rows = Math.ceil(Math.max(n, 1) / cols)
  const cellH = Math.max(
    GRID_MIN_CELL_H,
    Math.floor((availH - (rows - 1) * GRID_GAP) / rows)
  )

  const toggleAgentsOnly = () => patchUIState({ grid_agents_only: !agentsOnly })

  const setMode = (m: "all" | "watch") => patchUIState({ grid_mode: m })

  const toggleWatch = (key: string) => {
    const next = new Set(watched)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    patchUIState({ grid_watched: Array.from(next) })
  }

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
  // toggles selection instead. (Also fired for keyboard Enter/Space — only the
  // modifier keys are read off the event.)
  const onCellClick = (
    e: React.MouseEvent | React.KeyboardEvent,
    p: GridPane
  ) => {
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
        <button
          type="button"
          onClick={() => toggleRail()}
          aria-pressed={railOpen}
          className={cn(
            "rounded-full border px-2 py-0.5 text-[11px] transition-colors",
            railOpen
              ? "border-primary/40 bg-accent text-foreground"
              : "border-border text-muted-foreground hover:text-foreground"
          )}
          title={railOpen ? "Hide the pane list" : "Show the pane list"}
        >
          Panes
        </button>

        {/* All / Watch segmented toggle: All is the classic every-pane wall,
            Watch shows only starred panes (persisted server-side). */}
        <div className="flex overflow-hidden rounded-full border border-border text-[11px]">
          {(["all", "watch"] as const).map((m) => (
            <button
              key={m}
              type="button"
              onClick={() => setMode(m)}
              aria-pressed={mode === m}
              className={cn(
                "px-2 py-0.5 transition-colors",
                mode === m
                  ? "bg-accent text-foreground"
                  : "text-muted-foreground hover:text-foreground"
              )}
              title={
                m === "all" ? "Show every pane" : "Show only watched panes"
              }
            >
              {m === "all" ? "All" : "Watch"}
            </button>
          ))}
        </div>

        {/* Panes that appeared since this device last looked — Watch mode only.
            Clicking opens the rail with the new rows highlighted (a snapshot,
            since opening the rail immediately marks everything seen). */}
        {newKeys.size > 0 && (
          <button
            type="button"
            onClick={() => {
              setRailHighlight(new Set(newKeys))
              toggleRail(true)
            }}
            className="rounded-full border border-primary/40 bg-accent px-2 py-0.5 text-[11px] text-foreground transition-colors hover:border-primary"
            title="Panes that appeared since you last looked — click to review"
          >
            +{newKeys.size} new
          </button>
        )}

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
              ? mode === "watch"
                ? `${panes.length} watched`
                : `${panes.length} pane${panes.length === 1 ? "" : "s"}${
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

      <div className="flex min-h-0 flex-1">
        <GridRail
          open={railOpen}
          panes={all}
          watched={watched}
          newKeys={railHighlight}
          onToggleWatch={toggleWatch}
          onFocusPane={(p) => void focusPane(p)}
        />
        <div
          ref={gridRef}
          className="termgrid"
          style={
            {
              "--grid-cols": cols,
              "--grid-cell-h": `${cellH}px`,
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
              {mode === "watch" ? (
                <>
                  {watched.size === 0
                    ? "no watched panes yet — star ☆ panes to build your watch list"
                    : "no watched panes are running (or they're hidden by filters)"}
                  <br />
                  <button
                    type="button"
                    onClick={() => toggleRail(true)}
                    className="mt-2 rounded border border-border px-2 py-0.5 text-[11px] text-muted-foreground hover:text-foreground"
                  >
                    browse panes
                  </button>
                </>
              ) : agentsOnly || hidden.size ? (
                "no panes match the filters"
              ) : (
                "no panes"
              )}
            </div>
          ) : (
            panes.map((p) => (
              <GridCell
                key={cellKey(p)}
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
                watched={watched.has(cellKey(p))}
                onToggleWatch={() => toggleWatch(cellKey(p))}
                onClick={(e) => onCellClick(e, p)}
                onRename={() => requestRename(p)}
                onClose={() => requestClose(p)}
              />
            ))
          )}
        </div>
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
  watched,
  onToggleWatch,
  onClick,
  onRename,
  onClose,
}: {
  pane: GridPane
  active: boolean
  selected: boolean
  selectionCount: number
  focused: boolean
  watched: boolean
  onToggleWatch: () => void
  onClick: (e: React.MouseEvent | React.KeyboardEvent) => void
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
          {/* biome-ignore lint/a11y/useSemanticElements: the star inside is a
              real <button>, and buttons can't nest. */}
          <div
            role="button"
            tabIndex={0}
            className="termcell-head"
            title={`${tip}\n\nclick to focus in Herdr · ⌘/Ctrl-click to select`}
            onClick={onClick}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault()
                onClick(e)
              }
            }}
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
            <button
              type="button"
              className={cn("termcell-star", watched && "watched")}
              aria-pressed={watched}
              title={watched ? "Stop watching" : "Watch this pane"}
              onClick={(e) => {
                e.stopPropagation()
                onToggleWatch()
              }}
            >
              {watched ? "★" : "☆"}
            </button>
          </div>
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
