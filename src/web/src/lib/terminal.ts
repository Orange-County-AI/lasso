import { api } from "@/lib/api"
import { mountTerminalKeyBar } from "@/lib/mobile-keybar"
import {
  applyTermFont,
  applyTermTheme,
  lastTerminalTheme,
  startTermThemeReconciler,
} from "@/lib/theme"

// Behavior attached to the same-origin ttyd terminal iframes (/terminal/ and
// /shell/). All of this is ported faithfully from the original index.html —
// xterm.js can only paste text, sends a bare CR for both Enter and Shift+Enter,
// and starts from ttyd's theme on every reconnect, so we patch around each.

interface XTermBufferLine {
  translateToString: (trimRight?: boolean) => string
}
interface XTermBuffer {
  active?: {
    cursorX?: number
    cursorY?: number
    baseY?: number
    getLine?: (y: number) => XTermBufferLine | undefined
  }
}
interface XTerm {
  paste?: (text: string) => void
  input?: (data: string) => void
  focus?: () => void
  rows?: number
  buffer?: XTermBuffer
  options?: Record<string, unknown>
  attachCustomKeyEventHandler?: (h: (e: KeyboardEvent) => boolean) => void
  resize?: (cols: number, rows: number) => void
  // xterm.js public IParser. ttyd 1.7.4 never registers an OSC 52 handler, so
  // we add our own (wireOsc52) to honour herdr's clipboard copies.
  parser?: {
    registerOscHandler?: (
      ident: number,
      handler: (data: string) => boolean
    ) => unknown
  }
  _core?: {
    coreService?: { triggerDataEvent?: (data: string, sync?: boolean) => void }
  }
  __herdrShiftEnter?: boolean
  __herdrOsc52?: boolean
  __herdrResizeGate?: boolean
}
interface TermWindow extends Window {
  term?: XTerm
}
interface WiredDoc extends Document {
  __herdrWired?: boolean
  __touchScrollWired?: boolean
}

function frameWindow(id: string): TermWindow | null {
  const el = document.getElementById(id) as HTMLIFrameElement | null
  return (el?.contentWindow as TermWindow | null) ?? null
}

// xterm.js sends a bare CR for Shift+Enter too, so a line editor (Claude Code)
// can't tell it from Enter and submits. Emit backslash+CR instead: Claude's
// line-continuation inserts a newline and swallows the backslash.
const NEWLINE_SEQ = "\\\r"
function sendNewline(term: XTerm) {
  if (typeof term.input === "function") {
    term.input(NEWLINE_SEQ)
    return
  }
  try {
    const cs = term._core?.coreService
    if (cs && typeof cs.triggerDataEvent === "function")
      cs.triggerDataEvent(NEWLINE_SEQ, true)
  } catch {
    /* private API moved: no-op rather than throw */
  }
}

function wireShiftEnter(id: string, tries: number) {
  let term: XTerm | undefined
  try {
    term = frameWindow(id)?.term
  } catch {
    return
  }
  if (term && typeof term.attachCustomKeyEventHandler === "function") {
    if (!term.__herdrShiftEnter) {
      term.__herdrShiftEnter = true
      const t = term
      term.attachCustomKeyEventHandler((e) => {
        const enterish =
          e.key === "Enter" ||
          e.code === "Enter" ||
          e.code === "NumpadEnter" ||
          e.keyCode === 13
        if (
          e.type === "keydown" &&
          enterish &&
          e.shiftKey &&
          !e.ctrlKey &&
          !e.altKey &&
          !e.metaKey
        ) {
          // preventDefault is essential — without it the browser runs Enter's
          // default on the helper textarea, which xterm re-reads as a 2nd Enter.
          if (e.preventDefault) e.preventDefault()
          sendNewline(t)
          return false
        }
        return true
      })
    }
    return
  }
  if (tries < 20) setTimeout(() => wireShiftEnter(id, tries + 1), 150)
}

