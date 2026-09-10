// Appearance mode: what lasso's chrome is painted from. "herdr" makes the
// chrome track herdr's own theme (the pre-Nothing behavior — see
// lib/theme.ts:applyHerdrChrome), "system" follows the OS via
// prefers-color-scheme, "light"/"dark" pin the Nothing palette. The resolved
// value drives the html dark/light class, which all the --h-* Nothing tokens /
// shadcn primitives cascade from (see index.css).
//
// It is SERVER state, not a per-device one: the mode and the palette named for
// each scheme are three fields of ui_state (lib/ui-state.ts), so they ride the
// same patch-merge, the same React Query cache and the same ui_state_rev SSE
// bump as the backdrop and the usage footer. Choosing "Dark" on a phone lands
// on the desktop within a beat and with no reload, and a fresh browser wears
// what the last human chose rather than the defaults. Nothing about appearance
// is in localStorage — this module reads the cache and writes patches, and the
// class is (re)applied from whatever the cache holds.
//
// "system" is the one answer that stays per DEVICE, and deliberately: the OS
// scheme is an observation about the screen in front of someone, not a
// preference to be shared. So "system" is what is stored fleet-wide and each
// browser resolves it against its own media query.
//
// NOTE: unlike the main (tmux) branch, the *terminal* palette is not
// mode-driven — herdr's theme (or the palette named for the scheme in force)
// dictates it, see lib/theme.ts. So applyMode only touches the chrome class;
// it deliberately does not re-pin the terminals.
import { type AppearanceMode, api } from "@/lib/api"
import { qk, queryClient } from "@/lib/query"
import { patchUIState, uiStateNow, uiStateSettled } from "@/lib/ui-state"

export type Mode = AppearanceMode

// Installs default to "herdr" — the chrome matches herdr's theme out of the
// box; the Nothing light/dark palette is an explicit opt-in. Mirrors the
// server's own default (getUIState in db.go) and index.html's pre-paint class.
const DEFAULT_MODE: Mode = "herdr"
const mql = () => window.matchMedia("(prefers-color-scheme: dark)")

// getMode reads the stored mode out of the shared cache — the defaults until
// the first /api/ui-state lands, which is the same value index.html painted
// pre-paint, so a boot converges rather than flashing. The value is validated
// rather than trusted: an older server (or a hand-edited row) has no field
// here at all, and an unknown string must not reach the class toggle.
export function getMode(): Mode {
  const v = uiStateNow().appearance_mode
  return v === "light" || v === "dark" || v === "system" || v === "herdr"
    ? v
    : DEFAULT_MODE
}

// resolvedMode collapses "system" to the concrete light/dark the OS reports.
// "herdr" resolves to "dark" as its STARTING point only: it is what the class
// is before the palette has resolved, and herdr's own theme may well be a light
// one now that it is anything a user installed — see applyScheme, which
// refreshTheme calls with the palette's own lightness once it knows it.
export function resolvedMode(m: Mode = getMode()): "light" | "dark" {
  if (m === "dark" || m === "light") return m
  if (m === "herdr") return "dark"
  return mql().matches ? "dark" : "light"
}

// applyScheme pins the html light/dark class — the class every --h-* token
// block (index.css) and every `dark:` tailwind variant cascades from.
export function applyScheme(scheme: "light" | "dark") {
  const el = document.documentElement
  el.classList.toggle("dark", scheme === "dark")
  el.classList.toggle("light", scheme === "light")
}

// applyMode sets the class from the appearance mode. It's the chokepoint every
// appearance change funnels through — a pick here, a pick in another browser
// arriving over ui_state_rev, the on-mount call, and the watchSystemMode
// OS-change handler all land here.
//
// It is not the LAST word, though: while the chrome follows a herdr/Omarchy
// palette the class has to match that palette's canvas rather than the mode's
// nominal scheme, so refreshTheme re-derives it (it has /api/theme's palette to
// hand, and applies the --h-* override in the same pass). A light theme under
// the dark class left every `dark:` variant in the chrome painting for a canvas
// it no longer had.
export function applyMode(m: Mode = getMode()) {
  applyScheme(resolvedMode(m))
}

