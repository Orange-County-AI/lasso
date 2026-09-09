import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import {
  ChevronDown,
  ChevronUp,
  Download,
  ExternalLink,
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
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import {
  api,
  completeUsageProviderOrder,
  type ThemeCatalogEntry,
  type ThemeOption,
  type ThemePayload,
} from "@/lib/api"
import { lsGet, lsSet, useApp } from "@/lib/app-store"
import {
  getMode,
  getPalettePref,
  type Mode,
  resolvedMode,
  setMode,
  setPalettePref,
  subscribeSystemScheme,
  systemPrefersDark,
} from "@/lib/mode"
import {
  disablePush,
  enablePush,
  type PushState,
  readPushState,
} from "@/lib/push"
import { qk } from "@/lib/query"
import { SHORTCUTS } from "@/lib/shortcuts"
import {
  applyAtmosphere,
  invalidateThemeCatalog,
  refreshTheme,
  shippedPairs,
} from "@/lib/theme"
import { patchUIState, useUIState } from "@/lib/ui-state"
import { cn } from "@/lib/utils"
import {
  backgroundFor,
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

// The Settings tab, in two panes. General is lasso↔herdr socket-protocol
// compatibility (top) plus the "New Agent" creator configuration: lasso targets
// a fixed protocol (baked in at build time), the daemon reports its own over the
// socket, and when they drift terminals and RPC silently break, so we surface it
// here. The creator defaults (where to scan for repos, the default agent, the
// scratch setup script) are global; each repo's files-to-copy + setup commands
// are scoped to the active host. All of it persists in ~/.lasso/lasso.db.
//
// Themes is everything about how lasso and herdr LOOK — the palette, its
// backdrop, and which of those choices are this browser's alone (see
// ThemesSettings). It is its own pane because half of it is a gallery of
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
          <AutoTitleToggle active={active && sub === "general"} />
          <NotificationsSettings active={active && sub === "general"} />
          <UsageTrackingSettings />
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
          <CreationSettings active={active && sub === "general"} host={host} />
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

// ThemesSettings is the Themes pane: the palette, its backdrop, and which of
// those are this browser's own choice rather than the fleet's.
//
// Two scopes live here and the difference is the whole design. The herdr theme
// is SHARED: it is written to herdr's config.toml, which the TUI reloads and
// every lasso tab follows, and lasso mirrors it to the hosts and agent CLIs
// below. Appearance, the preferred light/dark palettes, and every backdrop
// control are PER BROWSER: they resolve through reads (/api/theme?name=) and
// localStorage, so a tab can wear its own look — and an OS that flips to dark
// at dusk re-themes that tab instead of oscillating the whole fleet twice a
// day.
//
// It owns the mode and palette state rather than each control holding its own,
// because the backdrop gallery has to follow whichever theme is actually on
// screen: under a browser-local palette that is not the one herdr is
// configured with.
function ThemesSettings({ active }: { active: boolean }) {
  const { themeRev } = useApp()
  const themeQuery = useQuery({
    queryKey: qk.theme(themeRev),
    queryFn: () => api.theme(),
    enabled: active,
  })
  const catalogQuery = useQuery({
    queryKey: qk.themeCatalog,
    queryFn: () => api.themeCatalog(),
    enabled: active,
  })
  const [mode, setModeState] = React.useState<Mode>(() => getMode())
  const [prefs, setPrefs] = React.useState(() => ({
    light: getPalettePref("light"),
    dark: getPalettePref("dark"),
  }))
  const t = themeQuery.data
  const catalog = catalogQuery.data?.themes ?? []
  // The theme on screen: the palette this browser resolves for itself, or
  // herdr's when it follows the fleet. Mirrors lib/theme.ts's own resolution —
  // it has to, or the gallery would offer another theme's backgrounds.
  //
  // The OS scheme is SUBSCRIBED, not read at render: on "system" it decides
  // which of the two preferred palettes is in force, and it changes under this
  // pane (at dusk, or from devtools). Without the subscription the document
  // re-themed while the gallery kept the other scheme's backgrounds and said
  // "Backdrop for <the other theme>".
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

  const chooseMode = (m: Mode) => {
    setModeState(m)
    setMode(m)
    // Repaint for the new mode: applies the palette when entering "herdr" or a
    // scheme with a preferred theme, removes the override when leaving one.
    refreshTheme()
  }
  const choosePalette = (s: "light" | "dark", name: string) => {
    setPalettePref(s, name)
    setPrefs((p) => ({ ...p, [s]: name }))
    refreshTheme()
  }

  return (
    <>
      <AppearanceToggle mode={mode} onChoose={chooseMode} />
      {mode !== "herdr" && (
        <PalettePrefs
          mode={mode}
          prefs={prefs}
          themes={t?.themes ?? []}
          onChoose={choosePalette}
        />
      )}
      <HerdrThemeSelect theme={t} pinned={!!localPalette} />
      <ThemeBackgrounds
        theme={effective}
        shipped={shipped}
        chromeToo={mode === "herdr" || !!localPalette}
      />
      <ThemeInstall themes={catalog} loading={catalogQuery.isLoading} />
      <SyncAgentThemesToggle enabled={t?.sync_agent_themes ?? true} />
      <ThemeSyncHosts active={active} off={t?.theme_sync_off ?? []} />
    </>
  )
}

// AppearanceToggle picks what the chrome is painted from: Herdr (match herdr's
// own theme — the default), System (follow the OS), or a pinned Light/Dark. The
// choice persists in localStorage and applies live: setMode sets the html
// dark/light class the Nothing --h-* tokens cascade from, then refreshTheme
// applies or clears the palette --h-* override (see lib/theme.ts).
//
// Outside Herdr the chrome is the flat Nothing palette unless a preferred theme
// is named for the scheme (PalettePrefs below), in which case this browser
// resolves that one for its chrome AND its terminals.
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
        Sets the UI theme. Herdr matches herdr's own colors; System follows your
        OS; Light/Dark pin the scheme. Saved in this browser only.
      </p>
    </div>
  )
}

