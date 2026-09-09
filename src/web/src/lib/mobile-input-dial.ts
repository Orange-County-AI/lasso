import { api } from "@/lib/api"
import { emitMobileCommand, type MobileCommand } from "@/lib/mobile-command"
import {
  focusTerminalFrame,
  pasteAndSubmitTerminal,
  pasteIntoTerminal,
  sendKeyToTerminal,
  type VirtualKey,
} from "@/lib/terminal"

// Two mounts, one dial. The TOUCH mount is injected inside each same-origin
// terminal iframe: keeping it beside xterm's textarea is intentional, since
// preventDefault on a same-document pointer gesture preserves the iOS software
// keyboard, while a control in the parent document would dismiss it and could
// not reopen it. The DESKTOP mount is a single control in the app document — one
// per tab, not one per iframe, so it is reachable with the sidebar open and over
// whichever terminal happens to be focused — and it drives the same command
// bridge with mouse hover and the keyboard instead of a hold-and-slide gesture.
// Geometry, markup and styling are shared (dialCSS, renderDialItem); only the
// input model differs, which is the one thing that genuinely cannot be.

// Exported so terminal.ts can tell a tap on the dial from a tap on the terminal.
export const DIAL_ID = "__lasso_mobile_input_dial"
const STYLE_ID = "__lasso_mobile_input_dial_style"
const TRACKING_CLASS = "__lasso_mobile_input_dial_tracking"
const HOLD_MS = 140
const ROOT_SIZE = 58
const ITEM_SIZE = 54
const BACK_RADIUS = 44
const TERMINAL_BOTTOM_GAP = 24
const DESKTOP_DIAL_ID = "__lasso_desktop_nav_dial"
const DESKTOP_STYLE_ID = "__lasso_desktop_nav_dial_style"
// A mouse travelling from the root to a pill crosses gaps that belong to
// neither, so leaving a button cannot be the close signal. The dial stays open
// while the pointer is anywhere in the fan's bounding corridor and closes this
// long after it genuinely leaves — long enough to cross a gap or overshoot,
// short enough that an abandoned dial puts itself away.
const DESKTOP_CLOSE_MS = 260
const DESKTOP_CORRIDOR_PAD = 30
// A hovering fine pointer is what the desktop dial is FOR, but it must not be
// the only way to qualify: a browser that reports `pointer: none` (a headless
// or embedded Chromium, a machine whose input devices it declined to classify)
// would then get no dial at all, since the touch dial mounts only on
// `pointer: coarse`. So the second clause makes the two mounts complementary by
// construction — anything that is not coarse-primary is served here — and a
// phone still matches neither clause. `not all and` rather than `not (…)`: the
// bare form is Media Queries 4 and silently invalidates the whole list in
// browsers that only parse level 3.
const DESKTOP_MEDIA =
  "(hover: hover) and (pointer: fine), not all and (pointer: coarse)"
// The two terminal iframes the app can show, in the order the dial cares about:
// #term is the herdr terminal (where typing goes), #shellframe the sidebar's
// plain shell. Both are same-origin, which is what lets the dial read a pointer
// that is over them and hand the keyboard back to the right one.
const TERMINAL_FRAME_IDS = ["term", "shellframe"] as const

type DialLevel = "root" | "keys" | "app"
type TargetKind = "branch" | "command" | "input" | "key"
type DialMode = "touch" | "desktop"

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

