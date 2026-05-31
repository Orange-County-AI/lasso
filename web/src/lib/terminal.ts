import { api } from "@/lib/api"
import { applyTermTheme, lastTerminalTheme } from "@/lib/theme"

// Behavior attached to the same-origin ttyd terminal iframes (/terminal/ and
// /shell/). All of this is ported faithfully from the original index.html —
// xterm.js can only paste text, sends a bare CR for both Enter and Shift+Enter,
// and starts from ttyd's theme on every reconnect, so we patch around each.

interface XTerm {
  paste?: (text: string) => void
  input?: (data: string) => void
  focus?: () => void
  options?: Record<string, unknown>
  attachCustomKeyEventHandler?: (h: (e: KeyboardEvent) => boolean) => void
  _core?: {
    coreService?: { triggerDataEvent?: (data: string, sync?: boolean) => void }
  }
  __herdrShiftEnter?: boolean
}
interface TermWindow extends Window {
  term?: XTerm
}
interface WiredDoc extends Document {
  __herdrWired?: boolean
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

  if (suppressContext)
    doc.addEventListener("contextmenu", (e) => e.preventDefault(), true)

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
}

// bootTermFrame wires the iframe now and re-wires (and re-applies the latest
// theme) on every reload, since ttyd reconnects yield a fresh xterm.
export function bootTermFrame(id: string, suppressContext: boolean) {
  const el = document.getElementById(id) as HTMLIFrameElement | null
  if (!el) return () => {}
  const onLoad = () => {
    applyTermTheme(lastTerminalTheme(), 0)
    wireTerminalIframe(id, suppressContext)
  }
  el.addEventListener("load", onLoad)
  wireTerminalIframe(id, suppressContext) // in case it already loaded
  return () => el.removeEventListener("load", onLoad)
}

// reloadTermFrame reloads a terminal iframe onto its (now host-switched) ttyd
// session. The ttyd was respawned server-side with the new host's command, so
// the old WebSocket is dead; reloading the same-origin frame reconnects and
// bootTermFrame's load handler re-wires xterm + re-applies the theme.
export function reloadTermFrame(id: string) {
  const el = document.getElementById(id) as HTMLIFrameElement | null
  if (!el) return
  try {
    el.contentWindow?.location.reload()
  } catch {
    // Cross-origin shouldn't happen (same-origin proxy), but fall back to a
    // src round-trip just in case.
    const src = el.src
    el.src = "about:blank"
    el.src = src
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

// Hand keyboard focus to the herdr terminal after focusing a pane.
export function focusHerdrTerminal() {
  try {
    frameWindow("term")?.focus()
  } catch {
    /* ignore */
  }
}
