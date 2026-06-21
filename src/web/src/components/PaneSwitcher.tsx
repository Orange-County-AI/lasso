import { useQuery } from "@tanstack/react-query"
import * as React from "react"
import { toast } from "sonner"

import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { api, type GridPane } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { tilde } from "@/lib/format"
import { focusPaneInHerdr } from "@/lib/pane-focus"
import { qk } from "@/lib/query"
import { focusHerdrTerminal } from "@/lib/terminal"
import { cn } from "@/lib/utils"

// cellKey uniquely identifies a pane across hosts (pane ids are only unique
// within a host) — also the Grid's identity formula.
const cellKey = (p: GridPane) => `${p.host}|${p.pane_id}`

// The descriptive pane title shown bold at the top of each row. Prefer the
// workspace label (e.g. "accessibility") over the bare herdr tab number.
const primaryLabel = (p: GridPane) =>
  p.workspace_label || p.tab_label || p.pane_id

// The most specific name *below* the workspace, shown as a badge to tell
// sibling panes apart. Prefer the pane's own label (herdr's per-pane title);
// fall back to the tab label — which, for an unnamed pane, is the name herdr
// shows on its tab. "" when neither adds anything over the primary label.
const detailLabel = (p: GridPane) => {
  const detail = p.pane_label || p.tab_label
  return detail && detail !== primaryLabel(p) ? detail : ""
}

// Everything worth matching against, lowercased and joined. A query token is a
// hit if it's a substring anywhere in here.
const haystack = (p: GridPane) =>
  [
    p.tab_label,
    p.pane_label,
    p.workspace_label,
    p.host_label,
    p.host,
    p.agent,
    p.cwd,
    p.pane_id,
    p.prompt, // full initial prompt — searchable but not shown in the list
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
  termWasFocused = false,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onFocusInHerdr: () => void
  // Whether the herdr terminal held keyboard focus when the palette opened. On a
  // cancel-close we re-focus its xterm so Esc'ing out of ⌘K leaves the keyboard
  // where it was, rather than stranding it on the (unfocusable-for-typing) iframe.
  termWasFocused?: boolean
}) {
  const { host: activeHost } = useApp()
  const [query, setQuery] = React.useState("")
  const [active, setActive] = React.useState(0)
  const listRef = React.useRef<HTMLDivElement>(null)
  // Set when a pane is chosen so the close handler can tell a pick (choose()
  // already hands focus to the pane's terminal) from a cancel.
  const chosenRef = React.useRef(false)

  // Shares the Grid tab's query cache (same key), so opening right after viewing
  // the Grid is instant; otherwise it fetches on open.
  const q = useQuery({
    queryKey: qk.grid,
    queryFn: () => api.gridPanes(),
    enabled: open,
  })
  const panes = q.data?.panes ?? [] // backend order = newest first (mirrors Grid)

  // Workspaces holding more than one pane — the only ones where the shared
  // workspace label is ambiguous and each row needs its more-specific name
  // (its tab or pane label) to tell siblings apart. This covers both split
  // panes in one tab and panes spread across several tabs. Computed off the
  // full set (not the filtered view) so the search query never flips a badge
  // on or off.
  const multiPaneWorkspaces = React.useMemo(() => {
    const countByWs = new Map<string, number>()
    for (const p of panes) {
      if (!p.workspace_id) continue
      countByWs.set(p.workspace_id, (countByWs.get(p.workspace_id) ?? 0) + 1)
    }
    const multi = new Set<string>()
    for (const [ws, n] of countByWs) if (n > 1) multi.add(ws)
    return multi
  }, [panes])

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
    chosenRef.current = true
    onOpenChange(false)
    // Close first so the Dialog doesn't re-grab focus on unmount — then hand the
    // keyboard to the pane's terminal.
    focusPaneInHerdr(p, activeHost, onFocusInHerdr).catch((e) =>
      toast.error(`focus failed: ${(e as Error).message}`)
    )
  }

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault()
      onOpenChange(false)
    } else if (e.key === "ArrowDown") {
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
        onCloseAutoFocus={(e) => {
          // A chosen pane already had focus handed to its terminal by choose();
          // leave it. On a cancel (Esc / click-away), Radix restores focus to
          // whatever held it before the palette opened — but when that was the
          // herdr terminal iframe, focusing the <iframe> element doesn't reach
          // the xterm inside, so the user would have to click the pane to type
          // again. Re-focus its xterm directly instead.
          if (chosenRef.current) {
            chosenRef.current = false
            return
          }
          if (termWasFocused) {
            e.preventDefault()
            focusHerdrTerminal()
          }
        }}
      >
        <DialogHeader className="sr-only">
          <DialogTitle>Find a pane</DialogTitle>
        </DialogHeader>
        <input
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="Search panes by tab, workspace, host, agent, path, or prompt…"
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
                  // The keyboard/hover highlight uses the primary tint so the
                  // active row reads clearly — `bg-accent` resolves to --h-hover,
                  // the same color as the dialog surface (DialogContent is
                  // bg-popover, also --h-hover), so the highlight was
                  // imperceptible and ↑/↓ navigation looked like it did nothing.
                  i === active && "bg-primary text-primary-foreground"
                )}
              >
                <span className="flex w-full items-center gap-2">
                  <span className="truncate font-bold text-sm">
                    {primaryLabel(p)}
                  </span>
                  {/* When a workspace holds several panes, the bold workspace
                      label is shared, so each row surfaces its more-specific
                      name (pane label, else tab label) to tell siblings apart —
                      the same name herdr shows on the pane/tab. */}
                  {p.workspace_id &&
                    multiPaneWorkspaces.has(p.workspace_id) &&
                    detailLabel(p) && (
                      <span
                        className={cn(
                          "shrink-0 rounded px-1.5 py-0.5 font-medium text-[11px]",
                          // On the active (bg-primary) row the foreground tints
                          // wash out, so swap to primary-foreground tints.
                          i === active
                            ? "bg-primary-foreground/20 text-primary-foreground"
                            : "bg-foreground/10 text-foreground/70"
                        )}
                      >
                        {detailLabel(p)}
                      </span>
                    )}
                  {p.has_agent && p.agent && (
                    <span
                      className={cn(
                        "shrink-0 rounded px-1.5 py-0.5 font-medium text-[11px]",
                        // text-primary on a bg-primary row is invisible — use
                        // the contrasting primary-foreground when active.
                        i === active
                          ? "bg-primary-foreground/20 text-primary-foreground"
                          : "bg-primary/15 text-primary"
                      )}
                    >
                      {p.agent}
                      {p.agent_status ? ` · ${p.agent_status}` : ""}
                    </span>
                  )}
                </span>
                <span
                  className={cn(
                    "flex w-full items-center gap-2 truncate text-xs",
                    // The muted gray subtitle is unreadable on the active row;
                    // ride the row's primary-foreground at reduced opacity.
                    i === active
                      ? "text-primary-foreground/80"
                      : "text-muted-foreground"
                  )}
                >
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
