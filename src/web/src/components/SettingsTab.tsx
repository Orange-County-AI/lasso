import {
  keepPreviousData,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query"
import {
  ChevronDown,
  ChevronRight,
  ChevronUp,
  Copy,
  Download,
  ExternalLink,
  Eye,
  EyeOff,
  Keyboard,
  Monitor,
  Moon,
  Palette,
  RotateCw,
  Sun,
  Upload,
  X,
} from "lucide-react"
import * as React from "react"
import { toast } from "sonner"
import { Pill } from "@/components/Pill"
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
import { Checkbox } from "@/components/ui/checkbox"
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { NO_AUTOCORRECT } from "@/components/ui/input"
import { Orb } from "@/components/ui/orb"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { SCRATCH_WORKSPACE } from "@/lib/agents"
import {
  ApiError,
  api,
  type BrowserAction,
  type BrowserMode,
  completeUsageProviderOrder,
  type Plugin,
  type PluginAction,
  type PluginMCPStatus,
  type PluginPermissions,
  type PluginPreview,
  type PluginState,
  type SidebarTabPref,
  type ThemeCatalogEntry,
  type ThemePayload,
  type TypographySlot,
} from "@/lib/api"
import { lsGet, lsSet, useApp } from "@/lib/app-store"
import { cdpURL } from "@/lib/cdp"
import {
  fleetThemeIsPalette,
  getMode,
  getPalettePref,
  localPaletteName,
  type Mode,
  resolvedMode,
  setMode,
  setPalettePref,
  subscribeSystemScheme,
  systemPrefersDark,
} from "@/lib/mode"
import {
  githubCommitURL,
  pluginSourceOf,
  pluginTabsOf,
  shortCommit,
  usePlugins,
} from "@/lib/plugins"
import {
  disablePush,
  enablePush,
  type PushState,
  readPushState,
} from "@/lib/push"
import { qk } from "@/lib/query"
import { SHORTCUT_GROUPS } from "@/lib/shortcuts"
import {
  arrangeTabs,
  BUILTIN_LABELS,
  isBuiltinTab,
  moveTab,
  resolveSidebarTabs,
  setTabHidden,
} from "@/lib/sidebar-tabs"
import { primeThemeCatalog, refreshTheme, shippedPairs } from "@/lib/theme"
import {
  fontForSlot,
  type ResolvedFont,
  resolvePluginFonts,
  SLOT_LABELS,
  setTypography,
  slotAccepts,
  TYPOGRAPHY_SLOTS,
} from "@/lib/typography"
import { patchUIState, useUIState } from "@/lib/ui-state"
import { cn } from "@/lib/utils"
import {
  backgroundFor,
  DEFAULT_SCRIM,
  forgetBackground,
  getScrim,
  getShading,
  NO_BACKGROUND,
  rememberBackground,
  type ShippedBackground,
  setScrim,
  setShading,
  setThemeBackground,
  themeBackgrounds,
} from "@/lib/wallpaper"

// Native textarea/select styled to match the shadcn <Input>.
const fieldClass =
  "w-full rounded-lg border border-input bg-transparent px-2.5 py-1.5 text-sm shadow-well outline-none transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 dark:bg-input/30"
const labelClass = "font-medium text-muted-foreground text-xs"

function Field({
  label,
  hint,
  htmlFor,
  children,
}: {
  label: string
  hint?: string
  htmlFor?: string
  children: React.ReactNode
}) {
  return (
    <div className="flex flex-col gap-1">
      <label className={labelClass} htmlFor={htmlFor}>
        {label}
      </label>
      {children}
      {hint && <p className="text-[11px] text-muted-foreground">{hint}</p>}
    </div>
  )
}

type SaveState = "idle" | "saving" | "saved" | "error"

// Debounced autosave: fires `save` `delay`ms after the watched fields last
// changed, but only while `dirty`. Returns `flush` to save immediately (on blur).
function useDebouncedSave(
  dirty: boolean,
  save: () => void,
  deps: React.DependencyList,
  delay = 600
) {
  const timer = React.useRef<ReturnType<typeof setTimeout> | undefined>(
    undefined
  )
  const saveRef = React.useRef(save)
  saveRef.current = save
  const dirtyRef = React.useRef(dirty)
  dirtyRef.current = dirty
  React.useEffect(() => {
    if (!dirty) return
    timer.current = setTimeout(() => saveRef.current(), delay)
    return () => clearTimeout(timer.current)
  }, [dirty, delay, ...deps])
  return React.useCallback(() => {
    clearTimeout(timer.current)
    if (dirtyRef.current) saveRef.current()
  }, [])
}

// Replaces the old explicit Save button: a quiet inline status that reflects the
// autosave lifecycle, with a retry affordance when a save fails.
function SaveStatus({
  state,
  onRetry,
}: {
  state: SaveState
  onRetry: () => void
}) {
  if (state === "idle") return null
  if (state === "saving")
    return <span className="text-[11px] text-muted-foreground">Saving…</span>
  if (state === "saved")
    return <span className="text-[11px] text-muted-foreground">Saved ✓</span>
  return (
    <span className="text-[11px] text-destructive">
      Couldn't save —{" "}
      <button
        type="button"
        className="underline hover:no-underline"
        onClick={onRetry}
      >
        retry
      </button>
    </span>
  )
}

// Which settings groups are open, per BROWSER rather than in ui_state: it is a
// reading convenience for the screen in front of someone, and syncing it would
// have a phone collapsing a group fold it on the desktop too. One key holding
// the whole map, and an absent entry means the group's own default — so a
// browser whose storage is blocked or cleared simply renders the defaults.
const GROUPS_KEY = "lasso-settings-groups"
function readGroups(): Record<string, boolean> {
  try {
    const v: unknown = JSON.parse(lsGet(GROUPS_KEY) ?? "{}")
    return v && typeof v === "object" && !Array.isArray(v)
      ? (v as Record<string, boolean>)
      : {}
  } catch {
    return {}
  }
}
function useGroupOpen(id: string, defaultOpen: boolean) {
  const [open, setOpen] = React.useState(() => {
    const v = readGroups()[id]
    return typeof v === "boolean" ? v : defaultOpen
  })
  const change = React.useCallback(
    (next: boolean) => {
      setOpen(next)
      // Read-modify-write rather than a copy held in state: another group (or
      // another tab of this browser) may have written its entry since.
      lsSet(GROUPS_KEY, JSON.stringify({ ...readGroups(), [id]: next }))
    },
    [id]
  )
  return [open, change] as const
}

// SettingsGroup folds a run of related sections under one header, so the pane
// reads as a table of contents on a phone instead of a scroll through every
// knob. A collapsed header carries a one-line summary of what is inside, which
// is also where anything needing attention stays visible (a plugin awaiting
// approval) — groups never open themselves, since that would fight whoever
// just closed one.
//
// `keepMounted` is for groups holding state that must not be dropped by a
// collapse — a draft, an upload in flight, a scrolled gallery — which then stay
// mounted and only hide. Everything else unmounts. `children` may take the
// open flag, so a section that polls while `active` can be handed
// `active && open` and a collapsed group costs nothing.
function SettingsGroup({
  id,
  title,
  defaultOpen = false,
  summary,
  keepMounted = false,
  children,
}: {
  id: string
  title: string
  defaultOpen?: boolean
  summary?: React.ReactNode
  keepMounted?: boolean
  children: React.ReactNode | ((open: boolean) => React.ReactNode)
}) {
  const [open, setOpen] = useGroupOpen(id, defaultOpen)
  return (
    <Collapsible
      open={open}
      onOpenChange={setOpen}
      className="border-border/60 border-t first:border-t-0"
    >
      <CollapsibleTrigger className="-mx-1 flex min-h-9 w-[calc(100%+0.5rem)] items-center gap-2 rounded-md px-1 py-2 text-left outline-none transition-colors hover:bg-muted/40 focus-visible:ring-3 focus-visible:ring-ring/50">
        <ChevronRight
          aria-hidden
          className={cn(
            "size-3.5 flex-none text-muted-foreground transition-transform",
            open && "rotate-90"
          )}
        />
        <span className="flex-none font-medium text-[13px] text-foreground">
          {title}
        </span>
        {!open && summary && (
          <span className="min-w-0 flex-1 truncate text-[11px] text-muted-foreground">
            {summary}
          </span>
        )}
      </CollapsibleTrigger>
      <CollapsibleContent
        forceMount={keepMounted ? true : undefined}
        className="pt-1 pb-1 data-[state=closed]:hidden"
      >
        {typeof children === "function" ? children(open) : children}
      </CollapsibleContent>
    </Collapsible>
  )
}

// The Settings tab, in two panes. General is lasso↔herdr socket-protocol
// compatibility (top) plus the "New Agent" creator configuration: lasso targets
// a fixed protocol (baked in at build time), the daemon reports its own over the
// socket, and when they drift terminals and RPC silently break, so we surface it
// here. The creator defaults (where to scan for repos, the default agent, the
// scratch setup script) are global; each repo's files-to-copy + setup commands
// are scoped to the active host. All of it persists in ~/.lasso/lasso.db.
//
// Themes is everything about how lasso and herdr LOOK — the palette, its
// backdrop, and which of those choices are lasso's own rather than herdr's
// (see ThemesSettings). It is its own pane because half of it is a gallery of
// pictures: inline with the creator settings it pushed everything else off the
// screen.
type SettingsSub = "general" | "themes"
const SUB_KEY = "lasso-settings-sub"
export function SettingsTab({
  active,
  onOpenShortcuts,
}: {
  active: boolean
  onOpenShortcuts: () => void
}) {
  const versionQuery = useQuery({
    queryKey: qk.version,
    queryFn: () => api.version(),
    enabled: active,
  })
  const info = versionQuery.data ?? null
  const loading = versionQuery.isLoading
  const errored = versionQuery.isError

  // Which pane is showing, remembered per browser like the right-hand view
  // itself: someone iterating on a theme should not have to re-find it.
  const [sub, setSub] = React.useState<SettingsSub>(() =>
    lsGet(SUB_KEY) === "themes" ? "themes" : "general"
  )
  React.useEffect(() => {
    lsSet(SUB_KEY, sub)
  }, [sub])

  // Which host's settings to edit — each host stores them in its own lasso.db.
  // The picker lists the local machine plus every reachable, compatible remote
  // (those can answer `lasso cli` over SSH). Defaults to the active host.
  const { host: activeHost } = useApp()
  const hostsQuery = useQuery({
    queryKey: ["hosts"],
    queryFn: () => api.hosts(),
    enabled: active,
  })
  const hostOptions = React.useMemo(() => {
    const d = hostsQuery.data
    const opts = [{ value: "local", label: d?.local?.hostname || "local" }]
    for (const h of d?.hosts ?? []) {
      if (h.reachable && h.running && h.compatible)
        opts.push({ value: h.alias, label: h.alias })
    }
    return opts
  }, [hostsQuery.data])
  const [selectedHost, setSelectedHost] = React.useState<string | null>(null)
  // Default to the active host once it's known; keep the user's choice after.
  React.useEffect(() => {
    if (selectedHost == null && activeHost) setSelectedHost(activeHost)
  }, [activeHost, selectedHost])
  const host = selectedHost ?? activeHost ?? "local"
  const generalActive = active && sub === "general"

  // The herdr-side pill: the daemon's protocol and how it compares to lasso's.
  let herdr: React.ReactNode
  if (loading) {
    herdr = <Pill>herdr …</Pill>
  } else if (errored || !info) {
    herdr = <Pill tone="warn">herdr unavailable</Pill>
  } else if (info.err) {
    herdr = (
      <Pill tone="warn" title={info.err}>
        herdr unreachable
      </Pill>
    )
  } else {
    const ver = info.herdr_version ? ` (${info.herdr_version})` : ""
    herdr = (
      <Pill tone={info.compatible ? "good" : "bad"} multiline>
        herdr protocol {info.herdr_protocol}
        {ver} · {info.compatible ? "compatible" : "incompatible"}
      </Pill>
    )
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <Tabs
        value={sub}
        onValueChange={(v) => setSub(v as SettingsSub)}
        className="@container min-h-0 flex-1 gap-0 overflow-hidden"
      >
        <TabsList className="mx-3 mt-3 grid w-auto grid-cols-2">
          <TabsTrigger value="general">General</TabsTrigger>
          <TabsTrigger value="themes">
            <Palette className="size-3.5" />
            Themes
          </TabsTrigger>
        </TabsList>
        {/* Both panes stay mounted so the theme queries and the gallery's
            scroll position survive a hop to General and back — and so a
            background pick is never interrupted by a remount. */}
        <TabsContent
          value="general"
          forceMount
          className="min-h-0 overflow-y-auto px-3 py-4 data-[state=inactive]:hidden"
        >
          {/* Grouped so a phone opens on a short list of headers. Agents is
              kept mounted: CreationSettings is a set of autosaving drafts
              with a save status, and the host picker's choice lives here. */}
          <SettingsGroup
            id="agents"
            title="Agents"
            keepMounted
            summary={<AgentsSummary active={generalActive} />}
          >
            {(open) => (
              <>
                <AutoTitleToggle active={generalActive && open} />
                <CreatorHostSetting hostOptions={hostOptions} />
                <div className="mb-4 flex flex-col gap-1">
                  <label className={labelClass} htmlFor="settings-host">
                    Configuring host
                  </label>
                  <select
                    id="settings-host"
                    className={cn(fieldClass, "max-w-xs")}
                    value={host}
                    onChange={(e) => setSelectedHost(e.target.value)}
                  >
                    {/* Ensure the current value is always selectable even before the
                        host probe returns (e.g. an active remote not yet in the list). */}
                    {!hostOptions.some((o) => o.value === host) && (
                      <option value={host}>{host}</option>
                    )}
                    {hostOptions.map((o) => (
                      <option key={o.value} value={o.value}>
                        {o.label}
                        {o.value === activeHost ? " (active)" : ""}
                      </option>
                    ))}
                  </select>
                  <p className="text-[11px] text-muted-foreground">
                    These settings live in {host}'s own ~/.lasso/lasso.db.
                  </p>
                </div>
                <CreationSettings active={generalActive && open} host={host} />
              </>
            )}
          </SettingsGroup>
          {/* Kept mounted for the proxy field, which is a draft while focused
              and reports a refused value inline — a collapse must not eat the
              error. Its status query runs either way (the Browser tab shares
              it); only the 5s poll stops while the group is closed. */}
          <SettingsGroup
            id="terminal-browser"
            title="Terminal & browser"
            keepMounted
            summary={<BrowserSummary />}
          >
            {(open) => (
              <>
                <TerminalLinksToggle />
                <SharedBrowserSettings active={generalActive && open} />
              </>
            )}
          </SettingsGroup>
          <SettingsGroup
            id="notifications"
            title="Notifications"
            summary={<PushSummary active={generalActive} />}
          >
            {(open) => <NotificationsSettings active={generalActive && open} />}
          </SettingsGroup>
          <SettingsGroup
            id="sidebar-usage"
            title="Sidebar & usage"
            summary={<SidebarSummary />}
          >
            <SidebarSettings />
            <UsageTrackingSettings />
          </SettingsGroup>
          <SettingsGroup
            id="plugins"
            title="Plugins"
            summary={<PluginsSummary />}
          >
            {(open) => <PluginsSettings active={generalActive && open} />}
          </SettingsGroup>
          <footer className="mt-4 border-border border-t bg-background px-3 py-2">
            <div className="flex flex-wrap items-center gap-2">
              <span className="mr-0.5 text-[13px] text-muted-foreground tracking-wide">
                lasso
              </span>
              <Pill multiline>
                targets protocol{" "}
                {loading
                  ? "…"
                  : errored || !info
                    ? "unknown"
                    : info.lasso_protocol}
              </Pill>
              {info?.lasso_version && (
                <Pill title="this lasso build's version" multiline>
                  lasso {info.lasso_version}
                </Pill>
              )}
              {info?.latest_version && info.update_state === "available" && (
                <Pill
                  tone="warn"
                  title="a newer lasso release is available — run `lasso update`"
                  multiline
                >
                  update available → {info.latest_version}
                </Pill>
              )}
              {herdr}
              {!loading &&
                !errored &&
                info &&
                !info.err &&
                !info.compatible && (
                  <span className="text-[13px] text-warn">
                    rebuild lasso (or update herdr) so both speak the same
                    protocol
                  </span>
                )}
              <Button
                variant="outline"
                size="icon"
                className="ml-auto size-7"
                title="Keyboard shortcuts"
                onClick={onOpenShortcuts}
              >
                <Keyboard />
              </Button>
              <Button
                variant="outline"
                size="icon"
                className="size-7"
                title="re-check protocol compatibility"
                onClick={() => versionQuery.refetch()}
              >
                <RotateCw />
              </Button>
            </div>
          </footer>
        </TabsContent>
        <TabsContent
          value="themes"
          forceMount
          className="min-h-0 overflow-y-auto px-3 py-4 data-[state=inactive]:hidden"
        >
          <ThemesSettings active={active && sub === "themes"} />
        </TabsContent>
      </Tabs>
    </div>
  )
}

// The collapsed groups' one-line summaries. Each is rendered only while its
// group is closed, and reads nothing the pane was not already reading: the
// same query keys and options its sections use (React Query dedupes the
// observers, so no request is added), the shared ui_state cache, or the
// plugin listing the sidebar strip already holds. Anything that could only be
// had by fetching for the summary alone is left out.
const sep = " · "

function AgentsSummary({ active }: { active: boolean }) {
  const autoTitle = useQuery({
    queryKey: qk.autoTitle,
    queryFn: () => api.autoTitle(),
    enabled: active,
  })
  const pinned = useUIState().creator_default_host ?? ""
  const parts = [
    autoTitle.data && `auto-title ${autoTitle.data.enabled ? "on" : "off"}`,
    `new on ${pinned || "last used host"}`,
  ].filter(Boolean)
  return <>{parts.join(sep)}</>
}

function BrowserSummary() {
  // Same key and options as SharedBrowserSettings, minus the poll — which is
  // mounted beside this (its group keeps it), so this is one query.
  const status = useQuery({
    queryKey: qk.browser,
    queryFn: () => api.browserStatus(),
    retry: false,
  })
  const links = useUIState().terminal_links_in_sidebar
  const st = status.data
  const browser = !st
    ? ""
    : !st.available
      ? "browser unavailable"
      : `browser ${st.running ? "running" : "stopped"}`
  const parts = [
    browser,
    `links open ${links ? "in the sidebar" : "in a new tab"}`,
  ].filter(Boolean)
  return <>{parts.join(sep)}</>
}

function PushSummary({ active }: { active: boolean }) {
  const config = useQuery({
    queryKey: qk.push,
    queryFn: () => api.pushConfig(),
    enabled: active,
  })
  if (!config.data) return null
  const devices = config.data.devices ?? []
  const failing = devices.filter((d) => d.last_error).length
  return (
    <>
      {devices.length === 0
        ? "no devices"
        : `${devices.length} ${devices.length === 1 ? "device" : "devices"}`}
      {failing > 0 && (
        <>
          {sep}
          <span className="text-warn">{failing} failing</span>
        </>
      )}
    </>
  )
}

function SidebarSummary() {
  const ui = useUIState()
  const plugins = usePlugins()
  const infos = React.useMemo(
    () => pluginTabsOf(plugins.data).map((p) => p.tab),
    [plugins.data]
  )
  const hiddenTabs = resolveSidebarTabs(ui.sidebar_tabs, infos).filter(
    (r) => r.hidden
  ).length
  const untracked = (ui.usage_hidden ?? []).length
  const parts = [
    hiddenTabs === 0
      ? "all tabs shown"
      : `${hiddenTabs} ${hiddenTabs === 1 ? "tab" : "tabs"} hidden`,
    untracked > 0 &&
      `${untracked} ${untracked === 1 ? "provider" : "providers"} untracked`,
    ui.usage_compact && "compact footer",
  ].filter(Boolean)
  return <>{parts.join(sep)}</>
}

// A plugin waiting on re-approval has silently unloaded its tabs and tools, so
// it is the one thing here that must read at a glance on a closed header.
function PluginsSummary() {
  const list = usePlugins().data?.plugins
  if (!list) return null
  const count = (s: PluginState) => list.filter((p) => p.state === s).length
  const enabled = count("enabled")
  const pending = count("needs_approval")
  const invalid = count("invalid")
  if (list.length === 0) return <>none installed</>
  return (
    <>
      {enabled} enabled
      {pending > 0 && (
        <>
          {sep}
          <span className="text-warn">{pending} need approval</span>
        </>
      )}
      {invalid > 0 && (
        <>
          {sep}
          <span className="text-destructive">{invalid} invalid</span>
        </>
      )}
    </>
  )
}

// ThemesSettings is the Themes pane: the palette, its backdrop, and which of
// those are lasso's own choice rather than the fleet's.
//
// Two scopes live here and the difference is the whole design. The herdr theme
// is SHARED WITH HERDR: it is written to herdr's config.toml, which the TUI
// reloads, and lasso mirrors it to the hosts and agent CLIs below. Appearance,
// the light/dark palettes and every backdrop control are lasso's own UI state
// (ui_state, shared by every browser on this lasso) and resolve through reads
// (/api/theme?name=) — so choosing one re-themes the browsers without touching
// herdr's config, and an OS that flips to dark at dusk re-themes them instead
// of oscillating the whole fleet twice a day.
//
// It derives the mode and palettes here rather than letting each control hold
// its own copy, because the backdrop gallery has to follow whichever theme is
// actually on screen: under a named palette that is not the one herdr is
// configured with.
function ThemesSettings({ active }: { active: boolean }) {
  const { themeRev } = useApp()
  const themeQuery = useQuery({
    queryKey: qk.theme(themeRev),
    queryFn: () => api.theme(),
    enabled: active,
    // theme_rev is part of the KEY, so every bump — a config edit anywhere in
    // the fleet, this pane's own save — starts a new query whose data is
    // undefined until it lands. Without carrying the last one over, the pane
    // blanked for a round trip on each: the Herdr select lost its value (a
    // <select> with no matching option shows its FIRST one, i.e. whatever
    // theme heads the list), and `effective` fell to "" — which is the theme
    // name a backdrop clicked in that window would have been persisted under.
    placeholderData: keepPreviousData,
  })
  const catalogQuery = useQuery({
    queryKey: qk.themeCatalog,
    queryFn: () => api.themeCatalog(),
    enabled: active,
  })
  // Appearance is server state, read straight from the shared ui_state cache —
  // useUIState is the subscription, getMode/getPalettePref the validated read
  // of the same entry. No copy in React state: that copy is how a control and
  // the document drift apart, and it is also what kept these buttons showing
  // this tab's own choice after another browser changed it. Now a pick made
  // anywhere re-renders this pane as its ui_state_rev bump lands.
  useUIState()
  const mode = getMode()
  const prefs = {
    light: getPalettePref("light"),
    dark: getPalettePref("dark"),
  }
  const t = themeQuery.data
  const catalogThemes = catalogQuery.data?.themes
  const catalog = catalogThemes ?? []
  // lib/theme.ts resolves a backdrop against the catalog too (which backgrounds
  // a theme shipped with), and it fetches its own copy. Hand it this one the
  // moment it lands — including the list an install writes straight into the
  // cache — so the gallery and what actually gets painted are the same list,
  // rather than two independently-failing fetches of it.
  React.useEffect(() => {
    if (catalogThemes) primeThemeCatalog(catalogThemes)
  }, [catalogThemes])
  // The theme on screen: the palette this browser resolves for itself, or
  // herdr's when it follows the fleet. Mirrors lib/theme.ts's own resolution —
  // it has to, or the gallery would offer another theme's backgrounds.
  //
  // The OS scheme is SUBSCRIBED, not read at render: on "system" it decides
  // which of the two palettes is in force, and it changes under this pane (at
  // dusk, or from devtools). Without the subscription the document re-themed
  // while the gallery kept the other scheme's backgrounds and said "Backdrop
  // for <the other theme>". It stays a per-DEVICE observation even though the
  // mode itself is shared — see lib/mode.ts.
  const systemDark = React.useSyncExternalStore(
    subscribeSystemScheme,
    systemPrefersDark
  )
  const scheme: "light" | "dark" =
    mode === "system" ? (systemDark ? "dark" : "light") : resolvedMode(mode)
  const localPalette = mode === "herdr" ? "" : prefs[scheme]
  const effective = localPalette || t?.resolved || ""
  const entry = catalog.find((c) => c.name === effective)
  // Memoized because the gallery takes it as a prop: a fresh array each render
  // would be a new identity on every keystroke in this pane.
  const shipped = React.useMemo(
    () => (entry ? shippedPairs(entry) : []),
    [entry]
  )

  // Both writes go through lib/mode.ts, which patches ui_state; the repaint is
  // AppProvider's subscribeAppearance(refreshTheme), so this tab and every
  // other one re-derive the palette from the same stored value through the same
  // path. Repainting from here as well would fetch /api/theme twice for one
  // click and give this tab a code path no other tab runs.

  return (
    <>
      {/* Appearance, Background and Install are kept mounted: a herdr theme
          pick is held optimistically until the server re-announces it, the
          gallery has an upload in flight and a scroll position, and the
          install field is a URL draft with a clone that can take a while. */}
      <SettingsGroup
        id="appearance"
        title="Appearance"
        keepMounted
        summary={[MODE_LABEL[mode], entry?.label || effective]
          .filter(Boolean)
          .join(sep)}
      >
        <AppearanceToggle mode={mode} onChoose={setMode} />
        {mode !== "herdr" && (
          <PalettePrefs
            mode={mode}
            prefs={prefs}
            themes={catalog}
            onChoose={setPalettePref}
          />
        )}
        <HerdrThemeSelect
          theme={t}
          themes={catalog}
          governs={fleetThemeIsPalette()}
        />
      </SettingsGroup>
      <SettingsGroup
        id="background"
        title="Background"
        keepMounted
        summary={<BackgroundSummary theme={effective} shipped={shipped} />}
      >
        <ThemeBackgrounds
          theme={effective}
          shipped={shipped}
          chromeToo={mode === "herdr" || !!localPalette}
        />
      </SettingsGroup>
      <SettingsGroup
        id="typography"
        title="Typography"
        summary={<TypographySummary />}
      >
        <TypographySettings />
      </SettingsGroup>
      <SettingsGroup id="install-theme" title="Install a theme" keepMounted>
        <ThemeInstall themes={catalog} loading={catalogQuery.isLoading} />
      </SettingsGroup>
      <SettingsGroup
        id="fleet-sync"
        title="Fleet sync"
        summary={
          t && (
            <FleetSyncSummary
              active={active}
              agents={t.sync_agent_themes}
              off={t.theme_sync_off ?? []}
            />
          )
        }
      >
        {(open) => (
          <>
            <SyncAgentThemesToggle enabled={t?.sync_agent_themes ?? true} />
            <ThemeSyncHosts
              active={active && open}
              off={t?.theme_sync_off ?? []}
            />
          </>
        )}
      </SettingsGroup>
    </>
  )
}

const MODE_LABEL: Record<Mode, string> = {
  herdr: "Herdr",
  system: "System",
  light: "Light",
  dark: "Dark",
}

function BackgroundSummary({
  theme,
  shipped,
}: {
  theme: string
  shipped: readonly ShippedBackground[]
}) {
  useUIState()
  const current = backgroundFor(theme, shipped)
  if (!current) return <>none</>
  return (
    <>
      {themeBackgrounds(theme, shipped).find((w) => w.url === current)?.label ??
        "custom picture"}
    </>
  )
}

function TypographySummary() {
  const plugins = usePlugins()
  const typography = useUIState().typography ?? {}
  const fonts = React.useMemo(
    () => resolvePluginFonts(plugins.data),
    [plugins.data]
  )
  const set = TYPOGRAPHY_SLOTS.flatMap((slot) => {
    const f = fontForSlot(slot, fonts, typography)
    return f ? [`${SLOT_LABELS[slot]} ${f.family}`] : []
  })
  return <>{set.length ? set.join(sep) : "lasso default"}</>
}

// Hosts are the same query ThemeSyncHosts and the General pane's pickers read.
function FleetSyncSummary({
  active,
  agents,
  off,
}: {
  active: boolean
  agents: boolean
  off: string[]
}) {
  const hosts = useQuery({
    queryKey: ["hosts"],
    queryFn: () => api.hosts(),
    enabled: active,
  })
  // Counted against the listed hosts: an opt-out can outlive its ssh alias.
  const listed = new Set([
    "local",
    ...(hosts.data?.hosts ?? []).map((h) => h.alias),
  ])
  const all = hosts.data ? listed.size : 0
  const synced = all - off.filter((h) => listed.has(h)).length
  const parts = [
    `agent themes ${agents ? "on" : "off"}`,
    all > 1 && `${synced} of ${all} hosts`,
  ].filter(Boolean)
  return <>{parts.join(sep)}</>
}

// What each typography slot reaches, for the line under its select.
const SLOT_HINTS: Record<TypographySlot, string> = {
  sans: "body and UI text",
  display: "hero headings",
  label: "small-caps labels",
  mono: "code, the file viewer and diffs",
  terminal: "every terminal; icon glyphs still come from the Nerd Font",
}

// TypographySettings picks a typeface per slot from the fonts enabled plugins
// contribute. Server state like the rest of appearance: a pick writes just that
// slot (the server merges per slot), and every browser re-applies it from the
// ui_state_rev bump through lib/typography.ts — this pane only reads the cache
// and writes. A slot whose font was withdrawn (its plugin disabled) keeps its
// stored id and shows lasso's default until the plugin comes back.
function TypographySettings() {
  const plugins = usePlugins()
  const ui = useUIState()
  const fonts = React.useMemo(
    () => resolvePluginFonts(plugins.data),
    [plugins.data]
  )
  const typography = ui.typography ?? {}
  return (
    <div className="mb-4 flex flex-col gap-2">
      <span className={labelClass}>Typography</span>
      {fonts.length === 0 ? (
        <p className="text-[11px] text-muted-foreground">
          No enabled plugin provides fonts. A plugin can add typefaces for the
          interface, code and the terminal — see docs/plugins.md.
        </p>
      ) : (
        <div className="flex flex-col gap-2">
          {TYPOGRAPHY_SLOTS.map((slot) => (
            <TypographySlotSelect
              key={slot}
              slot={slot}
              fonts={fonts.filter((f) => slotAccepts(slot, f.category))}
              stored={typography[slot] ?? ""}
              active={fontForSlot(slot, fonts, typography)}
            />
          ))}
        </div>
      )}
    </div>
  )
}

// previewStack renders a font's own name in its own face, over the interface
// stack while it loads (or if it never does). The family is validated
// (lib/typography.ts), and a React style is CSSOM rather than CSS text anyway.
function previewStack(f: ResolvedFont): string {
  return `"${f.family}", var(--font-sans)`
}

function TypographySlotSelect({
  slot,
  fonts,
  stored,
  active,
}: {
  slot: TypographySlot
  fonts: ResolvedFont[]
  stored: string
  // What the slot actually resolves to right now (null = lasso's default).
  active: ResolvedFont | null
}) {
  const id = `settings-typography-${slot}`
  // A stored id no enabled plugin provides still has to be the selected
  // option, or the <select> would display its first ("lasso default") while
  // the stored choice waits for its plugin to come back.
  const orphan = stored && !fonts.some((f) => f.globalID === stored)
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <label className="text-[11px] text-muted-foreground" htmlFor={id}>
        {SLOT_LABELS[slot]}
      </label>
      <select
        id={id}
        className={cn(fieldClass, "max-w-[15rem]")}
        value={stored}
        onChange={(e) => setTypography(slot, e.target.value)}
        style={active ? { fontFamily: previewStack(active) } : undefined}
      >
        <option value="">lasso default</option>
        {orphan && <option value={stored}>{stored} (unavailable)</option>}
        {fonts.map((f) => (
          <option
            key={f.globalID}
            value={f.globalID}
            style={{ fontFamily: previewStack(f) }}
          >
            {f.family} — {f.plugin}
          </option>
        ))}
      </select>
      <p
        className="text-[11px] text-muted-foreground"
        style={active ? { fontFamily: previewStack(active) } : undefined}
      >
        {SLOT_HINTS[slot]}
        {active?.license ? ` · ${active.license}` : ""}
        {orphan ? " · its plugin is off, so lasso's default applies" : ""}
      </p>
    </div>
  )
}

