import { useQuery } from "@tanstack/react-query"
import * as React from "react"
import { toast } from "sonner"

import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { type GridPane, api } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { tilde } from "@/lib/format"
import { focusPaneInHerdr } from "@/lib/pane-focus"
import { qk } from "@/lib/query"
import { cn } from "@/lib/utils"

// cellKey uniquely identifies a pane across hosts (pane ids are only unique
// within a host) — also the Grid's identity formula.
const cellKey = (p: GridPane) => `${p.host}|${p.pane_id}`

const primaryLabel = (p: GridPane) =>
  p.tab_label || p.workspace_label || p.pane_id

// Everything worth matching against, lowercased and joined. A query token is a
// hit if it's a substring anywhere in here.
const haystack = (p: GridPane) =>
  [
    p.tab_label,
    p.workspace_label,
    p.host_label,
    p.host,
    p.agent,
    p.cwd,
    p.pane_id,
  ]
    .filter(Boolean)
    .join(" ")
    .toLowerCase()

// PaneSwitcher: a Cmd+U command-palette over every herdr pane on every host.
// Type to filter; ↑/↓ to move; Enter to open + focus the pane in the Herdr tab
// (handing the keyboard straight to its terminal). Unlike the Grid's display
// filters, this always searches the full pane set across all hosts.
export function PaneSwitcher({
  open,
  onOpenChange,
  onFocusInHerdr,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onFocusInHerdr: () => void
}) {
  const { host: activeHost } = useApp()
  const [query, setQuery] = React.useState("")
  const [active, setActive] = React.useState(0)
  const listRef = React.useRef<HTMLDivElement>(null)

  // Shares the Grid tab's query cache (same key), so opening right after viewing
  // the Grid is instant; otherwise it fetches on open.
  const q = useQuery({
    queryKey: qk.grid,
    queryFn: () => api.gridPanes(),
    enabled: open,
  })
  const panes = q.data?.panes ?? [] // backend order = newest first (mirrors Grid)

  const filtered = React.useMemo(() => {
    const tokens = query.trim().toLowerCase().split(/\s+/).filter(Boolean)
    if (tokens.length === 0) return panes
    return panes.filter((p) => {
      const h = haystack(p)
      return tokens.every((t) => h.includes(t))
    })
  }, [panes, query])

  // Reset the highlight to the top whenever the query changes.
  // biome-ignore lint/correctness/useExhaustiveDependencies: reset on query change
  React.useEffect(() => {
    setActive(0)
  }, [query])

  // Clear the query each time the modal opens so it starts fresh.
  React.useEffect(() => {
    if (open) setQuery("")
  }, [open])

  // Keep the highlighted row scrolled into view.
  React.useEffect(() => {
    if (!open) return
    listRef.current
      ?.querySelector<HTMLElement>(`[data-index="${active}"]`)
      ?.scrollIntoView({ block: "nearest" })
  }, [active, open])

  const choose = (p: GridPane) => {
    onOpenChange(false)
    // Close first so the Dialog doesn't re-grab focus on unmount — then hand the
    // keyboard to the pane's terminal.
    focusPaneInHerdr(p, activeHost, onFocusInHerdr).catch((e) =>
      toast.error(`focus failed: ${(e as Error).message}`)
    )
  }

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault()
      setActive((a) => Math.min(a + 1, filtered.length - 1))
    } else if (e.key === "ArrowUp") {
      e.preventDefault()
      setActive((a) => Math.max(a - 1, 0))
    } else if (e.key === "Enter") {
      e.preventDefault()
      const p = filtered[active]
      if (p) choose(p)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        showCloseButton={false}
        className="top-[15%] translate-y-0 gap-0 p-0 sm:max-w-lg"
        onOpenAutoFocus={(e) => {
          // Focus the search input rather than the first row.
          e.preventDefault()
          ;(e.currentTarget as HTMLElement | null)
            ?.querySelector("input")
            ?.focus()
        }}
      >
        <DialogHeader className="sr-only">
          <DialogTitle>Find a pane</DialogTitle>
        </DialogHeader>
        <input
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="Search panes by tab, workspace, host, agent, or path…"
          className="w-full border-border border-b bg-transparent px-4 py-3 text-sm outline-none placeholder:text-muted-foreground"
        />
        <div ref={listRef} className="max-h-80 overflow-y-auto p-1">
          {filtered.length === 0 ? (
            <div className="px-3 py-6 text-center text-muted-foreground text-sm">
              {q.isLoading ? "Loading panes…" : "No matching panes."}
            </div>
          ) : (
            filtered.map((p, i) => (
              <button
                key={cellKey(p)}
                type="button"
                data-index={i}
                onClick={() => choose(p)}
                onMouseMove={() => setActive(i)}
                className={cn(
                  "flex w-full flex-col items-start gap-0.5 rounded-md px-3 py-2 text-left outline-none",
                  i === active && "bg-accent text-accent-foreground"
                )}
              >
                <span className="flex w-full items-center gap-2">
                  <span className="truncate font-medium text-sm">
                    {primaryLabel(p)}
                  </span>
                  {p.has_agent && p.agent && (
                    <span className="shrink-0 rounded bg-primary/15 px-1.5 py-0.5 font-medium text-[11px] text-primary">
                      {p.agent}
                      {p.agent_status ? ` · ${p.agent_status}` : ""}
                    </span>
                  )}
                </span>
                <span className="flex w-full items-center gap-2 truncate text-muted-foreground text-xs">
                  <span className="shrink-0">{p.host_label}</span>
                  {p.cwd && (
                    <span className="truncate font-mono">{tilde(p.cwd)}</span>
                  )}
                </span>
              </button>
            ))
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}
