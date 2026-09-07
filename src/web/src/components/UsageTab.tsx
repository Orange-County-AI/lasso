import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { ChevronDown, ChevronRight, RotateCw } from "lucide-react"
import * as React from "react"
import { toast } from "sonner"
import { Pill } from "@/components/Pill"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  api,
  type UsageBarModule,
  type UsageLimit,
  type UsageProvider,
} from "@/lib/api"
import { qk } from "@/lib/query"
import { cn } from "@/lib/utils"

// UsageTab — the rich view of subscription usage limits. The Luvus Bar widget
// (modules/usage-bar) is the glance; this is the detail: every window with its
// bar, the pace notch (elapsed share of the window), the projected landing when
// usage is ahead of the clock, and live reset countdowns. The section at the
// bottom edits the module's own settings, so what the bar shows is configured
// in exactly one place.

const REFRESH_MS = 60_000

function useUsage(active: boolean) {
  return useQuery({
    queryKey: qk.usage,
    queryFn: () => api.usage(),
    enabled: active,
    refetchInterval: active ? REFRESH_MS : false,
    staleTime: 20_000,
  })
}

// Live clock so countdowns tick between polls.
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

// Ahead of pace = usage past the elapsed share of the window. The projection
// is the naive linear landing at reset (percent ÷ elapsed share), withheld in
// the first tenth of a window where one burst extrapolates to nonsense.
function pace(limit: UsageLimit): {
  ahead: boolean
  projected: number | null
} {
  const e = limit.elapsedPct
  if (e < 0) return { ahead: false, projected: null }
  const ahead = limit.percent > e
  const projected =
    ahead && e >= 10
      ? Math.min(999, Math.round((limit.percent / e) * 100))
      : null
  return { ahead, projected }
}

function tone(limit: UsageLimit): "good" | "warn" | "bad" {
  if (limit.percent >= 100) return "bad"
  if (limit.percent >= 90 || pace(limit).ahead) return "warn"
  return "good"
}

const barToneClass = { good: "bg-good", warn: "bg-warn", bad: "bg-bad" }

// LimitRow: label · bar (fill + pace notch) · % · reset, with the pace line.
function LimitRow({ limit, now }: { limit: UsageLimit; now: number }) {
  const t = tone(limit)
  const { ahead, projected } = pace(limit)
  const notch = limit.elapsedPct >= 0 ? limit.elapsedPct : null
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
          {limit.percent}%{ahead ? "▲" : ""}
        </span>
        <span className="w-32 shrink-0 truncate text-muted-foreground text-xs">
          {resetText(limit, now)}
        </span>
      </div>
      {notch != null && (
        <div className="pl-30 text-[11px] text-muted-foreground">
          {ahead
            ? `ahead of pace — ${notch}% of the window elapsed${projected != null ? `, ~${projected}% at reset` : ""}`
            : `on track — ${notch}% of the window elapsed`}
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
  stale,
}: {
  provider: UsageProvider
  now: number
  stale: boolean
}) {
  const [open, setOpen] = React.useState(true)
  const w = worst(provider)
  const anyAhead = provider.limits.some((l) => pace(l).ahead)
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

// Provider keys in the module's settings ↔ the names the backend reports.
const BAR_PROVIDERS: { key: string; name: string }[] = [
  { key: "claude", name: "Claude Code" },
  { key: "kimi", name: "Kimi Code" },
  { key: "codex", name: "Codex" },
  { key: "zai", name: "Z.ai" },
]

function BarSettings({ module }: { module: UsageBarModule }) {
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: ({ key, value }: { key: string; value: unknown }) =>
      api.setUsageBarSetting(key, value),
    onSuccess: (m) => queryClient.setQueryData(qk.usageBar, m),
    onError: (e: Error) => toast.error(`Couldn't update the bar: ${e.message}`),
  })
  if (!module.linked) {
    return (
      <p className="text-muted-foreground text-xs">
        The Luvus Bar widget is not linked on this host. Run{" "}
        <code className="rounded bg-muted px-1">
          luvus module link &lt;lasso&gt;/modules/usage-bar
        </code>{" "}
        to show these limits in Luvus's status row.
      </p>
    )
  }
  const s = module.settings ?? {}
  const compact = s.compact === true
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
        <span className="text-muted-foreground">Shown in the Luvus Bar</span>
        {BAR_PROVIDERS.map(({ key, name }) => {
          const id = `usage-bar-show-${key}`
          const setting = `show-${key}`
          return (
            <label
              key={key}
              htmlFor={id}
              className="flex cursor-pointer select-none items-center gap-1.5"
            >
              <Checkbox
                id={id}
                checked={s[setting] !== false}
                disabled={mutation.isPending || !module.enabled}
                onCheckedChange={(c) =>
                  mutation.mutate({ key: setting, value: c === true })
                }
              />
              {name}
            </label>
          )
        })}
      </div>
      <div className="flex items-center gap-2 text-xs">
        <span className="text-muted-foreground">Bar layout</span>
        <div className="inline-flex w-fit gap-0.5 rounded-lg border border-border p-0.5">
          {[
            { compact: false, label: "Standard" },
            { compact: true, label: "Compact" },
          ].map((layout) => (
            <button
              key={layout.label}
              type="button"
              aria-pressed={compact === layout.compact}
              disabled={mutation.isPending || !module.enabled}
              onClick={() =>
                mutation.mutate({ key: "compact", value: layout.compact })
              }
              className={cn(
                "rounded-md px-2 py-0.5 text-xs transition-colors",
                compact === layout.compact
                  ? "bg-primary text-primary-foreground"
                  : "text-muted-foreground hover:text-foreground"
              )}
            >
              {layout.label}
            </button>
          ))}
        </div>
        {!module.enabled && (
          <span className="text-warn">module disabled in Luvus</span>
        )}
      </div>
      <p className="text-[11px] text-muted-foreground">
        Order and placement: Settings → Modules → Lasso usage, and Settings →
        Layout → Luvus Bar, in Luvus. Standard shows every window with a bar and
        reset; Luvus falls back to Compact on its own when the row is narrow.
      </p>
    </div>
  )
}

export function UsageTab({ active }: { active: boolean }) {
  const queryClient = useQueryClient()
  const usage = useUsage(active)
  const bar = useQuery({
    queryKey: qk.usageBar,
    queryFn: () => api.usageBar(),
    enabled: active,
    // Luvus is the source of truth and can be edited from its own Settings
    // screen, so re-read on every show of the tab and on the usage poll.
    staleTime: 0,
    refetchInterval: active ? REFRESH_MS : false,
  })
  const now = useNow(30_000)
  const updatedAgo = usage.data
    ? fmtDuration(now - new Date(usage.data.updatedAt).getTime())
    : null

  const providers = usage.data?.providers ?? []
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
          <RotateCw className={cn(usage.isFetching && "animate-spin")} />
        </Button>
      </div>

      {usage.data && providers.length === 0 && (
        <p className="text-muted-foreground text-xs">
          No providers with credentials on this host. lasso reads Claude Code,
          Kimi Code, Codex and Z.ai's own credential files.
        </p>
      )}
      {providers.map((p) => (
        <ProviderCard
          key={p.name}
          provider={p}
          now={now}
          stale={Boolean(p.err && p.limits.length > 0)}
        />
      ))}

      <div className="mt-auto border-border border-t pt-3">
        {bar.data ? (
          <BarSettings module={bar.data} />
        ) : (
          <span className="text-muted-foreground text-xs">
            {bar.isError ? "Luvus module state unavailable" : "…"}
          </span>
        )}
      </div>
    </div>
  )
}
