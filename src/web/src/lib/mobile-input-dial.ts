import { emitMobileCommand, type MobileCommand } from "@/lib/mobile-command"
import { sendKeyToTerminal, type VirtualKey } from "@/lib/terminal"

// The floating input controls injected inside each same-origin terminal
// iframe: the app's chrome for every device and width the md+ footer does not
// cover. Keeping the dial beside xterm's textarea is intentional: preventDefault
// on a same-document pointer gesture preserves the iOS software keyboard, while
// a control in the parent document would dismiss it and could not reopen it.
// That is why it lives here even at a width a mouse reached by dragging a
// window narrow — one control, one implementation, rather than a second copy in
// the parent document for the pointer that does not need the keyboard trick.

// Exported so terminal.ts can tell a tap on the dial from a tap on the terminal.
export const DIAL_ID = "__lasso_mobile_input_dial"
const STYLE_ID = "__lasso_mobile_input_dial_style"
const TRACKING_CLASS = "__lasso_mobile_input_dial_tracking"
const HOLD_MS = 140
const ROOT_SIZE = 58
const ITEM_SIZE = 54
// The dedicated chat button, which sits diagonally above-right of the root.
// Deliberately smaller than ROOT_SIZE: it is a destination, not the control you
// are operating, and it must not read as a second dial.
const CHAT_SIZE = 44
const BACK_RADIUS = 44
const TERMINAL_BOTTOM_GAP = 24
// The width at which the footer — the only other route to New, both sidebars,
// the host menu, the shortcuts sheet and chat — is gone. Tailwind's `md`, so
// the same 767px the sidebar's full-screen overlay uses in index.css: below it
// the app has no chrome of its own and the dial is it, whatever the pointer.
const NARROW_QUERY = "(max-width: 767px)"

type DialLevel = "root" | "keys" | "app"
type TargetKind = "branch" | "command" | "key"

type DialTarget = {
  id: string
  label: string
  glyph: string
  kind: TargetKind
  x: number
  y: number
  key?: VirtualKey
  width?: number
  branch?: DialLevel
  command?: MobileCommand
}

// Three targets on one arc from ~30° off vertical to straight left, evenly
// spaced at r=220 — the arc is re-spaced rather than crowded at one end, so the
// points sit 29° apart (the endpoint keeps the 1.5° pull-in that keeps a label
// pill inside the frame). Adding one more would overlap: the labels are drawn,
// so the geometry is read as much as it is remembered. The TOP of the arc is
// left free on purpose: that space belongs to the dedicated chat button above
// the root (.dial-chat), which is a destination rather than one of the
// terminal's input controls, and belongs one tap away rather than on an arc
// where a target is a hold-and-slide from its neighbour.
const ROOT_TARGETS: readonly DialTarget[] = [
  {
    id: "new",
    label: "New",
    glyph: "+",
    kind: "command",
    command: "new",
    x: -112,
    y: -190,
    width: 78,
  },
  {
    id: "app",
    label: "Lasso",
    glyph: "◆",
    kind: "branch",
    branch: "app",
    x: -190,
    y: -112,
    width: 96,
  },
  {
    id: "common-keys",
    label: "Common keys",
    glyph: "⌘",
    kind: "branch",
    branch: "keys",
    x: -219,
    y: -6,
    width: 116,
  },
]

const APP_TARGETS: readonly DialTarget[] = [
  {
    id: "search",
    label: "Search",
    glyph: "⌕",
    kind: "command",
    command: "search",
    x: -160,
    y: -120,
    width: 96,
  },
  {
    id: "host",
    label: "Host",
    glyph: "@",
    kind: "command",
    command: "host",
    x: -92,
    y: -190,
    width: 80,
  },
  {
    id: "sidebar",
    label: "Sidebar",
    glyph: "▣",
    kind: "command",
    command: "sidebar",
    x: -5,
    y: -235,
    width: 104,
  },
]

const KEY_TARGETS: readonly DialTarget[] = [
  {
    id: "escape",
    label: "Escape",
    glyph: "esc",
    kind: "key",
    key: "Escape",
    x: -216,
    y: -42,
  },
  {
    id: "ctrl-c",
    label: "Control C",
    glyph: "^C",
    kind: "key",
    key: "CtrlC",
    x: -156,
    y: -42,
  },
  {
    id: "tab",
    label: "Tab",
    glyph: "tab",
    kind: "key",
    key: "Tab",
    x: -194,
    y: -103,
  },
  {
    id: "shift-tab",
    label: "Shift Tab",
    glyph: "⇧⇥",
    kind: "key",
    key: "ShiftTab",
    x: -156,
    y: -156,
  },
  {
    id: "enter",
    label: "Enter",
    glyph: "↵",
    kind: "key",
    key: "Enter",
    x: 20,
    y: -219,
  },
  {
    id: "up",
    label: "Up arrow",
    glyph: "↑",
    kind: "key",
    key: "ArrowUp",
    x: -103,
    y: -194,
  },
  {
    id: "down",
    label: "Down arrow",
    glyph: "↓",
    kind: "key",
    key: "ArrowDown",
    x: -42,
    y: -216,
  },
]

