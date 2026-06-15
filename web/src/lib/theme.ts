// The xterm.js palettes for the terminal, tuned to the Nothing design system.
// The base is monochrome — OLED black canvas, white text, a white (not colored)
// cursor and a neutral selection — matching the achromatic chrome. The ANSI
// hue slots stay functional (TUIs, git diff, ls --color rely on them) but are
// mapped to Nothing's restrained status palette: red is the one true signal
// (#d71921), green/yellow the data status hues, and the cool slots
// (blue/cyan/magenta) are desaturated so the terminal reads calm rather than
// neon. Which palette is live follows the app's appearance mode (see
// lib/mode.ts); the browser re-pins it on mode change and across ttyd
// reconnects. (Export names keep the ONYX_* identifier for back-compat with
// importers — only the values changed.)
export const ONYX_XTERM_DARK: Record<string, unknown> = {
  background: "#000000",
  foreground: "#ededed",
  cursor: "#ffffff",
  cursorAccent: "#000000",
  selectionBackground: "rgba(255, 255, 255, 0.20)",
  black: "#1a1a1a",
  red: "#d71921",
  green: "#4a9e5c",
  yellow: "#d4a843",
  blue: "#5b9bf6",
  magenta: "#a78bda",
  cyan: "#6fb7c4",
  white: "#b8b8b8",
  brightBlack: "#555555",
  brightRed: "#ef5b61",
  brightGreen: "#6cba7d",
  brightYellow: "#e3c06a",
  brightBlue: "#82b4ff",
  brightMagenta: "#c0a9e6",
  brightCyan: "#92cbd6",
  brightWhite: "#ffffff",
}

// Light mode — "printed technical manual": black ink on warm off-white paper;
// the bright hues are darkened so they stay legible on a light background.
export const ONYX_XTERM_LIGHT: Record<string, unknown> = {
  background: "#f5f5f5",
  foreground: "#1a1a1a",
  cursor: "#000000",
  cursorAccent: "#f5f5f5",
  selectionBackground: "rgba(0, 0, 0, 0.14)",
  black: "#1a1a1a",
  red: "#c0141b",
  green: "#2e7d46",
  yellow: "#9a7b1f",
  blue: "#007aff",
  magenta: "#7a4fb0",
  cyan: "#2f7e8c",
  white: "#8a8a8a",
  brightBlack: "#666666",
  brightRed: "#d71921",
  brightGreen: "#3a9457",
  brightYellow: "#b48f24",
  brightBlue: "#3392ff",
  brightMagenta: "#9265c4",
  brightCyan: "#3f97a6",
  brightWhite: "#000000",
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
    // The swap changes xterm's cell metrics but not its rows/cols, so a fit
    // computed against the fallback font leaves the bottom rows drawn past the
    // iframe (Claude Code's input box clipped below the terminal on reload).
    // Nudge ttyd's own resize→fit handler so rows/cols are re-derived from the
    // real metrics — again a beat later in case the re-measure was deferred a
    // frame. Skip hidden frames (zero-size fit is garbage); they refit when
    // shown.
    try {
      const win = doc.defaultView
      if (win && win.innerWidth > 0 && win.innerHeight > 0) {
        const refit = () => win.dispatchEvent(new win.Event("resize"))
        refit()
        win.setTimeout(refit, 100)
      }
    } catch {
      /* never break the terminal */
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
