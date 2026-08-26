// The host THIS browser tab is looking at.
//
// It used to be a property of the server: POST /api/host swapped one global
// pointer, so opening lasso in a second tab and switching hosts there yanked
// the first tab's terminal, pane list and file viewer onto the new machine. The
// selection now lives here, per tab, and rides on every request.
//
// sessionStorage, deliberately: it is scoped to one tab, so a new tab starts on
// the default host rather than inheriting wherever another tab wandered, and it
// survives a reload, so refreshing does not throw you back to local.

const KEY = "lasso.host"

// hostHeader must match the Go side (src/reqhost.go).
const hostHeader = "X-Lasso-Host"

// Read once at module load. Every access to sessionStorage is guarded: it
// throws outright in some privacy modes, and a storage failure must cost the
// tab its memory of the host, never its ability to run.
let current: string | null = read()

// A ?host= already in the page URL wins over the remembered one on a fresh
// load. That param is lasso's history state (HostSwitcher pushes it, Back
// restores it), so a reload or a shared link must land where it says rather than
// silently ignoring it — and a duplicated tab then starts where its source was.
function read(): string | null {
  try {
    const fromURL = new URLSearchParams(location.search).get("host")
    if (fromURL) {
      sessionStorage.setItem(KEY, fromURL)
      return fromURL
    }
  } catch {
    // No location or no storage; fall through to whatever was remembered.
  }
  try {
    return sessionStorage.getItem(KEY)
  } catch {
    return null
  }
}

// tabHost is this tab's chosen host, or null when it has made no choice and the
// server's default answers.
export function tabHost(): string | null {
  return current
}

export function setTabHost(host: string) {
  current = host
  try {
    sessionStorage.setItem(KEY, host)
  } catch {
    // No persistence across a reload; the in-memory value still holds for now.
  }
}

// withTabHost appends the host as a query param, for the two carriers that
// cannot send a header: EventSource and <iframe> src. A url that already names
// a host is left alone — the sidebar addresses the FOCUSED pane's host that
// way, which is deliberately independent of this tab's selection.
export function withTabHost(url: string): string {
  if (!current || url.includes("host=")) return url
  return `${url}${url.includes("?") ? "&" : "?"}host=${encodeURIComponent(current)}`
}

// hostFetch is fetch() with this tab's host attached. Every call into lasso's
// API goes through it, so a handler always knows which machine the asking tab
// is on. The header is skipped when the tab has made no choice, which is how
// the server's default host stays the answer for a fresh tab.
export function hostFetch(
  input: RequestInfo | URL,
  init?: RequestInit
): Promise<Response> {
  if (!current) return fetch(input, init)
  const headers = new Headers(init?.headers)
  headers.set(hostHeader, current)
  return fetch(input, { ...init, headers })
}
