// Theme backgrounds: which image (if any) a theme paints behind the chrome and
// the terminals, plus the two knobs that go with one — the scrim that keeps
// glyphs readable over a photograph, and the palette-derived shading that gives
// a themed canvas some depth with no image at all.
//
// All of it is SERVER-owned. The picks live in lasso's own db as two fields of
// ui_state (theme_atmosphere, custom_backgrounds), reached through the same
// React Query cache, patch-merge and ui_state_rev SSE bump as the sidebar
// layout and the usage footer (lib/ui-state.ts). So a still picked on a phone
// paints on the desktop within a beat and with no reload, and a browser that
// has never been told anything wears what the last human chose. Nothing here
// touches herdr's config.toml — this is lasso's own UI state, not the shared
// theme, so nothing is mirrored to another host or to an agent CLI.
//
// The choice is stored PER THEME name — the one the BROWSER resolved, so a tab
// wearing the palette named for its scheme dresses that palette rather than
// herdr's — which means switching back and forth restores the backdrop each
// theme had rather than dragging one image across all of them. Two browsers
// resolving two different themes (one on the OS's light scheme, one on dark)
// therefore still differ on screen while agreeing on the state.
//
// Only explicit choices are stored: an absent entry is this module's default
// (below), never a value written back. That is what keeps a tab whose fetch is
// still in flight from persisting a default over a choice it hasn't seen yet —
// and why nothing paints until the fetch settles (see atmosphereKnown).
//
// Three sources feed the gallery for a theme:
//   1. what lasso bundles (retro-82's 27 vendored stills, served from the
//      embedded build — the one set that never touches the network),
//   2. what the theme itself shipped (an installed Omarchy theme clones its own
//      backgrounds; the server hands back root-relative URLs — see
//      ThemeCatalogEntry.backgrounds),
//   3. what a human handed lasso by hand: a URL, or a file uploaded to the
//      host through /api/paste-file and read back through /api/file. Kept
//      server-side too, so the picture is offerable from every browser.
// They are additive and deduped by URL, since a theme can plausibly be both
// bundled and installed.
import type { AtmospherePref } from "@/lib/api"
import { qk, queryClient } from "@/lib/query"
import bundledRetro82 from "@/lib/retro82-wallpapers.json"
import { patchUIState, uiStateNow, uiStateSettled } from "@/lib/ui-state"

// One picture the gallery can offer. `thumbnail` is a smaller copy where one
// exists (the bundled set ships 320px thumbs) and the image itself otherwise.
export interface BackgroundChoice {
  url: string
  label: string
  thumbnail: string
  // True for a picture this browser was given by hand, which is therefore the
  // only kind it can also forget.
  custom?: boolean
}

// Backgrounds lasso ships in its own bundle, keyed by theme. Only retro-82 has
// any: a palette is a few kilobytes of hex and is vendored offline for every
// official theme, a wallpaper set is tens of megabytes and is not (see
// public/wallpapers/retro-82/NOTICE.txt for what was taken and why).
const BUNDLED: Record<string, readonly BackgroundChoice[]> = {
  "retro-82": bundledRetro82.map((w) => ({
    url: w.image,
    label: w.label,
    thumbnail: w.thumbnail,
  })),
}

// The still retro-82 wears out of the box. Every OTHER theme starts with no
// background at all: a backdrop is an explicit pick, not something a palette
// switch drops on you — and only retro-82 has an offline set to default to.
const BUNDLED_DEFAULT: Record<string, string> = {
  "retro-82": "/wallpapers/retro-82/04-dusk-guardian.webp",
}

// The stored value meaning "paint no image" — distinct from an absent entry,
// which means "this theme's default" (a still for retro-82, nothing elsewhere).
export const NO_BACKGROUND = "none"

// The wash between a photograph and the content. Sized for the brightest still
// in the bundled set rather than the darkest: one value serves all 27, and dim
// is a better failure than unreadable. It is the default the transparency
// slider starts at and the reset button restores; saved preferences are kept.
export const DEFAULT_SCRIM = 0.7

