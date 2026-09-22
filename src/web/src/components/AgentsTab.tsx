import { AgentLines } from "@/components/AgentParts"
import { Orb } from "@/components/ui/orb"
import { paneKey, useAgents } from "@/lib/agents"
import type { HostPane } from "@/lib/api"
import { cn } from "@/lib/utils"

// The agent list as a SIDEBAR TAB, for the widths with no room to dock one
// beside the chat: below md the footer that carries the sidebar toggle is hidden
// and the chat's own docked column hides itself, so this is how a phone — and a
// tablet too narrow for the footer — picks which conversation it is reading.
//
// It lives in the sidebar rather than inside the chat (where it used to be a
// sheet over the conversation) because the sidebar is a phone's whole chrome:
// the same panel carries Files, Settings and the creator, the tab strip already
// remembers which face you were on, and the list is reachable from the TERMINAL
// this way too, where a sheet inside the chat was not. The tab is md:hidden — at
// md+ the docked column (AgentSidebar) is the same list, and exactly one of the
// two is reachable at any width.
//
// A phone gets a STACK and anything wider gets a grid, which is the whole
// difference screen size makes here: a tile holds a name, a machine, a worktree
// and a status, none of which grow with the viewport, so the second column is
// free real estate where there is room for it and a squeezed name where there is
// not.
export function AgentsTab({ onPick }: { onPick: () => void }) {
  const { agents, isLoading, error, unlisted, current, focusAgent } =
    useAgents()

  return (
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
          No agents on any connected host.
        </div>
      )}
      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
        {agents.map((p) => (
          <Tile
            key={paneKey(p)}
            pane={p}
            current={paneKey(p) === current}
            // Focus then dismiss, in that order and without waiting: the
            // conversation on screen is the answer to the tap, and below md this
            // panel covers it whole, so staying open would hide what was picked.
            // The highlight follows herdr's own report a beat later (lib/agents).
            // Which FACE that lands on is left alone deliberately — from the
            // terminal a pick lands on that agent's terminal and from the chat on
            // its conversation, both of which follow herdr's focus by themselves.
            onSelect={(pane) => {
              void focusAgent(pane)
              onPick()
            }}
          />
        ))}
      </div>
      {/* A host that did not answer is the one reason an agent you expect is
          not here, so it is said rather than left to be inferred. */}
      {unlisted.length > 0 && (
        <div
          className="px-1 py-2 text-[11px] text-muted-foreground"
          title={unlisted.join(", ")}
        >
          {unlisted.length} host{unlisted.length === 1 ? "" : "s"} could not be
          listed
        </div>
      )}
    </div>
  )
}

function Tile({
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
        "flex min-w-0 flex-col gap-1 rounded-lg border border-border px-2.5 py-2 text-left transition-colors hover:bg-accent",
        // A tile has a border to carry the "this is the one" step, where a row in
        // the docked column only has a background.
        current && "border-primary/50 bg-accent"
      )}
    >
      <AgentLines pane={pane} current={current} />
    </button>
  )
}
