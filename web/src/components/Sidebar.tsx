import { useQuery } from "@tanstack/react-query"
import {
  ChevronLeft,
  ChevronRight,
  GitBranch,
  Laptop,
  Loader2,
  Plus,
  Server,
  Terminal,
} from "lucide-react"
import * as React from "react"
import { toast } from "sonner"

import { PromptDialog } from "@/components/PromptDialog"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from "@/components/ui/context-menu"
import {
  type AgentRow,
  api,
  type TreePayload,
  type TreeRepo,
  type TreeTab,
  type TreeWorkspace,
} from "@/lib/api"
import { useApp } from "@/lib/app-store"
import {
  fetchAgents,
  fetchTree,
  qk,
  queryClient,
  spacesKeyRepo,
  spacesKeyWorkspace,
  treeAddScratchWorkspace,
  treeAddTab,
  treeReorderSpaces,
  treeSetRepoMain,
} from "@/lib/query"
import { useUIState } from "@/lib/ui-state"
import { cn } from "@/lib/utils"

// The left sidebar: a "spaces" tree (git repos with their worktrees nested, plus
// standalone scratch workspaces) over an "agents" pane that shows every agent's
// live status. Selecting a tab/agent shows its terminal in the center. The
// top-level "spaces" rows are a single manually-ordered list — drag to reorder;
// new workspaces land at the bottom (order persists server-side via /api/tree's
// `order` + /api/spaces/reorder).

const STATUS_DOT: Record<string, string> = {
  working: "bg-[var(--h-warn)] animate-pulse",
  blocked: "bg-[var(--h-bad)]",
  idle: "bg-[var(--h-good)]",
  unknown: "bg-muted-foreground/40",
}

function refreshTree() {
  queryClient.invalidateQueries({ queryKey: qk.tree })
  queryClient.invalidateQueries({ queryKey: qk.agents })
}

// A tab is "named" if its title isn't blank or a bare number (the default
// "1","2",… auto-names). Only named tabs surface in the spaces tree under their
// scratch workspace; numbered tabs stay in the TabStrip and don't pollute it.
function isNamedTab(t: TreeTab): boolean {
  const s = (t.title ?? "").trim()
  return s !== "" && !/^\d+$/.test(s)
}

// A single top-level row of the unified "spaces" list: either a standalone
// scratch workspace or a repo (which expands to its nested worktrees). `host` /
// `hostLabel` are the host the row lives on — used to group the list by host in
// all-hosts mode (and to route host-scoped mutations to the right machine).
type SpaceItem = { key: string; host?: string; hostLabel?: string } & (
  | { kind: "ws"; ws: TreeWorkspace }
  | { kind: "repo"; repo: TreeRepo }
)

// unifiedSpaces flattens the tree payload into one ordered list, following the
// server's `order` (stable keys) and appending any rows not yet placed in it —
// freshly-created/optimistic rows — at the bottom.
function unifiedSpaces(tree: TreePayload | undefined): SpaceItem[] {
  if (!tree) return []
  const scratch = tree.scratch ?? []
  const repos = tree.repos ?? []
  const wsById = new Map(scratch.map((w) => [w.id, w]))
  // Workspace ids are globally unique, but a repo:<path> key can recur in the
  // combined order — once per host that has that path (all-hosts mode). The repos
  // array is grouped host-first (matching the concatenated order), so a per-path
  // cursor consumes them in that order: the Nth repo:<path> in `order` is the Nth
  // repo with that path.
  const reposByPath = new Map<string, TreeRepo[]>()
  for (const r of repos) {
    const list = reposByPath.get(r.path)
    if (list) list.push(r)
    else reposByPath.set(r.path, [r])
  }
  const repoCursor = new Map<string, number>()
  const items: SpaceItem[] = []
  const placedWs = new Set<string>()
  const placedRepo = new Set<TreeRepo>()
  const pushWs = (ws: TreeWorkspace) => {
    if (placedWs.has(ws.id)) return
    items.push({
      key: spacesKeyWorkspace(ws.id),
      kind: "ws",
      ws,
      host: ws.host,
      hostLabel: ws.host_label,
    })
    placedWs.add(ws.id)
  }
  const pushRepo = (repo: TreeRepo) => {
    if (placedRepo.has(repo)) return
    items.push({
      key: spacesKeyRepo(repo.path),
      kind: "repo",
      repo,
      host: repo.host,
      hostLabel: repo.host_label,
    })
    placedRepo.add(repo)
  }
  for (const key of tree.order ?? []) {
    if (key.startsWith("ws:")) {
      const ws = wsById.get(key.slice(3))
      if (ws) pushWs(ws)
    } else if (key.startsWith("repo:")) {
      const path = key.slice(5)
      const list = reposByPath.get(path) ?? []
      const i = repoCursor.get(path) ?? 0
      if (i < list.length) {
        pushRepo(list[i])
        repoCursor.set(path, i + 1)
      }
    }
  }
  // Rows not present in `order` (freshly created/optimistic) render at the bottom.
  for (const ws of scratch) pushWs(ws)
  for (const repo of repos) pushRepo(repo)
  return items
}

// rowId is a row's drag identity: a repo:<path> key can recur across hosts
// (all-hosts mode), so the host-qualified id is what we drag/drop by — the bare
// key stays the per-host storage key.
const rowId = (item: SpaceItem) => `${item.host ?? ""}::${item.key}`

