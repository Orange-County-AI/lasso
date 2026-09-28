// An agent asked lasso to show the human a file (the `open_file` MCP tool, or
// `lasso open <path>`). The server validates it and pushes an `open-file` SSE
// event to every connected tab; this module is the hand-off from that stream
// to the two places that act on it, which never import each other — FilesPanel
// opens the viewer (it owns the per-pane viewers and the unsaved drafts) and
// App reveals the Files tab in the right sidebar. Same tiny pub/sub shape as
// lib/sidebar-browser.ts. A plugin tab's file.open (lib/plugins.ts) enters
// through the same requestOpenFile, so it gets the same protections.

// The event payload, plus a per-tab sequence number so asking for the same
// file and line twice still re-scrolls.
export interface OpenFileRequest {
  path: string
  host: string
  line?: number
  // A directory opens the Files tree rooted there instead of the viewer.
  dir?: boolean
  // Who asked: the agent's name, or "an agent".
  from: string
  seq: number
}

type Listener = (req: OpenFileRequest) => void
const listeners = new Set<Listener>()

export function onOpenFileRequest(fn: Listener): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

// App registers how to bring the Files tab (and a collapsed sidebar) into
// view. FilesPanel calls it only once it has decided to actually open
// something — a request parked behind an unsaved edit must not rearrange the
// screen around a toast.
const revealers = new Set<() => void>()

export function onRevealFiles(fn: () => void): () => void {
  revealers.add(fn)
  return () => {
    revealers.delete(fn)
  }
}

export function revealFiles() {
  for (const fn of revealers) fn()
}

let seq = 0

// requestOpenFile is the one client-side entry point for "show the human this
// file", shared by the agent path (the SSE event below) and a plugin tab's
// file.open (lib/plugins.ts). Both must land in the same place with the same
// unsaved-edits protection — FilesPanel parks a request behind a toast rather
// than replacing a dirty draft — so neither gets a route of its own.
//
// It hands the request on and returns whether anything was listening. The
// caller decides visibility: an agent's event is gated below, a plugin's call
// by its own tab being on screen.
export function requestOpenFile(req: Omit<OpenFileRequest, "seq">): boolean {
  const full: OpenFileRequest = { ...req, seq: ++seq }
  for (const fn of listeners) fn(full)
  return listeners.size > 0
}

// handleOpenFileEvent is the SSE listener's body. Only a VISIBLE tab acts: the
// event goes to every connected tab, and a background browser tab or a phone
// in a pocket that rearranged itself would only surprise the human later. If
// several tabs are visible at once (two monitors, a laptop beside a phone),
// each opens it — the human is plausibly looking at any of them, and the
// server cannot tell which.
export function handleOpenFileEvent(raw: string) {
  if (document.visibilityState !== "visible") return
  let ev: Partial<OpenFileRequest>
  try {
    ev = JSON.parse(raw)
  } catch {
    return
  }
  if (typeof ev?.path !== "string" || !ev.path.startsWith("/")) return
  requestOpenFile({
    path: ev.path,
    host: typeof ev.host === "string" && ev.host ? ev.host : "local",
    line: typeof ev.line === "number" && ev.line > 0 ? ev.line : undefined,
    dir: ev.dir === true,
    from: typeof ev.from === "string" && ev.from ? ev.from : "an agent",
  })
}

export function baseName(p: string): string {
  const trimmed = p.replace(/\/+$/, "")
  return trimmed.slice(trimmed.lastIndexOf("/") + 1) || p
}
