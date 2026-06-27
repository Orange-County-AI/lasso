import { useQuery } from "@tanstack/react-query"
import * as React from "react"
import { toast } from "sonner"

import { Checkbox } from "@/components/ui/checkbox"
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
import { qk, queryClient } from "@/lib/query"
import { focusHerdrTerminal } from "@/lib/terminal"
import { cn } from "@/lib/utils"

// cellKey uniquely identifies a row. Live panes are keyed by host+pane_id (pane
// ids are only unique within a host) — the Grid's identity formula. Closed rows
// have no live pane: recorded agents key by their record id, orphan worktree/
// scratch dirs (no record, no pane) key by their host+cwd.
const cellKey = (p: GridPane) => {
  if (p.closed && p.agent_id) return `agent|${p.host}|${p.agent_id}`
  if (p.closed && p.cwd) return `dir|${p.host}|${p.cwd}`
  return `${p.host}|${p.pane_id}`
}

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
  // "Active" filter — on by default, so the switcher shows only live panes (its
  // historical behavior). Turning it off folds in past agents whose herdr pane was
  // closed, so an old session can be found and its workspace reopened. Reset to on
  // every time the modal opens (see the open effect below).
  const [activeOnly, setActiveOnly] = React.useState(true)
  const listRef = React.useRef<HTMLDivElement>(null)
  // Tracks what last moved the highlight. We only auto-scroll the active row into
  // view for keyboard nav: doing it on pointer-driven changes re-snaps the list on
  // every hover, which on touch fights a drag-scroll so the list feels stuck.
  const navSource = React.useRef<"keyboard" | "pointer">("keyboard")
  // The search input — focus returns here after toggling the Active filter so the
  // user can keep typing without clicking back into it.
  const inputRef = React.useRef<HTMLInputElement>(null)
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
  const livePanes = q.data?.panes ?? [] // backend order = newest first (mirrors Grid)

  // Past agents (every one lasso spawned). Only fetched when the Active filter is
  // off, since that's the only mode that surfaces closed ones.
  const hist = useQuery({
    queryKey: qk.agentHistory,
    queryFn: () => api.agentHistory(),
    enabled: open && !activeOnly,
  })

  // Closed sessions = history rows whose herdr pane is no longer live: recorded
  // agents and orphan worktree/scratch dirs alike. Diff against the live set by
  // host+pane_id (a still-running agent already shows as its live pane) and by
  // host+cwd (an orphan dir that's currently open is already a live pane), so
  // nothing is listed twice.
  const closedAgents = React.useMemo(() => {
    if (activeOnly) return [] as GridPane[]
    const livePaneIds = new Set(livePanes.map((p) => `${p.host}|${p.pane_id}`))
    const liveCwds = new Set(
      livePanes.filter((p) => p.cwd).map((p) => `${p.host}|${p.cwd}`)
    )
    return (hist.data?.agents ?? [])
      .filter((a) => !livePaneIds.has(`${a.host}|${a.pane_id}`))
      .filter((a) => !a.cwd || !liveCwds.has(`${a.host}|${a.cwd}`))
      .map((a) => ({ ...a, closed: true }))
  }, [activeOnly, livePanes, hist.data])

  // Closed rows go after the live ones (newest live panes first, then history).
  const panes = activeOnly ? livePanes : [...livePanes, ...closedAgents]

  // Workspaces holding more than one pane — the only ones where the shared
  // workspace label is ambiguous and each row needs its more-specific name
  // (its tab or pane label) to tell siblings apart. This covers both split
  // panes in one tab and panes spread across several tabs. Computed off the
  // full set (not the filtered view) so the search query never flips a badge
  // on or off.
  const multiPaneWorkspaces = React.useMemo(() => {
    const countByWs = new Map<string, number>()
    for (const p of livePanes) {
      if (!p.workspace_id) continue
      countByWs.set(p.workspace_id, (countByWs.get(p.workspace_id) ?? 0) + 1)
    }
    const multi = new Set<string>()
    for (const [ws, n] of countByWs) if (n > 1) multi.add(ws)
    return multi
  }, [livePanes])

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

  // Each time the modal opens, start fresh: clear the query and reset the Active
  // filter to on (so it always defaults to live-panes-only, never inheriting a
  // prior session's "show closed" choice).
  React.useEffect(() => {
    if (open) {
      setQuery("")
      setActiveOnly(true)
    }
  }, [open])

  // Keep the highlighted row scrolled into view — but only when the highlight
  // moved via the keyboard. On pointer/touch it would re-snap the list on every
  // hover and block drag-scrolling.
  React.useEffect(() => {
    if (!open || navSource.current !== "keyboard") return
    listRef.current
      ?.querySelector<HTMLElement>(`[data-index="${active}"]`)
      ?.scrollIntoView({ block: "nearest" })
  }, [active, open])

  const choose = (p: GridPane) => {
    chosenRef.current = true
    onOpenChange(false)
    // Close first so the Dialog doesn't re-grab focus on unmount — then hand the
    // keyboard to the pane's terminal.
    if (p.closed && (p.agent_id || p.cwd)) {
      // A past session with no live pane: re-create its workspace at the work dir,
      // then focus the fresh pane. Refresh both lists so the row flips from closed
      // to live. The agent itself isn't relaunched (start claude yourself). Recorded
      // agents reopen by agent_id; orphan worktree/scratch dirs reopen by path.
      const body = p.agent_id ? { agent_id: p.agent_id } : { work_dir: p.cwd }
      api
        .reopenAgent(p.host, body)
        .then((np) => {
          queryClient.invalidateQueries({ queryKey: qk.grid })
          queryClient.invalidateQueries({ queryKey: qk.agentHistory })
          return focusPaneInHerdr(np, activeHost, onFocusInHerdr)
        })
        .catch((e) => toast.error(`reopen failed: ${(e as Error).message}`))
      return
    }
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
      navSource.current = "keyboard"
      setActive((a) => Math.min(a + 1, filtered.length - 1))
    } else if (e.key === "ArrowUp") {
      e.preventDefault()
      navSource.current = "keyboard"
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
          ref={inputRef}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="Search panes by tab, workspace, host, agent, path, or prompt…"
          className="w-full bg-transparent px-4 py-3 text-sm outline-none placeholder:text-muted-foreground"
        />
        {/* Active filter: on shows only live panes (default); off folds in past
            agents whose pane was closed, so old sessions can be reopened. */}
        <label className="flex cursor-pointer select-none items-center justify-end gap-2 border-border border-b px-4 pb-2 text-muted-foreground text-xs">
          <Checkbox
            // The shared checkbox's focus-visible ring doesn't render here, so give
            // it an explicit outline so keyboard users can see it's focused (it's
            // reachable via Tab from the search box).
            className="focus-visible:outline-2 focus-visible:outline-primary focus-visible:outline-offset-2 focus-visible:[outline-style:solid]"
            checked={activeOnly}
            onCheckedChange={(c) => setActiveOnly(c === true)}
            onKeyDown={(e) => {
              // Radix checkboxes toggle on Space, not Enter (per WAI-ARIA). Honor
              // Enter too, and keep focus on the toggle so a second Enter flips it
              // again instead of falling through to the input's Enter (= open the
              // selected pane and close the modal).
              if (e.key === "Enter") {
                e.preventDefault()
                setActiveOnly((v) => !v)
              }
            }}
            onClick={(e) => {
              // Only a real mouse click (detail > 0) hands focus back to the search
              // box so the user can keep typing. Keyboard activation keeps focus on
              // the toggle (Space's synthetic click has detail 0; Enter is handled
              // above and never fires a click).
              if (e.detail > 0) inputRef.current?.focus()
            }}
          />
          Active
        </label>
        <div ref={listRef} className="max-h-80 overflow-y-auto p-1">
          {filtered.length === 0 ? (
            <div className="px-3 py-6 text-center text-muted-foreground text-sm">
              {q.isLoading || hist.isLoading
                ? "Loading…"
                : "No matching panes."}
            </div>
          ) : (
            filtered.map((p, i) => (
              <button
                key={cellKey(p)}
                type="button"
                data-index={i}
                onClick={() => choose(p)}
                onMouseMove={() => {
                  navSource.current = "pointer"
                  setActive(i)
                }}
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
                  {/* Closed agents (no live pane) read distinctly so it's clear
                      selecting one reopens its workspace rather than focusing a
                      running pane. */}
                  {p.closed && (
                    <span
                      className={cn(
                        "shrink-0 rounded px-1.5 py-0.5 font-medium text-[11px]",
                        i === active
                          ? "bg-primary-foreground/20 text-primary-foreground"
                          : "bg-foreground/10 text-muted-foreground"
                      )}
                    >
                      closed
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
