import { useQuery } from "@tanstack/react-query"
import { api } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { qk } from "@/lib/query"

// Shared git-status query for the active repo's working tree. Polled app-wide
// (not just while the Files panel is open) so the tab badge and the
// collapsed-sidebar indicator stay live even when the file viewer is hidden or
// the sidebar starts collapsed. Multiple callers share a single poll via the
// query cache, keyed on the cwd's host + the cwd itself — the cwd belongs to
// the focused pane, which can sit on another host than the active one (an ssh
// window), so keying on the active host could serve one host's status for
// another's cwd.
export function useDiff() {
  const { activeCwd, cwdHost } = useApp()
  return useQuery({
    queryKey: qk.diff(cwdHost ?? "", activeCwd ?? ""),
    queryFn: () => api.diff(activeCwd as string, cwdHost ?? undefined),
    enabled: !!activeCwd,
    // A 2.5s poll retries by definition; the client-wide retry:1 only doubled the
    // request rate. And a genuine failure here is an ssh exec against a host that
    // isn't answering — back off rather than re-dial it 24x a minute.
    retry: false,
    refetchInterval: (q) => (q.state.status === "error" ? 15_000 : 2_500),
    staleTime: 1500,
  })
}