// PalettePrefs names a theme per light/dark scheme for THIS BROWSER. Picking
// one makes the tab resolve that theme through /api/theme?name= — a read — and
// wear it in the chrome and the terminals, while herdr's config.toml, the other
// tabs, the other hosts and every agent keep the shared theme. That is what
// makes "System" usable with real palettes: the OS flipping at dusk re-themes
// this window, where switching the herdr theme instead would re-theme every
// machine in the fleet twice a day.
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
  themes: ThemeOption[]
  onChoose: (scheme: "light" | "dark", name: string) => void
}) {
  const schemes: ("light" | "dark")[] =
    mode === "system" ? ["light", "dark"] : [resolvedMode(mode)]
  return (
    <div className="mb-4 flex flex-col gap-2">
      <span className={labelClass}>This browser's palette</span>
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
              {themes
                .filter((o) => o.light === (s === "light"))
                .map((o) => (
                  <option key={o.name} value={o.name}>
                    {o.label}
                  </option>
                ))}
            </select>
          </div>
        ))}
      </div>
      <p className="text-[11px] text-muted-foreground">
        Wears a theme in this browser only — nothing is written to herdr's
        config, and no other tab, host or agent follows. Leave it on Nothing for
        the flat monochrome chrome.
      </p>
    </div>
  )
}

