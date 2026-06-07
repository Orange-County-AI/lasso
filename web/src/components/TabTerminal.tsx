import * as React from "react"

import { api } from "@/lib/api"
import { bootTermFrame, refitTerminal, whenTerminalReady } from "@/lib/terminal"

// The viewport: a SINGLE persistent terminal iframe (one ttyd) that we point at
// whichever tab is selected by re-POSTing /api/tab/term — the backend uses tmux
// `switch-client` to repaint the already-warm client at the new session. The
// iframe is mounted once and never recreated, so the slow xterm⇄ttyd attach
// handshake is paid a single time (at app load, on the park session); switching
// tabs after that is instant. This replaced the per-tab mount/remount that paid
// the handshake on every switch.
const KEEPALIVE_MS = 18000

export function TabTerminal({ tabId }: { tabId: string | null }) {
  const [base, setBase] = React.useState<string | null>(null)
  // Whether the viewport's xterm has painted at least once (handshake done). It
  // stays true across tab switches — only the very first warm-up shows a spinner.
  const [ready, setReady] = React.useState(false)
  const id = "tabterm-viewport"

  // Warm the viewport once on mount (POST with no tab → attach to park). This
  // overlaps the attach handshake with app load so the first selected tab is
  // already warm.
  React.useEffect(() => {
    let cancelled = false
    api
      .tabTerm("")
      .then((r) => {
        if (!cancelled) setBase(r.base)
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [])

  // Point the viewport at the selected tab whenever it changes (instant switch —
  // no iframe churn). Refit after a beat so the new session sizes to the pane.
  React.useEffect(() => {
    if (!base || !tabId) return
    api.tabTerm(tabId).catch(() => {})
    const t = setTimeout(() => refitTerminal(id), 60)
    return () => clearTimeout(t)
  }, [tabId, base])

  // Keepalive: if the viewport ttyd died (crash), respawn it and pick up the new
  // base (which remounts the iframe — the only time we pay the handshake again).
  React.useEffect(() => {
    const t = setInterval(() => {
      api
        .tabTermTouch()
        .then((r) => {
          if (!r.alive)
            api
              .tabTerm(tabId ?? "")
              .then((x) => setBase(x.base))
              .catch(() => {})
        })
        .catch(() => {})
    }, KEEPALIVE_MS)
    return () => clearInterval(t)
  }, [tabId])

  // Wire xterm once the iframe element exists; lift the loading overlay once it
  // has actually painted. base only changes on (rare) respawn.
  // biome-ignore lint/correctness/useExhaustiveDependencies: re-wire when base changes (new iframe)
  React.useEffect(() => {
    if (!base) return
    setReady(false)
    const cleanup = bootTermFrame(id, false)
    refitTerminal(id)
    const cancel = whenTerminalReady(id, () => setReady(true))
    return () => {
      cancel()
      cleanup()
    }
  }, [base, id])

  return (
    <div className="relative flex min-h-0 flex-1 flex-col">
      {/* The iframe stays mounted always (warming on park); hidden until a tab is
          selected so the user never sees the park shell flash. */}
      {base && (
        <iframe
          id={id}
          src={base}
          title="terminal"
          className="frame"
          style={{ display: tabId ? "block" : "none" }}
        />
      )}
      {!tabId && (
        <div className="flex h-full items-center justify-center text-muted-foreground text-sm">
          No tab selected. Create an agent, or pick a workspace.
        </div>
      )}
      {tabId && (!base || !ready) && (
        <div className="absolute inset-0 flex items-center justify-center gap-2 bg-[var(--h-bg)] text-muted-foreground text-sm">
          <span className="size-3.5 animate-spin rounded-full border-2 border-current border-t-transparent" />
          {base ? "starting…" : "attaching…"}
        </div>
      )}
    </div>
  )
}