// herdr copies (copy-mode, double-click token, mouse selection in a pane) by
// emitting OSC 52 — `ESC ] 52 ; c ; <base64> BEL` — and immediately shows
// "copied to clipboard". But ttyd 1.7.4's bundled xterm.js registers no OSC 52
// handler, so the sequence is silently dropped and the browser clipboard is
// never written: herdr says it copied, but nothing actually did. We register
// the missing handler on the same-origin xterm to close that gap.
//
// `data` is the OSC payload after "52;" — "<Pc>;<base64>", where Pc is the
// target selection ("c" for clipboard, may be empty or multi-char). A read
// query ("?") or an empty/invalid payload yields null and is ignored.
function osc52Text(data: string): string | null {
  const semi = data.indexOf(";")
  const b64 = semi === -1 ? data : data.slice(semi + 1)
  if (b64 === "" || b64 === "?") return null
  try {
    const bin = atob(b64)
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0))
    return new TextDecoder().decode(bytes)
  } catch {
    return null // not valid base64 — don't guess
  }
}

// execCommandCopy is the fallback clipboard write for non-secure-context origins
// (plain http on the tailnet), where navigator.clipboard is undefined. It copies
// via a throwaway textarea inside the iframe — the frame that holds focus — then
// hands focus back to xterm.
function execCommandCopy(win: TermWindow, text: string) {
  try {
    const doc = win.document
    const ta = doc.createElement("textarea")
    ta.value = text
    ta.setAttribute("readonly", "")
    // Off-screen so it neither flashes nor scrolls the terminal.
    ta.style.position = "fixed"
    ta.style.top = "0"
    ta.style.left = "0"
    ta.style.opacity = "0"
    doc.body.appendChild(ta)
    ta.select()
    doc.execCommand("copy")
    doc.body.removeChild(ta)
    win.term?.focus?.() // execCommand stole focus to the textarea
  } catch {
    /* best effort — never throw back into xterm's parser */
  }
}

function writeClipboard(win: TermWindow, text: string) {
  try {
    const cb = win.navigator?.clipboard
    if (cb && typeof cb.writeText === "function") {
      // writeText can reject (lost user activation, permission denied); fall
      // back to execCommand so the copy still lands.
      cb.writeText(text).catch(() => execCommandCopy(win, text))
      return
    }
  } catch {
    /* clipboard access can throw in locked-down contexts */
  }
  execCommandCopy(win, text)
}

function wireOsc52(id: string, tries: number) {
  let win: TermWindow | null
  try {
    win = frameWindow(id)
  } catch {
    return
  }
  const term = win?.term
  if (term?.parser && typeof term.parser.registerOscHandler === "function") {
    if (!term.__herdrOsc52) {
      term.__herdrOsc52 = true
      const w = win as TermWindow
      term.parser.registerOscHandler(52, (data) => {
        const text = osc52Text(data)
        if (text != null) writeClipboard(w, text)
        return true // handled — suppress xterm's "unknown OSC" fallback
      })
    }
    return
  }
  if (tries < 20) setTimeout(() => wireOsc52(id, tries + 1), 150)
}

// The herdr server sizes the SHARED runtime to whichever client most recently
// interacted — or merely resized: a bare client resize steals the "foreground"
// slot. So a lasso session sitting in a background tab or unfocused window that
// reflows its terminal (an OS window resize, a mobile viewport change, the theme
// reconciler's refit nudge) clamps every other session's herdr view to ITS
// width — the "terminal shrinks as though the sidebar opened" effect. Gate the
// push at its chokepoint: every resize funnels through xterm's term.resize (the
// iframe's FitAddon reacts to resize events and calls it), so wrap it and drop
// resizes while this session is hidden or unfocused. When the session returns to
// the foreground, nudge a refit so xterm recomputes against the *current* box
// (the dropped dims may be stale by then).
//
// The first resize always passes, foreground or not: a session that loads in a
// background tab must still establish its real size at connect, or its herdr
// client attaches at the pty default (80×24) and clamps everyone far worse.
function sessionInForeground(): boolean {
  return document.visibilityState === "visible" && document.hasFocus()
}

