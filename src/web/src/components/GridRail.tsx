import * as React from "react"

import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuTrigger,
} from "@/components/ui/context-menu"
import { Input } from "@/components/ui/input"
import type { GridPane } from "@/lib/api"
import { tilde } from "@/lib/format"
import { patchUIState, useUIState } from "@/lib/ui-state"
import { cn } from "@/lib/utils"

// Same cross-host pane identity as GridTab / PaneSwitcher (pane ids are only
// unique within a host).
const railKey = (p: GridPane) => `${p.host}|${p.pane_id}`

// GridRail is the Grid tab's collapsible pane picker: every pane on every
// host, grouped by host, each row with an agent status dot. In Multi mode
// it's a multi-select: every row starts greyed out and clicking a row toggles
// that pane in or out of the grid. In Single mode clicking a row shows that
// one pane. Collapsed it renders zero width (the parent animates the
// container); content keeps a fixed width so text doesn't reflow
// mid-transition.
export function GridRail({
  open,
  panes,
  watched,
  newKeys,
  selectMode,
  selectedKey,
  onToggleWatch,
  onFocusPane,
  onOpenInHerdr,
  onClose,
}: {
  open: boolean
  /** ALL panes across hosts, unfiltered — the rail is the full roster. */
  panes: GridPane[] | null
  /** Multi mode's toggled-on panes — these rows render at full strength. */
  watched: Set<string>
  /** Keys to highlight as new (snapshotted by GridTab when the badge opens the rail). */
  newKeys: Set<string>
  /** Single mode: clicking a row shows that one pane instead of toggling. */
  selectMode: boolean
  /** The pane currently shown in Single mode (highlighted); null otherwise. */
  selectedKey: string | null
  /** Multi mode row click: toggle the pane in/out of the grid. */
  onToggleWatch: (key: string) => void
  /** Single mode row click: show this pane in the grid (no navigation). */
  onFocusPane: (p: GridPane) => void
  /** Context menu: deliberately leave the grid for the Herdr tab. */
  onOpenInHerdr: (p: GridPane) => void
  /** Context menu: close the pane (confirm dialog + herdr pane.close). */
  onClose: (p: GridPane) => void
}) {
  const [search, setSearch] = React.useState("")
  // The agents-only toggle is server-synced (grid_rail_agents_only) so every
  // tab's rail filters the same way, like the rest of the grid state.
  const agentsOnly = useUIState().grid_rail_agents_only
  const toggleAgentsOnly = () =>
    patchUIState({ grid_rail_agents_only: !agentsOnly })
  const firstNewRef = React.useRef<HTMLDivElement>(null)

  // Bring the first "new" row into view when the rail opens via the badge.
  React.useEffect(() => {
    if (open && newKeys.size > 0)
      firstNewRef.current?.scrollIntoView({ block: "nearest" })
  }, [open, newKeys])

  const groups = React.useMemo(() => {
    const q = search.trim().toLowerCase()
    const match = (p: GridPane) =>
      (!agentsOnly || p.has_agent) &&
      (!q ||
        [p.host_label, p.workspace_label, p.tab_label, p.agent, p.cwd]
          .filter(Boolean)
          .some((s) => (s as string).toLowerCase().includes(q)))
    const m = new Map<string, { label: string; panes: GridPane[] }>()
    for (const p of panes ?? []) {
      if (!match(p)) continue
      let g = m.get(p.host)
      if (!g) {
        g = { label: p.host_label, panes: [] }
        m.set(p.host, g)
      }
      g.panes.push(p)
    }
    // Agent panes first within each host (stable, so payload order holds
    // within each half) — the rows you actually scan for sit at the top.
    for (const g of m.values())
      g.panes.sort((a, b) => Number(b.has_agent) - Number(a.has_agent))
    return Array.from(m.values())
  }, [panes, search, agentsOnly])

  let sawNew = false
  return (
    <div
      className={cn(
        "shrink-0 overflow-hidden border-border border-r transition-[width] duration-150",
        open ? "w-64" : "w-0 border-r-0"
      )}
    >
      <div className="flex h-full w-64 flex-col">
        <div className="flex shrink-0 items-center gap-1.5 p-2">
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="filter panes…"
            className="h-7 min-w-0 flex-1 text-xs"
          />
          <button
            type="button"
            onClick={toggleAgentsOnly}
            aria-pressed={agentsOnly}
            className={cn(
              "flex shrink-0 items-center gap-1 rounded-full border px-1.5 py-0.5 text-[11px] transition-colors",
              agentsOnly
                ? "border-primary/40 bg-accent text-foreground"
                : "border-border text-muted-foreground hover:text-foreground"
            )}
            title="Show only panes with an agent"
          >
            <span
              className={cn(
                "size-2 rounded-full",
                agentsOnly ? "bg-primary" : "bg-muted-foreground/40"
              )}
            />
            agents
          </button>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto pb-2">
          {groups.length === 0 && (
            <div className="empty text-xs">
              {panes?.length ? "no panes match" : "no panes"}
            </div>
          )}
          {groups.map((g) => (
            <div key={g.label} className="mb-1">
              <div className="px-3 py-1 font-semibold text-[11px] text-accent-foreground/70">
                {g.label}
              </div>
              {g.panes.map((p) => {
                const key = railKey(p)
                const isNew = newKeys.has(key)
                const refNew = isNew && !sawNew
                if (refNew) sawNew = true
                const title = p.workspace_label || p.workspace_id || p.pane_id
                const tabLabel =
                  p.tab_label && p.tab_label !== title ? p.tab_label : ""
                const viewed = watched.has(key)
                return (
                  <div
                    key={key}
                    ref={refNew ? firstNewRef : undefined}
                    className={cn(
                      "group flex w-full items-center gap-1.5 px-2 py-1 text-xs hover:bg-accent/50",
                      (isNew || (selectMode && key === selectedKey)) &&
                        "bg-accent",
                      // Multi mode: rows start greyed out; toggled-on rows
                      // render at full strength with a selected background.
                      !selectMode &&
                        (viewed
                          ? "bg-accent"
                          : !isNew && "opacity-50 hover:opacity-100")
                    )}
                  >
                    <ContextMenu>
                      <ContextMenuTrigger asChild>
                        <button
                          type="button"
                          aria-pressed={selectMode ? undefined : viewed}
                          onClick={() =>
                            selectMode ? onFocusPane(p) : onToggleWatch(key)
                          }
                          title={[
                            p.host_label,
                            p.workspace_label,
                            p.tab_label,
                            p.agent,
                            tilde(p.cwd),
                            "",
                            selectMode
                              ? "click to show this pane"
                              : viewed
                                ? "click to hide from the grid"
                                : "click to view in the grid",
                          ]
                            .filter((s) => s !== undefined && s !== null)
                            .join("\n")}
                          className="flex min-w-0 flex-1 cursor-pointer items-center gap-1.5 text-left"
                        >
                          <span className="truncate text-foreground">
                            {title}
                            {tabLabel ? ` · ${tabLabel}` : ""}
                          </span>
                          {p.has_agent && (
                            <span
                              className={cn(
                                "shrink-0 text-[10px] text-muted-foreground",
                                p.agent_status === "working" && "text-warn",
                                p.agent_status === "blocked" && "text-bad",
                                (p.agent_status === "idle" ||
                                  p.agent_status === "done") &&
                                  "text-good"
                              )}
                            >
                              ● {p.agent || "agent"}
                            </span>
                          )}
                          {isNew && (
                            <span className="shrink-0 rounded-sm bg-primary/15 px-1 text-[9px] text-primary uppercase">
                              new
                            </span>
                          )}
                        </button>
                      </ContextMenuTrigger>
                      <ContextMenuContent>
                        <ContextMenuItem onSelect={() => onOpenInHerdr(p)}>
                          Open in Herdr
                        </ContextMenuItem>
                        {!selectMode && (
                          <ContextMenuItem onSelect={() => onToggleWatch(key)}>
                            {viewed ? "Hide from grid" : "Show in grid"}
                          </ContextMenuItem>
                        )}
                        <ContextMenuItem
                          variant="destructive"
                          onSelect={() => onClose(p)}
                        >
                          {p.has_agent ? "Close agent" : "Close pane"}
                        </ContextMenuItem>
                      </ContextMenuContent>
                    </ContextMenu>
                  </div>
                )
              })}
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}
