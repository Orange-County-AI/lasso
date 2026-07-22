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
import { api, type GridPane, type UIState } from "@/lib/api"
import { lsGet, lsSet, useApp } from "@/lib/app-store"
import { tilde } from "@/lib/format"
import { loadSeen, markSeen, reconcileSeen } from "@/lib/grid-seen"
import { focusPaneInHerdr, focusPaneInPlace } from "@/lib/pane-focus"
import { qk } from "@/lib/query"
import {
  bootTermFrame,
  focusTerminalFrame,
  onTerminalFocus,
  whenTerminalReady,
} from "@/lib/terminal"
import { GRID_FRAME_CLASS } from "@/lib/theme"
import { patchUIState, uiStateNow, useUIState } from "@/lib/ui-state"
import { cn } from "@/lib/utils"

// How often the grid re-lists panes across hosts while the tab is open. The
// backend coalesces overlapping fetches behind a short cache, so this only needs
// to be brisk enough to feel live.
const POLL_MS = 2500

// How often a mounted cell re-pings its terminal endpoint so the server keeps the
// ttyd alive. The server reaps idle attaches after gridTermIdle (120s) — kept well
// above this interval so a backgrounded tab (whose timers the browser throttles)
// or a host-switch request storm can miss a few keepalives without the cell being
// reaped out from under the still-visible grid.
const KEEPALIVE_MS = 18_000

// How long a cell waits before retrying a failed attach. A gridTerm POST can
// fail transiently (backend momentarily stalled, an SSH mux mid-redial after a
// network blip) — without a retry the cell would show "terminal unavailable"
// forever, since no other timer runs until an iframe exists.
const ATTACH_RETRY_MS = 5_000

// Stepping between panes (Single mode) or toggling one off and on (Multi)
// unmounts and remounts cells in quick succession. Releasing the server-side
// attach the instant a cell unmounts made every step back a cold start (fresh
// ttyd + herdr attach + xterm boot), so unmount releases are deferred by this
// grace: re-mounting within it finds the attach still live (ensureGridTerm
// returns the existing entry) and the terminal reconnects near-instantly. The
// deferred release is token-scoped, so if a newer attach claimed the pane in
// the meantime it's a no-op. Leaving the Grid view still tears everything down
// immediately (gridTermReleaseAll), so no thin attach lingers to clamp a pane
// being viewed full-size in Herdr.
const RELEASE_GRACE_MS = 30_000
const pendingReleases = new Map<string, ReturnType<typeof setTimeout>>()
function scheduleGridTermRelease(
  host: string,
  terminalId: string,
  token: string
) {
  const key = `${host}|${terminalId}`
  const prior = pendingReleases.get(key)
  if (prior) clearTimeout(prior)
  pendingReleases.set(
    key,
    setTimeout(() => {
      pendingReleases.delete(key)
      void api.gridTermRelease(host, terminalId, token)
    }, RELEASE_GRACE_MS)
  )
}
function cancelGridTermRelease(host: string, terminalId: string) {
  const key = `${host}|${terminalId}`
  const t = pendingReleases.get(key)
  if (t) {
    clearTimeout(t)
    pendingReleases.delete(key)
  }
}

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

// A request (from App) to focus a pane in the grid once it shows up in a grid
// payload — e.g. an agent just created from the New Agent dialog while the
// Grid view was active. ts makes each request distinct so the same pane can be
// requested twice.
export interface GridFocusRequest {
  host: string
  paneId?: string
  workspaceId?: string
  ts: number
}

