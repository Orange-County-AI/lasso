import type { BrowserMode } from "@/lib/api"
import { uiStateNow } from "@/lib/ui-state"

// A link clicked in a terminal opens in the sidebar's Browser tab instead of a
// new browser tab (Settings → General toggles it). The terminal iframe and the
// app shell never import each other, so the hand-off is a tiny pub/sub: the
// terminal wiring publishes a URL, App reveals the tab and BrowserTab loads it.

// A link carries the mode it should open in: Iframe by default, since a link
// clicked in a terminal is the human's own page, not one to put in front of
// every agent sharing the Chromium. Agent mode only when an Iframe cannot
// show it at all (below).
export interface SidebarBrowserLink {
  url: string
  mode: BrowserMode
}

type Listener = (link: SidebarBrowserLink) => void
const listeners = new Set<Listener>()

export function onSidebarBrowserOpen(fn: Listener): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

// Whether the Agent (live) browser can be shown here. BrowserTab publishes it
// from /api/browser; it starts false, so a link clicked before the tab has
// ever rendered gets the conservative routing (a new tab for mixed content).
let liveAvailable = false

export function setLiveBrowserAvailable(v: boolean) {
  liveAvailable = v
}

// sidebarLinkMode reports which mode a terminal link should open in, or null
// for a new browser tab. Mixed content is the one case an Iframe cannot take:
// an https lasso cannot embed an http:// page. The Agent browser is exempt (a
// real Chromium on lasso's machine, not a frame inside this page), so such a
// link goes there when it exists, and to a new tab (the old behavior) when it
// does not, which beats a sidebar that opens only to show an error.
export function sidebarLinkMode(url: string): BrowserMode | null {
  if (!uiStateNow().terminal_links_in_sidebar) return null
  if (!/^https?:\/\//i.test(url)) return null
  if (location.protocol !== "https:" || /^https:\/\//i.test(url)) return "embed"
  return liveAvailable ? "live" : null
}

export function openInSidebarBrowser(link: SidebarBrowserLink) {
  for (const fn of listeners) fn(link)
}