// AppearanceToggle picks what the chrome is painted from: Herdr (match herdr's
// own theme — the default), System (follow the OS), or a pinned Light/Dark.
// The choice is stored on the server (ui_state) and applies live everywhere:
// setMode patches it and sets the html dark/light class the Nothing --h-*
// tokens cascade from, and every open browser repaints from the bump (see
// lib/mode.ts:subscribeAppearance).
//
// Outside Herdr the chrome is the flat Nothing palette unless a theme is named
// for the scheme (PalettePrefs below), in which case lasso resolves that one
// for its chrome AND its terminals.
function AppearanceToggle({
  mode,
  onChoose,
}: {
  mode: Mode
  onChoose: (m: Mode) => void
}) {
  const opts: { m: Mode; label: string; Icon: typeof Monitor }[] = [
    { m: "herdr", label: "Herdr", Icon: Palette },
    { m: "system", label: "System", Icon: Monitor },
    { m: "light", label: "Light", Icon: Sun },
    { m: "dark", label: "Dark", Icon: Moon },
  ]
  return (
    <div className="mb-4 flex flex-col gap-1">
      <span className={labelClass}>Appearance</span>
      <div className="inline-flex w-fit gap-0.5 rounded-lg border border-border p-0.5">
        {opts.map(({ m, label, Icon }) => (
          <button
            key={m}
            type="button"
            onClick={() => onChoose(m)}
            className={cn(
              "inline-flex items-center gap-1.5 rounded-md px-2.5 py-1 text-[13px] transition-colors",
              mode === m
                ? "bg-primary text-primary-foreground"
                : "text-muted-foreground hover:text-foreground"
            )}
          >
            <Icon className="size-3.5" />
            {label}
          </button>
        ))}
      </div>
      <p className="text-[11px] text-muted-foreground">
        Sets the UI theme for every browser on this lasso. Herdr matches herdr's
        own colors; System follows each device's OS; Light/Dark pin the scheme.
      </p>
    </div>
  )
}

