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
// trusted HTTPS preview URL via /api/preview when lasso itself is served over
// HTTPS (otherwise an http:// iframe would be blocked as mixed content); over
// plain http it just points at the same host's port. Full URLs pass through.
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
        throw new Error((await res.text()).trim() || `preview failed (${res.status})`)
      }
      const data = (await res.json()) as { url: string }
      return data.url
    }
    return `http://${location.hostname}:${port}`
  }
  return normalize(raw)
}

// The Browser tab: a URL bar + preview iframe. Entering a local dev-server port
// (e.g. "5173") gets it a trusted HTTPS origin via tailscale serve when lasso
// is served over HTTPS — see /api/preview. We persist the RAW input so a stale
// HTTPS port self-heals (re-resolves) on reload.
export function BrowserTab() {
  const [url, setUrl] = React.useState(() => lsGet("browserUrl") ?? "")
  const [src, setSrc] = React.useState("about:blank")
  const [reloadKey, setReloadKey] = React.useState(0)
  const [status, setStatus] = React.useState<"idle" | "loading" | "error">("idle")
  const [err, setErr] = React.useState("")

  const nav = React.useCallback(async (raw: string) => {
    const input = raw.trim()
    if (!input) return
    setUrl(input)
    lsSet("browserUrl", input)
    setStatus("loading")
    setErr("")
    try {
      const resolved = await resolve(input)
      setSrc(resolved)
      setReloadKey((k) => k + 1)
      setStatus("idle")
    } catch (e) {
      setSrc("about:blank")
      setErr(e instanceof Error ? e.message : String(e))
      setStatus("error")
    }
  }, [])

  // Resolve any saved value on mount (e.g. a previously-previewed port whose
  // HTTPS forward may need re-creating).
  React.useEffect(() => {
    const saved = lsGet("browserUrl")
    if (saved) void nav(saved)
  }, [nav])

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
          onClick={() => {
            if (src && src !== "about:blank") window.open(src, "_blank")
          }}
        >
          <ExternalLink />
        </Button>
      </div>
      {status === "loading" && (
        <div className="flex-shrink-0 border-border border-b bg-background px-2 py-1 text-[12px] text-muted-foreground">
          resolving…
        </div>
      )}
      {status === "error" && (
        <div className="flex-shrink-0 border-border border-b bg-background px-2 py-1 text-[12px] text-destructive">
          {err}
        </div>
      )}
      <iframe
        key={reloadKey}
        src={src || "about:blank"}
        title="browser preview"
        referrerPolicy="no-referrer"
        className="frame"
      />
    </div>
  )
}
