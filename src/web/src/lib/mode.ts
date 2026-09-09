// Appearance mode: the user's chrome theme preference, persisted in localStorage
// (a per-device choice; no backend). "system" follows the OS via
// prefers-color-scheme; "light"/"dark" pin the Nothing palette; "herdr" makes the
// chrome track herdr's own theme (the pre-Nothing behavior — see
// lib/theme.ts:applyHerdrChrome). The resolved value drives the html dark/light
// class, which all the --h-* Nothing tokens / shadcn primitives cascade from (see
// index.css). An inline script in index.html applies the class before first paint
// to avoid a flash; this module keeps it in sync afterwards.
//
// NOTE: unlike the main (tmux) branch, the *terminal* palette is NOT mode-driven
// here — herdr always dictates the terminal theme (see lib/theme.ts). So
// applyMode only touches the chrome class; it deliberately does not re-pin the
// terminals.
export type Mode = "system" | "light" | "dark" | "herdr"

const KEY = "lasso-mode"
// New installs default to "herdr" — the chrome matches herdr's theme out of the
// box; the user opts into the Nothing light/dark palette explicitly.
const DEFAULT_MODE: Mode = "herdr"
const mql = () => window.matchMedia("(prefers-color-scheme: dark)")

export function getMode(): Mode {
  const v = localStorage.getItem(KEY)
  return v === "light" || v === "dark" || v === "system" || v === "herdr"
    ? v
    : DEFAULT_MODE
}

// resolvedMode collapses "system" to the concrete light/dark the OS reports.
// "herdr" resolves to "dark": herdr's palettes are dark-canvas, and the dark
// class keeps the `dark:` tailwind variants behaving as they do in dark mode
// (the herdr --h-* override is layered on top by applyHerdrChrome).
export function resolvedMode(m: Mode = getMode()): "light" | "dark" {
  if (m === "dark" || m === "light") return m
  if (m === "herdr") return "dark"
  return mql().matches ? "dark" : "light"
}

// applyMode sets the html dark/light class. It's the single chokepoint every
// appearance change funnels through — setMode, the on-mount call, and the
// watchSystemMode OS-change handler all land here. The herdr-mode --h-* override
// is applied separately (refreshTheme, which has /api/theme's palette to hand).
export function applyMode(m: Mode = getMode()) {
  const r = resolvedMode(m)
  const el = document.documentElement
  el.classList.toggle("dark", r === "dark")
  el.classList.toggle("light", r === "light")
}

// setMode persists the choice and applies the class immediately. The caller is
// responsible for refreshing the herdr chrome override (refreshTheme) so toggling
// into/out of "herdr" repaints the chrome without waiting for the next theme tick.
export function setMode(m: Mode) {
  localStorage.setItem(KEY, m)
  applyMode(m)
}

// watchSystemMode re-applies on OS theme changes while the user is on "system".
// `onChange` is called after the class flip so the caller can re-derive
// anything that depends on the resolved scheme — the browser-local palette is
// chosen PER SCHEME (see localPaletteName), so an OS flip at dusk changes which
// theme this tab wears and its terminals have to be re-pinned. It is a callback
// rather than a direct refreshTheme() call because lib/theme.ts imports this
// module, not the other way round.
// Idempotent — safe to call once on app mount.
let watching = false
export function watchSystemMode(onChange?: () => void) {
  if (watching) return
  watching = true
  mql().addEventListener("change", () => {
    if (getMode() !== "system") return
    applyMode("system")
    onChange?.()
  })
}

// subscribeSystemScheme / systemPrefersDark are the OS scheme as a store React
// can read (useSyncExternalStore). watchSystemMode above keeps the DOCUMENT in
// sync, which is not enough for a COMPONENT that renders something derived from
// the scheme: the Settings Themes pane shows the backdrop gallery and hints for
// the palette in force, and with nothing subscribed it kept the light theme's
// gallery ("Backdrop for ayu-light") after the OS flipped to dark and the
// document had already re-themed. Separate from watchSystemMode because that
// one is a single app-wide side effect and this one is per subscriber.
export function subscribeSystemScheme(onChange: () => void): () => void {
  const m = mql()
  m.addEventListener("change", onChange)
  return () => m.removeEventListener("change", onChange)
}

export function systemPrefersDark(): boolean {
  return mql().matches
}

// ---------------------------------------------------------------------------
// Browser-local palettes
// ---------------------------------------------------------------------------

// Outside "herdr" mode the chrome is the Nothing palette by default, and the
// terminals wear whatever herdr resolves fleet-wide. A user who wants their own
// look per light/dark can instead name a theme for each scheme: this tab then
// paints its chrome AND its terminals from that theme, resolved through
// /api/theme?name= — a read, so nothing is written to herdr's config.toml and
// no other tab, host or agent follows. That is the whole point: an OS that
// flips to dark at sunset must not re-theme the fleet, which with a global
// switch would oscillate every machine twice a day.
//
// "" (the default, and what every existing install has) means "no local
// palette": the Nothing chrome, exactly as before.
const PALETTE_KEYS: Record<"light" | "dark", string> = {
  light: "lasso-palette-light",
  dark: "lasso-palette-dark",
}

export function getPalettePref(scheme: "light" | "dark"): string {
  return localStorage.getItem(PALETTE_KEYS[scheme]) ?? ""
}

// setPalettePref persists one scheme's theme ("" clears it). Repainting is
// lib/theme.ts's job (refreshTheme), which has to re-resolve the palette.
export function setPalettePref(scheme: "light" | "dark", name: string) {
  if (name) localStorage.setItem(PALETTE_KEYS[scheme], name)
  else localStorage.removeItem(PALETTE_KEYS[scheme])
}

// localPaletteName is the theme THIS BROWSER should resolve for itself, or ""
// to follow herdr's own. In "herdr" mode it is always "" — that mode is the
// explicit "follow the fleet" choice — and otherwise it is the preference for
// the scheme currently in force, so "system" picks up the OS flip for free.
export function localPaletteName(m: Mode = getMode()): string {
  if (m === "herdr") return ""
  return getPalettePref(resolvedMode(m))
}