// PalettePrefs names a theme per light/dark scheme. Picking one makes lasso
// resolve that theme through /api/theme?name= — a read — and wear it in the
// chrome and the terminals, while herdr's config.toml, the other hosts and
// every agent keep the shared theme. That is what makes "System" usable with
// real palettes: the OS flipping at dusk re-themes the browsers, where
// switching the herdr theme instead would re-theme every machine in the fleet
// twice a day.
//
// Only the current mode's schemes are offered (System has both, a pinned
// Light/Dark just the one), and each list is filtered to themes of that
// lightness: a light palette under the dark class would leave every `dark:`
// variant in the chrome fighting the palette it sits on.
function PalettePrefs({
  mode,
  prefs,
  themes,
  onChoose,
}: {
  mode: Mode
  prefs: { light: string; dark: string }
  themes: ThemeCatalogEntry[]
  onChoose: (scheme: "light" | "dark", name: string) => void
}) {
  const schemes: ("light" | "dark")[] =
    mode === "system" ? ["light", "dark"] : [resolvedMode(mode)]
  return (
    <div className="mb-4 flex flex-col gap-2">
      <span className={labelClass}>Palette</span>
      <div className="flex flex-wrap gap-3">
        {schemes.map((s) => (
          <div key={s} className="flex min-w-0 flex-col gap-1">
            <label
              className="text-[11px] text-muted-foreground capitalize"
              htmlFor={`settings-palette-${s}`}
            >
              {s} scheme
            </label>
            <select
              id={`settings-palette-${s}`}
              className={cn(fieldClass, "max-w-[15rem]")}
              value={prefs[s]}
              disabled={themes.length === 0}
              onChange={(e) => onChoose(s, e.target.value)}
            >
              <option value="">Nothing (flat {s})</option>
              {/* A pref the catalog does not carry still has to be the
                  selected option: while the catalog is loading — or after a
                  request that failed — there would otherwise be no option
                  matching the value, and a <select> then displays its first,
                  i.e. this control read as "Nothing" while lasso was wearing
                  a palette. Its own name is the only label available. */}
              {prefs[s] && !themes.some((o) => o.name === prefs[s]) && (
                <option value={prefs[s]}>{prefs[s]}</option>
              )}
              <ThemePickerOptions
                themes={themes.filter((o) => o.light === (s === "light"))}
              />
            </select>
          </div>
        ))}
      </div>
      <p className="text-[11px] text-muted-foreground">
        Wears a theme in the browsers only — nothing is written to herdr's
        config, and no host or agent follows. Leave it on Nothing for the flat
        monochrome chrome.
      </p>
    </div>
  )
}

// ThemePickerOptions is the <option> half of every theme dropdown here, grouped
// by where a theme came from and then by its lightness — with a catalog that
// now runs to dozens of names, "Omarchy · Light" is the only thing that makes
// one findable.
//
// The grouping is the server's own provenance (`source`), never a name: a
// built-in whose key herdr itself accepts is reported "builtin" because the
// palette actually painted is herdr's, one that herdr rejects and lasso vendors
// from Omarchy is "official", a lasso theme drawn from one of our own sites is
// "brand", and a clone is "installed". Brand comes first: those are lasso's
// defaults (OCAI light, Execution Associates dark). retro-82 used to be
// special-cased to "official" here, which is exactly the kind of second
// convention that goes stale the moment the catalog gains a row.
function ThemePickerOptions({ themes }: { themes: ThemeCatalogEntry[] }) {
  return (
    <>
      {(["brand", "builtin", "official", "installed", "plugin"] as const).flatMap(
        (source) =>
          [false, true].map((light) => {
            const options = themes.filter(
              (theme) => theme.source === source && theme.light === light
            )
            if (!options.length) return null
            const label =
              source === "builtin"
                ? "Herdr"
                : source === "brand"
                  ? "Brand"
                  : source === "official"
                    ? "Omarchy"
                    : source === "installed"
                      ? "Omarchy · Installed"
                      : "Plugin"
            return (
              <optgroup
                key={`${source}-${light}`}
                label={`${label} · ${light ? "Light" : "Dark"}`}
              >
                {options.map((theme) => (
                  <option key={theme.name} value={theme.name}>
                    {theme.label}
                    {/* Which plugin, since two plugins' themes share a group
                      and disabling that plugin withdraws the theme. */}
                    {theme.source === "plugin" && theme.plugin
                      ? ` — plugin ${theme.plugin}`
                      : ""}
                  </option>
                ))}
              </optgroup>
            )
          })
      )}
    </>
  )
}

// HerdrThemeSelect picks the herdr theme itself — the SHARED one. Saving writes
// [theme].name in herdr's config.toml, the source of truth both already track:
// the herdr TUI reloads it, and lasso repaints the terminals (and, in Herdr
// mode, the chrome) off the theme_rev SSE bump. Every host lasso syncs to gets
// it too (see ThemeSyncHosts), so the remote TUIs follow as well.
//
// Held while an appearance palette is named: that palette is the fleet's theme
// then (lib/mode.ts:fleetThemeIsPalette), and a pick here would fan herdr's
// over every host and its agents — the endpoint refuses one for the same
// reason. Clear the palette above and this hands the fleet back to herdr.
//
// The list is the server's own: lasso's built-ins, the bundled Omarchy
// palettes, and anything installed from a URL, in one canonical order.
function HerdrThemeSelect({
  theme,
  themes,
  governs,
}: {
  theme: ThemePayload | undefined
  themes: ThemeCatalogEntry[]
  // True while a palette named in the appearance setting owns the fleet's
  // theme, i.e. while this control has nothing to say.
  governs: boolean
}) {
  const t = theme
  // Optimistic selection so the dropdown doesn't snap back while the config
  // write → theme_rev bump → refetch round-trips.
  const [pending, setPending] = React.useState<string | null>(null)
  // The payload that was on screen when the write went out. The optimistic
  // value is dropped as soon as the server answers with a DIFFERENT one:
  // react-query hands back the same object while nothing has changed
  // (structural sharing), and keepPreviousData makes the placeholder for the
  // new theme_rev that same object too, so an identity change is exactly "the
  // server has re-announced the theme".
  //
  // Deliberately not "hold it until the names agree": a pick that something
  // else overwrote — an older lasso sharing this config.toml, a hand edit,
  // herdr refusing it — would then be displayed indefinitely as though it had
  // taken, and a picker that hides a revert is worse than one that shows it.
  const wroteOn = React.useRef<ThemePayload | undefined>(undefined)
  React.useEffect(() => {
    if (pending && t && t !== wroteOn.current) setPending(null)
  }, [pending, t])
  const setMutation = useMutation({
    mutationFn: (name: string) => api.setTheme(name),
    onError: (e: Error) => {
      setPending(null)
      toast.error(`Couldn't set theme: ${e.message}`)
    },
  })
  const value = pending ?? t?.resolved ?? ""
  return (
    <div className="mb-4 flex flex-col gap-1">
      <label className={labelClass} htmlFor="settings-herdr-theme">
        Herdr theme (shared)
      </label>
      <select
        id="settings-herdr-theme"
        className={cn(fieldClass, "max-w-xs")}
        value={value}
        disabled={!t || governs}
        onChange={(e) => {
          wroteOn.current = t
          setPending(e.target.value)
          setMutation.mutate(e.target.value)
        }}
      >
        {/* Checked against the list actually RENDERED — the catalog — not
            against /api/theme's own themes: a value the catalog does not carry
            (still loading, or a request that failed) then had no matching
            option at all, and a <select> falls back to displaying its FIRST
            one, which is the top of the Herdr group. That is the whole of
            "my Omarchy theme reverted to a herdr one" as seen in this
            control. */}
        {!themes.some((o) => o.name === value) && (
          <option value={value}>{value || "…"}</option>
        )}
        <ThemePickerOptions themes={themes} />
      </select>
      <p className="text-[11px] text-muted-foreground">
        Sets herdr's own theme in its config.toml; herdr and the terminals
        follow it live.
        {t?.forced &&
          " This lasso was launched with a -theme override, so its terminals won't follow until that flag is dropped."}
        {governs &&
          " Off while a palette is named above: the fleet wears that palette, not herdr's theme. Clear it to pick herdr's again."}
      </p>
    </div>
  )
}

// ThemeBackgrounds is the backdrop gallery for the theme currently on screen:
// what lasso bundles for it (retro-82's 27 vendored stills, served from the
// embedded build), what the theme itself shipped with (an installed Omarchy
// theme brings its own), and whatever this browser was handed by URL or upload.
// Plus the two knobs that go with a backdrop: the scrim that keeps glyphs
// readable over a photograph, and the palette-derived shading that gives a flat
// theme some depth with no image at all.
//
// Every choice here is per THEME and lives on the server (ui_state — see
// lib/wallpaper.ts): nothing is written to herdr's config and nothing is
// mirrored to another host, but every browser reaching this lasso follows,
// and switching palette back and forth restores the backdrop each one had. A
// pick repaints on the spot without a remount or a theme tick, here and in
// every other open tab: the write lands in the shared prefs cache, and
// lib/wallpaper.ts's subscription drives applyAtmosphere off it — which pins
// what the chrome paints from and rewrites the injected stylesheet inside
// every already-loaded terminal iframe.
function ThemeBackgrounds({
  theme,
  shipped,
  chromeToo,
}: {
  theme: string
  shipped: readonly ShippedBackground[]
  // Whether the surrounding window wears the backdrop too, or only the
  // terminals (the flat Nothing chrome does not — say so rather than let the
  // gallery look broken).
  chromeToo: boolean
}) {
  // The tab's host, as api's optional selector: an upload has to land on the
  // machine whose /api/file will serve it back, and a bare undefined is that
  // host's own default.
  const host = useApp().host ?? undefined
  // Subscribing to the persisted prefs is what re-renders this pane when a
  // choice changes — here or in another browser. The values themselves are
  // read back through lib/wallpaper.ts on every render rather than mirrored
  // into component state: it is the single source of truth for both the
  // gallery and the selection, and a copy here is how the two drift apart.
  useUIState()
  const [url, setUrl] = React.useState("")
  const [uploading, setUploading] = React.useState(false)
  const fileRef = React.useRef<HTMLInputElement>(null)
  const gallery = themeBackgrounds(theme, shipped)
  const current = backgroundFor(theme, shipped)
  const choose = (pick: string) => {
    // The choice is stored PER THEME, so it needs a theme to store it under.
    // While /api/theme is still in flight — a first paint, a failed request —
    // there is none, and writing the pick under "" both loses it and leaves an
    // entry no theme will ever read.
    if (!theme) {
      toast.error("Waiting for the theme — try again in a moment")
      return
    }
    setThemeBackground(theme, pick)
  }
  const add = (pick: string) => {
    rememberBackground(pick)
    choose(pick)
  }
  const addTyped = () => {
    const raw = url.trim()
    if (!raw) return
    // An absolute path is read back through /api/file on the tab's host — the
    // same endpoint the file viewer previews an image with — so a picture
    // already sitting on the machine needs no upload. Anything else has to be
    // a URL a browser can actually fetch.
    if (raw.startsWith("/") && !raw.startsWith("//")) {
      add(raw.startsWith("/api/") ? raw : api.fileURL(raw, host))
    } else if (/^https?:\/\//i.test(raw)) {
      add(raw)
    } else {
      toast.error("Enter an http(s) URL or an absolute path on this host")
      return
    }
    setUrl("")
  }
  const upload = async (file: File) => {
    setUploading(true)
    try {
      // Written to the host's own ~/.lasso/uploads through the endpoint the
      // terminal's paste/drop already uses, then read back through /api/file:
      // one place that accepts bytes from a browser, and the picture survives a
      // reload (a blob: URL would not) without lasso growing an image store.
      const { path } = await api.pasteFile(file, host, file.name)
      add(api.fileURL(path, host))
      toast.success(`Uploaded ${file.name}`)
    } catch (e) {
      toast.error(`Couldn't upload: ${(e as Error).message}`)
    } finally {
      setUploading(false)
    }
  }
  const tileClass =
    "flex w-full flex-col gap-1 rounded-md border p-1 text-left outline-none transition-colors focus-visible:ring-3 focus-visible:ring-ring/50"
  return (
    <div className="mb-4 flex flex-col gap-1.5">
      <span className={labelClass}>Background</span>
      <div className="grid max-h-72 @lg:grid-cols-6 @sm:grid-cols-4 grid-cols-3 gap-1.5 overflow-y-auto rounded-lg border border-border p-1.5">
        <button
          type="button"
          aria-pressed={current === ""}
          onClick={() => choose(NO_BACKGROUND)}
          className={cn(
            tileClass,
            current === ""
              ? "border-primary bg-secondary"
              : "border-transparent hover:border-border"
          )}
        >
          <span className="flex aspect-video w-full items-center justify-center rounded-sm border border-border border-dashed text-[11px] text-muted-foreground">
            flat
          </span>
          <span
            className={cn(
              "truncate text-[11px]",
              current === "" ? "text-foreground" : "text-muted-foreground"
            )}
          >
            None
          </span>
        </button>
        {gallery.map((w) => (
          <div key={w.url} className="relative">
            <button
              type="button"
              aria-pressed={w.url === current}
              onClick={() => choose(w.url)}
              className={cn(
                tileClass,
                w.url === current
                  ? "border-primary bg-secondary"
                  : "border-transparent hover:border-border"
              )}
            >
              {/* Decorative: the visible caption is the button's name, so an
                  alt text here would just read it out twice. object-cover
                  because the images range from 16:9 to ultrawide. */}
              <img
                src={w.thumbnail}
                alt=""
                loading="lazy"
                decoding="async"
                className="aspect-video w-full rounded-sm object-cover"
              />
              <span
                className={cn(
                  "truncate text-[11px]",
                  w.url === current
                    ? "text-foreground"
                    : "text-muted-foreground"
                )}
              >
                {w.label}
              </span>
            </button>
            {w.custom && (
              // Only a hand-given picture can be forgotten; a bundled or
              // shipped one belongs to the theme. Sibling of the tile rather
              // than inside it: a button within a button is not a button.
              <button
                type="button"
                title="Forget this picture"
                onClick={() => {
                  forgetBackground(w.url)
                  // A theme still pointing at it re-resolves to its default on
                  // the next render; making that explicit for the theme on
                  // screen keeps the tile selection honest.
                  if (w.url === current) choose(NO_BACKGROUND)
                }}
                className="absolute top-1.5 right-1.5 rounded-sm bg-popover/90 p-0.5 text-muted-foreground hover:text-foreground"
              >
                <X className="size-3" />
              </button>
            )}
          </div>
        ))}
      </div>
      <div className="flex flex-wrap items-center gap-1.5">
        <input
          className={cn(fieldClass, "min-w-0 flex-1 basis-52")}
          {...NO_AUTOCORRECT}
          placeholder="Image URL, or an absolute path on this host"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault()
              addTyped()
            }
          }}
        />
        <Button
          variant="outline"
          size="sm"
          onClick={addTyped}
          disabled={!url.trim()}
        >
          Add
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={uploading}
          onClick={() => fileRef.current?.click()}
        >
          <Upload className="size-3.5" />
          {uploading ? "Uploading…" : "Upload"}
        </Button>
        <input
          ref={fileRef}
          type="file"
          accept="image/*"
          className="hidden"
          onChange={(e) => {
            const f = e.target.files?.[0]
            // Cleared first: picking the same file twice must fire again.
            e.target.value = ""
            if (f) upload(f)
          }}
        />
      </div>
      <p className="text-[11px] text-muted-foreground">
        {theme ? `Backdrop for ${theme}.` : "Backdrop for the current theme."}{" "}
        Saved on this lasso, per theme — every browser follows.
        {chromeToo
          ? " The terminals and the chrome both wear it."
          : " The terminals wear it; name a palette above for the chrome to as well."}
      </p>
      <AtmosphereControls theme={theme} hasImage={current !== ""} />
    </div>
  )
}

