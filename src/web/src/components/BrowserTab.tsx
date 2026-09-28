import { useQuery, useQueryClient } from "@tanstack/react-query"
import { ExternalLink, RotateCw } from "lucide-react"
import * as React from "react"
import { toast } from "sonner"
import { BrowserProfileBar } from "@/components/BrowserProfileBar"
import { LiveBrowser } from "@/components/LiveBrowser"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Orb } from "@/components/ui/orb"
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { api, type BrowserMode, type BrowserStatus } from "@/lib/api"
import { lsGet, lsSet } from "@/lib/app-store"
import {
  type BrowserShowRequest,
  DEFAULT_PROFILE,
  onBrowserShowRequest,
  profilesOf,
  storedProfile,
  storeProfile,
} from "@/lib/browser-profiles"
import { LOOPBACK, normalize } from "@/lib/browser-url"
import { qk } from "@/lib/query"
import {
  onSidebarBrowserOpen,
  setEffectiveBrowserMode,
} from "@/lib/sidebar-browser"
import { patchUIState, useUIState } from "@/lib/ui-state"
import { cn } from "@/lib/utils"

// parseLocalPort extracts a local dev-server port from user input. It matches a
// bare port ("5173"), a ":PORT" shorthand, or a full URL whose host is
// loopback-ish. Anything else (a real remote URL) returns null so it's left
// alone.
//
// Note the trap: a bare "8445" (or ":8445") means "port 8445 on whatever host
// this page is served from" — not some other machine's :8445. To open a remote
// page, type its full URL (https://host:8445/).
function parseLocalPort(raw: string): number | null {
  const s = raw.trim()
  if (!s) return null
  if (/^\d{2,5}$/.test(s)) return clampPort(Number(s))
  const colon = s.match(/^:(\d{2,5})$/)
  if (colon) return clampPort(Number(colon[1]))
  try {
    const u = new URL(/^https?:\/\//i.test(s) ? s : `http://${s}`)
    if (LOOPBACK.includes(u.hostname) && u.port) {
      return clampPort(Number(u.port))
    }
  } catch {
    return null
  }
  return null
}

function clampPort(n: number): number | null {
  return Number.isInteger(n) && n >= 1 && n <= 65535 ? n : null
}

// resolve maps user input to an iframe-able src. A bare local port becomes
// http://<this page's hostname>:<port> — the dev server as reached from the
// browser, with no tunnel or proxy in between. Full URLs pass through.
function resolve(raw: string): string {
  const port = parseLocalPort(raw)
  if (port != null) return `http://${location.hostname}:${port}`
  return normalize(raw)
}

// hostOf returns the host portion of a URL, or "" if it can't be parsed.
function hostOf(url: string): string {
  try {
    return new URL(url).host
  } catch {
    return ""
  }
}

// looksPrivateHost reports whether a hostname is a tailnet / private-network
// address. The browser blocks a public page from embedding these (Private
// Network Access), so we tailor the error message when one fails to load.
function looksPrivateHost(host: string): boolean {
  const h = host.replace(/:\d+$/, "").toLowerCase()
  if (h === "localhost" || h.endsWith(".ts.net")) return true
  if (/^127\./.test(h) || /^10\./.test(h) || /^192\.168\./.test(h)) return true
  if (/^172\.(1[6-9]|2\d|3[01])\./.test(h)) return true
  // Tailscale CGNAT 100.64.0.0/10
  if (/^100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\./.test(h)) return true
  return false
}

// probeReachable does a no-cors fetch to verify the browser can actually reach
// (and is allowed to embed) the target. It resolves to an opaque response when
// the host is reachable and rejects when it isn't — including when Private
// Network Access blocks a public→private request, which is exactly the case
// where the iframe silently renders blank. A PNA-blocked iframe still fires
// `onload`, so this probe (not onload) is the reliable failure signal.
async function probeReachable(url: string): Promise<boolean> {
  try {
    await fetch(url, {
      mode: "no-cors",
      cache: "no-store",
      signal: AbortSignal.timeout(8000),
    })
    return true
  } catch {
    return false
  }
}

// EmbedBrowser is the Browser tab's Embed mode: a URL bar + an iframe. A bare local dev-server port (e.g.
// "5173") is embedded as http://<this page's hostname>:<port>. We persist the
// RAW input so it re-resolves on reload. When a target can't be embedded
// (unreachable, mixed content, or a private page blocked while lasso is on a
// public origin), we show a clear error and a prominent open-in-new-tab.
function EmbedBrowser({
  openRequest,
  onOpened,
  modeSwitch,
  note,
}: {
  openRequest: OpenRequest | null
  onOpened: () => void
  modeSwitch: React.ReactNode
  note: string
}) {
  const [url, setUrl] = React.useState(() => lsGet("browserUrl") ?? "")
  const [src, setSrc] = React.useState("about:blank")
  // openTarget is the resolved URL to open in a new tab (kept even on error,
  // since opening externally works when embedding doesn't).
  const [openTarget, setOpenTarget] = React.useState("")
  const [reloadKey, setReloadKey] = React.useState(0)
  const [status, setStatus] = React.useState<
    "idle" | "loading" | "loaded" | "error"
  >("idle")
  const [err, setErr] = React.useState("")
  const seqRef = React.useRef(0)

  const nav = React.useCallback((raw: string) => {
    const input = raw.trim()
    if (!input) return
    const seq = ++seqRef.current
    setUrl(input)
    lsSet("browserUrl", input)
    setStatus("loading")
    setErr("")

    const target = resolve(input)
    setOpenTarget(target)

    // Mixed content: an https page can't embed an http:// page. A bare port
    // always resolves to http://, so an https lasso can only embed a full
    // https:// URL.
    if (location.protocol === "https:" && /^http:\/\//i.test(target)) {
      setSrc("about:blank")
      setErr(
        `lasso is served over https, so the browser won't embed the http:// page at ${hostOf(target) || target} (mixed content). Open it in a new tab, or reach lasso over http (loopback / tailnet) to embed a local dev server.`
      )
      setStatus("error")
      return
    }

    setSrc(target)
    setReloadKey((k) => k + 1)

    // Probe reachability/embeddability in parallel. The probe is authoritative:
    // if it fails, the iframe will be blank, so surface a clear reason.
    if (/^https?:\/\//i.test(target)) {
      // A site that forbids framing still loads "successfully" as a blank
      // frame, and the browser hides why. Ask lasso to read the headers.
      void api
        .frameable(target)
        .then((r) => {
          if (seq !== seqRef.current || r.frameable !== false) return
          setErr(
            `${hostOf(target) || target} doesn't allow other sites to embed it, so it can't show here. Open it in a new tab instead.`
          )
          setStatus("error")
        })
        .catch(() => {
          /* advisory only: an older server or a failed fetch says nothing */
        })
      void probeReachable(target).then((ok) => {
        if (seq !== seqRef.current || ok) return
        const host = hostOf(target)
        const pageIsPublic = !looksPrivateHost(location.host)
        const msg =
          pageIsPublic && looksPrivateHost(host)
            ? `Your browser blocked embedding ${host}. lasso is open over a public address, and browsers won't embed a private/tailnet page in a public one (Private Network Access). It opens fine in its own tab — or open lasso via its tailnet URL to embed tailnet pages.`
            : `Couldn't load ${host || target} in the embedded view — it may be unreachable from your browser or refuse embedding. Try opening it in a new tab.`
        setErr(msg)
        setStatus("error")
      })
    }
  }, [])

  // Re-resolve any saved value on mount, so a reload restores the last target.
  React.useEffect(() => {
    const saved = lsGet("browserUrl")
    if (saved) nav(saved)
  }, [nav])

  // A link clicked in a terminal lands here (lib/sidebar-browser.ts, by way
  // of BrowserTab, which holds it until whichever mode is showing takes it).
  React.useEffect(() => {
    if (!openRequest) return
    onOpened()
    nav(openRequest.url)
  }, [openRequest, onOpened, nav])

  const openExternal = React.useCallback(() => {
    const t = openTarget || (src !== "about:blank" ? src : "")
    if (t) window.open(t, "_blank", "noopener")
  }, [openTarget, src])

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex flex-shrink-0 items-center gap-1.5 border-border border-b bg-background px-2 py-1.5">
        {modeSwitch}
        <Button
          variant="outline"
          size="icon"
          className="size-7"
          title="reload"
          onClick={() => nav(url)}
        >
          <RotateCw />
        </Button>
        <Input
          value={url}
          placeholder="port or URL"
          className="h-7 flex-1 text-[13px]"
          onChange={(e) => setUrl(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") nav(e.currentTarget.value)
          }}
        />
        <Button
          variant="outline"
          size="sm"
          className="h-7"
          disabled={status === "loading"}
          onClick={() => nav(url)}
        >
          go
        </Button>
        <Button
          variant="outline"
          size="icon"
          className="size-7"
          title="open in new tab"
          disabled={!openTarget && src === "about:blank"}
          onClick={openExternal}
        >
          <ExternalLink />
        </Button>
      </div>
      {note && (
        <div className="flex-shrink-0 border-border border-b bg-background px-2 py-1 text-[12px] text-muted-foreground">
          {note}
        </div>
      )}
      {status === "loading" && (
        <div className="flex flex-shrink-0 items-center gap-2 border-border border-b bg-background px-2 py-1 text-[12px] text-muted-foreground">
          <Orb state="working" px={14} />
          loading…
        </div>
      )}
      {status === "error" && (
        <div className="flex flex-shrink-0 items-start justify-between gap-2 border-border border-b bg-background px-2 py-1.5">
          <span className="text-[12px] text-destructive">{err}</span>
          {openTarget && (
            <Button
              variant="outline"
              size="sm"
              className="h-6 flex-shrink-0 gap-1 text-[12px]"
              onClick={openExternal}
            >
              <ExternalLink className="size-3" />
              open in new tab
            </Button>
          )}
        </div>
      )}
      <div className="relative flex min-h-0 flex-1 flex-col">
        <iframe
          key={reloadKey}
          src={src || "about:blank"}
          title="browser preview"
          referrerPolicy="no-referrer"
          className="frame"
          onLoad={() => setStatus((s) => (s === "loading" ? "loaded" : s))}
        />
        {status === "loading" && src !== "about:blank" && (
          <div className="absolute inset-0 flex items-center justify-center bg-background text-[13px] text-muted-foreground">
            connecting…
          </div>
        )}
      </div>
    </div>
  )
}