// How long a run of dimming-slider writes is coalesced into one save. The
// slider fires per pixel of a drag and every save bumps ui_state_rev at every
// open tab, so the repaint stays immediate (the optimistic cache write) while
// the network write lands a few times a second.
const SCRIM_COALESCE_MS = 250

// uiStateSettled says the server's copy has arrived (or failed to). Until it
// has, this module reports NO backdrop rather than its defaults: painting the
// default still and then removing it a beat later — because this lasso's owner
// turned it off — is a photograph flashing on screen, while starting flat and
// fading the picture in is not.
//
// The per-theme choices are read from the React Query entry on every call
// rather than mirrored here: lib/ui-state.ts holds the single copy in this tab,
// and a mirror is how the painter and the gallery drift apart.
function preferences(): Record<string, AtmospherePref> {
  return uiStateNow().theme_atmosphere ?? {}
}

// subscribeAtmosphere fires when the SERVER's copy of the backdrop changes —
// this tab writing one, another browser's write arriving over ui_state_rev, or
// the first fetch landing. Both the document painter (app-store) and the open
// gallery hang off it.
//
// It compares only the atmosphere slice: the same cache entry carries the
// sidebar width, which a drag rewrites dozens of times a second, and
// applyAtmosphere reaches into every terminal iframe.
export function subscribeAtmosphere(onChange: () => void): () => void {
  let last = atmosphereSignature()
  return queryClient.getQueryCache().subscribe((event) => {
    if (event.query.queryKey[0] !== qk.uiState[0]) return
    const now = atmosphereSignature()
    if (now === last) return
    last = now
    onChange()
  })
}

// The slice of ui_state a repaint depends on: whether it has arrived, the
// per-theme choices, and the gallery (which decides whether a stored URL still
// resolves).
function atmosphereSignature(): string {
  const ui = uiStateNow()
  return JSON.stringify([
    uiStateSettled(),
    ui.theme_atmosphere ?? {},
    ui.custom_backgrounds ?? [],
  ])
}

// prettyName turns a background's URL into something a human can pick from:
// the file's own name, minus the extension and the numeric ordering prefix the
// upstream sets carry, with separators as spaces. A URL with nothing usable in
// its path (an /api/file read of an uploaded photo keeps the name in a query
// param, an image served from a bare directory has none) falls back to its
// host, which at least says where it came from.
function prettyName(url: string): string {
  let path = url
  let query = ""
  try {
    const u = new URL(url, window.location.origin)
    path = u.pathname
    query = u.searchParams.get("path") ?? ""
  } catch {
    /* keep the raw string: it is still better than nothing */
  }
  const base = (query || path).split("/").filter(Boolean).pop() ?? ""
  const name = base
    .replace(/\.[a-z0-9]+$/i, "")
    .replace(/^\d+[-_]/, "")
    .replace(/[-_]+/g, " ")
    .trim()
  if (name) return name.charAt(0).toUpperCase() + name.slice(1)
  try {
    return new URL(url, window.location.origin).host || url
  } catch {
    return url
  }
}

// One background a theme shipped with, as the catalog serves it: the full-size
// image and its 320px preview (see ThemeCatalogEntry.backgrounds/thumbs).
export interface ShippedBackground {
  url: string
  thumb: string
}

// themeBackgrounds is the gallery for one theme: what lasso bundles for it, or
// what the theme itself shipped, plus every hand-given picture on this lasso.
//
// The two shipped sources are exclusive, not additive: where lasso bundles a
// set (retro-82) it wins outright, because the upstream set is the same
// PICTURES under different filenames — merging them would offer every still
// twice under two labels, and a URL dedupe cannot see that. The bundled copy is
// also the superset and the one with real thumbnails.
export function themeBackgrounds(
  theme: string,
  shipped: readonly ShippedBackground[] = []
): BackgroundChoice[] {
  const out: BackgroundChoice[] = []
  const seen = new Set<string>()
  const push = (c: BackgroundChoice) => {
    if (!c.url || seen.has(c.url)) return
    seen.add(c.url)
    out.push(c)
  }
  const bundled = BUNDLED[theme]
  if (bundled) for (const b of bundled) push(b)
  else
    for (const s of shipped)
      push({
        url: s.url,
        label: prettyName(s.url),
        thumbnail: s.thumb || s.url,
      })
  for (const url of uiStateNow().custom_backgrounds ?? [])
    push({ url, label: prettyName(url), thumbnail: url, custom: true })
  return out
}

