import { api } from "@/lib/api"
import { matchShortcut } from "@/lib/shortcuts"
import {
  applyTermFont,
  applyTermTheme,
  lastTerminalTheme,
  startTermThemeReconciler,
} from "@/lib/theme"

// Behavior attached to the same-origin ttyd terminal iframes. All of this is
// ported faithfully from the original index.html — xterm.js can only paste text,
// sends a bare CR for both Enter and Shift+Enter, and starts from ttyd's theme
// on every reconnect, so we patch around each.

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
  resize?: (cols: number, rows: number) => void
  cols?: number
  rows?: number
  buffer?: XTermBuffer
  options?: Record<string, unknown>
  attachCustomKeyEventHandler?: (h: (e: KeyboardEvent) => boolean) => void
  // xterm.js public IParser. ttyd 1.7.4 never registers an OSC 52 handler, so
  // we add our own (wireOsc52) to honour the terminal's clipboard copies.
  parser?: {
    registerOscHandler?: (
      ident: number,
      handler: (data: string) => boolean
    ) => unknown
  }
  _core?: {
    coreService?: { triggerDataEvent?: (data: string, sync?: boolean) => void }
  }
  __shiftEnterWired?: boolean
  __osc52Wired?: boolean
}
interface TermWindow extends Window {
  term?: XTerm
}
interface WiredDoc extends Document {
  __terminalWired?: boolean
  __gridScrollWired?: boolean
  __gridFocusWired?: boolean
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

// sendKeyToTerminal feeds a raw key sequence to the active tab's terminal —
// used by the mobile key bar, whose soft keyboard lacks Esc/arrows/Enter. Writes
// straight to the PTY (same path as sendNewline), so it needs no focus and won't
// dismiss the soft keyboard. Arrows are the normal-mode (DECCKM off) CSI forms,
// correct for the shell + agents; app-cursor-mode TUIs (vim/less) won't see them.
export const TERM_KEY = {
  escape: "\x1b",
  up: "\x1b[A",
  down: "\x1b[B",
  enter: "\r",
} as const

export function sendKeyToTerminal(seq: string) {
  let term: XTerm | undefined
  try {
    term = frameWindow(VIEWPORT_TERM_ID)?.term
  } catch {
    return
  }
  if (!term) return
  if (typeof term.input === "function") {
    term.input(seq)
    return
  }
  try {
    const cs = term._core?.coreService
    if (cs && typeof cs.triggerDataEvent === "function")
      cs.triggerDataEvent(seq, true)
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
    if (!term.__shiftEnterWired) {
      term.__shiftEnterWired = true
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

// The terminal copies (copy-mode, double-click token, mouse selection) by
// emitting OSC 52 — `ESC ] 52 ; c ; <base64> BEL` — and immediately shows
// "copied to clipboard". But ttyd 1.7.4's bundled xterm.js registers no OSC 52
// handler, so the sequence is silently dropped and the browser clipboard is
// never written: it says it copied, but nothing actually did. We register the
// missing handler on the same-origin xterm to close that gap.
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
    if (!term.__osc52Wired) {
      term.__osc52Wired = true
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

// wireGridScroll lets the grid wall scroll while the cursor sits over a terminal:
// holding Shift turns the wheel into a grid scroll instead of the terminal's
// scrollback. The iframe is same-origin, so we read its frameElement, walk up to
// the enclosing .termgrid, and drive its scrollTop ourselves — capturing the
// event first (and stopping it) so xterm never sees it. (deltaX covers platforms
// where Shift+wheel arrives as a horizontal delta.) Idempotent per document.
function wireGridScroll(doc: WiredDoc, win: Window) {
  if (doc.__gridScrollWired) return
  doc.__gridScrollWired = true
  doc.addEventListener(
    "wheel",
    (e: WheelEvent) => {
      if (!e.shiftKey) return
      const grid = win.frameElement?.closest?.(
        ".termgrid"
      ) as HTMLElement | null
      if (!grid) return
      e.preventDefault()
      e.stopPropagation()
      grid.scrollTop += e.deltaY || e.deltaX
    },
    { capture: true, passive: false }
  )
}

// wireGridFocus tells the parent app which grid cell the user just interacted
// with, so the sidebar can follow it ("follow active pane"). The grid otherwise
// learns the focused cell from a window-level `blur`, but that only fires the
// FIRST time focus leaves the top document for a terminal — clicking straight
// from one terminal to another (iframe→iframe) never re-fires it. These iframes
// are same-origin, so we hook the cell's own pointerdown/focusin and dispatch its
// frame id up to the parent window. Idempotent per document.
function wireGridFocus(doc: WiredDoc, win: Window) {
  if (doc.__gridFocusWired) return
  doc.__gridFocusWired = true
  const notify = () => {
    const id = win.frameElement?.id
    if (!id) return
    window.dispatchEvent(
      new CustomEvent("lasso:grid-cell-focus", { detail: id })
    )
  }
  doc.addEventListener("pointerdown", notify, { capture: true })
  doc.addEventListener("focusin", notify, { capture: true })
}

// wireTouchScroll restores drag-to-scroll on touch devices. xterm.js DOES
// implement touch scrolling itself, but it gates it on the application NOT having
// mouse tracking active — and Claude Code (like many TUIs) turns mouse mode on,
// so xterm forwards the touch as a mouse drag to the app and the scrollback
// becomes unreachable on mobile. We capture the touch first and drive the
// `.xterm-viewport` scrollTop ourselves (xterm's own scroll listener then repaints
// the buffer), which sidesteps the mouse-mode gate. We only swallow the gesture
// while there's actually something to scroll (normal buffer with scrollback) — in
// an alt-screen app the viewport isn't overflowing, so we leave the touch alone
// and the app still receives it. A bare tap (no move) is never preventDefault'd,
// so taps still reach the terminal as clicks. Idempotent per document.
function wireTouchScroll(doc: WiredDoc) {
  if (doc.__touchScrollWired) return
  doc.__touchScrollWired = true
  let lastY = 0
  let tracking = false
  doc.addEventListener(
    "touchstart",
    (e: TouchEvent) => {
      tracking = e.touches.length === 1
      if (tracking) lastY = e.touches[0].clientY
    },
    { capture: true, passive: true }
  )
  doc.addEventListener(
    "touchmove",
    (e: TouchEvent) => {
      if (!tracking || e.touches.length !== 1) return
      const vp = doc.querySelector(".xterm-viewport") as HTMLElement | null
      // Nothing to scroll (e.g. a full-screen alt-buffer TUI): don't hijack the
      // touch — let it through so the app still gets its mouse events.
      if (!vp || vp.scrollHeight <= vp.clientHeight) return
      const y = e.touches[0].clientY
      vp.scrollTop += lastY - y
      lastY = y
      e.preventDefault()
      e.stopPropagation()
    },
    { capture: true, passive: false }
  )
}

// wireTerminalIframe: (1) optionally suppress the native context menu so
// right-click only triggers the terminal's handling; (2) intercept image paste
// — save it server-side and insert its path at the cursor (xterm only pastes
// text); (3) in the grid, forward Shift+wheel to the grid (gridScroll). Re-run on
// each iframe (re)load; __terminalWired guards double-attaching.
export function wireTerminalIframe(
  id: string,
  suppressContext: boolean,
  gridScroll = false
) {
  let win: TermWindow | null
  let doc: WiredDoc | null
  try {
    win = frameWindow(id)
    doc = (win?.document as WiredDoc) ?? null
  } catch {
    return
  }
  if (!doc) return
  wireTouchScroll(doc)
  if (gridScroll && win) {
    wireGridScroll(doc, win)
    wireGridFocus(doc, win)
  }

  if (doc.__terminalWired) return
  doc.__terminalWired = true

  if (suppressContext)
    doc.addEventListener("contextmenu", (e) => e.preventDefault(), true)

  // Forward app-level shortcuts (Cmd/Ctrl+<key>) to the parent document so
  // global handlers fire even while the terminal holds keyboard focus — the
  // iframe is same-origin, so we can re-dispatch. Clones land on the parent
  // `document`, not this one, so there's no re-entrancy.
  //
  // For our OWN keydown shortcuts (⌘K/⌘I — matchShortcut) we neutralize the
  // original unconditionally, so xterm never also acts on them, not just when the
  // re-dispatched clone is claimed. For any other Cmd/Ctrl combo we still mirror
  // a parent claim (preventDefault ⇒ dispatchEvent returns false) so e.g. Cmd-C
  // copy keeps reaching xterm when nobody claimed it. (⌘[/⌘] never get here —
  // the browser consumes them for history nav; they're handled by the history
  // trap, see lib/history-toggle.ts.)
  doc.addEventListener(
    "keydown",
    (e: KeyboardEvent) => {
      if (!(e.metaKey || e.ctrlKey)) return
      const ours = matchShortcut(e) !== null
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
      const claimed = !document.dispatchEvent(clone)
      if (ours || claimed) {
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
}

// --- auto-reconnect ----------------------------------------------------------
//
// When the ttyd backend process exits cleanly (e.g. its `tmux attach` ends
// because the park/agent session was torn down and recreated a tick later by
// the reconcile loop), the WebSocket closes with code 1000. ttyd 1.7.7 only
// auto-reconnects on ABnormal closes (code != 1000, and only when its
// `reconnect` client option is set); on a clean exit it parks on a manual
// "Press ⏎ to Reconnect" overlay and waits for the user to hit Enter. That
// Enter just refreshes the token and reconnects — almost always succeeding,
// since the session is back by then. So we press it for the user a couple of
// times before leaving the manual prompt for a disconnect that won't recover.
//
// ttyd's overlay is a class-less <div> appended to the xterm element, carrying
// a distinctive inline `border-radius: 15px`. We read it same-origin: the exact
// prompt text means "parked, waiting for Enter"; "Reconnecting…" means an
// attempt is in flight (wait); opacity "0" (faded out) or no overlay means
// connected — reset the attempt counter for the next disconnect episode.
const RECONNECT_PROMPT = "Press ⏎ to Reconnect"
const MAX_AUTO_RECONNECTS = 3

function ttydOverlay(win: TermWindow): HTMLElement | null {
  const xterm = win.document?.querySelector?.(".xterm")
  return (
    (xterm?.querySelector(
      ':scope > div[style*="border-radius"]'
    ) as HTMLElement | null) ?? null
  )
}

// pressTerminalEnter synthesizes the Enter keydown ttyd's reconnect listener
// waits on. xterm binds keydown on its `.xterm-helper-textarea`, maps keyCode 13
// to CR, and fires onKey({domEvent}) — which ttyd checks for `key === "Enter"`.
// keyCode must be set (xterm's evaluateKeyboardEvent switches on it); the
// re-dispatched CR goes nowhere since the socket's data listeners are disposed.
function pressTerminalEnter(win: TermWindow) {
  const ta = win.document?.querySelector?.(
    ".xterm-helper-textarea"
  ) as HTMLTextAreaElement | null
  if (!ta) return
  // Constructed in the parent realm but dispatched on the same-origin iframe's
  // textarea — xterm reads key/keyCode off the event, not its prototype.
  ta.dispatchEvent(
    new KeyboardEvent("keydown", {
      key: "Enter",
      code: "Enter",
      keyCode: 13,
      which: 13,
      bubbles: true,
      cancelable: true,
    } as KeyboardEventInit)
  )
}

// startAutoReconnect polls the ttyd overlay and presses Enter on the manual
// reconnect prompt up to MAX_AUTO_RECONNECTS times per disconnect episode.
// Returns a canceller. The interval outlives ttyd WS reconnects (which don't
// reload the iframe), so frameWindow(id) is re-resolved every tick.
function startAutoReconnect(id: string): () => void {
  let attempts = 0
  let timer: ReturnType<typeof setTimeout> | undefined
  const tick = () => {
    try {
      const win = frameWindow(id)
      const ov = win ? ttydOverlay(win) : null
      if (!ov || ov.style.opacity === "0") {
        attempts = 0 // connected (or overlay faded) — ready for the next episode
      } else if (ov.textContent === RECONNECT_PROMPT && attempts < MAX_AUTO_RECONNECTS) {
        attempts++
        pressTerminalEnter(win as TermWindow)
      }
      // "Reconnecting…" / hit the cap: leave it be
    } catch {
      /* same-origin; ignore */
    }
    timer = setTimeout(tick, 600)
  }
  timer = setTimeout(tick, 600)
  return () => {
    if (timer) clearTimeout(timer)
  }
}

// bootTermFrame wires the iframe now and re-wires (and re-applies the latest
// theme) on every reload, since ttyd reconnects yield a fresh xterm.
export function bootTermFrame(
  id: string,
  suppressContext: boolean,
  gridScroll = false
) {
  const el = document.getElementById(id) as HTMLIFrameElement | null
  if (!el) return () => {}
  const onLoad = () => {
    applyTermTheme(lastTerminalTheme(), 0)
    applyTermFont(0)
    wireTerminalIframe(id, suppressContext, gridScroll)
  }
  el.addEventListener("load", onLoad)
  // A ttyd WebSocket reconnect rebuilds xterm with its default theme without
  // reloading the iframe (no `load` event above), so arm the periodic reconcile
  // that re-pins the cached palette. Idempotent — safe to call per frame.
  startTermThemeReconciler()
  applyTermFont(0) // in case it already loaded
  wireTerminalIframe(id, suppressContext, gridScroll) // in case it already loaded
  const stopReconnect = startAutoReconnect(id)
  return () => {
    el.removeEventListener("load", onLoad)
    stopReconnect()
  }
}

// termHasRendered reports whether the same-origin xterm has painted real pane
// content yet. A fresh ttyd starts blank and flashes its own connect/reconnect
// chrome before the terminal repaints the pane; we treat the terminal as "live"
// only once the cursor has advanced or a visible row carries text, so a loading
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
// we reveal after the first paint settles rather than mid-redraw — or after a hard
// timeout as a backstop (a genuinely blank pane, or an xterm whose buffer we can't
// read, must not strand the loader forever). Returns a canceller; mirrors the
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

// kickTerminalSize forces a real resize round-trip to the tmux client behind a
// terminal iframe. tmux sizes windows to the most-recently-ACTIVE client
// (`window-size latest`), and activity means input or a resize — so after grid
// mode (whose per-cell clients shrank the window) the window can stay stuck at
// a stale co-viewer's size (e.g. a background browser tab's never-fit 80x24
// client) until the user types. A refit alone can't fix it: xterm only reports
// a resize when its dims actually change. Resize down a row and back — two real
// SIGWINCHes (the same trick the backend's nudgeRedraw plays) — which promotes
// THIS client to latest and snaps the window to it immediately.
export function kickTerminalSize(id: string) {
  try {
    const term = frameWindow(id)?.term
    if (!term || typeof term.resize !== "function") return
    const { cols, rows } = term
    if (!cols || !rows || rows < 2) return
    term.resize(cols, rows - 1)
    term.resize(cols, rows)
  } catch {
    /* same-origin; ignore */
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
// still loading. typeIntoShell / typeIntoTerminal are the per-frame shorthands.
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

// typeIntoShell pastes into the standalone shell (/shell/).
export function typeIntoShell(text: string) {
  pasteIntoTerminal("shellframe", text)
}

// VIEWPORT_TERM_ID is the iframe id of the single persistent viewport terminal
// (the active tab's terminal — see TabTerminal).
export const VIEWPORT_TERM_ID = "tabterm-viewport"

// typeIntoTerminal pastes into the active tab's terminal (the viewport) without
// submitting, so the user reviews and presses Enter — used by the scratch pad's
// "Send to Terminal".
export function typeIntoTerminal(text: string) {
  pasteIntoTerminal(VIEWPORT_TERM_ID, text)
}

// Hand keyboard focus to the viewport terminal (/terminal/) so the user can type
// into the focused pane without clicking it first. Focuses both the iframe
// window and xterm's input, and retries while xterm is still (re)connecting —
// mirrors pasteIntoTerminal. Used after creating/focusing an agent.
export function focusViewportTerminal(tries = 0) {
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
  if (tries < 20) setTimeout(() => focusViewportTerminal(tries + 1), 100)
}

// focusTerminal hands keyboard focus to a terminal iframe (by element id) so the
// user can type immediately — after creating/selecting a tab, workspace, or
// agent. Focuses both the iframe window and xterm's input, retrying while xterm
// is still (re)connecting. The viewport-model counterpart of focusViewportTerminal.
export function focusTerminal(id: string, tries = 0) {
  try {
    const w = frameWindow(id)
    if (w?.term && typeof w.term.focus === "function") {
      w.focus()
      w.term.focus()
      return
    }
  } catch {
    /* same-origin; ignore */
  }
  if (tries < 20) setTimeout(() => focusTerminal(id, tries + 1), 100)
}
