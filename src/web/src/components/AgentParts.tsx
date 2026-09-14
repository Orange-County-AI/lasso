import { Orb } from "@/components/ui/orb"
import { agentName, agentSub } from "@/lib/agents"
import type { HostPane } from "@/lib/api"
import { cn } from "@/lib/utils"

// The pieces a row and a tile are both made of. Two surfaces list agents — the
// docked column and the phone's sheet — and they are the same list at two widths:
// a name, a status, a machine and a worktree that read differently between them
// would be two accounts of one agent.

// The one place an agent's status is drawn.
//
// Only the two states a human acts on get a word: an agent that is working is
// the one being waited on, and one that is blocked has stopped for an answer.
// idle and done are rest, and naming them on every row turns a list you scan
// into a list you read.
export function AgentStatus({
  status,
  className,
}: {
  status?: string
  className?: string
}) {
  if (!status) return null
  const speaking = status === "working" || status === "blocked"
  return (
    <span
      className={cn(
        "flex shrink-0 items-center gap-1 text-[11px]",
        status === "blocked"
          ? "text-destructive"
          : status === "working"
            ? "text-primary"
            : "text-muted-foreground",
        className
      )}
      title={status}
    >
      {status === "working" ? (
        <Orb state="working" px={12} />
      ) : (
        <span className="size-1.5 rounded-full bg-current opacity-70" />
      )}
      {speaking && status}
    </span>
  )
}

// AgentLines is what an agent looks like: what it is called with its state, then
// where it runs and what it is working on.
//
// The machine is a chip rather than a field of the truncated line, because with
// the list spanning the fleet it is what tells two same-named agents apart — a
// host you have to guess at because the name beside it ate the row is the one
// mistake this list cannot afford.
export function AgentLines({
  pane,
  current,
}: {
  pane: HostPane
  current: boolean
}) {
  const sub = agentSub(pane)
  return (
    <>
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
      <span className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
        <span
          className="shrink-0 rounded-sm bg-muted px-1 text-[10px] text-foreground/70"
          title={pane.host}
        >
          {pane.host_label || pane.host}
        </span>
        {sub && <span className="min-w-0 truncate">{sub}</span>}
      </span>
    </>
  )
}