// A terminal link waiting for whichever mode is on screen to open it. Held
// here rather than handed straight to a mode, because the mode can change
// under it: a link clicked before /api/browser answers is aimed at Live, and
// must still land if the answer turns out to be "no Chromium".
export interface OpenRequest {
  url: string
  seq: number
}

// BrowserModeSwitch is the mode toggle both toolbars carry. The UI calls the
// modes Agent and Iframe (stored as "live" and "embed"): what a human needs to
// know is WHO else sees the page, not how it is transported. It writes the
// shared preference (ui_state.browser_mode), so every browser on this lasso
// follows it. Agent is disabled while no Chromium is available; the tooltip
// then carries the reason, on a wrapper span since a disabled button fires no
// pointer events.
const MODE_TIPS: Record<BrowserMode, string> = {
  live: "A real Chrome on lasso's machine that you and your agents share. Pages an agent opens show up here, and any site loads.",
  embed:
    "The page loads inside this tab, straight from your own browser. Private to you, but many sites refuse to be embedded.",
}

function BrowserModeSwitch({
  mode,
  liveDisabled,
  liveTitle,
  onPick,
}: {
  mode: BrowserMode
  liveDisabled: boolean
  liveTitle: string
  // Told about every click, so BrowserTab can drop an agent's temporary
  // switch to Agent mode the moment the human picks a mode themselves.
  onPick: () => void
}) {
  const pick = (m: BrowserMode) => {
    onPick()
    if (m !== mode) patchUIState({ browser_mode: m })
  }
  const seg = (m: BrowserMode) =>
    cn(
      "h-7 rounded-none px-2.5 text-[12px] transition-colors",
      mode === m
        ? "bg-primary font-semibold text-primary-foreground hover:bg-primary"
        : "bg-transparent text-muted-foreground hover:bg-muted hover:text-foreground"
    )
  const item = (m: BrowserMode, label: string, disabled = false) => (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="inline-flex">
          <Button
            variant="ghost"
            size="sm"
            className={seg(m)}
            aria-pressed={mode === m}
            disabled={disabled}
            onClick={() => pick(m)}
          >
            {label}
          </Button>
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-64">
        {disabled ? liveTitle : MODE_TIPS[m]}
      </TooltipContent>
    </Tooltip>
  )
  return (
    <TooltipProvider delayDuration={300}>
      <fieldset
        aria-label="Browser mode"
        className="m-0 flex flex-shrink-0 divide-x divide-border overflow-hidden rounded-lg border border-border p-0"
      >
        {item("live", "Agent", liveDisabled)}
        {item("embed", "Iframe")}
      </fieldset>
    </TooltipProvider>
  )
}

