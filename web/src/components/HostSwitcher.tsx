import { useQuery } from "@tanstack/react-query"
import { Check, Laptop, RefreshCw, Server } from "lucide-react"
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
import { api } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { qk, queryClient } from "@/lib/query"
import { cn } from "@/lib/utils"

// HostSwitcher picks which host lasso drives. The local box plus every reachable,
// tmux-capable ssh-config host is selectable; unreachable / tmux-less hosts are
// shown greyed with their reason. Switching changes the active backend (file
// browser, diff, repo picker, sidebar tree, new-agent target) and reloads the
// terminal viewport onto the new host (via the term_rev SSE bump).
export function HostSwitcher() {
  const { host: liveHost } = useApp()
  const hostsQuery = useQuery({
    queryKey: qk.hosts,
    queryFn: () => api.hosts(),
    staleTime: 20_000,
  })
  const [switching, setSwitching] = React.useState(false)

  const payload = hostsQuery.data
  // The active host follows the SSE stream once it lands; fall back to the
  // /api/hosts payload before the first event.
  const active = liveHost || payload?.active || "local"
  const isRemote = active !== "local" && active !== payload?.hostname
  const activeLabel =
    active === "local" ? (payload?.hostname ?? "local") : active

  const switchTo = async (host: string) => {
    if (host === active) return
    setSwitching(true)
    try {
      await api.switchHost(host)
      // The host list's `active` is now stale; the tree/grid/creator queries are
      // re-scoped by the term_rev handler (invalidateHostScoped).
      void queryClient.invalidateQueries({ queryKey: qk.hosts })
    } catch (e) {
      toast.error(`switch failed: ${(e as Error).message}`)
    } finally {
      setSwitching(false)
    }
  }

  const remoteHosts = payload?.hosts ?? []

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <button
          type="button"
          title={`active host: ${activeLabel} — click to switch`}
          className={cn(
            "ml-1 flex shrink-0 items-center gap-1.5 rounded border border-border px-2 py-1 text-[13px] hover:border-primary hover:text-primary",
            isRemote ? "text-primary" : "text-muted-foreground"
          )}
        >
          {switching ? (
            <RefreshCw className="size-3.5 animate-spin" />
          ) : isRemote ? (
            <Server className="size-3.5" />
          ) : (
            <Laptop className="size-3.5" />
          )}
          <span className="max-w-[10ch] truncate">{activeLabel}</span>
        </button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="min-w-52">
        <DropdownMenuLabel className="flex items-center justify-between">
          Hosts
          <button
            type="button"
            title="re-probe hosts"
            className="text-muted-foreground hover:text-primary"
            onClick={(e) => {
              e.preventDefault()
              void api
                .hosts(true)
                .then((d) => queryClient.setQueryData(qk.hosts, d))
            }}
          >
            <RefreshCw className="size-3" />
          </button>
        </DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => switchTo("local")}>
          <Laptop className="size-3.5" />
          <span className="flex-1">{payload?.hostname ?? "local"}</span>
          {active === "local" && <Check className="size-3.5" />}
        </DropdownMenuItem>
        {remoteHosts.length > 0 && <DropdownMenuSeparator />}
        {remoteHosts.map((h) => {
          const usable = h.reachable && h.has_tmux
          return (
            <DropdownMenuItem
              key={h.alias}
              disabled={!usable}
              onSelect={() => usable && switchTo(h.alias)}
              title={h.err || h.tmux_version || ""}
            >
              <Server className="size-3.5" />
              <span className="flex-1 truncate">{h.alias}</span>
              {active === h.alias ? (
                <Check className="size-3.5" />
              ) : (
                !usable && (
                  <span className="text-[10px] text-muted-foreground">
                    {h.err || "unavailable"}
                  </span>
                )
              )}
            </DropdownMenuItem>
          )
        })}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