// setMode stores the choice for every browser on this lasso and applies the
// class here immediately (the optimistic cache write is what the other tabs'
// subscription eventually reports too). The caller is responsible for
// refreshing the chrome override (refreshTheme) so toggling into/out of "herdr"
// repaints without waiting for the next theme tick.
export function setMode(m: Mode) {
  patchUIState({ appearance_mode: m })
  applyMode(m)
  pushPaletteToFleet()
}

// watchSystemMode re-applies on OS theme changes while the mode is "system".
// `onChange` is called after the class flip so the caller can re-derive
// anything that depends on the resolved scheme — the palette is named PER
// SCHEME (see localPaletteName), so an OS flip at dusk changes which theme this
// tab wears and its terminals have to be re-pinned. It is a callback rather
// than a direct refreshTheme() call because lib/theme.ts imports this module,
// not the other way round.
// Idempotent — safe to call once on app mount.
let watching = false
export function watchSystemMode(onChange?: () => void) {
  if (watching) return
  watching = true
  mql().addEventListener("change", () => {
    if (getMode() !== "system") return
    applyMode("system")
    onChange?.()
    pushPaletteToFleet()
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

// subscribeAppearance fires when the stored appearance changes — a pick made
// here, one made in another browser and delivered by the ui_state_rev bump, or
// the first fetch landing. AppProvider hangs refreshTheme off it, which is the
// full repaint: the class, the --h-* override, the terminals and the backdrop.
//
// It compares only the appearance slice for the same reason
// subscribeAtmosphere does: the one cache entry also carries the sidebar width,
// which a drag rewrites dozens of times a second, and a repaint reaches into
// every terminal iframe. Read-only — nothing here writes, so an arriving change
// cannot echo back out as a write.
export function subscribeAppearance(onChange: () => void): () => void {
  let last = appearanceSignature()
  return queryClient.getQueryCache().subscribe((event) => {
    if (event.query.queryKey[0] !== qk.uiState[0]) return
    const now = appearanceSignature()
    if (now === last) return
    last = now
    onChange()
  })
}

// The slice a repaint depends on: whether the server's copy has arrived (the
// defaults are painted before it does, and the real values may differ) and the
// three appearance fields.
function appearanceSignature(): string {
  const ui = uiStateNow()
  return JSON.stringify([
    uiStateSettled(),
    getMode(),
    ui.palette_light ?? "",
    ui.palette_dark ?? "",
  ])
}

// ---------------------------------------------------------------------------
// Palettes
// ---------------------------------------------------------------------------

// Outside "herdr" mode the chrome is the Nothing palette by default, and the
// terminals wear whatever herdr resolves fleet-wide. A user who wants a
// different look per light/dark can instead name a theme for each scheme:
// lasso then paints its chrome AND its terminals from that theme, resolved
// through /api/theme?name= — a read, so RESOLVING one writes nothing.
//
// The write is a separate, deliberate act: an appearance change made in this
// browser pushes what it resolved to the fleet (pushPaletteToFleet, below), so
// the agents inside the terminals match the terminals. A palette that merely
// arrived from another browser, or a repaint, still writes nothing — which is
// what keeps an OS flip on an unattended screen from oscillating every machine
// in the fleet twice a day.
//
// "" (the default, and what every existing install has) means "no palette":
// the Nothing chrome, exactly as before.
export function getPalettePref(scheme: "light" | "dark"): string {
  const ui = uiStateNow()
  return (scheme === "light" ? ui.palette_light : ui.palette_dark) ?? ""
}

// setPalettePref stores one scheme's theme ("" clears it) for every browser on
// this lasso. Repainting is lib/theme.ts's job (refreshTheme), which has to
// re-resolve the palette.
export function setPalettePref(scheme: "light" | "dark", name: string) {
  patchUIState(
    scheme === "light" ? { palette_light: name } : { palette_dark: name }
  )
  pushPaletteToFleet()
}

// localPaletteName is the theme to resolve instead of herdr's own, or "" to
// follow herdr's. In "herdr" mode it is always "" — that mode is the explicit
// "follow the fleet" choice — and otherwise it is the palette named for the
// scheme currently in force, so "system" picks up the OS flip for free.
export function localPaletteName(m: Mode = getMode()): string {
  if (m === "herdr") return ""
  return getPalettePref(resolvedMode(m))
}

// ---------------------------------------------------------------------------
// Pushing the resolved palette to the fleet
// ---------------------------------------------------------------------------

// The palette above is a browser-local READ, but the theme lasso WRITES —
// herdr's config.toml plus every agent CLI's own theme file (src/agentsync.go)
// — is a single fleet-wide value, and nothing connected the two. So naming a
// light palette repainted this tab's chrome and its terminals ayu-light while
// omp, opencode, Claude Code and herdr's own tab bar kept the theme the herdr
// picker last set: a cream canvas with retro-82 navy panels drawn on it. Only
// the Settings "Sync now" button closed that, by hand.
//
// An appearance change made HERE now makes the same call that button does.
// Deliberately only from the three entry points where this device is the CAUSE
// — setMode, setPalettePref and the OS-scheme flip — and never from
// refreshTheme, which also runs for a change that ARRIVED over ui_state_rev: a
// push bumps theme_rev and repaints every other browser, so pushing from a
// repaint would have two devices resolving "system" differently re-theming each
// other forever. The last human to act decides, which is what asking the fleet
// to follow your screen means.
const FLEET_PUSH_DEBOUNCE_MS = 500
const FLEET_PUSH_RETRY_MS = 3000

// The palette last handed to the fleet, so clicking around the appearance
// controls doesn't fan a 14-host SFTP push out per click. Reset on a final
// failure, below.
let pushedPalette: string | null = null
let pushTimer: ReturnType<typeof setTimeout> | null = null

// pushPaletteToFleet mirrors this browser's resolved palette onto herdr's config
// and every reachable host's agent themes. "" is skipped: that is "herdr" mode,
// whose whole meaning is that the fleet's own theme wins, and pushing it would
// be a fleet-wide write of what is already there.
export function pushPaletteToFleet() {
  const palette = localPaletteName()
  if (!palette) {
    // "herdr" mode hands the fleet theme back to the herdr picker, which may
    // move it while we are not looking. Forget what we pushed, or coming back to
    // a palette we happen to have pushed BEFORE that would read as already
    // synced and leave the fleet on whatever the picker set — the exact
    // mismatch this exists to close.
    pushedPalette = null
    return
  }
  if (palette === pushedPalette) return
  pushedPalette = palette
  if (pushTimer) clearTimeout(pushTimer)
  // Debounced because the appearance controls are clicked in runs (mode, then
  // the palette for it), and each push is a fleet-wide fanout the server
  // refuses to run twice at once.
  pushTimer = setTimeout(
    () => void sendPush(palette, true),
    FLEET_PUSH_DEBOUNCE_MS
  )
}

// sendPush is fire-and-forget except for one retry, because a dropped push is
// not self-healing: convergence catches a host up against the theme lasso last
// WROTE, and a push that never landed never became that. A 409 (the server
// already has a fanout in flight) is the likely case and is exactly the one
// worth retrying.
async function sendPush(palette: string, retry: boolean) {
  pushTimer = null
  try {
    await api.syncThemeNow(palette, true)
    return
  } catch {
    // fall through
  }
  if (retry && localPaletteName() === palette) {
    pushTimer = setTimeout(
      () => void sendPush(palette, false),
      FLEET_PUSH_RETRY_MS
    )
    return
  }
  // Out of retries: forget it, so the next appearance action tries again rather
  // than reading as already pushed. The failing fanout toasts on its own.
  if (pushedPalette === palette) pushedPalette = null
}