const THEME_VARS = [
  "--h-bg",
  "--h-fg",
  "--h-muted",
  "--h-border",
  "--h-panel",
  "--h-hover",
  "--h-accent",
]

// The dial is chrome, so it follows the same Nothing law as the rest of the app
// (index.css): it separates by border and a brightness step, never by drop
// shadow or backdrop blur. What it does NOT do any more is sit opaque: a
// permanent floating control over someone's terminal has to be readable without
// hiding the two lines of output underneath it, so the CLOSED root is a ~15%
// wash of the raised surface behind a lifted border — the glyph and the ring
// carry it — and every state that is actually being used steps up to a solid
// tint: hover, the accent fill for an armed/expanded root, and --h-panel for
// the ring of items. Brightness stays the hierarchy, and the monochrome
// --h-accent (white on dark, black on light) inverts correctly in both
// palettes, which a hand-mixed tint does not.
function dialCSS(): string {
  const sel = `#${DIAL_ID}`
  return `
${sel} {
  position: fixed;
  right: calc(18px + env(safe-area-inset-right, 0px));
  bottom: calc(18px + env(safe-area-inset-bottom, 0px));
  width: ${ROOT_SIZE}px;
  height: ${ROOT_SIZE}px;
  z-index: 2147483000;
  /* Same recipe index.css uses for --input: the bare seam is nearly invisible in
     low-contrast palettes, so lift it toward the foreground. Works both ways. */
  --dial-edge: color-mix(in oklch, var(--h-border, #262626), var(--h-fg, #ededed) 28%);
  color: var(--h-fg, #ededed);
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  pointer-events: none;
  -webkit-user-select: none;
  user-select: none;
}
${sel} button {
  appearance: none;
  -webkit-appearance: none;
  -webkit-tap-highlight-color: transparent;
}
${sel} .dial-root {
  position: absolute;
  inset: 0;
  z-index: 4;
  display: grid;
  place-items: center;
  width: ${ROOT_SIZE}px;
  height: ${ROOT_SIZE}px;
  padding: 0;
  border: 1px solid var(--dial-edge);
  border-radius: 50%;
  background: color-mix(in srgb, var(--h-hover, #1a1a1a) 15%, transparent);
  color: var(--h-fg, #ededed);
  font: 700 24px/1 ui-monospace, SFMono-Regular, Menlo, monospace;
  pointer-events: auto;
  touch-action: none;
  transition: transform 120ms ease, background 120ms ease, border-color 120ms ease, color 120ms ease;
}
${sel} .dial-root:hover {
  background: color-mix(in srgb, var(--h-hover, #1a1a1a) 72%, transparent);
}
/* The glyph has no separate fill; the button's translucent background shows
   through uniformly, including directly behind the character. */
${sel} .dial-root-glyph {
  display: grid;
  place-items: center;
  width: 32px;
  height: 32px;
  color: inherit;
  pointer-events: none;
}
/* Ordered after :hover deliberately — equal specificity, so an expanded root
   under the cursor must still read as armed rather than merely hovered. */
${sel} .dial-root[aria-expanded="true"] {
  border-color: var(--h-accent, #fff);
  background: var(--h-accent, #fff);
  color: var(--h-bg, #000);
}
${sel} .dial-root:active {
  transform: scale(.94);
}
/* Pressing a CLOSED dial dips its surface; an open one must keep the accent fill
   (equal specificity otherwise lets this rule win and drop the armed state). */
${sel} .dial-root[aria-expanded="false"]:active {
  background: color-mix(in srgb, var(--h-panel, #111) 82%, transparent);
}
${sel} .dial-root:focus-visible,
${sel} .dial-item:focus-visible {
  outline: 2px solid var(--h-accent, #fff);
  outline-offset: 3px;
}
${sel} .dial-menu {
  position: absolute;
  inset: 0;
  pointer-events: none;
}
/* The dedicated way into chat mode: a destination rather than one of the
   terminal's input controls (on the arc it would be one hold-and-slide from the
   wrong target), sitting up and to the RIGHT of the root instead of stacked
   above it. The two circles must NOT overlap — that needs their centres 51px
   apart, 58/2 + 44/2 — while the smaller one's bottom edge still has to drop
   BELOW the root's top line, which needs dy < 51. So the offset has to be bought
   with dx, and the screen edge caps dx: this spends 11px of it past the root's
   box, leaving 4px to the edge against the root's own 18px inset. Hence ~66°
   above horizontal and a 3px drop, rims ~1.4px clear. The drop and the angle
   trade against each other and against that margin: a 6px drop at the same
   margin puts the rims back through each other, which is the state this
   replaced, so the angle gives way to the separation. Each circle owns its own
   hit area outright. It wears the closed root's recipe (a ~15% wash behind the
   lifted edge), so it is the same chrome at a smaller size; the glyph is the
   arc's old Chat glyph. */
${sel} .dial-chat {
  position: absolute;
  left: calc(100% - 30px);
  bottom: calc(100% - 3px);
  z-index: 3;
  display: grid;
  place-items: center;
  width: ${CHAT_SIZE}px;
  height: ${CHAT_SIZE}px;
  padding: 0;
  border: 1px solid var(--dial-edge);
  border-radius: 50%;
  background: color-mix(in srgb, var(--h-hover, #1a1a1a) 15%, transparent);
  color: var(--h-fg, #ededed);
  font: 700 18px/1 ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  pointer-events: auto;
  touch-action: none;
  transition: transform 120ms ease, background 120ms ease, border-color 120ms ease, color 120ms ease;
}
${sel} .dial-chat:hover {
  background: color-mix(in srgb, var(--h-hover, #1a1a1a) 72%, transparent);
}
${sel} .dial-chat:active {
  background: color-mix(in srgb, var(--h-panel, #111) 82%, transparent);
  transform: scale(.94);
}
${sel} .dial-chat:focus-visible {
  outline: 2px solid var(--h-accent, #fff);
  outline-offset: 3px;
}
${sel} .dial-line {
  position: absolute;
  left: ${ROOT_SIZE / 2}px;
  top: ${ROOT_SIZE / 2}px;
  z-index: 0;
  height: 0;
  border-top: 1px dashed var(--h-muted, #8a8a8a);
  transform-origin: 0 50%;
  pointer-events: none;
}
${sel} .dial-item {
  position: absolute;
  z-index: 2;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  width: ${ITEM_SIZE}px;
  height: ${ITEM_SIZE}px;
  padding: 0;
  border: 1px solid var(--dial-edge);
  border-radius: 999px;
  background: color-mix(in srgb, var(--h-panel, #111) 92%, transparent);
  color: var(--h-fg, #ededed);
  font: 700 17px/1 ui-monospace, SFMono-Regular, Menlo, monospace;
  opacity: 0;
  transform: scale(.72);
  pointer-events: auto;
  touch-action: none;
  transition: opacity 130ms ease, transform 130ms ease, background 90ms ease, color 90ms ease;
}
${sel} .dial-item.is-visible {
  opacity: 1;
  transform: scale(1);
}
${sel} .dial-item[data-active="true"] {
  border-color: var(--h-accent, #fff);
  background: var(--h-accent, #fff);
  color: var(--h-bg, #000);
  transform: scale(1.1);
}
${sel} .dial-item::after {
  position: absolute;
  bottom: calc(100% + 9px);
  left: 50%;
  z-index: 5;
  padding: 6px 8px;
  border: 1px solid var(--dial-edge);
  border-radius: 8px;
  background: var(--h-panel, #111);
  color: var(--h-fg, #ededed);
  content: attr(data-tooltip);
  font: 600 11px/1 ui-monospace, SFMono-Regular, Menlo, monospace;
  opacity: 0;
  pointer-events: none;
  transform: translateX(-50%);
  visibility: hidden;
  white-space: nowrap;
}
${sel} .dial-item[data-active="true"]::after {
  opacity: 1;
  visibility: visible;
}
${sel} .dial-branch {
  justify-content: center;
  padding: 0 12px;
  font-size: 14px;
}
${sel} .dial-branch .dial-glyph {
  color: var(--h-accent, #fff);
  font-size: 18px;
}
${sel} .dial-branch[data-active="true"] .dial-glyph {
  color: inherit;
}
${sel} .dial-root {
  cursor: grab;
}
${sel} .dial-root:active {
  cursor: grabbing;
}
html.${TRACKING_CLASS},
html.${TRACKING_CLASS} body {
  overscroll-behavior: none !important;
  touch-action: none !important;
}
html.${TRACKING_CLASS} #terminal-container,
html.${TRACKING_CLASS} .xterm,
html.${TRACKING_CLASS} .xterm-viewport,
html.${TRACKING_CLASS} .xterm-screen {
  overscroll-behavior: none !important;
  touch-action: none !important;
}
/* The gap is where the closed dial sits: clear of the bottom row it would
   otherwise cover, and above the software keyboard's edge on a phone. It goes
   with the stylesheet, so a window widened back past md gets the rows back. */
#terminal-container {
  height: calc(100% - ${TERMINAL_BOTTOM_GAP}px) !important;
}
@media (prefers-reduced-motion: reduce) {
  ${sel} .dial-root,
  ${sel} .dial-item { transition: none; }
}
`
}

