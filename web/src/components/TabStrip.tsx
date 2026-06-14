import { Plus, X } from "lucide-react"
import * as React from "react"
import { toast } from "sonner"

import { PromptDialog } from "@/components/PromptDialog"
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from "@/components/ui/context-menu"
import { api, type TreeWorkspace } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { qk, queryClient, treeAddTab } from "@/lib/query"
import { cn } from "@/lib/utils"

const STATUS_DOT: Record<string, string> = {
  working: "bg-[var(--h-warn)] animate-pulse",
  blocked: "bg-[var(--h-bad)]",
  idle: "bg-[var(--h-good)]",
  unknown: "bg-muted-foreground/40",
}

// nextTabNumber suggests the default numeric name for a new tab ("1", "2",
// "3"): the highest numeric tab title + 1, else the tab count + 1.
// Pre-fills the new-tab modal so pressing Enter gives a numbered tab; the server
// applies the same default if the field is cleared.
function nextTabNumber(tabs: { title?: string }[]): string {
  const nums = tabs
    .map((t) => Number.parseInt(t.title ?? "", 10))
    .filter((n) => Number.isInteger(n))
  return String(nums.length ? Math.max(...nums) + 1 : tabs.length + 1)
}

// The tab strip above the terminal area: the active workspace's tabs, plus a "+"
// to open a new shell tab in the same workspace. The "+"
// and right-click "Rename…" both go through PromptDialog (our own modal).
export function TabStrip({
  workspace,
  selectedTabId,
  onSelectTab,
}: {
  workspace: TreeWorkspace | null
  selectedTabId: string | null
  onSelectTab: (tabId: string) => void
}) {
  const { agentStatuses } = useApp()
  // A pending name prompt: creating a new tab, or renaming an existing one.
  const [prompt, setPrompt] = React.useState<{
    mode: "new" | "rename"
    tabId?: string
    initial: string
  } | null>(null)

  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: qk.tree })
    queryClient.invalidateQueries({ queryKey: qk.agents })
  }

  const createTab = async (title: string) => {
    if (!workspace) return
    try {
      const t = await api.newTab(workspace.id, title)
      // Show the tab in the strip immediately, then reconcile.
      treeAddTab(workspace.id, { id: t.id, title: t.title, kind: "shell" })
      onSelectTab(t.id)
      refresh()
    } catch (e) {
      toast.error(String(e))
    }
  }

  const closeTab = async (tabId: string) => {
    await api.closeTab(tabId).catch((e) => toast.error(String(e)))
    refresh()
  }

  // ⌘U creates a tab immediately (auto-named) and ⌘⇧U closes the active tab.
  // App dispatches these events globally so they fire with focus inside a
  // terminal. Kept in refs so the listeners can stay mounted across renders
  // while always seeing the current workspace/selection.
  const createTabRef = React.useRef(createTab)
  createTabRef.current = createTab
  const closeTabRef = React.useRef(closeTab)
  closeTabRef.current = closeTab
  React.useEffect(() => {
    const onNew = () => {
      if (!workspace) return
      createTabRef.current(nextTabNumber(workspace.tabs ?? []))
    }
    const onClose = () => {
      if (selectedTabId) closeTabRef.current(selectedTabId)
    }
    window.addEventListener("lasso:new-tab", onNew)
    window.addEventListener("lasso:close-tab", onClose)
    return () => {
      window.removeEventListener("lasso:new-tab", onNew)
      window.removeEventListener("lasso:close-tab", onClose)
    }
  }, [workspace, selectedTabId])

  const renameTab = async (tabId: string, title: string) => {
    await api.renameTab(tabId, title).catch((e) => toast.error(String(e)))
    refresh()
  }

  if (!workspace) {
    return (
      <div className="flex h-9 items-center border-border border-b px-3 text-[13px] text-muted-foreground">
        no workspace selected
      </div>
    )
  }
  return (
    <>
      <div className="flex h-9 items-center gap-0 overflow-x-auto border-border border-b">
        {(workspace.tabs ?? []).map((tab) => {
          const status = agentStatuses[tab.id] ?? tab.status ?? "unknown"
          return (
            <ContextMenu key={tab.id}>
              <ContextMenuTrigger asChild>
                <div
                  className={cn(
                    "group flex shrink-0 items-center gap-1.5 border-transparent border-b-2 px-3 py-1.5 text-[13px]",
                    tab.id === selectedTabId
                      ? "border-primary text-primary"
                      : "text-muted-foreground hover:text-foreground"
                  )}
                >
                  <button
                    type="button"
                    className="flex items-center gap-1.5"
                    onClick={() => onSelectTab(tab.id)}
                    title={tab.title || tab.kind}
                  >
                    {tab.kind === "agent" && (
                      <span
                        className={cn(
                          "size-2 shrink-0 rounded-full",
                          STATUS_DOT[status]
                        )}
                      />
                    )}
                    <span className="max-w-[16ch] truncate">
                      {tab.title || tab.kind}
                    </span>
                  </button>
                  <button
                    type="button"
                    title="close tab"
                    className="opacity-0 hover:text-[var(--h-bad)] group-hover:opacity-100"
                    onClick={() => closeTab(tab.id)}
                  >
                    <X className="size-3" />
                  </button>
                </div>
              </ContextMenuTrigger>
              <ContextMenuContent>
                <ContextMenuItem
                  onSelect={() =>
                    setPrompt({
                      mode: "rename",
                      tabId: tab.id,
                      initial: tab.title || tab.kind,
                    })
                  }
                >
                  Rename…
                </ContextMenuItem>
                <ContextMenuSeparator />
                <ContextMenuItem
                  variant="destructive"
                  onSelect={() => closeTab(tab.id)}
                >
                  Close tab
                </ContextMenuItem>
              </ContextMenuContent>
            </ContextMenu>
          )
        })}
        <button
          type="button"
          title="new tab"
          className="shrink-0 px-2 py-1.5 text-muted-foreground hover:text-primary"
          onClick={() => createTab(nextTabNumber(workspace.tabs ?? []))}
        >
          <Plus className="size-4" />
        </button>
      </div>

      <PromptDialog
        open={!!prompt}
        onOpenChange={(o) => {
          if (!o) setPrompt(null)
        }}
        title={prompt?.mode === "rename" ? "Rename tab" : "New tab"}
        placeholder="Tab name"
        defaultValue={prompt?.initial ?? ""}
        submitLabel={prompt?.mode === "rename" ? "Rename" : "Create"}
        onSubmit={(name) => {
          if (prompt?.mode === "rename" && prompt.tabId)
            renameTab(prompt.tabId, name)
          else createTab(name)
          setPrompt(null)
        }}
      />
    </>
  )
}
