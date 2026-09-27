import { useQueryClient } from "@tanstack/react-query"
import { ArrowLeft, ArrowRight, Keyboard, Plus, RotateCw } from "lucide-react"
import * as React from "react"
import type { OpenRequest } from "@/components/BrowserTab"
import { Button } from "@/components/ui/button"
import { Input, NO_AUTOCORRECT } from "@/components/ui/input"
import { Orb } from "@/components/ui/orb"
import { api, type BrowserPage } from "@/lib/api"
import { lsGet, lsSet } from "@/lib/app-store"
import { resolveLive } from "@/lib/browser-url"
import { CDPClient, type CDPParams, reconnectDelay } from "@/lib/cdp"
import { qk } from "@/lib/query"
import { APP_KEYS, APP_SHIFT_KEYS } from "@/lib/shortcuts"
import { cn } from "@/lib/utils"

// LiveBrowser is the Browser tab's Live mode: the shared headless Chromium
// lasso supervises, drawn from a CDP screencast onto a <canvas> with this
// page's mouse, touch and keyboard forwarded as CDP input. It is one CDP
// client among several — agents connect to the same /cdp — so it never
// assumes it is alone: pages appear and vanish under it
// (Target.setDiscoverTargets, not a list it keeps).
//
// ONE page is shown at a time, with no tab strip: the view follows the page
// most recently opened, so when an agent opens one the human is looking at
// what the agent is working on, and a terminal link loads into the page on
// screen rather than stacking up pages nobody can see. Other pages keep
// running in the browser; they are simply not drawn.
//
// One socket, to the BROWSER target. Pages are reached through flat sessions
// (Target.attachToTarget with flatten:true), so following a new page is a
// detach and an attach on the same connection rather than a new websocket.

type Conn = "starting" | "connecting" | "live" | "reconnecting" | "error"

interface TargetInfo {
  targetId: string
  type: string
  url: string
  title: string
}

// The screencast's per-frame metadata (Page.ScreencastFrameMetadata).
// deviceWidth/deviceHeight are the page's viewport in CSS px — the space
// Input.dispatchMouseEvent's x/y are in — whatever size the JPEG arrived at.
interface FrameMeta {
  deviceWidth: number
  deviceHeight: number
  offsetTop: number
  pageScaleFactor: number
}

// Where the last frame landed on the canvas, in CSS px relative to the
// canvas, plus the metadata it came with. Input maps back through this.
interface DrawRect {
  x: number
  y: number
  w: number
  h: number
  meta: FrameMeta
}

// sameURL compares two absolute URLs as the browser would ("https://a.com"
// and "https://a.com/" are one page); unparseable input is simply unequal.
function sameURL(a: string, b: string): boolean {
  try {
    return new URL(a).href === new URL(b).href
  } catch {
    return a === b
  }
}

// The headless window's non-viewport height (see fit). A property of the
// browser, not of one mount, so a Live -> Embed -> Live round trip keeps it.
let chromeGap: number | null = null

// Editing chords go to the page as key events carrying the editor COMMAND,
// which is what makes them work in a headless Chromium on any OS: the
// platform's own key bindings (⌘ on a Mac, Ctrl elsewhere) are not consulted
// for synthetic input. ⌘V/Ctrl+V is absent on purpose — it is left to fire
// the real `paste` event, whose text goes out as Input.insertText.
function editCommand(e: KeyboardEvent | React.KeyboardEvent): string | null {
  switch (e.key.toLowerCase()) {
    case "a":
      return "selectAll"
    case "c":
      return "copy"
    case "x":
      return "cut"
    case "z":
      return e.shiftKey ? "redo" : "undo"
    case "y":
      return "redo"
    default:
      return null
  }
}

// CDP's modifier bitmask: Alt=1, Ctrl=2, Meta=4, Shift=8.
function modifiers(e: {
  altKey: boolean
  ctrlKey: boolean
  metaKey: boolean
  shiftKey: boolean
}): number {
  return (
    (e.altKey ? 1 : 0) |
    (e.ctrlKey ? 2 : 0) |
    (e.metaKey ? 4 : 0) |
    (e.shiftKey ? 8 : 0)
  )
}

const BUTTON_NAMES = ["left", "middle", "right", "back", "forward"]

// keyEvents builds the Input.dispatchKeyEvent params for one DOM key event.
// A printable key goes out as keyDown WITH its text (that is what types it);
// everything else as rawKeyDown with the virtual key code, which is what
// Chromium's own key handling (arrows, Backspace, Tab) keys off. Enter carries
// "\r" as its text, the way a real keyboard's does, or a form never submits.
function keyEvent(
  e: KeyboardEvent | React.KeyboardEvent,
  down: boolean,
  command?: string
): CDPParams {
  const base: CDPParams = {
    modifiers: modifiers(e),
    key: e.key,
    code: e.code,
    // keyCode is deprecated in the DOM, but it IS the Windows virtual key code
    // CDP asks for, and there is no other source of it.
    windowsVirtualKeyCode: e.keyCode,
    nativeVirtualKeyCode: e.keyCode,
    location: e.location,
    autoRepeat: e.repeat,
  }
  if (!down) return { ...base, type: "keyUp" }
  if (command) return { ...base, type: "rawKeyDown", commands: [command] }
  const text =
    e.key === "Enter"
      ? "\r"
      : e.key.length === 1 && !e.ctrlKey && !e.metaKey
        ? e.key
        : ""
  return text
    ? { ...base, type: "keyDown", text, unmodifiedText: text }
    : { ...base, type: "rawKeyDown" }
}