function targetCenter(target: DialTarget): { x: number; y: number } {
  return {
    x: ROOT_SIZE / 2 + target.x,
    y: ROOT_SIZE / 2 + target.y,
  }
}

// Two conditions put the dial on screen, and BOTH are watched rather than
// sampled at boot.
//
// Touch, because sampling it once was a hole either way round: a tab that went
// from a mouse to a touch primary (a hybrid folded into tablet mode, a tablet's
// keyboard case detached) never got the dial at all, and one that went the
// other way kept it — its hold-and-slide gesture and its 24px terminal gap
// still in force — with no touchscreen left to work them.
//
// Width, because a desktop dragged below md loses the footer and used to gain
// nothing: with a mouse and no touchscreen there was no route left to New, the
// host menu, either sidebar, the shortcuts sheet or the chat — the window had
// no chrome at all until it was widened again. The dial works a mouse as it is
// (a click opens the ring, a click on a target fires it, and the CSS has
// carried `cursor: grab` and a hover state all along), so the fix is to mount
// the control that already exists rather than to grow a second one.
//
// The returned teardown is what makes that possible, and terminal.ts holds
// exactly one per iframe: it releases the previous document's mount on every
// `load` (a reload builds a new document, so the dial in the old one is already
// gone, but its listeners on the parent's <html> observer are not) and on hook
// cleanup.
export function mountTerminalInputDial(id: string): () => void {
  const frame = document.getElementById(id) as HTMLIFrameElement | null
  const win = frame?.contentWindow as Window | null
  if (!win) return () => {}
  // Touch is asked of the IFRAME and the width of the PARENT, deliberately.
  // Pointer capability belongs to the device, so either document answers it —
  // but this document is only the terminal COLUMN, so a max-width query here
  // would measure the panel split (a wide window with a wide sidebar reads as
  // narrow) instead of the app, i.e. a dial beside a footer that is still
  // there. The parent's width is the one the footer's own breakpoint reads.
  const conditions = [
    win.matchMedia?.("(pointer: coarse)"),
    window.matchMedia?.(NARROW_QUERY),
  ].filter((q): q is MediaQueryList => !!q)
  if (conditions.length === 0) return () => {}

  return watchDialConditions(conditions, () => attachTerminalInputDial(win, id))
}