// The Grid tab: a wall of live terminals, one per herdr pane, spanning every
// reachable + protocol-compatible host. Each cell body is an interactive
// terminal (a ttyd attached to that pane). Multi mode shows the panes toggled
// on in the rail; Single shows one at a time. Click a header to focus the pane
// in the Herdr tab (switching host first when it lives elsewhere);
// ⌘/Ctrl-click to multi-select; right-click for rename / close. The mode and
// pane picks are persisted server-side so the view is the same every visit.
export function GridTab({
  active,
  onFocusInHerdr,
  focusRequest,
}: {
  active: boolean
  onFocusInHerdr: () => void
  focusRequest?: GridFocusRequest | null
}) {
  const { activePaneID, host: activeHost } = useApp()
  const ui = useUIState()
  const mode = ui.grid_mode
  // Toggled-on panes (host|pane_id keys) — the panes shown in Multi mode.
  // (Stored under the historical grid_watched name.)
  const watched = React.useMemo(
    () => new Set(ui.grid_watched),
    [ui.grid_watched]
  )
  // Select mode: one pane at a time. selectPane is the persisted choice.
  const selectPane = ui.grid_select_pane

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

  // Drop a pane from the grid the moment its terminal is replaced. herdr gives a
  // pane a NEW terminal_id when the process running in it exits and herdr
  // respawns a bare shell (pane_history / remain-on-exit) — e.g. an agent that
  // finished or crashed WITHOUT calling `lasso closeme` (which would pane.close
  // the pane, dropping it from the grid outright). A mounted cell stays pinned to
  // the now-dead terminal (it never re-attaches on a terminal_id change), so it
  // would otherwise sit forever on ttyd's "Press ⏎ to Reconnect". Treat the swap
  // as the pane being closed and remove it: un-watch it (Multi), un-select it,
  // and unpin it from Select mode. Non-destructive — the pane still lives in
  // herdr and can be re-added from the rail.
  const lastTermRef = React.useRef<Map<string, string>>(new Map())
  React.useEffect(() => {
    if (!all) return
    const map = lastTermRef.current
    const live = new Set<string>()
    const retired = new Set<string>()
    for (const p of all) {
      const k = cellKey(p)
      live.add(k)
      const prev = map.get(k)
      // Seed on first sight (a pane already on its current terminal is a live
      // shell, not a swap) — only a change from a known terminal retires.
      if (prev !== undefined && prev !== p.terminal_id) retired.add(k)
      map.set(k, p.terminal_id)
    }
    // Prune vanished panes so the map stays bounded and a pane that reappears
    // later seeds fresh instead of falsely retiring.
    for (const k of map.keys()) if (!live.has(k)) map.delete(k)
    if (retired.size === 0) return
    const ui = uiStateNow()
    const patch: Partial<UIState> = {}
    const nextWatched = ui.grid_watched.filter((k) => !retired.has(k))
    if (nextWatched.length !== ui.grid_watched.length)
      patch.grid_watched = nextWatched
    const nextSelected = ui.grid_selected.filter((k) => !retired.has(k))
    if (nextSelected.length !== ui.grid_selected.length)
      patch.grid_selected = nextSelected
    if (retired.has(ui.grid_select_pane)) patch.grid_select_pane = ""
    if (Object.keys(patch).length) patchUIState(patch)
  }, [all])

  // Select mode's cycling list: agent panes when any exist, else every pane.
  // An explicitly picked pane (from the rail) still shows even when it falls
  // outside the list.
  const selectCandidates = React.useMemo(() => {
    if (!all) return null
    const agents = all.filter((p) => p.has_agent)
    return agents.length ? agents : all
  }, [all])
  const selectShownKey = React.useMemo(() => {
    if (!all) return null
    if (selectPane && all.some((p) => cellKey(p) === selectPane))
      return selectPane
    const first = selectCandidates?.[0]
    return first ? cellKey(first) : null
  }, [all, selectCandidates, selectPane])

  // Multi mode shows exactly the toggled-on panes; Single mode shows exactly
  // one.
  const panes = React.useMemo(
    () =>
      all
        ? all.filter((p) =>
            mode === "select"
              ? cellKey(p) === selectShownKey
              : watched.has(cellKey(p))
          )
        : null,
    [all, mode, watched, selectShownKey]
  )

  // A pane counts as seen once it's been on screen: rendered in Single mode,
  // listed while the rail is open, or deliberately toggled on.
  React.useEffect(() => {
    if (active && mode !== "watch" && panes) mark(panes.map(cellKey))
  }, [active, mode, panes, mark])
  React.useEffect(() => {
    if (railOpen && all) mark(all.map(cellKey))
  }, [railOpen, all, mark])
  React.useEffect(() => {
    if (watched.size) mark(watched)
  }, [watched, mark])

  // The unseen, untoggled panes backing the Multi-mode "+N new" badge.
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

  const setMode = (m: "watch" | "select") => patchUIState({ grid_mode: m })

  // Step Select mode's shown pane through the cycling list (wraps). When the
  // shown pane sits outside the list (a non-agent pane picked from the rail),
  // stepping re-enters the list at its start. The displayed pane switches
  // immediately; the herdr focus (which the sidebar file viewer follows) trails
  // asynchronously so stepping never blocks on a cross-host switch.
  const stepSelect = (delta: number) => {
    const cands = selectCandidates
    if (!cands || cands.length === 0) return
    const idx = cands.findIndex((p) => cellKey(p) === selectShownKey)
    const next =
      cands[idx < 0 ? 0 : (idx + delta + cands.length) % cands.length]
    patchUIState({ grid_select_pane: cellKey(next) })
    focusInGridBackend(next)
  }

  const toggleWatch = (key: string) => {
    const next = new Set(watched)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    patchUIState({ grid_watched: Array.from(next) })
  }

  const clearSelection = () => patchUIState({ grid_selected: [] })
  const toggleSelect = (key: string) => {
    const next = new Set(selected)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    patchUIState({ grid_selected: Array.from(next) })
  }

  // Focusing a pane can take a few seconds when it switches the active host
  // across the network (SSH connect + terminal respawn). Track the cell whose
  // focus is in flight so it can show a spinner — the click reads as accepted
  // even before the switch lands, which also curbs the impatient re-clicks that
  // pile up host switches. A safety timeout clears it so a hung switch can't
  // strand the spinner forever.
  const [focusPending, setFocusPending] = React.useState<string | null>(null)
  const focusTimer = React.useRef<ReturnType<typeof setTimeout> | null>(null)
  const trackFocus = React.useCallback(
    (key: string, work: Promise<unknown>) => {
      setFocusPending(key)
      if (focusTimer.current) clearTimeout(focusTimer.current)
      focusTimer.current = setTimeout(
        () => setFocusPending((cur) => (cur === key ? null : cur)),
        20_000
      )
      void work.finally(() => {
        setFocusPending((cur) => (cur === key ? null : cur))
        if (focusTimer.current) clearTimeout(focusTimer.current)
      })
    },
    []
  )
  React.useEffect(
    () => () => {
      if (focusTimer.current) clearTimeout(focusTimer.current)
    },
    []
  )

  // In-grid focus: make the pane herdr's focused pane WITHOUT leaving the grid
  // (no history push, no grid-terminal release, no view switch). The cell's
  // border highlight and the sidebar file viewer both key off the SSE focus
  // state (activePaneID / activeCwd / host), so they follow automatically —
  // including across hosts.
  const focusInGridBackend = React.useCallback(
    (p: GridPane) => {
      if (p.host === activeHost && p.pane_id === activePaneID) return
      trackFocus(
        cellKey(p),
        focusPaneInPlace(p, activeHost).catch((e) =>
          toast.error(`focus failed: ${(e as Error).message}`)
        )
      )
    },
    [activeHost, activePaneID, trackFocus]
  )
  const focusInGrid = (p: GridPane) => {
    const key = cellKey(p)
    if (mode === "select") {
      // In Select mode, picking a pane (from the rail) makes it THE pane.
      if (key !== selectShownKey) patchUIState({ grid_select_pane: key })
    } else if (panes && !panes.some((x) => cellKey(x) === key)) {
      // Not part of the current view (toggled off in Multi mode).
      toast(`${p.host_label} pane isn't shown — toggle it on in the pane list`)
      return
    }
    focusInGridBackend(p)
    const fid = frameId(p.host, p.terminal_id)
    document
      .getElementById(`cell-${fid}`)
      ?.scrollIntoView({ block: "nearest", behavior: "smooth" })
    focusTerminalFrame(fid)
  }

  // Honor a focus request from App (an agent created from the New Agent dialog
  // while the Grid view was active): once the new pane shows up in a payload,
  // toggle it on if we're in Multi mode (creating it was an explicit ask to
  // see it), highlight it, and hand it the keyboard. Retries until a poll includes the
  // pane; a fresh request forces one refetch so it doesn't wait a full 2.5s.
  const handledFocusReq = React.useRef(0)
  React.useEffect(() => {
    if (focusRequest && focusRequest.ts !== handledFocusReq.current)
      void reload()
    // reload is stable (React Query refetch); keyed on the request itself.
  }, [focusRequest, reload])
  React.useEffect(() => {
    const req = focusRequest
    if (!req || req.ts === handledFocusReq.current || !all) return
    const p = all.find(
      (x) =>
        x.host === req.host &&
        ((req.paneId && x.pane_id === req.paneId) ||
          (req.workspaceId && x.workspace_id === req.workspaceId))
    )
    if (!p) return // not listed yet — the next poll retries
    handledFocusReq.current = req.ts
    const key = cellKey(p)
    if (mode === "watch" && !watched.has(key))
      patchUIState({ grid_watched: [...watched, key] })
    if (mode === "select") patchUIState({ grid_select_pane: key })
    focusInGridBackend(p)
    // The cell may still be mounting (or just became visible via the star), and
    // its ttyd takes a moment to attach — retry the scroll + keyboard handoff.
    const fid = frameId(p.host, p.terminal_id)
    for (const delay of [150, 600, 1500]) {
      setTimeout(() => {
        document
          .getElementById(`cell-${fid}`)
          ?.scrollIntoView({ block: "nearest", behavior: "smooth" })
        focusTerminalFrame(fid)
      }, delay)
    }
  }, [focusRequest, all, mode, watched, focusInGridBackend])

  // Header click: plain click opens the pane in Herdr (clicking into the cell
  // BODY is the in-grid interaction — the terminal takes the keyboard right
  // there); ⌘/Ctrl/Shift-click toggles selection instead. (Also fired for
  // keyboard Enter/Space — only the modifier keys are read off the event.)
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
  // with the Cmd+U pane switcher). Tracked so the cell shows a spinner while the
  // (possibly cross-host, multi-second) switch + focus is in flight, before the
  // view surfaces Herdr.
  const focusPane = (p: GridPane) => {
    trackFocus(
      cellKey(p),
      focusPaneInHerdr(p, activeHost, onFocusInHerdr).catch((e) => {
        toast.error(`focus failed: ${(e as Error).message}`)
      })
    )
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

        {/* Multi / Single segmented toggle: Multi shows the panes toggled on
            in the rail, Single shows one pane at a time (persisted
            server-side — the stored mode values keep their original
            "watch"/"select" names). */}
        <div className="flex overflow-hidden rounded-full border border-border text-[11px]">
          {(["watch", "select"] as const).map((m) => (
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
                m === "watch"
                  ? "Show the panes you've toggled on"
                  : "Show one pane at a time"
              }
            >
              {m === "watch" ? "Multi" : "Single"}
            </button>
          ))}
        </div>

        {/* Select mode: step through the cycling list (agent panes by default). */}
        {mode === "select" && (
          <div className="flex items-center gap-1 text-[11px] text-muted-foreground">
            <button
              type="button"
              onClick={() => stepSelect(-1)}
              className="rounded-full border border-border px-2 py-0.5 transition-colors hover:text-foreground"
              title="Previous pane"
            >
              ‹
            </button>
            <span className="tabular-nums">
              {(() => {
                const n = selectCandidates?.length ?? 0
                if (!n) return "0"
                const idx =
                  selectCandidates?.findIndex(
                    (p) => cellKey(p) === selectShownKey
                  ) ?? -1
                return idx >= 0 ? `${idx + 1} / ${n}` : `· / ${n}`
              })()}
            </span>
            <button
              type="button"
              onClick={() => stepSelect(1)}
              className="rounded-full border border-border px-2 py-0.5 transition-colors hover:text-foreground"
              title="Next pane"
            >
              ›
            </button>
          </div>
        )}

        {/* Panes that appeared since this device last looked — Multi mode only.
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
            {panes && mode === "watch"
              ? `${panes.length} viewed${
                  all?.length ? ` of ${all.length}` : ""
                }`
              : ""}
          </span>
        )}
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
          selectMode={mode === "select"}
          selectedKey={mode === "select" ? selectShownKey : null}
          onToggleWatch={toggleWatch}
          onFocusPane={focusInGrid}
          onOpenInHerdr={(p) => void focusPane(p)}
          onClose={requestClose}
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
                    ? "no panes viewed yet — toggle panes on in the pane list"
                    : "none of your viewed panes are running"}
                  <br />
                  <button
                    type="button"
                    onClick={() => toggleRail(true)}
                    className="mt-2 rounded border border-border px-2 py-0.5 text-[11px] text-muted-foreground hover:text-foreground"
                  >
                    browse panes
                  </button>
                </>
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
                watchable={mode !== "select"}
                pending={focusPending === cellKey(p)}
                onToggleWatch={() => toggleWatch(cellKey(p))}
                onClick={(e) => onCellClick(e, p)}
                onBodyFocus={() => focusInGridBackend(p)}
                onOpenInHerdr={() => focusPane(p)}
                onRename={() => requestRename(p)}
                onClose={() => requestClose(p)}
              />
            ))
          )}
        </div>
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
  watchable,
  pending,
  onToggleWatch,
  onClick,
  onBodyFocus,
  onOpenInHerdr,
  onRename,
  onClose,
}: {
  pane: GridPane
  active: boolean
  selected: boolean
  selectionCount: number
  focused: boolean
  watched: boolean
  /** Whether to offer the view toggle at all (hidden in Single mode). */
  watchable: boolean
  /** A focus (in-grid or into Herdr) for this cell is in flight — show a spinner. */
  pending: boolean
  onToggleWatch: () => void
  onClick: (e: React.MouseEvent | React.KeyboardEvent) => void
  /** The user clicked into this cell's terminal (its iframe took focus). */
  onBodyFocus: () => void
  onOpenInHerdr: () => void
  onRename: () => void
  onClose: () => void
}) {
  const id = frameId(p.host, p.terminal_id)
  const { host: activeHost } = useApp()
  const bodyRef = React.useRef<HTMLDivElement>(null)
  const [src, setSrc] = React.useState<string | null>(null)
  // The token of the attach this cell created (parsed from the proxy base).
  // Passed with our releases so a stale fire-and-forget release can only kill
  // OUR attach, never a newer one that re-claimed the pane after a remount.
  const tokenRef = React.useRef("")
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
    let retry: ReturnType<typeof setTimeout> | null = null
    const attach = async () => {
      // A pending deferred release means WE recently unmounted this pane —
      // cancel it so it can't kill the attach we're about to (re)use.
      cancelGridTermRelease(p.host, p.terminal_id)
      try {
        const { base } = await api.gridTerm(p.host, p.terminal_id)
        if (cancelled) return
        tokenRef.current = base.match(/\/grid-term\/([^/]+)\//)?.[1] ?? ""
        setFailed(false)
        setSrc(base)
      } catch {
        // Transient failures (backend stalled, SSH mux mid-redial) heal on
        // their own — keep retrying while the cell is on screen rather than
        // stranding it on "terminal unavailable".
        if (!cancelled) {
          setFailed(true)
          retry = setTimeout(() => void attach(), ATTACH_RETRY_MS)
        }
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
      if (retry) clearTimeout(retry)
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
    void api.gridTermRelease(p.host, p.terminal_id, tokenRef.current)
  }, [active, p.host, p.terminal_id])

  // When the cell goes away entirely, schedule (rather than fire) the release:
  // stepping right back re-uses the still-live attach instead of cold-starting.
  // Nothing to release if this cell never attached — an unconditional release
  // here could kill an attach another tab owns.
  React.useEffect(
    () => () => {
      if (tokenRef.current)
        scheduleGridTermRelease(p.host, p.terminal_id, tokenRef.current)
    },
    [p.host, p.terminal_id]
  )

  // Report clicks into the terminal (its window taking focus) so GridTab can
  // promote this pane to herdr's focused pane. Kept in a ref so the listener
  // attaches once per iframe rather than churning on every poll re-render.
  const onBodyFocusRef = React.useRef(onBodyFocus)
  onBodyFocusRef.current = onBodyFocus
  React.useEffect(() => {
    if (!src) return
    return onTerminalFocus(id, () => onBodyFocusRef.current())
  }, [src, id])

  // Drop back to the unattached state so the lazy-mount effect re-attaches (the
  // cell is on screen, so its IntersectionObserver fires immediately). Used when
  // the server killed our ttyd out from under us — a host switch evicts that
  // host's backend and releases every grid terminal streaming over it.
  const reattach = React.useCallback(() => {
    setSrc(null)
    setFailed(false)
    setReady(false)
  }, [])

  // A host switch releases grid terminals on BOTH the old and new active host
  // (their connections get replaced). Probe shortly after the active host
  // changes and re-attach if our ttyd died — twice, since the first probe can
  // race the switch still completing. The keepalive below is the slow backstop.
  const prevActiveHost = React.useRef(activeHost)
  React.useEffect(() => {
    if (prevActiveHost.current === activeHost) return
    prevActiveHost.current = activeHost
    if (!src) return
    const probe = () =>
      api
        .gridTermTouch(p.host, p.terminal_id)
        .then((r) => {
          if (!r.alive) reattach()
        })
        .catch(() => {})
    const timers = [500, 2500].map((d) => setTimeout(probe, d))
    return () => timers.forEach(clearTimeout)
  }, [activeHost, src, p.host, p.terminal_id, reattach])

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
    // It DOES report whether the entry is still alive — when the server released
    // us (host switch, reap), re-attach rather than showing a dead terminal.
    const ka = setInterval(() => {
      api
        .gridTermTouch(p.host, p.terminal_id)
        .then((r) => {
          if (!r.alive) reattach()
        })
        .catch(() => {})
    }, KEEPALIVE_MS)
    return () => {
      cleanup()
      cancelReady()
      clearInterval(ka)
    }
  }, [src, id, p.host, p.terminal_id, reattach])

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
      id={`cell-${id}`}
      className={cn(
        "termcell",
        focused && "focused",
        selected && "selected",
        pending && "focusing"
      )}
    >
      <ContextMenu>
        <ContextMenuTrigger asChild>
          {/* biome-ignore lint/a11y/useSemanticElements: the star inside is a
              real <button>, and buttons can't nest. */}
          <div
            role="button"
            tabIndex={0}
            className="termcell-head"
            title={`${tip}\n\nclick to open in Herdr · ⌘/Ctrl-click to select`}
            onClick={onClick}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault()
                onClick(e)
              }
            }}
          >
            <span className="termcell-host">{p.host_label}</span>
            {pending && (
              <span
                className="termcell-headspin"
                role="status"
                aria-label="focusing"
                title="focusing…"
              />
            )}
            <span className="termcell-title">
              {title}
              {tabLabel ? ` · ${tabLabel}` : ""}
            </span>
            {p.has_agent && (
              <span className={cn("termcell-agent", p.agent_status)}>
                ● {p.agent || "agent"}
              </span>
            )}
            {watchable && (
              <button
                type="button"
                className={cn("termcell-star", watched && "watched")}
                aria-pressed={watched}
                title={watched ? "Hide from the grid" : "View in the grid"}
                onClick={(e) => {
                  e.stopPropagation()
                  onToggleWatch()
                }}
              >
                {watched ? "★" : "☆"}
              </button>
            )}
          </div>
        </ContextMenuTrigger>
        <ContextMenuContent>
          <ContextMenuItem onSelect={onOpenInHerdr}>
            Open in Herdr
          </ContextMenuItem>
          {watchable && (
            <ContextMenuItem onSelect={onToggleWatch}>
              {watched ? "Hide from grid" : "Show in grid"}
            </ContextMenuItem>
          )}
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