function wireResizeGate(id: string, tries: number) {
  let win: TermWindow | null
  try {
    win = frameWindow(id)
  } catch {
    return
  }
  const term = win?.term
  if (win && term && typeof term.resize === "function") {
    if (term.__herdrResizeGate) return
    term.__herdrResizeGate = true
    const w = win
    const orig = term.resize.bind(term)
    let sized = false
    let deferred = false
    term.resize = (cols: number, rows: number) => {
      if (!sized || sessionInForeground()) {
        sized = true
        orig(cols, rows)
        return
      }
      deferred = true
    }
    const flush = () => {
      // Unhook once the iframe's window is gone (grid cells come and go), so
      // dead closures don't pile up on the parent document for the app's life.
      let alive = false
      try {
        alive = !w.closed
      } catch {
        /* cross-realm access after teardown */
      }
      if (!alive) {
        document.removeEventListener("visibilitychange", flush)
        window.removeEventListener("focus", flush)
        return
      }
      if (!deferred || !sessionInForeground()) return
      deferred = false
      try {
        w.dispatchEvent(new Event("resize"))
      } catch {
        /* ignore */
      }
    }
    document.addEventListener("visibilitychange", flush)
    window.addEventListener("focus", flush)
    // Focus can land directly in the iframe (a click on the terminal) without
    // the parent window ever firing focus, so listen there too.
    w.addEventListener("focus", flush)
    return
  }
  if (tries < 20) setTimeout(() => wireResizeGate(id, tries + 1), 150)
}

// wireTouchScroll gives terminals a finger-drag scroll on touch devices. Two
// things conspire to make the obvious approaches fail:
//   1. Our terminals are `tmux attach`, and tmux lives in xterm's ALTERNATE
//      screen — so `.xterm-viewport` holds no scrollback to move (scrollTop is a
//      no-op). The scrollable content belongs to the app inside tmux.
//   2. xterm has its own touch-scroll, but disables it whenever the app has mouse
//      tracking on (Claude Code does), forwarding the touch as a mouse drag.
// Both the alt-screen case and the scrollback case ARE reachable the same way the
// desktop reaches them: the wheel. xterm turns a wheel into scrollback movement
// (normal buffer), alternate-scroll arrow keys, or app mouse-wheel forwarding
// (mouse mode) — whichever applies. So we translate the drag into synthetic wheel
// events aimed at xterm, emitting one "line" per row-height of travel, and always
// preventDefault so the page itself never scrolls underneath. A bare tap (no
// move) is left alone, so taps still reach the terminal as clicks. Idempotent.
function wireTouchScroll(doc: WiredDoc, win: TermWindow) {
  if (doc.__touchScrollWired) return
  doc.__touchScrollWired = true
  let lastY = 0
  let accum = 0
  let tracking = false
  const rowHeight = (): number => {
    const screen = doc.querySelector(".xterm-screen") as HTMLElement | null
    const rows = win.term?.rows ?? 24
    const h = screen?.clientHeight ?? 0
    return h && rows ? h / rows : 18
  }
  doc.addEventListener(
    "touchstart",
    (e: TouchEvent) => {
      tracking = e.touches.length === 1
      if (tracking) {
        lastY = e.touches[0].clientY
        accum = 0
      }
    },
    { capture: true, passive: true }
  )
  doc.addEventListener(
    "touchmove",
    (e: TouchEvent) => {
      if (!tracking || e.touches.length !== 1) return
      const t = e.touches[0]
      // Finger DOWN (clientY grows) reveals older content above → negative
      // deltaY (wheel up), matching natural touch scrolling.
      accum += lastY - t.clientY
      lastY = t.clientY
      const target =
        (doc.querySelector(".xterm-viewport") as HTMLElement | null) ??
        (doc.querySelector(".xterm") as HTMLElement | null)
      const rh = rowHeight()
      // The iframe realm's WheelEvent ctor, so the event belongs to xterm's window.
      const WheelEventCtor = (
        win as unknown as { WheelEvent: typeof WheelEvent }
      ).WheelEvent
      while (target && Math.abs(accum) >= rh) {
        const dir = accum > 0 ? 1 : -1
        accum -= dir * rh
        target.dispatchEvent(
          new WheelEventCtor("wheel", {
            deltaY: dir * rh,
            deltaMode: 0, // pixels
            bubbles: true,
            cancelable: true,
            clientX: t.clientX,
            clientY: t.clientY,
          })
        )
      }
      e.preventDefault()
      e.stopPropagation()
    },
    { capture: true, passive: false }
  )
}

