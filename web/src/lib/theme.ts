// The xterm.js palettes for the terminal, derived from the Onyx design tokens
// (onyx preset.json / colors_and_type.css). Both run indigo-forward to match
// Onyx's cool, achromatic-plus-indigo look — the cool slots (blue+cyan+magenta)
// collapse into the indigo accent family, while green/yellow/red stay as the
// ok/warn/danger tokens so they remain functional status colors (git diff, ls).
// Which one is live follows the app's appearance mode (see lib/mode.ts); the
// browser re-pins it on mode change and across ttyd reconnects.
export const ONYX_XTERM_DARK: Record<string, unknown> = {
  background: "#06070c",
  foreground: "#f4f5fa",
  cursor: "#7b7fff",
  cursorAccent: "#06070c",
  selectionBackground: "rgba(123, 127, 255, 0.30)",
  black: "#11131c",
  red: "#f2545b",
  green: "#4ade9a",
  yellow: "#f2b144",
  blue: "#7b7fff",
  magenta: "#9498ff",
  cyan: "#9498ff",
  white: "#b7bbc8",
  brightBlack: "#3b3f4e",
  brightRed: "#f57b80",
  brightGreen: "#74e6a8",
  brightYellow: "#f6c674",
  brightBlue: "#9498ff",
  brightMagenta: "#b3a4ff",
  brightCyan: "#b3a4ff",
  brightWhite: "#f4f5fa",
}

// Light mode: dark text on Onyx's light canvas; the bright pastels are darkened
// so they stay legible on a light background.
export const ONYX_XTERM_LIGHT: Record<string, unknown> = {
  background: "#f4f5fa",
  foreground: "#1a1c23",
  cursor: "#5c61e6",
  cursorAccent: "#f4f5fa",
  selectionBackground: "rgba(92, 97, 230, 0.20)",
  black: "#1a1c23",
  red: "#c83a41",
  green: "#1a9259",
  yellow: "#a8780f",
  blue: "#5c61e6",
  magenta: "#5c61e6",
  cyan: "#5c61e6",
  white: "#8a8f9c",
  brightBlack: "#6b7080",
  brightRed: "#e0545b",
  brightGreen: "#28a86a",
  brightYellow: "#c9941f",
  brightBlue: "#7b7fff",
  brightMagenta: "#7b7fff",
  brightCyan: "#7b7fff",
  brightWhite: "#1a1c23",
}

// Fixed terminal iframes (the standalone terminal + shell). The per-tab
// terminals are matched by class instead — see termFrames.
const TERM_FRAME_IDS = ["term", "shellframe"]

// TERM_FRAME_CLASS marks the per-tab terminal iframes (TabTerminal renders the
// ttyd iframe with this class; its id is `tabterm-<tabId>`). Matching by class
// is what keeps the live terminal in sync with theme changes — without it the
// terminal stays on ttyd's default palette while the UI repaints.
const TERM_FRAME_CLASS = "frame"

// GRID_FRAME_CLASS marks each Grid cell's terminal iframe so it's re-themed
// alongside the main viewport on a mode change / ttyd reconnect.
export const GRID_FRAME_CLASS = "gridterm"

// termFrames collects every terminal iframe the theme should track: the fixed
// ones by id, plus every per-tab and grid terminal by class. Deduped, since a
// frame could in principle match more than one selector.
function termFrames(): HTMLIFrameElement[] {
  const seen = new Set<HTMLIFrameElement>()
  for (const id of TERM_FRAME_IDS) {
    const el = document.getElementById(id) as HTMLIFrameElement | null
    if (el) seen.add(el)
  }
  for (const cls of [TERM_FRAME_CLASS, GRID_FRAME_CLASS]) {
    for (const el of Array.from(
      document.querySelectorAll<HTMLIFrameElement>(`iframe.${cls}`)
    ))
      seen.add(el)
  }
  return Array.from(seen)
}

// The terminal font stack. JetBrainsMono Nerd Font carries the icon glyphs that
// TUIs (gh-dash, btop, …) draw with — without it xterm renders "tofu" boxes.
// The face is vendored as woff2 under web/public/fonts and served at /fonts/*.
const TERM_FONT_FAMILY = "JetBrainsMono Nerd Font"
const TERM_FONT_STACK = `"${TERM_FONT_FAMILY}", ui-monospace, monospace`
const TERM_FONT_STYLE_ID = "lasso-term-font"

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
  __fontWired?: boolean
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
// points xterm at it. Mirrors applyTermTheme: iterates the same frames, and
// retries while an iframe is still (re)connecting. Each fresh
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
      if (doc.__fontWired) continue
      doc.__fontWired = true
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
// the next theme change or a full page reload, while the React/CSS side
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

// applyTermThemeForMode pins every terminal to the Onyx palette for the given
// resolved appearance ("light" | "dark") plus the Nerd Font. The UI/CSS side is
// driven by the html dark/light class (see lib/mode.ts); this drives the live
// terminals — it sets the xterm.js theme inside the same-origin ttyd iframes (no
// reconnect) and injects the terminal font. Called on mount and whenever the mode
// changes; the reconciler (startTermThemeReconciler) re-pins across ttyd
// reconnects.
export function applyTermThemeForMode(resolved: "light" | "dark") {
  const theme = resolved === "light" ? ONYX_XTERM_LIGHT : ONYX_XTERM_DARK
  lastXtermTheme = theme
  applyTermTheme(theme, 0)
  applyTermFont(0)
}
