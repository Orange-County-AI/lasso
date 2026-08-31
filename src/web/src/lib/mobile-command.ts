export const MOBILE_COMMAND_EVENT = "lasso:mobile-command"

export type MobileCommand = "new" | "sidebar" | "host" | "search"

export function emitMobileCommand(command: MobileCommand): void {
  window.dispatchEvent(
    new CustomEvent<MobileCommand>(MOBILE_COMMAND_EVENT, { detail: command })
  )
}
