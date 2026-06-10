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
  { keys: "⌘U", label: "New tab…" },
  { keys: "⌘G", label: "Toggle the grid view" },
  { keys: "⌘[", label: "Toggle the left sidebar" },
  { keys: "⌘]", label: "Toggle the right panel" },
  { keys: "⌘?", label: "Keyboard shortcuts" },
]

export type ShortcutAction =
  | "palette"
  | "new-workspace"
  | "new-tab"
  | "grid"
  | "shortcuts"

// Match a keydown to one of the app's keydown-driven Cmd-shortcuts. Cmd-only
// (Ctrl and Alt must be up) so terminal control keys (e.g. Ctrl-H) are never
// clobbered. Shift is rejected for everything except ⌘? (the shortcuts
// reference), whose `?` is itself a shifted key. We key off `e.code` (the
// physical key) first because on macOS the reported `e.key` is unreliable while
// Cmd is held; `e.key` is a fallback for non-US physical layouts.
//
// ⌘[ / ⌘] are deliberately NOT here: macOS browsers reserve them for history
// Back/Forward and consume the keystroke before the page sees it (the page only
// ever receives the bare Meta keydown), so they can't be handled as keydowns.
// They're driven by a history trap instead — see lib/history-toggle.ts.
export function matchShortcut(e: {
  metaKey: boolean
  ctrlKey: boolean
  altKey: boolean
  shiftKey: boolean
  key: string
  code: string
}): ShortcutAction | null {
  if (!e.metaKey || e.ctrlKey || e.altKey) return null
  // ⌘? (Cmd-Shift-/) opens the shortcuts reference — the only binding that uses
  // Shift. Match the physical Slash key (code) first since `?` is its shifted
  // form; fall back to the `?` character for layouts that place it elsewhere.
  if (e.shiftKey) {
    if (e.code === "Slash" || e.key === "?") return "shortcuts"
    return null
  }
  switch (e.code) {
    case "KeyK":
      return "palette"
    case "KeyI":
      return "new-workspace"
    case "KeyU":
      return "new-tab"
    case "KeyG":
      return "grid"
  }
  switch (e.key.toLowerCase()) {
    case "k":
      return "palette"
    case "i":
      return "new-workspace"
    case "u":
      return "new-tab"
    case "g":
      return "grid"
  }
  return null
}
