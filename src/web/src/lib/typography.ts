import type {
  PluginFontCategory,
  PluginsPayload,
  Typography,
  TypographySlot,
} from "@/lib/api"
import { qk, queryClient } from "@/lib/query"
import { setTermFontChoice } from "@/lib/theme"
import { patchUIState, uiStateNow } from "@/lib/ui-state"

// Typography: which typeface each slot of the UI wears, chosen among the fonts
// enabled plugins contribute. The choice is server state (ui_state.typography,
// merged per slot) and the fonts are the plugin listing's; this module resolves
// one against the other and applies the result to the document and to every
// terminal iframe.
//
// A plugin contributes DATA here, never CSS. A plugin that could hand lasso a
// stylesheet could restyle lasso's own UI — hide the approval dialog's
// warnings, paint a fake prompt — so every byte below is composed by lasso from
// fields it re-validates on the way in, even though the server validated them
// first: a family matching FAMILY_RE, a same-origin /plugins/ URL, a numeric
// weight and a two-valued style. Anything that fails is skipped, not escaped.

export const TYPOGRAPHY_SLOTS: TypographySlot[] = [
  "sans",
  "display",
  "label",
  "mono",
  "terminal",
]

export const SLOT_LABELS: Record<TypographySlot, string> = {
  sans: "Interface",
  display: "Display",
  label: "Labels",
  mono: "Code",
  terminal: "Terminal",
}

// The custom property each document slot pins, and the stack it falls back to
// — the SAME stacks index.css declares, so a chosen family that is slow to
// load, or missing a glyph, degrades to the look lasso has without it.
const SLOT_VARS: Record<Exclude<TypographySlot, "terminal">, string> = {
  sans: "--font-sans",
  display: "--font-display",
  label: "--font-label",
  mono: "--font-mono",
}

const SLOT_DEFAULT_STACKS: Record<
  Exclude<TypographySlot, "terminal">,
  string
> = {
  sans: '"Space Grotesk", "Geist", system-ui, sans-serif',
  display: '"Doto", "Space Grotesk", system-ui, sans-serif',
  label: '"Space Mono", "JetBrains Mono", ui-monospace, monospace',
  mono: '"JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, monospace',
}

// Code and the terminal are grids: a proportional face there breaks alignment,
// so only mono fonts are offered (and resolved) for them.
export function slotAccepts(
  slot: TypographySlot,
  category: PluginFontCategory
): boolean {
  return slot === "mono" || slot === "terminal" ? category === "mono" : true
}

// The server's own injection guard (plugins.go), restated: this string lands
// inside a CSS string literal, and the regex is what makes that safe.
const FAMILY_RE = /^[A-Za-z0-9 _-]{1,64}$/
const CATEGORIES = new Set<PluginFontCategory>([
  "sans",
  "serif",
  "display",
  "mono",
])
const STYLE_ID = "plugin-fonts"

export interface ResolvedFace {
  url: string
  weight: number
  style: "normal" | "italic"
  format: string
}

export interface ResolvedFont {
  globalID: string
  plugin: string
  family: string
  category: PluginFontCategory
  license?: string
  faces: ResolvedFace[]
}

const FORMATS: Record<string, string> = {
  woff2: "woff2",
  woff: "woff",
  ttf: "truetype",
  otf: "opentype",
}

