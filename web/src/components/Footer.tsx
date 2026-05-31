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
import { cn } from "@/lib/utils"

// usable reports whether a host can be selected: reachable, herdr running, and
// protocol-compatible with this lasso.
function usable(h: HostInfo): boolean {
  return h.reachable && h.running && h.compatible
}

// Footer is the bottom row: a host switcher that lets the app drive a herdr
// daemon on any compatible ssh-config host as if it were local. Incompatible or
// unreachable hosts are shown greyed-out with the reason.
export function Footer() {
  const { host: liveHost } = useApp()
  const [data, setData] = React.useState<HostsPayload | null>(null)
  const [loading, setLoading] = React.useState(false)
  const [switching, setSwitching] = React.useState(false)
  const [open, setOpen] = React.useState(false)

  // Prefer the live SSE host (reflects switches from anywhere); fall back to the
  // last /api/hosts snapshot, then "local".
  const active = liveHost ?? data?.active ?? "local"

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
      const label = alias === "local" ? "local host" : alias
      try {
        await api.switchHost(alias)
        toast.success(`Switched to ${label}`)
      } catch (e) {
        toast.error(`Couldn't switch to ${label}: ${(e as Error).message}`)
      } finally {
        setSwitching(false)
      }
    },
    [active, switching]
  )

  const remotes = data?.hosts ?? []

  return (
    <footer className="flex h-7 flex-none items-center gap-2 border-border border-t bg-card px-2 text-muted-foreground text-xs">
      <DropdownMenu open={open} onOpenChange={setOpen}>
        <DropdownMenuTrigger
          disabled={switching}
          className="flex items-center gap-1.5 rounded px-1.5 py-0.5 hover:bg-accent hover:text-foreground disabled:opacity-60"
          title="Switch host"
        >
          {switching ? (
            <Loader2 className="size-3.5 animate-spin" />
          ) : active === "local" ? (
            <Laptop className="size-3.5" />
          ) : (
            <Server className="size-3.5" />
          )}
          <span className="font-medium text-foreground">{active}</span>
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
            <span className="flex-1">local</span>
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
    </footer>
  )
}
