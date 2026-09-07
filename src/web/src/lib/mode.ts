// Appearance mode: the user's chrome theme preference, persisted in localStorage
// (a per-device choice; no backend). "system" follows the OS via
// prefers-color-scheme; "light"/"dark" pin the Nothing palette; "luvus" makes the
// chrome track Luvus's own theme (the pre-Nothing behavior — see
// lib/theme.ts:applyLuvusChrome). The resolved value drives the html dark/light
// class, which all the --h-* Nothing tokens / shadcn primitives cascade from (see
// index.css). An inline script in index.html applies the class before first paint
// to avoid a flash; this module keeps it in sync afterwards.
//
// NOTE: unlike the main (tmux) branch, the *terminal* palette is NOT mode-driven
// here — Luvus always dictates the terminal theme (see lib/theme.ts). So
// applyMode only touches the chrome class; it deliberately does not re-pin the
// terminals.
export type Mode = "system" | "light" | "dark" | "luvus"

const KEY = "lasso-mode"
// Luvus's own polarity for the active theme, remembered per device so the
// pre-paint script in index.html can pick the right class before /api/theme
// lands (see APPEARANCE_KEY there). Luvus ships light palettes as well as dark
// ones, so "luvus" mode cannot assume a dark canvas: the html class has to match
// the palette the override paints, or the `dark:` variants and every shadcn
// token cascading off it fight the colors on screen.
const APPEARANCE_KEY = "lasso-luvus-appearance"
// New installs default to "luvus" — the chrome matches Luvus's theme out of the
// box; the user opts into the Nothing light/dark palette explicitly.
const DEFAULT_MODE: Mode = "luvus"
const mql = () => window.matchMedia("(prefers-color-scheme: dark)")

export function getMode(): Mode {
  const v = localStorage.getItem(KEY)
  return v === "light" || v === "dark" || v === "system" || v === "luvus"
    ? v
    : DEFAULT_MODE
}

// luvusAppearance reads the remembered polarity of the active Luvus theme.
// Unknown until the first /api/theme answer, and Luvus's default palettes are
// dark, so "dark" is the assumption that flashes least.
function luvusAppearance(): "light" | "dark" {
  return localStorage.getItem(APPEARANCE_KEY) === "light" ? "light" : "dark"
}

// setLuvusAppearance records what Luvus reports for the active theme
// ("terminal" palettes take their canvas from the terminal, so they follow the
// dark chrome) and re-applies the class when the polarity actually moved.
export function setLuvusAppearance(appearance: string) {
  const next = appearance === "light" ? "light" : "dark"
  if (localStorage.getItem(APPEARANCE_KEY) === next) return
  localStorage.setItem(APPEARANCE_KEY, next)
  applyMode()
}

// resolvedMode collapses "system" to the concrete light/dark the OS reports and
// "luvus" to the polarity of the theme Luvus currently has active.
export function resolvedMode(m: Mode = getMode()): "light" | "dark" {
  if (m === "dark" || m === "light") return m
  if (m === "luvus") return luvusAppearance()
  return mql().matches ? "dark" : "light"
}

// applyMode sets the html dark/light class. It's the single chokepoint every
// appearance change funnels through — setMode, the on-mount call, and the
// watchSystemMode OS-change handler all land here. The Luvus-mode --h-* override
// is applied separately (refreshTheme, which has /api/theme's palette to hand).
export function applyMode(m: Mode = getMode()) {
  const r = resolvedMode(m)
  const el = document.documentElement
  el.classList.toggle("dark", r === "dark")
  el.classList.toggle("light", r === "light")
}

// setMode persists the choice and applies the class immediately. The caller is
// responsible for refreshing the Luvus chrome override (refreshTheme) so toggling
// into/out of "luvus" repaints the chrome without waiting for the next theme tick.
export function setMode(m: Mode) {
  localStorage.setItem(KEY, m)
  applyMode(m)
}

// watchSystemMode re-applies on OS theme changes while the user is on "system".
// Idempotent — safe to call once on app mount.
let watching = false
export function watchSystemMode() {
  if (watching) return
  watching = true
  mql().addEventListener("change", () => {
    if (getMode() === "system") applyMode("system")
  })
}
