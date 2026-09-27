// A minimal Chrome DevTools Protocol client over the browser's own WebSocket.
//
// The Browser tab's Live mode is just another CDP client of the shared Chromium
// lasso supervises — a peer of whatever agents are connected — so there is no
// lasso-specific protocol here: lasso's Go side only reverse-proxies /cdp onto
// Chromium's debugging port. One socket speaks to the BROWSER target and every
// page is reached through it with flat sessions (Target.attachToTarget with
// flatten:true), which is why send() takes an optional sessionId and every
// event handler is told which session its event came from.

export type CDPParams = Record<string, unknown>
export type CDPHandler = (params: CDPParams, sessionId?: string) => void

interface Pending {
  resolve: (v: unknown) => void
  reject: (e: Error) => void
  method: string
}

// cdpURL addresses lasso's /cdp — relative to lasso's ORIGIN, never the tab's
// host: the shared browser always runs on lasso's own machine, whatever host a
// tab is driving. wss when lasso is on https (Cloudflare, a TLS proxy), since
// the page may not open a plain ws:// from a secure origin.
export function cdpURL(path = "/cdp"): string {
  const scheme = location.protocol === "https:" ? "wss" : "ws"
  return `${scheme}://${location.host}${path}`
}

// reconnectDelay is the backoff between reconnect attempts: 1s, 2s, 4s … capped
// at 15s. A relaunch (proxy change) is back within a couple of seconds; an idle
// stop needs a start first, which the caller does before reconnecting.
export function reconnectDelay(attempt: number): number {
  return Math.min(15_000, 1000 * 2 ** Math.max(0, attempt))
}

export class CDPError extends Error {
  code?: number
  constructor(message: string, code?: number) {
    super(message)
    this.code = code
  }
}

export class CDPClient {
  private ws: WebSocket
  private nextId = 1
  private pending = new Map<number, Pending>()
  private handlers = new Map<string, Set<CDPHandler>>()
  private closeHandlers = new Set<(ev: CloseEvent | null) => void>()
  private closed = false

  private constructor(ws: WebSocket) {
    this.ws = ws
    ws.onmessage = (ev) => this.onMessage(ev)
    ws.onclose = (ev) => this.finish(ev)
  }

  // connect resolves once the socket is OPEN and rejects if it closes first
  // (lasso answering 503 because no Chromium could start, a 403 from the
  // origin guard, a network drop). A browser reports none of those reasons to
  // script, so the caller asks /api/browser for the why.
  static connect(url = cdpURL(), timeoutMs = 20_000): Promise<CDPClient> {
    return new Promise((resolve, reject) => {
      let ws: WebSocket
      try {
        ws = new WebSocket(url)
      } catch (e) {
        reject(e instanceof Error ? e : new Error(String(e)))
        return
      }
      const timer = window.setTimeout(() => {
        ws.close()
        reject(new Error("timed out connecting to the shared browser"))
      }, timeoutMs)
      ws.onopen = () => {
        window.clearTimeout(timer)
        resolve(new CDPClient(ws))
      }
      ws.onclose = () => {
        window.clearTimeout(timer)
        reject(new Error("couldn't connect to the shared browser"))
      }
    })
  }

  get isOpen(): boolean {
    return !this.closed && this.ws.readyState === WebSocket.OPEN
  }

  send<T = CDPParams>(
    method: string,
    params: CDPParams = {},
    sessionId?: string
  ): Promise<T> {
    if (!this.isOpen) {
      return Promise.reject(new CDPError(`${method}: socket closed`))
    }
    const id = this.nextId++
    const msg: Record<string, unknown> = { id, method, params }
    if (sessionId) msg.sessionId = sessionId
    return new Promise<T>((resolve, reject) => {
      this.pending.set(id, {
        resolve: resolve as (v: unknown) => void,
        reject,
        method,
      })
      try {
        this.ws.send(JSON.stringify(msg))
      } catch (e) {
        this.pending.delete(id)
        reject(e instanceof Error ? e : new Error(String(e)))
      }
    })
  }

  // on subscribes to an event by method name (every session's). Returns the
  // unsubscribe.
  on(method: string, fn: CDPHandler): () => void {
    let set = this.handlers.get(method)
    if (!set) {
      set = new Set()
      this.handlers.set(method, set)
    }
    set.add(fn)
    return () => {
      set.delete(fn)
    }
  }

  // onClose fires once, when the socket goes away for any reason — including
  // close() — with null for a deliberate close.
  onClose(fn: (ev: CloseEvent | null) => void): () => void {
    this.closeHandlers.add(fn)
    return () => {
      this.closeHandlers.delete(fn)
    }
  }

  close() {
    if (this.closed) return
    this.finish(null)
    try {
      this.ws.close()
    } catch {
      /* already gone */
    }
  }

  private onMessage(ev: MessageEvent) {
    if (typeof ev.data !== "string") return
    let msg: {
      id?: number
      method?: string
      params?: CDPParams
      result?: unknown
      error?: { message?: string; code?: number }
      sessionId?: string
    }
    try {
      msg = JSON.parse(ev.data)
    } catch {
      return
    }
    if (typeof msg.id === "number") {
      const p = this.pending.get(msg.id)
      if (!p) return
      this.pending.delete(msg.id)
      if (msg.error) {
        p.reject(
          new CDPError(
            `${p.method}: ${msg.error.message ?? "failed"}`,
            msg.error.code
          )
        )
      } else {
        p.resolve(msg.result ?? {})
      }
      return
    }
    if (!msg.method) return
    const set = this.handlers.get(msg.method)
    if (!set) return
    for (const fn of [...set]) {
      try {
        fn(msg.params ?? {}, msg.sessionId)
      } catch (e) {
        console.error(`cdp ${msg.method} handler`, e)
      }
    }
  }

  // finish settles everything still waiting — a promise left pending on a dead
  // socket would hang whoever awaited it (a screencast restart, a navigate).
  private finish(ev: CloseEvent | null) {
    if (this.closed) return
    this.closed = true
    for (const p of this.pending.values()) {
      p.reject(new CDPError(`${p.method}: socket closed`))
    }
    this.pending.clear()
    for (const fn of [...this.closeHandlers]) fn(ev)
    this.closeHandlers.clear()
    this.handlers.clear()
  }
}
