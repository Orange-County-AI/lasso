import {
  Bot,
  Files,
  Gauge,
  Globe,
  Keyboard,
  type LucideIcon,
  MessageSquare,
  NotebookPen,
  PanelLeft,
  PanelLeftClose,
  PanelLeftOpen,
  PanelRightClose,
  PanelRightOpen,
  Plus,
  Server,
  Settings,
  SquareTerminal,
  Users,
} from "lucide-react"
import * as React from "react"
import type { Layout, PanelImperativeHandle } from "react-resizable-panels"
import { toast } from "sonner"
import { AgentSidebar } from "@/components/AgentSidebar"
import { AgentsTab } from "@/components/AgentsTab"
import { AgentsView } from "@/components/AgentsView"
import { BrowserTab } from "@/components/BrowserTab"
import { ChatView } from "@/components/ChatView"
import { FilesPanel } from "@/components/FilesPanel"
import { GitStatusBadge } from "@/components/GitStatusBadge"
import { HostSwitcher } from "@/components/HostSwitcher"
import { NewDialog, type NewDialogTab } from "@/components/NewDialog"
import { ScratchTab } from "@/components/ScratchTab"
import { SettingsTab, ShortcutsDialog } from "@/components/SettingsTab"
import { TerminalFrame } from "@/components/TerminalFrame"
import { UsageFooter } from "@/components/UsageFooter"
import { UsageTab } from "@/components/UsageTab"
import { Button } from "@/components/ui/button"
import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
} from "@/components/ui/resizable"
import { Toaster } from "@/components/ui/sonner"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { AppProvider, lsGet, lsSet, useApp } from "@/lib/app-store"
import { useDiff } from "@/lib/git"
import { MOBILE_COMMAND_EVENT, type MobileCommand } from "@/lib/mobile-command"
import { syncViewportHeight } from "@/lib/mobile-viewport"
import { restoreHost } from "@/lib/pane-focus"
import {
  beginSidebarDrag,
  markSidebarIntent,
  setSidebarPct,
  sidebarIntentFresh,
  sidebarPctNow,
} from "@/lib/sidebar"
import {
  blurHerdrTerminal,
  focusHerdrTerminal,
  openHerdrGoto,
  toggleHerdrSidebar,
} from "@/lib/terminal"
import { patchUIState, uiStateNow, useUIState } from "@/lib/ui-state"
import { getQueryParam, setQueryParams } from "@/lib/url"
import { cn } from "@/lib/utils"

type RightView =
  | "files"
  | "scratch"
  | "browser"
  | "terminal"
  | "usage"
  | "settings"
  // Below md only: the fleet's agents, which at md+ is the docked column beside
  // the chat instead (AgentsTab, AgentSidebar).
  | "agents"
