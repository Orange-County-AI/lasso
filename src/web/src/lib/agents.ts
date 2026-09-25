import { useQuery, useQueryClient } from "@tanstack/react-query"
import * as React from "react"
import { toast } from "sonner"

import { type AgentSort, api, type HostPane } from "@/lib/api"
import { moveTabToHost, useApp } from "@/lib/app-store"
import { qk } from "@/lib/query"

// paneKey is an identity for a pane across the whole fleet. herdr's own ids are
// unique only WITHIN a host, and this list spans every machine lasso can reach,
// so every address here — the highlight, the stand-in for a selection in flight,
// a React key — carries the host with the id. Two machines each running an agent
// called "w1:p1" are two rows.
export function paneKey(p: HostPane): string {
  return `${p.host}\u0000${p.pane_id}`
}

// What a human calls an agent, in the order herdr labels the pane: the workspace
// (the name lasso's creator set, and what auto-titling rewrites), then the pane's
// own label, then its tab, then the terminal title — which for an agent is what
// it is working on, and the only name left when nothing along the way was ever
// labelled.
export function agentName(p: HostPane): string {
  return (
    p.workspace_label ||
    p.pane_label ||
    p.tab_label ||
    p.terminal_title ||
    p.pane_id
  )
}

// The harness and the last cwd segment — which for an agent is the worktree it
// was started in. The second half of "which one is this", after its machine.
export function agentSub(p: HostPane): string {
  return [p.agent, (p.cwd ?? "").split("/").filter(Boolean).pop()]
    .filter(Boolean)
    .join(" · ")
}

// orderByHost puts this tab's own machine first and the rest in name order,
// keeping the payload's order inside each host. The aggregation's own order is
// herdr-target order across an ssh config, which is neither stable enough to tap
// at between polls nor any statement about where the reader is.
function orderByHost(panes: HostPane[], tabHost: string | null): HostPane[] {
  return [...panes].sort((a, b) => {
    if (a.host === b.host) return 0
    const rank = (p: HostPane) => (p.host === tabHost ? 0 : 1)
    if (rank(a) !== rank(b)) return rank(a) - rank(b)
    return (a.host_label || a.host).localeCompare(b.host_label || b.host)
  })
}
// Attention order for parallel monitoring: an agent that stopped for an answer
// (blocked) outranks one still producing (working), which outranks rest (idle),
// which outranks a finished turn nobody has looked at since (done). Unknown and
// unreported statuses sort last. Same concept as herdr's own agent list, which
// surfaces what needs a human first.
export function agentStatusRank(status?: string): number {
  switch (status) {
    case "blocked":
      return 0
    case "working":
      return 1
    case "idle":
      return 2
    case "done":
      return 3
    default:
      return 4
  }
}

// Numeric collation, so "agent-2" precedes "agent-10" rather than following
// it. One instance: constructing an Intl.Collator per comparison is the
// expensive way to do this, and a sort over N agents calls the comparator
// O(N log N) times on every poll.
const byName = new Intl.Collator(undefined, {
  numeric: true,
  sensitivity: "base",
})

// A total order over the fleet, used as the tiebreak under BOTH sorts. Name
// alone is not total — two agents genuinely share a name often enough (two
// worktrees of one repo, two panes herdr never labelled) — and a comparator
// that returns 0 for them leaves their relative order to the engine's sort,
// which is free to differ between two arrays holding the same elements. That
// is a pair of cards trading places on a poll where nothing changed, which is
// exactly what "alpha" exists to prevent.
function tiebreak(a: HostPane, b: HostPane): number {
  const byHost = (a.host_label || a.host).localeCompare(b.host_label || b.host)
  if (byHost !== 0) return byHost
  return a.pane_id.localeCompare(b.pane_id)
}

