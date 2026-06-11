import { QueryClient } from "@tanstack/react-query"
import type { TreePayload, TreeTab, TreeWorkspace } from "@/lib/api"

// One shared QueryClient for the app's server state. Exported (not just provided)
// so non-component code — the SSE handler in app-store — can invalidate queries.
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // The creator data (repos, branches, config) changes rarely; a short stale
      // window avoids refetching on every dialog open while staying fresh.
      staleTime: 30_000,
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
})

// Centralized query keys for the agent-creation data flow. The backend is
// local-only; the host segment is always "local" and kept only to preserve the
// existing key shape.
export const qk = {
  agentConfig: (host: string) => ["agent-config", host] as const,
  repos: (host: string) => ["repos", host] as const,
  repoBranches: (host: string, path: string) =>
    ["repo-branches", host, path] as const,
  tree: ["tree"] as const,
  agents: ["agents"] as const,
  diff: (host: string, path: string) => ["diff", host, path] as const,
  version: ["version"] as const,
  // Multi-host: the host list, the cross-host grid, and persisted UI prefs.
  hosts: ["hosts"] as const,
  grid: ["grid"] as const,
  uiState: ["ui-state"] as const,
}

// Optimistic tree edits: a freshly-created tab/workspace is written into the
// cached /api/tree immediately so the tab strip + sidebar render it without
// waiting for the (slower) refetch. The next refetch reconciles authoritatively.

// treeAddTab appends a tab to an existing workspace (the "+" new-tab flow).
export function treeAddTab(workspaceId: string, tab: TreeTab) {
  queryClient.setQueryData<TreePayload>(qk.tree, (old) => {
    if (!old) return old
    const add = (w: TreeWorkspace): TreeWorkspace =>
      w.id === workspaceId ? { ...w, tabs: [...(w.tabs ?? []), tab] } : w
    return {
      ...old,
      scratch: (old.scratch ?? []).map(add),
      repos: (old.repos ?? []).map((r) => ({
        ...r,
        workspaces: (r.workspaces ?? []).map(add),
        main_workspace: r.main_workspace
          ? add(r.main_workspace)
          : r.main_workspace,
      })),
    }
  })
}

// Stable keys for the unified "spaces" order (kept in sync with the backend's
// spacesKeyWorkspace/spacesKeyRepo in sidebar.go).
export const spacesKeyWorkspace = (id: string) => `ws:${id}`
export const spacesKeyRepo = (path: string) => `repo:${path}`

// treeAddScratchWorkspace appends a new scratch workspace. It's intentionally NOT
// added to `order`, so the sidebar renders it at the bottom (matching "new
// workspaces go to the bottom"); the next refetch reconciles the authoritative
// order with the new key appended server-side.
export function treeAddScratchWorkspace(ws: TreeWorkspace) {
  queryClient.setQueryData<TreePayload>(qk.tree, (old) => {
    if (!old || (old.scratch ?? []).some((w) => w.id === ws.id)) return old
    return { ...old, scratch: [...(old.scratch ?? []), ws] }
  })
}

// treeReorderSpaces optimistically applies a drag-and-drop reordering so the drop
// reflects immediately, before the refetch confirms it.
export function treeReorderSpaces(order: string[]) {
  queryClient.setQueryData<TreePayload>(qk.tree, (old) =>
    old ? { ...old, order } : old
  )
}

// treeAddWorktree adds a worktree workspace under an already-listed repo. (If the
// repo isn't in the tree yet, the refetch will surface it.) Skips the insert when
// a workspace with this id is already in the tree — the server persists the row
// before createAgent returns, so a refetch can land it first; appending blindly
// would render the same worktree twice until the next refetch reconciled it.
export function treeAddWorktree(repoPath: string, ws: TreeWorkspace) {
  queryClient.setQueryData<TreePayload>(qk.tree, (old) => {
    if (!old) return old
    const present = (old.repos ?? []).some((r) =>
      (r.workspaces ?? []).some((w) => w.id === ws.id)
    )
    if (present) return old
    return {
      ...old,
      repos: (old.repos ?? []).map((r) =>
        r.path === repoPath
          ? { ...r, workspaces: [...(r.workspaces ?? []), ws] }
          : r
      ),
    }
  })
}

// treeSetRepoMain attaches a freshly-opened main-checkout workspace to its repo.
export function treeSetRepoMain(repoPath: string, ws: TreeWorkspace) {
  queryClient.setQueryData<TreePayload>(qk.tree, (old) => {
    if (!old) return old
    return {
      ...old,
      repos: (old.repos ?? []).map((r) =>
        r.path === repoPath
          ? {
              ...r,
              main_workspace: ws,
              main_workspace_id: ws.id,
              main_tab_id: ws.tabs?.[0]?.id,
            }
          : r
      ),
    }
  })
}

// invalidateHostScoped refetches the creator data + version, called when the
// backend signals terminals must reload so the creator picks up fresh state.
export function invalidateHostScoped() {
  queryClient.invalidateQueries({ queryKey: ["agent-config"] })
  queryClient.invalidateQueries({ queryKey: ["repos"] })
  queryClient.invalidateQueries({ queryKey: ["repo-branches"] })
  queryClient.invalidateQueries({ queryKey: qk.version })
  // The sidebar tree and the grid are scoped to the active host, so re-scope
  // them when the host changes.
  queryClient.invalidateQueries({ queryKey: qk.tree })
  queryClient.invalidateQueries({ queryKey: qk.agents })
  queryClient.invalidateQueries({ queryKey: qk.grid })
}