// The left column's faces: the terminal, the focused session read as a
// conversation (ChatView), and the fleet as parallel conversations (AgentsView).
// Not a RightView — the sidebar and the left column answer different questions,
// and both chats are about panes, not files.
type LeftView = "terminal" | "chat" | "agents"

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
  listClassName,
  trailing,
}: {
  tabs: TabDef[]
  listClassName?: string
  // Pinned to the right of the strip, outside the scrolling track so it stays
  // reachable however many tabs there are.
  trailing?: React.ReactNode
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
  // Which face of the focused pane the left column shows: the herdr terminal,
  // or the agent session as a conversation. The terminal iframe stays MOUNTED
  // and sized underneath either way — the chat is an overlay, not a swap — so
  // the shared herdr pty keeps its width (and every other pane keeps its
  // layout) while the chat is up, exactly as the mobile sidebar overlay does.
  const [leftView, setLeftView] = React.useState<LeftView>("terminal")
  // The chat's own left sidebar: the host's agents, beside the conversation.
  // Per-TAB, like leftView itself and unlike the right sidebar's synced layout —
  // it is a property of this reading surface, not a shape the shared pty is
  // measured against (it takes no width from it: the chat is an overlay over a
  // terminal that stays sized, so opening this cannot reflow every other pane).
  const [chatSidebar, setChatSidebar] = React.useState(false)
  const toggleLeftView = React.useCallback(() => {
    // From the grid the Chat button means the single conversation; otherwise a
    // straight terminal <-> chat flip.
    const next = leftView === "chat" ? "terminal" : "chat"
    setLeftView(next)
    if (next === "chat") {
      // The composer is the input surface now, so focus LEAVES the terminal —
      // the dial hands it back on its way out, and a view opened to be READ
      // must not pop a keyboard over itself. xterm holding focus behind the
      // chat is also what sent a desktop's keystrokes into a terminal nobody
      // was looking at.
      blurHerdrTerminal()
      return
    }
    // The other way hands focus back, but only where there is a hardware
    // keyboard to use it: focusing xterm on a phone raises the on-screen
    // keyboard over a terminal the reader had not asked to type into yet. They
    // type through the dial, which focuses itself when it is used.
    if (!window.matchMedia("(pointer: coarse)").matches) focusHerdrTerminal()
  }, [leftView])
  // The grid holds a composer per card, so entering it blurs the terminal for
  // the same reason the single chat does; leaving for the terminal refocuses.
  const toggleAgentsView = React.useCallback(() => {
    const next = leftView === "agents" ? "terminal" : "agents"
    setLeftView(next)
    if (next === "agents") blurHerdrTerminal()
    else if (!window.matchMedia("(pointer: coarse)").matches)
      focusHerdrTerminal()
  }, [leftView])
  // The footer's left-hand toggle, whose subject is the mode it sits in. In the
  // terminal it is herdr's OWN sidebar — a chord into the TUI, because herdr's
  // socket API has no method for it and reports no state for it either, so the
  // button is an action rather than an indicator. In the chat it is lasso's agent
  // list, whose state this tab does own.
  const toggleLeftSidebar = React.useCallback(() => {
    if (leftView === "chat") setChatSidebar((open) => !open)
    else toggleHerdrSidebar()
  }, [leftView])
  const [collapsed, setCollapsed] = React.useState(false)
  const [newOpen, setNewOpen] = React.useState(false)
  const [newTab, setNewTab] = React.useState<NewDialogTab>("agent")
  const [shortcutsOpen, setShortcutsOpen] = React.useState(false)
  const [hostMenuOpen, setHostMenuOpen] = React.useState(false)
  // Keep the Files tab's git badge live even while another sidebar tab is
  // selected — the footer shows the same badge while the sidebar is collapsed.
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
  // The chat's own way in, for the widths where nothing else reaches the right
  // sidebar: the footer is md+ and the input dial's `sidebar` command lives
  // inside the terminal iframe the chat overlays. An OPEN rather than a toggle —
  // below md the panel covers the header the menu item sits in, so it can only
  // ever be invoked against a collapsed one — and it stamps intent like ⌘\,
  // being a human moving the layout.
  const openSidebar = React.useCallback(() => {
    markSidebarIntent()
    expandSidebar()
  }, [expandSidebar])

  // Footer navigation. New always opens on the agent tab — the terminal tab is
  // ⌘I's business — and the mobile dial's "new" command shares this.
  const openNew = React.useCallback(() => {
    setNewTab("agent")
    setNewOpen(true)
  }, [])

  // Collapsing the sidebar hands the screen back to the terminal, so hand it
  // the keyboard too rather than leaving focus parked on the footer button.
  // Expanding leaves focus where it is — the user is looking at what they just
  // opened, and stealing it would type their next keystroke into herdr.
  const toggleSidebarFromFooter = React.useCallback(() => {
    const wasOpen = rightPanel.current?.isCollapsed() === false
    toggleSidebar()
    if (wasOpen) focusHerdrTerminal()
  }, [toggleSidebar])

  // Below md there is no footer — it's md+ only — and an open sidebar covers
  // the whole screen there, so the tab strip's ✕ is the ONLY pointer route back
  // (the input dial is behind that overlay, with its sidebar command).
  // Same hand-off as the footer toggle: closing gives the terminal the keyboard
  // — but only when the terminal is what it uncovers. Over the chat or the
  // agents grid the iframe is hidden behind an overlay, so focusing it would pop
  // a phone's keyboard for a surface nobody can see and aim the next keystrokes
  // at herdr instead of the composer (the same reason entering the chat blurs
  // it).
  const closeSidebar = React.useCallback(() => {
    markSidebarIntent()
    collapseSidebar()
    if (leftView === "terminal") focusHerdrTerminal()
  }, [collapseSidebar, leftView])

  // Picking an agent from the sidebar's Agents tab: close the panel — below md it
  // covers the very view that answers the tap — and land on that agent's
  // CONVERSATION, never the terminal. The list is one row per conversation, so a
  // pick is a request to read one; a phone dropped on the terminal instead would
  // answer it with a pane you cannot scroll back through or type into without the
  // dial. The keyboard is deliberately NOT handed to the terminal on the way (the
  // ✕ does that): focusing xterm here would pop a phone's on-screen keyboard over
  // a session nobody has asked to type into yet, and aim it at herdr rather than
  // at the composer.
  const pickAgent = React.useCallback(() => {
    markSidebarIntent()
    collapseSidebar()
    setLeftView("chat")
    blurHerdrTerminal()
  }, [collapseSidebar])

  // The Agents tab is md:hidden — at md+ the same list is the chat's docked
  // column — so a window widened across the breakpoint while it is selected would
  // leave a pane showing with no lit trigger above it. Fall back to Files, which
  // is where the strip starts.
  React.useEffect(() => {
    if (rightView !== "agents") return
    const mq = window.matchMedia("(min-width: 768px)")
    const check = () => {
      if (mq.matches) setRightView("files")
    }
    check()
    mq.addEventListener("change", check)
    return () => mq.removeEventListener("change", check)
  }, [rightView])

  // The footer button is separate from the menu's mobile-capable anchor.
  // Capture its pointer-down state before Radix's outside-click dismissal, so
  // that same click toggles closed rather than reopening the menu.
  const hostOpenAtPointerDown = React.useRef<boolean | null>(null)
  const openHostMenu = React.useCallback(() => setHostMenuOpen(true), [])
  const toggleHostMenu = React.useCallback(() => {
    const wasOpen = hostOpenAtPointerDown.current
    hostOpenAtPointerDown.current = null
    setHostMenuOpen((current) => !(wasOpen ?? current))
  }, [])

  React.useEffect(() => {
    const onMobileCommand = (event: Event) => {
      const command = (event as CustomEvent<MobileCommand>).detail
      if (command === "new") {
        openNew()
      } else if (command === "sidebar") {
        toggleSidebar()
      } else if (command === "host") {
        openHostMenu()
      } else if (command === "chat") {
        // The way into the chat wherever the footer that carries this control
        // is hidden, which is every width below md — phone or a desktop window
        // dragged narrow. The command comes from the dedicated button the dial
        // holds above its root, not from an arc target (lib/mobile-input-dial).
        toggleLeftView()
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
  }, [toggleSidebar, openNew, openHostMenu, toggleLeftView])

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
        // ⌘I asks for a TERMINAL, and chat mode's creator has no terminal to
        // offer (see NewDialog's agentsOnly) — so it hands the screen back
        // first, rather than opening an agent form in answer to a terminal
        // shortcut. ⌘O stays where it is: a new agent is exactly what the chat
        // is for, and its dialog is pinned to agents there anyway.
        if (k === "i") setLeftView("terminal")
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
            {/* term-shell is the hook the Retro 82 atmosphere draws its
                amber/teal hairline on (index.css); inert under every other
                theme. */}
            <div className="term-shell relative isolate flex min-h-0 flex-1 flex-col">
              <TerminalFrame
                id="term"
                base="/terminal"
                title="Herdr terminal"
                suppressContext
                inputMode="herdr"
                hidden={false}
              />
              {/* The chat covers the terminal without unmounting it. That is
                  what keeps the shared pty's size (a hidden iframe would refit
                  it to nothing and reflow every other pane), and it is also
                  what "replaces" the mobile input dial: the dial lives inside
                  that iframe's document, so an overlay puts it out of both
                  sight and reach while the chat's own composer is the way to
                  type. */}
              {leftView === "chat" && (
                <div className="chat-overlay absolute inset-0 z-20 flex">
                  {/* The agent list TAKES width from the chat, not from the
                      terminal: the overlay is the only thing that grew, so the
                      iframe underneath keeps the size the shared pty was fitted
                      to and no other client's herdr reflows. */}
                  {chatSidebar && <AgentSidebar />}
                  <ChatView
                    className="min-w-0 flex-1"
                    onShowTerminal={() => setLeftView("terminal")}
                    onShowSidebar={openSidebar}
                  />
                </div>
              )}
              {/* The grid overlays for the same reason the single chat does: the
                  terminal stays mounted and sized underneath, so the shared pty
                  keeps its width while N transcripts (not N terminals) are read
                  above it. */}
              {leftView === "agents" && (
                <div className="chat-overlay absolute inset-0 z-20 flex">
                  <AgentsView
                    className="min-w-0 flex-1"
                    onNewAgent={openNew}
                    onShowChat={() => setLeftView("chat")}
                  />
                </div>
              )}
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
              // sidebar-panel: the atmosphere's translucency opt-out at phone
              // widths, where this panel covers the terminal (see index.css).
              "sidebar-panel relative flex h-full min-h-0 flex-col border-border border-l bg-card",
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
                  // Agents leads, and only below md: this panel IS a phone's
                  // chrome, so the list of what you could be reading belongs
                  // before the files you are reading. At md+ the chat's docked
                  // column is the same list and this tab is not rendered.
                  {
                    value: "agents",
                    label: "Agents",
                    icon: Bot,
                    className: "md:hidden",
                  },
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
                  { value: "usage", label: "Usage", icon: Gauge },
                  { value: "settings", label: "Settings", icon: Settings },
                ]}
                trailing={
                  <>
                    {/* Scoped to the Agents tab: it makes an agent, which is what
                        that tab is a list of. Ending one is NOT here — it belongs
                        to the row whose pane it closes (AgentsTab), where the
                        question is about something on screen and a ✕ in the strip
                        cannot be mistaken for it. */}
                    {rightView === "agents" && (
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        className="ml-1 flex-none self-center md:hidden"
                        title="New agent"
                        aria-label="New agent"
                        onClick={openNew}
                      >
                        <Plus />
                      </Button>
                    )}
                    {/* The mirror of the chat header's PanelRightOpen, not a bare
                        ✕: this closes the PANEL, and next to a row's close-pane
                        control a plain cross reads as the same kind of thing. The
                        pair of glyphs says which one ends a session. */}
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      className="mr-1 ml-1 flex-none self-center md:hidden"
                      title="Close sidebar"
                      aria-label="Close sidebar"
                      onClick={closeSidebar}
                    >
                      <PanelRightClose />
                    </Button>
                  </>
                }
              />

              <div className="relative min-h-0 flex-1">
                <Pane show={rightView === "agents"}>
                  <AgentsTab onPick={pickAgent} />
                </Pane>
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
                <Pane show={rightView === "usage"}>
                  <UsageTab active={rightView === "usage"} />
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
        {/* Match the footer's left-hand Host control on desktop; keep the
          mobile input dial's host menu anchored at the terminal's right edge. */}
        <HostSwitcher
          className="absolute right-8 bottom-1 z-40 md:right-auto md:left-2"
          open={hostMenuOpen}
          onOpenChange={setHostMenuOpen}
        />
        <NewDialog
          open={newOpen}
          onOpenChange={setNewOpen}
          tab={newTab}
          onTabChange={setNewTab}
          // Both chats' creator is agents-only (see the dialog): the footer's New
          // and the sidebar's Agents tab both land here, so the entry points
          // cannot disagree about what a reading view can make. The Agents tab
          // counts whatever the left column shows — its button says "New agent",
          // and below md (the only width that tab exists at) it is the only New
          // on screen. Derived rather than latched because the modal blocks the
          // page: nothing can change the view out from under it.
          agentsOnly={leftView !== "terminal" || rightView === "agents"}
        />
        {/* ⌘? keyboard-shortcuts reference — also opened by the Settings tab's
          keyboard button. Lives here so ⌘? works from any tab. */}
        <ShortcutsDialog open={shortcutsOpen} onOpenChange={setShortcutsOpen} />
      </div>
      {/* The app's only chrome. There is no header and no floating navigation,
        so this footer is always present at desktop widths and has no visibility
        toggle: it is the only pointer route to New, both sidebars, the host menu
        and the shortcuts reference. Usage metrics scroll inside their own track
        so a long provider list can never push the controls offscreen. Below md
        it is gone — a phone keeps the whole viewport for the terminal, and a
        desktop window dragged that narrow has no room for the row either — and
        the input dial beside xterm's textarea carries the same commands at that
        width, mouse or finger (see lib/mobile-input-dial). */}
      <footer className="hidden flex-none items-center gap-2 border-border border-t bg-card px-2 py-1 md:flex">
        <div className="flex flex-none items-center gap-1">
          {/* The list on the LEFT of the terminal column, which is a different
              thing in each view: herdr's own sidebar in the terminal, the host's
              agents in the chat (see toggleLeftSidebar). Disabled in the agents
              grid, which has no docked column and covers the terminal the chord
              would move. The chat's state is this tab's, so only there does the
              button carry a pressed state — herdr's sidebar reports nothing to
              press.
              It comes first in this row because the column it moves is the one
              nearest the edge — a control for the leftmost thing reads first
              left-to-right — and it keeps the corner-to-corner symmetry with the
              Sidebar toggle that closes the row on the right. */}
          <Button
            variant="ghost"
            size="icon-sm"
            title={
              leftView === "agents"
                ? "No sidebar in the agents grid"
                : leftView === "chat"
                  ? chatSidebar
                    ? "Hide agents"
                    : "Show agents"
                  : "Toggle herdr sidebar"
            }
            aria-label={
              leftView === "agents"
                ? "No sidebar in the agents grid"
                : leftView === "chat"
                  ? chatSidebar
                    ? "Hide agents"
                    : "Show agents"
                  : "Toggle herdr sidebar"
            }
            aria-pressed={leftView === "chat" ? chatSidebar : undefined}
            // The grid has no docked column and covers the terminal, so a chord
            // into herdr's sidebar would move something nobody can see. Off.
            disabled={leftView === "agents"}
            onClick={toggleLeftSidebar}
          >
            {leftView === "chat" ? (
              chatSidebar ? (
                <PanelLeftClose />
              ) : (
                <PanelLeftOpen />
              )
            ) : (
              <PanelLeft />
            )}
          </Button>
          <Button
            variant="ghost"
            size="icon-sm"
            title="Switch host"
            aria-label="Switch host"
            aria-haspopup="menu"
            aria-expanded={hostMenuOpen}
            onPointerDownCapture={() => {
              hostOpenAtPointerDown.current = hostMenuOpen
            }}
            onPointerCancel={() => {
              hostOpenAtPointerDown.current = null
            }}
            onKeyDownCapture={() => {
              hostOpenAtPointerDown.current = null
            }}
            onClick={toggleHostMenu}
          >
            <Server />
          </Button>
          <Button
            variant="ghost"
            size="icon-sm"
            title="Keyboard shortcuts (⌘/)"
            aria-label="Keyboard shortcuts"
            onClick={() => setShortcutsOpen(true)}
          >
            <Keyboard />
          </Button>
        </div>
        <UsageFooter />
        <div className="ml-auto flex flex-none items-center gap-1">
          {/* The parallel view: every agent as its own transcript card, grouped
              by machine, for interfacing with several at once. Sits left of Chat
              because it is the wider sibling — grid first, single second — and
              the footer is md+ only, which is exactly the tablet-and-larger
              surface this is for. */}
          <Button
            variant="ghost"
            size="sm"
            aria-pressed={leftView === "agents"}
            title={
              leftView === "agents"
                ? "Back to the terminal"
                : "All agents in parallel"
            }
            onClick={toggleAgentsView}
          >
            {leftView === "agents" ? <SquareTerminal /> : <Users />}
            {leftView === "agents" ? "Terminal" : "Agents"}
          </Button>
          {/* The label names where it goes, not where you are: one glance says
              what the click does. Below md this row is gone and the way in is
              the input dial's Chat button instead. */}
          <Button
            variant="ghost"
            size="sm"
            aria-pressed={leftView === "chat"}
            title={
              leftView === "chat"
                ? "Back to the terminal"
                : "Read this session as chat"
            }
            onClick={toggleLeftView}
          >
            {leftView === "chat" ? <SquareTerminal /> : <MessageSquare />}
            {leftView === "chat" ? "Terminal" : "Chat"}
          </Button>
          <Button
            variant="ghost"
            size="sm"
            title="New agent or terminal (⌘O / ⌘I)"
            onClick={openNew}
          >
            <Plus />
            New
          </Button>
          <Button
            variant="ghost"
            size="sm"
            aria-pressed={!collapsed}
            title="Toggle sidebar (⌘\)"
            onClick={toggleSidebarFromFooter}
          >
            {collapsed ? <PanelRightOpen /> : <PanelRightClose />}
            Sidebar
            {/* With the sidebar closed its Files tab's badge is offscreen, so
              the working tree's state shows here instead — the same round
              indicator the retired header carried. */}
            {collapsed && (
              <GitStatusBadge
                dirty={diffDirty}
                ready={gitReady}
                className="ml-0.5"
                textClassName="text-[11px]"
              />
            )}
          </Button>
        </div>
      </footer>
    </div>
  )
}