// A media-query list reduced to what the watcher needs, so a test can hand it a
// pair it drives by hand.
export type DialCondition = {
  matches: boolean
  addEventListener(type: "change", listener: () => void): void
  removeEventListener(type: "change", listener: () => void): void
}

// Mount while ANY condition holds, and mount exactly ONCE: the two overlap (a
// phone is both touch and narrow, and a tablet crosses either boundary on its
// own), so attaching per condition would build two dials in one document and
// leave one of them with nobody holding its teardown.
export function watchDialConditions(
  conditions: readonly DialCondition[],
  attach: () => () => void
): () => void {
  let release: (() => void) | null = null
  let disposed = false

  const sync = () => {
    if (disposed) return
    const wanted = conditions.some((c) => c.matches)
    if (wanted && !release) release = attach()
    else if (!wanted && release) {
      release()
      release = null
    }
  }
  sync()
  for (const c of conditions) c.addEventListener("change", sync)

  return () => {
    disposed = true
    for (const c of conditions) c.removeEventListener("change", sync)
    release?.()
    release = null
  }
}

// ttyd may not have built #terminal-container yet when the iframe's load event
// fires, so mounting retries briefly just like the old key bar did. The retry
// has to be cancellable now: a capability flip or an unmount landing inside that
// window would otherwise build a dial nobody is holding a teardown for.
function attachTerminalInputDial(win: Window, id: string): () => void {
  const doc = win.document
  let cancelled = false
  let release: (() => void) | null = null

  const attempt = (tries: number) => {
    if (cancelled) return
    if (!doc.getElementById("terminal-container")) {
      if (tries < 20) win.setTimeout(() => attempt(tries + 1), 150)
      return
    }
    release = buildTerminalInputDial(win, id)
  }
  attempt(0)

  return () => {
    cancelled = true
    release?.()
    release = null
  }
}

