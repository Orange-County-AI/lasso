// Theme backgrounds: which image (if any) a theme paints behind the chrome and
// the terminals, plus the two knobs that go with one — the scrim that keeps
// glyphs readable over a photograph, and the palette-derived shading that gives
// a themed canvas some depth with no image at all.
//
// All of it is per BROWSER (localStorage, like lib/mode.ts): no backend, no
// config.toml, nothing mirrored to another host — two tabs on two machines may
// wear different backdrops under the same theme. The choice is stored PER THEME
// name, so switching palette back and forth restores the backdrop each one had
// rather than dragging one image across all of them.
//
// Three sources feed the gallery for a theme:
//   1. what lasso bundles (retro-82's 27 vendored stills, served from the
//      embedded build — the one set that never touches the network),
//   2. what the theme itself shipped (an installed Omarchy theme clones its own
//      backgrounds; the server hands back root-relative URLs — see
//      ThemeCatalogEntry.backgrounds),
//   3. what this browser was given by hand: a URL, or a file uploaded to the
//      host through /api/paste-file and read back through /api/file.
// They are additive and deduped by URL, since a theme can plausibly be both
// bundled and installed.
import bundledRetro82 from "@/lib/retro82-wallpapers.json"

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

// Per-theme choice: theme name → image URL, or NO_BACKGROUND for "flat".
const BG_KEY = "lasso-theme-backgrounds"
// The pictures this browser was given by hand, newest first. Shared across
// themes on purpose: a photo you like is a property of your screen, not of a
// palette, so it stays offerable after a theme switch.
const CUSTOM_KEY = "lasso-theme-custom-backgrounds"
const SCRIM_KEY = "lasso-theme-scrim"
const SHADE_KEY = "lasso-theme-shading"
const ATMOSPHERE_KEY = "lasso-theme-atmosphere"
// The pre-catalog key, which held a bundled still's ID rather than a URL. Read
// once and migrated (see readChoices) so an existing browser keeps the backdrop
// it was already wearing.
const LEGACY_RETRO82_KEY = "lasso-retro82-wallpaper"

// The stored value meaning "paint no image" — distinct from an absent entry,
// which means "this theme's default" (a still for retro-82, nothing elsewhere).
export const NO_BACKGROUND = "none"

// The wash between a photograph and the content. Sized for the brightest still
// in the bundled set rather than the darkest: one value serves all 27, and dim
// is a better failure than unreadable. It is the default the transparency
// slider starts at and the reset button restores; saved preferences are kept.
export const DEFAULT_SCRIM = 0.7

function readChoices(): Record<string, string> {
  let out: Record<string, string> = {}
  try {
    const raw = localStorage.getItem(BG_KEY)
    if (raw) {
      const parsed: unknown = JSON.parse(raw)
      if (parsed && typeof parsed === "object" && !Array.isArray(parsed))
        out = parsed as Record<string, string>
    }
  } catch {
    /* a hand-edited or truncated value is not worth failing a repaint over */
  }
  // Migrate the one pre-catalog choice: a still ID under its own key. Resolved
  // through the bundled manifest rather than trusted, and written into the map
  // so it survives as a URL like every other pick.
  if (!out["retro-82"]) {
    const legacy = localStorage.getItem(LEGACY_RETRO82_KEY)
    const still = legacy
      ? bundledRetro82.find((w) => w.id === legacy)
      : undefined
    if (still) {
      out["retro-82"] = still.image
      localStorage.setItem(BG_KEY, JSON.stringify(out))
      localStorage.removeItem(LEGACY_RETRO82_KEY)
    }
  }
  return out
}

function readList(key: string): string[] {
  try {
    const raw = localStorage.getItem(key)
    if (!raw) return []
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed)) return []
    return parsed.filter((v): v is string => typeof v === "string" && !!v)
  } catch {
    return []
  }
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
// what the theme itself shipped, plus this browser's own pictures.
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
  for (const url of readList(CUSTOM_KEY))
    push({ url, label: prettyName(url), thumbnail: url, custom: true })
  return out
}

