import { api, type ThemePayload } from "@/lib/api"

// The fixed terminal iframes whose xterm.js theme we keep in sync with herdr's
// (the left "Herdr" terminal and the right shell). The Grid tab's per-pane
// terminals are matched by class instead — see termFrames.
const TERM_FRAME_IDS = ["term", "shellframe"]

// GRID_FRAME_CLASS marks each Grid cell's terminal iframe so it's re-themed
// alongside the fixed terminals (its id is dynamic, one per host+pane).
export const GRID_FRAME_CLASS = "gridterm"

// termFrames collects every terminal iframe the theme should track: the two
// fixed ones by id, plus every live Grid cell terminal by class.
function termFrames(): HTMLIFrameElement[] {
  const out: HTMLIFrameElement[] = []
  for (const id of TERM_FRAME_IDS) {
    const el = document.getElementById(id) as HTMLIFrameElement | null
    if (el) out.push(el)
  }
  out.push(
    ...Array.from(
      document.querySelectorAll<HTMLIFrameElement>(`iframe.${GRID_FRAME_CLASS}`)
    )
  )
  return out
}

// The terminal font stack. JetBrainsMono Nerd Font carries the icon glyphs that
// TUIs (gh-dash, btop, …) draw with — without it xterm renders "tofu" boxes.
// The face is vendored as woff2 under web/public/fonts and served at /fonts/*.
const TERM_FONT_FAMILY = "JetBrainsMono Nerd Font"
const TERM_FONT_STACK = `"${TERM_FONT_FAMILY}", ui-monospace, monospace`
const TERM_FONT_STYLE_ID = "herdr-term-font"

// The @font-face must live in the *terminal iframe's* document — a parent
// stylesheet doesn't cross the iframe boundary. We mirror index.css here so the
// same family resolves inside ttyd's xterm. Same-origin proxying lets us reach
// in (see applyTermTheme); /fonts/* is the embedded build's stable URL.
const TERM_FONT_FACE_CSS = (
  ["Regular", "Bold", "Italic", "BoldItalic"] as const
)
  .map((variant) => {
    const weight = variant.startsWith("Bold") ? 700 : 400
    const style = variant.endsWith("Italic") ? "italic" : "normal"
    return `@font-face{font-family:"${TERM_FONT_FAMILY}";font-style:${style};font-weight:${weight};font-display:swap;src:url("/fonts/JetBrainsMonoNerdFontMono-${variant}.woff2") format("woff2")}`
  })
  .join("")

interface FontDoc extends Document {
  __herdrFontWired?: boolean
}

// Once the webfont is actually loaded inside the iframe, set xterm's fontFamily.
// We deliberately set it *after* the load resolves (not before): xterm only
// rebuilds its glyph atlas when the option value changes, so assigning the final
// family after the font is ready guarantees a remeasure against real metrics
// rather than the fallback it would otherwise cache at startup.
function setTermFontWhenReady(
  doc: Document,
  term: { options?: Record<string, unknown> }
) {
  const apply = () => {
    try {
      if (term.options) term.options.fontFamily = TERM_FONT_STACK
    } catch {
      /* private/locked options: never break the terminal */
    }
  }
  const fonts = (doc as Document & { fonts?: FontFaceSet }).fonts
  if (fonts && typeof fonts.load === "function") {
    Promise.all([
      fonts.load(`400 1em "${TERM_FONT_FAMILY}"`),
      fonts.load(`700 1em "${TERM_FONT_FAMILY}"`),
    ]).then(apply, apply)
  } else {
    setTimeout(apply, 300)
  }
}

// applyTermFont injects the Nerd Font @font-face into every terminal iframe and
// points xterm at it. Mirrors applyTermTheme: iterates the same frames (fixed +
// Grid cells), and retries while an iframe is still (re)connecting. Each fresh
// xterm lives in a fresh iframe document, so the per-document guard re-arms on
// ttyd reconnects.
export function applyTermFont(tries = 0) {
  let pending = false
  for (const el of termFrames()) {
    try {
      const doc = el.contentDocument as FontDoc | null
      if (!doc?.head) {
        pending = true
        continue
      }
      // Inject the @font-face ASAP (even before xterm is ready) so the browser
      // starts fetching; idempotent via the style id.
      if (!doc.getElementById(TERM_FONT_STYLE_ID)) {
        const style = doc.createElement("style")
        style.id = TERM_FONT_STYLE_ID
        style.textContent = TERM_FONT_FACE_CSS
        doc.head.appendChild(style)
      }
      const w = el.contentWindow as unknown as {
        term?: { options?: Record<string, unknown> }
      }
      if (!w?.term?.options) {
        pending = true
        continue
      }
      if (doc.__herdrFontWired) continue
      doc.__herdrFontWired = true
      setTermFontWhenReady(doc, w.term)
    } catch {
      /* same-origin: shouldn't throw, but never let it break the caller */
    }
  }
  if (pending && tries < 20) setTimeout(() => applyTermFont(tries + 1), 250)
}

