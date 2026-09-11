import { useQuery, useQueryClient } from "@tanstack/react-query"
import { ChevronDown, ChevronRight, RotateCw } from "lucide-react"
import * as React from "react"
import { Pill } from "@/components/Pill"
import { Button } from "@/components/ui/button"
import { Orb } from "@/components/ui/orb"
import {
  api,
  completeUsageProviderOrder,
  USAGE_PROVIDER_NAMES,
  type UsageLimit,
  type UsageProvider,
} from "@/lib/api"
import { qk } from "@/lib/query"
import { useUIState } from "@/lib/ui-state"
import { pace } from "@/lib/usage"
import { cn } from "@/lib/utils"

// UsageTab — the rich view of subscription usage limits. The footer is the
// glance (one line, worst-first, text-only); this is the detail: every window
// with its own bar, the pace notch (the elapsed share of the window), the
// projected landing when usage runs ahead of the clock, and live reset
// countdowns.
//
// Both read the same /api/usage under one react-query key, so opening the tab
// costs no extra upstream call — the backend also caches for ~25s.
//
// Provider order and the tracked set are Settings → Usage tracking's, shared
// with the footer: an unchecked provider is one lasso doesn't poll at all, so
// it has no numbers to show here either. Filtered client-side as well because
// the server's payload can be up to one cache TTL behind an unchecked box.

const REFRESH_MS = 60_000

// Live clock so the countdowns tick between polls.
function useNow(tickMs: number) {
  const [now, setNow] = React.useState(() => Date.now())
  React.useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), tickMs)
    return () => clearInterval(id)
  }, [tickMs])
  return now
}

