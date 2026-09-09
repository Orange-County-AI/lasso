// The dial's own vocabulary, shared by both of its mounts (the touch dial
// inside a terminal iframe and the navigation dial in the app document) and
// answered once in App.tsx.
export const MOBILE_COMMAND_EVENT = "lasso:mobile-command"

export type MobileCommand =
  | "new"
  | "sidebar"
  | "host"
  | "search"
  | "keybindings"

export function emitMobileCommand(command: MobileCommand): void {
  window.dispatchEvent(
    new CustomEvent<MobileCommand>(MOBILE_COMMAND_EVENT, { detail: command })
  )
}