// backgroundFor resolves what to actually paint for a theme: "" for none.
// An unknown stored URL — a still dropped by a later vendoring, a theme whose
// images are gone, a custom picture this browser has since forgotten — resolves
// to the theme's default instead of pointing the backdrop at a 404.
export function backgroundFor(
  theme: string,
  shipped: readonly ShippedBackground[] = []
): string {
  const stored = readChoices()[theme]
  if (stored === NO_BACKGROUND) return ""
  const gallery = themeBackgrounds(theme, shipped)
  if (stored && gallery.some((g) => g.url === stored)) return stored
  const fallback = BUNDLED_DEFAULT[theme] ?? ""
  return gallery.some((g) => g.url === fallback) ? fallback : ""
}

// setThemeBackground persists one theme's pick (NO_BACKGROUND for flat).
// Repainting is lib/theme.ts's job (applyAtmosphere), which is where both the
// chrome and the already-loaded terminal iframes pick the new URL up.
export function setThemeBackground(theme: string, url: string) {
  const choices = readChoices()
  choices[theme] = url
  localStorage.setItem(BG_KEY, JSON.stringify(choices))
}

// rememberBackground adds a hand-given picture to the gallery (newest first,
// deduped) so it stays pickable — including under another theme — instead of
// being a value only the current selection remembers.
export function rememberBackground(url: string) {
  const list = readList(CUSTOM_KEY).filter((u) => u !== url)
  list.unshift(url)
  localStorage.setItem(CUSTOM_KEY, JSON.stringify(list.slice(0, 24)))
}

// forgetBackground drops a hand-given picture from the gallery. Any theme still
// pointing at it falls back to its default on the next resolve, so no entry has
// to be swept out of the per-theme map here.
export function forgetBackground(url: string) {
  localStorage.setItem(
    CUSTOM_KEY,
    JSON.stringify(readList(CUSTOM_KEY).filter((u) => u !== url))
  )
}

type AtmospherePreference = { scrim?: number; shading?: boolean }

function atmospherePreferences(
  theme: string
): Record<string, AtmospherePreference> {
  let preferences: Record<string, AtmospherePreference> = {}
  try {
    const parsed: unknown = JSON.parse(
      localStorage.getItem(ATMOSPHERE_KEY) ?? "{}"
    )
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed))
      preferences = parsed as Record<string, AtmospherePreference>
  } catch {
    /* A damaged preference must not prevent painting the theme. */
  }
  // Assign the former screen-wide settings to the first resolved theme only.
  // Other palettes start fresh; a later switch must not inherit this choice.
  if (theme) {
    const scrim = localStorage.getItem(SCRIM_KEY)
    const shading = localStorage.getItem(SHADE_KEY)
    if (scrim !== null || shading !== null) {
      preferences[theme] ??= {
        ...(scrim !== null && Number.isFinite(Number.parseFloat(scrim))
          ? { scrim: Math.min(1, Math.max(0, Number.parseFloat(scrim))) }
          : {}),
        ...(shading !== null ? { shading: shading !== "0" } : {}),
      }
      localStorage.setItem(ATMOSPHERE_KEY, JSON.stringify(preferences))
      localStorage.removeItem(SCRIM_KEY)
      localStorage.removeItem(SHADE_KEY)
    }
  }
  return preferences
}

// The wash's alpha, 0 (raw image) to 1 (opaque canvas), per theme.
export function getScrim(theme: string): number {
  const raw = atmospherePreferences(theme)[theme]?.scrim
  return typeof raw === "number" && Number.isFinite(raw)
    ? Math.min(1, Math.max(0, raw))
    : DEFAULT_SCRIM
}

export function setScrim(theme: string, value: number) {
  if (!theme || !Number.isFinite(value)) return
  const preferences = atmospherePreferences(theme)
  preferences[theme] = {
    ...preferences[theme],
    scrim: Math.min(1, Math.max(0, value)),
  }
  localStorage.setItem(ATMOSPHERE_KEY, JSON.stringify(preferences))
}

// Palette-derived shading defaults on; an explicit false belongs to this theme.
export function getShading(theme: string): boolean {
  return atmospherePreferences(theme)[theme]?.shading !== false
}

export function setShading(theme: string, on: boolean) {
  if (!theme) return
  const preferences = atmospherePreferences(theme)
  preferences[theme] = { ...preferences[theme], shading: on }
  localStorage.setItem(ATMOSPHERE_KEY, JSON.stringify(preferences))
}
