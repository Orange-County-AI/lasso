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

// The Browser tab: a URL bar + preview iframe. For local/tailnet access (plain
// http) it defaults to the same host's dev server on port 3000 — a co-located
// server the browser can actually reach. Behind a TLS terminator (e.g. lasso
// served over https through a Cloudflare tunnel on a public domain) there's no
// co-located :3000, and guessing one yields a cross-origin iframe error, so we
// start blank and let the user type a URL. Either way the URL persists.
export function BrowserTab() {
  const [url, setUrl] = React.useState(() => {
    const saved = lsGet("browserUrl")
    if (saved) return saved
    if (location.protocol === "https:") return ""
    return `http://${location.hostname}:3000`
  })
  const [src, setSrc] = React.useState(() => normalize(url) || "about:blank")
  const [reloadKey, setReloadKey] = React.useState(0)

  const nav = (raw: string) => {
    const u = normalize(raw)
    if (!u) return
    setUrl(u)
    setSrc(u)
    setReloadKey((k) => k + 1)
    lsSet("browserUrl", u)
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex flex-shrink-0 items-center gap-1.5 border-border border-b bg-background px-2 py-1.5">
        <Button
          variant="outline"
          size="icon"
          className="size-7"
          title="reload"
          onClick={() => setReloadKey((k) => k + 1)}
        >
          <RotateCw />
        </Button>
        <Input
          value={url}
          spellCheck={false}
          autoComplete="off"
          placeholder="http://host:3000"
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
            const u = normalize(url)
            if (u) window.open(u, "_blank")
          }}
        >
          <ExternalLink />
        </Button>
      </div>
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