// wireTerminalIframe: (1) for the herdr terminal, suppress the native context
// menu so right-click only triggers herdr's handling; (2) intercept image paste
// — save it server-side and insert its path at the cursor (xterm only pastes
// text). Re-run on each iframe (re)load; __herdrWired guards double-attaching.
export function wireTerminalIframe(id: string, suppressContext: boolean) {
  let win: TermWindow | null
  let doc: WiredDoc | null
  try {
    win = frameWindow(id)
    doc = (win?.document as WiredDoc) ?? null
  } catch {
    return
  }
  if (!doc || doc.__herdrWired) return
  doc.__herdrWired = true

  if (win) wireTouchScroll(doc, win)

  if (suppressContext)
    doc.addEventListener("contextmenu", (e) => e.preventDefault(), true)

  // Forward app-level shortcuts (Cmd/Ctrl+<key>) to the parent document so
  // global handlers fire even while the terminal holds keyboard focus — the
  // iframe is same-origin, so we can re-dispatch. If a parent listener claims
  // the combo (preventDefault ⇒ dispatchEvent returns false), mirror that back
  // into the iframe so neither xterm nor the browser also acts on it. Clones
  // land on the parent `document`, not this one, so there's no re-entrancy.
  doc.addEventListener(
    "keydown",
    (e: KeyboardEvent) => {
      if (!(e.metaKey || e.ctrlKey)) return
      const clone = new KeyboardEvent("keydown", {
        key: e.key,
        code: e.code,
        metaKey: e.metaKey,
        ctrlKey: e.ctrlKey,
        altKey: e.altKey,
        shiftKey: e.shiftKey,
        bubbles: true,
        cancelable: true,
      })
      if (!document.dispatchEvent(clone)) {
        e.preventDefault()
        e.stopPropagation()
      }
    },
    true
  )

  doc.addEventListener(
    "paste",
    async (e: ClipboardEvent) => {
      const items = e.clipboardData?.items
      if (!items) return
      const imgItem = Array.from(items).find(
        (it) => it.kind === "file" && it.type.startsWith("image/")
      )
      if (!imgItem) return // text paste: let xterm handle it
      const file = imgItem.getAsFile()
      if (!file) return
      e.preventDefault()
      e.stopPropagation()
      try {
        const { path } = await api.pasteImage(file)
        const term = win?.term
        if (term && typeof term.paste === "function") term.paste(`${path} `)
      } catch {
        /* never break the terminal */
      }
    },
    true
  )

  wireShiftEnter(id, 0)
  wireOsc52(id, 0)
  wireResizeGate(id, 0)
}

// bootTermFrame wires the iframe now and re-wires (and re-applies the latest
// theme) on every reload, since ttyd reconnects yield a fresh xterm.
export function bootTermFrame(id: string, suppressContext: boolean) {
  const el = document.getElementById(id) as HTMLIFrameElement | null
  if (!el) return () => {}
  const onLoad = () => {
    applyTermTheme(lastTerminalTheme(), 0)
    applyTermFont(0)
    wireTerminalIframe(id, suppressContext)
    mountTerminalKeyBar(id)
  }
  el.addEventListener("load", onLoad)
  // A ttyd WebSocket reconnect rebuilds xterm with its default theme without
  // reloading the iframe (no `load` event above), so arm the periodic reconcile
  // that re-pins the cached palette. Idempotent — safe to call per frame.
  startTermThemeReconciler()
  applyTermFont(0) // in case it already loaded
  wireTerminalIframe(id, suppressContext) // in case it already loaded
  mountTerminalKeyBar(id) // in case it already loaded
  return () => el.removeEventListener("load", onLoad)
}