// openNewAgent asks the (always-mounted) CreateAgentDialog to open prefilled with
// a repo + base, via a window event so the sidebar doesn't own the dialog.
function openNewAgent(repo: string, base: string, host?: string) {
  window.dispatchEvent(
    new CustomEvent("lasso:new-agent", { detail: { repo, base, host } })
  )
}

export function Sidebar({
  selectedTabId,
  onSelectTab,
  onCollapse,
}: {
  selectedTabId: string | null
  onSelectTab: (tabId: string) => void
  onCollapse?: () => void
}) {
  const { panesRev, agentStatuses } = useApp()
  const allHosts = useUIState().sidebar_all_hosts ?? false
  // In all-hosts mode the SSE panesRev only tracks LOCAL tree changes, so poll on
  // an interval to surface remote workspaces/agents appearing and disappearing.
  const tree = useQuery({
    queryKey: qk.tree,
    queryFn: fetchTree,
    refetchInterval: allHosts ? 4000 : false,
  })
  const agents = useQuery({
    queryKey: qk.agents,
    queryFn: fetchAgents,
    refetchInterval: allHosts ? 4000 : false,
  })
  // The usable host set, only needed in all-hosts mode to know which hosts are
  // still expected but haven't reported their tree yet (shared cache with the
  // HostSwitcher's query).
  const hostsQuery = useQuery({
    queryKey: qk.hosts,
    queryFn: () => api.hosts(),
    staleTime: 20_000,
    enabled: allHosts,
  })

  // Hosts the sidebar expects to show but whose tree hasn't loaded yet. In
  // all-hosts mode the tree dials each remote serially, so a cold host yields
  // nothing on early polls and fills in later. The backend reports the hosts it
  // successfully queried (tree.data.hosts) — even empty ones — so a host that's
  // usable (per /api/hosts) but not yet in that set is "still connecting", not
  // "empty". We render a per-host loading row for those instead of nothing.
  const pendingHosts = React.useMemo(() => {
    if (!allHosts) return []
    const hp = hostsQuery.data
    if (!hp) return [] // host list not loaded yet — don't guess
    const expected = [
      { key: "local", label: hp.hostname || "local" },
      ...hp.hosts
        .filter((h) => h.reachable && h.has_tmux)
        .map((h) => ({ key: h.alias, label: h.alias })),
    ]
    const loaded = new Set(tree.data?.hosts ?? [])
    return expected.filter((e) => !loaded.has(e.key))
  }, [allHosts, hostsQuery.data, tree.data?.hosts])

  // How many purely-numeric scratch agents ("1", "2"…) each workspace holds. A
  // lone one needs no number to disambiguate (just the workspace name); two or
  // more do ("52 Labs 1" / "52 Labs 2"). Keyed by workspace id. See AgentRowItem.
  const numericByWorkspace = React.useMemo(() => {
    const m = new Map<string, number>()
    for (const a of agents.data?.agents ?? [])
      if (!a.repo && /^\d+$/.test(a.title))
        m.set(a.workspace_id, (m.get(a.workspace_id) ?? 0) + 1)
    return m
  }, [agents.data])

  // Refetch the tree + agents whenever the workspace/tab layout changes (SSE).
  React.useEffect(() => {
    if (panesRev >= 0) refreshTree()
  }, [panesRev])

  // The new-workspace name modal (opened by the footer "+", the empty-area
  // right-click, and ⌘I). Submitting creates a bare scratch workspace (a shell,
  // no agent) and focuses it. PromptDialog autofocuses its input on open.
  const [newWsOpen, setNewWsOpen] = React.useState(false)
  React.useEffect(() => {
    const open = () => setNewWsOpen(true)
    window.addEventListener("lasso:new-workspace", open)
    return () => window.removeEventListener("lasso:new-workspace", open)
  }, [])
  const submitNewWorkspace = async (title: string) => {
    try {
      const { workspace_id, tab_id, work_dir } =
        await api.createWorkspace(title)
      treeAddScratchWorkspace({
        id: workspace_id,
        title,
        work_dir,
        kind: "scratch",
        tabs: [{ id: tab_id, title: "1", kind: "shell" }],
      })
      onSelectTab(tab_id)
      refreshTree()
    } catch (e) {
      toast.error(String(e))
    }
  }

  // The workspace whose tab is currently open (a scratch workspace or a worktree;
  // repo main-checkouts aren't bulk-deletable, so they resolve to null).
  const activeWorkspaceId = React.useMemo(() => {
    const t = tree.data
    if (!t || !selectedTabId) return null
    const has = (ws: TreeWorkspace) =>
      (ws.tabs ?? []).some((x) => x.id === selectedTabId)
    for (const ws of t.scratch ?? []) if (has(ws)) return ws.id
    for (const repo of t.repos ?? [])
      for (const ws of repo.workspaces ?? []) if (has(ws)) return ws.id
    return null
  }, [tree.data, selectedTabId])
  const activeWsRef = React.useRef<string | null>(null)
  activeWsRef.current = activeWorkspaceId

  // Multi-select for bulk deletion: ⌘/Ctrl-click (or the context menu) toggles a
  // workspace into `delSel`; an action bar then deletes them all at once.
  const [delSel, setDelSel] = React.useState<Set<string>>(new Set())
  const [confirmBulk, setConfirmBulk] = React.useState(false)
  const toggleDel = React.useCallback((id: string) => {
    setDelSel((prev) => {
      const next = new Set(prev)
      if (next.has(id)) {
        next.delete(id)
      } else {
        // Starting a fresh selection: fold in the currently-open workspace so the
        // one you're looking at is included alongside the one you ⌘-clicked.
        if (next.size === 0) {
          const active = activeWsRef.current
          if (active && active !== id) next.add(active)
        }
        next.add(id)
      }
      return next
    })
  }, [])
  const clearDel = React.useCallback(() => setDelSel(new Set()), [])
  const openBulkDelete = React.useCallback(() => setConfirmBulk(true), [])
  // Clear the selection with Escape.
  React.useEffect(() => {
    if (delSel.size === 0) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") clearDel()
    }
    window.addEventListener("keydown", onKey)
    return () => window.removeEventListener("keydown", onKey)
  }, [delSel.size, clearDel])
  // Clicking away dismisses the selection: any plain left-click outside the
  // selection's own controls clears it. ⌘-clicks (toggling more in) and
  // right-clicks (the delete menu) are left alone, as are elements tagged
  // data-bulk-keep (the action bar, confirm dialog, and delete context menu).
  React.useEffect(() => {
    if (delSel.size === 0) return
    const onDown = (e: MouseEvent) => {
      if (e.button !== 0 || e.metaKey || e.ctrlKey) return
      const el = e.target as HTMLElement | null
      if (el?.closest("[data-bulk-keep]")) return
      clearDel()
    }
    document.addEventListener("mousedown", onDown, true)
    return () => document.removeEventListener("mousedown", onDown, true)
  }, [delSel.size, clearDel])
  const bulkDelete = React.useCallback(async () => {
    const ids = [...delSel]
    setConfirmBulk(false)
    clearDel()
    await Promise.all(
      ids.map((id) =>
        api.closeWorkspace(id).catch((e) => toast.error(String(e)))
      )
    )
    refreshTree()
  }, [delSel, clearDel])

  // The unified, manually-ordered top-level "spaces" list (scratch workspaces +
  // repos interleaved). Drag a row onto another to reorder; the full new key
  // order is persisted server-side.
  const items = React.useMemo(() => unifiedSpaces(tree.data), [tree.data])
  const [dragId, setDragId] = React.useState<string | null>(null)
  const [dropTarget, setDropTarget] = React.useState<{
    id: string
    pos: "before" | "after"
  } | null>(null)
  const commitReorder = React.useCallback(
    (fromId: string, toId: string, pos: "before" | "after") => {
      if (fromId === toId) return
      const from = items.find((i) => rowId(i) === fromId)
      const to = items.find((i) => rowId(i) === toId)
      // Only reorder within a single host group (the saved order is per-host).
      if (!from || !to || (from.host ?? "") !== (to.host ?? "")) return
      const without = items.filter((i) => rowId(i) !== fromId)
      const idx = without.findIndex((i) => rowId(i) === toId)
      if (idx < 0) return
      without.splice(pos === "before" ? idx : idx + 1, 0, from)
      const newOrder = without.map((i) => i.key)
      if (newOrder.join("\n") === items.map((i) => i.key).join("\n")) return
      treeReorderSpaces(newOrder)
      // All-hosts mode persists just this host's slice under its own key; the
      // single-host view persists the whole list to the active host (no host arg).
      const host = from.host
      const order = allHosts
        ? without
            .filter((i) => (i.host ?? "") === (host ?? ""))
            .map((i) => i.key)
        : newOrder
      api.reorderSpaces(order, allHosts ? host : undefined).catch((e) => {
        toast.error(String(e))
        refreshTree()
      })
    },
    [items, allHosts]
  )
  // Native HTML5 drag props for one top-level row. Grabbing anywhere in a repo's
  // block (header or its nested worktrees) drags the whole repo; rows still
  // open on a plain click. Returns props to spread plus an indicator className.
  const dragFor = (item: SpaceItem) => {
    const id = rowId(item)
    const dragItem = dragId
      ? (items.find((i) => rowId(i) === dragId) ?? null)
      : null
    // Reject drops from another host group — the saved order is per-host.
    const sameHost = dragItem && (dragItem.host ?? "") === (item.host ?? "")
    return {
      props: {
        draggable: true,
        onDragStart: (e: React.DragEvent) => {
          setDragId(id)
          e.dataTransfer.effectAllowed = "move"
          e.dataTransfer.setData("text/plain", id)
        },
        onDragOver: (e: React.DragEvent) => {
          if (!dragId || dragId === id || !sameHost) return
          e.preventDefault()
          e.dataTransfer.dropEffect = "move"
          const rect = e.currentTarget.getBoundingClientRect()
          const pos: "before" | "after" =
            e.clientY < rect.top + rect.height / 2 ? "before" : "after"
          if (dropTarget?.id !== id || dropTarget.pos !== pos) {
            setDropTarget({ id, pos })
          }
        },
        onDragLeave: (e: React.DragEvent) => {
          if (!e.currentTarget.contains(e.relatedTarget as Node)) {
            setDropTarget((cur) => (cur?.id === id ? null : cur))
          }
        },
        onDrop: (e: React.DragEvent) => {
          e.preventDefault()
          if (dragId) commitReorder(dragId, id, dropTarget?.pos ?? "before")
          setDragId(null)
          setDropTarget(null)
        },
        onDragEnd: () => {
          setDragId(null)
          setDropTarget(null)
        },
      },
      className: cn(
        dragId === id && "opacity-40",
        dropTarget?.id === id &&
          (dropTarget.pos === "before"
            ? "border-[var(--h-accent)] border-t-2"
            : "border-[var(--h-accent)] border-b-2")
      ),
    }
  }

  return (
    <div className="flex h-full min-h-0 flex-col bg-card text-[13px]">
      {/* The spaces region is `relative` so the create-workspace button can pin to
          its bottom-right without scrolling with the list. Right-clicking the
          empty area below the tree opens the same "New workspace…" action; the
          repo/workspace rows have their own context menus that take precedence. */}
      <div className="relative min-h-0 flex-1">
        <ContextMenu>
          <ContextMenuTrigger asChild>
            <div className="h-full overflow-y-auto pb-12">
              <SectionLabel
                trailing={
                  onCollapse && (
                    <button
                      type="button"
                      title="collapse sidebar (⌘[)"
                      className="-my-1 flex items-center text-muted-foreground hover:text-primary"
                      onClick={onCollapse}
                    >
                      <ChevronLeft className="size-4" />
                    </button>
                  )
                }
              >
                spaces
              </SectionLabel>
              {items.map((item, idx) => {
                const drag = dragFor(item)
                // In all-hosts mode start each host's spaces with a header (the
                // host's first row is wherever its host differs from the prior).
                const header =
                  allHosts && item.host !== items[idx - 1]?.host ? (
                    <HostHeader
                      label={item.hostLabel ?? item.host ?? "local"}
                    />
                  ) : null
                return (
                  <React.Fragment key={rowId(item)}>
                    {header}
                    <div {...drag.props} className={drag.className}>
                      {item.kind === "ws" ? (
                        <WorkspaceNode
                          ws={item.ws}
                          selectedTabId={selectedTabId}
                          onSelectTab={onSelectTab}
                          depth={1}
                          delSel={delSel}
                          onToggleDel={toggleDel}
                          onBulkDelete={openBulkDelete}
                        />
                      ) : (
                        <RepoNode
                          repo={item.repo}
                          selectedTabId={selectedTabId}
                          onSelectTab={onSelectTab}
                          delSel={delSel}
                          onToggleDel={toggleDel}
                          onBulkDelete={openBulkDelete}
                        />
                      )}
                    </div>
                  </React.Fragment>
                )
              })}
              {/* Hosts still connecting: a header + loading row so their spaces
                  don't trickle in silently. Rendered after the loaded hosts. */}
              {pendingHosts.map((h) => (
                <React.Fragment key={`pending:${h.key}`}>
                  <HostHeader label={h.label} />
                  <HostLoadingRow />
                </React.Fragment>
              ))}
            </div>
          </ContextMenuTrigger>
          <ContextMenuContent>
            <ContextMenuItem onSelect={() => setNewWsOpen(true)}>
              New workspace…
            </ContextMenuItem>
          </ContextMenuContent>
        </ContextMenu>
        {delSel.size === 0 && (
          <button
            type="button"
            title="New workspace"
            aria-label="New workspace"
            onClick={() => setNewWsOpen(true)}
            className="absolute right-1.5 bottom-1.5 flex size-5 items-center justify-center rounded text-muted-foreground/60 hover:bg-accent hover:text-foreground"
          >
            <Plus className="size-3.5" />
          </button>
        )}
        {/* Bulk-delete bar: appears while one or more workspaces are ⌘/Ctrl-clicked
            (or selected via the context menu). */}
        {delSel.size > 0 && (
          <div
            data-bulk-keep
            className="absolute inset-x-0 bottom-0 flex items-center gap-2 border-border border-t bg-card px-3 py-1.5"
          >
            <span className="text-muted-foreground text-xs">
              {delSel.size} selected
            </span>
            <button
              type="button"
              onClick={() => setConfirmBulk(true)}
              className="rounded bg-[var(--h-bad)] px-2 py-0.5 font-medium text-white text-xs hover:bg-[var(--h-bad)]/90"
            >
              Delete
            </button>
            <button
              type="button"
              onClick={clearDel}
              className="ml-auto rounded px-2 py-0.5 text-muted-foreground text-xs hover:bg-accent hover:text-foreground"
            >
              Clear
            </button>
          </div>
        )}
        <PromptDialog
          open={newWsOpen}
          onOpenChange={setNewWsOpen}
          title="New workspace"
          placeholder="Workspace name"
          submitLabel="Create"
          onSubmit={submitNewWorkspace}
        />
        <AlertDialog open={confirmBulk} onOpenChange={setConfirmBulk}>
          <AlertDialogContent data-bulk-keep>
            <AlertDialogHeader>
              <AlertDialogTitle>
                Delete {delSel.size} workspace{delSel.size === 1 ? "" : "s"}?
              </AlertDialogTitle>
              <AlertDialogDescription>
                Closing them ends every tab in each, including any running
                agents. This can’t be undone.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>Cancel</AlertDialogCancel>
              <AlertDialogAction
                onClick={bulkDelete}
                className="bg-[var(--h-bad)] text-white hover:bg-[var(--h-bad)]/90"
              >
                Delete {delSel.size}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </div>

      <div className="max-h-[40%] min-h-0 shrink-0 overflow-y-auto border-border border-t">
        <SectionLabel>agents</SectionLabel>
        {(agents.data?.agents ?? []).map((a, idx) => {
          const list = agents.data?.agents ?? []
          // All-hosts mode: a header before each host's first agent (the list is
          // already grouped by host, local first).
          const header =
            allHosts && a.host !== list[idx - 1]?.host ? (
              <HostHeader label={a.host_label ?? a.host ?? "local"} />
            ) : null
          return (
            <React.Fragment key={`${a.host ?? ""}:${a.tab_id}`}>
              {header}
              <AgentRowItem
                agent={a}
                selected={a.tab_id === selectedTabId}
                status={agentStatuses[a.tab_id] ?? a.status}
                siblingNumeric={numericByWorkspace.get(a.workspace_id) ?? 0}
                onSelect={() => onSelectTab(a.tab_id)}
              />
            </React.Fragment>
          )
        })}
        {agents.data && (agents.data.agents ?? []).length === 0 && (
          <div className="px-3 py-2 text-muted-foreground">no agents</div>
        )}
      </div>
    </div>
  )
}

