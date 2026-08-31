import { useQuery } from "@tanstack/react-query"
import { Laptop, Server, X } from "lucide-react"
import * as React from "react"
import { toast } from "sonner"

import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { api, type HostPane } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { tilde } from "@/lib/format"
import { aliasFamilies, familyOf, LOCAL_KEY } from "@/lib/hosts"
import { focusPaneInHerdr } from "@/lib/pane-focus"
import { qk, queryClient } from "@/lib/query"
import { focusHerdrTerminal } from "@/lib/terminal"
import { cn } from "@/lib/utils"

// cellKey uniquely identifies a row. Live panes are keyed by host+pane_id (pane
// ids are only unique within a host). Closed rows have no live pane: recorded
// agents key by their record id, orphan worktree/scratch dirs (no record, no
// pane) key by their host+cwd.
const cellKey = (p: HostPane) => {
  if (p.closed && p.agent_id) return `agent|${p.host}|${p.agent_id}`
  if (p.closed && p.cwd) return `dir|${p.host}|${p.cwd}`
  return `${p.host}|${p.pane_id}`
}

// originHost names the machine a row's work actually lives on. For a
// herdr-mirror row that is NOT p.host: the mirror is a real local pane
// streaming another box's terminal, so it reports host "local" while everything
// it shows is elsewhere. "" for the machine lasso itself runs on.
const originHost = (p: HostPane) =>
  p.mirror_host || (p.host === "local" ? "" : p.host)

// The descriptive pane title shown bold at the top of each row. Prefer the
// workspace label (e.g. "accessibility") over the bare herdr tab number. A
// mirror's workspace label carries herdr-mirror's "<host>: " sidebar prefix
// ("ocai: clem"), which the host header above the row already says — so it
// shows the label as it reads on the remote instead.
const primaryLabel = (p: HostPane) =>
  p.mirror_label || p.workspace_label || p.tab_label || p.pane_id

// The most specific name *below* the workspace, shown as a badge to tell
// sibling panes apart. Prefer the pane's own label (herdr's per-pane title);
// fall back to the tab label — which, for an unnamed pane, is the name herdr
// shows on its tab. "" when neither adds anything over the primary label.
const detailLabel = (p: HostPane) => {
  const detail = p.pane_label || p.tab_label
  return detail && detail !== primaryLabel(p) ? detail : ""
}

// Everything worth matching against, lowercased and joined. A query token is a
// hit if it's a substring anywhere in here.
//
// A mirror row is searchable by its host AND its remote label independently
// ("gigachad lasso" and "clem" both find their row), which the prefixed
// workspace label alone would only manage while herdr-mirror's prefix happens
// to equal its host key — it is configurable, so it can't be relied on. Its
// cwd is deliberately left out: every mirror pane on the box reports the same
// herdr-mirror streamer directory, so including it would make one stray token
// match all 32 of them.
const haystack = (p: HostPane) =>
  [
    p.tab_label,
    p.pane_label,
    p.workspace_label,
    p.mirror_host,
    p.mirror_label,
    p.host_label,
    p.host,
    p.agent,
    p.mirror_host ? "" : p.cwd,
    p.pane_id,
    p.prompt, // full initial prompt — searchable but not shown in the list
  ]
    .filter(Boolean)
    .join(" ")
    .toLowerCase()

// One host's rows in the list, with the index its first row occupies in the
// flattened set the keyboard navigates.
interface PaneSection {
  key: string
  label: string
  local: boolean
  start: number
  panes: HostPane[]
}