// Priority sort within one surface: status rank first, then name, then the
// total-order tiebreak so rows do not jump between polls.
export function sortAgentsByPriority(panes: HostPane[]): HostPane[] {
  return [...panes].sort((a, b) => {
    const byRank =
      agentStatusRank(a.agent_status) - agentStatusRank(b.agent_status)
    if (byRank !== 0) return byRank
    const byLabel = byName.compare(agentName(a), agentName(b))
    if (byLabel !== 0) return byLabel
    return tiebreak(a, b)
  })
}

// Name order, ignoring status entirely. The point is what it does NOT do: the
// order changes only when an agent is created, closed or renamed, so a card
// stays where the reader last saw it while its agent blocks, answers and
// finishes. Status still reaches them — through the card's own badge and the
// group header's "1 blocked · 2 working" counts — it just stops moving things.
export function sortAgentsAlpha(panes: HostPane[]): HostPane[] {
  return [...panes].sort((a, b) => {
    const byLabel = byName.compare(agentName(a), agentName(b))
    if (byLabel !== 0) return byLabel
    return tiebreak(a, b)
  })
}

// The one entry point both orders go through, so a surface picks a mode rather
// than picking a function.
export function sortAgents(panes: HostPane[], sort: AgentSort): HostPane[] {
  return sort === "alpha" ? sortAgentsAlpha(panes) : sortAgentsByPriority(panes)
}

export interface AgentHostGroup {
  host: string
  hostLabel: string
  panes: HostPane[]
  blocked: number
  working: number
}

// Grouped for the parallel grid: one section per herdr machine, this tab's own
// machine first and the rest in name order, agents inside each group in the
// caller's chosen order. Groups (not a flat fleet sort) because a card's
// transcript is read from that machine — the header says where every composer
// below it sends.
//
// The GROUP order is deliberately not a sort mode: which machine a section
// belongs to never changes on its own, so the sections already hold still
// under either sort, and the reader's own machine leading is orientation
// rather than priority.
export function groupAgentsByHost(
  agents: HostPane[],
  tabHost: string | null,
  sort: AgentSort = "priority"
): AgentHostGroup[] {
  const byHost = new Map<string, HostPane[]>()
  for (const p of agents) {
    const list = byHost.get(p.host)
    if (list) list.push(p)
    else byHost.set(p.host, [p])
  }
  const groups: AgentHostGroup[] = [...byHost].map(([host, panes]) => ({
    host,
    hostLabel: panes[0]?.host_label || host,
    panes: sortAgents(panes, sort),
    blocked: panes.filter((p) => p.agent_status === "blocked").length,
    working: panes.filter((p) => p.agent_status === "working").length,
  }))
  return groups.sort((a, b) => {
    const rank = (g: AgentHostGroup) => (g.host === tabHost ? 0 : 1)
    if (rank(a) !== rank(b)) return rank(a) - rank(b)
    return a.hostLabel.localeCompare(b.hostLabel)
  })
}