function SectionLabel({
  children,
  trailing,
}: {
  children: React.ReactNode
  trailing?: React.ReactNode
}) {
  return (
    <div className="flex items-center justify-between px-3 pt-3 pb-1 font-semibold text-[11px] text-muted-foreground uppercase tracking-wider">
      <span>{children}</span>
      {trailing}
    </div>
  )
}

// HostHeader labels a host's section in the all-hosts sidebar (spaces + agents
// are grouped by host). The local box gets a laptop glyph; remotes a server one.
function HostHeader({ label }: { label: string }) {
  const local = label === "local"
  return (
    <div className="flex items-center gap-1.5 px-3 pt-2 pb-1 font-semibold text-[10px] text-muted-foreground/80 uppercase tracking-wider">
      {local ? <Laptop className="size-3" /> : <Server className="size-3" />}
      <span className="truncate">{label}</span>
      <span className="ml-1 h-px flex-1 bg-border" />
    </div>
  )
}

// HostLoadingRow is the placeholder shown under a host header while that host's
// spaces are still being fetched (all-hosts mode), so the section reads as
// "loading" rather than empty.
function HostLoadingRow() {
  return (
    <div className="flex items-center gap-2 px-3 py-1.5 text-muted-foreground text-xs">
      <Loader2 className="size-3 animate-spin" />
      <span>loading…</span>
    </div>
  )
}