// PaneSwitcher: a ⌘K command-palette over the panes of the ACTIVE host — which,
// with herdr-mirror running, is the whole fleet anyway: every other machine's
// workspaces are mirrored into this herdr as real local panes. Type to filter;
// ↑/↓ to move; Enter to open + focus the pane in the herdr terminal (handing
// the keyboard straight to its xterm).
//
// It deliberately drops the other hosts' rows /api/all-panes also carries. A
// mirrored pane would otherwise be listed twice — once as the local mirror,
// once as the remote pane it streams — and picking the remote copy switches
// lasso's active host (a seconds-long terminal reload) to show what the mirror
// row already had on screen. Nothing is lost from view: the rows still bucket by
// the machine their work is on (see sections), so the fleet reads as the fleet.
export function PaneSwitcher({
  open,
  onOpenChange,
  termWasFocused = false,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
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

  // The host whose panes are listed. activeHost is null only until the first
  // /api/active answer lands, and lasso boots on the local machine, so "local"
  // is the right assumption for that instant.
  const hostKey = activeHost ?? "local"

  // Shares the prefetched cross-host pane cache (same key), so the palette is
  // usually instant; otherwise it fetches on open. The request stays the
  // cross-host aggregation rather than one host's listing: its per-host success
  // branch is also what reconciles lasso's agent records against herdr
  // (fetchAllPanes → reconcileHostAgents), and this endpoint is the only thing
  // that drives it on a schedule. The narrowing happens here.
  const q = useQuery({
    queryKey: qk.panes,
    queryFn: () => api.allPanes(),
    enabled: open,
  })
  // The active host's live panes, mirrors included — a mirror IS a local pane
  // (it reports host "local" and names the machine it streams in mirror_host).
  const livePanes = React.useMemo(
    // backend order = newest first
    () => (q.data?.panes ?? []).filter((p) => p.host === hostKey),
    [q.data, hostKey]
  )

  // Past agents (every one lasso spawned). Only fetched when the Active filter is
  // off, since that's the only mode that surfaces closed ones.
  const hist = useQuery({
    queryKey: qk.agentHistory,
    queryFn: () => api.agentHistory(),
    enabled: open && !activeOnly,
  })

  // Closed sessions = history rows whose herdr pane is no longer live: recorded
  // agents and orphan worktree/scratch dirs alike. Scoped to the active host
  // like the live rows (the history endpoint spans every host), then diffed
  // against the live set by host+pane_id (a still-running agent already shows as
  // its live pane) and by host+cwd (an orphan dir that's currently open is
  // already a live pane), so nothing is listed twice.
  const closedAgents = React.useMemo(() => {
    if (activeOnly) return [] as HostPane[]
    const livePaneIds = new Set(livePanes.map((p) => `${p.host}|${p.pane_id}`))
    const liveCwds = new Set(
      livePanes.filter((p) => p.cwd).map((p) => `${p.host}|${p.cwd}`)
    )
    return (hist.data?.agents ?? [])
      .filter((a) => a.host === hostKey)
      .filter((a) => !livePaneIds.has(`${a.host}|${a.pane_id}`))
      .filter((a) => !a.cwd || !liveCwds.has(`${a.host}|${a.cwd}`))
      .map((a) => ({ ...a, closed: true }))
  }, [activeOnly, hostKey, livePanes, hist.data])

  // Closed rows go after the live ones (newest live panes first, then closed
  // agents newest-first — see /api/agent-history, which orders by creation time
  // descending).
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

  // Whichever fetch the current mode depends on failed, if either did. The
  // history query only counts when the Active filter is off — it isn't even
  // enabled otherwise.
  const loadError =
    (q.error as Error | null)?.message ||
    (!activeOnly && (hist.error as Error | null)?.message) ||
    ""

  const matched = React.useMemo(() => {
    const tokens = query.trim().toLowerCase().split(/\s+/).filter(Boolean)
    if (tokens.length === 0) return panes
    return panes.filter((p) => {
      const h = haystack(p)
      return tokens.every((t) => h.includes(t))
    })
  }, [panes, query])

  // The alias families to group on — computed off the FULL set (like
  // multiPaneWorkspaces) so typing a query never re-buckets the rows underneath
  // the cursor. Mirror host keys sit in the same namespace as ssh aliases here
  // on purpose: a mirror of a box lasso can also drive directly joins that
  // box's group instead of forming a parallel one beside it.
  const families = React.useMemo(
    () => aliasFamilies(panes.map(originHost).filter(Boolean)),
    [panes]
  )

  // Rows grouped by the machine their work lives on, using the navbar host
  // switcher's family rule (familyOf) rather than a second scheme of this
  // palette's own. Without this a mirror sorts in among the local panes by
  // herdr workspace number and reads as local — on titan that is 32 of 61 rows.
  //
  // lasso's own machine leads; the rest are alphabetical, because herdr's
  // creation order (which is what the payload carries) says nothing useful
  // across nine hosts, whereas an alphabetical list of hosts is scannable.
  const sections = React.useMemo(() => {
    const byKey = new Map<string, PaneSection>()
    for (const p of matched) {
      const origin = originHost(p)
      const key = origin ? familyOf(origin, families) : LOCAL_KEY
      let s = byKey.get(key)
      if (!s) {
        s = {
          key,
          label: origin ? key : p.host_label || "local",
          local: !origin,
          start: 0,
          panes: [],
        }
        byKey.set(key, s)
      }
      s.panes.push(p)
    }
    const out = [...byKey.values()].sort((a, b) =>
      a.local === b.local ? a.label.localeCompare(b.label) : a.local ? -1 : 1
    )
    let start = 0
    for (const s of out) {
      s.start = start
      start += s.panes.length
    }
    return out
  }, [matched, families])

  // The flattened rows, in the order they render — what ↑/↓ and Enter index
  // into.
  const filtered = React.useMemo(
    () => sections.flatMap((s) => s.panes),
    [sections]
  )

  // With one host in the list there is nothing to tell apart, so the headers
  // are pure noise — a lasso with no mirrors and no remote hosts looks exactly
  // as it did.
  const showHeaders = sections.length > 1

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

  const choose = (p: HostPane) => {
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
          queryClient.invalidateQueries({ queryKey: qk.panes })
          queryClient.invalidateQueries({ queryKey: qk.agentHistory })
          return focusPaneInHerdr(np, activeHost)
        })
        .catch((e) => toast.error(`reopen failed: ${(e as Error).message}`))
      return
    }
    focusPaneInHerdr(p, activeHost).catch((e) =>
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
        className={cn(
          "gap-0 p-0",
          // Mobile: a full-screen sheet sized to the visible area above the
          // keyboard (--vvh), so the list scrolls inside it and the soft keyboard
          // can't push it off-screen / scroll the page behind it.
          "inset-0 flex h-[var(--vvh,100dvh)] max-h-none w-full max-w-none translate-x-0 translate-y-0 flex-col rounded-none",
          // sm+: the centered floating palette.
          "sm:inset-auto sm:top-[15%] sm:left-1/2 sm:h-auto sm:max-h-[70dvh] sm:max-w-lg sm:-translate-x-1/2 sm:rounded-xl"
        )}
        onOpenAutoFocus={(e) => {
          e.preventDefault()
          // Mobile uses a 16px search field to prevent iOS page zoom, and radial
          // Search does not restore focus to xterm when this closes. Focusing here
          // can therefore open the keyboard without shifting the terminal page.
          inputRef.current?.focus({ preventScroll: true })
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
        {/* A full-screen sheet on mobile has no backdrop to tap away, so give it
            an explicit close. Hidden on sm+, where Esc / click-away dismiss. */}
        <button
          type="button"
          onClick={() => onOpenChange(false)}
          aria-label="Close"
          className="absolute top-2 right-2 z-10 flex size-9 items-center justify-center rounded-md text-muted-foreground hover:text-foreground sm:hidden"
        >
          <X className="size-5" />
        </button>
        <input
          ref={inputRef}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="Search panes by tab, workspace, host, agent, path, or prompt…"
          className="w-full bg-transparent px-4 py-3 pr-14 text-base outline-none placeholder:text-muted-foreground sm:pr-4 sm:text-sm"
        />
        {/* Active filter: on shows only live panes (default); off folds in past
            agents whose pane was closed, so old sessions can be reopened. */}
        <div className="flex select-none items-center justify-end gap-2 border-border border-b px-4 pb-2 text-muted-foreground text-xs">
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
        </div>
        <div
          ref={listRef}
          className="min-h-0 flex-1 overflow-y-auto overscroll-contain p-1 sm:max-h-80 sm:flex-none"
        >
          {filtered.length === 0 ? (
            <div className="px-3 py-6 text-center text-muted-foreground text-sm">
              {q.isLoading || hist.isLoading ? (
                "Loading…"
              ) : loadError ? (
                // A failed fetch used to render as "No matching panes." — the
                // palette claiming the fleet is empty when it simply couldn't
                // ask. Say what went wrong instead.
                <span className="text-destructive">
                  Couldn't load panes: {loadError}
                </span>
              ) : (
                "No matching panes."
              )}
            </div>
          ) : (
            sections.map((s) => (
              <div key={s.key}>
                {/* Host header, in the Grid rail's idiom: an icon + UPPERCASE
                    tracked label, so it reads as structure and can't be taken
                    for a pane row. */}
                {showHeaders && (
                  <div className="flex items-center gap-1.5 px-3 pt-2 pb-0.5 font-semibold text-[10px] text-muted-foreground/80 uppercase tracking-wide">
                    {s.local ? (
                      <Laptop className="size-3 shrink-0" />
                    ) : (
                      <Server className="size-3 shrink-0" />
                    )}
                    <span className="truncate">{s.label}</span>
                  </div>
                )}
                {s.panes.map((p, j) => {
                  const i = s.start + j
                  return (
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
                        {/* A mirror is a real local pane, but everything in it is
                      happening on another machine. Marked on the row itself
                      (not only in the header) so it still reads as remote when
                      a search has narrowed the list to one group. */}
                        {p.mirror_host && (
                          <span
                            className={cn(
                              "shrink-0 rounded px-1.5 py-0.5 font-medium text-[11px]",
                              i === active
                                ? "bg-primary-foreground/20 text-primary-foreground"
                                : "bg-foreground/10 text-muted-foreground"
                            )}
                            title={`mirrored from ${p.mirror_host}${
                              p.mirror_pane ? ` (${p.mirror_pane})` : ""
                            }`}
                          >
                            mirror
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
                        <span className="shrink-0">
                          {p.mirror_host || p.host_label}
                        </span>
                        {/* A mirror has no local working directory: herdr-mirror parks
                      every streamer in one sentinel dir, and herdr can't see the
                      remote's cwd (nor its repo/branch, which is why mirror rows
                      carry no git chip anywhere in lasso). Showing that path
                      would put the same meaningless line under 32 rows, so the
                      subtitle is just the host it lives on. */}
                        {!p.mirror_host && p.cwd && (
                          <span className="truncate font-mono">
                            {tilde(p.cwd)}
                          </span>
                        )}
                      </span>
                    </button>
                  )
                })}
              </div>
            ))
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}
