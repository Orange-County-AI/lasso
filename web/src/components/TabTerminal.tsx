import * as React from "react"

import { EmptyWorkspace } from "@/components/EmptyWorkspace"
import { api } from "@/lib/api"
import { HOST_CHANGED_EVENT } from "@/lib/app-store"
import { bootTermFrame, focusTerminal, refitTerminal } from "@/lib/terminal"

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
  //
  // But NOT while the page has never been visible: a browser session-restore
  // can load lasso in a background tab that's never looked at, and its iframe
  // would still connect — holding a tmux client stuck at ttyd's default 80x24
  // forever (xterm never gets layout, so it never fits). Under `window-size
  // latest` that ghost client can win the window size whenever the active
  // client detaches (e.g. leaving grid mode), shrinking the terminal everyone
  // else sees. Defer the attach until the tab is first shown.
  React.useEffect(() => {
    let cancelled = false
    const warm = () => {
      api
        .tabTerm("")
        .then((r) => {
          if (!cancelled) setBase(r.base)
        })
        .catch(() => {})
    }
    if (document.visibilityState !== "hidden") {
      warm()
      return () => {
        cancelled = true
      }
    }
    const onVisible = () => {
      if (document.visibilityState === "hidden") return
      document.removeEventListener("visibilitychange", onVisible)
      warm()
    }
    document.addEventListener("visibilitychange", onVisible)
    return () => {
      cancelled = true
      document.removeEventListener("visibilitychange", onVisible)
    }
  }, [])

  // Point the viewport at the selected tab whenever it changes, and show the
  // loading overlay until that tab's session has actually painted — polled from
  // the backend, since a freshly created shell takes a beat to boot its rc and an
  // existing tab returns ready on the first poll (so the overlay barely flashes).
  React.useEffect(() => {
    if (!base || !tabId) return
    setReady(false)
    // The returned base can CHANGE when the tab lives on a remote host: a remote
    // tab is shown through its own `ssh -tt` ttyd, not the warm local viewport. So
    // adopt the returned base — same value (local→local) is a no-op; a different
    // one remounts the iframe onto the right host's terminal.
    api
      .tabTerm(tabId)
      .then((r) => {
        if (r.base) setBase(r.base)
      })
      .catch(() => {})
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
            focusTerminal(id) // type immediately after creating/selecting a tab
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
  // Skipped while the page is hidden (same ghost-client concern as the warm-up);
  // a visible page recovers on its next tick.
  React.useEffect(() => {
    const t = setInterval(() => {
      if (document.visibilityState === "hidden") return
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

  // When the active host switches (term_rev bump), re-point the viewport at the
  // current tab so its base reflects the new host's terminal (the selected tab may
  // not change, e.g. switching back to a host you already had a tab open on).
  React.useEffect(() => {
    const onHostChange = () => {
      api
        .tabTerm(tabId ?? "")
        .then((r) => {
          if (r.base) setBase(r.base)
        })
        .catch(() => {})
    }
    window.addEventListener(HOST_CHANGED_EVENT, onHostChange)
    return () => window.removeEventListener(HOST_CHANGED_EVENT, onHostChange)
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
      {!tabId && <EmptyWorkspace />}
      {tabId && (!base || !ready) && (
        <div className="absolute inset-0 flex items-center justify-center gap-2 bg-background text-muted-foreground text-sm">
          <span className="size-3.5 animate-spin rounded-full border-2 border-current border-t-transparent" />
          {base ? "starting…" : "attaching…"}
        </div>
      )}
    </div>
  )
}