// termHasRendered reports whether the same-origin xterm has painted real pane
// content yet. A fresh ttyd starts blank and flashes its own connect/reconnect
// chrome before herdr repaints the pane; we treat the terminal as "live" only
// once the cursor has advanced or a visible row carries text, so a loading
// overlay can mask that churn until then.
function termHasRendered(term: XTerm): boolean {
  const buf = term.buffer?.active
  if (!buf) return false
  if ((buf.cursorX ?? 0) > 0 || (buf.cursorY ?? 0) > 0) return true
  const getLine = buf.getLine
  if (typeof getLine !== "function") return false
  const base = buf.baseY ?? 0
  const rows = term.rows ?? 24
  for (let r = 0; r < rows; r++) {
    const line = getLine.call(buf, base + r)
    if (line && line.translateToString(true).trim() !== "") return true
  }
  return false
}

// whenTerminalReady calls onReady once the terminal iframe's xterm exists and has
// rendered real content — requiring two consecutive observations a frame apart so
// we reveal after the first paint settles rather than mid-redraw. Two backstops
// keep the loader from stranding: once xterm is booted but the buffer stays blank
// (a genuinely empty pane — an idle prompt has nothing to paint), reveal after a
// short hold rather than sitting on a spinner; and a hard overall deadline covers
// an xterm whose buffer we can't read at all. Returns a canceller; mirrors the
// retry cadence of the other terminal helpers.
export function whenTerminalReady(id: string, onReady: () => void): () => void {
  let done = false
  let tick: ReturnType<typeof setTimeout> | undefined
  const finish = () => {
    if (done) return
    done = true
    if (tick) clearTimeout(tick)
    clearTimeout(deadline)
    onReady()
  }
  const deadline = setTimeout(finish, 6000)
  let sawContent = false
  let blankSince = 0
  const poll = () => {
    if (done) return
    let ready = false
    try {
      const term = frameWindow(id)?.term
      if (term && termHasRendered(term)) {
        if (sawContent) ready = true
        else sawContent = true
      } else {
        sawContent = false
        if (term) {
          // xterm is up but nothing has painted — an empty pane. Don't hold
          // the overlay for the full deadline; it would read as a slow switch.
          if (!blankSince) blankSince = Date.now()
          else if (Date.now() - blankSince > 1500) ready = true
        } else {
          blankSince = 0
        }
      }
    } catch {
      /* same-origin; ignore */
    }
    if (ready) finish()
    else tick = setTimeout(poll, 120)
  }
  poll()
  return () => {
    done = true
    if (tick) clearTimeout(tick)
    clearTimeout(deadline)
  }
}

// Invoke cb whenever a terminal iframe's window gains keyboard focus (the user
// clicked into it). Listening on the frame's own window is the only reliable
// signal: the parent window's blur only fires on the FIRST hop into an iframe —
// focus moving between two iframes never re-blurs the parent. Retries until
// the frame window exists (the iframe may still be loading). Returns cleanup.
export function onTerminalFocus(id: string, cb: () => void): () => void {
  let done = false
  let tick: ReturnType<typeof setTimeout> | null = null
  let win: Window | null = null
  const attach = () => {
    if (done) return
    try {
      const w = frameWindow(id)
      if (w) {
        win = w
        w.addEventListener("focus", cb)
        return
      }
    } catch {
      /* same-origin; ignore */
    }
    tick = setTimeout(attach, 200)
  }
  attach()
  return () => {
    done = true
    if (tick) clearTimeout(tick)
    try {
      win?.removeEventListener("focus", cb)
    } catch {
      /* ignore */
    }
  }
}

// Hand keyboard focus to a terminal iframe by element id (used by grid cells,
// where clicking a header should let the user type without a second click).
export function focusTerminalFrame(id: string) {
  try {
    const w = frameWindow(id)
    if (w) {
      w.focus()
      w.term?.focus?.()
    }
  } catch {
    /* ignore */
  }
}

// Nudge a hidden-then-shown terminal to refit and take the keyboard.
export function refitTerminal(id: string) {
  try {
    const w = frameWindow(id)
    if (w) {
      w.dispatchEvent(new Event("resize"))
      w.focus()
    }
  } catch {
    /* ignore */
  }
}

