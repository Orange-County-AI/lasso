// The first-run tour's trigger. App mounts the tour and opens it on its own
// when ui_state.onboarding_done is false; anything else that wants to replay
// it (the Settings tab's "Take the tour") calls startOnboarding, and the two
// never import each other. Same tiny pub/sub shape as lib/open-file.ts.

type Listener = () => void
const listeners = new Set<Listener>()

export function onStartOnboarding(fn: Listener): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

export function startOnboarding() {
  for (const fn of listeners) fn()
}

// A step's anchor is the element carrying data-tour="<target>". Anchors are
// attributes rather than refs so the tour can point at controls owned by any
// component without threading refs through App.
export function tourAnchor(target: string): HTMLElement | null {
  return document.querySelector<HTMLElement>(`[data-tour="${target}"]`)
}
