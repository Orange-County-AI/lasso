import * as React from "react"

import { api } from "@/lib/api"
import { bootTermFrame, refitTerminal } from "@/lib/terminal"

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
  // Whether the SELECTED tab's session has painted its prompt/frame yet. Reset on
  // every tab switch and driven by polling the backend (does the tmux pane have
  // content), so a freshly created shell shows a "starting…" spinner during its
  // rc boot instead of a blank pane with a bare cursor.
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

  // Point the viewport at the selected tab whenever it changes, and show the
  // loading overlay until that tab's session has actually painted — polled from
  // the backend, since a freshly created shell takes a beat to boot its rc and an
  // existing tab returns ready on the first poll (so the overlay barely flashes).
  React.useEffect(() => {
    if (!base || !tabId) return
    setReady(false)
    api.tabTerm(tabId).catch(() => {})
    const fit = setTimeout(() => refitTerminal(id), 60)
    let cancelled = false
    let timer: ReturnType<typeof setTimeout>
    const deadline = Date.now() + 30000
    const poll = () => {
      if (cancelled) return
      api
        .tabReady(tabId)
        .then((r) => {
          if (cancelled) return
          if (r.ready || Date.now() > deadline) {
            setReady(true)
            refitTerminal(id) // the switch-client redraw landed; size it to the pane
          } else {
            timer = setTimeout(poll, 200)
          }
        })
        .catch(() => {
          if (!cancelled) timer = setTimeout(poll, 400)
        })
    }
    poll()
    return () => {
      cancelled = true
      clearTimeout(fit)
      clearTimeout(timer)
    }
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

  // Wire xterm once the iframe element exists (base only changes on rare respawn).
  // Readiness is driven by the per-tab poll above, not xterm's first paint.
  // biome-ignore lint/correctness/useExhaustiveDependencies: re-wire when base changes (new iframe)
  React.useEffect(() => {
    if (!base) return
    const cleanup = bootTermFrame(id, false)
    refitTerminal(id)
    return cleanup
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
