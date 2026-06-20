// Appearance mode: the user's chrome light/dark preference, persisted in
// localStorage (a per-device choice; no backend). "system" follows the OS via
// prefers-color-scheme. The resolved value drives the html dark/light class,
// which all the --h-* Nothing tokens / shadcn primitives cascade from (see
// index.css). An inline script in index.html applies the class before first
// paint to avoid a flash; this module keeps it in sync afterwards.
//
// NOTE: unlike the main (tmux) branch, the *terminal* palette is NOT mode-driven
// here — herdr dictates the terminal theme (see lib/theme.ts). So applyMode only
// touches the chrome class; it deliberately does not re-pin the terminals.
export type Mode = "system" | "light" | "dark"

const KEY = "lasso-mode"
const mql = () => window.matchMedia("(prefers-color-scheme: dark)")

export function getMode(): Mode {
  const v = localStorage.getItem(KEY)
  return v === "light" || v === "dark" ? v : "system"
}

// resolvedMode collapses "system" to the concrete light/dark the OS reports.
export function resolvedMode(m: Mode = getMode()): "light" | "dark" {
  if (m === "dark" || m === "light") return m
  return mql().matches ? "dark" : "light"
}

// applyMode sets the html dark/light class. It's the single chokepoint every
// appearance change funnels through — setMode, the on-mount call, and the
// watchSystemMode OS-change handler all land here.
export function applyMode(m: Mode = getMode()) {
  const r = resolvedMode(m)
  const el = document.documentElement
  el.classList.toggle("dark", r === "dark")
  el.classList.toggle("light", r === "light")
}

// setMode persists the choice (clearing the key for "system" so it tracks the OS)
// and applies it immediately.
export function setMode(m: Mode) {
  if (m === "system") localStorage.removeItem(KEY)
  else localStorage.setItem(KEY, m)
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
