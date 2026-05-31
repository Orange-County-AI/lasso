import { ChevronLeft, ChevronRight } from "lucide-react"
import * as React from "react"
import type { Layout, PanelImperativeHandle } from "react-resizable-panels"
import { AgentsTab } from "@/components/AgentsTab"
import { BrowserTab } from "@/components/BrowserTab"
import { DiffTab } from "@/components/DiffTab"
import { FilesTab } from "@/components/FilesTab"
import { Footer } from "@/components/Footer"
import { SettingsTab } from "@/components/SettingsTab"
import { TerminalFrame } from "@/components/TerminalFrame"
import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
} from "@/components/ui/resizable"
import { Toaster } from "@/components/ui/sonner"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { AppProvider, lsGet, lsSet } from "@/lib/app-store"
import { typeIntoShell } from "@/lib/terminal"
import { cn } from "@/lib/utils"

// The file viewer pulls in react-markdown + highlight.js; load it only on first
// file open so the initial page stays light (mirrors the original lazy libs).
const FileViewer = React.lazy(() =>
  import("@/components/FileViewer").then((m) => ({ default: m.FileViewer }))
)

type LeftView = "herdr" | "settings"
type RightView = "diff" | "files" | "agents" | "browser" | "terminal"

const LEFT_VIEWS: LeftView[] = ["herdr", "settings"]

// Shared tab-strip styling: a full-width underline strip, matching the original
// vanilla UI rather than shadcn's default pill TabsList.
const stripClass =
  "h-auto w-full justify-start gap-0 rounded-none border-b border-border bg-background p-0"
const tabClass =
  "flex-none rounded-none border-0 border-b-2 border-transparent bg-transparent px-3 py-1.5 text-xs text-muted-foreground shadow-none data-[state=active]:border-primary data-[state=active]:bg-transparent data-[state=active]:text-primary data-[state=active]:shadow-none"

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

