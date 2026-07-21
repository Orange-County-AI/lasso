// Browser-side logging that lands in the SAME place as the backend log. In dev,
// the Go server tees its logger to /tmp/lasso-dev.log and exposes POST /api/log
// (see devlog.go); clog() batches events and posts them there, so a debugging
// session can watch frontend cell/poll/host events interleaved with backend pool
// and terminal events in one time-ordered file.
//
// Gated on import.meta.env.DEV, so the production bundle compiles clog() down to
// a no-op — the /api/log route doesn't exist there anyway.

const ENABLED = import.meta.env.DEV

interface LogEvent {
  msg: string
  data?: unknown
}

let buffer: LogEvent[] = []
let flushTimer: ReturnType<typeof setTimeout> | null = null

// A compact wall-clock stamp (HH:MM:SS.mmm) prefixed to each message so ordering
// within a batch is precise even though the backend timestamps the whole batch
// at receipt (up to FLUSH_MS later).
function stamp(): string {
  const d = new Date()
  const p = (n: number, w = 2) => String(n).padStart(w, "0")
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(
    d.getMilliseconds(),
    3
  )}`
}

const FLUSH_MS = 400

function flush(useBeacon = false) {
  if (flushTimer) {
    clearTimeout(flushTimer)
    flushTimer = null
  }
  if (buffer.length === 0) return
  const events = buffer
  buffer = []
  const body = JSON.stringify({ events })
  // On pagehide/visibility-hidden, a normal fetch may be cancelled — use
  // sendBeacon so the last events (e.g. a release burst) still land.
  if (useBeacon && typeof navigator.sendBeacon === "function") {
    try {
      navigator.sendBeacon("/api/log", new Blob([body], { type: "application/json" }))
      return
    } catch {
      /* fall through to fetch */
    }
  }
  fetch("/api/log", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body,
    keepalive: true,
  }).catch(() => {
    /* logging must never break the app */
  })
}

// clog records one event to the unified dev log (and mirrors it to the browser
// console for immediate local visibility). No-op outside dev.
export function clog(msg: string, data?: unknown): void {
  if (!ENABLED) return
  const line = `${stamp()} ${msg}`
  buffer.push({ msg: line, data })
  // eslint-disable-next-line no-console
  if (data !== undefined) console.debug("[clog]", line, data)
  else console.debug("[clog]", line)
  if (!flushTimer) flushTimer = setTimeout(() => flush(false), FLUSH_MS)
}

if (ENABLED && typeof window !== "undefined") {
  // Best-effort flush of any buffered events when the tab is backgrounded or
  // closed, so a burst right before navigation isn't lost.
  window.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "hidden") flush(true)
  })
  window.addEventListener("pagehide", () => flush(true))
}