const ROOT_TARGETS: readonly DialTarget[] = [
  {
    id: "input",
    label: "Input",
    glyph: "⌨︎",
    kind: "input",
    x: -23,
    y: -219,
  },
  {
    id: "new",
    label: "New",
    glyph: "+",
    kind: "command",
    command: "new",
    x: -110,
    y: -191,
    width: 78,
  },
  {
    id: "app",
    label: "Lasso",
    glyph: "◆",
    kind: "branch",
    branch: "app",
    x: -180,
    y: -126,
    width: 96,
  },
  {
    id: "common-keys",
    label: "Common keys",
    glyph: "⌘",
    kind: "branch",
    branch: "keys",
    x: -219,
    y: -23,
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

// The desktop fan is flat — four commands, no branches: there is no terminal
// input or common-keys level, because a physical keyboard already types every
// one of those keys straight into xterm. The labels are spelled out (a mouse
// has nothing like the hold-and-slide muscle memory to lean on), which makes
// the pills wide, so they climb an arc spaced by more than one pill height
// instead of sitting on a tidy circle where they would overlap.
const DESKTOP_TARGETS: readonly DialTarget[] = [
  {
    id: "new",
    label: "New",
    glyph: "+",
    kind: "command",
    command: "new",
    x: -22,
    y: -224,
    width: 84,
  },
  {
    id: "sidebar",
    label: "Sidebar",
    glyph: "▣",
    kind: "command",
    command: "sidebar",
    x: -86,
    y: -160,
    width: 112,
  },
  {
    id: "host",
    label: "Host",
    glyph: "@",
    kind: "command",
    command: "host",
    x: -142,
    y: -94,
    width: 88,
  },
  {
    id: "keybindings",
    label: "Keybindings",
    glyph: "⌘",
    kind: "command",
    command: "keybindings",
    x: -178,
    y: -24,
    width: 148,
  },
]

// The fan's widest item reaches ~250px out, which a pinched terminal pane does
// not have: the ring would hang past the pane's left edge, over the resize
// handle and the sidebar behind it. Same four commands, stacked straight up and
// right-aligned to the root instead, so the whole menu is as wide as its widest
// label. Derived from the fan rather than typed twice — the labels, glyphs and
// commands are one list.
const DESKTOP_COLUMN_GAP = 10
const DESKTOP_FAN_MIN_WIDTH = 300
const DESKTOP_COLUMN_TARGETS: readonly DialTarget[] = DESKTOP_TARGETS.map(
  (target, index) => ({
    ...target,
    x: ROOT_SIZE / 2 - (target.width ?? ITEM_SIZE) / 2,
    y: -(
      ROOT_SIZE / 2 +
      DESKTOP_COLUMN_GAP +
      ITEM_SIZE / 2 +
      index * (ITEM_SIZE + DESKTOP_COLUMN_GAP)
    ),
  })
)

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
function dialCSS(rootID: string, mode: DialMode): string {
  const sel = `#${rootID}`
  const base = `
${sel} {
  position: fixed;
  right: calc(18px + env(safe-area-inset-right, 0px));
  bottom: calc(18px + env(safe-area-inset-bottom, 0px));
  width: ${ROOT_SIZE}px;
  height: ${ROOT_SIZE}px;
  /* Inside the terminal iframe nothing else competes, so the touch dial takes
     the top of the stack. In the APP document it must stay below Radix's
     dialogs and popovers (z-50) — they are what its own actions open. */
  z-index: ${mode === "desktop" ? 40 : 2147483000};
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
/* Only the character gets a denser plate. The 58px circle stays a ~15% wash —
   that is the affordance, and making all of it opaque would blank out two lines
   of whatever the terminal is printing — while the glyph itself needs to be
   readable against arbitrary output, so it carries a small disc of the page
   background with it. Expanded, the accent fill already supplies the contrast,
   so the plate gets out of the way rather than sitting as a second shape
   inside it. */
${sel} .dial-root-glyph {
  display: grid;
  place-items: center;
  width: 32px;
  height: 32px;
  border-radius: 50%;
  background: color-mix(in srgb, var(--h-bg, #000) 60%, transparent);
  color: inherit;
  pointer-events: none;
  transition: background 120ms ease;
}
${sel} .dial-root[aria-expanded="true"] .dial-root-glyph {
  background: transparent;
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
@media (prefers-reduced-motion: reduce) {
  ${sel} .dial-root,
  ${sel} .dial-item { transition: none; }
}
`
  if (mode === "desktop") {
    return `${base}
/* Absolute, not fixed: the dial is a child of .term-shell (the terminal's own
   wrapper), so it rides the terminal's bottom-right corner through every
   sidebar drag, collapse and viewport change with no reposition math, and it
   can never float over the sidebar. The viewport insets in the base rule are
   meaningless inside a panel, so they go. */
${sel} {
  position: absolute;
  right: 18px;
  bottom: 18px;
}
/* The parent-document dial is a pointer control, not a draggable puck: no
   hold-and-slide, so no grab cursor. */
${sel} .dial-root {
  cursor: pointer;
  font-size: 22px;
}
/* Square corners are the app's law (--radius is 0), and these are text action
   buttons like any other. The ROOT keeps its circle: it is a status/affordance
   control, which is the one shape that stays round. The touch dial is
   deliberately not switched over — it renders inside the ttyd iframe, where
   --radius is not defined, so it would square itself off against a fallback
   rather than against the app's real value. */
${sel} .dial-item {
  border-radius: var(--radius, 0px);
}
/* Every desktop item is a labelled button, so the hover tooltip would only
   restate the label it sits above. */
${sel} .dial-item::after {
  content: none;
}
/* A modal marks the body children outside it aria-hidden (Radix/aria-hidden),
   and a floating control that stays clickable over a dialog is a trap. The
   ancestor form is the one that fires now that the dial lives inside
   .term-shell — the attribute lands on that subtree's root, not on the dial. */
${sel}[aria-hidden="true"],
[aria-hidden="true"] ${sel} {
  display: none;
}
`
  }
  return `${base}
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
/* Touch only: the gap is where the closed dial sits above the software
   keyboard's edge. The desktop dial floats over the layout instead, and must
   not shorten anyone's terminal to pay for a keyboard that isn't there. */
#terminal-container {
  height: calc(100% - ${TERMINAL_BOTTOM_GAP}px) !important;
}
${sel} .input-picker {
  display: none;
}
${sel} .input-panel {
  position: fixed;
  right: 14px;
  bottom: calc(88px + env(safe-area-inset-bottom, 0px));
  left: 14px;
  z-index: 6;
  display: flex;
  max-width: 440px;
  margin: 0 auto;
  flex-direction: column;
  gap: 10px;
  padding: 14px;
  border: 1px solid var(--dial-edge);
  border-radius: 18px;
  background: var(--h-panel, #111);
  color: var(--h-fg, #ededed);
  pointer-events: auto;
}
${sel} .input-header,
${sel} .input-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}
${sel} .input-header {
  justify-content: space-between;
  font: 600 13px/1.2 ui-monospace, SFMono-Regular, Menlo, monospace;
}
${sel} .input-status {
  color: var(--h-muted, #8a8a8a);
  font-size: 11px;
  font-weight: 500;
}
${sel} .input-buffer {
  box-sizing: border-box;
  width: 100%;
  min-height: 96px;
  resize: vertical;
  border: 1px solid var(--h-border, #262626);
  border-radius: 12px;
  background: var(--h-bg, #000);
  color: var(--h-fg, #ededed);
  padding: 10px 11px;
  font: 400 15px/1.45 ui-monospace, SFMono-Regular, Menlo, monospace;
  outline: none;
}
${sel} .input-buffer:focus {
  border-color: var(--h-accent, #fff);
}
${sel} .input-buffer::placeholder {
  color: var(--h-muted, #8a8a8a);
}
${sel} .input-actions {
  justify-content: flex-end;
}
/* The attach action belongs to the buffer, not to the commit trio, so it holds
   the left edge while Cancel/Insert/Enter stay grouped at the right. */
${sel} .input-action.attach {
  margin-right: auto;
}
${sel} .input-action {
  min-height: 38px;
  padding: 0 13px;
  border: 1px solid var(--dial-edge);
  border-radius: 999px;
  background: var(--h-hover, #1a1a1a);
  color: var(--h-fg, #ededed);
  font: 600 12px/1 ui-monospace, SFMono-Regular, Menlo, monospace;
}
${sel} .input-action.primary {
  border-color: var(--h-accent, #fff);
  background: var(--h-accent, #fff);
  color: var(--h-bg, #000);
}
${sel} .input-action:disabled {
  opacity: .42;
}
`
}

function targetCenter(target: DialTarget): { x: number; y: number } {
  return {
    x: ROOT_SIZE / 2 + target.x,
    y: ROOT_SIZE / 2 + target.y,
  }
}

// Shared geometry and markup: the dashed connector back to the root, and the
// button itself — a round glyph, or a labelled pill when the target names a
// width. Only the event wiring differs between the two mounts (a captured
// touch gesture versus mouse hover and the keyboard), so that stays with the
// caller, which also decides when to reveal the item.
function renderDialItem(
  doc: Document,
  menu: HTMLElement,
  target: DialTarget
): HTMLButtonElement {
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
  return button
}

// The root's character rides its own small backplate (see .dial-root-glyph),
// shared by both mounts so the two roots stay one control with one look. The
// returned span is what a level change writes into — the touch dial swaps in
// "‹" for a branch — since writing textContent on the button would throw the
// plate away.
function renderRootGlyph(
  doc: Document,
  root: HTMLElement,
  glyph: string
): HTMLSpanElement {
  const span = doc.createElement("span")
  span.className = "dial-root-glyph"
  span.textContent = glyph
  root.replaceChildren(span)
  return span
}

// The touch dial's lifecycle mirrors the desktop one: capability is watched, not
// merely sampled at boot. Sampling it once was a hole either way round — a tab
// that went from a mouse to a touch primary (a hybrid folded into tablet mode, a
// tablet's keyboard case detached) lost the desktop dial with no touch dial to
// replace it, and the reverse left this one mounted beside the desktop fan, its
// hold-and-slide gesture and its 24px terminal gap still in force.
//
// The returned teardown is what makes that possible, and terminal.ts holds
// exactly one per iframe: it releases the previous document's mount on every
// `load` (a reload builds a new document, so the dial in the old one is already
// gone, but its listeners on the parent's <html> observer are not) and on hook
// cleanup. `pasteHost` names the host an attached image must be written to — the
// focused pane's filesystem, resolved fresh on every use because focus moves
// without remounting anything.
export function mountTerminalInputDial(
  id: string,
  pasteHost: () => string | undefined
): () => void {
  const frame = document.getElementById(id) as HTMLIFrameElement | null
  const win = frame?.contentWindow as Window | null
  const coarse = win?.matchMedia?.("(pointer: coarse)")
  if (!win || !coarse) return () => {}

  let release: (() => void) | null = null
  let disposed = false

  const sync = () => {
    if (disposed) return
    if (coarse.matches && !release)
      release = attachTerminalInputDial(win, id, pasteHost)
    else if (!coarse.matches && release) {
      release()
      release = null
    }
  }
  sync()
  coarse.addEventListener("change", sync)

  return () => {
    disposed = true
    coarse.removeEventListener("change", sync)
    release?.()
    release = null
  }
}

// ttyd may not have built #terminal-container yet when the iframe's load event
// fires, so mounting retries briefly just like the old key bar did. The retry
// has to be cancellable now: a capability flip or an unmount landing inside that
// window would otherwise build a dial nobody is holding a teardown for.
function attachTerminalInputDial(
  win: Window,
  id: string,
  pasteHost: () => string | undefined
): () => void {
  const doc = win.document
  let cancelled = false
  let release: (() => void) | null = null

  const attempt = (tries: number) => {
    if (cancelled) return
    if (!doc.getElementById("terminal-container")) {
      if (tries < 20) win.setTimeout(() => attempt(tries + 1), 150)
      return
    }
    release = buildTerminalInputDial(win, id, pasteHost)
  }
  attempt(0)

  return () => {
    cancelled = true
    release?.()
    release = null
  }
}

function buildTerminalInputDial(
  win: Window,
  id: string,
  pasteHost: () => string | undefined
): () => void {
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
  style.textContent = dialCSS(DIAL_ID, "touch")
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
  const rootGlyph = renderRootGlyph(doc, root, "⌘")
  root.title = "Hold and slide for input controls"
  root.setAttribute("aria-label", "Open input controls")
  root.setAttribute("aria-expanded", "false")

  dial.append(menu, root)
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

  let inputPanel: HTMLDivElement | null = null

  const closeInputPanel = () => {
    inputPanel?.remove()
    inputPanel = null
    root.style.visibility = ""
  }

  const openInputPanel = () => {
    close()
    closeInputPanel()
    root.style.visibility = "hidden"

    const panel = doc.createElement("div")
    panel.className = "input-panel"
    panel.setAttribute("role", "dialog")
    panel.setAttribute("aria-label", "Terminal input buffer")

    const header = doc.createElement("div")
    header.className = "input-header"
    const title = doc.createElement("span")
    title.textContent = "Input buffer"
    const status = doc.createElement("span")
    status.className = "input-status"
    status.textContent = "Type, dictate, or attach a file"
    header.append(title, status)

    const buffer = doc.createElement("textarea")
    buffer.className = "input-buffer"
    buffer.placeholder =
      "Type, dictate, or attach a file, then insert or submit."
    buffer.spellcheck = true
    buffer.inputMode = "text"
    buffer.autocapitalize = "sentences"
    buffer.enterKeyHint = "done"
    buffer.setAttribute("aria-label", "Buffered terminal input")

    // A phone has no drag-and-drop and no file manager worth the name, so the
    // picker is the whole story: with no `accept` filter iOS offers Photo
    // Library / Take Photo / Choose File from this one input, which covers a
    // screenshot, a camera shot, and anything in Files or iCloud Drive alike.
    // Pasting into the buffer lands in the same handler (terminal.ts leaves
    // dial-targeted pastes alone for it).
    const picker = doc.createElement("input")
    picker.type = "file"
    picker.multiple = true
    picker.className = "input-picker"
    picker.tabIndex = -1

    const actions = doc.createElement("div")
    actions.className = "input-actions"
    const attach = doc.createElement("button")
    attach.type = "button"
    attach.className = "input-action attach"
    attach.textContent = "Attach"
    attach.title = "Attach a file and insert its path"
    attach.setAttribute("aria-label", "Attach a file")
    const cancel = doc.createElement("button")
    cancel.type = "button"
    cancel.className = "input-action"
    cancel.textContent = "Cancel"
    const insert = doc.createElement("button")
    insert.type = "button"
    insert.className = "input-action"
    insert.textContent = "Insert"
    insert.disabled = true
    const enter = doc.createElement("button")
    enter.type = "button"
    enter.className = "input-action primary"
    enter.textContent = "Enter"
    enter.title = "Insert and submit"
    enter.setAttribute("aria-label", "Insert and submit")
    enter.disabled = true
    actions.append(attach, cancel, insert, enter)
    panel.append(header, buffer, actions, picker)
    dial.appendChild(panel)
    inputPanel = panel

    const updateActions = () => {
      const disabled = !buffer.value.trim()
      insert.disabled = disabled
      enter.disabled = disabled
    }

    // Insert at the caret, space-separated, so a path can be dropped into the
    // middle of a sentence the way it reads in the composer afterwards.
    const insertAtCursor = (text: string) => {
      const start = buffer.selectionStart ?? buffer.value.length
      const end = buffer.selectionEnd ?? start
      const before = buffer.value.slice(0, start)
      const lead = before && !/\s$/.test(before) ? " " : ""
      const chunk = `${lead}${text} `
      buffer.value = before + chunk + buffer.value.slice(end)
      const caret = start + chunk.length
      buffer.setSelectionRange(caret, caret)
      updateActions()
    }

    // The file goes to the host the FOCUSED PANE's filesystem lives on (the
    // same target the terminal's own paste uses), because the path we insert is
    // read by the agent in that pane, not by the browser.
    let attaching = false
    const attachFiles = async (picked: ArrayLike<File> | null) => {
      const files = Array.from(picked ?? [])
      if (!files.length || attaching) return
      attaching = true
      attach.disabled = true
      status.textContent =
        files.length > 1 ? `Attaching ${files.length} files…` : "Attaching…"
      for (const file of files) {
        try {
          const { path } = await api.pasteFile(file, pasteHost(), file.name)
          // The panel can be cancelled mid-upload; the file is on the host
          // either way, but there is no buffer left to insert it into.
          if (!panel.isConnected) return
          insertAtCursor(path)
          status.textContent = path.split("/").pop() ?? "Attached"
        } catch (err) {
          if (!panel.isConnected) return
          status.textContent = `Attach failed: ${
            err instanceof Error ? err.message : String(err)
          }`
        }
      }
      attaching = false
      attach.disabled = false
    }
    const commit = (submit: boolean) => {
      const text = buffer.value.trim()
      if (!text) return
      // Typing and dictation both edit a normal textarea. Only these explicit
      // actions cross into xterm, as one paste and optionally one Enter.
      closeInputPanel()
      if (submit) pasteAndSubmitTerminal(id, text)
      else pasteIntoTerminal(id, text)
    }
    buffer.addEventListener("input", updateActions)
    cancel.addEventListener("click", closeInputPanel)
    insert.addEventListener("click", () => commit(false))
    enter.addEventListener("click", () => commit(true))
    attach.addEventListener("click", () => picker.click())
    picker.addEventListener("change", () => {
      // The selection must be reset or picking the same file twice fires no
      // second change event — but only after the upload has read the File,
      // which is backed by that selection.
      void attachFiles(picker.files).finally(() => {
        picker.value = ""
      })
    })
    buffer.addEventListener("paste", (event: ClipboardEvent) => {
      const clipboard = event.clipboardData
      // Text wins whenever the clipboard carries any: a rich copy hands over
      // both, and the buffer is a text field first.
      if (!clipboard || clipboard.getData("text/plain")) return
      const files = Array.from(clipboard.items)
        .filter((item) => item.kind === "file")
        .map((item) => item.getAsFile())
        .filter((file): file is File => file !== null)
      if (!files.length) return
      event.preventDefault()
      void attachFiles(files)
    })

    // Focusing synchronously from the dial gesture opens the software keyboard
    // with its microphone available, isolating buffered input from xterm.
    buffer.focus({ preventScroll: true })
    buffer.setSelectionRange(buffer.value.length, buffer.value.length)
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
    if (target.kind === "input") {
      openInputPanel()
      return
    }
    if (target.key) sendKeyToTerminal(id, target.key)
    setActive(null)
  }

  const makeItem = (target: DialTarget) => {
    const button = renderDialItem(doc, menu, target)
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
    if (inputPanel) closeInputPanel()
    else if (open) close()
  })

  return () => {
    // Order matters: the gesture state has to be released before the DOM goes,
    // since unlocking restores xterm's disableStdin and drops the tracking class
    // from <html> — neither of which lives inside the dial.
    clearGesture()
    endSwallow()
    closeInputPanel()
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

// The app document's own dial: ONE per tab, and a child of .term-shell — the
// terminal's own wrapper — rather than of <body>. Anchoring it there is what
// keeps it inside the terminal: it rides the pane's bottom-right corner through
// every sidebar drag and collapse, never floats over the sidebar, and never
// over the usage footer (a flex sibling of the whole panel group, so the
// terminal's box already excludes it — footer on or off, the dial moves with
// the pane it lives in). It still lives in the APP document, not in an iframe,
// so one dial serves every pane and the app's own keyboard handling reaches it;
// mounting per iframe would put a second and third copy on screen the moment a
// tab shows two terminals.
//
// Returned teardown removes the node, the stylesheet and every listener, so the
// caller can hand it straight to React as an effect cleanup, and a capability
// change (a mouse plugged into a tablet, a hybrid folding into tablet mode)
// swaps mounts instead of stacking them.
export function mountDesktopNavigationDial(): () => void {
  if (typeof window === "undefined" || typeof document === "undefined") {
    return () => {}
  }
  // Fine pointer AND hover, together: coarse-only devices keep the touch dial
  // inside the terminal iframe, where preventDefault preserves the iOS
  // keyboard. A hybrid reports both media and gets both dials, which is right —
  // each answers the input it was built for.
  const media = window.matchMedia(DESKTOP_MEDIA)
  let teardown: (() => void) | null = null

  const sync = () => {
    if (media.matches && !teardown) teardown = attachDesktopDial()
    else if (!media.matches && teardown) {
      teardown()
      teardown = null
    }
  }
  sync()
  media.addEventListener("change", sync)

  return () => {
    media.removeEventListener("change", sync)
    teardown?.()
    teardown = null
  }
}

// The dial mounts into .term-shell, which the app renders unconditionally — but
// an effect can still run before the panel group has laid it out, so resolving
// it retries briefly rather than falling back to <body>, where it would sit
// over the sidebar and the footer.
function attachDesktopDial(): () => void {
  let cancelled = false
  let release: (() => void) | null = null

  const attempt = (tries: number) => {
    if (cancelled) return
    const anchor = document.querySelector<HTMLElement>(".term-shell")
    if (!anchor) {
      if (tries < 20) window.setTimeout(() => attempt(tries + 1), 150)
      return
    }
    release = buildDesktopDial(anchor)
  }
  attempt(0)

  return () => {
    cancelled = true
    release?.()
    release = null
  }
}

function buildDesktopDial(anchor: HTMLElement): () => void {
  const doc = document
  // A hot reload or a StrictMode double-mount must leave one dial, not two.
  doc.getElementById(DESKTOP_DIAL_ID)?.remove()
  doc.getElementById(DESKTOP_STYLE_ID)?.remove()

  const style = doc.createElement("style")
  style.id = DESKTOP_STYLE_ID
  style.textContent = dialCSS(DESKTOP_DIAL_ID, "desktop")
  doc.head.appendChild(style)

  const dial = doc.createElement("div")
  dial.id = DESKTOP_DIAL_ID
  dial.setAttribute("role", "group")
  dial.setAttribute("aria-label", "Lasso navigation")

  const root = doc.createElement("button")
  root.type = "button"
  root.className = "dial-root"
  renderRootGlyph(doc, root, "⌘")
  root.title = "Navigation"
  root.setAttribute("aria-label", "Open navigation")
  root.setAttribute("aria-expanded", "false")
  root.setAttribute("aria-haspopup", "true")

  const menu = doc.createElement("div")
  menu.className = "dial-menu"

  // Root before the menu, unlike the touch dial: Tab must land on the control
  // that opens the fan before the items it reveals.
  dial.append(root, menu)
  anchor.appendChild(dial)
  // No theme copying and no MutationObserver here — the dial is in the document
  // that owns the --h-* palette, so a re-theme repaints it for free. (The touch
  // dial has to mirror them across the iframe boundary.)

  let open = false
  let activeID: string | null = null
  let closeTimer: number | undefined
  let restoreTimer: number | undefined
  let corridor: {
    left: number
    right: number
    top: number
    bottom: number
  } | null = null
  // Which arrangement the last open used. Chosen per open from the pane's own
  // width, so a sidebar drag is reflected the next time the fan comes out
  // without anything having to observe the layout.
  let targets: readonly DialTarget[] = DESKTOP_TARGETS
  let pointerInside = false
  let tracking = false

  // Which terminal the human was in when the fan opened, so a dismissal can
  // hand the keyboard back instead of leaving it on the dial (or on nothing).
  // /shell/ is remembered when it was the prior target; anything else — the
  // sidebar, a dialog field, a fresh page — defaults to the herdr terminal,
  // which is where typing goes.
  let focusReturn = "term"

  const captureFocusReturn = () => {
    const active = doc.activeElement
    focusReturn =
      active instanceof HTMLIFrameElement && active.id === "shellframe"
        ? "shellframe"
        : "term"
  }

  // Restoring is conditional on nobody else having claimed the keyboard: the
  // dial opens on hover, so the caret may well be in the file editor or a
  // dialog field that the mouse merely wandered past, and yanking it out of
  // there would be worse than leaving focus where it is. Nor after a real
  // window blur — they alt-tabbed, and grabbing focus back would eat the
  // keystroke they meant for something else.
  const claimTerminalFocus = () => {
    if (!doc.hasFocus()) return
    const active = doc.activeElement
    const claimed =
      !!active &&
      active !== doc.body &&
      active !== doc.documentElement &&
      !dial.contains(active) &&
      !(
        active instanceof HTMLIFrameElement &&
        (active.id === "term" || active.id === "shellframe")
      )
    if (claimed) return
    focusTerminalFrame(focusReturn)
  }

  // Twice, on purpose. The synchronous pass is what a dismissing pointerdown
  // needs: it runs before the event's own default action, so a click that
  // landed on an input still takes the focus off the terminal a moment later,
  // naturally. But that default action ALSO blurs to <body> when the click
  // landed on inert chrome (the footer, a label), which would leave the
  // keyboard nowhere — so the claim is re-asserted on the next tick, under the
  // same "has anyone else claimed it" guard.
  const restoreTerminalFocus = () => {
    claimTerminalFocus()
    if (restoreTimer !== undefined) window.clearTimeout(restoreTimer)
    restoreTimer = window.setTimeout(() => {
      restoreTimer = undefined
      claimTerminalFocus()
    }, 0)
  }

  const cancelClose = () => {
    if (closeTimer !== undefined) window.clearTimeout(closeTimer)
    closeTimer = undefined
  }

  const itemButtons = () =>
    Array.from(menu.querySelectorAll<HTMLButtonElement>(".dial-item"))

  const setActive = (id: string | null) => {
    if (activeID === id) return
    activeID = id
    for (const item of itemButtons()) {
      item.dataset.active = String(item.dataset.target === id)
    }
  }

  // Measured from the TARGET TABLE, not from the items' rects: they animate in
  // from scale(.72), so their boxes are wrong for exactly the frames in which
  // the pointer is travelling out to them.
  const measureCorridor = () => {
    const rect = root.getBoundingClientRect()
    const cx = rect.left + rect.width / 2
    const cy = rect.top + rect.height / 2
    let left = rect.left
    let right = rect.right
    let top = rect.top
    let bottom = rect.bottom
    for (const target of targets) {
      const width = target.width ?? ITEM_SIZE
      left = Math.min(left, cx + target.x - width / 2)
      right = Math.max(right, cx + target.x + width / 2)
      top = Math.min(top, cy + target.y - ITEM_SIZE / 2)
      bottom = Math.max(bottom, cy + target.y + ITEM_SIZE / 2)
    }
    corridor = {
      left: left - DESKTOP_CORRIDOR_PAD,
      right: right + DESKTOP_CORRIDOR_PAD,
      top: top - DESKTOP_CORRIDOR_PAD,
      bottom: bottom + DESKTOP_CORRIDOR_PAD,
    }
  }

  const inCorridor = (x: number, y: number) =>
    !!corridor &&
    x >= corridor.left &&
    x <= corridor.right &&
    y >= corridor.top &&
    y <= corridor.bottom

  // Leaving a pill is not leaving the dial — the fan has gaps between its items
  // and between the ring and the root, and a menu that closed on the way across
  // one would be unusable. So membership is the whole fan's box, and even
  // leaving that only ARMS the close, which the next move back in cancels.
  const handlePointerAt = (x: number, y: number) => {
    if (!open) return
    pointerInside = inCorridor(x, y)
    if (pointerInside) cancelClose()
    else scheduleClose(true)
  }

  const onPointerMove = (event: PointerEvent) => {
    if (event.pointerType === "touch") return
    handlePointerAt(event.clientX, event.clientY)
  }

  const onPointerLeaveDocument = () => {
    if (!open) return
    pointerInside = false
    scheduleClose(true)
  }

  const onResize = () => {
    if (open) measureCorridor()
  }

  // The terminal is an IFRAME filling the pane, and a mouse over an iframe
  // generates pointer events in that document alone — the parent document sees
  // nothing at all. Every gap in the fan lies over the terminal, so without
  // this the corridor watch goes blind the moment the cursor leaves a pill:
  // travelling from the root to an item never confirms it is still inside, and
  // leaving for good is never noticed, which leaves the fan hanging open. The
  // frames are same-origin (ttyd behind our own proxy), so their pointermove is
  // readable; the coordinates are frame-relative and get the frame's offset
  // added to compare against a corridor measured in parent-viewport space.
  const frameWatches: Array<() => void> = []

  const watchTerminalFrames = () => {
    for (const id of TERMINAL_FRAME_IDS) {
      const frame = doc.getElementById(id) as HTMLIFrameElement | null
      if (!frame) continue
      const onFrameMove = (event: Event) => {
        const pointer = event as PointerEvent
        if (pointer.pointerType === "touch") return
        const rect = frame.getBoundingClientRect()
        handlePointerAt(pointer.clientX + rect.left, pointer.clientY + rect.top)
      }
      // A press inside the terminal is a dismissal like any other click
      // outside the dial — and this is the only way to see it, since the
      // parent's own pointerdown never fires for it. No focus restore: that
      // press is xterm taking the keyboard, which is where it was going anyway.
      const onFrameDown = () => close()
      try {
        const frameWin = frame.contentWindow
        if (!frameWin) continue
        frameWin.addEventListener("pointermove", onFrameMove, { passive: true })
        frameWin.addEventListener("pointerdown", onFrameDown, {
          capture: true,
          passive: true,
        })
        frameWatches.push(() => {
          frameWin.removeEventListener("pointermove", onFrameMove)
          frameWin.removeEventListener("pointerdown", onFrameDown, {
            capture: true,
          })
        })
      } catch {
        /* a frame we cannot reach into just goes unwatched */
      }
    }
  }

  // The corridor watch only exists while the fan is out, so a closed dial costs
  // nothing on a document that already sees every mouse move.
  const startTracking = () => {
    if (tracking) return
    tracking = true
    window.addEventListener("pointermove", onPointerMove, { passive: true })
    window.addEventListener("resize", onResize)
    doc.addEventListener("pointerleave", onPointerLeaveDocument)
    watchTerminalFrames()
  }

  const stopTracking = () => {
    if (!tracking) return
    tracking = false
    window.removeEventListener("pointermove", onPointerMove)
    window.removeEventListener("resize", onResize)
    doc.removeEventListener("pointerleave", onPointerLeaveDocument)
    for (const unwatch of frameWatches) unwatch()
    frameWatches.length = 0
  }

  // `restore` is what tells a DISMISSAL from a hand-off. A dismissal (the
  // cursor leaving, a click outside, Escape, the root toggled shut) means the
  // human is done with the dial and wants the keyboard back where it was; a
  // hand-off (a command, a Tab out, a window blur, an unmount) must leave focus
  // to whatever is taking over — the dialog the command just opened, above all.
  function close(restore = false) {
    cancelClose()
    stopTracking()
    if (!open) return
    open = false
    activeID = null
    corridor = null
    pointerInside = false
    menu.replaceChildren()
    root.setAttribute("aria-expanded", "false")
    root.setAttribute("aria-label", "Open navigation")
    if (restore) restoreTerminalFocus()
  }

  function scheduleClose(restore = false) {
    if (!open || closeTimer !== undefined) return
    closeTimer = window.setTimeout(() => {
      closeTimer = undefined
      close(restore)
    }, DESKTOP_CLOSE_MS)
  }

  const focusItem = (index: number) => {
    const all = itemButtons()
    if (!all.length) return
    all[(index + all.length) % all.length]?.focus()
  }

  const run = (target: DialTarget) => {
    if (!target.command) return
    // Closed BEFORE the command lands: every desktop action opens a dialog, a
    // popover or the sidebar, and the fan must not be sitting over the thing it
    // just asked for (nor keep the focus the dialog is about to want).
    close()
    emitMobileCommand(target.command)
    // Sidebar is the only action with no dialog to claim focus. Return to
    // Herdr, especially when hiding the sidebar's previously focused shell.
    if (target.command === "sidebar") focusTerminalFrame("term")
  }

  const onItemKey = (event: KeyboardEvent, button: HTMLButtonElement) => {
    const all = itemButtons()
    const index = all.indexOf(button)
    // Up/left runs toward the top of the arc (New), down/right back toward its
    // foot, which is the order the items are declared in.
    if (event.key === "ArrowUp" || event.key === "ArrowLeft") {
      event.preventDefault()
      focusItem(index - 1)
    } else if (event.key === "ArrowDown" || event.key === "ArrowRight") {
      event.preventDefault()
      focusItem(index + 1)
    } else if (event.key === "Home") {
      event.preventDefault()
      focusItem(0)
    } else if (event.key === "End") {
      event.preventDefault()
      focusItem(all.length - 1)
    } else if (event.key === "Escape") {
      event.preventDefault()
      // Back to the root rather than out to the terminal: a keyboard user
      // stepping out of the ring is still in the dial, and a second Escape
      // there is what hands the keyboard back. Guarded, because focusing the
      // root would otherwise re-run the focus-opens rule and reopen the fan
      // that was just dismissed.
      reopenGuard = true
      close()
      root.focus()
      reopenGuard = false
    }
  }

  function makeItem(target: DialTarget) {
    const button = renderDialItem(doc, menu, target)
    button.addEventListener("pointerenter", () => {
      cancelClose()
      pointerInside = true
      setActive(target.id)
    })
    button.addEventListener("pointerleave", () => {
      if (activeID === target.id) setActive(null)
    })
    button.addEventListener("focus", () => {
      cancelClose()
      setActive(target.id)
    })
    button.addEventListener("blur", () => {
      if (activeID === target.id) setActive(null)
    })
    button.addEventListener("click", (event) => {
      event.preventDefault()
      run(target)
    })
    button.addEventListener("keydown", (event) => onItemKey(event, button))
    window.requestAnimationFrame(() => button.classList.add("is-visible"))
  }

  function show() {
    cancelClose()
    if (open) return
    open = true
    activeID = null
    root.setAttribute("aria-expanded", "true")
    root.setAttribute("aria-label", "Close navigation")
    menu.replaceChildren()
    // A pinched pane cannot hold the fan, so the arrangement is decided here,
    // per open, off the terminal's own width — no layout observer, and a
    // sidebar drag is accounted for by the time the fan next comes out.
    targets =
      anchor.clientWidth < DESKTOP_FAN_MIN_WIDTH
        ? DESKTOP_COLUMN_TARGETS
        : DESKTOP_TARGETS
    captureFocusReturn()
    for (const target of targets) makeItem(target)
    measureCorridor()
    startTracking()
  }

  let reopenGuard = false

  const onRootEnter = () => {
    pointerInside = true
    show()
  }
  const onRootFocus = () => {
    if (reopenGuard) return
    show()
  }
  const onRootClick = () => {
    if (open) close(true)
    else show()
  }
  const onRootKey = (event: KeyboardEvent) => {
    if (event.key === "Escape") {
      event.preventDefault()
      // From the root, Escape ends the interaction: hand the keyboard back
      // rather than leave it parked on the dial. Also when the fan is already
      // shut — that is the second Escape of a keyboard exit (the first stepped
      // out of the ring and back onto the root), and it means the same thing.
      if (open) close(true)
      else restoreTerminalFocus()
      return
    }
    const toFirst =
      event.key === "Enter" ||
      event.key === " " ||
      event.key === "ArrowUp" ||
      event.key === "ArrowLeft"
    const toLast = event.key === "ArrowDown" || event.key === "ArrowRight"
    if (!toFirst && !toLast) return
    // preventDefault so Enter/Space never reaches the click toggle: focus has
    // already opened the fan for a keyboard user, and "activate" from there
    // means step into it, not shut it again.
    event.preventDefault()
    show()
    focusItem(toFirst ? 0 : itemButtons().length - 1)
  }

  // Tabbing out of the dial closes it, but a mouse whose pointer is still in
  // the corridor keeps it: clicking a pill moves focus around inside the fan.
  // No restore — focus has gone somewhere the human aimed it.
  const onFocusOut = (event: FocusEvent) => {
    if (!open || pointerInside) return
    const next = event.relatedTarget as Node | null
    if (next && dial.contains(next)) return
    close()
  }

  // Escape from anywhere — the fan is routinely opened by hover, with the focus
  // still wherever the human left it.
  const onDocumentKey = (event: KeyboardEvent) => {
    if (open && event.key === "Escape") close(true)
  }

  // A click outside dismisses, and is deliberately NOT swallowed. The touch
  // dial has to eat its dismissing gesture (a finger over a mouse-reporting TUI
  // would otherwise also click inside the agent's own UI); on a desktop the
  // click that puts the fan away is an ordinary click on a terminal, a pane or
  // the sidebar, and stealing it would be the bug. The restore runs
  // SYNCHRONOUSLY here, before the pointerdown's own default action, so a click
  // that landed on something focusable still takes the focus straight back off
  // the terminal a moment later — and a click on inert chrome leaves the
  // keyboard in the terminal instead of nowhere.
  const onDocumentPointerDown = (event: PointerEvent) => {
    if (!open || dial.contains(event.target as Node)) return
    close(true)
  }

  // A real window blur (they alt-tabbed, or focus crossed into a terminal
  // iframe, whose events never reach this document) closes WITHOUT restoring:
  // the focus is already where it was going, and grabbing it back would eat the
  // next keystroke.
  const onWindowBlur = () => close()

  // The dial's own hit region — the root plus its items, gaps excluded. This is
  // the fallback for a surface whose pointer we cannot read at all (a
  // cross-origin frame, a plugin): the close is armed on leaving a control and
  // cancelled by re-entering one or by a corridor move, so an unreadable
  // surface still puts the fan away instead of leaving it hanging.
  const onDialEnter = () => {
    pointerInside = true
    cancelClose()
  }
  const onDialLeave = () => {
    if (!open) return
    pointerInside = false
    scheduleClose(true)
  }

  root.addEventListener("pointerenter", onRootEnter)
  root.addEventListener("focus", onRootFocus)
  root.addEventListener("click", onRootClick)
  root.addEventListener("keydown", onRootKey)
  dial.addEventListener("focusout", onFocusOut)
  dial.addEventListener("pointerenter", onDialEnter)
  dial.addEventListener("pointerleave", onDialLeave)
  doc.addEventListener("keydown", onDocumentKey)
  doc.addEventListener("pointerdown", onDocumentPointerDown)
  window.addEventListener("blur", onWindowBlur)

  return () => {
    close()
    doc.removeEventListener("keydown", onDocumentKey)
    doc.removeEventListener("pointerdown", onDocumentPointerDown)
    window.removeEventListener("blur", onWindowBlur)
    if (restoreTimer !== undefined) window.clearTimeout(restoreTimer)
    dial.remove()
    style.remove()
  }
}