function Shell() {
  const [leftView, setLeftView] = React.useState<LeftView>(() => {
    const h = location.hash.slice(1)
    return (LEFT_VIEWS as string[]).includes(h) ? (h as LeftView) : "herdr"
  })
  const [rightView, setRightView] = React.useState<RightView>("diff")
  const [collapsed, setCollapsed] = React.useState(false)
  const [viewerPath, setViewerPath] = React.useState<string | null>(null)
  const [diffDirty, setDiffDirty] = React.useState(0)
  const rightPanel = React.useRef<PanelImperativeHandle>(null)

  const savedLayout = React.useMemo<Layout | undefined>(() => {
    try {
      const v = lsGet("lasso-layout")
      return v ? (JSON.parse(v) as Layout) : undefined
    } catch {
      return undefined
    }
  }, [])

  const switchLeft = React.useCallback((name: LeftView, fromHash = false) => {
    setLeftView(name)
    if (!fromHash && location.hash.slice(1) !== name) location.hash = name
  }, [])

  // Back/forward and manual hash edits drive the active left tab.
  React.useEffect(() => {
    const onHash = () => {
      const h = location.hash.slice(1)
      switchLeft(
        (LEFT_VIEWS as string[]).includes(h) ? (h as LeftView) : "herdr",
        true
      )
    }
    window.addEventListener("hashchange", onHash)
    return () => window.removeEventListener("hashchange", onHash)
  }, [switchLeft])

  // Restore the persisted collapse state once the panel is mounted.
  React.useEffect(() => {
    if (lsGet("sidebarCollapsed") === "1") rightPanel.current?.collapse()
    // run once on mount
  }, [])

  const openUpdateInTerminal = () => {
    rightPanel.current?.expand() // ensure the right column is visible
    setRightView("terminal")
    typeIntoShell("herdr update")
  }

  return (
    <div className="flex h-full w-full flex-col">
      <ResizablePanelGroup
        orientation="horizontal"
        defaultLayout={savedLayout}
        onLayoutChanged={(l) => lsSet("lasso-layout", JSON.stringify(l))}
        className="min-h-0 w-full flex-1"
      >
        <ResizablePanel
          id="left"
          defaultSize={60}
          minSize={15}
          className="flex h-full min-h-0 flex-col"
        >
          <Tabs
            value={leftView}
            onValueChange={(v) => switchLeft(v as LeftView)}
            className="flex h-full flex-col gap-0"
          >
            <TabsList className={cn(stripClass, collapsed && "pr-2")}>
              <TabsTrigger value="herdr" className={tabClass}>
                Herdr
              </TabsTrigger>
              <TabsTrigger value="settings" className={tabClass}>
                Settings
              </TabsTrigger>
              {collapsed && (
                <button
                  className="ml-auto self-center rounded border border-border px-1.5 text-muted-foreground hover:border-primary hover:text-primary"
                  title="show file viewer"
                  onClick={() => rightPanel.current?.expand()}
                >
                  <ChevronLeft className="size-4" />
                </button>
              )}
            </TabsList>
            <div className="relative min-h-0 flex-1">
              <Pane show={leftView === "herdr"}>
                <TerminalFrame
                  id="term"
                  src="/terminal/"
                  title="Herdr terminal"
                  suppressContext
                  hidden={leftView !== "herdr"}
                />
              </Pane>
              <Pane show={leftView === "settings"}>
                <SettingsTab
                  active={leftView === "settings"}
                  onOpenUpdate={openUpdateInTerminal}
                />
              </Pane>
            </div>
          </Tabs>
        </ResizablePanel>

        <ResizableHandle withHandle className={cn(collapsed && "hidden")} />

        <ResizablePanel
          id="right"
          panelRef={rightPanel}
          defaultSize={40}
          minSize={15}
          collapsible
          collapsedSize={0}
          onResize={(size) => {
            const c = size.asPercentage < 0.05
            setCollapsed((prev) => (prev === c ? prev : c))
            lsSet("sidebarCollapsed", c ? "1" : "0")
          }}
          className="relative flex h-full min-h-0 flex-col border-border border-l bg-card"
        >
          <Tabs
            value={rightView}
            onValueChange={(v) => setRightView(v as RightView)}
            className="flex h-full flex-col gap-0"
          >
            <TabsList className={cn(stripClass, "pr-2")}>
              <TabsTrigger value="diff" className={tabClass}>
                Diff
                {diffDirty > 0 && (
                  <span
                    className="ml-1.5 rounded-full bg-warn px-1.5 font-semibold text-[10px] text-background"
                    title={`${diffDirty} uncommitted change${diffDirty === 1 ? "" : "s"}`}
                  >
                    {diffDirty}
                  </span>
                )}
              </TabsTrigger>
              <TabsTrigger value="files" className={tabClass}>
                Files
              </TabsTrigger>
              <TabsTrigger value="agents" className={tabClass}>
                Agents
              </TabsTrigger>
              <TabsTrigger value="browser" className={tabClass}>
                Browser
              </TabsTrigger>
              <TabsTrigger value="terminal" className={tabClass}>
                Terminal
              </TabsTrigger>
              <button
                className="ml-auto self-center rounded border border-border px-1.5 text-muted-foreground hover:border-primary hover:text-primary"
                title="collapse sidebar"
                onClick={() => rightPanel.current?.collapse()}
              >
                <ChevronRight className="size-4" />
              </button>
            </TabsList>

            <div className="relative min-h-0 flex-1">
              <Pane show={rightView === "diff"}>
                <DiffTab
                  active={rightView === "diff"}
                  viewerOpen={viewerPath != null}
                  onDirty={setDiffDirty}
                />
              </Pane>
              <Pane show={rightView === "files"}>
                <FilesTab viewerPath={viewerPath} onOpenFile={setViewerPath} />
              </Pane>
              <Pane show={rightView === "agents"}>
                <AgentsTab
                  active={rightView === "agents"}
                  onFocusAgent={() => switchLeft("herdr")}
                />
              </Pane>
              <Pane show={rightView === "browser"}>
                <BrowserTab />
              </Pane>
              <Pane show={rightView === "terminal"}>
                <TerminalFrame
                  id="shellframe"
                  src="/shell/"
                  title="Terminal (outside herdr)"
                  suppressContext={false}
                  hidden={rightView !== "terminal"}
                />
              </Pane>

              {viewerPath && (
                <React.Suspense fallback={null}>
                  <FileViewer
                    path={viewerPath}
                    onClose={() => setViewerPath(null)}
                  />
                </React.Suspense>
              )}
            </div>
          </Tabs>
        </ResizablePanel>
      </ResizablePanelGroup>
      <Footer />
    </div>
  )
}
