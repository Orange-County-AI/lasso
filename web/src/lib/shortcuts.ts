// Keyboard shortcuts, defined once so the handler (App) and the reference list
// (Settings) stay in sync. All are bound to the Cmd key (⌘) only — never Ctrl —
// so they don't clobber terminal control keys (e.g. Ctrl-H is backspace). The
// terminal iframes re-dispatch Cmd-shortcuts to the document, so these fire even
// while a terminal holds focus.
export interface Shortcut {
  keys: string
  label: string
}

export const SHORTCUTS: Shortcut[] = [
  { keys: "⌘K", label: "Find a pane…" },
  { keys: "⌘I", label: "New terminal…" },
  { keys: "⌘[", label: "Toggle the left sidebar" },
  { keys: "⌘]", label: "Toggle the right panel" },
]

export type ShortcutAction =
  | "toggle-left"
  | "toggle-right"
  | "palette"
  | "new-workspace"

// Match a keydown to one of the app's global Cmd-shortcuts. Cmd-only (Ctrl, Alt
// and Shift must be up) so terminal control keys (e.g. Ctrl-H) are never
// clobbered. We key off `e.code` (the physical key) first because on macOS the
// reported `e.key` is unreliable while Cmd is held — that mismatch is why ⌘[/⌘]
// flashed the terminal (the browser ran its Back/Forward default) instead of
// toggling. `e.key` is a fallback for non-US physical layouts.
export function matchShortcut(e: {
  metaKey: boolean
  ctrlKey: boolean
  altKey: boolean
  shiftKey: boolean
  key: string
  code: string
}): ShortcutAction | null {
  if (!e.metaKey || e.ctrlKey || e.altKey || e.shiftKey) return null
  switch (e.code) {
    case "BracketLeft":
      return "toggle-left"
    case "BracketRight":
      return "toggle-right"
    case "KeyK":
      return "palette"
    case "KeyI":
      return "new-workspace"
  }
  switch (e.key.toLowerCase()) {
    case "[":
      return "toggle-left"
    case "]":
      return "toggle-right"
    case "k":
      return "palette"
    case "i":
      return "new-workspace"
  }
  return null
}
