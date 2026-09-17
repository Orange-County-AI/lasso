// App commands sent by the input dial inside a terminal iframe. Dispatched on
// the PARENT's window — this module runs in the parent realm, so App's listener
// hears a dial in either iframe (herdr's terminal and the sidebar's shell).
export const MOBILE_COMMAND_EVENT = "lasso:mobile-command"

export type MobileCommand = "new" | "sidebar" | "host" | "search" | "chat"

export function emitMobileCommand(command: MobileCommand): void {
  window.dispatchEvent(
    new CustomEvent<MobileCommand>(MOBILE_COMMAND_EVENT, { detail: command })
  )
}