// The Browser tab. Two modes behind one toolbar switch:
//
//   - Live: the shared headless Chromium lasso runs on its own machine, shown
//     as a CDP screencast and driven with forwarded input (LiveBrowser). Agents
//     connect to the same Chromium over /cdp, so its tab strip is where their
//     pages show up. Each browser PROFILE (own cookies, own proxy) is its own
//     Chromium; the bar along the bottom picks which one this client shows.
//   - Embed: an iframe, exactly as the tab always was.
//
// Live is the stored default but not always what is shown: a lasso with no
// Chromium (or an older server with no /api/browser) shows Embed with a line
// saying why, without rewriting the preference. What IS shown is published to
// lib/sidebar-browser.ts, since whether a terminal link can open here at all
// depends on it (mixed content only binds an iframe).
//
// `active` is whether a human can see the tab (selected, sidebar open); Live
// streams only then, so an unwatched browser costs lasso a closed socket and
// lets its idle timer stop Chromium.
export function BrowserTab({ active }: { active: boolean }) {
  const queryClient = useQueryClient()
  const pref = useUIState().browser_mode
  const status = useQuery({
    queryKey: qk.browser,
    queryFn: () => api.browserStatus(),
    retry: false,
    staleTime: 5_000,
    // Only while someone could act on it: a Chromium installed, or a proxy
    // changed from another browser, shows up without a reload.
    refetchInterval: active ? 30_000 : false,
  })
  const unavailable = status.isError || status.data?.available === false

  // An agent showing the human a page switches this client to Agent mode
  // without touching the shared preference: it is one agent's page on one
  // screen, not a decision about how every browser shows the tab. Any click
  // on the mode switch hands the choice back.
  const [forceLive, setForceLive] = React.useState(false)
  const mode: BrowserMode =
    (pref === "live" || forceLive) && !unavailable ? "live" : "embed"
  React.useEffect(() => setEffectiveBrowserMode(mode), [mode])

  // ---- profiles ----------------------------------------------------------
  const profiles = profilesOf(status.data)
  const manageable = !!status.data?.profiles?.length
  const [profile, setProfileState] = React.useState(storedProfile)
  // When the selection last changed, so a status fetched BEFORE a profile
  // existed (an agent creating one and opening a page in it straight away)
  // does not read as "that profile is gone" and bounce back to default.
  const pickedAt = React.useRef(0)
  const pickProfile = React.useCallback((id: string) => {
    pickedAt.current = Date.now()
    storeProfile(id)
    setProfileState(id)
  }, [])
  const known = profiles.some((p) => p.id === profile)
  React.useEffect(() => {
    if (!status.data || known) return
    if (status.dataUpdatedAt < pickedAt.current) {
      void queryClient.invalidateQueries({ queryKey: qk.browser })
      return
    }
    pickProfile(DEFAULT_PROFILE)
  }, [status.data, status.dataUpdatedAt, known, pickProfile, queryClient])
  const currentProfile = profiles.find((p) => p.id === profile)
  // Until the status catches up with a just-picked profile, its paths follow
  // the server's convention so the connection can already start.
  const wsPath =
    currentProfile?.ws_path ||
    (profile === DEFAULT_PROFILE
      ? "/cdp"
      : `/cdp/p/${encodeURIComponent(profile)}`)

  const [show, setShow] = React.useState<BrowserShowRequest | null>(null)
  React.useEffect(
    () =>
      onBrowserShowRequest((req) => {
        setForceLive(true)
        pickProfile(req.profile)
        setShow(req)
        const st = queryClient.getQueryData<BrowserStatus>(qk.browser)
        const name =
          profilesOf(st).find((p) => p.id === req.profile)?.name ?? req.profile
        toast(`${req.from} opened a page in ${name}`, {
          description: req.url && req.url !== "about:blank" ? req.url : "",
        })
      }),
    [pickProfile, queryClient]
  )

  const reason = status.isError
    ? `this lasso has no shared browser (${status.error instanceof Error ? status.error.message : "status unavailable"})`
    : status.data?.reason || "no Chromium was found"
  const note =
    (pref === "live" || forceLive) && unavailable
      ? `Agent browser unavailable: ${reason}. Install Chrome or Chromium, or point LASSO_BROWSER at one, to enable it.`
      : ""

  const [openRequest, setOpenRequest] = React.useState<OpenRequest | null>(null)
  React.useEffect(() => {
    let seq = 0
    return onSidebarBrowserOpen((url) => setOpenRequest({ url, seq: ++seq }))
  }, [])
  const onOpened = React.useCallback(() => setOpenRequest(null), [])

  const modeSwitch = (
    <BrowserModeSwitch
      mode={mode}
      liveDisabled={unavailable}
      liveTitle={`Agent browser unavailable: ${reason}`}
      onPick={() => setForceLive(false)}
    />
  )

  return mode === "live" ? (
    <LiveBrowser
      // A profile is a different Chromium, so switching is a fresh
      // connection with fresh state rather than a filter over one browser.
      key={profile}
      profile={profile}
      wsPath={wsPath}
      active={active}
      openRequest={openRequest}
      onOpened={onOpened}
      showRequest={show?.profile === profile ? show : null}
      modeSwitch={modeSwitch}
      footer={
        <BrowserProfileBar
          profiles={profiles}
          current={profile}
          onPick={pickProfile}
          manageable={manageable}
        />
      }
    />
  ) : (
    <EmbedBrowser
      openRequest={openRequest}
      onOpened={onOpened}
      modeSwitch={modeSwitch}
      note={note}
    />
  )
}
