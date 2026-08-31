import { emitMobileCommand, type MobileCommand } from "@/lib/mobile-command"
import {
  pasteAndSubmitTerminal,
  pasteIntoTerminal,
  sendKeyToTerminal,
  type VirtualKey,
} from "@/lib/terminal"

// Touch-only input controls injected inside each same-origin terminal iframe.
// Keeping the dial beside xterm's textarea is intentional: preventDefault on a
// same-document pointer gesture preserves the iOS software keyboard, while a
// control in the parent document would dismiss it and could not reopen it.

// Exported so terminal.ts can tell a tap on the dial from a tap on the terminal.
export const DIAL_ID = "__lasso_mobile_input_dial"
const STYLE_ID = "__lasso_mobile_input_dial_style"
const TRACKING_CLASS = "__lasso_mobile_input_dial_tracking"
const HOLD_MS = 140
const ROOT_SIZE = 58
const ITEM_SIZE = 54
const BACK_RADIUS = 44
const TERMINAL_BOTTOM_GAP = 24

type DialLevel = "root" | "keys" | "app"
type TargetKind = "branch" | "command" | "input" | "key"

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
// (index.css): surfaces are OPAQUE and separate by border + a panel/hover
// brightness step — never by drop shadow, translucency, or backdrop blur. It
// used to float as a 30%-alpha blurred panel, which on the light palette mixed
// the control into the terminal underneath and read washed out; the two states
// that matter now carry real contrast instead. Brightness is the hierarchy:
// --h-hover (surface-raised) for the root, --h-panel for the ring of items, and
// the monochrome --h-accent fill (white on dark, black on light) for whatever is
// armed. That inverts correctly in both modes, which a hand-mixed tint does not.
function dialCSS(): string {
  return `
#${DIAL_ID} {
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
#terminal-container {
  height: calc(100% - ${TERMINAL_BOTTOM_GAP}px) !important;
}
#${DIAL_ID} button {
  appearance: none;
  -webkit-appearance: none;
  -webkit-tap-highlight-color: transparent;
}
#${DIAL_ID} .dial-root {
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
  background: var(--h-hover, #1a1a1a);
  color: var(--h-fg, #ededed);
  font: 700 24px/1 ui-monospace, SFMono-Regular, Menlo, monospace;
  cursor: grab;
  pointer-events: auto;
  touch-action: none;
  transition: transform 120ms ease, background 120ms ease, border-color 120ms ease, color 120ms ease;
}
#${DIAL_ID} .dial-root[aria-expanded="true"] {
  border-color: var(--h-accent, #fff);
  background: var(--h-accent, #fff);
  color: var(--h-bg, #000);
}
#${DIAL_ID} .dial-root:active {
  cursor: grabbing;
  transform: scale(.94);
}
/* Pressing a CLOSED dial dips its surface; an open one must keep the accent fill
   (equal specificity otherwise lets this rule win and drop the armed state). */
#${DIAL_ID} .dial-root[aria-expanded="false"]:active {
  background: var(--h-panel, #111);
}
#${DIAL_ID} .dial-root:focus-visible,
#${DIAL_ID} .dial-item:focus-visible {
  outline: 2px solid var(--h-accent, #fff);
  outline-offset: 3px;
}
#${DIAL_ID} .dial-menu {
  position: absolute;
  inset: 0;
  pointer-events: none;
}
#${DIAL_ID} .dial-line {
  position: absolute;
  left: ${ROOT_SIZE / 2}px;
  top: ${ROOT_SIZE / 2}px;
  z-index: 0;
  height: 0;
  border-top: 1px dashed var(--h-muted, #8a8a8a);
  transform-origin: 0 50%;
  pointer-events: none;
}
#${DIAL_ID} .dial-item {
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
  background: var(--h-panel, #111);
  color: var(--h-fg, #ededed);
  font: 700 17px/1 ui-monospace, SFMono-Regular, Menlo, monospace;
  opacity: 0;
  transform: scale(.72);
  pointer-events: auto;
  touch-action: none;
  transition: opacity 130ms ease, transform 130ms ease, background 90ms ease, color 90ms ease;
}
#${DIAL_ID} .dial-item.is-visible {
  opacity: 1;
  transform: scale(1);
}
#${DIAL_ID} .dial-item[data-active="true"] {
  border-color: var(--h-accent, #fff);
  background: var(--h-accent, #fff);
  color: var(--h-bg, #000);
  transform: scale(1.1);
}
#${DIAL_ID} .dial-item::after {
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
#${DIAL_ID} .dial-item[data-active="true"]::after {
  opacity: 1;
  visibility: visible;
}
#${DIAL_ID} .dial-branch {
  justify-content: center;
  padding: 0 12px;
  font-size: 14px;
}
#${DIAL_ID} .dial-branch .dial-glyph {
  color: var(--h-accent, #fff);
  font-size: 18px;
}
#${DIAL_ID} .dial-branch[data-active="true"] .dial-glyph {
  color: inherit;
}
#${DIAL_ID} .dial-crumb {
  position: absolute;
  left: -198px;
  top: -210px;
  z-index: 1;
  display: flex;
  align-items: center;
  gap: 7px;
  min-height: 36px;
  padding: 0 13px;
  border: 1px solid var(--dial-edge);
  border-radius: 999px;
  background: var(--h-panel, #111);
  color: var(--h-muted, #8a8a8a);
  font-size: 12px;
  white-space: nowrap;
  pointer-events: none;
}
#${DIAL_ID} .dial-crumb strong {
  color: var(--h-fg, #ededed);
  font-weight: 600;
}
#${DIAL_ID} .input-panel {
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
#${DIAL_ID} .input-header,
#${DIAL_ID} .input-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}
#${DIAL_ID} .input-header {
  justify-content: space-between;
  font: 600 13px/1.2 ui-monospace, SFMono-Regular, Menlo, monospace;
}
#${DIAL_ID} .input-status {
  color: var(--h-muted, #8a8a8a);
  font-size: 11px;
  font-weight: 500;
}
#${DIAL_ID} .input-buffer {
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
#${DIAL_ID} .input-buffer:focus {
  border-color: var(--h-accent, #fff);
}
#${DIAL_ID} .input-buffer::placeholder {
  color: var(--h-muted, #8a8a8a);
}
#${DIAL_ID} .input-actions {
  justify-content: flex-end;
}
#${DIAL_ID} .input-action {
  min-height: 38px;
  padding: 0 13px;
  border: 1px solid var(--dial-edge);
  border-radius: 999px;
  background: var(--h-hover, #1a1a1a);
  color: var(--h-fg, #ededed);
  font: 600 12px/1 ui-monospace, SFMono-Regular, Menlo, monospace;
}
#${DIAL_ID} .input-action.primary {
  border-color: var(--h-accent, #fff);
  background: var(--h-accent, #fff);
  color: var(--h-bg, #000);
}
#${DIAL_ID} .input-action:disabled {
  opacity: .42;
}
@media (prefers-reduced-motion: reduce) {
  #${DIAL_ID} .dial-root,
  #${DIAL_ID} .dial-item { transition: none; }
}
`
}

