import { Bot, X } from "lucide-react"
import * as React from "react"

import { AgentStatus } from "@/components/AgentStatus"
import { Orb } from "@/components/ui/orb"
import { agentName, agentSub, useAgents } from "@/lib/agents"
import type { Pane } from "@/lib/api"
import { cn } from "@/lib/utils"

// The agent list as a SHEET, for the widths with no room to dock one: below md
// the footer that carries the sidebar toggle is hidden and the docked column
// hides itself, so this is how a phone — and a tablet too narrow for the footer
// — picks which conversation the chat is showing.
//
// A phone gets a STACK and anything wider gets a grid, which is the whole
// difference screen size makes here: a tile holds a name, a worktree and a
// status, none of which grow with the viewport, so the second column is free
// real estate where there is room for it and a squeezed name where there is not.
export function AgentPicker({
  host,
  onClose,
}: {
  host: string | null
  onClose: () => void
}) {
  const { agents, isLoading, error, current, focusAgent } = useAgents(host)

  // Escape dismisses it: a sheet over a reading surface should close without
  // hunting for the button that opened it.
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose()
    }
    document.addEventListener("keydown", onKey)
    return () => document.removeEventListener("keydown", onKey)
  }, [onClose])

  return (
    // An overlay over the chat, not a swap for it: the conversation stays
    // mounted underneath (and the terminal under THAT), so nothing refits or
    // reloads on the way in or out.
    <div
      role="dialog"
      aria-label="Agents"
      className="vsurface absolute inset-0 z-30 flex flex-col bg-background"
    >
      <div className="flex flex-none items-center gap-2 border-border border-b px-2.5 py-1.5">
        <Bot className="size-3.5 text-muted-foreground" />
        <span className="text-[12.5px] text-foreground">Agents</span>
        {agents.length > 0 && (
          <span className="text-[11px] text-muted-foreground">
            {agents.length}
          </span>
        )}
        <button
          type="button"
          onClick={onClose}
          title="Close"
          aria-label="Close"
          className="ml-auto flex size-7 shrink-0 items-center justify-center rounded-lg text-muted-foreground hover:bg-accent hover:text-foreground"
        >
          <X className="size-4" />
        </button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto p-2">
        {isLoading && (
          <div className="flex items-center justify-center gap-2 py-3 text-[12px] text-muted-foreground">
            <Orb state="working" px={16} />
            loading…
          </div>
        )}
        {error && (
          <div className="px-1 py-3 text-[11.5px] text-destructive">
            could not list agents: {(error as Error).message}
          </div>
        )}
        {!isLoading && !error && agents.length === 0 && (
          <div className="px-1 py-3 text-[11.5px] text-muted-foreground">
            No agents on this host.
          </div>
        )}
        <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
          {agents.map((p) => (
            <Tile
              key={p.pane_id}
              pane={p}
              current={p.pane_id === current}
              // Focus then dismiss, in that order and without waiting: the
              // conversation on screen is the answer to the tap, and the highlight
              // follows herdr's own report a beat later (lib/agents).
              onSelect={(pane) => {
                void focusAgent(pane)
                onClose()
              }}
            />
          ))}
        </div>
      </div>
    </div>
  )
}

function Tile({
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
        "flex min-w-0 flex-col gap-1 rounded-lg border border-border px-2.5 py-2 text-left transition-colors hover:bg-accent",
        // A tile has a border to carry the "this is the one" step, where a row in
        // the docked column only has a background.
        current && "border-primary/50 bg-accent"
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
