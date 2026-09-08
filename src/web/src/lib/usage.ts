import type { UsageLimit } from "@/lib/api"

// Pace math, shared by the usage footer (the glance) and the Usage tab (the
// detail) so both read one quota window the same way: usage past the elapsed
// share of the window means you're burning faster than the clock and will hit
// the cap before it resets.
//
// `elapsed` is -1 when the window length is unknown (the backend couldn't
// derive it) — there is then nothing to compare usage against. A window with
// 0% elapsed is the same case for a different reason: it has just reset, so
// any usage at all is trivially "ahead" of a clock that has not started, which
// would flag a freshly-reset quota as a problem (Codex's weekly window, seen
// minutes after its reset).
//
// `projected` is the naive linear landing at reset (percent ÷ elapsed share),
// withheld in the first tenth of a window where a single burst extrapolates to
// nonsense, and clamped so a 4%-elapsed spike doesn't render five digits.
export function pace(limit: UsageLimit): {
  elapsed: number
  ahead: boolean
  projected: number | null
} {
  const elapsed = limit.elapsedPct
  if (elapsed < 0) return { elapsed: -1, ahead: false, projected: null }
  const ahead = elapsed > 0 && limit.percent > elapsed
  const projected =
    ahead && elapsed >= 10
      ? Math.min(999, Math.round((limit.percent / elapsed) * 100))
      : null
  return { elapsed, ahead, projected }
}
