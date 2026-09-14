import { useQuery } from "@tanstack/react-query"
import * as React from "react"
import { toast } from "sonner"

import { type Pane, api } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { qk } from "@/lib/query"

// What a human calls an agent, in the order herdr labels the pane: the workspace
// (the name lasso's creator set, and what auto-titling rewrites), then the tab,
// then the pane id as a last resort so a row is never blank.
export function agentName(p: Pane): string {
  return p.workspace_label || p.tab_label || p.pane_id
}

// The harness and the last cwd segment — which for an agent is the worktree it
// was started in. The second half of "which one is this".
export function agentSub(p: Pane): string {
  return [p.agent, (p.cwd ?? "").split("/").filter(Boolean).pop()]
    .filter(Boolean)
    .join(" · ")
}

// The agents running on this tab's host, and the one action both surfaces offer
// on them: focus.
//
// Two callers share it — the docked column (App.tsx) and the phone's sheet
// (ChatView.tsx) — and they have to agree on every part of it: the same list,
// the same stand-in highlight, the same address. They are never both on screen
// (one is md+ and the other is not), so the second observer costs a shared
// cache entry rather than a second poll.
//
// Focus stays herdr's, not this hook's: a selection focuses the pane the way the
// rest of lasso does (api.focus), and the chat then follows the same pane_id
// over SSE that the terminal does. So the highlight is not a second source of
// truth — a row is current because herdr says that pane is focused, with the
// selected one standing in for it only until that answer arrives.
export function useAgents(host: string | null) {
  const { activePaneID, panesRev } = useApp()
  const { data, isLoading, error } = useQuery({
    // panes_rev covers the list itself (a create, a close, a rename); the
    // interval covers what a layout revision cannot: an agent's STATUS moves
    // without one, and a row still reading "working" for a turn that ended is
    // the one way a list can lie.
    queryKey: qk.panes(host ?? "", panesRev),
    queryFn: () => api.panes(),
    refetchInterval: 4000,
    refetchIntervalInBackground: false,
  })

  // The pane focus is heading to. The host feed reports herdr's focus on an
  // event or its 2s poll, so a selection would otherwise look like it did
  // nothing for a beat; the highlight prefers this until the real answer
  // catches up.
  const [pending, setPending] = React.useState<string | null>(null)
  // Retired when that answer moves — which is what it stood in for — or after 3s
  // of silence: a focus that never lands (a split tab whose active pane is
  // another pane, a refusal) must not leave the wrong row looking current.
  // biome-ignore lint/correctness/useExhaustiveDependencies: activePaneID is the trigger, not an input — the effect exists to RUN when herdr's focus moves.
  React.useEffect(() => {
    setPending(null)
  }, [activePaneID])
  React.useEffect(() => {
    if (!pending) return
    const t = setTimeout(() => setPending(null), 3000)
    return () => clearTimeout(t)
  }, [pending])

  const focusAgent = React.useCallback(async (p: Pane) => {
    if (!p.workspace_id) return
    setPending(p.pane_id)
    try {
      // Reached the way the rest of lasso reaches a pane: herdr has no
      // pane.focus, so a pane is focused through its workspace and then its tab
      // (see serveFocus). A split tab therefore lands on the tab's active pane.
      await api.focus(p.workspace_id, p.tab_id)
    } catch (e) {
      // Nothing is on its way, so drop the stand-in now rather than at the next
      // focus change, which may never come.
      setPending(null)
      toast.error(`could not focus ${agentName(p)}: ${(e as Error).message}`)
    }
  }, [])

  return {
    agents: (data?.panes ?? []).filter((p) => p.agent),
    isLoading,
    error,
    // The pane the highlight belongs on: the selection in flight, else herdr's
    // own answer. Both surfaces take it from here so they cannot disagree.
    current: pending ?? activePaneID,
    focusAgent,
  }
}
