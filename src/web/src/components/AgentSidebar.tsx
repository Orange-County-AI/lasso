import { Orb } from "@/components/ui/orb"
import { AgentStatus } from "@/components/AgentStatus"
import { agentName, agentSub, useAgents } from "@/lib/agents"
import type { Pane } from "@/lib/api"
import { cn } from "@/lib/utils"

// The chat's docked left sidebar: the agents running on THIS tab's host, in
// herdr's own order.
//
// Only agents — a pane with no agent is a bare shell or a stray tab, and there
// is no session to read, so listing it would offer a row that selects nothing.
// herdr's own sidebar indexes workspaces; this one indexes conversations, which
// is one per agent pane, and the chat beside it can show exactly one of those.
//
// The list, the naming and the focus action come from lib/agents, because the
// phone's sheet (AgentPicker) is the same list for the widths where a docked
// column does not fit — see md:hidden here and md:hidden on the button that
// opens the sheet. Exactly one of the two is reachable at any width.
export function AgentSidebar({ host }: { host: string | null }) {
  const { agents, isLoading, error, current, focusAgent } = useAgents(host)

  return (
    // vsurface: this sits over the terminal (the chat is an overlay), and under
    // the atmosphere --card is translucent — herdr's pane text would read
    // straight through the list. Same opt-out the chat and the file viewer take.
    <aside className="vsurface flex w-52 flex-none flex-col border-border border-r bg-card max-md:hidden">
      <div className="flex flex-none items-center gap-2 border-border border-b px-2.5 py-1.5">
        <span className="text-[12px] text-muted-foreground">Agents</span>
        {agents.length > 0 && (
          <span className="ml-auto text-[11px] text-muted-foreground">
            {agents.length}
          </span>
        )}
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto p-1">
        {isLoading && (
          <div className="flex items-center justify-center gap-2 py-3 text-[12px] text-muted-foreground">
            <Orb state="working" px={16} />
            loading…
          </div>
        )}
        {error && (
          <div className="px-2 py-3 text-[11.5px] text-destructive">
            could not list agents: {(error as Error).message}
          </div>
        )}
        {!isLoading && !error && agents.length === 0 && (
          <div className="px-2 py-3 text-[11.5px] text-muted-foreground">
            No agents on this host.
          </div>
        )}
        {agents.map((p) => (
          <Row
            key={p.pane_id}
            pane={p}
            current={p.pane_id === current}
            onSelect={focusAgent}
          />
        ))}
      </div>
    </aside>
  )
}

function Row({
  pane,
  current,
  onSelect,
}: {
  pane: Pane
  current: boolean
  onSelect: (p: Pane) => void
}) {
  const sub = agentSub(pane)
  return (
    <button
      type="button"
      onClick={() => onSelect(pane)}
      aria-current={current ? "true" : undefined}
      className={cn(
        "flex w-full flex-col gap-0.5 rounded-md px-2 py-1.5 text-left transition-colors hover:bg-accent",
        current && "bg-accent"
      )}
    >
      <span className="flex items-center gap-2">
        <span
          className={cn(
            "min-w-0 flex-1 truncate text-[13px]",
            current ? "text-foreground" : "text-muted-foreground"
          )}
        >
          {agentName(pane)}
        </span>
        <AgentStatus status={pane.agent_status} />
      </span>
      {sub && (
        <span className="truncate text-[11px] text-muted-foreground">
          {sub}
        </span>
      )}
    </button>
  )
}