// safeFaceURL admits only a same-origin path under /plugins/ with no parent
// segment, and percent-encodes every character outside a conservative set so
// nothing in it can close the url("…") it is placed in. "%" is kept as-is:
// the server may already have encoded the path.
function safeFaceURL(url: unknown): string | null {
  if (typeof url !== "string" || !url.startsWith("/plugins/")) return null
  const path = url.split(/[?#]/)[0]
  if (path.split("/").some((seg) => seg === ".." || seg === ".")) return null
  let out = ""
  for (const ch of url) {
    if (/[A-Za-z0-9._~/%?=&-]/.test(ch)) out += ch
    else
      for (const b of new TextEncoder().encode(ch))
        out += `%${b.toString(16).toUpperCase().padStart(2, "0")}`
  }
  return out
}

function faceFormat(url: string): string {
  const ext = /\.([a-z0-9]+)$/i.exec(url.split(/[?#]/)[0])?.[1]?.toLowerCase()
  return (ext && FORMATS[ext]) || ""
}

// resolvePluginFonts flattens the listing to the fonts lasso may use: enabled
// plugins only, each field re-validated, at least one usable face. A family
// two plugins both claim goes to the first in listing order — @font-face rules
// sharing a family MERGE, so the second plugin's faces would otherwise fill in
// weights of the first's typeface.
export function resolvePluginFonts(
  data: PluginsPayload | undefined
): ResolvedFont[] {
  const out: ResolvedFont[] = []
  const families = new Set<string>()
  for (const p of data?.plugins ?? []) {
    if (p.state !== "enabled") continue
    for (const f of p.fonts ?? []) {
      if (typeof f?.family !== "string" || !FAMILY_RE.test(f.family)) continue
      if (!CATEGORIES.has(f.category)) continue
      const key = f.family.toLowerCase()
      if (families.has(key)) continue
      const faces: ResolvedFace[] = []
      for (const face of f.faces ?? []) {
        const url = safeFaceURL(face?.url)
        const weight = face?.weight
        if (!url) continue
        if (
          typeof weight !== "number" ||
          !Number.isInteger(weight) ||
          weight < 100 ||
          weight > 900 ||
          weight % 100 !== 0
        )
          continue
        if (face.style !== "normal" && face.style !== "italic") continue
        faces.push({ url, weight, style: face.style, format: faceFormat(url) })
      }
      if (!faces.length) continue
      families.add(key)
      out.push({
        globalID:
          typeof f.global_id === "string" && f.global_id
            ? f.global_id
            : `plugin:${p.name}:${f.id}`,
        plugin: p.name,
        family: f.family,
        category: f.category,
        license: typeof f.license === "string" ? f.license : undefined,
        faces,
      })
    }
  }
  return out
}

function fontFaceCSS(font: ResolvedFont): string {
  return font.faces
    .map((face) => {
      const format = face.format ? ` format("${face.format}")` : ""
      return `@font-face{font-family:"${font.family}";font-style:${face.style};font-weight:${face.weight};font-display:swap;src:url("${face.url}")${format}}`
    })
    .join("")
}

function pluginsNow(): PluginsPayload | undefined {
  return queryClient.getQueryData<PluginsPayload>(qk.plugins)
}

function typographyNow(): Typography {
  return uiStateNow().typography ?? {}
}

// fontForSlot is the font a slot resolves to, or null for lasso's default —
// an unset slot, or an id no enabled plugin provides right now (a plugin
// disabled for a while keeps its selection and gets it back on re-enable).
export function fontForSlot(
  slot: TypographySlot,
  fonts: ResolvedFont[],
  typography: Typography
): ResolvedFont | null {
  const id = typography[slot]
  if (!id) return null
  const font = fonts.find((f) => f.globalID === id)
  return font && slotAccepts(slot, font.category) ? font : null
}

// applyTypography is the one chokepoint. Every enabled plugin font's
// @font-face goes into ONE <style> (they are lazy — a face nobody uses is
// never downloaded — and Settings previews every option in its own face), and
// each document slot is pinned on <html> as the chosen family in front of the
// slot's default stack. An unset slot REMOVES the pin, which is an exact
// restore of index.css. Nothing is written when nothing changed.
export function applyTypography() {
  const fonts = resolvePluginFonts(pluginsNow())
  const typography = typographyNow()
  const css = fonts.map(fontFaceCSS).join("")
  const have = document.getElementById(STYLE_ID)
  if (!css) have?.remove()
  else if (have) {
    if (have.textContent !== css) have.textContent = css
  } else {
    const style = document.createElement("style")
    style.id = STYLE_ID
    style.textContent = css
    document.head.appendChild(style)
  }
  const root = document.documentElement
  for (const slot of Object.keys(SLOT_VARS) as (keyof typeof SLOT_VARS)[]) {
    const font = fontForSlot(slot, fonts, typography)
    const prop = SLOT_VARS[slot]
    const want = font ? `"${font.family}", ${SLOT_DEFAULT_STACKS[slot]}` : ""
    if (root.style.getPropertyValue(prop) === want) continue
    if (want) root.style.setProperty(prop, want)
    else root.style.removeProperty(prop)
  }
  const term = fontForSlot("terminal", fonts, typography)
  setTermFontChoice(
    term ? { family: term.family, css: fontFaceCSS(term) } : null
  )
}

// The slice a repaint depends on: the stored choices and the fonts on offer.
function signature(): string {
  return JSON.stringify([typographyNow(), resolvePluginFonts(pluginsNow())])
}

// subscribeTypography applies now and again whenever the stored choice or the
// plugin listing changes — a pick here, one from another browser over
// ui_state_rev, a plugin enabled or disabled over plugins_rev. It compares only
// its own slice, since the ui_state entry also carries the sidebar width a drag
// rewrites dozens of times a second.
export function subscribeTypography(): () => void {
  let last = signature()
  applyTypography()
  return queryClient.getQueryCache().subscribe((event) => {
    const k = event.query.queryKey[0]
    if (k !== qk.uiState[0] && k !== qk.plugins[0]) return
    const now = signature()
    if (now === last) return
    last = now
    applyTypography()
  })
}

// setTypography writes ONE slot — the server merges per slot, so this cannot
// clobber a slot another device just set. "" restores the default.
export function setTypography(slot: TypographySlot, id: string) {
  patchUIState({ typography: { [slot]: id } })
}