// AtmosphereControls is the pair of knobs a backdrop needs: how much wash sits
// between an image and the glyphs, and whether a theme with no image gets a
// little light. Both are per theme, both live on the server beside the picture,
// and both repaint live: the write lands in the shared prefs cache, which
// re-renders this pane and drives applyAtmosphere for the document.
function AtmosphereControls({
  theme,
  hasImage,
}: {
  theme: string
  hasImage: boolean
}) {
  const shade = getShading(theme)
  const scrim = getScrim(theme)
  return (
    <div className="mt-1 flex flex-col gap-2">
      <label
        className="flex cursor-pointer select-none items-center gap-2 text-muted-foreground text-xs"
        htmlFor="settings-atmo-shade"
      >
        <Checkbox
          id="settings-atmo-shade"
          checked={shade}
          disabled={!theme}
          onCheckedChange={(c) => setShading(theme, c === true)}
        />
        Palette shading — two faint washes of the theme's own colors across the
        canvas
      </label>
      <div className="flex flex-col gap-1">
        <label
          className="flex items-center gap-2 text-muted-foreground text-xs"
          htmlFor="settings-atmo-scrim"
        >
          Image dimming
          <span className="font-mono text-[11px]">
            {Math.round(scrim * 100)}%
          </span>
        </label>
        <div className="flex items-center gap-2">
          <input
            id="settings-atmo-scrim"
            type="range"
            min={0}
            max={100}
            step={1}
            value={Math.round(scrim * 100)}
            disabled={!theme || !hasImage}
            className="w-56 min-w-0 accent-primary disabled:opacity-50"
            onChange={(e) => setScrim(theme, Number(e.target.value) / 100)}
          />
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="shrink-0"
            disabled={!theme || scrim === DEFAULT_SCRIM}
            title={`Reset image dimming to ${Math.round(DEFAULT_SCRIM * 100)}%`}
            onClick={() => setScrim(theme, DEFAULT_SCRIM)}
          >
            Reset to default
          </Button>
        </div>
        <p className="text-[11px] text-muted-foreground">
          {hasImage
            ? `How much of the theme's canvas color washes over the picture. The default is ${Math.round(DEFAULT_SCRIM * 100)}%.`
            : "Applies once a background image is picked."}
        </p>
      </div>
    </div>
  )
}

// ThemeInstall adds a community Omarchy theme from its git URL — the server
// clones it, reads its palette (and any backgrounds it ships) and adds it to
// the catalog, so it becomes selectable above like a built-in. Uninstalling is
// deliberately not offered: the clone is a directory on the machine, and
// deleting one from a browser is a footgun with no undo.
function ThemeInstall({
  themes,
  loading,
}: {
  themes: ThemeCatalogEntry[]
  loading: boolean
}) {
  const queryClient = useQueryClient()
  const [url, setUrl] = React.useState("")
  // The install answers with the whole catalog, so the cache is written from
  // the response rather than invalidated into a second round trip. That write
  // is also what re-primes the module-level catalog lib/theme.ts resolves
  // backgrounds from (ThemesSettings watches the same query), so the palette
  // only has to be re-resolved: an install can change what the CURRENT theme
  // has to offer, since a name already selected can arrive with images.
  const settle = (list: ThemeCatalogEntry[]) => {
    queryClient.setQueryData(qk.themeCatalog, { themes: list })
    queryClient.invalidateQueries({ queryKey: ["theme"] })
    refreshTheme()
  }
  const install = useMutation({
    mutationFn: (u: string) => api.installTheme(u),
    onSuccess: (res) => {
      setUrl("")
      settle(res.themes)
      toast.success(`Installed ${res.installed}`)
    },
    onError: (e: Error) => toast.error(`Couldn't install: ${e.message}`),
  })
  const installed = themes.filter((c) => c.source === "installed")
  return (
    <div className="mb-4 flex flex-col gap-1.5">
      <span className={labelClass}>Install a theme</span>
      <div className="flex flex-wrap items-center gap-1.5">
        <input
          className={cn(fieldClass, "min-w-0 flex-1 basis-64")}
          {...NO_AUTOCORRECT}
          placeholder="https://github.com/user/omarchy-<name>-theme"
          value={url}
          disabled={install.isPending}
          onChange={(e) => setUrl(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && url.trim()) {
              e.preventDefault()
              install.mutate(url.trim())
            }
          }}
        />
        <Button
          variant="outline"
          size="sm"
          disabled={!url.trim() || install.isPending}
          onClick={() => install.mutate(url.trim())}
        >
          <Download className="size-3.5" />
          {install.isPending ? "Installing…" : "Install"}
        </Button>
      </div>
      {installed.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {installed.map((c) => (
            <span
              key={c.name}
              className="flex items-center gap-1.5 rounded-md border border-border px-1.5 py-0.5 text-[11px]"
              title={c.url || c.name}
            >
              <span
                aria-hidden
                className="size-3 rounded-full border border-border"
                style={{
                  background: `linear-gradient(135deg, ${c.background} 50%, ${c.accent} 50%)`,
                }}
              />
              <span className="truncate">{c.label}</span>
              {c.url && (
                <a
                  href={c.url}
                  target="_blank"
                  rel="noreferrer"
                  title={c.url}
                  className="text-muted-foreground hover:text-foreground"
                >
                  <ExternalLink className="size-3" />
                </a>
              )}
            </span>
          ))}
        </div>
      )}
      <p className="text-[11px] text-muted-foreground">
        Clones an Omarchy theme repo on this machine and adds it — palette and
        any backgrounds it ships — to the lists above.
        {loading && " Loading the catalog…"}
      </p>
    </div>
  )
}

// SyncAgentThemesToggle gates lasso's mirroring of the herdr theme into agent
// CLIs' own theme files (opencode's tui.json, Claude Code's herdr.json, omp's
// themes/herdr.json — the one a running agent picks up live) on this host and
// any connected remote host. Server-level setting, default on; a host switched
// off below is excluded from it regardless.
function SyncAgentThemesToggle({ enabled }: { enabled: boolean }) {
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: (on: boolean) => api.setSyncAgentThemes(on),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["theme"] }),
    onError: (e: Error) => toast.error(`Couldn't save: ${e.message}`),
  })
  return (
    <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1">
      <label
        className="flex cursor-pointer select-none items-center gap-2 text-muted-foreground text-xs"
        htmlFor="settings-sync-agent-themes"
      >
        <Checkbox
          id="settings-sync-agent-themes"
          checked={enabled}
          disabled={mutation.isPending}
          onCheckedChange={(c) => mutation.mutate(c === true)}
        />
        Sync agent themes (Claude Code, OpenCode, Oh My Pi)
      </label>
      <SyncThemeNowButton />
    </div>
  )
}

// SyncThemeNowButton pushes the current theme to the fleet on demand.
//
// Everything else about theme sync is implicit — a theme change fans out, and a
// host that was asleep for one catches up on its next probe — which leaves no
// way to ask "did minime actually get this?", and no way to force it after
// something on the far side drifted.
//
// The push runs on the server and answers by notice toast, because it is slow
// (titan's fourteen hosts measured 40s: theme writes wait on SFTP, six hosts at
// a time). So this button reports that it STARTED, and the outcome — named
// hosts, not a count, since which machine is out of step is the whole question —
// arrives on its own. The button re-enables immediately rather than pretending
// to track work it is no longer holding.
//
// It sends the palette THIS browser resolved, which the server cannot work out
// for itself: appearance "system" resolves per device, so a light desktop and a
// dark phone genuinely disagree about what the fleet should wear. Whoever
// presses the button decides — the one place a browser-local palette is allowed
// to reach the fleet, and only because a press is a deliberate act by someone
// looking at the result. In "herdr" appearance mode localPaletteName() is "" and
// the server falls back to herdr's own theme, which is that mode's whole meaning.
function SyncThemeNowButton() {
  const mutation = useMutation({
    mutationFn: () => api.syncThemeNow(localPaletteName()),
    onSuccess: (r) =>
      toast.info(
        `Syncing ${r.theme} to ${r.hosts} host${r.hosts === 1 ? "" : "s"}…`
      ),
    onError: (e: Error) => toast.error(`Couldn't sync: ${e.message}`),
  })
  return (
    <Button
      variant="outline"
      size="sm"
      className="h-6 px-2 text-xs"
      disabled={mutation.isPending}
      onClick={() => mutation.mutate()}
      title="Push the current theme to every reachable host now"
    >
      {mutation.isPending ? (
        <Orb state="working" px={14} />
      ) : (
        <RotateCw className="size-3.5" />
      )}
      Sync now
    </Button>
  )
}

// ThemeSyncHosts is the per-host opt-out: an unchecked host is left entirely
// alone by lasso's theme writes — neither herdr's [theme].name (which a host
// switch would otherwise mirror onto it) nor its agents' theme files — so a
// machine that is themed independently stops being dragged along. Every host
// lasso can address is listed, reachable or not: the preference is stored here,
// so an asleep laptop can be excluded before it next answers. Hidden when the
// ssh config names no hosts, since then it only restates the toggle above.
function ThemeSyncHosts({ active, off }: { active: boolean; off: string[] }) {
  const queryClient = useQueryClient()
  const hostsQuery = useQuery({
    queryKey: ["hosts"],
    queryFn: () => api.hosts(),
    enabled: active,
  })
  const mutation = useMutation({
    mutationFn: ({ host, on }: { host: string; on: boolean }) =>
      api.setHostThemeSync(host, on),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["theme"] }),
    onError: (e: Error) => toast.error(`Couldn't save: ${e.message}`),
  })
  const d = hostsQuery.data
  // Local first, then aliases by name: /api/hosts orders reachable hosts ahead
  // of unprobed ones, so taking its order would shuffle the grid under the
  // cursor as probes land.
  const rows = React.useMemo(
    () => [
      {
        host: "local",
        label: `${d?.local?.hostname || "local"} (this machine)`,
      },
      ...(d?.hosts ?? [])
        .map((h) => ({ host: h.alias, label: h.alias }))
        .sort((a, b) => a.label.localeCompare(b.label)),
    ],
    [d]
  )
  if (rows.length < 2) return null
  return (
    <div className="mt-2 flex flex-col gap-1">
      <span className={labelClass}>Sync theme to hosts</span>
      {/* A fixed grid, not a wrapped row: with 13 hosts of wildly different name
          lengths, flex-wrap staggers every line's checkboxes. Column count
          follows the settings pane's own width (@container above). */}
      <div className="grid @2xl:grid-cols-4 @md:grid-cols-3 grid-cols-2 gap-x-6 gap-y-1.5">
        {rows.map((r) => (
          <label
            key={r.host}
            className="flex min-w-0 cursor-pointer select-none items-center gap-2 text-muted-foreground text-xs"
            htmlFor={`settings-theme-sync-${r.host}`}
            title={r.host}
          >
            <Checkbox
              id={`settings-theme-sync-${r.host}`}
              className="shrink-0"
              checked={!off.includes(r.host)}
              disabled={mutation.isPending}
              onCheckedChange={(c) =>
                mutation.mutate({ host: r.host, on: c === true })
              }
            />
            <span className="truncate">{r.label}</span>
          </label>
        ))}
      </div>
      <p className="text-[11px] text-muted-foreground">
        An unchecked host keeps its own theme: lasso writes neither herdr's
        config.toml nor any agent theme file there. Re-checking one pushes the
        current theme to it right away if it's reachable.
      </p>
    </div>
  )
}

// AutoTitleToggle gates auto-titling: after a new agent is created (here or
// over MCP), lasso asks a local agent CLI to name it from the whole prompt and
// renames its workspace to that, instead of leaving the prompt's clipped first
// line as the sidebar entry. Unlike everything below it this is NOT host-scoped
// — the CLI runs on the box lasso runs on, whichever host the agent landed on —
// so it sits above the host picker. Server-level setting, default on.
function AutoTitleToggle({ active }: { active: boolean }) {
  const queryClient = useQueryClient()
  const query = useQuery({
    queryKey: qk.autoTitle,
    queryFn: () => api.autoTitle(),
    enabled: active,
  })
  const mutation = useMutation({
    mutationFn: (on: boolean) => api.setAutoTitle(on),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: qk.autoTitle }),
    onError: (e: Error) => toast.error(`Couldn't save: ${e.message}`),
  })
  return (
    <div className="mb-4 flex flex-col gap-1">
      <span className={labelClass}>New agents</span>
      <label
        className="flex cursor-pointer select-none items-center gap-2 text-[13px] text-foreground"
        htmlFor="settings-auto-title"
      >
        <Checkbox
          id="settings-auto-title"
          checked={query.data?.enabled ?? true}
          disabled={!query.data || mutation.isPending}
          onCheckedChange={(c) => mutation.mutate(c === true)}
        />
        Auto-title new agents from their prompt
      </label>
      <p className="text-[11px] text-muted-foreground">
        Names each new agent by asking the first local CLI that answers (claude,
        codex, opencode, omp, pi) to summarize its prompt — so the sidebar shows
        a title instead of the prompt's clipped first line. Runs on this
        machine, whichever host the agent was created on, and only renames the
        workspace: the branch and working directory keep their original names.
      </p>
    </div>
  )
}

// capLabel names the browser's resource limits in a pill: "CPU 200% · 2G".
function capLabel(cpu: string, mem: string): string {
  const parts = [cpu && `CPU ${cpu}`, mem && `mem ${mem}`].filter(Boolean)
  return parts.length ? parts.join(" · ") : "uncapped"
}

// TerminalLinksToggle: where a link clicked in a terminal opens. Stored in
// lasso's ui_state, so every browser on this lasso follows it.
function TerminalLinksToggle() {
  const on = useUIState().terminal_links_in_sidebar
  return (
    <div className="mb-4 flex flex-col gap-1">
      <span className={labelClass}>Terminal links</span>
      <label
        className="flex cursor-pointer select-none items-center gap-2 text-[13px] text-foreground"
        htmlFor="settings-terminal-links"
      >
        <Checkbox
          id="settings-terminal-links"
          checked={on}
          onCheckedChange={(c) =>
            patchUIState({ terminal_links_in_sidebar: c === true })
          }
        />
        Open terminal links in the sidebar browser
      </label>
      <p className="text-[11px] text-muted-foreground">
        Cmd/Ctrl-click still opens a new browser tab. Sites that refuse to be
        embedded (GitHub, Google and many others) show a note there with a
        button to open them in a new tab. An http:// link on an https lasso
        opens in a new tab in Iframe mode, since the browser won't embed it; the
        Agent browser loads every link into the page it is showing.
      </p>
    </div>
  )
}

// copyText writes to the clipboard, falling back to a throwaway textarea and
// execCommand where navigator.clipboard is missing — lasso is routinely opened
// over plain http on a tailnet address, which is not a secure context.
async function copyText(text: string) {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
      toast.success("Copied")
      return
    }
  } catch {
    /* fall through to the textarea path */
  }
  const ta = document.createElement("textarea")
  ta.value = text
  ta.setAttribute("readonly", "")
  ta.style.position = "fixed"
  ta.style.top = "0"
  ta.style.opacity = "0"
  document.body.appendChild(ta)
  ta.select()
  const ok = document.execCommand("copy")
  document.body.removeChild(ta)
  if (ok) toast.success("Copied")
  else toast.error("Couldn't copy — select the text instead")
}

function CopyLine({ label, text }: { label: string; text: string }) {
  return (
    <div className="flex items-start gap-1.5">
      <code className="min-w-0 flex-1 select-all rounded-md border border-border bg-muted/40 px-2 py-1 font-mono text-[11px] text-foreground [overflow-wrap:anywhere]">
        {text}
      </code>
      <Button
        variant="outline"
        size="icon"
        className="size-7 flex-shrink-0"
        title={`copy ${label}`}
        aria-label={`copy ${label}`}
        onClick={() => void copyText(text)}
      >
        <Copy />
      </Button>
    </div>
  )
}

