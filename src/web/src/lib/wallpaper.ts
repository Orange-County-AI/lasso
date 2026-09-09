// The Retro 82 wallpaper choice: which of the vendored stills the atmosphere
// paints. Persisted in localStorage (a per-device choice, like lib/mode.ts — no
// backend, no sync), read by lib/theme.ts, written by the Settings gallery.
//
// The stills themselves are vendored under web/public/wallpapers/retro-82 and
// served from the embedded bundle, so nothing here ever reaches the network for
// an image. The manifest is the single description of what was vendored (ids,
// display names, and the two root-relative URLs per still); this module only
// resolves a stored id against it.
import manifest from "@/lib/retro82-wallpapers.json"

export interface Retro82Wallpaper {
  id: string
  label: string
  /** Root-relative URL of the still, e.g. /wallpapers/retro-82/<id>.webp */
  image: string
  /** Root-relative URL of its gallery thumbnail. */
  thumbnail: string
}

export const RETRO82_WALLPAPERS: readonly Retro82Wallpaper[] = manifest

const KEY = "lasso-retro82-wallpaper"

// The still a fresh browser lands on. Resolved through the manifest rather than
// trusted: a default naming something that was not vendored would leave the
// atmosphere with no image at all, so the first entry is the floor.
const DEFAULT_WALLPAPER =
  RETRO82_WALLPAPERS.find((w) => w.id === "02-retro-sf") ??
  RETRO82_WALLPAPERS[0]

// getWallpaper resolves the stored choice. An unknown id — a still dropped by a
// later vendoring, or a hand-edited value — falls back to the default instead
// of pointing the backdrop at a 404.
export function getWallpaper(): Retro82Wallpaper {
  const id = localStorage.getItem(KEY)
  if (!id) return DEFAULT_WALLPAPER
  return RETRO82_WALLPAPERS.find((w) => w.id === id) ?? DEFAULT_WALLPAPER
}

// setWallpaperId persists the choice; repainting is lib/theme.ts's job
// (applyWallpaper), which is where both the chrome and the terminal iframes
// pick the new URL up.
export function setWallpaperId(id: string) {
  localStorage.setItem(KEY, id)
}
