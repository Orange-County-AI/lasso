import * as React from "react"
import { toast } from "sonner"
import { CreateAgentDialog } from "@/components/CreateAgentDialog"
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
import { Button } from "@/components/ui/button"
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuTrigger,
} from "@/components/ui/context-menu"
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { type Agent, api } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { tilde } from "@/lib/format"
import { cn } from "@/lib/utils"

// How often the grid re-lists agents while the tab is open. herdr doesn't always
// push a layout event the moment an agent appears, so we poll rather than rely
// solely on the revision counter.
const POLL_MS = 1500

// The Agents tab: a grid of every herdr-detected agent (claude, codex, …),
// tagged by workspace. Click to focus it in herdr, right-click to rename its
// pane or close it. Polls every POLL_MS while open so new agents and status
// changes show up without a page reload.
export function AgentsTab({
  active,
  onFocusAgent,
}: {
  active: boolean
  onFocusAgent: () => void
}) {
  const { activePaneID } = useApp()
  const [agents, setAgents] = React.useState<Agent[] | null>(null)
  const [error, setError] = React.useState<string | null>(null)
  const [renameTarget, setRenameTarget] = React.useState<Agent | null>(null)
  const [renameValue, setRenameValue] = React.useState("")
  const [closeTarget, setCloseTarget] = React.useState<Agent | null>(null)

  const load = React.useCallback(async () => {
    try {
      const data = await api.agents()
      setError(null)
      setAgents(data.agents || [])
    } catch (e) {
      setAgents([])
      setError((e as Error).message)
    }
  }, [])

  // Load on open, then poll every POLL_MS while the tab stays open. Stops when
  // the tab is hidden so we don't keep hitting herdr in the background.
  React.useEffect(() => {
    if (!active) return
    load()
    const id = setInterval(load, POLL_MS)
    return () => clearInterval(id)
  }, [active, load])

  const focusAgent = async (a: Agent) => {
    try {
      await api.agentFocus(a.target)
      onFocusAgent() // surface the herdr terminal
    } catch (e) {
      toast.error(`focus failed: ${(e as Error).message}`)
    }
  }

  const submitRename = async () => {
    const a = renameTarget
    if (!a) return
    const label = renameValue.trim()
    const cur = a.workspace_label || ""
    setRenameTarget(null)
    if (!label || label === cur) return
    try {
      await api.workspaceRename(a.workspace_id, label)
      load()
    } catch (e) {
      toast.error(`rename failed: ${(e as Error).message}`)
    }
  }

  const doClose = async (a: Agent) => {
    setCloseTarget(null)
    try {
      const res = await api.close([a.pane_id])
      load()
      const nErr = res.errors ? Object.keys(res.errors).length : 0
      if (nErr) toast.error(`close failed`)
    } catch (e) {
      toast.error(`close failed: ${(e as Error).message}`)
    }
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex shrink-0 items-center justify-between border-border border-b px-2 py-1.5">
        <span className="text-muted-foreground text-xs">
          {agents
            ? `${agents.length} agent${agents.length === 1 ? "" : "s"}`
            : ""}
        </span>
        <CreateAgentDialog
          onCreated={() => {
            load()
            onFocusAgent() // surface the herdr terminal for the new agent
          }}
        />
      </div>
      <div className="panegrid">
        {error ? (
          <div className="empty">
            cannot list agents
            <br />
            {error}
          </div>
        ) : !agents ? (
          <div className="empty">loading agents…</div>
        ) : agents.length === 0 ? (
          <div className="empty">no agents running</div>
        ) : (
          agents.map((a) => (
            <AgentCard
              key={a.pane_id}
              agent={a}
              focused={activePaneID ? a.pane_id === activePaneID : !!a.focused}
              onClick={() => focusAgent(a)}
              onRename={() => {
                setRenameTarget(a)
                setRenameValue(a.workspace_label || "")
              }}
              onClose={() => setCloseTarget(a)}
            />
          ))
        )}
      </div>

      <div className="hint">
        click to focus · right-click for rename / close
      </div>

      {/* rename the workspace (relabels every agent grouped under it) */}
      <Dialog
        open={renameTarget != null}
        onOpenChange={(o) => !o && setRenameTarget(null)}
      >
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>Rename workspace</DialogTitle>
          </DialogHeader>
          <Input
            autoFocus
            placeholder={renameTarget?.workspace_id || "workspace"}
            value={renameValue}
            onChange={(e) => setRenameValue(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") submitRename()
            }}
          />
          <DialogFooter>
            <Button variant="outline" onClick={() => setRenameTarget(null)}>
              Cancel
            </Button>
            <Button onClick={submitRename}>Rename</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* close confirmation — terminates the pane and its agent */}
      <AlertDialog
        open={closeTarget != null}
        onOpenChange={(o) => !o && setCloseTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Close agent pane?</AlertDialogTitle>
            <AlertDialogDescription>
              {closeTarget
                ? [closeTarget.workspace_label, closeTarget.agent]
                    .filter(Boolean)
                    .join(" · ") || closeTarget.pane_id
                : ""}
              <br />
              <br />
              This terminates the terminal and the agent running in it.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => closeTarget && doClose(closeTarget)}
            >
              Close
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

function AgentCard({
  agent: a,
  focused,
  onClick,
  onRename,
  onClose,
}: {
  agent: Agent
  focused: boolean
  onClick: () => void
  onRename: () => void
  onClose: () => void
}) {
  const title = a.workspace_label || a.workspace_id || a.pane_id
  const cwd = tilde(a.cwd)
  const tip = [a.workspace_label, a.agent, a.cwd, a.pane_id]
    .filter(Boolean)
    .join("\n")

  return (
    <ContextMenu>
      <ContextMenuTrigger asChild>
        <div
          className={cn("pcard", focused && "focused")}
          title={tip}
          onClick={onClick}
        >
          <div className="ptitle">{title}</div>
          {cwd && <div className="pmeta">{cwd}</div>}
          <div className={cn("pagent", a.agent_status)}>
            ● {a.agent || "agent"}
            {a.agent_status ? ` · ${a.agent_status}` : ""}
          </div>
        </div>
      </ContextMenuTrigger>
      <ContextMenuContent>
        <ContextMenuItem onSelect={onRename}>Rename workspace…</ContextMenuItem>
        <ContextMenuItem variant="destructive" onSelect={onClose}>
          Close pane
        </ContextMenuItem>
      </ContextMenuContent>
    </ContextMenu>
  )
}
