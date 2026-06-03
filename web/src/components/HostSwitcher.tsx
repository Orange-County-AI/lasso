import { useQuery } from "@tanstack/react-query"
import {
  ArrowUpCircle,
  Check,
  Download,
  Laptop,
  Loader2,
  RefreshCw,
  Server,
} from "lucide-react"
import * as React from "react"
import { toast } from "sonner"

import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { api, type HostInfo, type HostsPayload } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { qk } from "@/lib/query"
import { getQueryParam, setQueryParam } from "@/lib/url"
import { cn } from "@/lib/utils"

// usable reports whether a host can be selected: reachable, herdr running, and
// protocol-compatible with this lasso.
function usable(h: HostInfo): boolean {
  return h.reachable && h.running && h.compatible
}

// behind reports whether a reachable host runs an older herdr protocol than the
// local one — the case a remote `herdr update` can fix (an ahead host can't be
// helped by updating it; you'd update locally instead).
function behind(h: HostInfo, localProtocol: number): boolean {
  return (
    h.reachable &&
    h.running &&
    !h.compatible &&
    h.protocol > 0 &&
    localProtocol > 0 &&
    h.protocol < localProtocol
  )
}

// formatBehind renders how far the running lasso is behind main for the update
// tooltip — "3 commits"/"1 commit", or a vague "some commits" when the count
// didn't come through.
function formatBehind(n: number | undefined): string {
  if (!n || n < 1) return "some commits"
  return `${n} commit${n === 1 ? "" : "s"}`
}

// provisionable reports whether a reachable host has no herdr server running
// (missing entirely, or installed but stopped) — the case a fresh
// install-and-supervise (via pitchfork) can fix.
function provisionable(h: HostInfo): boolean {
  return h.reachable && !h.running
}

