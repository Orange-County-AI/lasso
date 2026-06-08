// ⌘[ / ⌘] sidebar toggles, done via a history trap instead of a keydown.
//
// macOS browsers hard-reserve ⌘[ and ⌘] for history Back/Forward and CONSUME the
// keystroke before the page's JS ever sees it — verified on real Safari/Chrome:
// only the bare `Meta` keydown reaches the document, never `BracketLeft/Right`,
// and the page (terminal column included) reloads. So they cannot be caught as
// keydowns and `preventDefault` is moot — the event never arrives.
//
// Instead we commandeer history navigation. We sandwich the app between two
// SAME-DOCUMENT history entries — [back, center, fwd] — and sit on `center`.
// A Back/Forward traversal (⌘[, ⌘], the toolbar buttons, or a two-finger swipe)
// then lands on a same-document entry, which the browser delivers as an in-page
// `popstate` with NO reload — and whose `state.s` tells us the direction. We map
// back→onBack, fwd→onForward, then immediately re-center. The Cloudflare Access
// redirect (and any real prior page) is pushed two entries away, so a single
// traversal can never leave the app.
//
// This is safe because lasso has no client-side router: it only uses
// `history.replaceState` for query-param state (see lib/url.ts) and has zero
// `popstate` listeners of its own, so Back/Forward were doing nothing useful.
// Validated on real macOS Chrome via the equivalent history.back()/forward().

type SandwichState = { s: "back" | "center" | "fwd" }

let wired = false
let backCb: () => void = () => {}
let fwdCb: () => void = () => {}

// installHistoryToggle builds the trap once (subsequent calls just refresh the
// callbacks, so React StrictMode's double-mount can't stack extra entries) and
// returns a no-op cleanup — the trap intentionally lives for the page lifetime.
export function installHistoryToggle(
  onBack: () => void,
  onForward: () => void
) {
  backCb = onBack
  fwdCb = onForward
  if (wired) return () => {}
  wired = true

  const here = location.href
  let settling = true
  let bouncing = false

  const onPop = (e: PopStateEvent) => {
    const s = (e.state as SandwichState | null)?.s
    // The go(-1) below (and our own re-centering go()s) generate popstates we
    // must ignore — only genuine back/fwd landings should toggle.
    if (settling) return
    if (bouncing) {
      bouncing = false
      return
    }
    if (s === "back") {
      bouncing = true
      history.go(1) // re-center
      backCb()
    } else if (s === "fwd") {
      bouncing = true
      history.go(-1) // re-center
      fwdCb()
    }
  }

  // [back, center, fwd], same href so traversal never reloads or shows a URL
  // change; sit on center.
  history.replaceState({ s: "back" } satisfies SandwichState, "", here)
  history.pushState({ s: "center" } satisfies SandwichState, "", here)
  history.pushState({ s: "fwd" } satisfies SandwichState, "", here)
  window.addEventListener("popstate", onPop)
  history.go(-1) // -> center (its popstate is swallowed while `settling`)
  // A tick after the go(-1) popstate lands, arm for real.
  setTimeout(() => {
    settling = false
  }, 150)

  return () => {}
}
