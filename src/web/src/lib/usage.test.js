import { describe, expect, test } from "bun:test"

import { pace } from "@/lib/usage"

const limit = (percent, elapsedPct) => ({
  label: "Weekly",
  percent,
  elapsedPct,
})

describe("pace", () => {
  test("flags usage running ahead of the window's clock", () => {
    expect(pace(limit(16, 10))).toEqual({
      elapsed: 10,
      ahead: true,
      projected: 160,
    })
    expect(pace(limit(40, 52)).ahead).toBe(false)
  })

  test("a just-reset window is never behind — nothing has elapsed yet", () => {
    // Codex's weekly quota minutes after its reset: 4% used, 0% elapsed. Any
    // usage beats a clock that hasn't started, so treating this as "ahead"
    // marks a fresh window as a problem.
    expect(pace(limit(4, 0))).toEqual({
      elapsed: 0,
      ahead: false,
      projected: null,
    })
  })

  test("an unknown window length has no pace at all", () => {
    expect(pace(limit(80, -1))).toEqual({
      elapsed: -1,
      ahead: false,
      projected: null,
    })
  })

  test("withholds the projection until a tenth of the window has passed", () => {
    // Ahead of pace either way; in the first tenth of a window one burst
    // extrapolates to nonsense, so only the later reading projects.
    expect(pace(limit(12, 9)).projected).toBeNull()
    expect(pace(limit(12, 10)).projected).toBe(120)
  })

  test("clamps a projection an early burst would blow up", () => {
    expect(pace(limit(99, 10)).projected).toBe(990)
    expect(pace(limit(100, 10)).projected).toBe(999)
  })
})