let lastXtermTheme: Record<string, unknown> | null = null

// applyTermTheme sets xterm.js's theme on every terminal iframe. ttyd 1.7.4
// exposes the Terminal as window.term and the iframes are same-origin (proxied
// under /terminal/ and /shell/), so the parent can reach in. A terminal may not
// be ready when a theme arrives (iframe still loading), so retry a few times.
export function applyTermTheme(
  theme: Record<string, unknown> | null,
  tries = 0
) {
  if (!theme) return
  let pending = false
  for (const el of termFrames()) {
    try {
      const w = el.contentWindow as unknown as {
        term?: { options?: Record<string, unknown> }
      }
      if (w?.term?.options) {
        w.term.options.theme = theme
        continue
      }
    } catch {
      /* same-origin: shouldn't throw, but never let it break the caller */
    }
    pending = true
  }
  if (pending && tries < 20)
    setTimeout(() => applyTermTheme(theme, tries + 1), 250)
}

export function lastTerminalTheme() {
  return lastXtermTheme
}

// reconcileTermTheme re-pins any terminal whose live xterm theme has drifted
// from the cached palette. ttyd rebuilds its xterm with a built-in default
// (light) theme whenever its WebSocket reconnects — idle timeout, laptop
// sleep/wake, a network blip — and that reconnect happens *inside the existing
// iframe document*, so it fires no iframe `load` event for bootTermFrame to
// hook. Without this, a reconnected terminal keeps ttyd's default theme until
// the next herdr theme change or a full page reload, while the React/CSS side
// (whose --h-* vars live on the parent document and persist) stays correctly
// themed — the half-light/half-dark desync. We compare the live background
// against the cached one and only write when they differ, so xterm rebuilds its
// glyph atlas on a real drift, never every tick.
function reconcileTermTheme() {
  if (!lastXtermTheme) return
  const want = (lastXtermTheme as { background?: unknown }).background
  for (const el of termFrames()) {
    try {
      const w = el.contentWindow as unknown as {
        term?: { options?: Record<string, unknown> }
      }
      const opts = w?.term?.options
      if (!opts) continue
      const live = (opts.theme as { background?: unknown } | undefined)
        ?.background
      if (live === want) continue
      opts.theme = lastXtermTheme
    } catch {
      /* same-origin: never let a reconcile break the terminal */
    }
  }
}

let termThemeReconciler: ReturnType<typeof setInterval> | null = null

// startTermThemeReconciler arms a single shared interval that keeps every
// terminal pinned to the latest palette across ttyd reconnects (see
// reconcileTermTheme). Idempotent: the first caller starts it and the rest are
// no-ops, so the per-frame bootTermFrame can call it freely. Runs for the app's
// lifetime — a few DOM reads and a string compare every couple of seconds.
export function startTermThemeReconciler() {
  if (termThemeReconciler) return
  termThemeReconciler = setInterval(reconcileTermTheme, 1500)
}

// refreshTheme pulls herdr's resolved theme and applies it to the *terminals
// only*: it sets the xterm.js palette inside the same-origin ttyd iframes (no
// reconnect) and pins the terminal font. herdr dictates the terminal theme; the
// surrounding chrome is the Nothing design system (its --h-* vars come from
// index.css and follow the system light/dark mode, see lib/mode.ts) — so we
// deliberately no longer repaint the chrome from herdr's palette here.
export async function refreshTheme() {
  let t: ThemePayload
  try {
    t = await api.theme()
  } catch {
    return
  }
  lastXtermTheme = t.xterm
  applyTermTheme(t.xterm, 0)
  applyTermFont(0)
}
