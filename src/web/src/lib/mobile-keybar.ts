import { sendKeyToTerminal, type VirtualKey } from "@/lib/terminal"

// Mobile virtual keys (esc / ↑ / ↓ / tab), injected INSIDE the terminal iframe.
//
// They have to live in the iframe, not the parent: on iOS, a tap anywhere
// outside the focused element's browsing context dismisses the soft keyboard,
// and programmatic focus from the parent into an iframe input does NOT re-show it
// (iOS only honours focus tied to a gesture within the input's own context). So a
// parent-document bar could never keep the keyboard open. Inside the iframe, a
// preventDefault'd pointerdown on a sibling control keeps the xterm textarea
// focused exactly like any same-document toolbar, so the keyboard stays put.
//
// ttyd's body is a single static #terminal-container. We shorten it by the bar
// height (height: calc(100% - Npx)) and append the bar as a normal block element
// below, so xterm refits to the smaller box (rows shrink, COLS UNCHANGED) and the
// bar sits beneath the prompt rather than over it. Crucially we DON'T switch the
// body to flexbox — that subtly narrowed the container (~37px), which dropped
// columns and tripped Claude Code's wide/"desktop" layout on a phone. Touch
// devices only — desktop never mounts it, so its layout is untouched.

const BAR_ID = "__lasso_mobile_keybar"
const BAR_PX = 56 // tall, thumb-friendly targets

const KEYS: { key: VirtualKey; label: string }[] = [
  { key: "Escape", label: "esc" },
  { key: "ArrowUp", label: "↑" },
  { key: "ArrowDown", label: "↓" },
  { key: "Tab", label: "tab" },
]

// Pull these from the parent root so the bar tracks the active (light/dark) theme.
const THEME_VARS = [
  "--h-bg",
  "--h-fg",
  "--h-muted",
  "--h-border",
  "--h-panel",
  "--h-hover",
  "--h-accent-dim",
]

// mountTerminalKeyBar injects the bar into a terminal iframe. Idempotent, and a
// no-op on non-touch devices. Re-runnable on every iframe (re)load (the document
// is fresh after a host-switch reload; a ttyd WS reconnect keeps the body, so the
// bar simply survives).
export function mountTerminalKeyBar(id: string, tries = 0): void {
  const el = document.getElementById(id) as HTMLIFrameElement | null
  const win = el?.contentWindow as Window | null
  if (!win) return
  // Touch only — keep desktop terminals full-height and bar-free.
  if (!win.matchMedia?.("(pointer: coarse)").matches) return
  const doc = win.document
  if (doc.getElementById(BAR_ID)) return
  const container = doc.getElementById("terminal-container")
  if (!container) {
    // ttyd may still be loading its document; retry briefly.
    if (tries < 20) setTimeout(() => mountTerminalKeyBar(id, tries + 1), 150)
    return
  }

  // Shorten the terminal by the bar height (block flow, width untouched) so the
  // bar can sit below it without covering the prompt.
  container.style.height = `calc(100% - ${BAR_PX}px)`

  const cs = getComputedStyle(document.documentElement)
  const bar = doc.createElement("div")
  bar.id = BAR_ID
  for (const v of THEME_VARS) bar.style.setProperty(v, cs.getPropertyValue(v))
  bar.style.cssText += `;height:${BAR_PX}px;display:flex;border-top:1px solid var(--h-border);background:var(--h-panel,var(--h-bg));`

  for (const { key, label } of KEYS) {
    const b = doc.createElement("button")
    b.type = "button"
    b.tabIndex = -1
    b.textContent = label
    b.style.cssText =
      "flex:1 1 0;border:0;border-right:1px solid var(--h-border);background:transparent;color:var(--h-muted);font:500 17px/1 ui-monospace,SFMono-Regular,Menlo,monospace;display:flex;align-items:center;justify-content:center;cursor:pointer;-webkit-tap-highlight-color:transparent;touch-action:manipulation;"
    const press = (e: Event) => {
      // Same-document preventDefault keeps the xterm textarea focused, so the
      // on-screen keyboard never drops.
      e.preventDefault()
      // Focus xterm BEFORE sending the key — crucial. A tap can briefly blur the
      // textarea, making xterm emit a focus-out (ESC[O); a key that lands in that
      // window is ignored by apps that gate on focus (Claude Code runs with
      // sendFocusMode on, which is why ↑ didn't reach history). Refocusing first
      // emits focus-in (ESC[I) so the key arrives focused. Verified byte order:
      // ESC[O, ESC[I, ESC[A — arrow after focus-in, not before.
      ;(
        doc.querySelector(".xterm-helper-textarea") as HTMLElement | null
      )?.focus?.()
      sendKeyToTerminal(id, key)
      b.style.background = "var(--h-hover,var(--h-accent-dim))"
      b.style.color = "var(--h-fg)"
    }
    const release = () => {
      b.style.background = "transparent"
      b.style.color = "var(--h-muted)"
    }
    b.addEventListener("pointerdown", press)
    b.addEventListener("pointerup", release)
    b.addEventListener("pointercancel", release)
    b.addEventListener("pointerleave", release)
    bar.appendChild(b)
  }
  ;(bar.lastElementChild as HTMLElement).style.borderRight = "0"
  doc.body.appendChild(bar)
  win.dispatchEvent(new Event("resize")) // refit xterm to the shorter container
}
