import { api, type ThemePayload } from "@/lib/api"
import { getMode } from "@/lib/mode"

// The terminal iframes whose xterm.js theme we keep in sync with herdr's: the
// left herdr terminal and the right shell.
const TERM_FRAME_IDS = ["term", "shellframe"]

function termFrames(): HTMLIFrameElement[] {
  const out: HTMLIFrameElement[] = []
  for (const id of TERM_FRAME_IDS) {
    const el = document.getElementById(id) as HTMLIFrameElement | null
    if (el) out.push(el)
  }
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

// ttyd's own stylesheet reserves a 5px frame around the terminal, and xterm's
// FitAddon then subtracts a scrollbar gutter on top of it — 27px of dead
// background between herdr's right edge and the splitter, which reads as a gap
// in the chrome and costs three columns of terminal.
//
// The gutter is the odd half: xterm's Viewport computes
// `viewportEl.offsetWidth - scrollArea.offsetWidth || 15`, so a scrollbar that
// measures 0 (overlay scrollbars on macOS, or one we hide) falls through the
// `||` to a *phantom* 15px it then reserves anyway. Hiding it in CSS is
// therefore necessary but not sufficient — pinScrollBarWidth below zeroes the
// measurement itself (see reconcileTermFit for why it needs re-pinning).
const TERM_FIT_STYLE_ID = "herdr-term-fit"
const TERM_FIT_CSS = [
  ".terminal{padding:0!important;height:100%!important}",
  ".xterm-viewport{scrollbar-width:none!important}",
  ".xterm-viewport::-webkit-scrollbar{width:0!important;height:0!important}",
].join("")

interface XtermCore {
  viewport?: { scrollBarWidth?: number }
}

// Zero xterm's reserved scrollbar width and refit. Returns true when it actually
// changed something, so callers only pay the resize/reflow on a real drift.
function pinScrollBarWidth(win: Window | null, term: unknown): boolean {
  const vp = (term as { _core?: XtermCore } | undefined)?._core?.viewport
  if (!vp || vp.scrollBarWidth === 0) return false
  vp.scrollBarWidth = 0
  try {
    win?.dispatchEvent(new Event("resize"))
  } catch {
    /* ignore */
  }
  return true
}

// applyTermFit strips ttyd's padding and the scrollbar gutter from every
// terminal iframe so xterm's grid runs edge to edge. Mirrors applyTermFont: the
// <style> goes in as soon as the document exists, and we retry while an iframe
// is still (re)connecting so a not-yet-built xterm still gets pinned.
export function applyTermFit(tries = 0) {
  let pending = false
  for (const el of termFrames()) {
    try {
      const doc = el.contentDocument
      if (!doc?.head) {
        pending = true
        continue
      }
      if (!doc.getElementById(TERM_FIT_STYLE_ID)) {
        const style = doc.createElement("style")
        style.id = TERM_FIT_STYLE_ID
        style.textContent = TERM_FIT_CSS
        doc.head.appendChild(style)
      }
      const w = el.contentWindow as unknown as { term?: unknown }
      if (!w?.term) {
        pending = true
        continue
      }
      pinScrollBarWidth(el.contentWindow, w.term)
    } catch {
      /* same-origin: never let a fit tweak break the terminal */
    }
  }
  if (pending && tries < 20) setTimeout(() => applyTermFit(tries + 1), 250)
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
      return
    }
    // Switching to the real font makes xterm remeasure its cell against the
    // (taller) Nerd Font metrics, but it does NOT re-fit the row count: the rows
    // ttyd computed against the startup fallback font are now too many for the
    // container, so on a cold load the bottom row(s) render below the viewport
    // until a reload (where the cached font is measured up front). ttyd's
    // FitAddon refits on window resize, so nudge it to recompute rows for the new
    // metrics — once now, once on the next frame in case the remeasure trails the
    // option write.
    const win = doc.defaultView
    if (!win) return
    try {
      win.dispatchEvent(new Event("resize"))
      win.requestAnimationFrame(() => {
        try {
          win.dispatchEvent(new Event("resize"))
        } catch {
          /* ignore */
        }
      })
    } catch {
      /* ignore */
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
// retries while an iframe is still (re)connecting. Each fresh xterm lives in a
// fresh iframe document, so the per-document guard re-arms on ttyd reconnects.
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

// A reconnect rebuilds the Viewport too, and its constructor re-derives the
// phantom 15px (see TERM_FIT_CSS) — the injected <style> survives, since the
// document never reloads, but the measurement does not. Re-pin it on the same
// tick as the theme, writing only on a real drift so the refit isn't paid every
// 1.5s.
function reconcileTermFit() {
  for (const el of termFrames()) {
    try {
      const w = el.contentWindow as unknown as { term?: unknown }
      if (w?.term) pinScrollBarWidth(el.contentWindow, w.term)
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
  termThemeReconciler = setInterval(() => {
    reconcileTermTheme()
    reconcileTermFit()
  }, 1500)
}

// The <style> that overrides the chrome's --h-* vars with herdr's palette while
// the user is in "herdr" appearance mode (see lib/mode.ts). Appended after the
// bundled stylesheet so its plain :root{} rule wins by source order; absent in
// every other mode, where the chrome is the static Nothing palette.
const HERDR_CHROME_STYLE_ID = "lasso-herdr-chrome"

// applyHerdrChrome paints the chrome from herdr's resolved palette, reproducing
// the pre-Nothing behavior where the whole UI tracked herdr's theme. `css` is
// /api/theme's bare "--bg: …;" declaration block; we prefix each property to
// "--h-" to match the token names index.css cascades from (the same mapping the
// Go server's cssVarsRoot() once injected). Idempotent — reuses the style node.
function applyHerdrChrome(css: string) {
  let el = document.getElementById(
    HERDR_CHROME_STYLE_ID
  ) as HTMLStyleElement | null
  if (!el) {
    el = document.createElement("style")
    el.id = HERDR_CHROME_STYLE_ID
    document.head.appendChild(el)
  }
  el.textContent = `:root{${css.replaceAll("--", "--h-")}}`
}

// clearHerdrChrome removes the override so the chrome falls back to the static
// Nothing palette (used by every non-"herdr" appearance mode).
function clearHerdrChrome() {
  document.getElementById(HERDR_CHROME_STYLE_ID)?.remove()
}

// refreshTheme pulls herdr's resolved theme and applies it. The terminals always
// track it: it sets the xterm.js palette inside the same-origin ttyd iframes (no
// reconnect) and pins the terminal font. The surrounding chrome is the Nothing
// design system by default (its --h-* vars come from index.css and follow the
// light/dark mode) — EXCEPT in "herdr" appearance mode, where we also repaint the
// chrome from herdr's palette so the whole UI matches the terminal.
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
  if (getMode() === "herdr") applyHerdrChrome(t.css)
  else clearHerdrChrome()
}