// namedKey is one keystroke the hidden phone textarea can only report by name.
function namedKey(key: "Enter" | "Backspace"): CDPParams[] {
  const code = key === "Enter" ? 13 : 8
  const common = {
    key,
    code: key,
    windowsVirtualKeyCode: code,
    nativeVirtualKeyCode: code,
  }
  const down =
    key === "Enter"
      ? { ...common, type: "keyDown", text: "\r", unmodifiedText: "\r" }
      : { ...common, type: "rawKeyDown" }
  return [down, { ...common, type: "keyUp" }]
}

// The target a remount resumes on. Module-level on purpose: switching the
// sidebar to Files and back unmounts nothing (the Pane is hidden, not
// removed), but a mode switch or an ErrorBoundary reset does, and landing on
// some other page afterwards would read as the browser having navigated.
let lastSelected: string | null = null

function useDocumentVisible(): boolean {
  const [visible, setVisible] = React.useState(
    () => document.visibilityState === "visible"
  )
  React.useEffect(() => {
    const on = () => setVisible(document.visibilityState === "visible")
    document.addEventListener("visibilitychange", on)
    return () => document.removeEventListener("visibilitychange", on)
  }, [])
  return visible
}

function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

export function LiveBrowser({
  active,
  openRequest,
  onOpened,
  modeSwitch,
}: {
  active: boolean
  openRequest: OpenRequest | null
  onOpened: () => void
  modeSwitch: React.ReactNode
}) {
  const queryClient = useQueryClient()
  const visible = useDocumentVisible()
  const streaming = active && visible

  const [conn, setConn] = React.useState<Conn>("starting")
  const [err, setErr] = React.useState("")
  const [client, setClient] = React.useState<CDPClient | null>(null)
  const [pages, setPages] = React.useState<BrowserPage[]>([])
  const [selected, setSelectedState] = React.useState<string | null>(
    lastSelected
  )
  const [urlInput, setUrlInput] = React.useState("")
  // Bumped to force a re-attach to the same target (its session detached
  // under us — a crash, or another client's Target.detachFromTarget).
  const [attachNonce, setAttachNonce] = React.useState(0)
  const [retryNonce, setRetryNonce] = React.useState(0)

  const canvasRef = React.useRef<HTMLCanvasElement>(null)
  const boxRef = React.useRef<HTMLDivElement>(null)
  const kbdRef = React.useRef<HTMLTextAreaElement>(null)
  const urlFocused = React.useRef(false)
  const clientRef = React.useRef<CDPClient | null>(null)
  const sessionRef = React.useRef<{
    targetId: string
    sessionId: string
  } | null>(null)
  const drawRef = React.useRef<DrawRect | null>(null)
  const lastFrame = React.useRef<{
    img: HTMLImageElement
    meta: FrameMeta
  } | null>(null)
  const targets = React.useRef(new Map<string, BrowserPage>())
  // A URL to open once a socket exists: a terminal link aimed at a tab that
  // was collapsed a moment ago, and so not yet connected.
  const pendingOpen = React.useRef<string | null>(null)
  // Whether this mount has carried the shared address over yet (see the
  // attach effect and the browserUrl write-through).
  const urlSynced = React.useRef(false)
  const selectedRef = React.useRef(selected)
  selectedRef.current = selected

  const select = React.useCallback((id: string | null) => {
    lastSelected = id
    setSelectedState(id)
  }, [])

  // ---- drawing -----------------------------------------------------------

  // paint draws a frame letterboxed into the canvas (aspect preserved) and
  // records where it landed, which is what input maps back through.
  const paint = React.useCallback((img: HTMLImageElement, meta: FrameMeta) => {
    const canvas = canvasRef.current
    if (!canvas) return
    const ctx = canvas.getContext("2d")
    if (!ctx) return
    const dpr = window.devicePixelRatio || 1
    const cw = canvas.width
    const ch = canvas.height
    const iw = img.naturalWidth
    const ih = img.naturalHeight
    if (!iw || !ih || !cw || !ch) return
    const scale = Math.min(cw / iw, ch / ih)
    const dw = iw * scale
    const dh = ih * scale
    const dx = (cw - dw) / 2
    const dy = (ch - dh) / 2
    ctx.clearRect(0, 0, cw, ch)
    ctx.drawImage(img, dx, dy, dw, dh)
    drawRef.current = {
      x: dx / dpr,
      y: dy / dpr,
      w: dw / dpr,
      h: dh / dpr,
      meta,
    }
  }, [])

  // ---- viewport ----------------------------------------------------------

  // fit sizes the canvas's backing store to its box, the shared page's window
  // to the same CSS size, and (re)starts the screencast at that size times the
  // device pixel ratio — a JPEG no larger than what the canvas can show.
  const fit = React.useCallback(async () => {
    const c = clientRef.current
    const s = sessionRef.current
    const canvas = canvasRef.current
    const box = boxRef.current
    if (!canvas || !box) return
    const r = box.getBoundingClientRect()
    const w = Math.round(r.width)
    const h = Math.round(r.height)
    if (w < 40 || h < 40) return
    const dpr = window.devicePixelRatio || 1
    const bw = Math.round(w * dpr)
    const bh = Math.round(h * dpr)
    if (canvas.width !== bw || canvas.height !== bh) {
      canvas.width = bw
      canvas.height = bh
      if (lastFrame.current) {
        paint(lastFrame.current.img, lastFrame.current.meta)
      }
    }
    if (!c?.isOpen || !s) return
    // The window, not an emulation override, is the first choice: an override
    // is per-session and would leave an agent attached to the same page
    // looking at a different layout than the human. Sharpness is not set
    // here: the screencast ignores emulated scale factors, so lasso launches
    // Chromium at the right one (--force-device-scale-factor, browser.go).
    try {
      const { windowId } = await c.send<{ windowId: number }>(
        "Browser.getWindowForTarget",
        { targetId: s.targetId }
      )
      // Headless's window is taller than its viewport (measured: 87px of
      // invisible chrome), so a window sized to the box leaves the page short
      // and the frame letterboxed. The gap is read as outer minus inner height
      // — one snapshot, so it holds whatever size the window is at — rather
      // than by resizing and re-reading innerHeight, which answers before
      // the resize lands and so reads the previous size.
      if (chromeGap === null) {
        chromeGap = await c
          .send<{ result: { value?: number } }>(
            "Runtime.evaluate",
            {
              expression: "window.outerHeight - window.innerHeight",
              returnByValue: true,
            },
            s.sessionId
          )
          .then((r) => {
            const v = r.result.value
            return typeof v === "number" && v >= 0 && v < 400 ? v : null
          })
          .catch(() => null)
      }
      await c.send("Browser.setWindowBounds", {
        windowId,
        bounds: {
          width: w,
          height: h + (chromeGap ?? 0),
          windowState: "normal",
        },
      })
    } catch {
      await c
        .send(
          "Emulation.setDeviceMetricsOverride",
          { width: w, height: h, deviceScaleFactor: 0, mobile: false },
          s.sessionId
        )
        .catch(() => {})
    }
    if (sessionRef.current !== s) return
    // Screencast parameters are fixed at start, so a resize restarts it.
    await c.send("Page.stopScreencast", {}, s.sessionId).catch(() => {})
    await c
      .send(
        "Page.startScreencast",
        { format: "jpeg", quality: 80, maxWidth: bw, maxHeight: bh },
        s.sessionId
      )
      .catch(() => {})
  }, [paint])

  React.useEffect(() => {
    const box = boxRef.current
    if (!box) return
    let t = 0
    const ro = new ResizeObserver(() => {
      window.clearTimeout(t)
      t = window.setTimeout(() => void fit(), 150)
    })
    ro.observe(box)
    return () => {
      window.clearTimeout(t)
      ro.disconnect()
    }
  }, [fit])

  // ---- connection ---------------------------------------------------------

  // Connected only while a human can see the tab. Hidden, the socket closes
  // — which is what lets lasso's idle timer stop Chromium — and it comes back
  // (starting Chromium first if it was stopped) when the tab is shown again.
  // A drop while visible (a relaunch for a proxy change, an idle stop racing a
  // reveal, lasso restarting) reconnects with backoff.
  // biome-ignore lint/correctness/useExhaustiveDependencies: retryNonce is the manual retry trigger
  React.useEffect(() => {
    if (!streaming) return
    let cancelled = false
    let timer = 0
    let attempt = 0
    let c: CDPClient | null = null

    const schedule = () => {
      if (cancelled) return
      timer = window.setTimeout(run, reconnectDelay(attempt))
      attempt++
    }

    const run = async () => {
      if (cancelled) return
      setConn(attempt === 0 ? "starting" : "reconnecting")
      try {
        let st = await api.browserStatus()
        queryClient.setQueryData(qk.browser, st)
        // Unavailable is not retried here: BrowserTab reads the same cache
        // entry and swaps to Embed with the reason.
        if (!st.available) {
          throw new Error(st.reason || "the shared browser is unavailable")
        }
        if (!st.running) {
          st = await api.browserAction("start")
          queryClient.setQueryData(qk.browser, st)
        }
        if (cancelled) return
        setConn("connecting")
        const next = await CDPClient.connect()
        if (cancelled) {
          next.close()
          return
        }
        c = next
        attempt = 0
        targets.current.clear()
        // Pages that already exist are announced as targetCreated too, when
        // discovery starts; only one created AFTER that is new, and a new page
        // is what the view follows.
        let discovered = false
        const sync = () => setPages([...targets.current.values()])
        const upsert = (p: CDPParams) => {
          const t = p.targetInfo as TargetInfo | undefined
          if (t?.type !== "page") return
          targets.current.set(t.targetId, {
            id: t.targetId,
            url: t.url,
            title: t.title,
          })
          sync()
        }
        next.on("Target.targetCreated", (p) => {
          upsert(p)
          const t = p.targetInfo as TargetInfo | undefined
          if (discovered && t?.type === "page") select(t.targetId)
        })
        next.on("Target.targetInfoChanged", upsert)
        next.on("Target.targetDestroyed", (p) => {
          if (targets.current.delete(p.targetId as string)) sync()
        })
        next.on("Target.detachedFromTarget", (p) => {
          const s = sessionRef.current
          if (!s || p.sessionId !== s.sessionId) return
          sessionRef.current = null
          // The page is still there (a destroyed one also leaves the list,
          // which reselects): attach to it again.
          if (targets.current.has(s.targetId)) setAttachNonce((n) => n + 1)
        })
        next.onClose((ev) => {
          if (ev === null || cancelled) return
          clientRef.current = null
          sessionRef.current = null
          setClient(null)
          setConn("reconnecting")
          schedule()
        })
        // Emits targetCreated for every page that already exists, so the
        // list fills without a separate Target.getTargets.
        await next.send("Target.setDiscoverTargets", { discover: true })
        discovered = true
        clientRef.current = next
        setClient(next)
        setErr("")
        setConn("live")
      } catch (e) {
        if (cancelled) return
        c?.close()
        c = null
        setErr(errText(e))
        const st = queryClient.getQueryData<{ available?: boolean }>(qk.browser)
        if (st?.available === false) return
        setConn("reconnecting")
        schedule()
      }
    }

    void run()
    return () => {
      cancelled = true
      window.clearTimeout(timer)
      const s = sessionRef.current
      if (c?.isOpen && s) {
        void c.send("Page.stopScreencast", {}, s.sessionId).catch(() => {})
      }
      c?.close()
      clientRef.current = null
      sessionRef.current = null
      setClient(null)
    }
  }, [streaming, queryClient, retryNonce, select])

  // Keep a selection whenever there is anything to select: the one this tab
  // (or its last mount) had, else the first page. A selected page closing —
  // by an agent, or by the × here — moves on to another rather than leaving a
  // frozen last frame.
  //
  // An EMPTY list keeps the selection: it is also what a fresh connection
  // reads for the instant before discovery reports what is open, and
  // clearing it then would drop this tab's page for whichever comes first.
  React.useEffect(() => {
    if (!client || pages.length === 0) return
    const cur = selectedRef.current
    if (cur && pages.some((p) => p.id === cur)) return
    select(pages[0].id)
  }, [client, pages, select])

  // Attach only to a page discovery has reported: a remembered id from
  // before a relaunch names a target that no longer exists.
  const attachable =
    selected && pages.some((p) => p.id === selected) ? selected : null

  // ---- the selected page's session -----------------------------------------

  // biome-ignore lint/correctness/useExhaustiveDependencies: attachNonce forces a re-attach to the same target
  React.useEffect(() => {
    const selected = attachable
    if (!client || !selected) return
    let dead = false
    let sessionId: string | undefined
    const off = client.on("Page.screencastFrame", (p, sid) => {
      if (!sessionId || sid !== sessionId) return
      const ack = () =>
        void client
          .send(
            "Page.screencastFrameAck",
            { sessionId: p.sessionId },
            sessionId
          )
          .catch(() => {})
      const meta = p.metadata as FrameMeta
      const img = new Image()
      // Acked once drawn, not on arrival: Chromium sends the next frame only
      // after the ack, so this is what keeps a slow decode from queueing
      // frames nobody will see.
      img.onload = () => {
        if (dead) return
        lastFrame.current = { img, meta }
        paint(img, meta)
        ack()
      }
      img.onerror = ack
      img.src = `data:image/jpeg;base64,${p.data as string}`
    })
    ;(async () => {
      const r = await client.send<{ sessionId: string }>(
        "Target.attachToTarget",
        { targetId: selected, flatten: true }
      )
      if (dead) {
        void client
          .send("Target.detachFromTarget", { sessionId: r.sessionId })
          .catch(() => {})
        return
      }
      sessionId = r.sessionId
      sessionRef.current = { targetId: selected, sessionId }
      await client.send("Page.enable", {}, sessionId)
      await fit()
      // First attach of this mount: carry over the address Iframe mode (or
      // the last visit) left, so switching modes keeps the page. Done here,
      // once a session exists, because navigating without one opens a new
      // page. A page already there is left alone.
      if (!urlSynced.current && !dead) {
        urlSynced.current = true
        const want = resolveLive(lsGet("browserUrl") ?? "")
        const have = targets.current.get(selected)?.url ?? ""
        if (want && !sameURL(want, have)) {
          await client
            .send("Page.navigate", { url: want }, sessionId)
            .catch(() => {})
        }
      }
    })().catch((e) => {
      if (!dead) setErr(errText(e))
    })
    return () => {
      dead = true
      off()
      if (sessionRef.current?.sessionId === sessionId) {
        sessionRef.current = null
      }
      if (sessionId && client.isOpen) {
        void client
          .send("Page.stopScreencast", {}, sessionId)
          .catch(() => {})
          .then(() =>
            client
              .send("Target.detachFromTarget", { sessionId })
              .catch(() => {})
          )
      }
    }
  }, [client, attachable, attachNonce, fit, paint])

  // The URL bar follows the selected page — its own navigations, and ones an
  // agent makes — except while someone is typing in it.
  const current = pages.find((p) => p.id === selected)
  const currentURL = current?.url ?? ""
  React.useEffect(() => {
    if (urlFocused.current) return
    setUrlInput(currentURL === "about:blank" ? "" : currentURL)
  }, [currentURL])

  // The address is shared with Iframe mode (one "browserUrl" slot), so a
  // mode switch shows the same page: this records where the shown page is,
  // and the attach below loads the slot into it. Held off until that first
  // sync has run, or the page's old URL would overwrite the one being
  // carried over before it was ever applied.
  React.useEffect(() => {
    if (!urlSynced.current || !currentURL || currentURL === "about:blank") {
      return
    }
    lsSet("browserUrl", currentURL)
  }, [currentURL])

  // ---- page actions ---------------------------------------------------------

  const openNew = React.useCallback(
    async (url: string) => {
      const c = clientRef.current
      if (!c?.isOpen) {
        pendingOpen.current = url
        return
      }
      try {
        const { targetId } = await c.send<{ targetId: string }>(
          "Target.createTarget",
          { url }
        )
        select(targetId)
      } catch (e) {
        setErr(errText(e))
      }
    },
    [select]
  )

  // A terminal link: always a NEW page, never the selected one — an agent may
  // be mid-task in it, and navigating its page away would be taking the
  // browser out from under it.
  React.useEffect(() => {
    if (!openRequest) return
    onOpened()
    void openNew(openRequest.url)
  }, [openRequest, onOpened, openNew])

  React.useEffect(() => {
    if (!client || !pendingOpen.current) return
    const url = pendingOpen.current
    pendingOpen.current = null
    void openNew(url)
  }, [client, openNew])

  const navigate = React.useCallback(
    async (raw: string) => {
      const url = resolveLive(raw)
      if (!url) return
      const c = clientRef.current
      const s = sessionRef.current
      if (!c?.isOpen || !s) {
        void openNew(url)
        return
      }
      try {
        const r = await c.send<{ errorText?: string }>(
          "Page.navigate",
          { url },
          s.sessionId
        )
        setErr(r.errorText ? `${url}: ${r.errorText}` : "")
      } catch (e) {
        setErr(errText(e))
      }
      canvasRef.current?.focus()
    },
    [openNew]
  )

  // A terminal link loads into the page on screen (one page at a time; see
  // the header). With no page yet, navigate opens one.
  React.useEffect(() => {
    if (!openRequest) return
    onOpened()
    void navigate(openRequest.url)
  }, [openRequest, onOpened, navigate])

  React.useEffect(() => {
    if (!client || !pendingOpen.current) return
    const url = pendingOpen.current
    pendingOpen.current = null
    void navigate(url)
  }, [client, navigate])

  const history = React.useCallback(async (delta: -1 | 1) => {
    const c = clientRef.current
    const s = sessionRef.current
    if (!c?.isOpen || !s) return
    try {
      const h = await c.send<{
        currentIndex: number
        entries: { id: number }[]
      }>("Page.getNavigationHistory", {}, s.sessionId)
      const entry = h.entries[h.currentIndex + delta]
      if (entry) {
        await c.send(
          "Page.navigateToHistoryEntry",
          { entryId: entry.id },
          s.sessionId
        )
      }
    } catch (e) {
      setErr(errText(e))
    }
  }, [])

  const reload = React.useCallback(() => {
    const c = clientRef.current
    const s = sessionRef.current
    if (!c?.isOpen || !s) return
    void c.send("Page.reload", {}, s.sessionId).catch((e) => setErr(errText(e)))
  }, [])

  // ---- input ----------------------------------------------------------------

  const send = React.useCallback((method: string, params: CDPParams) => {
    const c = clientRef.current
    const s = sessionRef.current
    if (!c?.isOpen || !s) return
    void c.send(method, params, s.sessionId).catch(() => {})
  }, [])

  // toPage maps a client-space point to the page's CSS px, through where the
  // last frame was drawn. The frame is the page's viewport scaled to fit, so
  // one factor — drawn width over the viewport's CSS width — covers both
  // axes. Points in the letterbox bars answer null.
  const toPage = React.useCallback((clientX: number, clientY: number) => {
    const canvas = canvasRef.current
    const d = drawRef.current
    if (!canvas || !d?.meta.deviceWidth) return null
    const r = canvas.getBoundingClientRect()
    const px = clientX - r.left - d.x
    const py = clientY - r.top - d.y
    if (px < 0 || py < 0 || px > d.w || py > d.h) return null
    const s = d.w / d.meta.deviceWidth
    return { x: Math.round(px / s), y: Math.round(py / s), scale: s }
  }, [])

  // Pointer state lives in a ref: it changes at pointer rate and nothing
  // renders from it.
  const ptr = React.useRef({
    raf: 0,
    move: null as CDPParams | null,
    wheel: null as { x: number; y: number; dx: number; dy: number } | null,
    lastDown: { t: 0, x: 0, y: 0, count: 0 },
    touch: null as {
      id: number
      sx: number
      sy: number
      lx: number
      ly: number
      dragged: boolean
    } | null,
  })

  // One mouse move and one accumulated wheel per animation frame: a pointer
  // reports far faster than a screencast can show, and every message is a
  // websocket round trip to a Chromium that may be software-rasterizing.
  const flush = React.useCallback(() => {
    const p = ptr.current
    p.raf = 0
    if (p.move) send("Input.dispatchMouseEvent", p.move)
    p.move = null
    if (p.wheel) {
      send("Input.dispatchMouseEvent", {
        type: "mouseWheel",
        x: p.wheel.x,
        y: p.wheel.y,
        deltaX: p.wheel.dx,
        deltaY: p.wheel.dy,
      })
    }
    p.wheel = null
  }, [send])

  const queue = React.useCallback(() => {
    const p = ptr.current
    if (!p.raf) p.raf = requestAnimationFrame(flush)
  }, [flush])

  const addWheel = React.useCallback(
    (x: number, y: number, dx: number, dy: number) => {
      const p = ptr.current
      if (p.wheel) {
        p.wheel.dx += dx
        p.wheel.dy += dy
      } else {
        p.wheel = { x, y, dx, dy }
      }
      queue()
    },
    [queue]
  )

  const click = React.useCallback(
    (x: number, y: number) => {
      const base = { x, y, button: "left", clickCount: 1, modifiers: 0 }
      send("Input.dispatchMouseEvent", { ...base, type: "mouseMoved" })
      send("Input.dispatchMouseEvent", {
        ...base,
        type: "mousePressed",
        buttons: 1,
      })
      send("Input.dispatchMouseEvent", {
        ...base,
        type: "mouseReleased",
        buttons: 0,
      })
    },
    [send]
  )

  const onPointerDown = (e: React.PointerEvent<HTMLCanvasElement>) => {
    const canvas = e.currentTarget
    canvas.focus({ preventScroll: true })
    const pt = toPage(e.clientX, e.clientY)
    if (!pt) return
    const p = ptr.current
    if (e.pointerType === "touch") {
      // One finger drives; a second is ignored rather than guessed at.
      if (p.touch) return
      canvas.setPointerCapture(e.pointerId)
      p.touch = {
        id: e.pointerId,
        sx: e.clientX,
        sy: e.clientY,
        lx: e.clientX,
        ly: e.clientY,
        dragged: false,
      }
      return
    }
    e.preventDefault()
    canvas.setPointerCapture(e.pointerId)
    // A press carries its own position, so a move still queued would only
    // arrive after it — out of order.
    p.move = null
    const now = performance.now()
    const near =
      Math.abs(e.clientX - p.lastDown.x) < 5 &&
      Math.abs(e.clientY - p.lastDown.y) < 5
    const count = now - p.lastDown.t < 500 && near ? p.lastDown.count + 1 : 1
    p.lastDown = { t: now, x: e.clientX, y: e.clientY, count }
    send("Input.dispatchMouseEvent", {
      type: "mousePressed",
      x: pt.x,
      y: pt.y,
      button: BUTTON_NAMES[e.button] ?? "none",
      buttons: e.buttons,
      clickCount: count,
      modifiers: modifiers(e),
    })
  }

  const onPointerMove = (e: React.PointerEvent<HTMLCanvasElement>) => {
    const p = ptr.current
    if (e.pointerType === "touch") {
      const t = p.touch
      if (!t || t.id !== e.pointerId) return
      if (!t.dragged && Math.hypot(e.clientX - t.sx, e.clientY - t.sy) > 8) {
        t.dragged = true
      }
      if (t.dragged) {
        const at = toPage(t.sx, t.sy)
        if (at) {
          // Natural direction: dragging the finger up scrolls the page down.
          addWheel(
            at.x,
            at.y,
            -(e.clientX - t.lx) / at.scale,
            -(e.clientY - t.ly) / at.scale
          )
        }
      }
      t.lx = e.clientX
      t.ly = e.clientY
      return
    }
    const pt = toPage(e.clientX, e.clientY)
    if (!pt) return
    const held = [1, 4, 2, 8, 16].findIndex((bit) => e.buttons & bit)
    p.move = {
      type: "mouseMoved",
      x: pt.x,
      y: pt.y,
      button: held >= 0 ? BUTTON_NAMES[held] : "none",
      buttons: e.buttons,
      modifiers: modifiers(e),
    }
    queue()
  }

  const onPointerUp = (e: React.PointerEvent<HTMLCanvasElement>) => {
    const p = ptr.current
    if (e.pointerType === "touch") {
      const t = p.touch
      if (!t || t.id !== e.pointerId) return
      p.touch = null
      if (!t.dragged) {
        const pt = toPage(e.clientX, e.clientY)
        if (pt) click(pt.x, pt.y)
      }
      return
    }
    const pt = toPage(e.clientX, e.clientY) ?? {
      // Released over the letterbox: still release, at the edge it left by,
      // or the page is left holding a button down.
      x: 0,
      y: 0,
    }
    p.move = null
    send("Input.dispatchMouseEvent", {
      type: "mouseReleased",
      x: pt.x,
      y: pt.y,
      button: BUTTON_NAMES[e.button] ?? "none",
      buttons: e.buttons,
      clickCount: p.lastDown.count,
      modifiers: modifiers(e),
    })
  }

  const onPointerCancel = (e: React.PointerEvent<HTMLCanvasElement>) => {
    if (ptr.current.touch?.id === e.pointerId) ptr.current.touch = null
  }

  // Wheel needs a non-passive listener to preventDefault (React's is
  // passive), or the sidebar scrolls along with the page.
  React.useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas) return
    const onWheel = (e: WheelEvent) => {
      e.preventDefault()
      const pt = toPage(e.clientX, e.clientY)
      if (!pt) return
      const unit =
        e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? canvas.clientHeight : 1
      addWheel(pt.x, pt.y, e.deltaX * unit, e.deltaY * unit)
    }
    canvas.addEventListener("wheel", onWheel, { passive: false })
    return () => canvas.removeEventListener("wheel", onWheel)
  }, [toPage, addWheel])

  React.useEffect(
    () => () => {
      if (ptr.current.raf) cancelAnimationFrame(ptr.current.raf)
    },
    []
  )

  const onKey = (e: React.KeyboardEvent<HTMLCanvasElement>, down: boolean) => {
    if (e.nativeEvent.isComposing) return
    if (e.metaKey || e.ctrlKey) {
      const k = e.key.toLowerCase()
      // lasso's own ⌘ shortcuts (lib/shortcuts.ts) bubble on to App's
      // document listener, exactly as they do from anywhere else in the app.
      if (
        e.metaKey &&
        !e.ctrlKey &&
        !e.altKey &&
        (e.shiftKey ? APP_SHIFT_KEYS : APP_KEYS).has(k)
      ) {
        return
      }
      // Left for the real paste event (onPaste), which carries the text.
      if (k === "v") return
      const cmd = editCommand(e)
      // Any other ⌘ chord (⌘L, ⌘R, ⌘T …) is this browser's, not the page's.
      if (e.metaKey && !cmd) return
      e.preventDefault()
      send("Input.dispatchKeyEvent", keyEvent(e, down, cmd ?? undefined))
      return
    }
    // Everything else is the page's — including Tab, which would otherwise
    // move focus off the canvas, and Space/arrows, which would scroll ours.
    e.preventDefault()
    send("Input.dispatchKeyEvent", keyEvent(e, down))
  }

  const insertText = React.useCallback(
    (text: string) => {
      if (text) send("Input.insertText", { text })
    },
    [send]
  )

  // ---- phone keyboard ---------------------------------------------------------

  // A canvas cannot raise a software keyboard, so the toolbar's keyboard
  // button focuses a visually hidden textarea instead. What is typed goes out
  // as text and the textarea is emptied again; Enter and Backspace, which an
  // empty field has nothing to show for, go out as the keys they are.
  const onKbdKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.nativeEvent.isComposing) return
    if (e.key === "Enter" || e.key === "Backspace") {
      e.preventDefault()
      for (const k of namedKey(e.key)) send("Input.dispatchKeyEvent", k)
      return
    }
    // Arrows, Tab, Escape from a hardware keyboard attached to a tablet.
    if (e.key.length > 1 && e.key !== "Unidentified") {
      e.preventDefault()
      send("Input.dispatchKeyEvent", keyEvent(e, true))
      send("Input.dispatchKeyEvent", keyEvent(e, false))
    }
  }

  const drainKbd = () => {
    const ta = kbdRef.current
    if (!ta?.value) return
    insertText(ta.value)
    ta.value = ""
  }

  React.useEffect(() => {
    const ta = kbdRef.current
    if (!ta) return
    // Android's keyboards report every key as "Unidentified" (keyCode 229),
    // so its Backspace and Enter only show up as input types.
    const onBefore = (e: InputEvent) => {
      if (e.inputType === "deleteContentBackward") {
        e.preventDefault()
        for (const k of namedKey("Backspace")) send("Input.dispatchKeyEvent", k)
      } else if (
        e.inputType === "insertLineBreak" ||
        e.inputType === "insertParagraph"
      ) {
        e.preventDefault()
        for (const k of namedKey("Enter")) send("Input.dispatchKeyEvent", k)
      }
    }
    ta.addEventListener("beforeinput", onBefore)
    return () => ta.removeEventListener("beforeinput", onBefore)
  }, [send])

  // ---- render -----------------------------------------------------------------

  const statusLine =
    conn === "live"
      ? ""
      : conn === "starting"
        ? "starting the shared browser…"
        : conn === "connecting"
          ? "connecting…"
          : "reconnecting…"

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex flex-shrink-0 items-center gap-1.5 border-border border-b bg-background px-2 py-1.5">
        {modeSwitch}
        <Button
          variant="outline"
          size="icon"
          className="size-7 max-sm:hidden"
          title="back"
          disabled={!current}
          onClick={() => void history(-1)}
        >
          <ArrowLeft />
        </Button>
        <Button
          variant="outline"
          size="icon"
          className="size-7 max-sm:hidden"
          title="forward"
          disabled={!current}
          onClick={() => void history(1)}
        >
          <ArrowRight />
        </Button>
        <Button
          variant="outline"
          size="icon"
          className="size-7"
          title="reload"
          disabled={!current}
          onClick={reload}
        >
          <RotateCw />
        </Button>
        <Input
          value={urlInput}
          placeholder="port or URL"
          className="h-7 min-w-0 flex-1 text-[13px]"
          onFocus={(e) => {
            urlFocused.current = true
            e.currentTarget.select()
          }}
          onBlur={() => {
            urlFocused.current = false
            setUrlInput(currentURL === "about:blank" ? "" : currentURL)
          }}
          onChange={(e) => setUrlInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              urlFocused.current = false
              void navigate(e.currentTarget.value)
            } else if (e.key === "Escape") {
              e.currentTarget.blur()
            }
          }}
        />
        {/* Touch only: a phone has no physical keyboard to reach the canvas,
            so this focuses the hidden textarea that raises the software one.
            With a mouse and keyboard the canvas takes keys directly and the
            button does nothing visible. */}
        <Button
          variant="outline"
          size="icon"
          className="pointer-coarse:inline-flex hidden size-7"
          title="Show keyboard"
          disabled={!current}
          onClick={() => kbdRef.current?.focus()}
        >
          <Keyboard />
        </Button>
      </div>

      {(statusLine || err) && (
        <div className="flex flex-shrink-0 items-center gap-2 border-border border-b bg-background px-2 py-1 text-[12px]">
          {statusLine && <Orb state="working" px={14} />}
          <span
            className={cn(
              "min-w-0 flex-1",
              err && conn !== "starting" && conn !== "connecting"
                ? "text-destructive"
                : "text-muted-foreground"
            )}
          >
            {statusLine}
            {statusLine && err ? " — " : ""}
            {err}
          </span>
          {conn === "reconnecting" && (
            <Button
              variant="outline"
              size="sm"
              className="h-6 flex-shrink-0 text-[12px]"
              onClick={() => setRetryNonce((n) => n + 1)}
            >
              retry now
            </Button>
          )}
        </div>
      )}

      <div ref={boxRef} className="relative min-h-0 flex-1 bg-muted/40">
        <canvas
          ref={canvasRef}
          tabIndex={0}
          aria-label="shared browser"
          className="absolute inset-0 size-full touch-none select-none outline-none"
          onPointerDown={onPointerDown}
          onPointerMove={onPointerMove}
          onPointerUp={onPointerUp}
          onPointerCancel={onPointerCancel}
          onContextMenu={(e) => e.preventDefault()}
          onKeyDown={(e) => onKey(e, true)}
          onKeyUp={(e) => onKey(e, false)}
          onPaste={(e) => {
            e.preventDefault()
            insertText(e.clipboardData.getData("text/plain"))
          }}
        />
        {client && pages.length === 0 && (
          <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 text-[13px] text-muted-foreground">
            No page open.
            <Button
              variant="outline"
              size="sm"
              onClick={() => void openNew("about:blank")}
            >
              <Plus />
              open a page
            </Button>
          </div>
        )}
        <textarea
          ref={kbdRef}
          aria-label="type into the shared browser"
          {...NO_AUTOCORRECT}
          className="pointer-events-none absolute bottom-0 left-0 size-px resize-none text-base opacity-0"
          onKeyDown={onKbdKeyDown}
          onInput={(e) => {
            if (!(e.nativeEvent as InputEvent).isComposing) drainKbd()
          }}
          onCompositionEnd={drainKbd}
        />
      </div>
    </div>
  )
}
