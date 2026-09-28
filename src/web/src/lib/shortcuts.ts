// Keyboard shortcuts, defined once so the handler (App) and the reference list
// (Settings) stay in sync. All are bound to the Cmd key (⌘) only — never Ctrl —
// so they don't clobber terminal control keys (e.g. Ctrl-H is backspace). The
// terminal iframes re-dispatch Cmd-shortcuts to the document, so these fire even
// while a terminal holds focus.
//
// Keys are picked around three things that would otherwise fight them: the
// browser's own unpreventable chords (⌘T/⌘W/⌘N/⌘Q, ⌘1–9, ⌘⇧T), the file
// viewer's CodeMirror keymap (⌘F/⌘G/⌘D/⌘U/⌘[ ⌘] — App's listener sits on the
// document, so a chord CodeMirror also binds would do both), and copy/paste.
export interface Shortcut {
  keys: string
  label: string
}

export interface ShortcutGroup {
  title: string
  shortcuts: Shortcut[]
}

export const SHORTCUT_GROUPS: ShortcutGroup[] = [
  {
    title: "Anywhere",
    shortcuts: [
      {
        keys: "⌘K",
        label:
          "Find a pane… (herdr's search, ⌃B G; in chat/agents: find an agent)",
      },
      { keys: "⌘O", label: "New agent…" },
      { keys: "⌘I", label: "New terminal…" },
      { keys: "⌘/", label: "Toggle keyboard shortcuts" },
    ],
  },
  {
    title: "Views",
    shortcuts: [
      { keys: "⌘J", label: "Chat ⟷ terminal" },
      { keys: "⌘E", label: "Agents grid ⟷ terminal" },
      {
        keys: "⌘B",
        label: "Left sidebar (herdr's in the terminal, agents in chat)",
      },
      { keys: "⌘\\", label: "Toggle the right sidebar" },
      { keys: "⌘⇧F", label: "Sidebar: Files" },
      { keys: "⌘⇧S", label: "Sidebar: Scratch" },
      { keys: "⌘⇧B", label: "Sidebar: Browser" },
    ],
  },
  {
    title: "In context",
    shortcuts: [
      { keys: "⌘↩", label: "Chat: send (↩ breaks the line)" },
      { keys: "⌘↩", label: "Scratch: send to the terminal" },
      { keys: "⌘↩", label: "New agent/terminal: create" },
      { keys: "⌘S", label: "File viewer: save" },
    ],
  },
]

// The ⌘ keys (lower-cased e.key) App claims, split by whether Shift is held.
// LiveBrowser leaves exactly these to bubble instead of sending them to the page.
export const APP_KEYS = new Set(["k", "o", "i", "\\", "/", "j", "e", "b"])
export const APP_SHIFT_KEYS = new Set(["f", "s", "b"])
