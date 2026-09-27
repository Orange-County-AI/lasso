import * as React from "react"
import { AgentLines } from "@/components/AgentParts"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "@/components/ui/dialog"
import { NO_AUTOCORRECT } from "@/components/ui/input"
import { Orb } from "@/components/ui/orb"
import { agentName, agentSub, paneKey, useAgents } from "@/lib/agents"
import type { HostPane } from "@/lib/api"
import { blurHerdrTerminal } from "@/lib/terminal"
import { cn } from "@/lib/utils"

// ⌘K from a reading view (the chat or the agents grid). In the terminal ⌘K is
// herdr's own pane search, which searches ONE machine's panes and answers by
// moving the terminal; from a conversation the question is "which conversation
// next", across the fleet, so this is the same list the agent sidebar shows
// with a filter on top. A pick focuses the agent (moving the tab when it lives
// elsewhere, lib/agents) and lands on its conversation.
export function AgentSwitcher({
  open,
  onOpenChange,
  onPicked,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  // Called before the focus is sent, so the view has switched to the chat by
  // the time the focus lands — the same order AgentsView's card open uses.
  onPicked: () => void
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        showCloseButton={false}
        // grid-cols-1 is minmax(0,1fr): the default auto track sizes to the
        // longest worktree name and pushes the list past the dialog's edge.
        className="grid-cols-1 gap-0 overflow-hidden p-0 sm:max-w-md"
        // The terminal is hidden under the view this was opened from, so
        // handing it the keyboard on close would pop a phone's keyboard and
        // aim the next keystrokes at herdr (see NewDialog's agentsOnly).
        onCloseAutoFocus={(e) => {
          e.preventDefault()
          blurHerdrTerminal()
        }}
      >
        <DialogTitle className="sr-only">Switch agent</DialogTitle>
        <DialogDescription className="sr-only">
          Type to filter the fleet's agents; Enter opens one in the chat.
        </DialogDescription>
        {/* Mounted only while open, so the filter starts empty every time. */}
        {open && (
          <SwitcherBody
            onPick={() => {
              onOpenChange(false)
              onPicked()
            }}
          />
        )}
      </DialogContent>
    </Dialog>
  )
}

// Every whitespace-separated term must appear somewhere in the row's name,
// harness/worktree or machine — so "norm claude" narrows to claude agents on
// norm without caring which field each word came from.
function matches(p: HostPane, terms: string[]): boolean {
  if (terms.length === 0) return true
  const hay = [agentName(p), agentSub(p), p.host_label, p.host]
    .filter(Boolean)
    .join(" ")
    .toLowerCase()
  return terms.every((t) => hay.includes(t))
}

function SwitcherBody({ onPick }: { onPick: () => void }) {
  const { agents, isLoading, error, unlisted, current, focusAgent } =
    useAgents()
  const [query, setQuery] = React.useState("")
  const [active, setActive] = React.useState(0)
  const listRef = React.useRef<HTMLDivElement>(null)
  // Scroll the highlight into view only for keyboard moves; doing it on hover
  // re-snaps the list under the wheel (same reasoning as ui/combobox).
  const navSource = React.useRef<"keyboard" | "pointer">("keyboard")

  const filtered = React.useMemo(() => {
    const terms = query.toLowerCase().split(/\s+/).filter(Boolean)
    return agents.filter((p) => matches(p, terms))
  }, [agents, query])

  // biome-ignore lint/correctness/useExhaustiveDependencies: reset on filter change
  React.useEffect(() => {
    setActive(0)
  }, [query])

  // A poll can shrink the list under the highlight.
  const clamped = Math.min(active, Math.max(filtered.length - 1, 0))

  React.useEffect(() => {
    if (navSource.current !== "keyboard") return
    listRef.current
      ?.querySelector<HTMLElement>(`[data-index="${clamped}"]`)
      ?.scrollIntoView({ block: "nearest" })
  }, [clamped])

  const choose = (p: HostPane) => {
    onPick()
    void focusAgent(p)
  }

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown" || (e.ctrlKey && e.key === "n")) {
      e.preventDefault()
      navSource.current = "keyboard"
      setActive(Math.min(clamped + 1, filtered.length - 1))
    } else if (e.key === "ArrowUp" || (e.ctrlKey && e.key === "p")) {
      e.preventDefault()
      navSource.current = "keyboard"
      setActive(Math.max(clamped - 1, 0))
    } else if (e.key === "Enter" && !e.nativeEvent.isComposing) {
      e.preventDefault()
      const p = filtered[clamped]
      if (p) choose(p)
    }
  }

  return (
    <div className="flex min-w-0 flex-col">
      <input
        // biome-ignore lint/a11y/noAutofocus: a switcher opened by a shortcut exists to be typed into
        autoFocus
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        onKeyDown={onKeyDown}
        placeholder="Find an agent…"
        aria-label="Filter agents"
        {...NO_AUTOCORRECT}
        className="w-full border-border border-b bg-transparent px-3 py-2.5 text-sm outline-none placeholder:text-muted-foreground"
      />
      <div
        ref={listRef}
        role="listbox"
        aria-label="Agents"
        className="max-h-[min(60vh,24rem)] overflow-y-auto p-1"
      >
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
        {!isLoading && !error && filtered.length === 0 && (
          <div className="px-2 py-3 text-[11.5px] text-muted-foreground">
            {agents.length === 0
              ? "No agents on any connected host."
              : "No agents match."}
          </div>
        )}
        {filtered.map((p, i) => (
          <button
            key={paneKey(p)}
            type="button"
            role="option"
            aria-selected={i === clamped}
            data-index={i}
            onClick={() => choose(p)}
            onMouseMove={() => {
              navSource.current = "pointer"
              if (i !== clamped) setActive(i)
            }}
            className={cn(
              "flex w-full flex-col gap-0.5 rounded-md px-2 py-1.5 text-left outline-none",
              // bg-accent is the popover's own color, so the highlight needs
              // the ring to be visible at all (see ui/combobox).
              i === clamped && "bg-accent ring-1 ring-primary/60"
            )}
          >
            <AgentLines pane={p} current={paneKey(p) === current} />
          </button>
        ))}
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
    </div>
  )
}
