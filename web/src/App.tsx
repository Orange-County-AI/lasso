import { useQuery } from "@tanstack/react-query"
import {
  ChevronLeft,
  ChevronRight,
  Files,
  Globe,
  LayoutGrid,
  type LucideIcon,
  NotebookPen,
  PanelLeft,
  Settings,
} from "lucide-react"
import * as React from "react"
import type { PanelImperativeHandle } from "react-resizable-panels"
import { BrowserTab } from "@/components/BrowserTab"
import { CreateAgentDialog } from "@/components/CreateAgentDialog"
import { FilesPanel } from "@/components/FilesPanel"
import { GitStatusBadge } from "@/components/GitStatusBadge"
import { GridTab } from "@/components/GridTab"
import { HostSwitcher } from "@/components/HostSwitcher"
import { PaneSwitcher } from "@/components/PaneSwitcher"
import { ScratchTab } from "@/components/ScratchTab"
import { SettingsTab, ShortcutsDialog } from "@/components/SettingsTab"
import { Sidebar } from "@/components/Sidebar"
import { TabStrip } from "@/components/TabStrip"
import { TabTerminal } from "@/components/TabTerminal"
import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
} from "@/components/ui/resizable"
import { Toaster } from "@/components/ui/sonner"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { api, type TreeWorkspace } from "@/lib/api"
import { AppProvider, lsGet, lsSet, useApp } from "@/lib/app-store"
import { useDiff } from "@/lib/git"
import { installHistoryToggle } from "@/lib/history-toggle"
import { qk, queryClient } from "@/lib/query"
import { matchShortcut } from "@/lib/shortcuts"
import { kickTerminalSize, VIEWPORT_TERM_ID } from "@/lib/terminal"
import { patchUIState } from "@/lib/ui-state"
import { cn } from "@/lib/utils"

type RightView = "files" | "scratch" | "browser" | "settings"

// Persist each side panel's collapsed state and width (% of the group) so both
// survive reloads/restarts.
const LEFT_COLLAPSED_KEY = "lasso-left-collapsed"
const RIGHT_COLLAPSED_KEY = "lasso-right-collapsed"
const LEFT_WIDTH_KEY = "lasso-left-width"
const RIGHT_WIDTH_KEY = "lasso-right-width"
const GRID_MODE_KEY = "lasso-grid-mode"

// Read a persisted panel width (percentage), falling back to a default when
// it's missing or unparseable.
function lsGetWidth(key: string, fallback: number): number {
  const n = Number.parseFloat(lsGet(key) ?? "")
  return Number.isFinite(n) && n > 0 ? n : fallback
}

const stripClass =
  "h-auto w-full justify-start gap-0 rounded-none border-b border-border bg-background p-0"
const tabClass =
  "flex-none rounded-none border-0 border-b-2 border-transparent bg-transparent px-3 py-1.5 text-[13px] text-muted-foreground shadow-none data-[state=active]:border-primary data-[state=active]:bg-transparent data-[state=active]:text-primary data-[state=active]:shadow-none"

type TabDef = {
  value: string
  label: string
  icon: LucideIcon
  badge?: React.ReactNode
}

