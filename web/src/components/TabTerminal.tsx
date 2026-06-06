import * as React from "react"

import { api } from "@/lib/api"
import { bootTermFrame, refitTerminal } from "@/lib/terminal"

// One terminal for a tab: a ttyd attached to the tab's tmux session
// (`/api/tab/term` → /tab-term/<token>/). Only the selected tab is mounted; the
// tmux session keeps running detached when we leave (destroy-unattached off), so
// switching tabs is cheap and never loses the agent.
const KEEPALIVE_MS = 18000

// Release is deferred so React StrictMode's dev double-mount (mount → unmount →
// remount, same tab) doesn't kill the ttyd the remount reuses, which would
// 404 the iframe. A real tab switch (different tab unmounts and doesn't come
// back) still releases after the short grace window; remounting the same tab
// cancels its pending release.
const RELEASE_GRACE_MS = 400
const pendingRelease = new Map<string, ReturnType<typeof setTimeout>>()

export function TabTerminal({ tabId }: { tabId: string }) {
  const [base, setBase] = React.useState<string | null>(null)
  const id = `tabterm-${tabId}`

  // Attach on mount; release (deferred) on unmount — detaches the viewer, the
  // tmux session lives on.
  React.useEffect(() => {
    let cancelled = false
    // Re-mounting this tab cancels any release the previous unmount scheduled.
    const pending = pendingRelease.get(tabId)
    if (pending) {
      clearTimeout(pending)
      pendingRelease.delete(tabId)
    }
    setBase(null)
    api
      .tabTerm(tabId)
      .then((r) => {
        if (!cancelled) setBase(r.base)
      })
      .catch(() => {})
    return () => {
      cancelled = true
      const t = setTimeout(() => {
        pendingRelease.delete(tabId)
        api.tabTermRelease(tabId)
      }, RELEASE_GRACE_MS)
      pendingRelease.set(tabId, t)
    }
  }, [tabId])

  // Keepalive; re-attach if the pool reaped us while still mounted.
  React.useEffect(() => {
    const t = setInterval(() => {
      api
        .tabTermTouch(tabId)
        .then((r) => {
          if (!r.alive)
            api
              .tabTerm(tabId)
              .then((x) => setBase(x.base))
              .catch(() => {})
        })
        .catch(() => {})
    }, KEEPALIVE_MS)
    return () => clearInterval(t)
  }, [tabId])

  // Wire xterm once the iframe element exists, and refit when its src lands.
  // biome-ignore lint/correctness/useExhaustiveDependencies: re-wire when base changes (new iframe)
  React.useEffect(() => {
    if (!base) return
    const cleanup = bootTermFrame(id, false)
    refitTerminal(id)
    return cleanup
  }, [base, id])

  if (!base)
    return (
      <div className="flex h-full items-center justify-center text-muted-foreground text-sm">
        attaching…
      </div>
    )
  return <iframe id={id} src={base} title="terminal" className="frame" />
}