function buildTerminalInputDial(win: Window, id: string): () => void {
  const doc = win.document
  // A hot reload or a double `load` must leave one dial, not two.
  doc.getElementById(DIAL_ID)?.remove()
  doc.getElementById(STYLE_ID)?.remove()

  // Collected rather than removed by hand: half a dozen of these sit on the
  // iframe's window at capture, and one missed pair is a dial that keeps eating
  // the terminal's touches after it is gone.
  const cleanups: Array<() => void> = []
  const on = <T extends EventTarget>(
    target: T,
    type: string,
    handler: EventListenerOrEventListenerObject,
    options?: AddEventListenerOptions
  ) => {
    target.addEventListener(type, handler, options)
    cleanups.push(() => target.removeEventListener(type, handler, options))
  }

  const style = doc.createElement("style")
  style.id = STYLE_ID
  style.textContent = dialCSS()
  doc.head.appendChild(style)

  const dial = doc.createElement("div")
  dial.id = DIAL_ID
  dial.setAttribute("role", "group")
  dial.setAttribute("aria-label", "Terminal input controls")

  // The dial renders inside the ttyd iframe, so the parent's --h-* palette has to
  // be copied across the document boundary. An appearance change (Settings →
  // light/dark/herdr, or the OS flipping under "system") only rewrites the parent
  // <html>, and nothing remounts the dial — so watch that element and re-copy,
  // otherwise the control stays painted in whichever palette it mounted with.
  const syncTheme = () => {
    const parentTheme = getComputedStyle(document.documentElement)
    for (const variable of THEME_VARS) {
      dial.style.setProperty(variable, parentTheme.getPropertyValue(variable))
    }
  }
  syncTheme()
  const themeObserver = new MutationObserver(() => {
    // An iframe torn out of the DOM (host switch, closed tab) does not reliably
    // fire pagehide, so drop the observer the first time we notice the dial went
    // with it rather than writing to a detached element forever.
    if (!dial.isConnected) {
      themeObserver.disconnect()
      return
    }
    syncTheme()
  })
  themeObserver.observe(document.documentElement, {
    attributes: true,
    attributeFilter: ["class", "style"],
  })
  cleanups.push(() => themeObserver.disconnect())
  on(win, "pagehide", () => themeObserver.disconnect(), { once: true })

  const menu = doc.createElement("div")
  menu.className = "dial-menu"

  const root = doc.createElement("button")
  root.type = "button"
  root.className = "dial-root"
  // Level changes replace the glyph with "‹" without changing the root button.
  const rootGlyph = doc.createElement("span")
  rootGlyph.className = "dial-root-glyph"
  rootGlyph.textContent = "⌘"
  root.appendChild(rootGlyph)
  root.title = "Hold and slide for input controls"
  root.setAttribute("aria-label", "Open input controls")
  root.setAttribute("aria-expanded", "false")

  // The way into chat mode. It is not a dial target because it is a destination
  // rather than an input control, and because the arc asks for a hold-and-slide
  // that a plain "go there" should not: one tap, always in the same place,
  // whatever level the dial is on. preventDefault on the gesture is what keeps
  // the software keyboard up (the same reason the root and the items do it), and
  // the command goes out on the UP — a finger that slid off is a cancel, which
  // matters because a touch pointer is implicitly captured and its up lands here
  // even when it ended somewhere else.
  const chatButton = doc.createElement("button")
  chatButton.type = "button"
  chatButton.className = "dial-chat"
  // The glyph the arc's Chat target carried, so the control reads the same as
  // the one it replaces.
  chatButton.textContent = "☰"
  chatButton.title = "Chat"
  chatButton.setAttribute("aria-label", "Read this session as chat")
  let chatDown: { x: number; y: number } | null = null
  on(chatButton, "pointerdown", (event: Event) => {
    event.preventDefault()
    const p = event as PointerEvent
    chatDown = { x: p.clientX, y: p.clientY }
  })
  on(chatButton, "pointerup", (event: Event) => {
    event.preventDefault()
    const p = event as PointerEvent
    const from = chatDown
    chatDown = null
    if (from && Math.hypot(p.clientX - from.x, p.clientY - from.y) > 12) return
    emitMobileCommand("chat")
  })
  on(chatButton, "pointercancel", () => {
    chatDown = null
  })

  dial.append(menu, root, chatButton)
  doc.body.appendChild(dial)
  win.requestAnimationFrame(() => win.dispatchEvent(new Event("resize")))
  let open = false
  let level: DialLevel = "root"
  let activeID: string | null = null
  let pointerID: number | null = null
  let holdTimer: number | undefined
  let moved = false
  let startedOpen = false
  let startedLevel: DialLevel = "root"
  let startX = 0
  let startY = 0

  const targets = (): readonly DialTarget[] =>
    level === "keys"
      ? KEY_TARGETS
      : level === "app"
        ? APP_TARGETS
        : ROOT_TARGETS

  let inputLocked = false
  let lockedOptions: Record<string, unknown> | null = null
  let previousDisableStdin: unknown

  const lockTerminalInput = () => {
    if (inputLocked) return
    inputLocked = true
    const terminalWindow = win as Window & {
      term?: { options?: Record<string, unknown> }
    }
    lockedOptions = terminalWindow.term?.options ?? null
    if (lockedOptions) {
      previousDisableStdin = lockedOptions.disableStdin
      lockedOptions.disableStdin = true
    }
    doc.documentElement.classList.add(TRACKING_CLASS)
  }

  const unlockTerminalInput = () => {
    if (!inputLocked) return
    inputLocked = false
    doc.documentElement.classList.remove(TRACKING_CLASS)
    if (lockedOptions) {
      if (previousDisableStdin === undefined) delete lockedOptions.disableStdin
      else lockedOptions.disableStdin = previousDisableStdin
    }
    lockedOptions = null
    previousDisableStdin = undefined
  }

  // xterm translates touch movement into terminal input when a foreground app
  // has mouse reporting enabled. Block the parallel touch stream at Window
  // capture while the dial owns the pointer; pointermove still reaches the
  // captured dial button and drives selection.
  const blockParallelTouch = (event: Event) => {
    if (!inputLocked) return
    event.preventDefault()
    event.stopImmediatePropagation()
  }
  on(win, "touchmove", blockParallelTouch, { capture: true, passive: false })
  on(win, "wheel", blockParallelTouch, { capture: true, passive: false })

  const setActive = (id: string | null) => {
    if (activeID === id) return
    activeID = id
    for (const item of menu.querySelectorAll<HTMLElement>(".dial-item")) {
      item.dataset.active = String(item.dataset.target === id)
    }
  }

  const close = () => {
    open = false
    level = "root"
    activeID = null
    menu.replaceChildren()
    rootGlyph.textContent = "⌘"
    root.title = "Hold and slide for input controls"
    root.setAttribute("aria-label", "Open input controls")
    root.setAttribute("aria-expanded", "false")
  }

  const activate = (target: DialTarget) => {
    if (target.kind === "branch") {
      show(target.branch ?? "root")
      return
    }
    if (target.kind === "command" && target.command) {
      close()
      emitMobileCommand(target.command)
      return
    }
    if (target.key) sendKeyToTerminal(id, target.key)
    setActive(null)
  }

  const makeItem = (target: DialTarget) => {
    const center = targetCenter(target)
    const line = doc.createElement("span")
    const distance = Math.hypot(target.x, target.y)
    const angle = (Math.atan2(target.y, target.x) * 180) / Math.PI
    line.className = "dial-line"
    line.style.width = `${distance}px`
    line.style.transform = `rotate(${angle}deg)`
    menu.appendChild(line)

    const button = doc.createElement("button")
    button.type = "button"
    button.className = `dial-item${target.width ? " dial-branch" : ""}`
    button.dataset.target = target.id
    button.dataset.active = "false"
    button.dataset.tooltip = target.label
    button.title = target.label
    button.setAttribute("aria-label", target.label)
    button.style.width = `${target.width ?? ITEM_SIZE}px`
    button.style.left = `${center.x - (target.width ?? ITEM_SIZE) / 2}px`
    button.style.top = `${center.y - ITEM_SIZE / 2}px`

    if (target.width) {
      const glyph = doc.createElement("span")
      glyph.className = "dial-glyph"
      glyph.textContent = target.glyph
      const label = doc.createElement("span")
      label.textContent = target.label
      button.append(glyph, label)
    } else {
      button.textContent = target.glyph
    }

    menu.appendChild(button)

    button.addEventListener("pointerdown", (event) => {
      event.preventDefault()
      event.stopImmediatePropagation()
      button.setPointerCapture?.(event.pointerId)
      lockTerminalInput()
      setActive(target.id)
    })
    button.addEventListener("pointermove", (event) => {
      if (!button.hasPointerCapture?.(event.pointerId)) return
      event.preventDefault()
      event.stopImmediatePropagation()
      const rect = button.getBoundingClientRect()
      const isInside =
        event.clientX >= rect.left &&
        event.clientX <= rect.right &&
        event.clientY >= rect.top &&
        event.clientY <= rect.bottom
      setActive(isInside ? target.id : null)
    })
    button.addEventListener("pointerup", (event) => {
      event.preventDefault()
      event.stopImmediatePropagation()
      const shouldActivate = activeID === target.id
      unlockTerminalInput()
      if (shouldActivate) activate(target)
      else setActive(null)
    })
    button.addEventListener("pointercancel", () => {
      setActive(null)
      unlockTerminalInput()
    })
    button.addEventListener("lostpointercapture", () => {
      setActive(null)
      unlockTerminalInput()
    })
    win.requestAnimationFrame(() => button.classList.add("is-visible"))
  }

  function show(nextLevel: DialLevel) {
    open = true
    level = nextLevel
    activeID = null
    menu.replaceChildren()
    // The root glyph flips to "‹" for a branch, which is the whole affordance:
    // a crumb chip naming the branch sat where the ring's own items are and
    // covered them, to say what the ring below it already says.
    const inBranch = level !== "root"
    rootGlyph.textContent = inBranch ? "‹" : "⌘"
    root.title = inBranch ? "Back to input controls" : "Close input controls"
    root.setAttribute(
      "aria-label",
      inBranch ? "Back to input controls" : "Close input controls"
    )
    root.setAttribute("aria-expanded", "true")

    for (const target of targets()) makeItem(target)
  }

  const nearestTarget = (
    clientX: number,
    clientY: number
  ): DialTarget | null => {
    const rect = root.getBoundingClientRect()
    const rootX = rect.left + rect.width / 2
    const rootY = rect.top + rect.height / 2
    let nearest: DialTarget | null = null
    let nearestDistance = Number.POSITIVE_INFINITY
    for (const target of targets()) {
      const distance = Math.hypot(
        clientX - (rootX + target.x),
        clientY - (rootY + target.y)
      )
      const hitRadius = Math.max(34, (target.width ?? ITEM_SIZE) / 2)
      if (distance <= hitRadius && distance < nearestDistance) {
        nearest = target
        nearestDistance = distance
      }
    }
    return nearest
  }

  const clearGesture = () => {
    if (holdTimer !== undefined) win.clearTimeout(holdTimer)
    holdTimer = undefined
    pointerID = null
    setActive(null)
    unlockTerminalInput()
  }

  root.addEventListener("pointerdown", (event) => {
    if (!event.isPrimary) return
    event.preventDefault()
    event.stopImmediatePropagation()
    pointerID = event.pointerId
    root.setPointerCapture?.(event.pointerId)
    lockTerminalInput()
    startedOpen = open
    startedLevel = level
    moved = false
    startX = event.clientX
    startY = event.clientY
    if (!open) holdTimer = win.setTimeout(() => show("root"), HOLD_MS)
  })

  root.addEventListener("pointermove", (event) => {
    if (pointerID !== event.pointerId) return
    event.preventDefault()
    event.stopImmediatePropagation()
    if (Math.hypot(event.clientX - startX, event.clientY - startY) > 7) {
      moved = true
      if (!open) show("root")
    }
    if (!open) return

    const rect = root.getBoundingClientRect()
    const rootDistance = Math.hypot(
      event.clientX - (rect.left + rect.width / 2),
      event.clientY - (rect.top + rect.height / 2)
    )
    if (level !== "root" && moved && rootDistance <= BACK_RADIUS) {
      show("root")
      return
    }

    const target = nearestTarget(event.clientX, event.clientY)
    if (level === "root" && target?.kind === "branch") {
      show(target.branch ?? "root")
      return
    }
    setActive(target?.id ?? null)
  })

  root.addEventListener("pointerup", (event) => {
    if (pointerID !== event.pointerId) return
    event.preventDefault()
    event.stopImmediatePropagation()
    const target = activeID
      ? (targets().find((candidate) => candidate.id === activeID) ?? null)
      : null

    clearGesture()
    if (target) {
      activate(target)
    } else if (!moved) {
      if (!startedOpen) show("root")
      else if (startedLevel !== "root") show("root")
      else close()
    }
  })

  root.addEventListener("pointercancel", () => {
    clearGesture()
    if (!startedOpen) close()
  })
  root.addEventListener("lostpointercapture", () => {
    if (pointerID !== null) clearGesture()
  })

  // A tap that dismisses the open dial is spent on the dismissal. Without this
  // the same gesture also reaches the terminal underneath: xterm focuses its
  // textarea, and an app with mouse reporting on (every agent harness) reads the
  // tap as a click — so putting the menu away could ALSO pick something in the
  // agent's own UI, which is the one thing a dismissal must never do. It is the
  // whole gesture that has to go, not just the pointerdown: a touch is trailed
  // by a synthetic mousedown/mouseup/click, and terminal.ts's long-press
  // right-click and tap-to-reconnect watch touchstart/touchend. Window capture
  // is the only place that reaches all of them — it runs before both of those
  // document-capture listeners and before anything xterm binds to its own
  // elements. The swallow is scoped to the dismissing POINTER (plus the short
  // tail its compatibility events arrive in), not to a fixed slice of time, so
  // the next gesture is the terminal's again and no half-seen touch sequence
  // leaves the scroll handler holding a stale origin.
  const SWALLOW_TAIL_MS = 500
  const SWALLOW_MAX_MS = 3000
  const SWALLOWED_EVENTS: readonly string[] = [
    "pointerdown",
    "pointerup",
    "pointercancel",
    "touchstart",
    "touchmove",
    "touchend",
    "touchcancel",
    "mousedown",
    "mouseup",
    "click",
    "dblclick",
    "contextmenu",
  ]

  let swallowing = false
  let swallowPointer: number | null = null
  let swallowTimer: number | undefined

  const endSwallow = () => {
    swallowing = false
    swallowPointer = null
    if (swallowTimer !== undefined) win.clearTimeout(swallowTimer)
    swallowTimer = undefined
  }

  const armSwallowTimer = (ms: number) => {
    if (swallowTimer !== undefined) win.clearTimeout(swallowTimer)
    swallowTimer = win.setTimeout(endSwallow, ms)
  }

  const swallowGesture = (event: Event) => {
    if (!swallowing) return
    // The dial's own events are never swallowed: a dismissing tap may be
    // followed straight away by a deliberate press on the root.
    if (dial.contains(event.target as Node)) {
      endSwallow()
      return
    }
    event.preventDefault()
    event.stopImmediatePropagation()
    // `click` closes the compatibility sequence; anything after it belongs
    // to a new gesture, which the terminal is entitled to.
    if (event.type === "click") {
      endSwallow()
      return
    }
    // The finger is up, so only that trailing mouse pair is still owed —
    // and preventDefault here usually means it never comes at all.
    if (
      (event.type === "pointerup" || event.type === "pointercancel") &&
      (event as PointerEvent).pointerId === swallowPointer
    ) {
      swallowPointer = null
      armSwallowTimer(SWALLOW_TAIL_MS)
    }
  }
  for (const type of SWALLOWED_EVENTS) {
    on(win, type, swallowGesture, { capture: true, passive: false })
  }

  // Registered after the swallow so it sees the dismissing pointerdown first
  // (the loop above ignores that one, nothing being swallowed yet) and can arm
  // on it. Any dial level counts, root included: whether an item happens to be
  // armed changes nothing about where the tap would otherwise land.
  on(
    win,
    "pointerdown",
    (event: Event) => {
      if (!open || dial.contains(event.target as Node)) return
      close()
      event.preventDefault()
      event.stopImmediatePropagation()
      swallowing = true
      swallowPointer = (event as PointerEvent).pointerId
      // Backstop for a pointer whose up/cancel never arrives.
      armSwallowTimer(SWALLOW_MAX_MS)
    },
    { capture: true, passive: false }
  )
  on(doc, "keydown", (event: Event) => {
    if ((event as KeyboardEvent).key !== "Escape") return
    if (open) close()
  })

  return () => {
    // Order matters: the gesture state has to be released before the DOM goes,
    // since unlocking restores xterm's disableStdin and drops the tracking class
    // from <html> — neither of which lives inside the dial.
    clearGesture()
    endSwallow()
    close()
    for (const undo of cleanups.reverse()) undo()
    cleanups.length = 0
    dial.remove()
    style.remove()
    // The 24px terminal gap went with the stylesheet, so xterm has to refit to
    // the height it just got back.
    win.requestAnimationFrame(() => win.dispatchEvent(new Event("resize")))
  }
}
