import { describe, expect, test } from "bun:test"

import { watchDialConditions } from "@/lib/mobile-input-dial"

// A MediaQueryList stand-in, flipped by hand: the real pair is the iframe's
// `(pointer: coarse)` and the parent's `(max-width: 767px)`, and neither can be
// driven from a test without a browser.
const query = (matches = false) => {
  const listeners = new Set()
  return {
    matches,
    listeners,
    addEventListener: (_type, fn) => listeners.add(fn),
    removeEventListener: (_type, fn) => listeners.delete(fn),
    set(next) {
      this.matches = next
      for (const fn of listeners) fn()
    },
  }
}

// An attach that records how many dials are live, which is the thing the
// watcher must get right: one, or none.
const counter = () => {
  const state = { attached: 0, released: 0 }
  return {
    state,
    attach: () => {
      state.attached++
      return () => {
        state.released++
      }
    },
  }
}

describe("watchDialConditions", () => {
  test("mounts when a window narrows past md, with no touchscreen in sight", () => {
    const touch = query(false)
    const narrow = query(false)
    const { state, attach } = counter()

    watchDialConditions([touch, narrow], attach)
    expect(state.attached).toBe(0) // wide desktop: the footer is the chrome

    narrow.set(true)
    expect(state.attached).toBe(1)
    expect(state.released).toBe(0)
  })

  test("one dial, not one per condition", () => {
    const touch = query(true)
    const narrow = query(true)
    const { state, attach } = counter()

    watchDialConditions([touch, narrow], attach)
    // A phone matches both. Attaching per condition would build two dials in
    // one document and leave one with nobody holding its teardown.
    expect(state.attached).toBe(1)

    touch.set(false) // keyboard case attached, still a narrow window
    expect(state.released).toBe(0)
    expect(state.attached).toBe(1)

    narrow.set(false)
    expect(state.released).toBe(1)

    narrow.set(true)
    expect(state.attached).toBe(2) // re-mounted, not resurrected
  })

  test("teardown releases the dial and stops watching", () => {
    const touch = query(false)
    const narrow = query(true)
    const { state, attach } = counter()

    const dispose = watchDialConditions([touch, narrow], attach)
    dispose()
    expect(state.released).toBe(1)
    expect(touch.listeners.size).toBe(0)
    expect(narrow.listeners.size).toBe(0)

    // A flip arriving after the frame was released must not build a dial in a
    // document nobody is holding a teardown for.
    touch.set(true)
    narrow.set(false)
    expect(state.attached).toBe(1)
  })
})
