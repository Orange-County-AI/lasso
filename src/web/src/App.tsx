import {
  ChevronLeft,
  ChevronRight,
  Files,
  Globe,
  type LucideIcon,
  NotebookPen,
  Search,
  Settings,
  SquareTerminal,
} from "lucide-react"
import * as React from "react"
import type { Layout, PanelImperativeHandle } from "react-resizable-panels"
import { toast } from "sonner"
import { BrowserTab } from "@/components/BrowserTab"
import { FilesPanel } from "@/components/FilesPanel"
import { GitStatusBadge } from "@/components/GitStatusBadge"
import { HostSwitcher } from "@/components/HostSwitcher"
import { NewDialog, type NewDialogTab } from "@/components/NewDialog"
import { PaneSwitcher } from "@/components/PaneSwitcher"
import { ScratchTab } from "@/components/ScratchTab"
import { SettingsTab, ShortcutsDialog } from "@/components/SettingsTab"
import { TerminalFrame } from "@/components/TerminalFrame"
import { UsageFooter } from "@/components/UsageFooter"
import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
} from "@/components/ui/resizable"
import { Toaster } from "@/components/ui/sonner"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { api } from "@/lib/api"
import { AppProvider, lsGet, lsSet, useApp } from "@/lib/app-store"
import { useDiff } from "@/lib/git"
import { MOBILE_COMMAND_EVENT, type MobileCommand } from "@/lib/mobile-command"
import { syncViewportHeight } from "@/lib/mobile-viewport"
import { restoreHost } from "@/lib/pane-focus"
import { qk, queryClient } from "@/lib/query"
import {
  beginSidebarDrag,
  markSidebarIntent,
  setSidebarPct,
  sidebarIntentFresh,
  sidebarPctNow,
} from "@/lib/sidebar"
import { openHerdrGoto } from "@/lib/terminal"
import { patchUIState, uiStateNow, useUIState } from "@/lib/ui-state"
import { getQueryParam, setQueryParams } from "@/lib/url"
import { cn } from "@/lib/utils"

type RightView = "files" | "scratch" | "browser" | "terminal" | "settings"

// Shared tab-strip styling: a full-width underline strip, matching the original
// vanilla UI rather than shadcn's default pill TabsList.
const stripClass =
  "h-auto w-full justify-start gap-0 rounded-none border-b border-border bg-background p-0"
const tabClass =
  "flex-none rounded-none border-0 border-b-2 border-transparent bg-transparent px-3 py-1.5 text-[13px] text-muted-foreground shadow-none data-[state=active]:border-primary data-[state=active]:bg-transparent data-[state=active]:text-primary data-[state=active]:shadow-none"

type TabDef = {
  value: string
  label: string
  icon: LucideIcon
  badge?: React.ReactNode
  // Extra classes for this tab's trigger. Applied to the hidden measure copy
  // too so the icon-collapse measurement stays in sync.
  className?: string
}