// A tab strip that shows full text labels when they fit, and collapses every
// tab to its icon when the track is too narrow for the labels — rather than
// truncating the last tab to "Settin…" or forcing a horizontal scroll.
function FitTabs({
  tabs,
  leading,
  trailing,
  listClassName,
}: {
  tabs: TabDef[]
  leading?: React.ReactNode
  trailing?: React.ReactNode
  listClassName?: string
}) {
  const scrollRef = React.useRef<HTMLDivElement>(null)
  const measureRef = React.useRef<HTMLDivElement>(null)
  const [compact, setCompact] = React.useState(false)

  React.useLayoutEffect(() => {
    const scroll = scrollRef.current
    const measure = measureRef.current
    if (!scroll || !measure) return
    const check = () => {
      // `measure` always renders the full-text tabs (hidden), so its natural
      // width is the space the labels need. If that can't fit the visible
      // track, switch to icons. The +1 absorbs sub-pixel rounding.
      setCompact(measure.scrollWidth > scroll.clientWidth + 1)
    }
    const ro = new ResizeObserver(check)
    ro.observe(scroll)
    ro.observe(measure)
    check()
    return () => ro.disconnect()
  }, [])

  return (
    <TabsList className={cn(stripClass, listClassName)}>
      {leading}
      {/* Tabs live in their own region; the leading/trailing controls stay fixed
          on the row. no-scrollbar hides the scrollbar so it doesn't steal row
          height. */}
      <div
        ref={scrollRef}
        className="no-scrollbar relative flex min-w-0 flex-1 overflow-x-auto"
      >
        {tabs.map(({ value, label, icon: Icon, badge }) => (
          <TabsTrigger
            key={value}
            value={value}
            className={tabClass}
            title={compact ? label : undefined}
          >
            {compact ? <Icon className="size-4" aria-label={label} /> : label}
            {badge}
          </TabsTrigger>
        ))}
        {/* Hidden full-text copy used only to measure the width the labels
            need; absolutely positioned so it never affects layout or the
            track's own width (which would create a measurement feedback loop). */}
        <div
          ref={measureRef}
          aria-hidden
          className="pointer-events-none invisible absolute top-0 left-0 flex"
        >
          {tabs.map(({ value, label, badge }) => (
            <span key={value} className={tabClass}>
              {label}
              {badge}
            </span>
          ))}
        </div>
      </div>
      {trailing}
    </TabsList>
  )
}

function Pane({
  show,
  children,
}: {
  show: boolean
  children: React.ReactNode
}) {
  return (
    <div className={cn("absolute inset-0 flex flex-col", !show && "hidden")}>
      {children}
    </div>
  )
}

export function App() {
  return (
    <AppProvider>
      <Shell />
      <Toaster />
    </AppProvider>
  )
}

// allWorkspaces flattens every selectable workspace: scratch, repo worktrees, and
// each repo's main checkout (which is folded into the repo header in the tree but
// must still resolve for the tab strip).
function allWorkspaces(
  tree:
    | {
        repos: {
          workspaces?: TreeWorkspace[]
          main_workspace?: TreeWorkspace
        }[]
        scratch?: TreeWorkspace[]
      }
    | undefined
): TreeWorkspace[] {
  if (!tree) return []
  const out: TreeWorkspace[] = [...(tree.scratch ?? [])]
  for (const r of tree.repos ?? []) {
    out.push(...(r.workspaces ?? []))
    if (r.main_workspace) out.push(r.main_workspace)
  }
  return out
}

// findWorkspace locates the workspace owning a tab id.
function findWorkspace(
  tree: Parameters<typeof allWorkspaces>[0],
  tabId: string | null
): TreeWorkspace | null {
  if (!tabId) return null
  return (
    allWorkspaces(tree).find((ws) =>
      (ws.tabs ?? []).some((t) => t.id === tabId)
    ) ?? null
  )
}

