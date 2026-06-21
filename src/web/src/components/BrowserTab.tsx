import { ExternalLink, RotateCw } from "lucide-react"
import * as React from "react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { lsGet, lsSet } from "@/lib/app-store"

function normalize(raw: string): string {
  const u = raw.trim()
  if (!u) return ""
  return /^https?:\/\//i.test(u) ? u : `http://${u}`
}

// parseLocalPort extracts a local dev-server port from user input. It matches a
// bare port ("5173"), a ":PORT" shorthand, or a full URL whose host is
// loopback-ish. Anything else (a real remote URL) returns null so it's left
// alone.
//
// Note the trap: a bare "8445" (or ":8445") is always treated as a LOCAL port
// and routed through /api/preview — it does NOT mean a remote host's :8445. To
// open a remote page, type its full URL (https://host:8445/). The backend
// rejects a bare port that's actually a tailscale HTTPS serve port with a hint.
function parseLocalPort(raw: string): number | null {
  const s = raw.trim()
  if (!s) return null
  if (/^\d{2,5}$/.test(s)) return clampPort(Number(s))
  const colon = s.match(/^:(\d{2,5})$/)
  if (colon) return clampPort(Number(colon[1]))
  try {
    const u = new URL(/^https?:\/\//i.test(s) ? s : `http://${s}`)
    const loopback = ["localhost", "127.0.0.1", "0.0.0.0", "::1", "[::1]"]
    if (loopback.includes(u.hostname) && u.port) {
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

// resolve maps user input to an iframe-able src. A local port is turned into a
// trusted preview URL via /api/preview (a Cloudflare-fronted public hostname
// when lasso is reached over a public origin, otherwise a tailscale-serve HTTPS
// URL). Full URLs pass through.
async function resolve(raw: string): Promise<string> {
  const port = parseLocalPort(raw)
  if (port != null) {
    if (location.protocol === "https:") {
      const res = await fetch("/api/preview", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ port }),
      })
      if (!res.ok) {
        throw new Error(
          (await res.text()).trim() || `preview failed (${res.status})`
        )
      }
      const data = (await res.json()) as { url: string }
      return data.url
    }
    return `http://${location.hostname}:${port}`
  }
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

// The Browser tab: a URL bar + preview iframe. Entering a local dev-server port
// (e.g. "5173") gets it a trusted origin via /api/preview when lasso is served
// over HTTPS. We persist the RAW input so a stale preview self-heals
// (re-resolves) on reload. When a target can't be embedded (unreachable, or a
// private page blocked while lasso is on a public origin), we show a clear
// error and a prominent open-in-new-tab.
export function BrowserTab() {
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

  const nav = React.useCallback(async (raw: string) => {
    const input = raw.trim()
    if (!input) return
    const seq = ++seqRef.current
    setUrl(input)
    lsSet("browserUrl", input)
    setStatus("loading")
    setErr("")

    let resolved: string
    try {
      resolved = await resolve(input)
    } catch (e) {
      if (seq !== seqRef.current) return
      setSrc("about:blank")
      setOpenTarget("")
      setErr(e instanceof Error ? e.message : String(e))
      setStatus("error")
      return
    }
    if (seq !== seqRef.current) return

    setOpenTarget(resolved)

    // Mixed content: an https page can't embed an http:// page.
    if (location.protocol === "https:" && /^http:\/\//i.test(resolved)) {
      setSrc("about:blank")
      setErr(
        "This is an https app, so it can't embed an http:// page (mixed content). Open it in a new tab instead."
      )
      setStatus("error")
      return
    }

    setSrc(resolved)
    setReloadKey((k) => k + 1)

    // Probe reachability/embeddability in parallel. The probe is authoritative:
    // if it fails, the iframe will be blank, so surface a clear reason.
    if (/^https?:\/\//i.test(resolved)) {
      void probeReachable(resolved).then((ok) => {
        if (seq !== seqRef.current || ok) return
        const host = hostOf(resolved)
        const pageIsPublic = !looksPrivateHost(location.host)
        const msg =
          pageIsPublic && looksPrivateHost(host)
            ? `Your browser blocked embedding ${host}. lasso is open over a public address, and browsers won't embed a private/tailnet page in a public one (Private Network Access). It opens fine in its own tab — or open lasso via its tailnet URL to embed tailnet pages.`
            : `Couldn't load ${host || resolved} in the embedded view — it may be unreachable from your browser or refuse embedding. Try opening it in a new tab.`
        setErr(msg)
        setStatus("error")
      })
    }
  }, [])

  // Resolve any saved value on mount (e.g. a previously-previewed port whose
  // forward may need re-creating).
  React.useEffect(() => {
    const saved = lsGet("browserUrl")
    if (saved) void nav(saved)
  }, [nav])

  const openExternal = React.useCallback(() => {
    const t = openTarget || (src !== "about:blank" ? src : "")
    if (t) window.open(t, "_blank", "noopener")
  }, [openTarget, src])

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex flex-shrink-0 items-center gap-1.5 border-border border-b bg-background px-2 py-1.5">
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
          spellCheck={false}
          autoComplete="off"
          placeholder="5173 or http://host:port"
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
      {status === "loading" && (
        <div className="flex-shrink-0 border-border border-b bg-background px-2 py-1 text-[12px] text-muted-foreground">
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
      <iframe
        key={reloadKey}
        src={src || "about:blank"}
        title="browser preview"
        referrerPolicy="no-referrer"
        className="frame"
        onLoad={() => setStatus((s) => (s === "loading" ? "loaded" : s))}
      />
    </div>
  )
}