// A tab strip that shows full text labels when they fit, and collapses every
// tab to its icon when the track is too narrow for the labels — rather than
// truncating the last tab to "Settin…" or forcing a horizontal scroll.
function FitTabs({
  tabs,
  trailing,
  listClassName,
}: {
  tabs: TabDef[]
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
      {/* no-scrollbar hides the scrollbar so it doesn't steal row height. */}
      <div
        ref={scrollRef}
        className="no-scrollbar relative flex min-w-0 flex-1 overflow-x-auto"
      >
        {tabs.map(({ value, label, icon: Icon, badge, className }) => (
          <TabsTrigger
            key={value}
            value={value}
            className={cn(tabClass, className)}
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
          {tabs.map(({ value, label, badge, className }) => (
            <span key={value} className={cn(tabClass, className)}>
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

// A search affordance for the header: styled like an input but it's a button
// that fires ⌘K, i.e. herdr's own pane search inside the terminal. It
// fills its centre slot (flex-1, capped at max-w-xs) so it reads as a real
// search bar at every width — the label just truncates when space runs out
// rather than collapsing to a lone icon. The ⌘K hint shows once the strip has
// room for it (container query, `/lnav`).
function HeaderSearch({ onOpen }: { onOpen: () => void }) {
  return (
    <button
      type="button"
      onClick={onOpen}
      title="Search panes (⌘K)"
      className="flex h-7 w-full max-w-xs items-center gap-2 rounded-md border border-border bg-muted/40 px-2.5 text-muted-foreground text-sm hover:border-primary hover:text-foreground"
    >
      <Search className="size-3.5 shrink-0" />
      {/* The bar fills its centre slot (flex-1), so the label rides along in
          whatever space is free and only truncates when the strip is genuinely
          tight — no premature collapse to a lone icon with dead space around it. */}
      <span className="min-w-0 flex-1 truncate text-left">Search</span>
      <kbd className="@min-[520px]/lnav:inline-block hidden rounded border border-border bg-background px-1.5 py-0.5 font-mono text-[11px] text-muted-foreground">
        ⌘K
      </kbd>
    </button>
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

function Shell() {
  const [rightView, setRightView] = React.useState<RightView>("files")
  const [collapsed, setCollapsed] = React.useState(false)
  const [paletteOpen, setPaletteOpen] = React.useState(false)
  const [newOpen, setNewOpen] = React.useState(false)
  const [newTab, setNewTab] = React.useState<NewDialogTab>("agent")
  const [shortcutsOpen, setShortcutsOpen] = React.useState(false)
  const [mobileHostOpen, setMobileHostOpen] = React.useState(false)
  // Git working-tree status, polled app-wide (see useDiff) so the tab badge and
  // the collapsed-sidebar indicator stay live even when the Files panel is
  // hidden or the sidebar boots collapsed. `gitReady` gates the badge on the
  // active pane actually being in a repo — GitStatusBadge renders a green
  // "clean" dot for ready && dirty === 0, which would be a lie for a plain
  // directory like $HOME.
  const diff = useDiff()
  const diffDirty = diff.data?.dirty ?? 0
  const gitReady = diff.data?.isRepo === true
  const rightPanel = React.useRef<PanelImperativeHandle>(null)
  const ui = useUIState()

  // The active host (SSE-driven), mirrored into a ref so the (referentially
  // stable) popstate handler always sees the current one. herdr's focused pane
  // is deliberately NOT part of the URL — see lib/url.
  const { host } = useApp()
  const hostRef = React.useRef(host)
  hostRef.current = host

  const savedLayout = React.useMemo<Layout | undefined>(() => {
    try {
      const v = lsGet("lasso-layout")
      return v ? (JSON.parse(v) as Layout) : undefined
    } catch {
      return undefined
    }
  }, [])

  // Warm the pane list in the background on load so the first ⌘K pane-switcher
  // search is instant instead of waiting on a fresh fetch. Shares qk.panes with
  // the switcher, so both reuse one cache — the fetch spans every host (that
  // aggregation is also what reconciles agent records server-side); the switcher
  // lists the active host's rows out of it.
  React.useEffect(() => {
    void queryClient.prefetchQuery({
      queryKey: qk.panes,
      queryFn: () => api.allPanes(),
    })
  }, [])

  // Keep the app pinned to the space above the mobile keyboard so the terminal's
  // input line never hides behind it (no-op on desktop).
  React.useEffect(syncViewportHeight, [])

  // Clear URL state we no longer honor, once on mount: a legacy #hash
  // (setQueryParams drops the fragment), the ?view= of the retired left tab
  // strip, and any stale ?pane= from a link written before pane focus left the
  // URL — so we never look like we honor a param we ignore. ?host= is lasso's
  // only URL state now, and HostSwitcher owns it.
  React.useEffect(() => {
    setQueryParams({ view: null, pane: null })
  }, [])

  // Back/forward re-points lasso at the host the history entry names. The
  // focused pane is not restored — that is herdr's state, and a browser history
  // step must not re-point it (see lib/pane-focus's restoreHost).
  React.useEffect(() => {
    const onPop = () => {
      const host = getQueryParam("host") ?? "local"
      if (host !== hostRef.current) {
        restoreHost(host).catch((e) =>
          toast.error(`host switch failed: ${(e as Error).message}`)
        )
      }
    }
    window.addEventListener("popstate", onPop)
    return () => window.removeEventListener("popstate", onPop)
  }, [])

  // The sidebar's last open width (% of the group), so expanding restores it
  // rather than snapping to minSize. react-resizable-panels' expand() only
  // remembers the size from this session, so a sidebar that loads collapsed (or
  // whose persisted layout is ~0) would expand thin — we resize() explicitly
  // instead. The width is persisted to localStorage (see lib/sidebar) so it also
  // survives a page reload / lasso restart, not just refreshed as the user drags.
  const expandSidebar = React.useCallback(() => {
    // Prefer the synced width (shared across tabs); fall back to the device-
    // local memory for installs that have never persisted one.
    const synced = uiStateNow().sidebar_pct
    rightPanel.current?.resize(`${synced >= 15 ? synced : sidebarPctNow()}%`)
  }, [])
  const collapseSidebar = React.useCallback(() => {
    const p = rightPanel.current
    if (!p) return
    const s = p.getSize().asPercentage
    if (s > 5) setSidebarPct(s) // capture the true open width before hiding
    p.collapse()
  }, [])
  // The user-driven toggle. It stamps intent, which is what makes this tab the
  // owner of the synced sidebar layout (lib/sidebar, uilock.go) — expand and
  // collapse themselves don't, because the SSE apply effect below calls them
  // too and an applied change must not claim ownership back.
  const toggleSidebar = React.useCallback(() => {
    markSidebarIntent()
    if (rightPanel.current?.isCollapsed()) expandSidebar()
    else collapseSidebar()
  }, [expandSidebar, collapseSidebar])

  // The touch-only terminal dial lives inside the ttyd iframe. Its app branch
  // crosses that same-origin boundary with one typed event; desktop keeps using
  // the visible header and keyboard shortcuts.
  React.useEffect(() => {
    const onMobileCommand = (event: Event) => {
      const command = (event as CustomEvent<MobileCommand>).detail
      if (command === "new") {
        setNewOpen(true)
      } else if (command === "sidebar") {
        toggleSidebar()
      } else if (command === "host") {
        setMobileHostOpen(true)
      } else if (command === "search") {
        // Same destination as ⌘K: herdr's own search. The dial supplies the
        // chord a software keyboard can't type, and openHerdrGoto hands the
        // keyboard to xterm so the query can be typed straight into it.
        openHerdrGoto()
      }
    }
    window.addEventListener(MOBILE_COMMAND_EVENT, onMobileCommand)
    return () =>
      window.removeEventListener(MOBILE_COMMAND_EVENT, onMobileCommand)
  }, [toggleSidebar])

  // ⌘K → herdr's own pane search, ⌘O/⌘I → the agent/terminal tabs in the New dialog,
  // keyboard-shortcuts reference. Bound to the Cmd key only (not Ctrl) so it
  // never clobbers terminal control keys like Ctrl-H (backspace). The
  // herdr/shell terminal iframes re-dispatch Cmd-shortcuts to this document, so
  // these work even while a terminal holds focus. (See SHORTCUTS, the reference
  // list shown in Settings.)
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!e.metaKey || e.ctrlKey || e.altKey || e.shiftKey) return
      const k = e.key.toLowerCase()
      if (k === "\\") {
        e.preventDefault()
        toggleSidebar()
      } else if (k === "k") {
        e.preventDefault()
        openHerdrGoto()
      } else if (k === "o" || k === "i") {
        e.preventDefault()
        setNewTab(k === "o" ? "agent" : "terminal")
        setNewOpen(true)
      } else if (k === "/") {
        e.preventDefault()
        setShortcutsOpen(true)
      }
    }
    document.addEventListener("keydown", onKey)
    return () => document.removeEventListener("keydown", onKey)
  }, [toggleSidebar])

  // Apply the synced sidebar layout continuously — including changes arriving
  // from other tabs over SSE — not just once at load. The sidebar's footprint
  // sets the shared herdr pty's width, so tabs must agree on it or the wider
  // one renders a blank gutter. Value guards make the echo of this tab's own
  // writes a no-op, and remote applies don't re-persist because the debounced
  // persist below compares against the incoming state before writing.
  React.useEffect(() => {
    const p = rightPanel.current
    if (!p) return
    if (ui.sidebar_collapsed !== p.isCollapsed()) {
      if (ui.sidebar_collapsed) collapseSidebar()
      else expandSidebar()
      return // width settles via the expand; next pass reconciles if needed
    }
    if (!ui.sidebar_collapsed && ui.sidebar_pct >= 15) {
      const cur = p.getSize().asPercentage
      if (Math.abs(cur - ui.sidebar_pct) > 1) p.resize(`${ui.sidebar_pct}%`)
    }
  }, [ui.sidebar_collapsed, ui.sidebar_pct, collapseSidebar, expandSidebar])

  // Debounced persist of the sidebar layout: onResize fires for every frame of
  // a drag (and for programmatic applies), so wait for it to settle, then write
  // only what actually differs from the synced state — a remote apply therefore
  // never echoes a write back.
  const layoutPersist = React.useRef<ReturnType<typeof setTimeout> | null>(null)
  const scheduleLayoutPersist = React.useCallback(
    (collapsedNow: boolean, pct: number) => {
      if (layoutPersist.current) clearTimeout(layoutPersist.current)
      layoutPersist.current = setTimeout(() => {
        layoutPersist.current = null
        const cur = uiStateNow()
        const patch: Parameters<typeof patchUIState>[0] = {}
        if (cur.sidebar_collapsed !== collapsedNow)
          patch.sidebar_collapsed = collapsedNow
        if (
          !collapsedNow &&
          pct > 5 &&
          Math.abs((cur.sidebar_pct || 0) - pct) > 1
        )
          patch.sidebar_pct = pct
        // Whether a human in THIS tab caused the size being written. onResize
        // fires identically for a drag, a mount and the apply of a change that
        // arrived over SSE; only the first should take the layout lock.
        if (Object.keys(patch).length > 0)
          patchUIState(patch, sidebarIntentFresh())
      }, 400)
    },
    []
  )

  return (
    <div className="relative flex h-full w-full flex-col">
      <div className="relative min-h-0 flex-1">
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
            {/* The left column's header row. It is not a tab strip: the column
                is always the Herdr terminal, so the row carries the host picker,
                ⌘K search, and the shared New agent/terminal action. Styled with
                the sidebar's strip classes so both columns share one chrome. */}
            <div
              className={cn(
                stripClass,
                "@container/lnav mobile-terminal-nav flex items-center pr-2 text-muted-foreground"
              )}
            >
              <HostSwitcher variant="nav" />
              <div className="flex min-w-0 flex-1 justify-center px-2">
                {/* Arrow, not the bare function: onClick would otherwise hand
                    the click event in as `tries` and kill the retry. */}
                <HeaderSearch onOpen={() => openHerdrGoto()} />
              </div>
              {/* New sits at the far-right of the row; when the sidebar is
                  collapsed the git status + expand control follow it. The
                  dialog hands keyboard focus to a newly created pane. */}
              <div className="ml-2 flex items-center gap-1.5">
                <NewDialog
                  open={newOpen}
                  onOpenChange={setNewOpen}
                  tab={newTab}
                  onTabChange={setNewTab}
                  variant="header"
                />
                {collapsed && (
                  <>
                    {/* Git status at a glance while the file viewer is hidden:
                        the uncommitted-change count (or a green dot when clean),
                        mirroring the Files tab's badge. */}
                    <GitStatusBadge
                      dirty={diffDirty}
                      ready={gitReady}
                      textClassName="self-center text-[13px]"
                    />
                    <button
                      type="button"
                      className="my-1 flex size-6 shrink-0 items-center justify-center self-center rounded border border-border text-muted-foreground hover:border-primary hover:text-primary"
                      title="show file viewer"
                      onClick={() => {
                        markSidebarIntent()
                        expandSidebar()
                      }}
                    >
                      <ChevronLeft className="size-4" />
                    </button>
                  </>
                )}
              </div>
            </div>
            <div className="relative flex min-h-0 flex-1 flex-col">
              <TerminalFrame
                id="term"
                base="/terminal"
                title="Herdr terminal"
                suppressContext
                inputMode="herdr"
                hidden={false}
              />
            </div>
          </ResizablePanel>

          {/* Dragging the handle is a human changing the layout, so it claims
              ownership of the synced width the same way ⌘\ does. Keyboard
              resizing goes through the separator's own key handling, hence both
              listeners. */}
          <ResizableHandle
            withHandle
            onPointerDown={beginSidebarDrag}
            onKeyDown={markSidebarIntent}
            className={cn(collapsed && "hidden", "max-md:hidden")}
          />

          <ResizablePanel
            id="right"
            panelRef={rightPanel}
            defaultSize={40}
            minSize={15}
            collapsible
            collapsedSize={0}
            onResize={(size) => {
              const pct = size.asPercentage
              const c = pct < 0.05
              setCollapsed((prev) => (prev === c ? prev : c))
              // Remember the open width so a later expand restores it (the panel
              // snaps to 0 below minSize, so any non-zero size is a real width).
              if (pct > 5) setSidebarPct(pct)
              scheduleLayoutPersist(c, pct)
            }}
            className={cn(
              "relative flex h-full min-h-0 flex-col border-border border-l bg-card",
              // On phones there isn't room to split the screen, so an open sidebar
              // takes it over entirely: lift it out of the flex flow and overlay the
              // left panel full-screen. Drops back to an in-flow resizable panel at
              // md+. Gated on !collapsed so a collapsed sidebar stays hidden (0-width)
              // rather than overlaying everything.
              !collapsed &&
                "max-md:absolute max-md:inset-0 max-md:z-30 max-md:w-full max-md:border-l-0"
            )}
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
                  {
                    value: "terminal",
                    label: "Terminal",
                    icon: SquareTerminal,
                  },
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
                    title="collapse sidebar"
                    onClick={() => {
                      markSidebarIntent()
                      collapseSidebar()
                    }}
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
                <Pane show={rightView === "terminal"}>
                  <TerminalFrame
                    id="shellframe"
                    base="/shell"
                    title="Terminal (outside herdr)"
                    suppressContext={false}
                    inputMode="shell"
                    hidden={rightView !== "terminal"}
                  />
                </Pane>
                <Pane show={rightView === "settings"}>
                  <SettingsTab
                    active={rightView === "settings"}
                    onOpenShortcuts={() => setShortcutsOpen(true)}
                  />
                </Pane>
              </div>
            </Tabs>
          </ResizablePanel>
        </ResizablePanelGroup>
        <HostSwitcher
          anchorOnly
          className="mobile-host-anchor fixed right-6 bottom-24 z-40"
          open={mobileHostOpen}
          onOpenChange={setMobileHostOpen}
        />
        {/* The MOBILE pane switcher — searches the ACTIVE host's panes, which
          with herdr-mirror running covers the fleet (other machines' workspaces
          are mirrored in as local panes), and focuses the chosen one in the
          herdr terminal. Desktop ⌘K goes to herdr's own search instead; this is
          what the dial's search command opens, since a prefix chord isn't
          reachable from a software keyboard. focusPaneInHerdr still pushes the
          landing host's history entry; same-host now, so pushQueryParam
          collapses it into a replace rather than a dead Back step. */}
        <PaneSwitcher open={paletteOpen} onOpenChange={setPaletteOpen} />
        {/* ⌘? keyboard-shortcuts reference — also opened by the Settings tab's
          keyboard button. Lives here so ⌘? works from any tab. */}
        <ShortcutsDialog open={shortcutsOpen} onOpenChange={setShortcutsOpen} />
      </div>
      <UsageFooter />
    </div>
  )
}