// HerdrThemeSelect picks the herdr theme itself — the SHARED one. Saving writes
// [theme].name in herdr's config.toml, the source of truth both already track:
// the herdr TUI reloads it, and lasso repaints the terminals (and, in Herdr
// mode, the chrome) off the theme_rev SSE bump. Every host lasso syncs to gets
// it too (see ThemeSyncHosts), so the remote TUIs follow as well.
//
// The list is the server's own: lasso's built-ins, the bundled Omarchy
// palettes, and anything installed from a URL, in one canonical order.
function HerdrThemeSelect({
  theme,
  pinned,
}: {
  theme: ThemePayload | undefined
  // True when this browser has a palette of its own, so the shared theme is not
  // what is on screen here — worth saying, or the select reads as broken.
  pinned: boolean
}) {
  const t = theme
  // Optimistic selection so the dropdown doesn't snap back while the config
  // write → theme_rev bump round-trips; cleared once the server agrees (or the
  // write fails).
  const [pending, setPending] = React.useState<string | null>(null)
  React.useEffect(() => {
    if (pending && t?.resolved === pending) setPending(null)
  }, [pending, t])
  const setMutation = useMutation({
    mutationFn: (name: string) => api.setTheme(name),
    onError: (e: Error) => {
      setPending(null)
      toast.error(`Couldn't set theme: ${e.message}`)
    },
  })
  const value = pending ?? t?.resolved ?? ""
  const group = (light: boolean) =>
    (t?.themes ?? [])
      .filter((o) => o.light === light)
      .map((o) => (
        <option key={o.name} value={o.name}>
          {o.label}
        </option>
      ))
  return (
    <div className="mb-4 flex flex-col gap-1">
      <label className={labelClass} htmlFor="settings-herdr-theme">
        Herdr theme (shared)
      </label>
      <select
        id="settings-herdr-theme"
        className={cn(fieldClass, "max-w-xs")}
        value={value}
        disabled={!t}
        onChange={(e) => {
          setPending(e.target.value)
          setMutation.mutate(e.target.value)
        }}
      >
        {!t?.themes.some((o) => o.name === value) && (
          <option value={value}>{value || "…"}</option>
        )}
        <optgroup label="Dark">{group(false)}</optgroup>
        <optgroup label="Light">{group(true)}</optgroup>
      </select>
      <p className="text-[11px] text-muted-foreground">
        Sets herdr's own theme in its config.toml; herdr and the terminals
        follow it live.
        {t?.forced &&
          " This lasso was launched with a -theme override, so its terminals won't follow until that flag is dropped."}
        {pinned &&
          " This browser is wearing a palette of its own above, so the change will show in herdr and in other tabs, not here."}
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
// Every choice here is per browser AND per theme (localStorage — see
// lib/wallpaper.ts): nothing is written to herdr's config, nothing is mirrored
// to another host, and switching palette back and forth restores the backdrop
// each one had. A pick repaints on the spot — applyAtmosphere pins what the
// chrome paints from and rewrites the injected stylesheet inside every
// already-loaded terminal iframe, so neither half waits for a remount or a
// theme tick.
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
  // The persisted choices are read on every render rather than mirrored into
  // component state: lib/wallpaper.ts is the single source of truth for both
  // the gallery and the selection, and a copy here is how the two drift apart.
  // A write therefore needs nothing but a re-render, which is all `bump` is.
  const [, bump] = React.useReducer((n: number) => n + 1, 0)
  const [url, setUrl] = React.useState("")
  const [uploading, setUploading] = React.useState(false)
  const fileRef = React.useRef<HTMLInputElement>(null)
  const gallery = themeBackgrounds(theme, shipped)
  const current = backgroundFor(theme, shipped)
  const choose = (pick: string) => {
    setThemeBackground(theme, pick)
    bump()
    applyAtmosphere()
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
                  if (w.url === current) choose(NO_BACKGROUND)
                  else bump()
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
        Saved in this browser only, per theme.
        {chromeToo
          ? " The terminals and this window both wear it."
          : " The terminals wear it; pick a palette above for this window to as well."}
      </p>
      <AtmosphereControls hasImage={current !== ""} onChange={bump} />
    </div>
  )
}

// AtmosphereControls is the pair of knobs a backdrop needs: how much wash sits
// between an image and the glyphs, and whether a theme with no image gets a
// little light. Both are per browser and apply to every theme — they are
// properties of this screen, not of a palette — and both repaint live through
// applyAtmosphere.
function AtmosphereControls({
  hasImage,
  onChange,
}: {
  hasImage: boolean
  // The gallery owns the re-read counter (the selection and these values are
  // read from the same store), so a change here has to tell it.
  onChange: () => void
}) {
  const [shade, setShade] = React.useState(() => getShading())
  const [scrim, setScrimState] = React.useState(() => getScrim())
  return (
    <div className="mt-1 flex flex-col gap-2">
      <label
        className="flex cursor-pointer select-none items-center gap-2 text-muted-foreground text-xs"
        htmlFor="settings-atmo-shade"
      >
        <Checkbox
          id="settings-atmo-shade"
          checked={shade}
          onCheckedChange={(c) => {
            const on = c === true
            setShade(on)
            setShading(on)
            applyAtmosphere()
            onChange()
          }}
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
        <input
          id="settings-atmo-scrim"
          type="range"
          min={0}
          max={100}
          step={1}
          value={Math.round(scrim * 100)}
          disabled={!hasImage}
          className="w-56 max-w-full accent-primary disabled:opacity-50"
          onChange={(e) => {
            const v = Number(e.target.value) / 100
            setScrimState(v)
            setScrim(v)
            applyAtmosphere()
          }}
        />
        <p className="text-[11px] text-muted-foreground">
          {hasImage
            ? "How much of the theme's canvas color washes over the picture. 62% is sized for the brightest of the bundled stills — dim is a better failure than unreadable."
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
  // the response rather than invalidated into a second round trip — and the
  // module-level cache lib/theme.ts resolves backgrounds from is dropped, then
  // the palette re-resolved: an install can change what the CURRENT theme has
  // to offer, since a name already selected can arrive with images.
  const settle = (list: ThemeCatalogEntry[]) => {
    queryClient.setQueryData(qk.themeCatalog, { themes: list })
    queryClient.invalidateQueries({ queryKey: ["theme"] })
    invalidateThemeCatalog()
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
    <label
      className="mt-1 flex cursor-pointer select-none items-center gap-2 text-muted-foreground text-xs"
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

// ShortcutsDialog shows the app's keyboard shortcuts (the SHORTCUTS the App key
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
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>Keyboard shortcuts</DialogTitle>
        </DialogHeader>
        <ul className="flex flex-col gap-1.5">
          {SHORTCUTS.map((s) => (
            <li key={s.keys} className="flex items-center gap-3 text-sm">
              <kbd className="min-w-10 rounded border border-border bg-muted px-1.5 py-0.5 text-center font-mono text-muted-foreground text-xs">
                {s.keys}
              </kbd>
              <span className="text-foreground">{s.label}</span>
            </li>
          ))}
        </ul>
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
    React.useState("~")
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
      configQuery.data.default_terminal_workspace || "~"
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
      (configQuery.data.default_terminal_workspace || "~") !==
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
