// Installed as an app, or open in a browser tab. Two callers, one definition:
// push (iOS only exposes notifications to a Home Screen web app) and the chat
// composer's layout below md.
//
// `navigator.standalone` is Apple's original signal and is still the reliable
// one on iOS; display-mode covers every other engine.
//
// Read at call time, but fixed for the document's life in practice: launching
// from the Home Screen loads a new document, so nothing that reads this has to
// watch for a change.
export function isStandalone(): boolean {
  // Not in the DOM typings (it is Apple's own), so it is narrowed rather than
  // asserted: `in` makes the read checked and typed unknown.
  if ("standalone" in navigator && navigator.standalone === true) return true
  return window.matchMedia("(display-mode: standalone)").matches
}
