import { api, type ThemeCatalogEntry, type ThemePayload } from "@/lib/api"
import { ensureContrast } from "@/lib/contrast"
import { getMode, localPaletteName } from "@/lib/mode"
import {
  backgroundFor,
  getScrim,
  getShading,
  type ShippedBackground,
} from "@/lib/wallpaper"

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

// ---------------------------------------------------------------------------
// Atmosphere: the backdrop a theme wears
// ---------------------------------------------------------------------------

// Any theme can carry a backdrop, and the backdrop is a real image: one of the
// 27 stills lasso bundles for retro-82 (served from the embedded build, so it
// never reaches the network), one that came with an installed Omarchy theme
// (served by lasso from its clone), or one this browser was handed by URL or
// upload. WHICH — and whether a theme wears one at all — is a per-browser,
// per-theme choice (lib/wallpaper.ts, the Settings gallery), so none of it can
// be authored in CSS: applyAtmosphere pins the base color and the whole
// background-image stack as custom properties on <html>, and both the chrome
// (index.css) and each ttyd document read them from there.
//
// The theme it all keys off is the one THIS BROWSER wears — herdr's resolved
// theme, or the browser-local palette when one is chosen (see
// lib/mode.ts:localPaletteName) — never the raw configured name, so herdr's
// alternate spellings and a [theme.custom]-tweaked palette land on the same
// entry.
const TERM_ATMOSPHERE_STYLE_ID = "herdr-term-atmosphere"
// The canvas color under the backdrop, and the background-image stack painted
// on it (scrim over shading over image; "none" when the theme is flat).
const ATMOSPHERE_BASE_VAR = "--atmo-base"
const ATMOSPHERE_LAYERS_VAR = "--atmo-layers"

// Whether this browser's theme has a backdrop at all — an image, palette
// shading, or both. Every consumer reads it (applyTermTheme pins xterm's
// allowTransparency off it, the reconciler re-pins after a ttyd reconnect), so
// it lives beside the cached palette instead of being threaded through each
// call.
let atmosphereOn = false

// The palette this browser is wearing, kept so a backdrop change (a still, the
// scrim slider, the shading toggle) can rebuild the atmosphere from the colors
// it derives from without re-fetching the theme.
let lastPalette: ThemePayload | null = null

// The palette every terminal is pinned to — the theme's own xterm ITheme, with
// its background made transparent while a backdrop is on. Read live by
// applyTermTheme and its reconciler rather than passed to them, so a retry
// queued before a theme switch cannot land the palette it was queued under.
let lastXtermTheme: Record<string, unknown> | null = null

// The theme name the backdrop is chosen for: /api/theme's RESOLVED name, or the
// browser-local palette's. Exported because the Settings gallery has to show
// the backgrounds of the theme actually on screen, which under a local palette
// is not the one herdr is configured with.
let effectiveTheme = ""
export function effectiveThemeName(): string {
  return effectiveTheme
}

// The theme catalog, for the backgrounds a theme shipped with. Cached for the
// page's life — it only changes when a theme is installed or removed, and both
// go through invalidateThemeCatalog — and a FAILURE is cached as an empty list
// for the same reason: a server that doesn't serve it (an older build) must not
// be asked again on every re-theme. In-flight requests are shared so a boot
// that re-themes twice makes one call.
let catalog: ThemeCatalogEntry[] | null = null
let catalogFetch: Promise<ThemeCatalogEntry[]> | null = null

async function loadCatalog(): Promise<ThemeCatalogEntry[]> {
  if (catalog) return catalog
  if (!catalogFetch) {
    catalogFetch = api
      .themeCatalog()
      .then((c) => c.themes ?? [])
      .catch(() => [])
      .then((themes) => {
        catalog = themes
        catalogFetch = null
        return themes
      })
  }
  return catalogFetch
}

// invalidateThemeCatalog drops the cache after an install or an uninstall, so
// the next resolve sees the new theme's backgrounds.
export function invalidateThemeCatalog() {
  catalog = null
}