function RepoNode({
  repo,
  selectedTabId,
  onSelectTab,
  delSel,
  onToggleDel,
  onBulkDelete,
}: {
  repo: TreeRepo
  selectedTabId: string | null
  onSelectTab: (tabId: string) => void
  delSel: Set<string>
  onToggleDel: (id: string) => void
  onBulkDelete: () => void
}) {
  const { agentStatuses } = useApp()
  const [open, setOpen] = React.useState(true)
  const worktrees = repo.workspaces ?? []
  const status =
    (repo.main_tab_id && agentStatuses[repo.main_tab_id]) || repo.agent_status
  const selected = !!repo.main_tab_id && repo.main_tab_id === selectedTabId

  // Clicking the repo row opens a terminal on its primary branch (the main
  // checkout) — a repo is a workspace, not just a worktree grouping.
  const openRepo = async () => {
    if (repo.main_tab_id) {
      onSelectTab(repo.main_tab_id)
      return
    }
    try {
      const { tab_id, workspace_id } = await api.openRepo(repo.path, repo.host)
      // Surface the main checkout immediately so the tab strip resolves it.
      treeSetRepoMain(repo.path, {
        id: workspace_id,
        title: repo.name,
        repo: repo.path,
        work_dir: repo.path,
        kind: "git",
        branch: repo.primary_branch,
        host: repo.host,
        host_label: repo.host_label,
        tabs: [{ id: tab_id, title: "1", kind: "shell" }],
      })
      onSelectTab(tab_id)
      refreshTree()
    } catch (e) {
      toast.error(String(e))
    }
  }
  const [renameOpen, setRenameOpen] = React.useState(false)
  const submitRename = async (name: string) => {
    await api
      .renameRepo(repo.path, name, repo.host)
      .catch((e) => toast.error(String(e)))
    refreshTree()
  }
  const [newWorktreeOpen, setNewWorktreeOpen] = React.useState(false)
  const submitNewWorktree = async (title: string) => {
    try {
      await api.createWorktree({
        repo: repo.path,
        base_branch: repo.primary_branch,
        title,
        host: repo.host,
      })
      refreshTree()
    } catch (e) {
      toast.error(String(e))
    }
  }
  const [confirmClose, setConfirmClose] = React.useState(false)
  const doClose = async () => {
    await api
      .closeRepo(repo.path, repo.host)
      .catch((e) => toast.error(String(e)))
    refreshTree()
  }
  return (
    <div>
      <ContextMenu>
        <ContextMenuTrigger asChild>
          <div
            className={cn(
              "flex w-full items-center gap-1 px-2 py-1 hover:bg-accent/40",
              selected && "bg-accent/60"
            )}
          >
            <button
              type="button"
              aria-label={open ? "collapse" : "expand"}
              onClick={(e) => {
                e.stopPropagation()
                setOpen((o) => !o)
              }}
              className={cn(
                "shrink-0",
                worktrees.length === 0 && "pointer-events-none opacity-0"
              )}
            >
              <ChevronRight
                className={cn(
                  "size-3 transition-transform",
                  open && "rotate-90"
                )}
              />
            </button>
            <button
              type="button"
              onClick={openRepo}
              className="flex min-w-0 flex-1 items-center gap-1.5 text-left"
            >
              {status && (
                <span
                  className={cn(
                    "size-2 shrink-0 rounded-full",
                    STATUS_DOT[status]
                  )}
                />
              )}
              <span className="truncate font-medium">{repo.name}</span>
              <span className="ml-auto flex shrink-0 items-center gap-1 pl-1 text-[11px] text-muted-foreground">
                {repo.upstream && (repo.ahead || repo.behind) ? (
                  <span className="flex items-center gap-0.5 tabular-nums">
                    {repo.ahead ? <span>↑{repo.ahead}</span> : null}
                    {repo.behind ? <span>↓{repo.behind}</span> : null}
                  </span>
                ) : null}
                <span>{repo.primary_branch}</span>
              </span>
            </button>
          </div>
        </ContextMenuTrigger>
        <ContextMenuContent>
          <ContextMenuItem
            onSelect={() =>
              openNewAgent(repo.path, repo.primary_branch, repo.host)
            }
          >
            New agent…
          </ContextMenuItem>
          <ContextMenuItem onSelect={() => setNewWorktreeOpen(true)}>
            New worktree (shell)…
          </ContextMenuItem>
          <ContextMenuSeparator />
          <ContextMenuItem onSelect={() => setRenameOpen(true)}>
            Rename…
          </ContextMenuItem>
          <ContextMenuItem
            variant="destructive"
            onSelect={() => setConfirmClose(true)}
          >
            Close repo
          </ContextMenuItem>
        </ContextMenuContent>
      </ContextMenu>

      <PromptDialog
        open={renameOpen}
        onOpenChange={setRenameOpen}
        title="Rename repo"
        placeholder="Repo display name"
        defaultValue={repo.name}
        submitLabel="Rename"
        onSubmit={submitRename}
      />

      <PromptDialog
        open={newWorktreeOpen}
        onOpenChange={setNewWorktreeOpen}
        title="New worktree"
        placeholder="Worktree / branch name"
        submitLabel="Create"
        onSubmit={submitNewWorktree}
      />

      <AlertDialog open={confirmClose} onOpenChange={setConfirmClose}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Close “{repo.name}”?</AlertDialogTitle>
            <AlertDialogDescription>
              Closing this repo ends its main checkout
              {worktrees.length > 0
                ? ` and ${worktrees.length} worktree${
                    worktrees.length === 1 ? "" : "s"
                  }`
                : ""}
              , including any running agents. The checkout on disk is kept —
              this just removes it from the sidebar.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={doClose}
              className="bg-[var(--h-bad)] text-white hover:bg-[var(--h-bad)]/90"
            >
              Close repo
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {open &&
        worktrees.map((ws) => (
          <WorkspaceNode
            key={ws.id}
            ws={ws}
            selectedTabId={selectedTabId}
            onSelectTab={onSelectTab}
            depth={2}
            delSel={delSel}
            onToggleDel={onToggleDel}
            onBulkDelete={onBulkDelete}
          />
        ))}
    </div>
  )
}

