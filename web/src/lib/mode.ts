import { applyTermThemeForMode } from "@/lib/theme"

// Appearance mode: the user's UI+terminal light/dark preference, persisted in
// localStorage (a per-device choice; no backend). "system" follows the OS via
// prefers-color-scheme. The resolved value drives both the html dark/light class
// (which all the --h-* tokens / shadcn primitives cascade from, see index.css)
// and the live xterm palette (see lib/theme.ts). An inline script in index.html
// applies the class before first paint to avoid a flash; this module keeps it in
// sync afterwards and re-pins the terminals.
export type Mode = "system" | "light" | "dark"

const KEY = "lasso-mode"
const mql = () => window.matchMedia("(prefers-color-scheme: dark)")

// persistTheme mirrors the applied appearance to ~/.lasso/theme.json (via
// POST /api/theme) so external tools — e.g. a shell statusline — can read the
// live light/dark state without a browser. Fire-and-forget: the file is a
// convenience for outside readers, never on the UI's critical path, so we
// swallow network errors rather than block or surface them.
function persistTheme(m: Mode) {
  void fetch("/api/theme", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ mode: m, resolved: resolvedMode(m) }),
  }).catch(() => {})
}

export function getMode(): Mode {
  const v = localStorage.getItem(KEY)
  return v === "light" || v === "dark" ? v : "system"
}

// resolvedMode collapses "system" to the concrete light/dark the OS reports.
export function resolvedMode(m: Mode = getMode()): "light" | "dark" {
  if (m === "dark" || m === "light") return m
  return mql().matches ? "dark" : "light"
}

// applyMode sets the html class and re-pins the terminals to match. It's the
// single chokepoint every appearance change funnels through — setMode, the
// on-mount call, and the watchSystemMode OS-change handler all land here — so we
// mirror the applied state to ~/.lasso/theme.json from this one spot.
export function applyMode(m: Mode = getMode()) {
  const r = resolvedMode(m)
  const el = document.documentElement
  el.classList.toggle("dark", r === "dark")
  el.classList.toggle("light", r === "light")
  applyTermThemeForMode(r)
  persistTheme(m)
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
