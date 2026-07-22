import { useQuery } from "@tanstack/react-query"
import { Fragment } from "react"
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { api, type UsageLimit, type UsageProvider } from "@/lib/api"
import { qk } from "@/lib/query"
import { cn } from "@/lib/utils"

// UsageFooter — a slim status bar pinned to the bottom of the app that surfaces
// subscription usage limits (Claude Code / Kimi Code / Codex), the same numbers
// the `clui` TUI shows. Deliberately text-only and one line tall: the goal is to
// keep the user aware of their quotas without spending screen real estate.
//
// Each metric is `LABEL [pace-bar] nn%`. The pace bar's key idea is *velocity*:
// the bar fills to your usage %, with a notch at how far through the period you
// are. Usage that spills past the notch (drawn red) means you're burning faster
// than the clock and will hit the cap before it resets. Hover any metric for the
// reset time and the pace read-out.

// Poll on the same cadence clui uses (60s). The backend caches for ~25s so
// multiple tabs don't multiply upstream calls.
function useUsage() {
  return useQuery({
    queryKey: qk.usage,
    queryFn: () => api.usage(),
    refetchInterval: 60_000,
    staleTime: 30_000,
  })
}

// clui's thresholds for the % text: normal < 70%, amber 70–89%, red ≥ 90%.
function pctClass(percent: number): string {
  if (percent >= 90) return "text-bad"
  if (percent >= 70) return "text-warn"
  return "text-foreground"
}

// Compact all-caps tag for a limit: "5-Hour Block" → "5H", "Fable Weekly" →
// "FABLE", "Weekly Limit" → "WK". Falls back to the raw label upper-cased.
function shortLabel(label: string): string {
  const l = label.toLowerCase()
  if (l.includes("5-hour") || l === "session" || l.startsWith("5h")) return "5H"
  if (l.includes("7-day")) return "7D"
  if (l.endsWith(" limit")) {
    // "5h Limit" / "45m Limit" / "Weekly Limit"
    const base = label.slice(0, -" Limit".length)
    return /weekly/i.test(base) ? "WK" : base.toUpperCase()
  }
  if (l.endsWith(" weekly")) {
    const scope = label.slice(0, -" Weekly".length)
    return scope.toLowerCase() === "scoped" ? "WK" : scope.toUpperCase()
  }
  if (l === "weekly" || l === "week") return "WK"
  return label.toUpperCase()
}

function pad(n: number): string {
  return n < 10 ? `0${n}` : `${n}`
}

// Reset time formatted client-side against the current time so it stays live
// between polls. Mirrors clui's phrasing.
function resetText(limit: UsageLimit): string {
  if (!limit.resetsAt) return ""
  const reset = new Date(limit.resetsAt)
  if (Number.isNaN(reset.getTime())) return ""
  const now = new Date()

  if (limit.countdown) {
    const mins = Math.max(
      0,
      Math.round((reset.getTime() - now.getTime()) / 60000)
    )
    return mins < 60
      ? `Resets in ${mins}m`
      : `Resets in ${Math.floor(mins / 60)}h ${mins % 60}m`
  }
  const t = `${pad(reset.getHours())}:${pad(reset.getMinutes())}`
  const dayDiff = Math.floor(
    (new Date(reset).setHours(0, 0, 0, 0) - new Date().setHours(0, 0, 0, 0)) /
      86400000
  )
  if (dayDiff <= 0) return `Resets ${t}`
  if (dayDiff === 1) return `Resets Tomorrow ${t}`
  if (dayDiff < 7)
    return `Resets ${reset.toLocaleDateString(undefined, { weekday: "short" })} ${t}`
  return `Resets ${reset.toLocaleDateString(undefined, { month: "short", day: "numeric" })} ${t}`
}

// Human read-out of pace for the tooltip: compares usage against how far through
// the period we are and, when over pace, projects where usage lands at reset.
function paceText(limit: UsageLimit): string {
  const e = limit.elapsedPct
  if (e < 0) return ""
  if (limit.percent > e + 1 && e > 0) {
    const projected = Math.round((limit.percent / e) * 100)
    return (
      `Ahead of pace — ${limit.percent}% used at ${e}% through the period` +
      (projected > 100 ? ` (on track for ~${projected}% by reset)` : "")
    )
  }
  return `${e}% through the period — on pace`
}