function fmtDuration(ms: number): string {
  const m = Math.max(0, Math.round(ms / 60_000))
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h}h ${String(m % 60).padStart(2, "0")}m`
  return `${Math.floor(h / 24)}d ${h % 24}h`
}

function resetText(limit: UsageLimit, now: number): string {
  if (!limit.resetsAt) return ""
  const at = new Date(limit.resetsAt).getTime()
  if (Number.isNaN(at)) return ""
  const delta = at - now
  if (delta <= 0) return "resetting…"
  if (limit.countdown || delta < 48 * 3_600_000) {
    return `resets in ${fmtDuration(delta)}`
  }
  return `resets ${new Date(at).toLocaleString(undefined, {
    weekday: "short",
    hour: "2-digit",
    minute: "2-digit",
  })}`
}

function tone(limit: UsageLimit): "good" | "warn" | "bad" {
  if (limit.percent >= 100) return "bad"
  if (limit.percent >= 90 || pace(limit).ahead) return "warn"
  return "good"
}

const barToneClass = { good: "bg-good", warn: "bg-warn", bad: "bg-bad" }

// LimitRow — `label · bar (fill + pace notch) · %` on the first line, with the
// reset and the pace read-out beneath it. The reset deliberately does NOT sit
// in a fixed column beside the bar: the sidebar is routinely 390px wide (a
// phone, or a narrow split on the desktop), where three fixed columns starve
// the bar down to a stub — the one element that has to be readable at a glance.
function LimitRow({ limit, now }: { limit: UsageLimit; now: number }) {
  const t = tone(limit)
  const { elapsed, ahead, projected } = pace(limit)
  const notch = elapsed >= 0 ? elapsed : null
  const reset = resetText(limit, now)
  const paceNote =
    notch == null
      ? ""
      : ahead
        ? `ahead of pace — ${notch}% of the window elapsed${projected != null ? `, ~${projected}% at reset` : ""}`
        : `on track — ${notch}% of the window elapsed`
  return (
    <div className="flex flex-col gap-0.5">
      <div className="flex items-center gap-2 text-[13px]">
        <span className="w-28 shrink-0 truncate text-muted-foreground">
          {limit.label}
        </span>
        <div
          className="relative h-2 min-w-0 flex-1 overflow-hidden rounded-sm bg-muted"
          role="progressbar"
          aria-valuenow={limit.percent}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-label={limit.label}
        >
          <div
            className={cn("h-full", barToneClass[t])}
            style={{ width: `${Math.min(100, limit.percent)}%` }}
          />
          {notch != null && (
            <div
              className="absolute top-0 h-full w-px bg-foreground/70"
              style={{ left: `${Math.min(100, notch)}%` }}
              title={`${notch}% of the window has elapsed`}
            />
          )}
        </div>
        <span
          className={cn(
            "w-14 shrink-0 text-right font-label tabular-nums",
            t === "bad" && "text-bad",
            t === "warn" && "text-warn"
          )}
        >
          {limit.percent}%{ahead ? " ▲" : ""}
        </span>
      </div>
      {(reset || paceNote) && (
        <div className="pl-30 text-[11px] text-muted-foreground">
          {[reset, paceNote].filter(Boolean).join(" · ")}
        </div>
      )}
    </div>
  )
}

function worst(p: UsageProvider): UsageLimit | undefined {
  return p.limits.reduce<UsageLimit | undefined>(
    (best, l) => (best == null || l.percent > best.percent ? l : best),
    undefined
  )
}

function ProviderCard({
  provider,
  now,
}: {
  provider: UsageProvider
  now: number
}) {
  const [open, setOpen] = React.useState(true)
  const w = worst(provider)
  const anyAhead = provider.limits.some((l) => pace(l).ahead)
  // A provider that reports an error AND numbers is showing the last good
  // reading the server kept through a rate-limit or timeout.
  const stale = Boolean(provider.err && provider.limits.length > 0)
  const Chevron = open ? ChevronDown : ChevronRight
  return (
    <section className="rounded-lg border border-border">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center gap-2 px-3 py-2 text-left hover:bg-accent/40"
        aria-expanded={open}
      >
        <Chevron className="size-3.5 text-muted-foreground" />
        <span className="font-medium text-[13px]">{provider.name}</span>
        {provider.plan && (
          <span className="text-muted-foreground text-xs">
            · {provider.plan}
          </span>
        )}
        <span className="ml-auto flex items-center gap-1.5">
          {provider.err && (
            <Pill tone="warn" title={provider.err}>
              {stale ? "cached" : "unavailable"}
            </Pill>
          )}
          {w && (
            <Pill tone={tone(w)}>
              {w.percent}%{anyAhead ? " ▲" : ""}
            </Pill>
          )}
        </span>
      </button>
      {open && (
        <div className="flex flex-col gap-2 border-border border-t px-3 py-2">
          {provider.err && (
            <p className="text-warn text-xs">
              {provider.err}
              {stale ? " — showing the last successful reading." : ""}
            </p>
          )}
          {provider.limits.map((l) => (
            <LimitRow key={l.label} limit={l} now={now} />
          ))}
        </div>
      )}
    </section>
  )
}

// The tracked providers in the user's chosen order. Anything the payload
// carries that the order doesn't know about (a provider a newer backend added)
// keeps its backend position at the end rather than disappearing.
function tracked(
  providers: UsageProvider[],
  order: readonly string[],
  hidden: readonly string[]
): UsageProvider[] {
  const rank = (name: string) => {
    const i = order.indexOf(name)
    return i < 0 ? order.length : i
  }
  return providers
    .filter((p) => !hidden.includes(p.name))
    .map((p, i) => ({ p, i }))
    .sort((a, b) => rank(a.p.name) - rank(b.p.name) || a.i - b.i)
    .map(({ p }) => p)
}

export function UsageTab({ active }: { active: boolean }) {
  const queryClient = useQueryClient()
  const usage = useQuery({
    queryKey: qk.usage,
    queryFn: () => api.usage(),
    enabled: active,
    refetchInterval: active ? REFRESH_MS : false,
    staleTime: 20_000,
  })
  const ui = useUIState()
  const order = completeUsageProviderOrder(ui.usage_order)
  const hidden = ui.usage_hidden ?? []
  const now = useNow(30_000)
  const updatedAgo = usage.data
    ? fmtDuration(now - new Date(usage.data.updatedAt).getTime())
    : null

  const providers = tracked(usage.data?.providers ?? [], order, hidden)
  return (
    <div className="flex h-full flex-col gap-3 overflow-y-auto p-3">
      <div className="flex items-center gap-2">
        <span className="font-medium text-sm">Usage limits</span>
        <span className="ml-auto text-muted-foreground text-xs">
          {usage.isError
            ? "couldn't reach lasso"
            : updatedAgo != null
              ? `refreshed ${updatedAgo} ago`
              : "loading…"}
        </span>
        <Button
          variant="outline"
          size="icon"
          className="size-7"
          title="Refresh usage limits"
          disabled={usage.isFetching}
          onClick={() => queryClient.invalidateQueries({ queryKey: qk.usage })}
        >
          {usage.isFetching ? <Orb state="working" px={16} /> : <RotateCw />}
        </Button>
      </div>

      {usage.data && providers.length === 0 && (
        <p className="text-muted-foreground text-xs">
          {hidden.length >= USAGE_PROVIDER_NAMES.length
            ? "Every provider is switched off in Settings → Usage tracking."
            : "No providers with credentials on this host. lasso reads Claude Code, Kimi Code, Codex and Z.ai's own credential files."}
        </p>
      )}
      {providers.map((p) => (
        <ProviderCard key={p.name} provider={p} now={now} />
      ))}
    </div>
  )
}
