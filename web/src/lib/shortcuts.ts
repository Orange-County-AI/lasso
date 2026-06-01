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
  { keys: "⌘G", label: "Open the Grid tab" },
  { keys: "⌘H", label: "Open the Herdr tab" },
  { keys: "⌘P", label: "Toggle the sidebar" },
]