// A workspace is a single clickable leaf in the spaces tree (the tree shows
// workspaces/worktrees here, never individual tabs — tabs live in the TabStrip
// above the terminal). Its dot shows the workspace's aggregate live-agent status
// (computed client-side off the SSE status map so it updates without a refetch),
// or a terminal glyph when no agent is running.
function WorkspaceNode({
  ws,
  selectedTabId,
  onSelectTab,
  depth,
  delSel,
  onToggleDel,
  onBulkDelete,
}: {
  ws: TreeWorkspace
  selectedTabId: string | null
  onSelectTab: (tabId: string) => void
  depth: number
  delSel: Set<string>
  onToggleDel: (id: string) => void
  onBulkDelete: () => void
}) {
  const { agentStatuses } = useApp()
  const [open, setOpen] = React.useState(true)
  const tabs = ws.tabs ?? []
  const selected = tabs.some((t) => t.id === selectedTabId)
  const markedForDelete = delSel.has(ws.id)
  // Open the agent tab if there is one, else the first tab.
  const primary = tabs.find((t) => agentStatuses[t.id]) ?? tabs[0]
  // A scratch workspace expands (like a repo over its worktrees) to list its
  // *named* tabs as nested rows. Numbered tabs stay in the TabStrip only. It
  // takes two or more named tabs to expand — with just one, the single nested
  // row would only duplicate the workspace row itself (even if numbered tabs
  // also exist alongside it).
  const namedTabs = tabs.filter(isNamedTab)
  const expandable =
    ws.kind === "scratch" && depth === 1 && namedTabs.length > 1
  // When the active tab is one of the visible nested rows, let that row carry the
  // highlight; otherwise (selected tab is a hidden numbered tab) keep it on the row.
  const rowSelected =
    expandable && namedTabs.some((t) => t.id === selectedTabId)
      ? false
      : selected
  // Live workspace status: merge the SSE status of any of its tabs that are
  // running an agent (blocked > working > idle), falling back to the server's
  // computed value from the last tree fetch.
  let status: string | undefined
  for (const t of tabs) {
    status = mergeStatus(status, agentStatuses[t.id])
  }
  status = status ?? ws.agent_status

  // Rename modal + close confirmation (the latter only when the workspace has
  // more than one tab — closing it kills them all).
  const [renameOpen, setRenameOpen] = React.useState(false)
  const [confirmClose, setConfirmClose] = React.useState(false)
  const submitRename = async (title: string) => {
    await api.renameWorkspace(ws.id, title).catch((e) => toast.error(String(e)))
    refreshTree()
  }
  const doClose = async () => {
    await api.closeWorkspace(ws.id).catch((e) => toast.error(String(e)))
    refreshTree()
  }
  return (
    <>
      <ContextMenu>
        <ContextMenuTrigger asChild>
          <div
            style={{ paddingLeft: `${depth * 12}px` }}
            className={cn(
              "flex w-full items-center gap-1 py-1 pr-2 hover:bg-accent/40",
              rowSelected && "bg-accent/60",
              markedForDelete &&
                "bg-[var(--h-bad)]/15 ring-1 ring-[var(--h-bad)]/60 ring-inset"
            )}
          >
            {expandable && (
              <button
                type="button"
                aria-label={open ? "collapse" : "expand"}
                onClick={(e) => {
                  e.stopPropagation()
                  setOpen((o) => !o)
                }}
                className="shrink-0"
              >
                <ChevronRight
                  className={cn(
                    "size-3 transition-transform",
                    open && "rotate-90"
                  )}
                />
              </button>
            )}
            <button
              type="button"
              onClick={(e) => {
                // ⌘/Ctrl-click toggles this workspace in the bulk-delete selection
                // instead of opening it.
                if (e.metaKey || e.ctrlKey) {
                  e.preventDefault()
                  onToggleDel(ws.id)
                  return
                }
                if (primary) onSelectTab(primary.id)
              }}
              className="flex min-w-0 flex-1 items-center gap-1.5 text-left"
            >
              {status ? (
                <span
                  className={cn(
                    "size-2 shrink-0 rounded-full",
                    STATUS_DOT[status]
                  )}
                />
              ) : (
                <Terminal className="size-3 shrink-0 text-muted-foreground" />
              )}
              <span className="truncate">{ws.title}</span>
              {ws.branch && (
                <span className="ml-auto flex shrink-0 items-center gap-0.5 text-[11px] text-muted-foreground">
                  <GitBranch className="size-3" />
                  {ws.branch}
                </span>
              )}
            </button>
          </div>
        </ContextMenuTrigger>
        {markedForDelete ? (
          // Right-clicking a selected workspace during a multi-select offers only
          // the bulk delete (data-bulk-keep so picking it doesn't clear first).
          <ContextMenuContent data-bulk-keep>
            <ContextMenuItem variant="destructive" onSelect={onBulkDelete}>
              Delete {delSel.size} workspace{delSel.size === 1 ? "" : "s"}
            </ContextMenuItem>
          </ContextMenuContent>
        ) : (
          <ContextMenuContent>
            <ContextMenuItem
              onSelect={async () => {
                const t = await api.newTab(ws.id).catch((e) => {
                  toast.error(String(e))
                  return null
                })
                if (t) {
                  treeAddTab(ws.id, { id: t.id, title: t.title, kind: "shell" })
                  onSelectTab(t.id)
                }
                refreshTree()
              }}
            >
              New tab
            </ContextMenuItem>
            <ContextMenuItem onSelect={() => setRenameOpen(true)}>
              Rename…
            </ContextMenuItem>
            <ContextMenuSeparator />
            <ContextMenuItem onSelect={() => onToggleDel(ws.id)}>
              Select (⌘-click)
            </ContextMenuItem>
            <ContextMenuItem
              variant="destructive"
              onSelect={() => {
                // A multi-tab workspace close kills every tab — confirm first.
                if (tabs.length > 1) setConfirmClose(true)
                else doClose()
              }}
            >
              Close workspace
            </ContextMenuItem>
          </ContextMenuContent>
        )}
      </ContextMenu>

      {expandable &&
        open &&
        namedTabs.map((tab) => (
          <TabNode
            key={tab.id}
            tab={tab}
            depth={depth + 1}
            selected={tab.id === selectedTabId}
            onSelectTab={onSelectTab}
          />
        ))}

      <PromptDialog
        open={renameOpen}
        onOpenChange={setRenameOpen}
        title="Rename workspace"
        placeholder="Workspace name"
        defaultValue={ws.title}
        submitLabel="Rename"
        onSubmit={submitRename}
      />

      <AlertDialog open={confirmClose} onOpenChange={setConfirmClose}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Close “{ws.title}”?</AlertDialogTitle>
            <AlertDialogDescription>
              This workspace has {tabs.length} tabs. Closing it ends all of
              them, including any running agents. This can’t be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={doClose}
              className="bg-[var(--h-bad)] text-white hover:bg-[var(--h-bad)]/90"
            >
              Close workspace
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

// A single named tab nested under its scratch workspace in the spaces tree
// (mirrors how a worktree row sits under its repo). Clicking switches to the tab
// — kept in sync with the TabStrip via the shared selectedTabId. Right-click to
// rename or close it.
function TabNode({
  tab,
  depth,
  selected,
  onSelectTab,
}: {
  tab: TreeTab
  depth: number
  selected: boolean
  onSelectTab: (tabId: string) => void
}) {
  const { agentStatuses } = useApp()
  const status = agentStatuses[tab.id] ?? tab.status
  const [renameOpen, setRenameOpen] = React.useState(false)
  const submitRename = async (title: string) => {
    await api.renameTab(tab.id, title).catch((e) => toast.error(String(e)))
    refreshTree()
  }
  return (
    <>
      <ContextMenu>
        <ContextMenuTrigger asChild>
          <button
            type="button"
            onClick={() => onSelectTab(tab.id)}
            style={{ paddingLeft: `${depth * 12}px` }}
            className={cn(
              "flex w-full items-center gap-1.5 py-1 pr-2 text-left hover:bg-accent/40",
              selected && "bg-accent/60"
            )}
          >
            {tab.kind === "agent" && status ? (
              <span
                className={cn(
                  "size-2 shrink-0 rounded-full",
                  STATUS_DOT[status]
                )}
              />
            ) : (
              <Terminal className="size-3 shrink-0 text-muted-foreground" />
            )}
            <span className="truncate">{tab.title}</span>
          </button>
        </ContextMenuTrigger>
        <ContextMenuContent>
          <ContextMenuItem onSelect={() => setRenameOpen(true)}>
            Rename…
          </ContextMenuItem>
          <ContextMenuSeparator />
          <ContextMenuItem
            variant="destructive"
            onSelect={async () => {
              await api.closeTab(tab.id).catch((e) => toast.error(String(e)))
              refreshTree()
            }}
          >
            Close tab
          </ContextMenuItem>
        </ContextMenuContent>
      </ContextMenu>

      <PromptDialog
        open={renameOpen}
        onOpenChange={setRenameOpen}
        title="Rename tab"
        placeholder="Tab name"
        defaultValue={tab.title}
        submitLabel="Rename"
        onSubmit={submitRename}
      />
    </>
  )
}

// mergeStatus keeps the most attention-worthy of two agent statuses (blocked >
// working > idle); either may be undefined.
function mergeStatus(a: string | undefined, b: string | undefined) {
  const rank: Record<string, number> = { blocked: 3, working: 2, idle: 1 }
  return (rank[b ?? ""] ?? 0) > (rank[a ?? ""] ?? 0) ? b : a
}

function AgentRowItem({
  agent,
  selected,
  status,
  siblingNumeric,
  onSelect,
}: {
  agent: AgentRow
  selected: boolean
  status: string
  siblingNumeric: number
  onSelect: () => void
}) {
  const [renameOpen, setRenameOpen] = React.useState(false)
  const submitRename = async (title: string) => {
    await api
      .renameTab(agent.tab_id, title)
      .catch((e) => toast.error(String(e)))
    refreshTree()
  }
  // Scratch-task agents live in a numbered tab ("1", "2"…) whose title alone is
  // meaningless out of context, so surface the workspace name. With a single
  // numeric tab in the workspace the number adds nothing — just show "52 Labs";
  // with two or more, keep the number to disambiguate ("52 Labs 1" / "52 Labs
  // 2"). Repo agents and lasso-created scratch agents (whose title already IS the
  // workspace name) are left as-is — no doubled "52 Labs 52 Labs".
  const displayTitle =
    !agent.repo &&
    agent.workspace_title &&
    agent.title !== agent.workspace_title
      ? siblingNumeric === 1 && /^\d+$/.test(agent.title)
        ? agent.workspace_title
        : `${agent.workspace_title} ${agent.title}`
      : agent.title
  return (
    <>
      <ContextMenu>
        <ContextMenuTrigger asChild>
          <button
            type="button"
            onClick={onSelect}
            className={cn(
              "flex w-full items-center gap-1.5 px-3 py-1 text-left hover:bg-accent/40",
              selected && "bg-accent/60"
            )}
          >
            <span
              className={cn("size-2 shrink-0 rounded-full", STATUS_DOT[status])}
            />
            <span className="truncate">{displayTitle}</span>
            <span className="ml-auto shrink-0 text-[10px] text-muted-foreground">
              {status} · {agent.agent}
            </span>
          </button>
        </ContextMenuTrigger>
        <ContextMenuContent>
          <ContextMenuItem onSelect={() => setRenameOpen(true)}>
            Rename…
          </ContextMenuItem>
          <ContextMenuItem
            variant="destructive"
            onSelect={async () => {
              await api
                .closeAgent(agent.tab_id)
                .catch((e) => toast.error(String(e)))
              refreshTree()
            }}
          >
            Close agent
          </ContextMenuItem>
        </ContextMenuContent>
      </ContextMenu>

      <PromptDialog
        open={renameOpen}
        onOpenChange={setRenameOpen}
        title="Rename agent"
        placeholder="Agent name"
        defaultValue={agent.title}
        submitLabel="Rename"
        onSubmit={submitRename}
      />
    </>
  )
}