// pasteIntoTerminal pastes text into a same-origin terminal iframe without
// submitting, so the user can review and press Enter. Retries while xterm is
// still loading. typeIntoShell / typeIntoHerdr are the per-frame shorthands.
export function pasteIntoTerminal(id: string, text: string, tries = 0) {
  try {
    const w = frameWindow(id)
    if (w?.term && typeof w.term.paste === "function") {
      w.focus()
      w.term.focus?.()
      w.term.paste(text)
      return
    }
  } catch {
    /* same-origin; ignore */
  }
  if (tries < 20) setTimeout(() => pasteIntoTerminal(id, text, tries + 1), 150)
}

// typeIntoShell pastes into the out-of-herdr shell (/shell/).
export function typeIntoShell(text: string) {
  pasteIntoTerminal("shellframe", text)
}

// typeIntoHerdr pastes into the herdr terminal (/terminal/), where agents run.
export function typeIntoHerdr(text: string) {
  pasteIntoTerminal("term", text)
}

// Virtual on-screen keys for mobile, where the soft keyboard offers no Esc, Tab,
// or arrows — keys agents (Claude Code) lean on constantly (Enter is omitted: the
// iOS keyboard already has Return). We dispatch a real keydown at xterm's hidden
// textarea and let XTERM encode it, exactly as it would a hardware keypress. This
// is the only way to be correct across every keyboard mode the app may turn on —
// application-cursor (ESCO vs ESC[ on the arrows), the kitty/extended protocol,
// modifyOtherKeys — which a fixed byte sequence written via term.input() can't
// track. xterm 5 keys its encoder off event.keyCode, which the KeyboardEvent ctor
// ignores from the init dict, so we pin it (and legacy `which`). Verified
// end-to-end: a dispatched ArrowUp drives shell/Claude Code history. Falls back to
// a raw sequence only if the textarea isn't mounted yet.
export type VirtualKey = "Escape" | "ArrowUp" | "ArrowDown" | "Tab"

const KEY_SPEC: Record<
  VirtualKey,
  { code: string; keyCode: number; seq: string }
> = {
  Escape: { code: "Escape", keyCode: 27, seq: "\x1b" },
  Tab: { code: "Tab", keyCode: 9, seq: "\t" },
  ArrowUp: { code: "ArrowUp", keyCode: 38, seq: "\x1b[A" },
  ArrowDown: { code: "ArrowDown", keyCode: 40, seq: "\x1b[B" },
}

export function sendKeyToTerminal(id: string, key: VirtualKey) {
  try {
    const win = frameWindow(id)
    if (!win) return
    const spec = KEY_SPEC[key]
    const ta = win.document.querySelector(
      ".xterm-helper-textarea"
    ) as HTMLElement | null
    if (ta) {
      const Ctor = (win as unknown as { KeyboardEvent: typeof KeyboardEvent })
        .KeyboardEvent
      const ev = new Ctor("keydown", {
        key,
        code: spec.code,
        bubbles: true,
        cancelable: true,
      })
      Object.defineProperty(ev, "keyCode", { get: () => spec.keyCode })
      Object.defineProperty(ev, "which", { get: () => spec.keyCode })
      ta.dispatchEvent(ev)
      return
    }
    win.term?.input?.(spec.seq)
  } catch {
    /* same-origin; ignore */
  }
}

// Hand keyboard focus to the herdr terminal (/terminal/) so the user can type
// into the focused pane without clicking it first. Focuses both the iframe
// window and xterm's input, and retries while xterm is still (re)connecting —
// mirrors pasteIntoTerminal. Used after creating/focusing an agent.
export function focusHerdrTerminal(tries = 0) {
  try {
    const w = frameWindow("term")
    if (w?.term && typeof w.term.focus === "function") {
      w.focus()
      w.term.focus()
      return
    }
  } catch {
    /* same-origin; ignore */
  }
  if (tries < 20) setTimeout(() => focusHerdrTerminal(tries + 1), 100)
}
