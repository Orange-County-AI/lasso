import type { BrowserMode } from "@/lib/api"
import { uiStateNow } from "@/lib/ui-state"

// A link clicked in a terminal opens in the sidebar's Browser tab instead of a
// new browser tab (Settings → General toggles it). The terminal iframe and the
// app shell never import each other, so the hand-off is a tiny pub/sub: the
// terminal wiring publishes a URL, App reveals the tab and BrowserTab loads it.

type Listener = (url: string) => void
const listeners = new Set<Listener>()

export function onSidebarBrowserOpen(fn: Listener): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

// The mode the Browser tab is ACTUALLY showing, which is not always the stored
// preference: live is stored by default, but a lasso with no Chromium shows
// embed. BrowserTab publishes it; the link routing below is the reader. Starts
// at embed so a link clicked before the tab has ever rendered keeps the
// conservative (mixed-content-aware) routing.
let effectiveMode: BrowserMode = "embed"

export function setEffectiveBrowserMode(m: BrowserMode) {
  effectiveMode = m
}

export function effectiveBrowserMode(): BrowserMode {
  return effectiveMode
}

// routeLinkToSidebar reports whether a terminal link should go to the sidebar.
// Mixed content is the one case decided here rather than in the tab: an https
// lasso cannot embed an http:// page, and a new tab (the old behavior) beats a
// sidebar that opens only to say so. The live browser is exempt — it is a real
// Chromium on lasso's machine, not a frame inside this page, so this page's
// mixed-content rules never see the URL.
export function routeLinkToSidebar(url: string): boolean {
  if (!uiStateNow().terminal_links_in_sidebar) return false
  if (!/^https?:\/\//i.test(url)) return false
  if (effectiveMode === "live") return true
  return location.protocol !== "https:" || /^https:\/\//i.test(url)
}

export function openInSidebarBrowser(url: string) {
  for (const fn of listeners) fn(url)
}
