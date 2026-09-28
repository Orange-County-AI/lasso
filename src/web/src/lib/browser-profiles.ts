import type { BrowserProfileStatus, BrowserStatus } from "@/lib/api"
import { lsGet, lsSet } from "@/lib/app-store"
import { qk, queryClient } from "@/lib/query"

// Browser profiles: each is its own Chromium with its own persistent cookies
// and optional proxy (browser.go). Which one the Browser tab shows is a
// per-device preference, so it lives in localStorage rather than ui_state —
// two people (or a laptop and a phone) looking at different profiles of the
// same lasso is the point of having them.

export const DEFAULT_PROFILE = "default"

const PROFILE_KEY = "browserProfile"

export function storedProfile(): string {
  return lsGet(PROFILE_KEY) || DEFAULT_PROFILE
}

export function storeProfile(id: string) {
  lsSet(PROFILE_KEY, id)
}

// profilesOf lists a status's profiles, synthesizing the one default profile
// an older server (no `profiles` field) implicitly has.
export function profilesOf(
  st: BrowserStatus | undefined
): BrowserProfileStatus[] {
  if (st?.profiles?.length) return st.profiles
  return [
    {
      id: DEFAULT_PROFILE,
      name: "Default",
      proxy: st?.proxy ?? "",
      default: true,
      running: st?.running ?? false,
      started_at: st?.started_at ?? "",
      capped: st?.capped ?? false,
      reason: st?.reason ?? "",
      note: st?.note,
      pages: st?.pages ?? [],
      ws_path: st?.ws_path || "/cdp",
      mcp_path: "/browser-mcp",
    },
  ]
}

// An agent opened a tab (or asked to show one) with the open_browser_tab /
// show_browser_tab MCP tools. The server pushes a `browser-open` SSE event to
// every connected lasso tab; App reveals the Browser tab and BrowserTab
// switches profile and selects the page. Same pub/sub shape as
// lib/open-file.ts, with a per-tab sequence so showing the same page twice
// still re-selects it.
export interface BrowserShowRequest {
  profile: string
  tabId: string
  url: string
  from: string
  seq: number
}

type Listener = (req: BrowserShowRequest) => void
const listeners = new Set<Listener>()

export function onBrowserShowRequest(fn: Listener): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

let seq = 0

// handleBrowserOpenEvent is the SSE listener's body. Only a VISIBLE tab acts,
// for open-file's reason: a background tab that rearranged itself would only
// surprise the human later.
export function handleBrowserOpenEvent(raw: string) {
  if (document.visibilityState !== "visible") return
  let ev: { profile?: unknown; tab_id?: unknown; url?: unknown; from?: unknown }
  try {
    ev = JSON.parse(raw)
  } catch {
    return
  }
  const req: BrowserShowRequest = {
    profile:
      typeof ev?.profile === "string" && ev.profile
        ? ev.profile
        : DEFAULT_PROFILE,
    tabId: typeof ev?.tab_id === "string" ? ev.tab_id : "",
    url: typeof ev?.url === "string" ? ev.url : "",
    from: typeof ev?.from === "string" && ev.from ? ev.from : "an agent",
    seq: ++seq,
  }
  for (const fn of listeners) fn(req)
}

// A profile was created, renamed, re-proxied or deleted somewhere (another
// tab, an agent): refetch the status every selector reads.
export function handleBrowserProfilesEvent() {
  void queryClient.invalidateQueries({ queryKey: qk.browser })
}
