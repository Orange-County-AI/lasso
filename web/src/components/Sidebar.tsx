import { useQuery } from "@tanstack/react-query"
import { ChevronRight, GitBranch, Pin, Plus, Terminal } from "lucide-react"
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
  type TreeRepo,
  type TreeWorkspace,
} from "@/lib/api"
import { useApp } from "@/lib/app-store"
import {
  qk,
  queryClient,
  treeAddScratchWorkspace,
  treeAddTab,
  treeSetRepoMain,
} from "@/lib/query"
import { cn } from "@/lib/utils"

// The left sidebar: a "spaces" tree (git repos with their worktrees nested,
// ordered by latest commit, pinned first; plus scratch workspaces) over an
// "agents" pane that shows every agent's live status. Replaces herdr's TUI
// sidebar. Selecting a tab/agent shows its terminal in the center.

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

// openNewAgent asks the (always-mounted) CreateAgentDialog to open prefilled with
// a repo + base, via a window event so the sidebar doesn't own the dialog.
function openNewAgent(repo: string, base: string) {
  window.dispatchEvent(
    new CustomEvent("lasso:new-agent", { detail: { repo, base } })
  )
}

export function Sidebar({
  selectedTabId,
  onSelectTab,
}: {
  selectedTabId: string | null
  onSelectTab: (tabId: string) => void
}) {
  const { panesRev, agentStatuses } = useApp()
  const tree = useQuery({ queryKey: qk.tree, queryFn: api.tree })
  const agents = useQuery({ queryKey: qk.agents, queryFn: api.agentsList })

  // Refetch the tree + agents whenever the workspace/tab layout changes (SSE).
  // biome-ignore lint/correctness/useExhaustiveDependencies: panesRev is the refetch trigger
  React.useEffect(() => {
    if (panesRev >= 0) refreshTree()
  }, [panesRev])

  // The new-workspace name modal (opened by the footer "+" and the empty-area
  // right-click). Submitting creates a bare scratch workspace (a shell, no agent)
  // and focuses it.
  const [newWsOpen, setNewWsOpen] = React.useState(false)
  const submitNewWorkspace = async (title: string) => {
    try {
      const { workspace_id, tab_id, work_dir } =
        await api.createWorkspace(title)
      treeAddScratchWorkspace({
        id: workspace_id,
        title,
        work_dir,
        kind: "scratch",
        pinned: false,
        tabs: [{ id: tab_id, title: "shell", kind: "shell" }],
      })
      onSelectTab(tab_id)
      refreshTree()
    } catch (e) {
      toast.error(String(e))
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
              <SectionLabel>spaces</SectionLabel>
              {(tree.data?.scratch ?? []).map((ws) => (
                <WorkspaceNode
                  key={ws.id}
                  ws={ws}
                  selectedTabId={selectedTabId}
                  onSelectTab={onSelectTab}
                  depth={1}
                />
              ))}
              {(tree.data?.repos ?? []).map((repo) => (
                <RepoNode
                  key={repo.path}
                  repo={repo}
                  selectedTabId={selectedTabId}
                  onSelectTab={onSelectTab}
                />
              ))}
            </div>
          </ContextMenuTrigger>
          <ContextMenuContent>
            <ContextMenuItem onSelect={() => setNewWsOpen(true)}>
              New workspace…
            </ContextMenuItem>
          </ContextMenuContent>
        </ContextMenu>
        <button
          type="button"
          title="New workspace"
          aria-label="New workspace"
          onClick={() => setNewWsOpen(true)}
          className="absolute right-1.5 bottom-1.5 flex size-5 items-center justify-center rounded text-muted-foreground/60 hover:bg-accent hover:text-foreground"
        >
          <Plus className="size-3.5" />
        </button>
        <PromptDialog
          open={newWsOpen}
          onOpenChange={setNewWsOpen}
          title="New workspace"
          placeholder="Workspace name"
          submitLabel="Create"
          onSubmit={submitNewWorkspace}
        />
      </div>

      <div className="max-h-[40%] min-h-0 shrink-0 overflow-y-auto border-border border-t">
        <SectionLabel>agents</SectionLabel>
        {(agents.data?.agents ?? []).map((a) => (
          <AgentRowItem
            key={a.tab_id}
            agent={a}
            selected={a.tab_id === selectedTabId}
            status={agentStatuses[a.tab_id] ?? a.status}
            onSelect={() => onSelectTab(a.tab_id)}
          />
        ))}
        {agents.data && (agents.data.agents ?? []).length === 0 && (
          <div className="px-3 py-2 text-muted-foreground">no agents</div>
        )}
      </div>
    </div>
  )
}

function SectionLabel({ children }: { children: React.ReactNode }) {
  return (
    <div className="px-3 pt-3 pb-1 font-semibold text-[11px] text-muted-foreground uppercase tracking-wider">
      {children}
    </div>
  )
}

function RepoNode({
  repo,
  selectedTabId,
  onSelectTab,
}: {
  repo: TreeRepo
  selectedTabId: string | null
  onSelectTab: (tabId: string) => void
}) {
  const { agentStatuses } = useApp()
  const [open, setOpen] = React.useState(true)
  const worktrees = repo.workspaces ?? []
  const status =
    (repo.main_tab_id && agentStatuses[repo.main_tab_id]) || repo.agent_status
  const selected = !!repo.main_tab_id && repo.main_tab_id === selectedTabId

  // Clicking the repo row opens a terminal on its primary branch (the main
  // checkout) — like herdr, a repo is a workspace, not just a worktree grouping.
  const openRepo = async () => {
    if (repo.main_tab_id) {
      onSelectTab(repo.main_tab_id)
      return
    }
    try {
      const { tab_id, workspace_id } = await api.openRepo(repo.path)
      // Surface the main checkout immediately so the tab strip resolves it.
      treeSetRepoMain(repo.path, {
        id: workspace_id,
        title: repo.name,
        repo: repo.path,
        work_dir: repo.path,
        kind: "git",
        pinned: false,
        branch: repo.primary_branch,
        tabs: [{ id: tab_id, title: "shell", kind: "shell" }],
      })
      onSelectTab(tab_id)
      refreshTree()
    } catch (e) {
      toast.error(String(e))
    }
  }
  const rename = async () => {
    const name = window.prompt("Repo display name:", repo.name)
    if (name == null) return
    await api
      .renameRepo(repo.path, name.trim())
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
              {repo.pinned && (
                <Pin className="size-3 shrink-0 text-[var(--h-accent)]" />
              )}
              {status && (
                <span
                  className={cn(
                    "size-2 shrink-0 rounded-full",
                    STATUS_DOT[status]
                  )}
                />
              )}
              <span className="truncate font-medium">{repo.name}</span>
              <span className="ml-auto shrink-0 pl-1 text-[11px] text-muted-foreground">
                {repo.primary_branch}
              </span>
            </button>
          </div>
        </ContextMenuTrigger>
        <ContextMenuContent>
          <ContextMenuItem
            onSelect={() => openNewAgent(repo.path, repo.primary_branch)}
          >
            New agent…
          </ContextMenuItem>
          <ContextMenuItem
            onSelect={async () => {
              const title = window.prompt("Worktree / branch name:")
              if (!title || !title.trim()) return
              try {
                await api.createWorktree({
                  repo: repo.path,
                  base_branch: repo.primary_branch,
                  title: title.trim(),
                })
                refreshTree()
              } catch (e) {
                toast.error(String(e))
              }
            }}
          >
            New worktree (shell)…
          </ContextMenuItem>
          <ContextMenuSeparator />
          <ContextMenuItem
            onSelect={async () => {
              await api
                .pinRepo(repo.path, !repo.pinned)
                .catch((e) => toast.error(String(e)))
              refreshTree()
            }}
          >
            {repo.pinned ? "Unpin" : "Pin to top"}
          </ContextMenuItem>
          <ContextMenuItem onSelect={rename}>Rename…</ContextMenuItem>
        </ContextMenuContent>
      </ContextMenu>
      {open &&
        worktrees.map((ws) => (
          <WorkspaceNode
            key={ws.id}
            ws={ws}
            selectedTabId={selectedTabId}
            onSelectTab={onSelectTab}
            depth={2}
          />
        ))}
    </div>
  )
}

// A workspace is a single clickable leaf in the spaces tree (herdr shows
// workspaces/worktrees here, never individual tabs — tabs live in the TabStrip
// above the terminal). Its dot shows the workspace's aggregate live-agent status
// (computed client-side off the SSE status map so it updates without a refetch),
// or a terminal glyph when no agent is running.
function WorkspaceNode({
  ws,
  selectedTabId,
  onSelectTab,
  depth,
}: {
  ws: TreeWorkspace
  selectedTabId: string | null
  onSelectTab: (tabId: string) => void
  depth: number
}) {
  const { agentStatuses } = useApp()
  const tabs = ws.tabs ?? []
  const selected = tabs.some((t) => t.id === selectedTabId)
  // Open the agent tab if there is one, else the first tab.
  const primary = tabs.find((t) => agentStatuses[t.id]) ?? tabs[0]
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
          <button
            type="button"
            onClick={() => primary && onSelectTab(primary.id)}
            style={{ paddingLeft: `${depth * 12}px` }}
            className={cn(
              "flex w-full items-center gap-1.5 py-1 pr-2 text-left hover:bg-accent/40",
              selected && "bg-accent/60"
            )}
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
        </ContextMenuTrigger>
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
      </ContextMenu>

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
  onSelect,
}: {
  agent: AgentRow
  selected: boolean
  status: string
  onSelect: () => void
}) {
  const rename = async () => {
    const title = window.prompt("Rename agent:", agent.title)
    if (title == null || !title.trim()) return
    await api
      .renameTab(agent.tab_id, title.trim())
      .catch((e) => toast.error(String(e)))
    refreshTree()
  }
  return (
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
          <span className="truncate">{agent.title}</span>
          <span className="ml-auto shrink-0 text-[10px] text-muted-foreground">
            {status} · {agent.agent}
          </span>
        </button>
      </ContextMenuTrigger>
      <ContextMenuContent>
        <ContextMenuItem onSelect={rename}>Rename…</ContextMenuItem>
        <ContextMenuItem
          variant="destructive"
          onSelect={async () => {
            await api
              .closeTab(agent.tab_id)
              .catch((e) => toast.error(String(e)))
            refreshTree()
          }}
        >
          Close agent
        </ContextMenuItem>
      </ContextMenuContent>
    </ContextMenu>
  )
}