// SharedBrowserSettings: the headless Chromium lasso runs on its own machine
// for the Browser tab's Live mode and for agents (browser.go). Server-level —
// there is one per lasso whatever host a tab is on — so it sits with the other
// server-wide settings above the host picker. The status is the same cache
// entry the Browser tab reads, so a start here shows there and vice versa.
function SharedBrowserSettings({ active }: { active: boolean }) {
  const queryClient = useQueryClient()
  const mode = useUIState().browser_mode
  const status = useQuery({
    queryKey: qk.browser,
    queryFn: () => api.browserStatus(),
    retry: false,
    refetchInterval: active ? 5_000 : false,
  })
  const st = status.data
  const action = useMutation({
    mutationFn: (a: BrowserAction) => api.browserAction(a),
    onSuccess: (next) => queryClient.setQueryData(qk.browser, next),
    onError: (e: Error) => toast.error(`Shared browser: ${e.message}`),
    onSettled: () => queryClient.invalidateQueries({ queryKey: qk.browser }),
  })

  // The field is a draft while focused and follows the server otherwise, so a
  // proxy changed from another browser lands here without clobbering typing.
  const [proxy, setProxy] = React.useState("")
  const [editing, setEditing] = React.useState(false)
  const [proxyErr, setProxyErr] = React.useState("")
  const stored = st?.proxy ?? ""
  React.useEffect(() => {
    if (!editing) setProxy(stored)
  }, [stored, editing])
  const saveProxy = useMutation({
    mutationFn: (p: string) => api.setBrowserProxy(p),
    onSuccess: (next) => {
      setProxyErr("")
      queryClient.setQueryData(qk.browser, next)
      toast.success(
        next.proxy
          ? `Shared browser now uses ${next.proxy}`
          : "Shared browser proxy cleared",
        // e.g. which pages the relaunch reopened.
        { description: next.note }
      )
    },
    // The server's 400 is a sentence meant for exactly this spot.
    onError: (e: Error) => setProxyErr(e.message),
    // A 502 means the proxy was STORED but the relaunch failed; refetch so
    // the field and the status show what the server now holds.
    onSettled: () => queryClient.invalidateQueries({ queryKey: qk.browser }),
  })
  const commitProxy = () => {
    setEditing(false)
    const p = proxy.trim()
    if (p === stored) {
      setProxyErr("")
      return
    }
    saveProxy.mutate(p)
  }

  const endpoint = cdpURL()
  // /browser-mcp is lasso's origin, like /cdp: there is one shared browser per
  // lasso whatever host this tab is driving.
  const mcpEndpoint = `${location.origin}/browser-mcp`
  const mcpAdd = `claude mcp add --transport http lasso-browser ${mcpEndpoint}`
  const mcpSessions = st?.mcp_sessions ?? 0
  const busy = action.isPending || saveProxy.isPending

  let state: React.ReactNode
  if (status.isLoading) {
    state = <Pill>checking…</Pill>
  } else if (status.isError || !st) {
    state = (
      <Pill tone="warn" title={status.error?.message}>
        unavailable on this lasso
      </Pill>
    )
  } else if (!st.available) {
    state = <Pill tone="warn">unavailable</Pill>
  } else {
    state = (
      <>
        <Pill tone={st.running ? "good" : "muted"}>
          {st.running ? "running" : "stopped"}
        </Pill>
        <Pill
          tone={st.cpu_quota || st.mem_high ? "muted" : "warn"}
          title={
            st.cpu_quota || st.mem_high
              ? "systemd CPU/memory cap. Change it with LASSO_BROWSER_CPU and LASSO_BROWSER_MEM in lasso's environment."
              : "No resource cap: headless Chromium without a GPU can use several cores"
          }
        >
          {capLabel(st.cpu_quota, st.mem_high)}
        </Pill>
        {st.running && st.pages && (
          <Pill>
            {st.pages.length} {st.pages.length === 1 ? "page" : "pages"}
          </Pill>
        )}
      </>
    )
  }

  return (
    <div className="mb-4 flex flex-col gap-1.5">
      <span className={labelClass}>Shared browser</span>
      <div className="flex flex-wrap items-center gap-1.5">
        {state}
        {st?.available && (
          <div className="ml-auto flex gap-1">
            {st.running ? (
              <>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={busy}
                  onClick={() => action.mutate("restart")}
                >
                  Restart
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={busy}
                  onClick={() => action.mutate("stop")}
                >
                  Stop
                </Button>
              </>
            ) : (
              <Button
                variant="outline"
                size="sm"
                disabled={busy}
                onClick={() => action.mutate("start")}
              >
                {action.isPending ? "Starting…" : "Start"}
              </Button>
            )}
          </div>
        )}
      </div>
      {st?.binary && (
        <p className="font-mono text-[11px] text-muted-foreground [overflow-wrap:anywhere]">
          {st.binary}
        </p>
      )}
      {st?.reason && (
        <p
          className={cn(
            "text-[11px] [overflow-wrap:anywhere]",
            st.available ? "text-warn" : "text-muted-foreground"
          )}
        >
          {st.reason}
          {!st.available &&
            " — install Chromium (or Chrome), or set LASSO_BROWSER to one, to enable it."}
        </p>
      )}
      <p className="text-[11px] text-muted-foreground">
        A real Chromium on lasso's own machine, shown in the sidebar's Browser
        tab in Agent mode. Agents can drive the same pages you see, so localhost
        in it means this lasso's machine. It starts on first use and stops after{" "}
        {st?.idle_minutes ?? 15} minutes with nobody connected.
      </p>

      <label className={cn(labelClass, "mt-1")} htmlFor="settings-browser-mode">
        Browser tab mode
      </label>
      <select
        id="settings-browser-mode"
        className={cn(fieldClass, "max-w-xs")}
        value={mode}
        onChange={(e) =>
          patchUIState({ browser_mode: e.target.value as BrowserMode })
        }
      >
        <option value="live">Agent — the shared Chrome agents can drive</option>
        <option value="embed">Iframe — the page inside this tab</option>
      </select>

      <label
        className={cn(labelClass, "mt-1")}
        htmlFor="settings-browser-proxy"
      >
        Proxy
      </label>
      <input
        id="settings-browser-proxy"
        {...NO_AUTOCORRECT}
        className={cn(fieldClass, "max-w-xs font-mono")}
        placeholder="socks5://host:1080"
        value={proxy}
        disabled={status.isError || !st?.available}
        aria-invalid={proxyErr ? true : undefined}
        onFocus={() => setEditing(true)}
        onChange={(e) => {
          setEditing(true)
          setProxy(e.target.value)
        }}
        onBlur={commitProxy}
        onKeyDown={(e) => {
          if (e.key === "Enter") e.currentTarget.blur()
        }}
      />
      {proxyErr && <p className="text-[11px] text-destructive">{proxyErr}</p>}
      <p className="text-[11px] text-muted-foreground">
        socks5://, socks4://, http:// or https://. Changing it restarts the
        browser and reopens its pages. With socks5:// DNS is resolved through
        the proxy too. Chromium cannot log in to a proxy, so one that needs a
        username and password won't work.
      </p>

      <div className="mt-1 flex flex-wrap items-center gap-1.5">
        <span className={labelClass}>Connect an agent</span>
        {mcpSessions > 0 && (
          <Pill tone="good">
            {mcpSessions} {mcpSessions === 1 ? "agent" : "agents"} connected
          </Pill>
        )}
      </div>
      {st && !st.mcp_available && st.mcp_reason && (
        <p className="text-[11px] text-warn [overflow-wrap:anywhere]">
          {st.mcp_reason}
        </p>
      )}
      <CopyLine label="browser MCP URL" text={mcpEndpoint} />
      <CopyLine label="claude mcp add command" text={mcpAdd} />
      <p className="text-[11px] text-muted-foreground">
        Gives an agent chrome-devtools-mcp's tools, already pointed at this
        browser, with nothing to install on its machine. Other agents (Codex,
        OpenCode, …) add the same URL as a streamable-HTTP MCP server. Behind
        UI_AUTH or MCP_OAUTH a remote agent sends an Authorization header (a
        token from <code className="font-mono">lasso mcp-client token</code>, or
        Basic credentials for UI_AUTH). This URL and the fields above are the
        default profile's; each other browser profile has its own at{" "}
        <code className="font-mono">/browser-mcp/&lt;id&gt;</code>, and profiles
        are managed from the bar along the bottom of the Browser tab.
      </p>
      <CopyLine label="CDP endpoint" text={endpoint} />
      <p className="text-[11px] text-muted-foreground">
        For Playwright (
        <code className="font-mono">chromium.connectOverCDP(endpoint)</code>) or
        any raw CDP client, which sends the same Authorization header on its
        websocket.
      </p>
    </div>
  )
}

// NotificationsSettings turns on Web Push for THIS device — the only way lasso
// can reach you when no tab is open, and on iOS the only way at all (a Home
// Screen web app, 16.4+). Server-level: the subscription lives in lasso's db
// and every registered device gets every notification, so it sits above the
// host picker with the other server-wide settings.
//
// Half the state is the browser's (permission, this device's subscription) and
// half is the server's (which devices it pushes to), so the section reads both
// and never infers one from the other — a device whose permission was revoked
// in iOS Settings still has a server row, and saying "on" then would be a lie.
function NotificationsSettings({ active }: { active: boolean }) {
  const queryClient = useQueryClient()
  const config = useQuery({
    queryKey: qk.push,
    queryFn: () => api.pushConfig(),
    enabled: active,
  })
  const [state, setState] = React.useState<PushState | null>(null)
  const [busy, setBusy] = React.useState(false)
  const refreshState = React.useCallback(() => {
    // readPushState re-announces this device to the server if it holds a
    // subscription, so the device list is refetched after it lands — otherwise a
    // row it just repaired would not show until something else invalidated.
    readPushState().then((s) => {
      setState(s)
      queryClient.invalidateQueries({ queryKey: qk.push })
    })
  }, [queryClient])
  React.useEffect(() => {
    if (active) refreshState()
  }, [active, refreshState])

  const key = config.data?.public_key
  const devices = config.data?.devices ?? []
  const on = state?.subscribed === true && state?.permission === "granted"

  async function toggle(next: boolean) {
    if (!key) return
    setBusy(true)
    try {
      // enablePush must run inside this click: Safari only honors
      // requestPermission from a user gesture, and awaiting anything else first
      // (a refetch, say) spends it.
      setState(next ? await enablePush(key) : await disablePush())
      queryClient.invalidateQueries({ queryKey: qk.push })
      if (next) toast.success("Notifications on for this device")
    } catch (e) {
      toast.error(e instanceof Error ? e.message : String(e))
      refreshState()
    } finally {
      setBusy(false)
    }
  }

  const test = useMutation({
    mutationFn: () => api.pushTest(),
    onSuccess: (r) => {
      if (r.ok) toast.success(`Sent to ${r.devices} device(s)`)
      else toast.error(`Couldn't send: ${r.error}`)
      queryClient.invalidateQueries({ queryKey: qk.push })
    },
    onError: (e: Error) => toast.error(`Couldn't send: ${e.message}`),
  })

  return (
    <div className="mb-4 flex flex-col gap-1">
      <span className={labelClass}>Notifications</span>
      <label
        className="flex cursor-pointer select-none items-center gap-2 text-[13px] text-foreground"
        htmlFor="settings-push"
      >
        <Checkbox
          id="settings-push"
          checked={on}
          disabled={!key || busy || state?.support === "unsupported"}
          onCheckedChange={(c) => toggle(c === true)}
        />
        Push notifications to this device
      </label>
      <p className="text-[11px] text-muted-foreground">
        Two things reach you: an agent that <em>blocks</em> waiting on you — a
        tool approval, a plan gate — which lasso watches every host for in the
        background, and an agent that pings you deliberately (its{" "}
        <code>lasso notify</code>). Every device registered here gets both, and
        opening one lands you on that agent's host. Nothing is polled while no
        device is registered.
      </p>
      {state?.support === "needs-home-screen" && (
        <p className="text-[11px] text-warn">
          On iOS, notifications only work from a Home Screen web app: open the
          Share sheet, choose "Add to Home Screen", then turn this on from the
          installed app.
        </p>
      )}
      {state?.support === "unsupported" && (
        <p className="text-[11px] text-warn">
          This browser can't do Web Push (it needs a service worker and a secure
          origin — https, or localhost).
        </p>
      )}
      {state?.permission === "denied" && (
        <p className="text-[11px] text-warn">
          Notifications are blocked for this site — allow them in your browser's
          site settings, then turn this back on.
        </p>
      )}
      {devices.length > 0 && (
        <div className="mt-1 flex flex-col gap-1">
          {devices.map((d) => (
            <div
              key={d.id}
              className="flex flex-wrap items-center gap-2 text-[11px] text-muted-foreground"
            >
              <span className="text-foreground">{d.label}</span>
              <span className="font-mono opacity-60">{d.id}</span>
              {d.last_error ? (
                <span className="text-destructive">
                  last push failed: {d.last_error}
                </span>
              ) : (
                d.last_ok && <span>last push ok</span>
              )}
            </div>
          ))}
          <div>
            <Button
              variant="outline"
              size="sm"
              className="mt-1 h-7"
              disabled={test.isPending}
              onClick={() => test.mutate()}
            >
              Send a test notification
            </Button>
          </div>
        </div>
      )}
    </div>
  )
}

const USAGE_LAYOUTS = [
  { compact: false, label: "Standard" },
  { compact: true, label: "Compact" },
] as const

// Usage tracking is global app chrome — it decides which providers lasso polls
// at all, and drives both the footer and the Usage tab — so it belongs with the
// other server-level settings above the host picker.
// Which host the New dialog opens on. Unlike the creator defaults further down
// this pane, it is a property of THIS lasso rather than of a host — you cannot
// ask a machine which machine you meant to work on — so it lives in ui_state
// beside the appearance prefs and is shared by every browser here.
//
// "Auto" is the default and the historical behavior: follow the last host a
// create actually ran on, and before there is one, the host the tab is viewing.
function CreatorHostSetting({
  hostOptions,
}: {
  hostOptions: { value: string; label: string }[]
}) {
  const ui = useUIState()
  const pinned = ui.creator_default_host ?? ""
  return (
    <div className="mb-4 flex flex-col gap-1">
      <label className={labelClass} htmlFor="settings-creator-host">
        New agent/terminal host
      </label>
      <select
        id="settings-creator-host"
        className={cn(fieldClass, "max-w-xs")}
        value={pinned}
        onChange={(e) => patchUIState({ creator_default_host: e.target.value })}
      >
        <option value="">Auto (use last used)</option>
        {/* A pinned host that has since gone away stays selectable, or the
            picker would silently read as Auto while the pin is still stored. */}
        {pinned && !hostOptions.some((o) => o.value === pinned) && (
          <option value={pinned}>{pinned} (unavailable)</option>
        )}
        {hostOptions.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
      <p className="text-[11px] text-muted-foreground">
        Which host the New dialog opens on, for both agents and terminals.
      </p>
    </div>
  )
}

function UsageTrackingSettings() {
  const ui = useUIState()
  const hidden = ui.usage_hidden ?? []
  const order = completeUsageProviderOrder(ui.usage_order)
  const compact = ui.usage_compact ?? false

  const setShown = (provider: string, shown: boolean) => {
    const next = new Set(hidden)
    if (shown) next.delete(provider)
    else next.add(provider)
    patchUIState({ usage_hidden: Array.from(next) })
  }
  const moveProvider = (index: number, direction: -1 | 1) => {
    const target = index + direction
    if (target < 0 || target >= order.length) return
    const next = order.slice()
    ;[next[index], next[target]] = [next[target], next[index]]
    patchUIState({ usage_order: next })
  }

  return (
    <div className="mb-4 flex flex-col gap-1">
      <span className={labelClass}>Usage tracking</span>
      <div className="mb-1 flex items-center gap-2">
        <span className="text-[11px] text-muted-foreground">Footer layout</span>
        <div className="inline-flex w-fit gap-0.5 rounded-lg border border-border p-0.5">
          {USAGE_LAYOUTS.map((layout) => (
            <button
              key={layout.label}
              type="button"
              aria-pressed={compact === layout.compact}
              onClick={() => patchUIState({ usage_compact: layout.compact })}
              className={cn(
                "rounded-md px-2 py-0.5 text-xs transition-colors",
                compact === layout.compact
                  ? "bg-primary text-primary-foreground"
                  : "text-muted-foreground hover:text-foreground"
              )}
            >
              {layout.label}
            </button>
          ))}
        </div>
      </div>
      <div className="flex max-w-xs flex-col gap-1">
        {order.map((provider, index) => {
          const id = `settings-usage-${provider
            .toLowerCase()
            .replace(/[^a-z0-9]+/g, "-")}`
          return (
            <div key={provider} className="flex items-center gap-2">
              <span
                aria-hidden
                className="w-3 text-right font-label text-[11px] text-muted-foreground"
              >
                {index + 1}
              </span>
              <Checkbox
                id={id}
                checked={!hidden.includes(provider)}
                onCheckedChange={(checked) =>
                  setShown(provider, checked === true)
                }
              />
              <label
                className="min-w-0 flex-1 cursor-pointer select-none text-[13px] text-foreground"
                htmlFor={id}
              >
                {provider}
              </label>
              <div className="flex items-center gap-0.5">
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  disabled={index === 0}
                  aria-label={`Move ${provider} up`}
                  title={`Move ${provider} up`}
                  onClick={() => moveProvider(index, -1)}
                >
                  <ChevronUp />
                </Button>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  disabled={index === order.length - 1}
                  aria-label={`Move ${provider} down`}
                  title={`Move ${provider} down`}
                  onClick={() => moveProvider(index, 1)}
                >
                  <ChevronDown />
                </Button>
              </div>
            </div>
          )
        })}
      </div>
      <p className="text-[11px] text-muted-foreground">
        Checked providers are tracked: lasso polls their quota endpoints and
        shows them in the footer and the Usage tab. Unchecking one stops the
        requests too, so a provider you don't care about costs nothing. Arrows
        set the order. Compact shortens provider names and removes the footer's
        pace bars—hover a metric for its full label, reset, and pace. Providers
        without credentials stay hidden automatically.
      </p>
    </div>
  )
}

// ShortcutsDialog shows the app's keyboard shortcuts (the SHORTCUT_GROUPS the App key
// handler implements) in a modal. Reference only — nothing to configure.
// Rendered by App (so ⌘? can open it from any tab); the Settings tab's keyboard
// button just toggles the same App-owned state.
export function ShortcutsDialog({
  open,
  onOpenChange,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85dvh] overflow-y-auto sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Keyboard shortcuts</DialogTitle>
        </DialogHeader>
        <div className="flex flex-col gap-4">
          {SHORTCUT_GROUPS.map((g) => (
            <section key={g.title} className="flex flex-col gap-1.5">
              <h3 className="font-medium text-muted-foreground text-xs uppercase tracking-wide">
                {g.title}
              </h3>
              <ul className="flex flex-col gap-1.5">
                {g.shortcuts.map((s) => (
                  <li
                    key={s.keys + s.label}
                    className="flex items-center gap-3 text-sm"
                  >
                    <kbd className="min-w-10 rounded border border-border bg-muted px-1.5 py-0.5 text-center font-mono text-muted-foreground text-xs">
                      {s.keys}
                    </kbd>
                    <span className="text-foreground">{s.label}</span>
                  </li>
                ))}
              </ul>
            </section>
          ))}
        </div>
      </DialogContent>
    </Dialog>
  )
}

