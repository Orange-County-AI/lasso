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

// Centralized query keys for the agent-creation data flow. host-scoped data
// (agentConfig, repos, branches, agents) is invalidated wholesale on a host
// switch, so the keys don't embed the host — invalidation clears them all.
export const qk = {
  agentConfig: ["agent-config"] as const,
  repos: ["repos"] as const,
  repoBranches: (path: string) => ["repo-branches", path] as const,
  agents: ["agents"] as const,
  grid: ["grid"] as const,
  version: ["version"] as const,
}

// invalidateHostScoped refetches everything tied to the active host, called when
// the host switches so the creator reloads the new host's remembered selections.
export function invalidateHostScoped() {
  queryClient.invalidateQueries({ queryKey: qk.agentConfig })
  queryClient.invalidateQueries({ queryKey: qk.repos })
  queryClient.invalidateQueries({ queryKey: ["repo-branches"] })
  queryClient.invalidateQueries({ queryKey: qk.agents })
  queryClient.invalidateQueries({ queryKey: qk.version })
}
