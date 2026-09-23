import { AgentLines } from "@/components/AgentParts"
import { Orb } from "@/components/ui/orb"
import { paneKey, useAgents } from "@/lib/agents"
import type { HostPane } from "@/lib/api"
import { cn } from "@/lib/utils"

// The chat's docked left sidebar: every agent lasso can reach, across every
// connected machine, this tab's own host first.
//
// Only agents — a pane with no agent is a bare shell or a stray tab, and there
// is no session to read, so listing it would offer a row that selects nothing.
// herdr's own sidebar indexes one machine's workspaces; this one indexes the
// fleet's conversations, which is one per agent pane, and the chat beside it can
// show exactly one of those.
//
// The list, the naming and the focus action come from lib/agents, because the
// sidebar's Agents tab (AgentsTab) is the same list for the widths where a
// docked column does not fit — see max-md:hidden here and md:hidden on that tab.
// Exactly one of the two is reachable at any width.
export function AgentSidebar() {
  const { agents, isLoading, error, unlisted, current, focusAgent } =
    useAgents()

  return (
    // vsurface: this sits over the terminal (the chat is an overlay), and under
    // the atmosphere --card is translucent — herdr's pane text would read
    // straight through the list. Same opt-out the chat and the file viewer take;
    // inside the chat that opt-out paints the backdrop rather than going flat,
    // so this column carries the same shading as the conversation beside it.
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
            No agents on any connected host.
          </div>
        )}
        {agents.map((p) => (
          <Row
            key={paneKey(p)}
            pane={p}
            current={paneKey(p) === current}
            onSelect={focusAgent}
          />
        ))}
        {/* A host that did not answer is the one reason an agent you expect is
            not here, so it is said rather than left to be inferred. */}
        {unlisted.length > 0 && (
          <div
            className="px-2 py-2 text-[11px] text-muted-foreground"
            title={unlisted.join(", ")}
          >
            {unlisted.length} host{unlisted.length === 1 ? "" : "s"} could not
            be listed
          </div>
        )}
      </div>
    </aside>
  )
}

function Row({
  pane,
  current,
  onSelect,
}: {
  pane: HostPane
  current: boolean
  onSelect: (p: HostPane) => void
}) {
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
      <AgentLines pane={pane} current={current} />
    </button>
  )
}
