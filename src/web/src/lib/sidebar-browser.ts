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

// routeLinkToSidebar reports whether a terminal link should go to the sidebar.
// Mixed content is the one case decided here rather than in the tab: an https
// lasso cannot embed an http:// page, and a new tab (the old behavior) beats a
// sidebar that opens only to say so.
export function routeLinkToSidebar(url: string): boolean {
  if (!uiStateNow().terminal_links_in_sidebar) return false
  if (!/^https?:\/\//i.test(url)) return false
  return location.protocol !== "https:" || /^https:\/\//i.test(url)
}

export function openInSidebarBrowser(url: string) {
  for (const fn of listeners) fn(url)
}