// Every agent lasso can reach, on every connected machine — not just this tab's
// host. An agent on another box is the one you cannot see any other way short of
// switching tabs, which is the whole point of listing the fleet.
//
// Two surfaces render it (the docked column and the phone's sheet) and they have
// to agree on every part of it: the same list, the same naming, the same
// stand-in highlight, the same address. They are never both on screen (one is
// md+ and the other is not), so the second observer costs a shared cache entry
// rather than a second poll.
//
// Focus stays herdr's, not this hook's: a selection moves the tab when the agent
// is elsewhere and then focuses the pane the way the rest of lasso does, and the
// chat follows the same pane_id over SSE that the terminal does. So the
// highlight is not a second source of truth — a row is current because herdr
// says that pane is focused, with the selected one standing in for it only until
// that answer arrives.
export function useAgents() {
  const { activePaneID, host: tabHost, panesRev } = useApp()
  const queryClient = useQueryClient()
  const { data, isLoading, error } = useQuery({
    // panes_rev is this TAB's host's revision, so it covers a create, a close or
    // a rename on the machine the reader is looking at without waiting for the
    // poll; the interval covers everything else — the other hosts, and the one
    // thing no layout revision carries, an agent's STATUS moving (which is the
    // one way a list like this can lie).
    queryKey: qk.allPanes(panesRev),
    queryFn: () => api.allPanes(),
    refetchInterval: 5000,
    refetchIntervalInBackground: false,
  })

  // The pane focus is heading to. The host feed reports herdr's focus on an
  // event or its 2s poll, and a cross-host selection has a whole attach in front
  // of that, so a tap would otherwise look like it did nothing for a beat; the
  // highlight prefers this until the real answer catches up.
  const [pending, setPending] = React.useState<string | null>(null)
  // Retired when that answer moves — which is what it stood in for — or after 3s
  // of silence: a focus that never lands (an unreachable host, a split tab whose
  // active pane is another pane, a refusal) must not leave the wrong row looking
  // current.
  // biome-ignore lint/correctness/useExhaustiveDependencies: activePaneID is the trigger, not an input — the effect exists to RUN when herdr's focus (or the tab's host) moves.
  React.useEffect(() => {
    setPending(null)
  }, [activePaneID, tabHost])
  React.useEffect(() => {
    if (!pending) return
    const t = setTimeout(() => setPending(null), 3000)
    return () => clearTimeout(t)
  }, [pending])

  const focusAgent = React.useCallback(async (p: HostPane) => {
    if (!p.workspace_id) return
    setPending(paneKey(p))
    try {
      // The TAB moves first when the agent is on another machine. Two reasons,
      // in order: the focus call is addressed to the host this tab is on
      // (lib/host), and the chat follows that host's focused pane — so attaching
      // after focusing would land the focus on a host the chat is not showing.
      // focusCreatedAgent and the creator's own success path take the same two
      // steps in the same order.
      await moveTabToHost(p.host)
      // pane.focus lands on THIS pane, not merely its tab: a split tab's two
      // agents are two rows here, and focusing the tab landed on whichever of
      // them was active. workspace_id/tab_id ride along as the server's
      // fallback for a pane closed since this listing (see api.focus).
      await api.focus({
        pane_id: p.pane_id,
        workspace_id: p.workspace_id,
        tab_id: p.tab_id,
      })
    } catch (e) {
      // Nothing is on its way, so drop the stand-in now rather than at the next
      // focus change, which may never come.
      setPending(null)
      toast.error(`could not focus ${agentName(p)}: ${(e as Error).message}`)
    }
  }, [])

  // Ending the agent in a pane — herdr's own pane.close, the same call the chat's
  // header makes. It lives here rather than in the surface that offers it for the
  // reason focusAgent does: the address is the row's (host + pane id, unique only
  // together), and the list that has to forget the row is this hook's. panes_rev
  // covers a close on the tab's OWN host; another machine's is carried only by the
  // 5s poll, so the cache is dropped outright and the row goes now either way.
  const closeAgent = React.useCallback(
    async (p: HostPane) => {
      try {
        await api.close([p.pane_id], p.host)
      } catch (e) {
        toast.error(`could not close ${agentName(p)}: ${(e as Error).message}`)
        return
      }
      await queryClient.invalidateQueries({ queryKey: qk.allPanesAny })
    },
    [queryClient]
  )

  const agents = React.useMemo(
    () =>
      orderByHost(
        (data?.panes ?? []).filter((p) => p.has_agent),
        tabHost
      ),
    [data, tabHost]
  )

  return {
    agents,
    isLoading,
    error,
    // Hosts the aggregation could not list this pass. They contribute no rows,
    // and saying so is the difference between "that agent is gone" and "that
    // machine did not answer".
    unlisted: Object.keys(data?.errors ?? {}),
    // The pane the highlight belongs on: the selection in flight, else herdr's
    // own answer, which only means anything on the host this tab is showing.
    current:
      pending ??
      (tabHost && activePaneID ? `${tabHost}\u0000${activePaneID}` : null),
    focusAgent,
    closeAgent,
  }
}