// CreationSettings edits one host's agent and terminal defaults plus each
// repository's copy-files/setup settings.
function CreationSettings({ active, host }: { active: boolean; host: string }) {
  const queryClient = useQueryClient()

  const configQuery = useQuery({
    queryKey: qk.agentConfig(host),
    queryFn: () => api.agentConfig(host),
    enabled: active,
  })
  const reposQuery = useQuery({
    queryKey: qk.repos(host),
    queryFn: () => api.repos(host),
    enabled: active,
  })
  const workspacesQuery = useQuery({
    queryKey: qk.workspaces(host),
    queryFn: () => api.workspaces(host),
    enabled: active,
    staleTime: 0,
  })
  const workspaces = workspacesQuery.data?.workspaces ?? []
  const workspaceLabels = React.useMemo(() => {
    const labels = [
      SCRATCH_WORKSPACE,
      "~",
      ...workspaces
        .slice()
        .sort((a, b) => a.number - b.number)
        .map((workspace) => workspace.label),
    ]
    return [...new Set(labels)]
  }, [workspaces])
  const repos = reposQuery.data?.repos ?? []

  // Defaults (editable copies, re-seeded whenever the selected host's config
  // arrives — tracked per host so switching hosts reloads, but a refetch of the
  // same host doesn't clobber in-progress edits).
  const [reposRoot, setReposRoot] = React.useState("")
  const [defaultAgent, setDefaultAgent] = React.useState("")
  const [scratchSetup, setScratchSetup] = React.useState("")
  const [defaultTerminalWorkspace, setDefaultTerminalWorkspace] =
    React.useState(SCRATCH_WORKSPACE)
  const terminalWorkspaceOptions = workspaceLabels.includes(
    defaultTerminalWorkspace
  )
    ? workspaceLabels
    : [defaultTerminalWorkspace, ...workspaceLabels]
  const seededHostRef = React.useRef<string | null>(null)
  React.useEffect(() => {
    if (seededHostRef.current === host || !configQuery.data) return
    seededHostRef.current = host
    setReposRoot(configQuery.data.repos_root || "")
    // Empty string is meaningful: "Auto (use last used)". Don't coerce to claude.
    setDefaultAgent(configQuery.data.default_agent ?? "")
    setScratchSetup(configQuery.data.scratch_setup || "")
    setDefaultTerminalWorkspace(
      configQuery.data.default_terminal_workspace || SCRATCH_WORKSPACE
    )
  }, [configQuery.data, host])

  // Per-repo settings.
  const [repoPath, setRepoPath] = React.useState("")
  const [copyFiles, setCopyFiles] = React.useState("")
  const [setup, setSetup] = React.useState("")
  const [savedRepo, setSavedRepo] = React.useState<string | null>(null)

  // Keep the selected repo valid against the (host-scoped) repo list, which
  // changes when the active host switches.
  React.useEffect(() => {
    if (repos.length === 0) return
    setRepoPath((prev) =>
      prev && repos.some((r) => r.path === prev) ? prev : repos[0].path
    )
  }, [repos])

  // Mirror the selected repo's saved copy-files/setup into the editors. Runs on
  // repo switch and on repos refetch (incl. after an autosave) — but it must NOT
  // clear the saved status, or a save's own refetch would wipe the "Saved ✓".
  React.useEffect(() => {
    const re = repos.find((r) => r.path === repoPath)
    setCopyFiles(re?.copy_files || "")
    setSetup(re?.setup || "")
  }, [repoPath, repos])

  // Clear the saved status only when the user actually switches repos.
  // biome-ignore lint/correctness/useExhaustiveDependencies: reset on repo switch only
  React.useEffect(() => {
    setSavedRepo(null)
  }, [repoPath])

  const saveDefaultsMutation = useMutation({
    mutationFn: () =>
      api.saveAgentConfig(
        {
          repos_root: reposRoot,
          default_agent: defaultAgent,
          scratch_setup: scratchSetup,
          default_terminal_workspace: defaultTerminalWorkspace,
        },
        host
      ),
    onSuccess: () => {
      // Repos root may have changed — refetch both config and the repo scan.
      queryClient.invalidateQueries({ queryKey: qk.agentConfig(host) })
      queryClient.invalidateQueries({ queryKey: qk.repos(host) })
    },
    onError: (e: Error) =>
      toast.error(`Couldn't save agent defaults: ${e.message}`),
  })

  const saveRepoMutation = useMutation({
    mutationFn: () =>
      api.saveRepoConfig(
        { path: repoPath, copy_files: copyFiles, setup },
        host
      ),
    onSuccess: () => {
      setSavedRepo(repoPath)
      queryClient.invalidateQueries({ queryKey: qk.repos(host) })
    },
    onError: (e: Error) =>
      toast.error(`Couldn't save repository setup: ${e.message}`),
  })

  // A failed config/repo read (e.g. a remote host missing the sqlite3 CLI)
  // otherwise leaves the panel showing placeholder defaults and an empty repo
  // list — indistinguishable from a host that genuinely has none. Surface it.
  const readError =
    configQuery.isError || reposQuery.isError || workspacesQuery.isError
      ? (
          (configQuery.error ??
            reposQuery.error ??
            workspacesQuery.error) as Error
        ).message
      : null

  const dirtyDefaults =
    !!configQuery.data &&
    ((configQuery.data.repos_root || "") !== reposRoot ||
      (configQuery.data.default_agent ?? "") !== defaultAgent ||
      (configQuery.data.default_terminal_workspace || SCRATCH_WORKSPACE) !==
        defaultTerminalWorkspace ||
      (configQuery.data.scratch_setup || "") !== scratchSetup)

  const dirtyRepo = (() => {
    const re = repos.find((r) => r.path === repoPath)
    return (re?.copy_files || "") !== copyFiles || (re?.setup || "") !== setup
  })()

  // Autosave: debounce edits, flush on blur. No explicit Save button.
  const flushDefaults = useDebouncedSave(
    dirtyDefaults,
    () => saveDefaultsMutation.mutate(),
    [reposRoot, defaultAgent, defaultTerminalWorkspace, scratchSetup]
  )
  const flushRepo = useDebouncedSave(dirtyRepo, () => {
    if (repoPath) saveRepoMutation.mutate()
  }, [copyFiles, setup, repoPath])

  const defaultsStatus: SaveState = saveDefaultsMutation.isError
    ? "error"
    : dirtyDefaults
      ? "saving"
      : saveDefaultsMutation.isSuccess
        ? "saved"
        : "idle"
  const repoStatus: SaveState = saveRepoMutation.isError
    ? "error"
    : dirtyRepo
      ? "saving"
      : savedRepo === repoPath
        ? "saved"
        : "idle"

  return (
    <div className="flex flex-col gap-4">
      {readError && (
        <div className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-[12px] text-destructive">
          Couldn't load {host}'s settings: {readError}
        </div>
      )}
      <div className="grid @2xl:grid-cols-2 grid-cols-1 gap-4">
        <section className="flex min-w-0 flex-1 flex-col gap-3 rounded-lg border border-border p-4 shadow-sm">
          <div className="flex items-center justify-between gap-2">
            <h3 className="font-medium text-foreground text-sm">
              New Agent defaults
            </h3>
            <SaveStatus
              state={defaultsStatus}
              onRetry={() => saveDefaultsMutation.mutate()}
            />
          </div>

          <Field
            label="Git repos directories"
            hint="One directory per line. The repo picker scans each (one level deep) for git repos."
            htmlFor="settings-repos-root"
          >
            <textarea
              id="settings-repos-root"
              {...NO_AUTOCORRECT}
              className={cn(fieldClass, "resize-none")}
              rows={3}
              value={reposRoot}
              onChange={(e) => setReposRoot(e.target.value)}
              onBlur={flushDefaults}
              placeholder={"~/projects\n~/work"}
            />
          </Field>

          <Field
            label="Default agent"
            hint="Auto remembers the agent you picked last time instead of forcing one."
            htmlFor="settings-default-agent"
          >
            <select
              id="settings-default-agent"
              className={fieldClass}
              value={defaultAgent}
              onChange={(e) => setDefaultAgent(e.target.value)}
              onBlur={flushDefaults}
            >
              <option value="">Auto (use last used)</option>
              <option value="claude">Claude Code</option>
              <option value="codex">Codex</option>
              <option value="opencode">OpenCode</option>
              <option value="omp">Oh My Pi</option>
              <option value="pi">Pi</option>
            </select>
          </Field>

          <Field
            label="Scratch setup commands"
            hint="Run before the agent in scratch (non-git) workspaces."
            htmlFor="settings-scratch-setup"
          >
            <textarea
              id="settings-scratch-setup"
              {...NO_AUTOCORRECT}
              className={cn(fieldClass, "resize-none font-mono")}
              rows={3}
              value={scratchSetup}
              onChange={(e) => setScratchSetup(e.target.value)}
              onBlur={flushDefaults}
              placeholder="uv venv"
            />
          </Field>
        </section>
        <section className="flex min-w-0 flex-col gap-3 rounded-lg border border-border p-4 shadow-sm">
          <div className="flex items-center justify-between gap-2">
            <h3 className="font-medium text-foreground text-sm">
              New terminal defaults
            </h3>
            <SaveStatus
              state={defaultsStatus}
              onRetry={() => saveDefaultsMutation.mutate()}
            />
          </div>

          <Field
            label="Default workspace"
            hint="Selected when the New terminal form opens on this host. If it is missing, the form offers to create it."
            htmlFor="settings-default-terminal-workspace"
          >
            <select
              id="settings-default-terminal-workspace"
              className={fieldClass}
              value={defaultTerminalWorkspace}
              onChange={(event) =>
                setDefaultTerminalWorkspace(event.target.value)
              }
              onBlur={flushDefaults}
            >
              {terminalWorkspaceOptions.map((label) => (
                <option key={label} value={label}>
                  {label}
                </option>
              ))}
            </select>
          </Field>
        </section>

        <section className="@2xl:col-span-2 flex min-w-0 flex-col gap-3 rounded-lg border border-border p-4 shadow-sm">
          <div className="flex items-start justify-between gap-2">
            <div className="flex flex-col gap-0.5">
              <h3 className="font-medium text-foreground text-sm">
                Per-repository setup
              </h3>
              <p className="text-[11px] text-muted-foreground">
                Files copied into a new worktree and commands run before the
                agent — both relative to the repo, applied to every agent
                created from it.
              </p>
            </div>
            {repoPath && (
              <SaveStatus
                state={repoStatus}
                onRetry={() => saveRepoMutation.mutate()}
              />
            )}
          </div>

          <Field label="Repository" htmlFor="settings-repo">
            <select
              id="settings-repo"
              className={fieldClass}
              value={repoPath}
              onChange={(e) => setRepoPath(e.target.value)}
            >
              {repos.length === 0 && <option value="">No repos found</option>}
              {repos.map((r) => (
                <option key={r.path} value={r.path}>
                  {r.name}
                </option>
              ))}
            </select>
          </Field>

          <Field
            label="Copy files into worktree (globs)"
            hint="Comma- or newline-separated. Matched in the repo, copied into the new worktree (e.g. .env, .env.local)."
            htmlFor="settings-copy-files"
          >
            <textarea
              id="settings-copy-files"
              {...NO_AUTOCORRECT}
              className={cn(fieldClass, "resize-none")}
              rows={2}
              value={copyFiles}
              onChange={(e) => setCopyFiles(e.target.value)}
              onBlur={flushRepo}
              placeholder=".env, .env.local"
              disabled={!repoPath}
            />
          </Field>

          <Field
            label="Setup commands"
            hint="Run in the worktree's shell before the agent starts."
            htmlFor="settings-setup"
          >
            <textarea
              id="settings-setup"
              {...NO_AUTOCORRECT}
              className={cn(fieldClass, "resize-none font-mono")}
              rows={3}
              value={setup}
              onChange={(e) => setSetup(e.target.value)}
              onBlur={flushRepo}
              placeholder="bun install"
              disabled={!repoPath}
            />
          </Field>
        </section>
      </div>
    </div>
  )
}

// SidebarSettings arranges the right sidebar's tabs: order (up/down) and which
// are hidden, for the built-ins and every enabled plugin's tabs alike. Stored
// in ui_state.sidebar_tabs, so every browser on this lasso follows it; the
// merge rules (what an absent plugin keeps, where a new tab lands) are
// lib/sidebar-tabs.ts's.
//
// The editor moves rows within the FULL arrangement — including entries for
// plugins that are disabled right now — so re-enabling one puts its tab back
// where the human left it rather than at the end.
function SidebarSettings() {
  const ui = useUIState()
  const plugins = usePlugins()
  const pluginTabs = React.useMemo(
    () => pluginTabsOf(plugins.data),
    [plugins.data]
  )
  const infos = React.useMemo(() => pluginTabs.map((p) => p.tab), [pluginTabs])
  const arrangement = arrangeTabs(ui.sidebar_tabs, infos)
  const rows = resolveSidebarTabs(ui.sidebar_tabs, infos)
  const save = (next: SidebarTabPref[]) => patchUIState({ sidebar_tabs: next })

  const describe = (id: string): { label: string; hint?: string } => {
    if (isBuiltinTab(id))
      return {
        label: BUILTIN_LABELS[id],
        hint: id === "agents" ? "phones and narrow windows only" : undefined,
      }
    const p = pluginTabs.find((t) => t.tab.global_id === id)
    return {
      label: p?.tab.label ?? id,
      hint: p ? `plugin ${p.plugin.name}` : undefined,
    }
  }

  return (
    <div className="mb-4 flex flex-col gap-1">
      <div className="flex items-center gap-2">
        <span className={labelClass}>Sidebar</span>
        {(ui.sidebar_tabs?.length ?? 0) > 0 && (
          <Button
            type="button"
            variant="ghost"
            size="xs"
            className="ml-auto"
            onClick={() => save([])}
          >
            Reset
          </Button>
        )}
      </div>
      <div className="flex max-w-xs flex-col gap-1">
        {rows.map((row, index) => {
          const { label, hint } = describe(row.id)
          const Icon = row.hidden ? EyeOff : Eye
          return (
            <div key={row.id} className="flex items-center gap-2">
              <span
                aria-hidden
                className="w-3 text-right font-label text-[11px] text-muted-foreground"
              >
                {index + 1}
              </span>
              {/* Settings can never be hidden — it is where the switch to
                  unhide everything else lives — so it gets no toggle, only a
                  spacer that keeps the column aligned. */}
              {row.id === "settings" ? (
                <span aria-hidden className="size-6 flex-none" />
              ) : (
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  aria-pressed={!row.hidden}
                  aria-label={row.hidden ? `Show ${label}` : `Hide ${label}`}
                  title={row.hidden ? `Show ${label}` : `Hide ${label}`}
                  onClick={() =>
                    save(setTabHidden(arrangement, row.id, !row.hidden))
                  }
                >
                  <Icon />
                </Button>
              )}
              <span
                className={cn(
                  "min-w-0 flex-1 truncate text-[13px]",
                  row.hidden
                    ? "text-muted-foreground line-through"
                    : "text-foreground"
                )}
              >
                {label}
                {hint && (
                  <span className="ml-1.5 text-[11px] text-muted-foreground no-underline">
                    {hint}
                  </span>
                )}
              </span>
              <div className="flex items-center gap-0.5">
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  disabled={index === 0}
                  aria-label={`Move ${label} up`}
                  title={`Move ${label} up`}
                  onClick={() => {
                    const next = moveTab(arrangement, rows, row.id, -1)
                    if (next) save(next)
                  }}
                >
                  <ChevronUp />
                </Button>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  disabled={index === rows.length - 1}
                  aria-label={`Move ${label} down`}
                  title={`Move ${label} down`}
                  onClick={() => {
                    const next = moveTab(arrangement, rows, row.id, 1)
                    if (next) save(next)
                  }}
                >
                  <ChevronDown />
                </Button>
              </div>
            </div>
          )
        })}
      </div>
      <p className="text-[11px] text-muted-foreground">
        Which tabs this sidebar shows, and in what order. Applies to every
        browser on this lasso. A hidden tab still opens when something needs it
        — a terminal link shows the Browser, an agent opening a file shows Files
        — until you pick another tab.
      </p>
    </div>
  )
}