// HostSwitcher lets the app drive a herdr daemon on any compatible ssh-config
// host as if it were local. It names the active host — the local machine's
// hostname (laptop icon) or the remote alias (server icon, primary-tinted as a
// "you are elsewhere" cue). Incompatible/unreachable hosts are listed greyed-out
// with why. The "nav" variant sits inline in the left tab strip (left of the
// Herdr tab, menu opening downward); "floating" keeps the old pinned-pill look.
export function HostSwitcher({
  variant = "floating",
}: {
  variant?: "floating" | "nav"
}) {
  const isNav = variant === "nav"
  const { host: liveHost } = useApp()
  const [data, setData] = React.useState<HostsPayload | null>(null)
  const [loading, setLoading] = React.useState(false)
  const [switching, setSwitching] = React.useState(false)
  // Hosts with an update/provision in flight. A set (not a single alias) so
  // several remote actions can run at once — clicking a second host's button
  // while the first is still spinning starts it concurrently rather than being
  // ignored. Each endpoint shells out to its own ssh and the server handles them
  // on independent goroutines, so the only thing that ever serialized them was
  // this client-side state.
  const [busyHosts, setBusyHosts] = React.useState<ReadonlySet<string>>(
    () => new Set()
  )
  const [open, setOpen] = React.useState(false)
  const [updatingLasso, setUpdatingLasso] = React.useState(false)

  // This lasso build's version + whether it can self-update (a pitchfork-
  // supervised git checkout). Fetched while the menu is open.
  const versionQuery = useQuery({
    queryKey: qk.version,
    queryFn: () => api.version(),
    enabled: open,
  })

  // Prefer the live SSE host (reflects switches from anywhere); fall back to the
  // last /api/hosts snapshot, then "local".
  const active = liveHost ?? data?.active ?? "local"
  const isRemote = active !== "local"
  // The local host is shown by its machine hostname rather than the literal
  // "local" sentinel; the label for whatever host is active.
  const localLabel = data?.local?.hostname || "local"
  const activeLabel = isRemote ? active : localLabel

  const load = React.useCallback(async (refresh = false) => {
    setLoading(true)
    try {
      setData(await api.hosts(refresh))
    } catch (e) {
      toast.error(`Couldn't list hosts: ${(e as Error).message}`)
    } finally {
      setLoading(false)
    }
  }, [])

  // Load once on mount, and refresh the probe whenever the menu is opened (cheap
  // server-side cache absorbs rapid reopens).
  React.useEffect(() => {
    if (open) void load()
  }, [open, load])
  React.useEffect(() => {
    void load()
  }, [load])

  const switchTo = React.useCallback(
    async (alias: string) => {
      if (alias === active || switching) return
      setSwitching(true)
      const label = alias === "local" ? localLabel : alias
      try {
        await api.switchHost(alias)
        toast.success(`Switched to ${label}`)
      } catch (e) {
        toast.error(`Couldn't switch to ${label}: ${(e as Error).message}`)
      } finally {
        setSwitching(false)
      }
    },
    [active, switching, localLabel]
  )

  // Run a remote action on a host then re-probe so its row reflects the result.
  // "update" runs `herdr update` (host is behind; stops its old server/panes);
  // "provision" installs herdr + supervises it with pitchfork (host had none
  // running). Both are slow, but they run independently per host — several can be
  // in flight at once (the button only disables for the host already running).
  const runHostAction = React.useCallback(
    async (alias: string, kind: "update" | "provision") => {
      if (busyHosts.has(alias)) return
      setBusyHosts((prev) => new Set(prev).add(alias))
      const verb = kind === "update" ? "Update" : "Setup"
      try {
        const res =
          kind === "update"
            ? await api.updateHost(alias)
            : await api.provisionHost(alias)
        if (res.ok) {
          toast.success(
            kind === "update"
              ? `Updated herdr on ${alias}`
              : `Set up herdr on ${alias}`
          )
          void load(true) // re-probe; the host should now be selectable
        } else {
          if (res.output)
            console.error(`herdr ${kind} on ${alias}:\n${res.output}`)
          toast.error(
            `${verb} failed on ${alias}: ${res.error || "see console"}`
          )
        }
      } catch (e) {
        toast.error(`${verb} failed on ${alias}: ${(e as Error).message}`)
      } finally {
        setBusyHosts((prev) => {
          const next = new Set(prev)
          next.delete(alias)
          return next
        })
      }
    },
    [busyHosts, load]
  )

  // Update lasso itself: pull + rebuild + restart via the supervisor. The server
  // bounces a moment later, so the page reconnects on its own.
  const updateLasso = React.useCallback(async () => {
    if (updatingLasso) return
    setUpdatingLasso(true)
    try {
      await api.selfUpdate()
      toast.success("Updating lasso — it'll restart in a moment")
    } catch (e) {
      toast.error(`Couldn't update lasso: ${(e as Error).message}`)
      setUpdatingLasso(false) // on success we expect a restart, so leave it busy
    }
  }, [updatingLasso])

  // ?host=<alias> in the URL reflects the active host (omitted for local).
  // Captured once at mount so the deep-link below survives the reflect effect.
  const initialUrlHost = React.useRef(getQueryParam("host"))
  const deepLinkApplied = React.useRef(false)

  // Reflect the SSE-confirmed active host in the URL.
  React.useEffect(() => {
    if (liveHost == null) return
    setQueryParam("host", liveHost === "local" ? null : liveHost)
  }, [liveHost])

  // Deep link: if the URL named a host, switch to it once the host list has
  // loaded (so the server can validate the alias). Runs at most once.
  React.useEffect(() => {
    if (deepLinkApplied.current || !data) return
    deepLinkApplied.current = true
    const want = initialUrlHost.current
    if (want && want !== active) void switchTo(want)
  }, [data, active, switchTo])

  const remotes = data?.hosts ?? []

  const iconClass = isNav ? "size-3.5" : "size-3"
  return (
    <div className="relative">
      <DropdownMenu open={open} onOpenChange={setOpen}>
        <DropdownMenuTrigger
          disabled={switching}
          title={`Host: ${activeLabel} (click to switch)`}
          className={cn(
            isNav
              ? // Inline nav affordance: borderless, matching the tab strip.
                "flex shrink-0 items-center gap-1 self-center rounded-md px-2 py-1 text-[13px] text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:opacity-60"
              : "flex items-center gap-1 rounded-full border border-border bg-card/90 px-2 py-0.5 text-[11px] text-muted-foreground shadow-md backdrop-blur transition-colors hover:bg-accent hover:text-foreground disabled:opacity-60",
            // Tint when remote so it reads as an active "you are elsewhere" badge.
            isRemote &&
              (isNav ? "text-foreground" : "border-primary/40 text-foreground")
          )}
        >
          {switching ? (
            <Loader2 className={cn(iconClass, "animate-spin")} />
          ) : isRemote ? (
            <Server className={cn(iconClass, "text-primary")} />
          ) : (
            <Laptop className={iconClass} />
          )}
          {/* The nav variant is icon-only (host name lives in the menu + tooltip);
              the floating pill names the active host inline. */}
          {!isNav && (
            <span className="max-w-32 truncate font-medium">{activeLabel}</span>
          )}
        </DropdownMenuTrigger>
        <DropdownMenuContent
          align="start"
          side={isNav ? "bottom" : "top"}
          className="min-w-56"
        >
          <DropdownMenuLabel className="flex items-center justify-between">
            <span>Host</span>
            <button
              type="button"
              className="rounded p-0.5 hover:bg-accent hover:text-foreground"
              title="Re-scan ssh config"
              onClick={(e) => {
                e.preventDefault()
                void load(true)
              }}
            >
              <RefreshCw className={cn("size-3", loading && "animate-spin")} />
            </button>
          </DropdownMenuLabel>

          <DropdownMenuItem onSelect={() => void switchTo("local")}>
            <Laptop className="size-3.5" />
            <span className="flex-1 truncate">{localLabel}</span>
            {data?.local?.version && (
              <span className="text-[10px] text-muted-foreground">
                {data.local.version}
              </span>
            )}
            {active === "local" && <Check className="size-3.5" />}
          </DropdownMenuItem>

          {remotes.length > 0 && <DropdownMenuSeparator />}

          {remotes.map((h) => {
            const ok = usable(h)
            const canUpdate = !ok && behind(h, data?.local?.protocol ?? 0)
            const canProvision = !ok && !canUpdate && provisionable(h)
            const action = canUpdate
              ? ("update" as const)
              : canProvision
                ? ("provision" as const)
                : null
            const busy = busyHosts.has(h.alias)
            return (
              <DropdownMenuItem
                key={h.alias}
                disabled={!ok && !action}
                // Non-selectable hosts with an action keep the menu open so the
                // action button stays clickable instead of switching.
                onSelect={(e) =>
                  ok ? void switchTo(h.alias) : e.preventDefault()
                }
                className={cn(!ok && !action && "opacity-60")}
              >
                <Server className="size-3.5" />
                <span className="flex-1 truncate">{h.alias}</span>
                {ok ? (
                  <span className="text-[10px] text-muted-foreground">
                    {h.version}
                  </span>
                ) : action ? (
                  <button
                    type="button"
                    className="flex items-center gap-1 rounded border border-primary/40 px-1.5 py-0.5 text-[10px] text-primary hover:bg-accent disabled:opacity-60"
                    title={
                      action === "update"
                        ? `Run \`herdr update\` on ${h.alias} (protocol ${h.protocol} → ${data?.local?.protocol}; stops its running sessions)`
                        : `Install herdr on ${h.alias} and supervise it with pitchfork`
                    }
                    disabled={busy}
                    onClick={(e) => {
                      e.preventDefault()
                      e.stopPropagation()
                      void runHostAction(h.alias, action)
                    }}
                  >
                    {busy ? (
                      <Loader2 className="size-3 animate-spin" />
                    ) : action === "update" ? (
                      <RefreshCw className="size-3" />
                    ) : (
                      <Download className="size-3" />
                    )}
                    {busy
                      ? action === "update"
                        ? "updating…"
                        : "setting up…"
                      : action === "update"
                        ? "update"
                        : "set up"}
                  </button>
                ) : (
                  <span className="truncate text-[10px] text-warn">
                    {h.err || "unavailable"}
                  </span>
                )}
                {active === h.alias && <Check className="size-3.5" />}
              </DropdownMenuItem>
            )
          })}

          {!loading && remotes.length === 0 && (
            <div className="px-2 py-1.5 text-[11px] text-muted-foreground">
              No other hosts in ~/.ssh/config
            </div>
          )}

          {/* lasso itself: its version, and (on the supervised prod install) a
              self-update — the local-side counterpart to a remote `herdr update`
              when a host needs a newer lasso. */}
          <DropdownMenuSeparator />
          <div className="flex items-center gap-2 px-2 py-1.5 text-[11px] text-muted-foreground">
            <span className="flex-1 truncate">
              lasso{" "}
              <span className="font-mono">
                {versionQuery.data?.lasso_version ?? "…"}
              </span>
            </span>
            {versionQuery.data?.updatable &&
              // Show the button when behind main, or when we can't tell (keep the
              // escape hatch); show a quiet "up to date" check when on main's tip.
              (versionQuery.data.update_state === "current" ? (
                <span
                  className="flex items-center gap-1 text-[10px] text-muted-foreground"
                  title="lasso is built from the latest commit on main"
                >
                  <Check className="size-3" />
                  up to date
                </span>
              ) : (
                <button
                  type="button"
                  className="flex items-center gap-1 rounded border border-primary/40 px-1.5 py-0.5 text-[10px] text-primary hover:bg-accent disabled:opacity-60"
                  title={
                    versionQuery.data.update_state === "available"
                      ? `main is ${formatBehind(versionQuery.data.commits_behind)} ahead — pull, rebuild, and restart lasso (briefly disconnects)`
                      : "Pull the latest lasso, rebuild, and restart it (briefly disconnects)"
                  }
                  disabled={updatingLasso}
                  onClick={(e) => {
                    e.preventDefault()
                    e.stopPropagation()
                    void updateLasso()
                  }}
                >
                  {updatingLasso ? (
                    <Loader2 className="size-3 animate-spin" />
                  ) : (
                    <ArrowUpCircle className="size-3" />
                  )}
                  {updatingLasso ? "updating…" : "update lasso"}
                </button>
              ))}
          </div>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  )
}
