import { SquareX } from "lucide-react"
import * as React from "react"
import { AgentLines } from "@/components/AgentParts"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Orb } from "@/components/ui/orb"
import { agentName, paneKey, useAgents } from "@/lib/agents"
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
  const {
    agents,
    isLoading,
    error,
    unlisted,
    current,
    focusAgent,
    closeAgent,
  } = useAgents()
  // The row whose pane the ✕ would end. Closing is herdr's own pane.close — what
  // ends the agent in that pane, there being no softer "detach" — so it is asked
  // before it happens, and the question names the agent because the button that
  // raised it belongs to one row rather than to the panel.
  const [closing, setClosing] = React.useState<HostPane | null>(null)

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
            onSelect={(pane) => {
              void focusAgent(pane)
              onPick()
            }}
            onClose={setClosing}
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
      <AlertDialog
        open={closing !== null}
        onOpenChange={(open) => {
          if (!open) setClosing(null)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Close this pane?</AlertDialogTitle>
            <AlertDialogDescription>
              Herdr closes {closing ? agentName(closing) : "this pane"} and the
              agent running in it stops. Its transcript stays on disk.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (closing) void closeAgent(closing)
              }}
            >
              Close pane
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

// One agent, and the two things you can do to it. The close is a SIBLING of the
// selecting button rather than something inside it — a button inside a button is
// invalid, and it would also mean a tap that means "end this" travelling through
// a handler that means "read this". The hover fill therefore sits on the wrapper,
// so the tile lights as one thing under either half.
function Tile({
  pane,
  current,
  onSelect,
  onClose,
}: {
  pane: HostPane
  current: boolean
  onSelect: (p: HostPane) => void
  onClose: (p: HostPane) => void
}) {
  return (
    <div
      className={cn(
        "flex min-w-0 rounded-lg border border-border transition-colors hover:bg-accent",
        // A tile has a border to carry the "this is the one" step, where a row in
        // the docked column only has a background.
        current && "border-primary/50 bg-accent"
      )}
    >
      <button
        type="button"
        onClick={() => onSelect(pane)}
        aria-current={current ? "true" : undefined}
        className="flex min-w-0 flex-1 flex-col gap-1 px-2.5 py-2 text-left"
      >
        <AgentLines pane={pane} current={current} />
      </button>
      {/* Its own column rather than a corner glyph: on a phone the thing beside
          it is the whole rest of the tile, and a destructive control that shares
          an edge with "open this" is one mis-tap from ending a session. */}
      <button
        type="button"
        onClick={() => onClose(pane)}
        title={`Close ${agentName(pane)}'s pane`}
        aria-label={`Close ${agentName(pane)}'s pane`}
        className="flex flex-none items-center px-2 text-muted-foreground transition-colors hover:text-destructive"
      >
        <SquareX className="size-4" />
      </button>
    </div>
  )
}
