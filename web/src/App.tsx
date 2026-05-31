import { ChevronLeft, ChevronRight } from "lucide-react"
import * as React from "react"
import type { Layout, PanelImperativeHandle } from "react-resizable-panels"
import { AgentsTab } from "@/components/AgentsTab"
import { BrowserTab } from "@/components/BrowserTab"
import { CreateAgentDialog } from "@/components/CreateAgentDialog"
import { FilesPanel } from "@/components/FilesPanel"
import { HostSwitcher } from "@/components/HostSwitcher"
import { ScratchTab } from "@/components/ScratchTab"
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
import { getQueryParam, setQueryParam } from "@/lib/url"
import { cn } from "@/lib/utils"

type LeftView = "herdr" | "settings"
type RightView = "files" | "scratch" | "browser" | "terminal" | "agents"

const LEFT_VIEWS: LeftView[] = ["herdr", "settings"]

// Shared tab-strip styling: a full-width underline strip, matching the original
// vanilla UI rather than shadcn's default pill TabsList.
const stripClass =
  "h-auto w-full justify-start gap-0 rounded-none border-b border-border bg-background p-0"
const tabClass =
  "flex-none rounded-none border-0 border-b-2 border-transparent bg-transparent px-3 py-1.5 text-[13px] text-muted-foreground shadow-none data-[state=active]:border-primary data-[state=active]:bg-transparent data-[state=active]:text-primary data-[state=active]:shadow-none"

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
    // Prefer ?view=; fall back to a legacy #hash (migrated to a query param on
    // first write below) so old links still land on the right tab.
    const v = getQueryParam("view") ?? location.hash.slice(1)
    return (LEFT_VIEWS as string[]).includes(v) ? (v as LeftView) : "herdr"
  })
  const [rightView, setRightView] = React.useState<RightView>("files")
  const [collapsed, setCollapsed] = React.useState(false)
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

  const switchLeft = React.useCallback((name: LeftView, fromUrl = false) => {
    setLeftView(name)
    if (!fromUrl) setQueryParam("view", name)
  }, [])

  // Reflect the initial tab in the query string once on mount — this also
  // clears any legacy #hash (setQueryParam drops the fragment). The initial
  // value is captured in a ref so the effect needs no reactive deps.
  const initialView = React.useRef(leftView)
  React.useEffect(() => {
    setQueryParam("view", initialView.current)
  }, [])

  // Back/forward drives the active left tab from the URL.
  React.useEffect(() => {
    const onPop = () => {
      const v = getQueryParam("view") ?? ""
      switchLeft(
        (LEFT_VIEWS as string[]).includes(v) ? (v as LeftView) : "herdr",
        true
      )
    }
    window.addEventListener("popstate", onPop)
    return () => window.removeEventListener("popstate", onPop)
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
    <div className="relative h-full w-full">
      <ResizablePanelGroup
        orientation="horizontal"
        defaultLayout={savedLayout}
        onLayoutChanged={(l) => lsSet("lasso-layout", JSON.stringify(l))}
        className="h-full w-full"
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
                  type="button"
                  className="my-1 mr-1 ml-auto flex size-6 shrink-0 items-center justify-center self-center rounded border border-border text-muted-foreground hover:border-primary hover:text-primary"
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
              {/* Tabs scroll horizontally within their own region so the
                  collapse button below stays fixed on the row and always
                  visible. no-scrollbar hides the scrollbar so it doesn't steal
                  vertical space (which would inflate the row height and push
                  the button out of vertical alignment with the tabs). */}
              <div className="no-scrollbar flex min-w-0 flex-1 overflow-x-auto">
                <TabsTrigger value="files" className={tabClass}>
                  Files
                  {diffDirty > 0 && (
                    <span
                      className="ml-1.5 rounded-full bg-warn px-1.5 font-semibold text-[13px] text-background"
                      title={`${diffDirty} uncommitted change${diffDirty === 1 ? "" : "s"}`}
                    >
                      {diffDirty}
                    </span>
                  )}
                </TabsTrigger>
                <TabsTrigger value="scratch" className={tabClass}>
                  Scratch
                </TabsTrigger>
                <TabsTrigger value="browser" className={tabClass}>
                  Browser
                </TabsTrigger>
                <TabsTrigger value="terminal" className={tabClass}>
                  Terminal
                </TabsTrigger>
                <TabsTrigger value="agents" className={tabClass}>
                  Agents
                </TabsTrigger>
              </div>
              <button
                type="button"
                className="ml-2 flex-none self-center rounded border border-border px-1.5 text-muted-foreground hover:border-primary hover:text-primary"
                title="collapse sidebar"
                onClick={() => rightPanel.current?.collapse()}
              >
                <ChevronRight className="size-4" />
              </button>
            </TabsList>

            <div className="relative min-h-0 flex-1">
              <Pane show={rightView === "files"}>
                <FilesPanel
                  active={rightView === "files"}
                  onDirty={setDiffDirty}
                />
              </Pane>
              <Pane show={rightView === "scratch"}>
                <ScratchTab />
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
              <Pane show={rightView === "agents"}>
                <AgentsTab
                  active={rightView === "agents"}
                  onFocusAgent={() => switchLeft("herdr")}
                />
              </Pane>
            </div>
          </Tabs>
        </ResizablePanel>
      </ResizablePanelGroup>
      {leftView === "herdr" && (
        <div className="fixed bottom-3 left-3 z-40 flex items-center gap-2">
          <HostSwitcher />
          <CreateAgentDialog variant="floating" />
        </div>
      )}
    </div>
  )
}