const PLUGIN_STATE_LABEL: Record<PluginState, string> = {
  disabled: "disabled",
  enabled: "enabled",
  needs_approval: "needs approval",
  invalid: "invalid",
}

const PLUGIN_STATE_TONE: Record<
  PluginState,
  "muted" | "good" | "warn" | "bad"
> = {
  disabled: "muted",
  enabled: "good",
  needs_approval: "warn",
  invalid: "bad",
}

const MCP_STATUS_TONE: Record<
  PluginMCPStatus,
  "muted" | "good" | "warn" | "bad"
> = {
  stopped: "muted",
  starting: "muted",
  running: "good",
  error: "bad",
  unavailable: "warn",
}

// PluginPermissionList spells out exactly what enabling approves — the same
// fields the server fingerprints, so what the human reads here is what a
// later manifest edit would have to be re-approved against.
function PluginPermissionList({
  permissions,
}: {
  permissions: PluginPermissions
}) {
  // Go encodes an empty list as null, so every list is defaulted on the way in.
  const tabs = permissions.tabs ?? []
  const mcp = permissions.mcp
  const command = mcp?.command ?? []
  const network = mcp?.network ?? []
  const envKeys = mcp?.env_keys ?? []
  const secrets = mcp?.secrets ?? []
  const themes = permissions.themes ?? []
  const fonts = permissions.fonts ?? []
  const item = "text-[13px] text-foreground [overflow-wrap:anywhere]"
  const code = "font-mono text-[12px]"
  return (
    <div className="flex flex-col gap-2 text-left">
      {/* Low-risk (data, never code or CSS), but a human should see
          everything a plugin adds — and they are part of what is approved. */}
      {themes.length > 0 && (
        <div>
          <p className={labelClass}>Themes</p>
          <p className={item}>
            <code className={code}>{themes.join(", ")}</code>
          </p>
        </div>
      )}
      {fonts.length > 0 && (
        <div>
          <p className={labelClass}>Fonts</p>
          <p className={item}>
            {fonts.map((f) => `${f.family} (${f.category})`).join(", ")}
          </p>
        </div>
      )}
      <div>
        <p className={labelClass}>Sidebar tabs</p>
        {tabs.length === 0 ? (
          <p className="text-[13px] text-muted-foreground">none</p>
        ) : (
          <ul className="list-disc pl-5">
            {tabs.map((t) => (
              <li key={t.id} className={item}>
                {t.label ?? t.id}:{" "}
                {t.url ? (
                  <>
                    frames <code className={code}>{t.url}</code>
                  </>
                ) : (
                  <>
                    serves <code className={code}>{t.entry}</code> from the
                    plugin's directory
                  </>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>
      {mcp && (
        <>
          <div>
            <p className={labelClass}>
              MCP server (its tools join lasso's /mcp)
            </p>
            <ul className="list-disc pl-5">
              <li className={item}>
                image <code className={code}>{mcp.image}</code>
              </li>
              <li className={item}>
                runs <code className={code}>{command.join(" ")}</code>
              </li>
              {envKeys.length > 0 && (
                <li className={item}>
                  environment <code className={code}>{envKeys.join(", ")}</code>
                </li>
              )}
            </ul>
          </div>
          <div>
            <p className={labelClass}>Network</p>
            {network.length === 0 ? (
              <p className="text-[13px] text-muted-foreground">
                none — no outbound connections at all
              </p>
            ) : (
              <ul className="list-disc pl-5">
                {network.map((n) => (
                  <li key={n} className={item}>
                    <code className={code}>{n}</code>
                  </li>
                ))}
              </ul>
            )}
          </div>
          <div>
            <p className={labelClass}>Secrets</p>
            {secrets.length === 0 ? (
              <p className="text-[13px] text-muted-foreground">none</p>
            ) : (
              <ul className="list-disc pl-5">
                {secrets.map((s) => (
                  <li key={s.name} className={item}>
                    <code className={code}>{s.name}</code>, sent only to{" "}
                    <code className={code}>{(s.hosts ?? []).join(", ")}</code>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </>
      )}
    </div>
  )
}

// PluginsSettings lists what is in the plugins directory and lets the
// operator approve it. Server-level, like the shared browser: plugins run on
// lasso's own machine whatever host a tab is driving.
//
// Nothing a manifest says grants anything. Enabling approves the permissions
// shown in the dialog — and only those: a manifest that later asks for more
// reads "needs approval" and loads nothing until approved again. Trusted
// (run on the host, outside the microVM) is likewise only ever this switch.
function PluginsSettings({ active }: { active: boolean }) {
  const queryClient = useQueryClient()
  const plugins = usePlugins()
  const data = plugins.data
  const [approving, setApproving] = React.useState<Plugin | null>(null)
  const [trusting, setTrusting] = React.useState<Plugin | null>(null)
  // A staged checkout awaiting confirm: a fresh install, or an update of a
  // github-managed plugin. Dismissing it without confirming drops the staging.
  const [staged, setStaged] = React.useState<StagedPlugin | null>(null)
  const [uninstalling, setUninstalling] = React.useState<Plugin | null>(null)
  const [unlinking, setUnlinking] = React.useState<Plugin | null>(null)
  const [logsOf, setLogsOf] = React.useState<string | null>(null)

  // A starting MCP child settles on its own; the plugins_rev bump says when,
  // but poll lightly while one is on screen starting, in case that bump is the
  // one a reconnecting stream missed.
  const starting = (data?.plugins ?? []).some(
    (p) => p.mcp?.status === "starting"
  )
  React.useEffect(() => {
    if (!active || !starting) return
    const t = setInterval(
      () => queryClient.invalidateQueries({ queryKey: qk.plugins }),
      3000
    )
    return () => clearInterval(t)
  }, [active, starting, queryClient])

  const settle = () => queryClient.invalidateQueries({ queryKey: qk.plugins })
  const action = useMutation({
    mutationFn: ({
      name,
      act,
      fingerprint,
    }: {
      name: string
      act: PluginAction
      fingerprint?: string
    }) => api.pluginAction(name, act, fingerprint),
    onError: (e: Error, v) => toast.error(`Plugin ${v.name}: ${e.message}`),
    onSettled: settle,
  })
  const trust = useMutation({
    mutationFn: ({ name, trusted }: { name: string; trusted: boolean }) =>
      api.setPluginTrusted(name, trusted),
    onError: (e: Error, v) => toast.error(`Plugin ${v.name}: ${e.message}`),
    onSettled: settle,
  })
  const reload = useMutation({
    mutationFn: () => api.reloadPlugins(),
    onSuccess: (next) => queryClient.setQueryData(qk.plugins, next),
    onError: (e: Error) => toast.error(`Reloading plugins: ${e.message}`),
    onSettled: settle,
  })
  const installPreview = useMutation({
    mutationFn: ({ source, ref }: { source: string; ref?: string }) =>
      api.pluginInstallPreview(source, ref),
    onSuccess: (preview) => setStaged({ kind: "install", preview }),
    onError: (e: Error) => toast.error(`Preview: ${e.message}`),
  })
  const installConfirm = useMutation({
    mutationFn: ({
      preview,
      enable,
    }: {
      preview: PluginPreview
      enable: boolean
    }) => api.pluginInstallConfirm(preview.token, preview.fingerprint, enable),
    onSuccess: (next, v) => {
      if (next && Array.isArray(next.plugins)) {
        queryClient.setQueryData(qk.plugins, next)
      }
      toast.success(
        `Installed ${v.preview.name}${v.enable ? " and enabled it" : ""}`
      )
    },
    onError: (e: Error, v) => toast.error(stagedError(v.preview.name, e)),
    onSettled: settle,
  })
  const updatePreview = useMutation({
    mutationFn: (name: string) => api.pluginUpdatePreview(name),
    onSuccess: (preview, name) =>
      setStaged({ kind: "update", plugin: name, preview }),
    onError: (e: Error, name) => toast.error(`Update ${name}: ${e.message}`),
  })
  const updateConfirm = useMutation({
    mutationFn: ({ name, preview }: { name: string; preview: PluginPreview }) =>
      api.pluginUpdateConfirm(name, preview.token, preview.fingerprint),
    onSuccess: (_, v) =>
      toast.success(
        `Updated ${v.name}${v.preview.commit ? ` to ${shortCommit(v.preview.commit)}` : ""}`
      ),
    onError: (e: Error, v) => toast.error(stagedError(v.name, e)),
    onSettled: settle,
  })
  const uninstall = useMutation({
    mutationFn: ({ name, purge }: { name: string; purge: boolean }) =>
      api.pluginUninstall(name, purge),
    onSuccess: (_, v) => toast.success(`Uninstalled ${v.name}`),
    onError: (e: Error, v) => toast.error(`Uninstall ${v.name}: ${e.message}`),
    onSettled: settle,
  })
  const unlink = useMutation({
    mutationFn: (name: string) => api.pluginUnlink(name),
    onSuccess: (_, name) => toast.success(`Unlinked ${name}`),
    onError: (e: Error, name) => toast.error(`Unlink ${name}: ${e.message}`),
    onSettled: settle,
  })
  // Best effort: an unconfirmed staging also expires on the server.
  const cancelStaged = (token: string) => {
    api.pluginInstallCancel(token).catch(() => {})
  }
  const busy =
    action.isPending ||
    trust.isPending ||
    reload.isPending ||
    installPreview.isPending ||
    installConfirm.isPending ||
    updatePreview.isPending ||
    updateConfirm.isPending ||
    uninstall.isPending ||
    unlink.isPending

  const list = data?.plugins ?? []

  return (
    <div className="mb-4 flex flex-col gap-1.5">
      <div className="flex items-center gap-2">
        <span className={labelClass}>Plugins</span>
        <Button
          variant="outline"
          size="sm"
          className="ml-auto"
          disabled={busy}
          onClick={() => reload.mutate()}
        >
          <RotateCw />
          {reload.isPending ? "Reloading…" : "Reload"}
        </Button>
      </div>
      {plugins.isLoading ? (
        <Pill>checking…</Pill>
      ) : plugins.isError || !data ? (
        <Pill tone="warn" title={plugins.error?.message}>
          unavailable on this lasso
        </Pill>
      ) : (
        <>
          <p className="text-[11px] text-muted-foreground">
            {data.msb.available ? (
              <>
                Sandbox: microsandbox
                {data.msb.path && (
                  <>
                    {" "}
                    at <code className="font-mono">{data.msb.path}</code>
                  </>
                )}
                . Plugin MCP servers run in their own microVM.
              </>
            ) : (
              <span className="text-warn">
                Sandbox unavailable
                {data.msb.reason ? ` — ${data.msb.reason}` : ""}. Plugin MCP
                servers won't start unless trusted; tabs still work.
              </span>
            )}
          </p>
          <CopyLine label="plugins directory" text={data.dir} />
          <PluginInstallRow
            busy={busy}
            previewing={installPreview.isPending}
            onPreview={(source, ref) => installPreview.mutate({ source, ref })}
          />
          {list.length === 0 && (
            <p className="text-[13px] text-muted-foreground">
              No plugins installed. Install one from GitHub above, or put one in
              the plugins directory and press Reload.
            </p>
          )}
          {list.map((p) => (
            <PluginRow
              key={p.name}
              plugin={p}
              busy={busy}
              onEnable={() => setApproving(p)}
              onDisable={() => action.mutate({ name: p.name, act: "disable" })}
              onRestart={() => action.mutate({ name: p.name, act: "restart" })}
              updating={
                updatePreview.isPending && updatePreview.variables === p.name
              }
              onUpdate={() => updatePreview.mutate(p.name)}
              onUninstall={() => setUninstalling(p)}
              onUnlink={() => setUnlinking(p)}
              onLogs={() => setLogsOf(p.name)}
              onTrust={(trusted) => {
                if (trusted) setTrusting(p)
                else trust.mutate({ name: p.name, trusted: false })
              }}
            />
          ))}
        </>
      )}
      <p className="text-[11px] text-muted-foreground">
        A plugin adds sidebar tabs, MCP tools, themes, fonts, or any mix. A new
        plugin stays disabled until you enable it here, and enabling approves
        exactly the permissions it shows. If its manifest later asks for
        anything more, it stops loading until you approve it again. Its tabs run
        sandboxed and can't read lasso or your other tabs.
      </p>

      <AlertDialog
        open={approving !== null}
        onOpenChange={(open) => {
          if (!open) setApproving(null)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {approving?.state === "needs_approval" ? "Re-approve" : "Enable"}{" "}
              {approving?.name}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              {approving?.state === "needs_approval"
                ? "Its manifest changed since you approved it. Enabling approves exactly this:"
                : "Enabling approves exactly this:"}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="max-h-[50vh] overflow-y-auto">
            {approving && (
              <PluginPermissionList permissions={approving.permissions} />
            )}
          </div>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (approving)
                  action.mutate({
                    name: approving.name,
                    act: "enable",
                    fingerprint: approving.fingerprint,
                  })
              }}
            >
              Enable
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <PluginStagedDialog
        staged={staged}
        onClose={(confirmed) => {
          if (!confirmed && staged) cancelStaged(staged.preview.token)
          setStaged(null)
        }}
        onInstall={(preview, enable) =>
          installConfirm.mutate({ preview, enable })
        }
        onUpdate={(name, preview) => updateConfirm.mutate({ name, preview })}
      />

      <PluginUninstallDialog
        plugin={uninstalling}
        onClose={() => setUninstalling(null)}
        onConfirm={(name, purge) => uninstall.mutate({ name, purge })}
      />

      <AlertDialog
        open={unlinking !== null}
        onOpenChange={(open) => {
          if (!open) setUnlinking(null)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Unlink {unlinking?.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              lasso forgets it: its tabs, tools, themes and fonts go away and
              its approval is dropped. The directory
              {unlinking && pluginSourceOf(unlinking).path
                ? ` ${pluginSourceOf(unlinking).path}`
                : ""}{" "}
              is left exactly as it is.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (unlinking) unlink.mutate(unlinking.name)
              }}
            >
              Unlink
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <PluginLogDialog name={logsOf} onClose={() => setLogsOf(null)} />

      <AlertDialog
        open={trusting !== null}
        onOpenChange={(open) => {
          if (!open) setTrusting(null)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Trust {trusting?.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              Its MCP server will run as your user on this machine, outside the
              sandbox — with your files, your network and everything else your
              account can reach. Its network allowlist no longer applies, and
              its secrets are handed to it as plain environment variables. Only
              trust code you have read or wrote yourself.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              onClick={() => {
                if (trusting)
                  trust.mutate({ name: trusting.name, trusted: true })
              }}
            >
              Run outside the sandbox
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

function PluginRow({
  plugin: p,
  busy,
  updating,
  onEnable,
  onDisable,
  onRestart,
  onTrust,
  onUpdate,
  onUninstall,
  onUnlink,
  onLogs,
}: {
  plugin: Plugin
  busy: boolean
  updating: boolean
  onEnable: () => void
  onDisable: () => void
  onRestart: () => void
  onTrust: (trusted: boolean) => void
  onUpdate: () => void
  onUninstall: () => void
  onUnlink: () => void
  onLogs: () => void
}) {
  const trustID = `settings-plugin-trust-${p.name}`
  const tools = p.mcp?.tools ?? []
  const src = pluginSourceOf(p)
  return (
    <div className="flex flex-col gap-1 rounded-lg border border-border p-2">
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="font-medium text-[13px] text-foreground">
          {p.name}
        </span>
        {p.version && (
          <span className="text-[11px] text-muted-foreground">{p.version}</span>
        )}
        <Pill tone={PLUGIN_STATE_TONE[p.state]}>
          {PLUGIN_STATE_LABEL[p.state]}
        </Pill>
        <div className="ml-auto flex gap-1">
          {(p.state === "disabled" || p.state === "needs_approval") && (
            <Button
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={onEnable}
            >
              {p.state === "needs_approval" ? "Review…" : "Enable…"}
            </Button>
          )}
          {p.state === "enabled" && p.permissions.mcp && (
            <Button
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={onRestart}
            >
              Restart
            </Button>
          )}
          {(p.state === "enabled" || p.state === "needs_approval") && (
            <Button
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={onDisable}
            >
              Disable
            </Button>
          )}
        </div>
      </div>
      {p.description && (
        <p className="text-[12px] text-muted-foreground">{p.description}</p>
      )}
      <PluginSourceLine info={src} />
      {p.state === "invalid" && p.error && (
        <p className="text-[11px] text-bad [overflow-wrap:anywhere]">
          {p.error}
        </p>
      )}
      {p.state === "needs_approval" && (
        <p className="text-[11px] text-warn">
          Its permissions changed since you approved them, so its tabs and tools
          are off until you review them.
        </p>
      )}
      {p.state === "enabled" && (p.tabs?.length ?? 0) > 0 && (
        <p className="text-[11px] text-muted-foreground">
          Tabs: {(p.tabs ?? []).map((t) => t.label).join(", ")}
        </p>
      )}
      {(p.themes?.length ?? 0) > 0 && (
        <p className="text-[11px] text-muted-foreground [overflow-wrap:anywhere]">
          Themes:{" "}
          {(p.themes ?? [])
            .map((t) => (t.key_taken ? `${t.label} (skipped)` : t.label))
            .join(", ")}
        </p>
      )}
      {(p.fonts?.length ?? 0) > 0 && (
        <p className="text-[11px] text-muted-foreground [overflow-wrap:anywhere]">
          Fonts:{" "}
          {(p.fonts ?? [])
            .map(
              (f) =>
                `${f.family} (${f.category}${f.license ? `, ${f.license}` : ""})`
            )
            .join(", ")}
        </p>
      )}
      {(p.warnings ?? []).map((w) => (
        <p key={w} className="text-[11px] text-warn [overflow-wrap:anywhere]">
          {w}
        </p>
      ))}
      {p.mcp && p.state === "enabled" && (
        <div className="flex flex-col gap-0.5">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="text-[11px] text-muted-foreground">MCP</span>
            <Pill tone={MCP_STATUS_TONE[p.mcp.status]}>{p.mcp.status}</Pill>
            <Pill tone={p.mcp.sandboxed ? "muted" : "warn"}>
              {p.mcp.sandboxed ? "sandboxed" : "on this machine"}
            </Pill>
          </div>
          {p.mcp.detail && (
            <p
              className={cn(
                "text-[11px] [overflow-wrap:anywhere]",
                p.mcp.status === "error" || p.mcp.status === "unavailable"
                  ? "text-warn"
                  : "text-muted-foreground"
              )}
            >
              {p.mcp.detail}
            </p>
          )}
          {tools.length > 0 && (
            <p className="font-mono text-[11px] text-muted-foreground [overflow-wrap:anywhere]">
              {tools.join(", ")}
            </p>
          )}
        </div>
      )}
      {p.permissions.mcp && p.state !== "invalid" && (
        <div className="flex flex-col gap-0.5">
          <label
            className="flex cursor-pointer select-none items-center gap-2 text-[13px] text-foreground"
            htmlFor={trustID}
          >
            <Checkbox
              id={trustID}
              checked={p.trusted}
              disabled={busy}
              onCheckedChange={(c) => onTrust(c === true)}
            />
            Trusted
          </label>
          <p
            className={cn(
              "text-[11px]",
              p.trusted ? "text-warn" : "text-muted-foreground"
            )}
          >
            {p.trusted
              ? "Its MCP server runs as your user on this machine, outside the sandbox."
              : "Off: its MCP server runs in a microVM. Trusting it runs it as your user on this machine, outside the sandbox."}
          </p>
        </div>
      )}
      {/* What can be done to the checkout itself depends on who owns it:
          lasso's own clone (github) can be updated and removed, a linked
          directory only forgotten, and a hand-placed one is the operator's. */}
      <div className="flex flex-wrap gap-1">
        {src.kind === "github" && (
          <>
            <Button
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={onUpdate}
            >
              {updating ? "Checking…" : "Update…"}
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={onUninstall}
            >
              Uninstall…
            </Button>
          </>
        )}
        {src.kind === "linked" && (
          <Button
            variant="outline"
            size="sm"
            disabled={busy}
            onClick={onUnlink}
          >
            Unlink…
          </Button>
        )}
        <Button variant="ghost" size="sm" onClick={onLogs}>
          Logs
        </Button>
      </div>
    </div>
  )
}

// A preview awaiting confirmation. The token names the staged checkout on the
// server; the fingerprint is what confirm must send back.
type StagedPlugin =
  | { kind: "install"; preview: PluginPreview }
  | { kind: "update"; plugin: string; preview: PluginPreview }

// stagedError turns a failed confirm into a toast. 409 is the one that needs
// words: the staged manifest is not the one the dialog showed.
function stagedError(name: string, e: Error): string {
  if (e instanceof ApiError && e.status === 409) {
    return `${name}: the plugin changed since the preview — preview again.`
  }
  return `${name}: ${e.message}`
}

// PluginSourceLine says where a plugin came from: lasso's clone of a GitHub
// repo (linked to the exact commit), a linked development directory, or a
// directory someone put in the plugins dir by hand.
function PluginSourceLine({
  info,
}: {
  info: ReturnType<typeof pluginSourceOf>
}) {
  const cls =
    "text-[11px] text-muted-foreground [overflow-wrap:anywhere] font-mono"
  if (info.kind === "github") {
    const href = githubCommitURL(info)
    const commit = shortCommit(info.commit)
    return (
      <p className={cls}>
        github {info.source ?? "?"}
        {info.ref ? ` (${info.ref})` : ""}
        {commit &&
          (href ? (
            <>
              {" @ "}
              <a
                href={href}
                target="_blank"
                rel="noopener noreferrer"
                className="underline decoration-dotted underline-offset-2 hover:text-foreground"
              >
                {commit}
              </a>
            </>
          ) : (
            ` @ ${commit}`
          ))}
      </p>
    )
  }
  if (info.kind === "linked") {
    return <p className={cls}>linked {info.path ?? ""}</p>
  }
  return <p className={cls}>local</p>
}

// PluginInstallRow is the one way into GitHub from here — and it is lasso
// that clones, never the browser. Preview stages a checkout and shows what it
// asks for; nothing is installed until that dialog is confirmed.
function PluginInstallRow({
  busy,
  previewing,
  onPreview,
}: {
  busy: boolean
  previewing: boolean
  onPreview: (source: string, ref?: string) => void
}) {
  const [source, setSource] = React.useState("")
  const [ref, setRef] = React.useState("")
  const [pinning, setPinning] = React.useState(false)
  const submit = () => {
    const s = source.trim()
    if (!s || busy) return
    const r = pinning ? ref.trim() : ""
    onPreview(s, r || undefined)
  }
  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === "Enter") {
      e.preventDefault()
      submit()
    }
  }
  return (
    <div className="flex flex-col gap-1 rounded-lg border border-border border-dashed p-2">
      <span className={labelClass}>Install from GitHub</span>
      <div className="flex flex-wrap items-center gap-1.5">
        <input
          className={cn(fieldClass, "min-w-0 flex-1 basis-52 font-mono")}
          {...NO_AUTOCORRECT}
          aria-label="GitHub source"
          placeholder="owner/repo or owner/repo/subdir"
          value={source}
          onChange={(e) => setSource(e.target.value)}
          onKeyDown={onKey}
        />
        <Button
          variant="outline"
          size="sm"
          disabled={busy || !source.trim()}
          onClick={submit}
        >
          {previewing ? "Fetching…" : "Preview…"}
        </Button>
      </div>
      <button
        type="button"
        className="flex items-center gap-1 self-start text-[11px] text-muted-foreground hover:text-foreground"
        aria-expanded={pinning}
        onClick={() => setPinning((v) => !v)}
      >
        {pinning ? (
          <ChevronDown className="size-3" />
        ) : (
          <ChevronRight className="size-3" />
        )}
        Pin a version
      </button>
      {pinning && (
        <input
          className={cn(fieldClass, "font-mono")}
          {...NO_AUTOCORRECT}
          aria-label="Git ref"
          placeholder="tag, branch or commit (default branch if empty)"
          value={ref}
          onChange={(e) => setRef(e.target.value)}
          onKeyDown={onKey}
        />
      )}
      <p className="text-[11px] text-muted-foreground">
        Anyone can publish a plugin; nothing here is reviewed. The preview shows
        exactly what it asks for, and its MCP server runs in a microVM.
      </p>
    </div>
  )
}

// previewExtras lists the themes and fonts a preview contributes when its
// permissions do not already (they normally do — this covers a server that
// sends them only beside it).
function previewExtras(preview: PluginPreview): {
  themes: string[]
  fonts: string[]
} {
  const perms = preview.permissions
  const themes =
    (perms.themes ?? []).length > 0
      ? []
      : (preview.themes ?? []).map((t) =>
          typeof t === "string"
            ? t
            : `${t.label || t.id}${t.key_taken ? " (skipped: id taken)" : ""}`
        )
  const fonts =
    (perms.fonts ?? []).length > 0
      ? []
      : (preview.fonts ?? []).map((f) => `${f.family} (${f.category})`)
  return { themes, fonts }
}

// PluginStagedDialog is the approval dialog for a staged checkout. Install
// offers "install only" beside the default "install and enable"; update says
// up front whether the new version asks for different permissions (if it
// does, it loads nothing until approved again).
function PluginStagedDialog({
  staged,
  onClose,
  onInstall,
  onUpdate,
}: {
  staged: StagedPlugin | null
  onClose: (confirmed: boolean) => void
  onInstall: (preview: PluginPreview, enable: boolean) => void
  onUpdate: (name: string, preview: PluginPreview) => void
}) {
  // Set by a confirming button just before Radix closes the dialog, so the
  // close can tell a confirm from a dismissal (which cancels the staging).
  const confirmed = React.useRef(false)
  React.useEffect(() => {
    if (staged) confirmed.current = false
  }, [staged])
  const pv = staged?.preview
  const extras = pv ? previewExtras(pv) : { themes: [], fonts: [] }
  const warnings = pv?.warnings ?? []
  const commit = shortCommit(pv?.commit)
  const current = shortCommit(pv?.current_commit)
  const update = staged?.kind === "update" ? staged : null
  return (
    <AlertDialog
      open={staged !== null}
      onOpenChange={(open) => {
        if (!open) onClose(confirmed.current)
      }}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>
            {update ? "Update" : "Install"} {pv?.name}
            {pv?.version ? ` ${pv.version}` : ""}?
          </AlertDialogTitle>
          <AlertDialogDescription className="[overflow-wrap:anywhere]">
            <span className="font-mono">
              {pv?.source}
              {pv?.ref ? ` (${pv.ref})` : ""}
              {update && current && commit
                ? ` @ ${current} → ${commit}`
                : commit
                  ? ` @ ${commit}`
                  : ""}
            </span>
            {pv?.description ? (
              <>
                <br />
                {pv.description}
              </>
            ) : null}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <div className="flex max-h-[50vh] flex-col gap-2 overflow-y-auto">
          {update && (
            <p
              className={cn(
                "text-[13px]",
                update.preview.changes_permissions
                  ? "text-warn"
                  : "text-muted-foreground"
              )}
            >
              Permissions change:{" "}
              {update.preview.changes_permissions
                ? "yes — it stays off until you review and approve the new ones."
                : "no — its approval carries over."}
            </p>
          )}
          <p className="text-left text-[13px] text-muted-foreground">
            {update
              ? "The new version asks for:"
              : "Installing and enabling approves exactly this:"}
          </p>
          {pv && <PluginPermissionList permissions={pv.permissions} />}
          {extras.themes.length > 0 && (
            <div>
              <p className={labelClass}>Themes</p>
              <p className="text-[13px] text-foreground">
                {extras.themes.join(", ")}
              </p>
            </div>
          )}
          {extras.fonts.length > 0 && (
            <div>
              <p className={labelClass}>Fonts</p>
              <p className="text-[13px] text-foreground">
                {extras.fonts.join(", ")}
              </p>
            </div>
          )}
          {warnings.map((w) => (
            <p
              key={w}
              className="text-left text-[12px] text-warn [overflow-wrap:anywhere]"
            >
              {w}
            </p>
          ))}
        </div>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          {update ? (
            <AlertDialogAction
              onClick={() => {
                confirmed.current = true
                onUpdate(update.plugin, update.preview)
              }}
            >
              Update
            </AlertDialogAction>
          ) : (
            <>
              <AlertDialogAction
                variant="outline"
                onClick={() => {
                  confirmed.current = true
                  if (pv) onInstall(pv, false)
                }}
              >
                Install only
              </AlertDialogAction>
              <AlertDialogAction
                autoFocus
                onClick={() => {
                  confirmed.current = true
                  if (pv) onInstall(pv, true)
                }}
              >
                Install and enable
              </AlertDialogAction>
            </>
          )}
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

// PluginUninstallDialog confirms removing lasso's clone of a github plugin.
// Its data directory survives unless the box is ticked — reinstalling then
// picks up where it left off.
function PluginUninstallDialog({
  plugin,
  onClose,
  onConfirm,
}: {
  plugin: Plugin | null
  onClose: () => void
  onConfirm: (name: string, purge: boolean) => void
}) {
  const [purge, setPurge] = React.useState(false)
  React.useEffect(() => {
    if (plugin) setPurge(false)
  }, [plugin])
  const id = "settings-plugin-uninstall-purge"
  return (
    <AlertDialog
      open={plugin !== null}
      onOpenChange={(open) => {
        if (!open) onClose()
      }}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Uninstall {plugin?.name}?</AlertDialogTitle>
          <AlertDialogDescription>
            Stops it, withdraws its tabs, tools, themes and fonts, and deletes
            lasso's checkout of it. Its approval and trust are dropped.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <label
          className="flex cursor-pointer select-none items-center gap-2 text-[13px] text-foreground"
          htmlFor={id}
        >
          <Checkbox
            id={id}
            checked={purge}
            onCheckedChange={(c) => setPurge(c === true)}
          />
          Also delete its data
        </label>
        {plugin?.data_dir && (
          <p className="font-mono text-[11px] text-muted-foreground [overflow-wrap:anywhere]">
            {plugin.data_dir}
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            onClick={() => {
              if (plugin) onConfirm(plugin.name, purge)
            }}
          >
            Uninstall
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

// PluginLogDialog shows a plugin's last 200 log lines. Read once on open and
// again on Refresh — nothing streams and nothing polls.
function PluginLogDialog({
  name,
  onClose,
}: {
  name: string | null
  onClose: () => void
}) {
  const log = useQuery({
    queryKey: qk.pluginLog(name ?? ""),
    queryFn: () => api.pluginLog(name ?? "", 200),
    enabled: name !== null,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    refetchOnWindowFocus: false,
  })
  const lines = log.data?.lines ?? []
  return (
    <Dialog
      open={name !== null}
      onOpenChange={(open) => {
        if (!open) onClose()
      }}
    >
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{name} logs</DialogTitle>
        </DialogHeader>
        <div className="flex items-center gap-2">
          <span className="text-[11px] text-muted-foreground">
            {log.isFetching
              ? "reading…"
              : log.isError
                ? ""
                : `last ${lines.length} ${lines.length === 1 ? "line" : "lines"}`}
          </span>
          <Button
            variant="outline"
            size="sm"
            className="ml-auto"
            disabled={log.isFetching}
            onClick={() => log.refetch()}
          >
            <RotateCw />
            Refresh
          </Button>
        </div>
        {log.isError ? (
          <p className="text-[12px] text-warn [overflow-wrap:anywhere]">
            {log.error.message}
          </p>
        ) : (
          <pre className="max-h-[60vh] overflow-auto whitespace-pre rounded-lg border border-border bg-muted/40 p-2 font-mono text-[11px] text-foreground leading-snug">
            {lines.length > 0
              ? lines.join("\n")
              : log.isFetching
                ? ""
                : `(no log lines${log.data?.note ? ` — ${log.data.note}` : ""})`}
          </pre>
        )}
      </DialogContent>
    </Dialog>
  )
}