// The backgrounds the effective theme shipped with, as url/thumb pairs (the
// catalog serves the two arrays positionally). Deliberately tolerant of a miss:
// a not-yet-loaded catalog paints the bundled/custom half now and gets a second
// pass when it lands (refreshTheme), rather than holding the whole repaint on a
// request.
function shippedBackgrounds(): ShippedBackground[] {
  const entry = catalog?.find((c) => c.name === effectiveTheme)
  if (!entry) return []
  return shippedPairs(entry)
}

// shippedPairs zips a catalog entry's two parallel arrays. Exported because the
// Settings gallery needs the same pairs for the theme it is showing, and one
// zip is better than two conventions for the same positional contract.
export function shippedPairs(entry: ThemeCatalogEntry): ShippedBackground[] {
  return entry.backgrounds.map((url, i) => ({
    url,
    thumb: entry.thumbs?.[i] ?? url,
  }))
}

// hexRGBA re-spells a #rrggbb as an rgba() at the given alpha. Returns "" for
// anything else — a palette is free to hand us a color spelling we don't parse,
// and a wash we can't compute is better skipped than guessed.
function hexRGBA(hex: string, alpha: number): string {
  if (!/^#[0-9a-f]{6}$/i.test(hex)) return ""
  const n = Number.parseInt(hex.slice(1), 16)
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`
}

// paletteColor reads one xterm ITheme entry as a color string. The payload's
// shape is opaque to us (we hand it straight to the iframe), so every read is
// guarded rather than typed.
function paletteColor(key: string): string {
  const v = lastPalette?.xterm?.[key]
  return typeof v === "string" ? v : ""
}

// The canvas color: the theme's own terminal background, so the chrome, the
// terminal and the gaps between them are one surface.
function atmosphereBase(): string {
  return paletteColor("background") || "#000000"
}

// shadeLayers is the optional palette-derived shading: two very low-alpha
// washes of the theme's own accent colors, one from the top-left and one from
// the bottom-right, over the flat canvas. It is what gives a theme with no
// image some depth — the alphas are deliberately at the edge of visible, since
// the point is a lit canvas, not a gradient someone has to read text off.
function shadeLayers(): string[] {
  const a = hexRGBA(paletteColor("blue") || paletteColor("cyan"), 0.16)
  const b = hexRGBA(paletteColor("magenta") || paletteColor("green"), 0.12)
  const out: string[] = []
  if (a)
    out.push(`radial-gradient(120% 90% at 8% 0%, ${a} 0%, transparent 60%)`)
  if (b)
    out.push(`radial-gradient(110% 90% at 100% 100%, ${b} 0%, transparent 62%)`)
  return out
}

// The xterm background pinned under the atmosphere: the theme's own background
// with a zero alpha, so a cell no program has colored paints nothing and the
// CSS backdrop below reaches the screen. Only the DEFAULT background can be
// made transparent this way — a cell carrying its own SGR background (herdr's
// own chrome, a selected row in a TUI) is opaque by definition and stays that
// way.
//
// Spelling it as the palette's own background — rather than any transparent
// color — is what makes the backdrop visible inside herdr's PANES rather than
// only at its edges. xterm answers a program's OSC 11 background query from
// this color with the alpha dropped, herdr records that as the host terminal's
// background, and its pane renderer then emits no explicit background for a
// cell whose default matches the host's (src/pane/terminal.rs:ghostty_default_bg
// returns None → Color::Reset) — i.e. the pane's own default cells stay xterm's
// default background, which is the transparent one. A different RGB here would
// make herdr paint every pane cell explicitly and the backdrop would never
// show. The same reply is how omp and opencode infer light-vs-dark, so it must
// keep the theme's own lightness. A palette whose background isn't a plain
// #rrggbb gets no transparency at all (nothing to append an alpha to), which
// degrades to a normally-painted terminal rather than a broken one.
function transparentTermBG(): string {
  const bg = atmosphereBase()
  return /^#[0-9a-f]{6}$/i.test(bg) ? `${bg}00` : ""
}

// termAtmosphereCSS builds the stylesheet injected into a ttyd document: the
// canvas color and the whole backdrop stack on <html>, and everything ttyd and
// xterm paint between it and the glyphs turned transparent so the backdrop
// reaches the screen. The two declarations are read from the parent document's
// own custom properties — the pin applyAtmosphere writes — so there is one
// definition across two documents, same as the @font-face applyTermFont mirrors
// across the same boundary. Every URL in them is root-relative (or absolute),
// which is what makes the same string resolve inside a document served from
// /terminal/<slug>/.
//
// The scrim inside that stack is the wash xterm no longer paints (see
// transparentTermBG). Doing it in CSS rather than as a translucent xterm
// background keeps the tint identical under all three of ttyd's renderers,
// instead of depending on how each composites a half-transparent theme color.
// ttyd's own <body> rule is neutralized rather than used for the wash: with the
// scrim now a layer of the stack, a second painted surface on top of it would
// double the tint. `.xterm-viewport` needs the !important: xterm writes the
// theme background onto it as an inline style on every theme change, and an
// author !important is what outranks that.
function termAtmosphereCSS(): string {
  const cs = getComputedStyle(document.documentElement)
  return [
    `html{background-color:${cs.getPropertyValue(ATMOSPHERE_BASE_VAR).trim()}!important;`,
    `background-image:${cs.getPropertyValue(ATMOSPHERE_LAYERS_VAR).trim()}!important;`,
    "background-attachment:fixed!important;background-repeat:no-repeat!important;",
    "background-size:cover!important}",
    "body{background:transparent!important}",
    "#terminal-container,.terminal,.xterm,.xterm-screen,.xterm-viewport",
    "{background-color:transparent!important}",
  ].join("")
}

// applyTermAtmosphere injects — or, under a flat theme, removes — the backdrop
// stylesheet in every terminal iframe. Mirrors applyTermFont: the <style> goes
// in as soon as the document exists, and we retry while an iframe is still
// (re)connecting so a frame that arrives late still gets it. Removal is what
// restores a theme with no backdrop: the ttyd document is otherwise untouched.
export function applyTermAtmosphere(tries = 0) {
  // Built once per pass rather than per frame: it reads the parent's computed
  // style, and both frames get the identical sheet.
  const css = atmosphereOn ? termAtmosphereCSS() : ""
  let pending = false
  for (const el of termFrames()) {
    try {
      const doc = el.contentDocument
      if (!doc?.head) {
        pending = true
        continue
      }
      const have = doc.getElementById(TERM_ATMOSPHERE_STYLE_ID)
      if (!atmosphereOn) {
        have?.remove()
        continue
      }
      if (have) {
        // Picking a different still has to reach an ALREADY-injected sheet, so
        // presence alone is not enough to skip on — but only a real difference
        // is written: restating identical text on every reconcile would have
        // ttyd's document recompute style for nothing.
        if (have.textContent !== css) have.textContent = css
        continue
      }
      const style = doc.createElement("style")
      style.id = TERM_ATMOSPHERE_STYLE_ID
      style.textContent = css
      doc.head.appendChild(style)
    } catch {
      /* same-origin: never let the backdrop break the terminal */
    }
  }
  // Only the injecting direction is worth retrying: a document that has not
  // loaded yet cannot be holding a stylesheet to remove.
  if (atmosphereOn && pending && tries < 20)
    setTimeout(() => applyTermAtmosphere(tries + 1), 250)
}

// chromeFollowsPalette reports whether the surrounding UI is painted from a
// herdr/Omarchy palette at all — "herdr" appearance mode, or a browser-local
// palette chosen for the current scheme. It gates both the --h-* override and
// the chrome's half of the backdrop: the Nothing light/dark canvases are flat
// monochrome by design, and a photograph behind them is not that design.
function chromeFollowsPalette(): boolean {
  return getMode() === "herdr" || localPaletteName() !== ""
}

// applyAtmosphere is the one chokepoint for the backdrop. It pins the canvas
// color and the background-image stack on <html> — where index.css reads them
// for the chrome and termAtmosphereCSS mirrors them into each ttyd document —
// re-derives whether the terminal has to be transparent, and pushes both out.
// refreshTheme calls it (so a reload, a re-theme and an appearance change all
// land here) and so does every Settings control that can change a backdrop,
// which is what makes a pick repaint the chrome and an already-loaded terminal
// iframe at once instead of waiting for a remount.
//
// The properties are pinned unconditionally, flat themes included: they cost
// two custom properties nothing else reads, and it means the backdrop is
// already correct at the instant data-atmosphere goes back on.
//
// Before the first palette has resolved there is nothing to derive a canvas
// color from, so it does nothing at all: a Settings pick made while /api/theme
// is still in flight (or failing) has already been persisted, and painting a
// black canvas and an imageless stack in the meantime would be a flash of a
// theme nobody chose. refreshTheme repaints when the palette lands.
export function applyAtmosphere() {
  if (!lastPalette) return
  const image = backgroundFor(effectiveTheme, shippedBackgrounds())
  const shade = getShading() ? shadeLayers() : []
  const base = atmosphereBase()
  // Outermost first, as CSS paints them: the scrim washes the image AND the
  // shading below it, so a photograph is never read through less wash than the
  // slider asks for. With no image there is nothing to wash — the shading is
  // already at the edge of visible — so the scrim is left out entirely.
  const layers: string[] = []
  if (image) {
    const scrim = hexRGBA(base, getScrim())
    if (scrim) layers.push(`linear-gradient(${scrim}, ${scrim})`)
  }
  layers.push(...shade)
  if (image) layers.push(`url("${image}")`)
  atmosphereOn = layers.length > 0
  const root = document.documentElement
  root.style.setProperty(ATMOSPHERE_BASE_VAR, base)
  root.style.setProperty(
    ATMOSPHERE_LAYERS_VAR,
    layers.length ? layers.join(", ") : "none"
  )
  if (atmosphereOn && chromeFollowsPalette())
    root.setAttribute("data-atmosphere", effectiveTheme || "on")
  else root.removeAttribute("data-atmosphere")
  // The terminal's palette carries the transparency the backdrop needs, so a
  // backdrop change re-pins it: turning an image off has to put the opaque
  // background back, or the terminal keeps showing the chrome behind it. A
  // palette whose background we can't spell transparently keeps its own.
  const transparent = atmosphereOn ? transparentTermBG() : ""
  lastXtermTheme = !lastPalette
    ? null
    : transparent
      ? { ...lastPalette.xterm, background: transparent }
      : lastPalette.xterm
  applyTermTheme(0)
  applyTermAtmosphere(0)
}

// applyTermTheme pins every terminal iframe to the CACHED palette. ttyd 1.7.4
// exposes the Terminal as window.term and the iframes are same-origin (proxied
// under /terminal/ and /shell/), so the parent can reach in. A terminal may not
// be ready when a theme arrives (iframe still loading), so retry a few times.
//
// It deliberately takes no theme argument, and each retry re-reads the module
// cache: a snapshot captured before a `setTimeout` outlives the palette it came
// from. Switching a backdrop theme → a flat one while an iframe is still
// connecting would otherwise land the queued palette — its background
// transparent — under the flat theme's atmosphereOn=false pin a quarter second
// later, i.e. a terminal of blank tiles until the next reconcile.
export function applyTermTheme(tries = 0) {
  const theme = lastXtermTheme
  if (!theme) return
  let pending = false
  for (const el of termFrames()) {
    try {
      const w = el.contentWindow as unknown as {
        term?: { options?: Record<string, unknown> }
      }
      if (w?.term?.options) {
        // allowTransparency has to be true for the glyph atlas to leave a
        // default-bg cell transparent (xterm bakes the background behind each
        // glyph into the atlas otherwise, and every cell becomes an opaque
        // tile). xterm rebuilds the atlas when either option changes, and
        // writing an unchanged boolean is a no-op in its option setter, so
        // pinning it here — the one place boot, re-theme and the post-reconnect
        // reconcile all funnel through — costs nothing under a flat theme.
        w.term.options.allowTransparency = atmosphereOn
        w.term.options.theme = theme
        continue
      }
    } catch {
      /* same-origin: shouldn't throw, but never let it break the caller */
    }
    pending = true
  }
  if (pending && tries < 20) setTimeout(() => applyTermTheme(tries + 1), 250)
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
// against the cached one, and allowTransparency against the flag it is pinned
// from, and only write on a real drift so xterm rebuilds its glyph atlas when
// something actually moved rather than every tick. Both are compared because
// either alone can drift: ttyd's reconnect re-applies its own theme (and the
// `?theme=` query), while the boolean survives a reconnect but not a terminal
// this tab has never pinned — and a transparent background under an atlas built
// for opaque cells is a terminal of blank tiles.
function reconcileTermTheme() {
  if (!lastXtermTheme) return
  const want = lastXtermTheme.background
  for (const el of termFrames()) {
    try {
      const w = el.contentWindow as unknown as {
        term?: { options?: Record<string, unknown> }
      }
      const opts = w?.term?.options
      if (!opts) continue
      const liveTheme = opts.theme
      const live =
        liveTheme && typeof liveTheme === "object" && "background" in liveTheme
          ? liveTheme.background
          : undefined
      if (live === want && opts.allowTransparency === atmosphereOn) continue
      opts.allowTransparency = atmosphereOn
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

// The <style> that overrides the chrome's --h-* vars with the palette this
// browser wears — herdr's own in "herdr" appearance mode, or the theme chosen
// for the current light/dark scheme (see lib/mode.ts:localPaletteName). Absent
// otherwise, where the chrome is the static Nothing palette.
//
// It is appended after the bundled stylesheet, and its selector is doubled
// (`:root:root`) rather than a plain `:root`: index.css declares the light
// tokens on `:root.light`, which outranks a single `:root` no matter where it
// sits in the cascade. Under the old herdr-only behavior that never showed —
// "herdr" mode resolves to the dark class — but a browser-local LIGHT palette
// would have been silently ignored.
const HERDR_CHROME_STYLE_ID = "lasso-herdr-chrome"

// The tokens index.css cascades into TEXT, with the contrast each one has to
// reach against the surface it sits on. A theme's palette is written for a
// terminal, where "muted" is the dim ANSI gray a prompt uses — not a promise
// that secondary text is readable on this canvas; ayu-light ships `--muted:
// #d1d1d1` on `--bg: #f8f9fa` (1.2:1), which made every label, hint and caption
// in Settings invisible, and its `--warn` is 1.9:1 there. Since a theme is now
// anything a user installed from a git URL, the chrome checks rather than
// trusts (see lib/contrast.ts).
//
// --fg is body text and gets WCAG AA; --muted is secondary and gets a lower bar
// so the brightness hierarchy survives the repair; the status hues and --accent
// are small text, icons and button fills and get AA-large. Surfaces
// (--panel/--border/--hover) and --accent-dim are deliberately not touched:
// they are backgrounds, and "repairing" one would flatten the very step the
// design separates surfaces by.
const TEXT_TOKEN_CONTRAST: Record<string, number> = {
  "--fg": 4.5,
  "--muted": 3.2,
  "--accent": 3,
  "--dir": 3,
  "--good": 3,
  "--warn": 3,
  "--bad": 3,
  "--link": 3,
}

// legiblePalette rewrites a declaration block's text colors to ones that can
// actually be read on that palette's own surfaces. Each is checked against the
// canvas AND the panel (cards, the sidebar, popovers — a token failing on
// either is unreadable somewhere), so the stricter of the two wins. A color
// that already passes, or one we cannot parse, is left exactly as the theme
// wrote it, which is why every dark palette lasso ships — retro-82 included —
// comes through byte for byte.
function legiblePalette(css: string): string {
  const decls = css
    .split(";")
    .map((d) => d.trim())
    .filter(Boolean)
    .map((d) => {
      const i = d.indexOf(":")
      return [d.slice(0, i).trim(), d.slice(i + 1).trim()] as const
    })
    .filter(([name]) => name.startsWith("--"))
  const value = (name: string) => decls.find(([n]) => n === name)?.[1] ?? ""
  const bg = value("--bg")
  const panel = value("--panel") || bg
  if (!bg) return css
  return decls
    .map(([name, raw]) => {
      const target = TEXT_TOKEN_CONTRAST[name]
      if (!target) return `${name}: ${raw};`
      const onBG = ensureContrast(raw, bg, target)
      return `${name}: ${ensureContrast(onBG, panel, target)};`
    })
    .join("")
}

// applyHerdrChrome paints the chrome from the effective palette, reproducing
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
  el.textContent = `:root:root{${legiblePalette(css).replaceAll("--", "--h-")}}`
}

// clearHerdrChrome removes the override so the chrome falls back to the static
// Nothing palette (used whenever no palette is in force — see
// chromeFollowsPalette).
function clearHerdrChrome() {
  document.getElementById(HERDR_CHROME_STYLE_ID)?.remove()
}

// refreshTheme resolves the palette this browser should wear and applies it.
// The terminals always track it: it sets the xterm.js palette inside the
// same-origin ttyd iframes (no reconnect) and pins the terminal font. The
// surrounding chrome is the Nothing design system by default (its --h-* vars
// come from index.css and follow the light/dark mode) — except while a palette
// is in force (see chromeFollowsPalette), where the chrome is repainted from it
// so the whole UI matches the terminal.
//
// WHICH palette is the one question this answers. By default it is herdr's own
// resolved theme, shared by every tab and every host. But a browser may name a
// preferred theme per light/dark scheme, and then it resolves THAT one through
// /api/theme?name= — a read: nothing is written to herdr's config.toml, so no
// other tab, host or agent follows, and an OS that flips at dusk re-themes this
// window instead of oscillating the fleet twice a day. A preview that fails
// (an uninstalled theme, an older server) falls back to herdr's, so a stale
// preference degrades to the shared look rather than to no theme at all.
//
// A theme may additionally carry a backdrop (see the atmosphere section above).
// The terminal wears it whenever the effective theme has one; the chrome only
// while it is following a palette, since the Nothing light/dark canvases are
// flat by design. Everything is re-derived on every call, so switching theme,
// backdrop or appearance mode puts the rest back exactly as it was.
export async function refreshTheme() {
  const local = localPaletteName()
  let t: ThemePayload | null = null
  if (local) t = await api.theme(local).catch(() => null)
  if (!t) t = await api.theme().catch(() => null)
  if (!t) return
  lastPalette = t
  effectiveTheme = t.resolved
  applyAtmosphere()
  applyTermFont(0)
  if (chromeFollowsPalette()) applyHerdrChrome(t.css)
  else clearHerdrChrome()
  // The catalog only matters for the backgrounds a theme SHIPPED with, so the
  // repaint above never waits on it: the bundled and hand-given halves are
  // already resolved, and a first load (or a fresh install) gets a second pass
  // once the list lands. Cached after that, so a re-theme costs nothing.
  if (!catalog) {
    const themes = await loadCatalog()
    // The theme may have changed while that was in flight; only the pass whose
    // theme is still current may repaint.
    if (themes.length && effectiveTheme === t.resolved) applyAtmosphere()
  }
}