// PaceBar — usage fill with a time notch; the stretch of fill past the notch is
// drawn red (you're consuming faster than the period elapses). When the window
// length is unknown (elapsed < 0) it degrades to a plain usage bar.
function PaceBar({ percent, elapsed }: { percent: number; elapsed: number }) {
  const p = Math.max(0, Math.min(100, percent))
  const e = elapsed >= 0 ? Math.max(0, Math.min(100, elapsed)) : -1
  const onPace = e >= 0 ? Math.min(p, e) : p
  const over = e >= 0 ? Math.max(0, p - e) : 0
  return (
    <span className="relative inline-block h-[7px] w-9 shrink-0 overflow-hidden rounded-[2px] bg-border align-middle">
      <span
        className="absolute inset-y-0 left-0 bg-foreground/70"
        style={{ width: `${onPace}%` }}
      />
      {over > 0 ? (
        <span
          className="absolute inset-y-0 bg-[var(--h-bad)]"
          style={{ left: `${onPace}%`, width: `${over}%` }}
        />
      ) : null}
      {e >= 0 ? (
        <span
          className="absolute inset-y-0 w-px bg-foreground"
          style={{ left: `${e}%` }}
          aria-hidden
        />
      ) : null}
    </span>
  )
}

function LimitChip({ limit }: { limit: UsageLimit }) {
  const reset = resetText(limit)
  const pace = paceText(limit)
  const chip = (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap">
      <span className="text-muted-foreground">{shortLabel(limit.label)}</span>
      <PaceBar percent={limit.percent} elapsed={limit.elapsedPct} />
      <span className={cn("tabular-nums", pctClass(limit.percent))}>
        {limit.percent}%
      </span>
    </span>
  )
  if (!reset && !pace) return chip
  return (
    <Tooltip>
      <TooltipTrigger asChild>{chip}</TooltipTrigger>
      <TooltipContent
        side="top"
        className="max-w-64 font-sans text-xs normal-case tracking-normal"
      >
        <div className="font-medium">{limit.label}</div>
        {reset ? <div className="text-muted-foreground">{reset}</div> : null}
        {pace ? <div className="text-muted-foreground">{pace}</div> : null}
      </TooltipContent>
    </Tooltip>
  )
}

function ProviderGroup({ provider }: { provider: UsageProvider }) {
  return (
    <div className="inline-flex items-center gap-3.5 whitespace-nowrap">
      {/* Provider name is the bright anchor of each block; the metric labels
          inside stay muted so the eye can find where one provider ends and the
          next begins. */}
      <span className="font-medium text-foreground">{provider.name}</span>
      {provider.err ? (
        <span className="text-muted-foreground/40">—</span>
      ) : (
        provider.limits.map((limit) => (
          <LimitChip key={limit.label} limit={limit} />
        ))
      )}
    </div>
  )
}

export function UsageFooter() {
  const { data, isError } = useUsage()

  // Nothing to show (no configured providers, or the whole fetch failed) — stay
  // out of the way rather than render an empty bar.
  const providers = data?.providers ?? []
  if (isError || providers.length === 0) return null

  return (
    <TooltipProvider delayDuration={200}>
      {/* The footer is the scroll viewport; the inner row is `w-max` so it sizes
          to its content — `mx-auto` then centers it when it fits and collapses
          the margins (letting it scroll left/right) when it's wider than the
          screen. `no-scrollbar` keeps the slim bar from growing a scrollbar. */}
      <footer className="no-scrollbar flex-none overflow-x-auto border-border border-t bg-card">
        <div className="mx-auto flex w-max items-center px-4 py-1 font-label text-[11px] uppercase tracking-wider">
          {providers.map((p, i) => (
            <Fragment key={p.name}>
              {i > 0 ? (
                <span
                  className="mx-5 h-4 w-px flex-none bg-muted-foreground/50"
                  aria-hidden
                />
              ) : null}
              <ProviderGroup provider={p} />
            </Fragment>
          ))}
        </div>
      </footer>
    </TooltipProvider>
  )
}