function Shell() {
  const [rightView, setRightView] = React.useState<RightView>("files")
  const [rightCollapsed, setRightCollapsed] = React.useState(
    () => lsGet(RIGHT_COLLAPSED_KEY) === "true"
  )
  const [leftCollapsed, setLeftCollapsed] = React.useState(
    () => lsGet(LEFT_COLLAPSED_KEY) === "true"
  )
  const [paletteOpen, setPaletteOpen] = React.useState(false)
  const [shortcutsOpen, setShortcutsOpen] = React.useState(false)
  const [gridMode, setGridMode] = React.useState(
    () => lsGet(GRID_MODE_KEY) === "true"
  )
  const [selectedTabId, setSelectedTabId] = React.useState<string | null>(() =>
    lsGet("lasso-selected-tab")
  )

  const leftPanel = React.useRef<PanelImperativeHandle>(null)
  const rightPanel = React.useRef<PanelImperativeHandle>(null)

  // Last expanded width of each side panel (% of the group). Seeded from
  // localStorage for an instant first paint, and updated on every resize so
  // expanding from collapsed returns to where they left it. The durable copy
  // lives server-side (/api/ui-state) so the layout survives a lasso restart
  // and stays consistent across browsers/tabs: widths are pulled below and
  // pushed (debounced) on resize; other clients' writes arrive via the SSE
  // ui_rev. Last write wins.
  const leftWidth = React.useRef(lsGetWidth(LEFT_WIDTH_KEY, 18))
  const rightWidth = React.useRef(lsGetWidth(RIGHT_WIDTH_KEY, 32))

  // What the server is known to hold, so an incoming pull doesn't get echoed
  // straight back as a push. pushable stays false until the first fetch lands —
  // a freshly-opened tab must not clobber the server with its localStorage seed.
  const serverWidths = React.useRef<{ left?: number; right?: number }>({})
  const pushable = React.useRef(false)
  const pushTimer = React.useRef<number | null>(null)

  // Persist the current widths server-side, debounced past the drag's resize
  // stream so only the width the user settles on is written.
  const schedulePush = React.useCallback(() => {
    if (!pushable.current) return
    if (pushTimer.current !== null) window.clearTimeout(pushTimer.current)
    pushTimer.current = window.setTimeout(() => {
      pushTimer.current = null
      const left = leftWidth.current
      const right = rightWidth.current
      const s = serverWidths.current
      const same = (a: number | undefined, b: number) =>
        a !== undefined && Math.abs(a - b) < 0.5
      if (same(s.left, left) && same(s.right, right)) return
      serverWidths.current = { left, right }
      // patchUIState merges into the shared ui_state blob, so the width push
      // can't clobber the grid filters (and vice versa).
      patchUIState({ left_width: left, right_width: right })
    }, 400)
  }, [])
  React.useEffect(
    () => () => {
      if (pushTimer.current !== null) window.clearTimeout(pushTimer.current)
    },
    []
  )

  // Pull the server widths on load, and again whenever any client writes them
  // (the SSE ui_rev bumps on every /api/ui-state POST).
  const { uiRev } = useApp()
  React.useEffect(() => {
    if (uiRev >= 0) void queryClient.invalidateQueries({ queryKey: qk.uiState })
  }, [uiRev])
  const uiState = useQuery({ queryKey: qk.uiState, queryFn: api.uiState })
  React.useEffect(() => {
    const us = uiState.data
    if (!us) return
    serverWidths.current = { left: us.left_width, right: us.right_width }
    pushable.current = true
    const apply = (
      pct: number | undefined,
      widthRef: React.RefObject<number>,
      key: string,
      panel: React.RefObject<PanelImperativeHandle | null>
    ) => {
      // Zero/absent = never saved; the ±0.5% tolerance keeps a pull from
      // fighting this tab's own in-flight drag over rounding noise.
      if (!pct || !Number.isFinite(pct)) return
      if (Math.abs(pct - widthRef.current) < 0.5) return
      widthRef.current = pct
      lsSet(key, String(pct))
      const p = panel.current
      if (p && !p.isCollapsed()) p.resize(`${pct}%`)
    }
    apply(us.left_width, leftWidth, LEFT_WIDTH_KEY, leftPanel)
    apply(us.right_width, rightWidth, RIGHT_WIDTH_KEY, rightPanel)
  }, [uiState.data])

  const diff = useDiff()
  const diffDirty = diff.data?.dirty ?? 0
  const gitReady = diff.data != null

  const tree = useQuery({ queryKey: qk.tree, queryFn: api.tree })

  const activeWorkspace = React.useMemo(
    () => findWorkspace(tree.data, selectedTabId),
    [tree.data, selectedTabId]
  )

  // Tabs we've ever seen in the tree. Lets us tell a tab that's *gone* (was
  // present, now closed → move selection off it) from one that's merely *not
  // arrived yet* (a freshly-created agent the tree hasn't refetched → keep the
  // selection so create-focus isn't clobbered by a stale refetch).
  const seenTabs = React.useRef<Set<string>>(new Set())

  // Order in which tabs have been opened, most-recent last (deduped). When the
  // active tab's workspace is closed we fall back to the last terminal the user
  // was on rather than jumping to the global first tab.
  const tabHistory = React.useRef<string[]>([])
  React.useEffect(() => {
    if (!selectedTabId) return
    const h = tabHistory.current
    const i = h.indexOf(selectedTabId)
    if (i !== -1) h.splice(i, 1)
    h.push(selectedTabId)
  }, [selectedTabId])

  // Default the selection to the first tab once the tree loads, and move off a
  // selection whose tab was closed — but never off one still loading in.
  React.useEffect(() => {
    if (!tree.data) return
    const tabs = allWorkspaces(tree.data).flatMap((ws) => ws.tabs ?? [])
    const ids = new Set(tabs.map((t) => t.id))
    for (const t of tabs) seenTabs.current.add(t.id)
    // Drop closed tabs from history so the fallback only ever lands on a live one.
    tabHistory.current = tabHistory.current.filter((id) => ids.has(id))
    if (selectedTabId) {
      const exists = ids.has(selectedTabId)
      // Present, or selected-but-not-yet-in-tree (pending create) → leave it.
      if (exists || !seenTabs.current.has(selectedTabId)) return
    }
    // Selection's tab is gone (e.g. its workspace was closed): fall back to the
    // last opened terminal still alive, else the first tab.
    const recent = tabHistory.current[tabHistory.current.length - 1]
    setSelectedTabId(recent ?? tabs[0]?.id ?? null)
  }, [tree.data, selectedTabId])

  const selectTab = React.useCallback((tabId: string) => {
    setSelectedTabId(tabId)
    lsSet("lasso-selected-tab", tabId)
  }, [])

  // The UI agent creator focuses its new agent by asking us to select its tab.
  // (Agents created via MCP never dispatch this, so they don't steal focus.)
  React.useEffect(() => {
    const onSelect = (e: Event) => {
      const tabId = (e as CustomEvent).detail as string
      if (tabId) selectTab(tabId)
    }
    window.addEventListener("lasso:select-tab", onSelect)
    return () => window.removeEventListener("lasso:select-tab", onSelect)
  }, [selectTab])

  // Feed the selected tab's cwd to the Files/Diff panel (which follows
  // useApp().activeCwd). The workspace's work_dir is only the tab's *launch*
  // dir; we seed from it for an instant value, then poll the terminal's live
  // pane cwd so the panel follows the active terminal as the user cd's around.
  React.useEffect(() => {
    if (!selectedTabId) return
    const emit = (cwd: string | undefined) => {
      if (cwd)
        window.dispatchEvent(new CustomEvent("lasso:cwd", { detail: cwd }))
    }
    // Instant seed so the panel isn't blank while the first poll is in flight.
    emit(activeWorkspace?.work_dir)
    let cancelled = false
    const poll = () =>
      api
        .tabCwd(selectedTabId)
        .then((r) => {
          if (!cancelled) emit(r.cwd)
        })
        .catch(() => {
          /* session not live yet / transient host blip — keep last cwd */
        })
    void poll()
    // Backgrounded tabs don't change cwd visibly; skip polling while hidden.
    const id = setInterval(() => {
      if (!document.hidden) void poll()
    }, 2500)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [selectedTabId, activeWorkspace?.work_dir])

  const toggleLeft = React.useCallback(() => {
    const p = leftPanel.current
    if (!p) return
    if (p.isCollapsed()) p.resize(`${leftWidth.current}%`)
    else p.collapse()
  }, [])
  const toggleRight = React.useCallback(() => {
    const p = rightPanel.current
    if (!p) return
    if (p.isCollapsed()) p.resize(`${rightWidth.current}%`)
    else p.collapse()
  }, [])

  // The Grid is a full main view: it replaces the spaces sidebar AND the center
  // terminal (the wall of live terminals IS the workspace overview). Entering it
  // collapses the left panel so the grid spans both; leaving restores the
  // sidebar unless the user had collapsed it themselves before entering. The
  // terminal viewport underneath stays mounted (just hidden) so flipping back
  // is instant — no xterm re-handshake.
  const preGridLeftCollapsed = React.useRef(false)
  const wasGridMode = React.useRef(false)
  const toggleGrid = React.useCallback(() => {
    setGridMode((on) => {
      lsSet(GRID_MODE_KEY, String(!on))
      return !on
    })
  }, [])
  React.useEffect(() => {
    const p = leftPanel.current
    if (!p) return
    if (gridMode) {
      // Entering (or loading straight into) grid mode: remember whether the
      // user had the sidebar collapsed themselves, then take its space.
      if (!wasGridMode.current) preGridLeftCollapsed.current = p.isCollapsed()
      p.collapse()
    } else if (wasGridMode.current) {
      if (!preGridLeftCollapsed.current) p.resize(`${leftWidth.current}%`)
      // The grid's per-cell tmux clients shrank the shared windows (tmux sizes
      // to the latest active client). Their release alone may not restore the
      // viewport's width — tmux falls back to whichever remaining client was
      // active last, which can be a stale co-viewer. Assert the visible
      // viewport as the latest client so the window snaps back now, not on the
      // user's next keystroke. Delayed a beat so the iframe is unhidden first.
      const t = setTimeout(() => kickTerminalSize(VIEWPORT_TERM_ID), 150)
      wasGridMode.current = gridMode
      return () => clearTimeout(t)
    }
    wasGridMode.current = gridMode
  }, [gridMode])

  // Focusing a grid pane drops back to the normal view on that pane (the grid
  // is the overview; focus means "take me to this terminal").
  const selectTabFromGrid = React.useCallback(
    (tabId: string) => {
      if (gridMode) toggleGrid()
      selectTab(tabId)
    },
    [gridMode, toggleGrid, selectTab]
  )

  // ⌘K opens the switcher, ⌘I the new-workspace modal. Cmd-only so terminal
  // control keys (Ctrl-*) are never clobbered; the terminal iframes re-dispatch
  // Cmd shortcuts to this document so they work with focus inside. (⌘[/⌘] are
  // NOT keydowns — macOS reserves them for history nav and eats them before the
  // page; they're handled by the history trap below.)
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const action = matchShortcut(e)
      if (!action) return
      e.preventDefault()
      e.stopPropagation()
      if (action === "palette") setPaletteOpen(true)
      else if (action === "shortcuts") setShortcutsOpen(true)
      else if (action === "grid") toggleGrid()
      else if (action === "new-workspace")
        window.dispatchEvent(new CustomEvent("lasso:new-workspace"))
      else if (action === "new-tab")
        window.dispatchEvent(new CustomEvent("lasso:new-tab"))
    }
    document.addEventListener("keydown", onKey, true)
    return () => document.removeEventListener("keydown", onKey, true)
  }, [toggleGrid])

  // ⌘[ / ⌘] (and the Back/Forward buttons / swipe) toggle the side panels via a
  // history trap, since macOS browsers won't deliver those keys to the page.
  React.useEffect(
    () => installHistoryToggle(toggleLeft, toggleRight),
    [toggleLeft, toggleRight]
  )

  return (
    <div className="relative flex h-full w-full flex-col">
      {/* Full-width header: the app-level controls live here — never inside a
          panel — so their placement is identical whether the sidebar is open,
          collapsed, or replaced by the grid. */}
      <div className="flex shrink-0 items-center border-border border-b">
        <button
          type="button"
          title={
            gridMode
              ? "exit grid to the sidebar"
              : leftCollapsed
                ? "show sidebar (⌘[)"
                : "hide sidebar (⌘[)"
          }
          aria-pressed={!gridMode && !leftCollapsed}
          className="px-2 py-1.5 text-muted-foreground hover:text-primary"
          onClick={() => {
            // In grid mode the sidebar's space belongs to the grid; asking for
            // the sidebar means leaving the grid (and restoring it expanded).
            if (gridMode) {
              preGridLeftCollapsed.current = false
              toggleGrid()
              return
            }
            toggleLeft()
          }}
        >
          <PanelLeft className="size-4" />
        </button>
        <button
          type="button"
          aria-pressed={gridMode}
          title={gridMode ? "back to terminal (⌘G)" : "grid view (⌘G)"}
          className={cn(
            "mr-1 flex size-6 shrink-0 items-center justify-center self-center rounded border",
            gridMode
              ? "border-primary/50 bg-accent text-primary"
              : "border-border text-muted-foreground hover:border-primary hover:text-primary"
          )}
          onClick={toggleGrid}
        >
          <LayoutGrid className="size-3.5" />
        </button>
        <HostSwitcher />
        <div className="min-w-0 flex-1" />
        <div className="ml-2 flex shrink-0 items-center gap-1.5">
          <CreateAgentDialog variant="header" />
          {rightCollapsed && (
            <>
              <GitStatusBadge
                dirty={diffDirty}
                ready={gitReady}
                textClassName="self-center text-[13px]"
              />
              <button
                type="button"
                className="my-1 mr-1.5 flex size-6 shrink-0 items-center justify-center self-center rounded border border-border text-muted-foreground hover:border-primary hover:text-primary"
                title="show file viewer (⌘])"
                onClick={toggleRight}
              >
                <ChevronLeft className="size-4" />
              </button>
            </>
          )}
        </div>
      </div>

      <ResizablePanelGroup
        orientation="horizontal"
        className="min-h-0 w-full flex-1"
      >
        {/* Left: spaces tree + agents pane */}
        <ResizablePanel
          id="sidebar"
          panelRef={leftPanel}
          defaultSize={leftCollapsed ? 0 : leftWidth.current}
          minSize={12}
          collapsible
          collapsedSize={0}
          onResize={(s) => {
            const collapsed = s.asPercentage < 0.05
            setLeftCollapsed(collapsed)
            lsSet(LEFT_COLLAPSED_KEY, String(collapsed))
            if (!collapsed) {
              leftWidth.current = s.asPercentage
              lsSet(LEFT_WIDTH_KEY, String(s.asPercentage))
              schedulePush()
            }
          }}
          className="min-h-0"
        >
          <Sidebar
            selectedTabId={selectedTabId}
            onSelectTab={selectTab}
            onCollapse={toggleLeft}
          />
        </ResizablePanel>

        <ResizableHandle withHandle className={cn(leftCollapsed && "hidden")} />

        {/* Center: the selected workspace's tabs + terminal (or the grid) */}
        <ResizablePanel
          id="center"
          defaultSize={50}
          minSize={20}
          className="min-h-0"
        >
          <div className="flex h-full min-h-0 flex-col">
            {/* The active workspace's tab strip, its own row under the global
                header (which holds the app-level controls). Hidden in grid
                mode — the grid spans every workspace. */}
            {!gridMode && (
              <TabStrip
                workspace={activeWorkspace}
                selectedTabId={selectedTabId}
                onSelectTab={selectTab}
              />
            )}
            <div className="relative flex min-h-0 flex-1 flex-col">
              {/* One persistent viewport, mounted once and pointed at the
                  selected tab — never remounted per tab (see TabTerminal). Pass
                  null when the selection doesn't resolve to a workspace (matches
                  the strip's "no workspace selected") so TabTerminal hides the
                  iframe and shows the empty state instead of a stray session.
                  In grid mode it stays mounted underneath (hidden) so leaving
                  the grid doesn't re-handshake xterm. */}
              <div
                className={cn(
                  "flex min-h-0 flex-1 flex-col",
                  gridMode && "hidden"
                )}
              >
                <TabTerminal tabId={activeWorkspace ? selectedTabId : null} />
              </div>
              {/* The grid view — the cross-host wall of live terminals that
                  replaces the sidebar + terminal while active. */}
              <div
                className={cn(
                  "absolute inset-0 flex-col",
                  gridMode ? "flex" : "hidden"
                )}
              >
                <GridTab active={gridMode} selectTab={selectTabFromGrid} />
              </div>
            </div>
          </div>
        </ResizablePanel>

        <ResizableHandle
          withHandle
          className={cn(rightCollapsed && "hidden", "max-md:hidden")}
        />

        {/* Right: files / scratch / browser / settings */}
        <ResizablePanel
          id="right"
          panelRef={rightPanel}
          defaultSize={rightCollapsed ? 0 : rightWidth.current}
          minSize={15}
          collapsible
          collapsedSize={0}
          onResize={(s) => {
            const collapsed = s.asPercentage < 0.05
            setRightCollapsed(collapsed)
            lsSet(RIGHT_COLLAPSED_KEY, String(collapsed))
            if (!collapsed) {
              rightWidth.current = s.asPercentage
              lsSet(RIGHT_WIDTH_KEY, String(s.asPercentage))
              schedulePush()
            }
          }}
          className="relative flex h-full min-h-0 flex-col border-border border-l bg-card"
        >
          <Tabs
            value={rightView}
            onValueChange={(v) => setRightView(v as RightView)}
            className="flex h-full flex-col gap-0"
          >
            <FitTabs
              tabs={[
                {
                  value: "files",
                  label: "Files",
                  icon: Files,
                  badge: (
                    <GitStatusBadge
                      dirty={diffDirty}
                      ready={gitReady}
                      className="ml-1.5"
                      textClassName="text-[13px]"
                    />
                  ),
                },
                { value: "scratch", label: "Scratch", icon: NotebookPen },
                { value: "browser", label: "Browser", icon: Globe },
                { value: "settings", label: "Settings", icon: Settings },
              ]}
              trailing={
                // Styled like the tab icons (same box model) rather than a
                // bordered box, so it sits on the same baseline as them.
                <button
                  type="button"
                  className={cn(
                    tabClass,
                    "flex items-center hover:text-primary"
                  )}
                  title="collapse panel (⌘])"
                  onClick={toggleRight}
                >
                  <ChevronRight className="size-4" />
                </button>
              }
            />
            <div className="relative min-h-0 flex-1">
              <Pane show={rightView === "files"}>
                <FilesPanel />
              </Pane>
              <Pane show={rightView === "scratch"}>
                <ScratchTab />
              </Pane>
              <Pane show={rightView === "browser"}>
                <BrowserTab />
              </Pane>
              <Pane show={rightView === "settings"}>
                <SettingsTab active={rightView === "settings"} />
              </Pane>
            </div>
          </Tabs>
        </ResizablePanel>
      </ResizablePanelGroup>

      {/* ⌘K switcher — searches workspaces/tabs/agents by display name. */}
      <PaneSwitcher
        open={paletteOpen}
        onOpenChange={setPaletteOpen}
        onSelectTab={selectTab}
      />

      {/* ⌘? opens the keyboard-shortcuts reference from anywhere (the Settings
          tab has its own copy behind the keyboard icon). */}
      <ShortcutsDialog open={shortcutsOpen} onOpenChange={setShortcutsOpen} />
    </div>
  )
}
