import * as React from "react"

// Card-reorder animation for the agents grid, by the FLIP method: measure
// where each card WAS, let the layout change, then start it at its old place
// and let it transition home.
//
// It has to be FLIP rather than a CSS transition because reordering a grid
// changes which grid slot an item occupies, and a grid slot is not an
// animatable property — nothing transitions, the card teleports. The
// alternative, a view transition, snapshots the whole grid and suppresses
// input for its duration, which is the wrong trade on a surface where every
// card holds a live composer someone may be typing into.
//
// The animation is a transform on the card's own element, so React keeps the
// same DOM node: focus, caret and an unsent draft all survive the move. That
// is the point — a priority resort moving a card out from under a reader's
// hands is exactly what this is here to make legible.

const DURATION = 220
const EASE = "cubic-bezier(.2,.7,.3,1)"

type Pos = { x: number; y: number }

// The card's position WITHIN its own grid container, not in the viewport and
// not in the scroll root. Two reasons, both bugs seen on the way here:
// getBoundingClientRect is viewport-relative and these measurements straddle a
// paint, so any scroll in between lands in the delta and the cards fly; and an
// offset taken against the positioned root picks up the header wrapping to a
// second line, which would slide every card at once on the next reorder.
function posIn(el: HTMLElement, parent: HTMLElement): Pos {
  return {
    x: el.offsetLeft - parent.offsetLeft,
    y: el.offsetTop - parent.offsetTop,
  }
}

// How far a card is currently displaced by an animation still in flight. A
// reorder landing mid-flight would otherwise animate from the card's LAYOUT
// position, which is not where the reader can see it, and the card would jump
// before it slid.
function inflight(el: HTMLElement): Pos {
  const t = getComputedStyle(el).transform
  if (!t || t === "none") return { x: 0, y: 0 }
  try {
    const m = new DOMMatrixReadOnly(t)
    return { x: m.e, y: m.f }
  } catch {
    return { x: 0, y: 0 }
  }
}

/**
 * useFlip returns a ref registrar: call it with a stable id per item and spread
 * the result onto that item's element. `orderKey` is what tells the hook a
 * reorder may have happened — it re-measures only when that changes, so the
 * grid's per-agent transcript polls (N renders every few seconds) cost no
 * layout reads at all.
 */
export function useFlip(orderKey: string) {
  const nodes = React.useRef(new Map<string, HTMLElement>())
  const prev = React.useRef(new Map<string, Pos>())
  // Ref callbacks are cached per id: a fresh closure each render would make
  // React detach and re-attach every card's ref on every keystroke.
  const refs = React.useRef(new Map<string, (el: HTMLElement | null) => void>())
  // Nothing animates into the first layout. Fading twenty cards in when the
  // view opens is a load screen, not a transition.
  const measured = React.useRef(false)

  const register = React.useCallback((id: string) => {
    const cached = refs.current.get(id)
    if (cached) return cached
    const fn = (el: HTMLElement | null) => {
      if (el) nodes.current.set(id, el)
      else {
        nodes.current.delete(id)
        prev.current.delete(id)
      }
    }
    refs.current.set(id, fn)
    return fn
  }, [])

  // Re-measure without animating whenever the grid itself is resized (a window
  // drag, the sidebar handle). The cards move, but not because anything
  // reordered — recording the new positions is what stops the NEXT reorder
  // from animating out of coordinates that stopped being true.
  // biome-ignore lint/correctness/useExhaustiveDependencies: orderKey is not read here — it is the signal that the SET of grid containers may have changed, which is when the observer has to be rebuilt.
  React.useEffect(() => {
    if (typeof ResizeObserver === "undefined") return
    const seen = new Set<HTMLElement>()
    const ro = new ResizeObserver(() => {
      for (const [id, el] of nodes.current) {
        const parent = el.parentElement
        if (parent) prev.current.set(id, posIn(el, parent))
      }
    })
    for (const el of nodes.current.values()) {
      const parent = el.parentElement
      if (parent && !seen.has(parent)) {
        seen.add(parent)
        ro.observe(parent)
      }
    }
    return () => ro.disconnect()
  }, [orderKey])

  // biome-ignore lint/correctness/useExhaustiveDependencies: orderKey is the trigger, not an input — the effect exists to RUN when the card order changes.
  React.useLayoutEffect(() => {
    const reduce =
      typeof window.matchMedia === "function" &&
      window.matchMedia("(prefers-reduced-motion: reduce)").matches
    const next = new Map<string, Pos>()
    // [element, from-x, from-y, fading-in]
    const play: [HTMLElement, number, number, boolean][] = []
    for (const [id, el] of nodes.current) {
      const parent = el.parentElement
      if (!parent) continue
      const now = posIn(el, parent)
      next.set(id, now)
      const was = prev.current.get(id)
      if (!was) {
        // A card with no prior position is new (created, or revealed by a
        // cleared filter). It has nowhere to slide from, so it fades in.
        if (measured.current && !reduce) play.push([el, 0, 0, true])
        continue
      }
      const held = inflight(el)
      const dx = was.x + held.x - now.x
      const dy = was.y + held.y - now.y
      if (dx !== 0 || dy !== 0) play.push([el, dx, dy, false])
    }
    prev.current = next
    const first = !measured.current
    measured.current = true
    // A card that LEFT is already unmounted by the time this runs, so there is
    // no exit animation — keeping a removed card mounted to fade it out would
    // mean keeping a dead agent's composer on screen, which is worse than a
    // card that simply goes.
    if (first || reduce || play.length === 0) return

    for (const [el, dx, dy, fading] of play) {
      el.style.transition = "none"
      if (fading) {
        el.style.opacity = "0"
        el.style.transform = "scale(0.97)"
      } else {
        el.style.transform = `translate(${dx}px, ${dy}px)`
      }
    }
    // The inverted position has to be COMMITTED before the transition is
    // armed, or the browser coalesces both writes into one style recalc and
    // the card goes straight to its destination. A forced reflow is what
    // commits it. A single requestAnimationFrame does not: the callback runs
    // inside the same frame's rendering steps, before the style the layout
    // effect just wrote has been recalculated, so the transition has no start
    // value to animate from — measured here as a resort finishing in ~80ms of
    // a 220ms transition.
    void play[0][0].offsetWidth
    for (const [el, , , fading] of play) {
      el.style.transition = `transform ${DURATION}ms ${EASE}${
        fading ? `, opacity ${DURATION}ms ease-out` : ""
      }`
      el.style.transform = ""
      if (fading) el.style.opacity = ""
    }
  }, [orderKey])

  return register
}