// backgroundFor resolves what to actually paint for a theme: "" for none.
// An unknown stored URL — a still dropped by a later vendoring, a theme whose
// images are gone, a custom picture since forgotten — resolves to the theme's
// default instead of pointing the backdrop at a 404.
//
// Nothing is painted until the server's copy has settled: see the note above
// preferences() for why a default flashing in and out is the worse failure.
export function backgroundFor(
  theme: string,
  shipped: readonly ShippedBackground[] = []
): string {
  if (!uiStateSettled()) return ""
  const stored = preferences()[theme]?.background
  if (stored === NO_BACKGROUND) return ""
  const gallery = themeBackgrounds(theme, shipped)
  if (stored && gallery.some((g) => g.url === stored)) return stored
  const fallback = BUNDLED_DEFAULT[theme] ?? ""
  return gallery.some((g) => g.url === fallback) ? fallback : ""
}

// setThemeBackground persists one theme's pick (NO_BACKGROUND for flat) for
// every browser on this lasso. Repainting is lib/theme.ts's job
// (applyAtmosphere): this tab through subscribeAtmosphere's optimistic cache
// update, every other tab when the ui_state_rev bump lands.
export function setThemeBackground(theme: string, url: string) {
  patchAtmosphere(theme, { background: url })
}

// rememberBackground adds a hand-given picture to the gallery (newest first,
// deduped) so it stays pickable — under another theme, and from another
// browser — instead of being a value only the current selection remembers.
// Sent as an op rather than a list: see UIStatePatch.
export function rememberBackground(url: string) {
  if (url) patchUIState({ remember_background: url })
}

// forgetBackground drops a hand-given picture from the gallery, everywhere. Any
// theme still pointing at it falls back to its default on the next resolve, so
// no entry has to be swept out of the per-theme map here.
export function forgetBackground(url: string) {
  if (url) patchUIState({ forget_background: url })
}

// patchAtmosphere writes ONE field of one theme's entry. The patch shape is
// what keeps the merge honest end to end: the server folds it in per theme and
// per field (mergeThemeAtmosphere), so a browser dressing rose-pine cannot drop
// what another one just picked for retro-82, nor its own theme's other knobs.
function patchAtmosphere(theme: string, pref: AtmospherePref, coalesceMs = 0) {
  // A pick made before /api/theme resolved has no theme to belong to. The
  // server drops an empty key too; refusing here keeps the optimistic copy from
  // showing a choice that will never come back.
  if (!theme) return
  patchUIState({ theme_atmosphere: { [theme]: pref } }, true, coalesceMs)
}

// The wash's alpha, 0 (raw image) to 1 (opaque canvas), per theme. No settled
// gate: it is only read when backgroundFor has already resolved an image.
export function getScrim(theme: string): number {
  const raw = preferences()[theme]?.scrim
  return typeof raw === "number" && Number.isFinite(raw)
    ? Math.min(1, Math.max(0, raw))
    : DEFAULT_SCRIM
}

export function setScrim(theme: string, value: number) {
  if (!Number.isFinite(value)) return
  patchAtmosphere(
    theme,
    { scrim: Math.min(1, Math.max(0, value)) },
    SCRIM_COALESCE_MS
  )
}

// Palette-derived shading defaults on; an explicit false belongs to this theme.
// Off while the server's copy is in flight, for the same reason as the image:
// two washes appearing and vanishing is a flicker nobody asked for.
export function getShading(theme: string): boolean {
  if (!uiStateSettled()) return false
  return preferences()[theme]?.shading !== false
}

export function setShading(theme: string, on: boolean) {
  patchAtmosphere(theme, { shading: on })
}
