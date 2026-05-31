import { api, type ThemePayload } from "@/lib/api"

// The terminal iframes whose xterm.js theme we keep in sync with herdr's.
const TERM_FRAME_IDS = ["term", "shellframe"]

let lastXtermTheme: Record<string, unknown> | null = null

// applyCSSVars takes the `:root { --bg: …; --accent: … }` block the Go server
// produces and writes each variable onto the document root, namespaced as
// --h-* so it can't collide with shadcn's own design tokens (which read these
// via index.css). This is the live-theme repaint: the sidebar, file viewer,
// diff, markdown and syntax colors all cascade from these vars.
function applyCSSVars(css: string) {
  const root = document.documentElement
  const re = /--([\w-]+)\s*:\s*([^;]+);/g
  let m: RegExpExecArray | null
  while ((m = re.exec(css)) !== null) {
    root.style.setProperty(`--h-${m[1]}`, m[2].trim())
  }
}

// applyTermTheme sets xterm.js's theme on every terminal iframe. ttyd 1.7.4
// exposes the Terminal as window.term and the iframes are same-origin (proxied
// under /terminal/ and /shell/), so the parent can reach in. A terminal may not
// be ready when a theme arrives (iframe still loading), so retry a few times.
export function applyTermTheme(
  theme: Record<string, unknown> | null,
  tries = 0
) {
  if (!theme) return
  let pending = false
  for (const id of TERM_FRAME_IDS) {
    const el = document.getElementById(id) as HTMLIFrameElement | null
    if (!el) continue
    try {
      const w = el.contentWindow as unknown as {
        term?: { options?: Record<string, unknown> }
      }
      if (w?.term?.options) {
        w.term.options.theme = theme
        continue
      }
    } catch {
      /* same-origin: shouldn't throw, but never let it break the caller */
    }
    pending = true
  }
  if (pending && tries < 20)
    setTimeout(() => applyTermTheme(theme, tries + 1), 250)
}

export function lastTerminalTheme() {
  return lastXtermTheme
}

// refreshTheme pulls the resolved palette and repaints both halves of the UI:
// the React/CSS side (rewrite the --h-* vars) and the live terminals (set the
// xterm.js theme inside the same-origin ttyd iframes — no reconnect).
export async function refreshTheme() {
  let t: ThemePayload
  try {
    t = await api.theme()
  } catch {
    return
  }
  applyCSSVars(t.css)
  lastXtermTheme = t.xterm
  applyTermTheme(t.xterm, 0)
}
