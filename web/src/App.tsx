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
import { SettingsTab } from "@/components/SettingsTab"
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
import { AppProvider, lsGet, lsSet } from "@/lib/app-store"
import { useDiff } from "@/lib/git"
import { qk } from "@/lib/query"
import { cn } from "@/lib/utils"

type RightView = "files" | "scratch" | "browser" | "grid" | "settings"

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
  const [rightCollapsed, setRightCollapsed] = React.useState(false)
  const [leftCollapsed, setLeftCollapsed] = React.useState(false)
  const [paletteOpen, setPaletteOpen] = React.useState(false)
  const [selectedTabId, setSelectedTabId] = React.useState<string | null>(() =>
    lsGet("lasso-selected-tab")
  )

  const leftPanel = React.useRef<PanelImperativeHandle>(null)
  const rightPanel = React.useRef<PanelImperativeHandle>(null)

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

  // Default the selection to the first tab once the tree loads, and move off a
  // selection whose tab was closed — but never off one still loading in.
  React.useEffect(() => {
    if (!tree.data) return
    const tabs = allWorkspaces(tree.data).flatMap((ws) => ws.tabs ?? [])
    for (const t of tabs) seenTabs.current.add(t.id)
    if (selectedTabId) {
      const exists = tabs.some((t) => t.id === selectedTabId)
      // Present, or selected-but-not-yet-in-tree (pending create) → leave it.
      if (exists || !seenTabs.current.has(selectedTabId)) return
    }
    setSelectedTabId(tabs[0]?.id ?? null)
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

  // Feed the selected workspace's cwd to the Files/Diff panel (which follows
  // useApp().activeCwd).
  React.useEffect(() => {
    if (activeWorkspace?.work_dir)
      window.dispatchEvent(
        new CustomEvent("lasso:cwd", { detail: activeWorkspace.work_dir })
      )
  }, [activeWorkspace?.work_dir])

  const toggleLeft = React.useCallback(() => {
    const p = leftPanel.current
    if (!p) return
    if (p.isCollapsed()) p.resize("18%")
    else p.collapse()
  }, [])
  const toggleRight = React.useCallback(() => {
    const p = rightPanel.current
    if (!p) return
    if (p.isCollapsed()) p.resize("32%")
    else p.collapse()
  }, [])

  // The Grid wants more room than the 32% default right panel (its cells have a
  // ~360px min). When the Grid tab is opened, widen the panel; restore the prior
  // size on leave. The width before opening Grid is remembered in a ref.
  const preGridSize = React.useRef<number | null>(null)
  React.useEffect(() => {
    const p = rightPanel.current
    if (!p) return
    if (rightView === "grid") {
      const cur = p.getSize().asPercentage * 100
      if (cur < 55) {
        preGridSize.current = cur
        p.resize("62%")
      }
    } else if (preGridSize.current != null) {
      p.resize(`${preGridSize.current}%`)
      preGridSize.current = null
    }
  }, [rightView])

  // ⌘[ toggles the left sidebar, ⌘] the right panel, ⌘K the switcher, ⌘I opens
  // the new-workspace modal. Cmd-only so terminal control keys (Ctrl-*) are never
  // clobbered; the terminal iframes re-dispatch Cmd shortcuts to this document so
  // they work with focus inside.
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!e.metaKey || e.ctrlKey || e.altKey || e.shiftKey) return
      const k = e.key.toLowerCase()
      if (k === "[") {
        e.preventDefault()
        toggleLeft()
      } else if (k === "]") {
        e.preventDefault()
        toggleRight()
      } else if (k === "k") {
        e.preventDefault()
        setPaletteOpen(true)
      } else if (k === "i") {
        e.preventDefault()
        window.dispatchEvent(new CustomEvent("lasso:new-workspace"))
      }
    }
    document.addEventListener("keydown", onKey)
    return () => document.removeEventListener("keydown", onKey)
  }, [toggleLeft, toggleRight])

  return (
    <div className="relative h-full w-full">
      <ResizablePanelGroup orientation="horizontal" className="h-full w-full">
        {/* Left: spaces tree + agents pane */}
        <ResizablePanel
          id="sidebar"
          panelRef={leftPanel}
          defaultSize={18}
          minSize={12}
          collapsible
          collapsedSize={0}
          onResize={(s) => setLeftCollapsed(s.asPercentage < 0.05)}
          className="min-h-0"
        >
          <Sidebar selectedTabId={selectedTabId} onSelectTab={selectTab} />
        </ResizablePanel>

        <ResizableHandle withHandle className={cn(leftCollapsed && "hidden")} />

        {/* Center: tab strip + the selected tab's terminal */}
        <ResizablePanel
          id="center"
          defaultSize={50}
          minSize={20}
          className="min-h-0"
        >
          <div className="flex h-full min-h-0 flex-col">
            <div className="flex items-center">
              {leftCollapsed && (
                <button
                  type="button"
                  title="show sidebar (⌘[)"
                  className="px-2 py-1.5 text-muted-foreground hover:text-primary"
                  onClick={toggleLeft}
                >
                  <PanelLeft className="size-4" />
                </button>
              )}
              <HostSwitcher />
              <div className="min-w-0 flex-1">
                <TabStrip
                  workspace={activeWorkspace}
                  selectedTabId={selectedTabId}
                  onSelectTab={selectTab}
                />
              </div>
              {/* New Agent at the far right; when the file viewer is collapsed the
                  git status + an expand control follow it (mirrors main, so git
                  state is visible and the panel reachable without ⌘]). */}
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
            <div className="relative flex min-h-0 flex-1 flex-col">
              {/* One persistent viewport, mounted once and pointed at the
                  selected tab — never remounted per tab (see TabTerminal). Pass
                  null when the selection doesn't resolve to a workspace (matches
                  the strip's "no workspace selected") so TabTerminal hides the
                  iframe and shows the empty state instead of a stray session. */}
              <TabTerminal tabId={activeWorkspace ? selectedTabId : null} />
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
          defaultSize={32}
          minSize={15}
          collapsible
          collapsedSize={0}
          onResize={(s) => setRightCollapsed(s.asPercentage < 0.05)}
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
                { value: "grid", label: "Grid", icon: LayoutGrid },
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
              <Pane show={rightView === "grid"}>
                <GridTab active={rightView === "grid"} selectTab={selectTab} />
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
    </div>
  )
}
