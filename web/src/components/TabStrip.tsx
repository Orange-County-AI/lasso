import { Plus, X } from "lucide-react"
import { toast } from "sonner"

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

// The tab strip above the terminal area: the active workspace's tabs (herdr-style
// grouping), plus a "+" to open a new shell tab in the same workspace.
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
  if (!workspace) {
    return (
      <div className="flex h-9 items-center border-border border-b px-3 text-[13px] text-muted-foreground">
        no workspace selected
      </div>
    )
  }
  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: qk.tree })
    queryClient.invalidateQueries({ queryKey: qk.agents })
  }
  return (
    <div className="flex h-9 items-center gap-0 overflow-x-auto border-border border-b">
      {(workspace.tabs ?? []).map((tab) => {
        const status = agentStatuses[tab.id] ?? tab.status ?? "unknown"
        return (
          <div
            key={tab.id}
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
              onClick={async () => {
                await api.closeTab(tab.id).catch((e) => toast.error(String(e)))
                refresh()
              }}
            >
              <X className="size-3" />
            </button>
          </div>
        )
      })}
      <button
        type="button"
        title="new tab"
        className="shrink-0 px-2 py-1.5 text-muted-foreground hover:text-primary"
        onClick={async () => {
          try {
            const t = await api.newTab(workspace.id)
            // Show the tab in the strip immediately, then reconcile.
            treeAddTab(workspace.id, {
              id: t.id,
              title: t.title,
              kind: "shell",
            })
            onSelectTab(t.id)
            refresh()
          } catch (e) {
            toast.error(String(e))
          }
        }}
      >
        <Plus className="size-4" />
      </button>
    </div>
  )
}
