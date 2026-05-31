import * as React from "react"
import { toast } from "sonner"
import { Pill } from "@/components/Pill"
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
import { api, type Pane } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { tilde } from "@/lib/format"
import { cn } from "@/lib/utils"

// The Grid tab: a flat grid of every herdr pane (tagged by workspace). Click to
// focus, ⌘/ctrl/shift-click to multi-select, right-click to rename or close.
export function PaneGrid({
  active,
  onFocusPane,
}: {
  active: boolean
  onFocusPane: () => void
}) {
  const { activePaneID } = useApp()
  const [panes, setPanes] = React.useState<Pane[] | null>(null)
  const [error, setError] = React.useState<string | null>(null)
  const [selected, setSelected] = React.useState<Set<string>>(new Set())
  const [renameTarget, setRenameTarget] = React.useState<Pane | null>(null)
  const [renameValue, setRenameValue] = React.useState("")
  const [closeTargets, setCloseTargets] = React.useState<string[] | null>(null)

  const load = React.useCallback(async () => {
    try {
      const data = await api.panes()
      const list = data.panes || []
      setError(null)
      setPanes(list)
      // drop selections for panes that vanished
      const present = new Set(list.map((p) => p.pane_id))
      setSelected((prev) => {
        const n = new Set([...prev].filter((id) => present.has(id)))
        return n.size === prev.size ? prev : n
      })
    } catch (e) {
      setPanes([])
      setError((e as Error).message)
    }
  }, [])

  // Load when the tab opens, and reload when herdr's layout revision moves.
  React.useEffect(() => {
    if (active) load()
  }, [active, load])

  const byId = React.useMemo(() => {
    const m: Record<string, Pane> = {}
    for (const p of panes || []) m[p.pane_id] = p
    return m
  }, [panes])

  const toggleSelect = (id: string) =>
    setSelected((prev) => {
      const n = new Set(prev)
      if (n.has(id)) n.delete(id)
      else n.add(id)
      return n
    })

  const clearSelection = () => setSelected(new Set())

  const focusPane = async (p: Pane) => {
    try {
      await api.focus(p.workspace_id, p.tab_id)
    } catch (e) {
      toast.error(`focus failed: ${(e as Error).message}`)
    }
  }

  const onCardClick = (p: Pane, e: React.MouseEvent) => {
    if (e.ctrlKey || e.metaKey || e.shiftKey) {
      toggleSelect(p.pane_id)
    } else {
      clearSelection()
      focusPane(p)
      onFocusPane() // show the herdr terminal
    }
  }

  const submitRename = async () => {
    const p = renameTarget
    if (!p) return
    const label = renameValue.trim()
    const cur = p.tab_label || ""
    setRenameTarget(null)
    if (!label || label === cur) return
    try {
      await api.rename(p.tab_id, label)
      load()
    } catch (e) {
      toast.error(`rename failed: ${(e as Error).message}`)
    }
  }

  const doClose = async (paneIds: string[]) => {
    const ids = [...new Set(paneIds)].filter(Boolean)
    setCloseTargets(null)
    if (!ids.length) return
    try {
      const res = await api.close(ids)
      clearSelection()
      load()
      const nErr = res.errors ? Object.keys(res.errors).length : 0
      if (nErr)
        toast.error(
          `closed ${res.closed ? res.closed.length : 0}, ${nErr} failed`
        )
    } catch (e) {
      toast.error(`close failed: ${(e as Error).message}`)
    }
  }

  const closeLabel = closeNames(closeTargets, byId)

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      {selected.size > 0 && (
        <header className="border-border border-b bg-background px-3 py-2">
          <div className="flex flex-wrap items-center gap-2">
            <Pill tone="accent">{selected.size} selected</Pill>
            <Button
              variant="outline"
              size="sm"
              className="h-7 hover:border-bad hover:text-bad"
              onClick={() => setCloseTargets([...selected])}
            >
              Close selected
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="h-7"
              onClick={clearSelection}
            >
              Clear
            </Button>
          </div>
        </header>
      )}

      <div className="panegrid">
        {error ? (
          <div className="empty">
            cannot list panes
            <br />
            {error}
          </div>
        ) : !panes ? (
          <div className="empty">loading panes…</div>
        ) : panes.length === 0 ? (
          <div className="empty">no panes</div>
        ) : (
          panes.map((p) => (
            <PaneCard
              key={p.pane_id}
              pane={p}
              focused={activePaneID ? p.pane_id === activePaneID : !!p.focused}
              selected={selected.has(p.pane_id)}
              multiSelected={selected.size > 1 && selected.has(p.pane_id)}
              selectedCount={selected.size}
              onClick={(e) => onCardClick(p, e)}
              onRename={() => {
                setRenameTarget(p)
                setRenameValue(p.tab_label || "")
              }}
              onCloseOne={() => setCloseTargets([p.pane_id])}
              onCloseSelected={() => setCloseTargets([...selected])}
            />
          ))
        )}
      </div>

      <div className="hint">
        click to focus · ⌘/ctrl/shift-click to multi-select · right-click for
        rename / close
      </div>

      {/* rename — replaces window.prompt */}
      <Dialog
        open={renameTarget != null}
        onOpenChange={(o) => !o && setRenameTarget(null)}
      >
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>Rename tab</DialogTitle>
          </DialogHeader>
          <Input
            autoFocus
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

      {/* close confirmation — replaces window.confirm */}
      <AlertDialog
        open={closeTargets != null}
        onOpenChange={(o) => !o && setCloseTargets(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Close {closeTargets?.length || 0} pane
              {closeTargets?.length === 1 ? "" : "s"}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              {closeLabel}
              <br />
              <br />
              This terminates the terminal
              {closeTargets?.length === 1 ? "" : "s"} (and any agent running in{" "}
              {closeTargets?.length === 1 ? "it" : "them"}).
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => closeTargets && doClose(closeTargets)}
            >
              Close
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

function closeNames(ids: string[] | null, byId: Record<string, Pane>): string {
  if (!ids) return ""
  const names = ids.map((id) => byId[id]?.tab_label || id)
  return names.slice(0, 5).join(", ") + (names.length > 5 ? ", …" : "")
}

function PaneCard({
  pane: p,
  focused,
  selected,
  multiSelected,
  selectedCount,
  onClick,
  onRename,
  onCloseOne,
  onCloseSelected,
}: {
  pane: Pane
  focused: boolean
  selected: boolean
  multiSelected: boolean
  selectedCount: number
  onClick: (e: React.MouseEvent) => void
  onRename: () => void
  onCloseOne: () => void
  onCloseSelected: () => void
}) {
  const title = p.tab_label || p.pane_id
  const cwd = tilde(p.cwd)
  const ws = p.workspace_label || p.workspace_id || ""
  const showAgent = p.agent || (p.agent_status && p.agent_status !== "unknown")
  const tip = [p.tab_label, p.cwd, p.pane_id].filter(Boolean).join("\n")

  return (
    <ContextMenu>
      <ContextMenuTrigger asChild>
        <div
          className={cn("pcard", focused && "focused", selected && "selected")}
          title={tip}
          onClick={onClick}
        >
          {ws && <div className="pws">{ws}</div>}
          <div className="ptitle">{title}</div>
          {cwd && <div className="pmeta">{cwd}</div>}
          {showAgent && (
            <div className={cn("pagent", p.agent_status)}>
              ● {p.agent || "agent"}
              {p.agent_status ? ` · ${p.agent_status}` : ""}
            </div>
          )}
        </div>
      </ContextMenuTrigger>
      <ContextMenuContent>
        {multiSelected ? (
          <ContextMenuItem variant="destructive" onSelect={onCloseSelected}>
            Close {selectedCount} panes
          </ContextMenuItem>
        ) : (
          <>
            <ContextMenuItem onSelect={onRename}>Rename…</ContextMenuItem>
            <ContextMenuItem variant="destructive" onSelect={onCloseOne}>
              Close pane
            </ContextMenuItem>
          </>
        )}
      </ContextMenuContent>
    </ContextMenu>
  )
}