function targetCenter(target: DialTarget): { x: number; y: number } {
  return {
    x: ROOT_SIZE / 2 + target.x,
    y: ROOT_SIZE / 2 + target.y,
  }
}

// mountTerminalInputDial is idempotent and touch-only. ttyd may not have built
// #terminal-container yet when the iframe load event fires, so mounting retries
// briefly just like the old key bar did.
export function mountTerminalInputDial(id: string, tries = 0): void {
  const frame = document.getElementById(id) as HTMLIFrameElement | null
  const win = frame?.contentWindow as Window | null
  if (!win?.matchMedia?.("(pointer: coarse)").matches) return

  const doc = win.document
  if (doc.getElementById(DIAL_ID)) return
  if (!doc.getElementById("terminal-container")) {
    if (tries < 20) {
      win.setTimeout(() => mountTerminalInputDial(id, tries + 1), 150)
    }
    return
  }

  if (!doc.getElementById(STYLE_ID)) {
    const style = doc.createElement("style")
    style.id = STYLE_ID
    style.textContent = dialCSS()
    doc.head.appendChild(style)
  }

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
  win.addEventListener("pagehide", () => themeObserver.disconnect(), {
    once: true,
  })

  const menu = doc.createElement("div")
  menu.className = "dial-menu"

  const root = doc.createElement("button")
  root.type = "button"
  root.className = "dial-root"
  root.textContent = "⌘"
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
  win.addEventListener(
    "touchmove",
    (event) => {
      if (!inputLocked) return
      event.preventDefault()
      event.stopImmediatePropagation()
    },
    { capture: true, passive: false }
  )
  win.addEventListener(
    "wheel",
    (event) => {
      if (!inputLocked) return
      event.preventDefault()
      event.stopImmediatePropagation()
    },
    { capture: true, passive: false }
  )

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
    root.textContent = "⌘"
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
    status.textContent = "Type or use the keyboard microphone"
    header.append(title, status)

    const buffer = doc.createElement("textarea")
    buffer.className = "input-buffer"
    buffer.placeholder = "Type or dictate, then insert or submit."
    buffer.spellcheck = true
    buffer.inputMode = "text"
    buffer.autocapitalize = "sentences"
    buffer.enterKeyHint = "done"
    buffer.setAttribute("aria-label", "Buffered terminal input")

    const actions = doc.createElement("div")
    actions.className = "input-actions"
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
    actions.append(cancel, insert, enter)
    panel.append(header, buffer, actions)
    dial.appendChild(panel)
    inputPanel = panel

    const updateActions = () => {
      const disabled = !buffer.value.trim()
      insert.disabled = disabled
      enter.disabled = disabled
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
    menu.appendChild(button)
    win.requestAnimationFrame(() => button.classList.add("is-visible"))
  }

  function show(nextLevel: DialLevel) {
    open = true
    level = nextLevel
    activeID = null
    menu.replaceChildren()
    const inBranch = level !== "root"
    root.textContent = inBranch ? "‹" : "⌘"
    root.title = inBranch ? "Back to input controls" : "Close input controls"
    root.setAttribute(
      "aria-label",
      inBranch ? "Back to input controls" : "Close input controls"
    )
    root.setAttribute("aria-expanded", "true")

    if (inBranch) {
      const isKeys = level === "keys"
      const crumb = doc.createElement("div")
      crumb.className = "dial-crumb"
      const glyph = doc.createElement("span")
      glyph.textContent = isKeys ? "⌘ ›" : "◆ ›"
      const label = doc.createElement("strong")
      label.textContent = isKeys ? "Common keys" : "Lasso"
      crumb.append(glyph, label)
      menu.appendChild(crumb)
    }
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

  doc.addEventListener(
    "pointerdown",
    (event) => {
      if (open && !dial.contains(event.target as Node)) close()
    },
    true
  )
  doc.addEventListener("keydown", (event) => {
    if (event.key !== "Escape") return
    if (inputPanel) closeInputPanel()
    else if (open) close()
  })
}
