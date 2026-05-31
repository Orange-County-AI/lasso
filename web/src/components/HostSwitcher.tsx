import { Check, Laptop, Loader2, RefreshCw, Server } from "lucide-react"
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
import { getQueryParam, setQueryParam } from "@/lib/url"
import { cn } from "@/lib/utils"

// usable reports whether a host can be selected: reachable, herdr running, and
// protocol-compatible with this lasso.
function usable(h: HostInfo): boolean {
  return h.reachable && h.running && h.compatible
}

// HostSwitcher is a floating control pinned to the bottom-left corner (so it
// costs no layout space): it lets the app drive a herdr daemon on any
// compatible ssh-config host as if it were local. It names the active host —
// the local machine's hostname (laptop icon) or the remote alias (server icon,
// primary-tinted as a "you are elsewhere" cue). Incompatible/unreachable hosts
// are listed greyed-out with why.
export function HostSwitcher() {
  const { host: liveHost } = useApp()
  const [data, setData] = React.useState<HostsPayload | null>(null)
  const [loading, setLoading] = React.useState(false)
  const [switching, setSwitching] = React.useState(false)
  const [open, setOpen] = React.useState(false)

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

  return (
    <div className="absolute bottom-3 left-3 z-40">
      <DropdownMenu open={open} onOpenChange={setOpen}>
        <DropdownMenuTrigger
          disabled={switching}
          title={`Host: ${activeLabel} (click to switch)`}
          className={cn(
            "flex items-center gap-1.5 rounded-full border border-border bg-card/90 px-2 py-1 text-[13px] text-muted-foreground shadow-md backdrop-blur transition-colors hover:bg-accent hover:text-foreground disabled:opacity-60",
            // Tint when remote so it reads as an active "you are elsewhere" badge.
            isRemote && "border-primary/40 text-foreground"
          )}
        >
          {switching ? (
            <Loader2 className="size-3.5 animate-spin" />
          ) : isRemote ? (
            <Server className="size-3.5 text-primary" />
          ) : (
            <Laptop className="size-3.5" />
          )}
          <span className="max-w-32 truncate font-medium">{activeLabel}</span>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" side="top" className="min-w-56">
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
            return (
              <DropdownMenuItem
                key={h.alias}
                disabled={!ok}
                onSelect={() => ok && void switchTo(h.alias)}
                className={cn(!ok && "opacity-60")}
              >
                <Server className="size-3.5" />
                <span className="flex-1 truncate">{h.alias}</span>
                {ok ? (
                  <span className="text-[10px] text-muted-foreground">
                    {h.version}
                  </span>
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
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  )
}
