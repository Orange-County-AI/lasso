// Contrast repair for palettes lasso did not author.
//
// The chrome's --h-* tokens come straight from a theme's own colors, and a
// theme is now anything a user installed from a git URL. Those palettes are
// written for a TERMINAL, where "muted" means the dim ANSI gray a prompt uses
// against its own background — which is not the same promise as "readable
// secondary text on this canvas". ayu-light is the honest example: `--muted:
// #d1d1d1` on `--bg: #f8f9fa` is ~1.2:1, i.e. every label, hint and caption in
// Settings invisible, and `--warn: #eca944` on white is ~1.9:1.
//
// So each foreground token is checked against the canvas and, only when it
// fails, walked toward the canvas's opposite until it passes. Hue is preserved
// (the gray stays gray, the warn stays amber) and a palette that already
// contrasts is returned untouched — which is every dark theme lasso ships, so
// the default look does not move.
//
// WCAG 2.1 relative luminance and contrast ratio; the thresholds are the
// caller's (see lib/theme.ts:applyHerdrChrome).

interface RGB {
  r: number
  g: number
  b: number
}

// parseHex accepts #rgb and #rrggbb, the two spellings herdr's palettes use.
// Anything else (a named color, an rgba(), a var()) returns null and is left
// exactly as the theme wrote it — a color we cannot measure is a color we must
// not rewrite.
function parseHex(color: string): RGB | null {
  const hex = color.trim()
  if (!/^#([0-9a-f]{3}|[0-9a-f]{6})$/i.test(hex)) return null
  const full =
    hex.length === 4
      ? `#${hex[1]}${hex[1]}${hex[2]}${hex[2]}${hex[3]}${hex[3]}`
      : hex
  const n = Number.parseInt(full.slice(1), 16)
  // Channel extraction from a packed hex.
  return { r: (n >> 16) & 255, g: (n >> 8) & 255, b: n & 255 }
}

function toHex({ r, g, b }: RGB): string {
  const part = (v: number) =>
    Math.round(Math.min(255, Math.max(0, v)))
      .toString(16)
      .padStart(2, "0")
  return `#${part(r)}${part(g)}${part(b)}`
}

// WCAG relative luminance.
function luminance({ r, g, b }: RGB): number {
  const chan = (v: number) => {
    const s = v / 255
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4
  }
  return 0.2126 * chan(r) + 0.7152 * chan(g) + 0.0722 * chan(b)
}

// contrastRatio is WCAG's (L1 + 0.05) / (L2 + 0.05), 1 (identical) to 21
// (black on white). Exported for the tests and for callers that want to report
// rather than repair.
export function contrastRatio(a: string, b: string): number {
  const x = parseHex(a)
  const y = parseHex(b)
  if (!x || !y) return 21
  const la = luminance(x)
  const lb = luminance(y)
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05)
}

function mix(color: RGB, toward: RGB, amount: number): RGB {
  return {
    r: color.r + (toward.r - color.r) * amount,
    g: color.g + (toward.g - color.g) * amount,
    b: color.b + (toward.b - color.b) * amount,
  }
}

const BLACK: RGB = { r: 0, g: 0, b: 0 }
const WHITE: RGB = { r: 255, g: 255, b: 255 }

// ensureContrast returns `color` when it already reaches `target` against
// `bg`, and otherwise the same color walked toward black (on a light canvas) or
// white (on a dark one) in 4% steps until it does. Stepping rather than
// solving keeps the result as close to what the theme asked for as the
// threshold allows; a fully-walked color that still fails (a canvas at
// mid-luminance can defeat both directions) falls back to whichever pole is
// further from the canvas, which is the most readable color available.
export function ensureContrast(
  color: string,
  bg: string,
  target: number
): string {
  const rgb = parseHex(color)
  const canvas = parseHex(bg)
  if (!rgb || !canvas) return color
  if (contrastRatio(color, bg) >= target) return color
  const toward = luminance(canvas) > 0.45 ? BLACK : WHITE
  for (let step = 1; step <= 25; step++) {
    const candidate = toHex(mix(rgb, toward, step * 0.04))
    if (contrastRatio(candidate, bg) >= target) return candidate
  }
  return toHex(toward)
}
