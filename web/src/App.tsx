import { useQuery } from "@tanstack/react-query"
import {
  ChevronRight,
  Files,
  Globe,
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

type RightView = "files" | "scratch" | "browser" | "settings"

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

// findWorkspace locates the workspace owning a tab id across repos + scratch.
function findWorkspace(
  tree:
    | { repos: { workspaces: TreeWorkspace[] }[]; scratch: TreeWorkspace[] }
    | undefined,
  tabId: string | null
): TreeWorkspace | null {
  if (!tree || !tabId) return null
  const all = [...tree.scratch, ...tree.repos.flatMap((r) => r.workspaces)]
  return all.find((ws) => ws.tabs.some((t) => t.id === tabId)) ?? null
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

  // Default the selection to the first available tab once the tree loads, and
  // drop a stale selection (its tab was closed) onto another tab.
  React.useEffect(() => {
    if (!tree.data) return
    const all = [
      ...tree.data.scratch,
      ...tree.data.repos.flatMap((r) => r.workspaces),
    ]
    const exists = all.some((ws) => ws.tabs.some((t) => t.id === selectedTabId))
    if (selectedTabId && exists) return
    const first = all.flatMap((ws) => ws.tabs)[0]
    setSelectedTabId(first?.id ?? null)
  }, [tree.data, selectedTabId])

  const selectTab = React.useCallback((tabId: string) => {
    setSelectedTabId(tabId)
    lsSet("lasso-selected-tab", tabId)
  }, [])

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

  // ⌘[ toggles the left sidebar, ⌘] the right panel, ⌘K the switcher. Cmd-only so
  // terminal control keys (Ctrl-*) are never clobbered; the terminal iframes
  // re-dispatch Cmd shortcuts to this document so they work with focus inside.
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
              <div className="min-w-0 flex-1">
                <TabStrip
                  workspace={activeWorkspace}
                  selectedTabId={selectedTabId}
                  onSelectTab={selectTab}
                />
              </div>
              <CreateAgentDialog variant="header" />
            </div>
            <div className="relative flex min-h-0 flex-1 flex-col">
              {selectedTabId ? (
                <TabTerminal key={selectedTabId} tabId={selectedTabId} />
              ) : (
                <div className="flex h-full items-center justify-center text-muted-foreground text-sm">
                  No tab selected. Create an agent, or pick a workspace.
                </div>
              )}
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
            <TabsList className={stripClass}>
              {(
                [
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
                ] as TabDef[]
              ).map(({ value, label, badge }) => (
                <TabsTrigger key={value} value={value} className={tabClass}>
                  {label}
                  {badge}
                </TabsTrigger>
              ))}
              <button
                type="button"
                className={cn(
                  tabClass,
                  "ml-auto flex items-center hover:text-primary"
                )}
                title="collapse panel (⌘])"
                onClick={toggleRight}
              >
                <ChevronRight className="size-4" />
              </button>
            </TabsList>
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
    </div>
  )
}
