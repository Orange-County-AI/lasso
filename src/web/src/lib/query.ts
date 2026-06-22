import { QueryClient } from "@tanstack/react-query"

// One shared QueryClient for the app's server state. Exported (not just provided)
// so non-component code — the SSE host-change handler in app-store — can
// invalidate queries when the active host switches.
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

// Centralized query keys for the agent-creation data flow. Config keys embed the
// host, since each host's settings live in its own lasso.db and the Settings tab
// can address any host — invalidateHostScoped clears every host by key prefix.
export const qk = {
  agentConfig: (host: string) => ["agent-config", host] as const,
  repos: (host: string) => ["repos", host] as const,
  repoBranches: (host: string, path: string) =>
    ["repo-branches", host, path] as const,
  grid: ["grid"] as const,
  agentHistory: ["agent-history"] as const,
  diff: (host: string, path: string) => ["diff", host, path] as const,
  uiState: ["ui-state"] as const,
  sidebarPct: ["sidebar-pct"] as const,
  version: ["version"] as const,
}

// invalidateHostScoped refetches everything tied to a host, called when the
// active host switches so the creator reloads the new host's remembered
// selections. Matches every host's config by key prefix.
export function invalidateHostScoped() {
  queryClient.invalidateQueries({ queryKey: ["agent-config"] })
  queryClient.invalidateQueries({ queryKey: ["repos"] })
  queryClient.invalidateQueries({ queryKey: ["repo-branches"] })
  queryClient.invalidateQueries({ queryKey: qk.version })
}
