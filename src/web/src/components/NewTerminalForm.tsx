import { useQuery, useQueryClient } from "@tanstack/react-query"
import * as React from "react"
import { toast } from "sonner"

import { Button } from "@/components/ui/button"
import { DialogFooter } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { api } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { qk } from "@/lib/query"

const NEW_WORKSPACE = "__new_workspace__"
const MAX_COMMAND_LENGTH = 512

const labelClass = "font-medium text-muted-foreground text-xs"

function Field({
  label,
  htmlFor,
  children,
  hint,
}: {
  label: string
  htmlFor: string
  children: React.ReactNode
  hint?: string
}) {
  return (
    <div className="flex flex-col gap-1">
      <label className={labelClass} htmlFor={htmlFor}>
        {label}
      </label>
      {children}
      {hint && <p className="text-[11px] text-muted-foreground">{hint}</p>}
    </div>
  )
}

export function NewTerminalForm({
  open,
  active,
  onCreated,
  onCancel,
}: {
  open: boolean
  active: boolean
  onCreated: () => void
  onCancel: () => void
}) {
  const { host } = useApp()
  const selectedHost = host || "local"
  const queryClient = useQueryClient()
  const [command, setCommand] = React.useState("")
  const [workspace, setWorkspace] = React.useState("")
  const [workspaceName, setWorkspaceName] = React.useState("~")
  const [tabName, setTabName] = React.useState("1")
  const [creating, setCreating] = React.useState(false)
  const commandRef = React.useRef<HTMLInputElement>(null)
  const selectionTouchedRef = React.useRef(false)
  const tabNameTouchedRef = React.useRef(false)

  const configQuery = useQuery({
    queryKey: qk.agentConfig(selectedHost),
    queryFn: () => api.agentConfig(selectedHost),
    enabled: open,
  })
  const workspacesQuery = useQuery({
    queryKey: qk.workspaces(selectedHost),
    queryFn: () => api.workspaces(selectedHost),
    enabled: open,
    staleTime: 0,
  })
  const workspaces = workspacesQuery.data?.workspaces ?? []

  React.useEffect(() => {
    if (!open) return
    selectionTouchedRef.current = false
    tabNameTouchedRef.current = false
    setCommand("")
    setWorkspace("")
    setWorkspaceName("~")
    setTabName("1")
    setCreating(false)
  }, [open])

  React.useEffect(() => {
    if (!open || !configQuery.data || !workspacesQuery.data) return
    const preferred = selectionTouchedRef.current
      ? workspaceName.trim() || "~"
      : configQuery.data.default_terminal_workspace?.trim() || "~"
    const currentExists = workspacesQuery.data.workspaces.some(
      (item) => item.workspace_id === workspace
    )
    if (
      currentExists ||
      (workspace === NEW_WORKSPACE && selectionTouchedRef.current)
    ) {
      return
    }
    const match = workspacesQuery.data.workspaces.find(
      (item) => item.label === preferred
    )
    setWorkspaceName(preferred)
    setWorkspace(match?.workspace_id ?? NEW_WORKSPACE)
  }, [open, workspace, workspaceName, configQuery.data, workspacesQuery.data])
  React.useEffect(() => {
    if (!open || !workspace || tabNameTouchedRef.current) return
    const selected = workspaces.find((item) => item.workspace_id === workspace)
    setTabName(
      workspace === NEW_WORKSPACE ? "1" : String((selected?.tab_count ?? 0) + 1)
    )
  }, [open, workspace, workspaces])

  React.useEffect(() => {
    if (!open || !active) return
    requestAnimationFrame(() => commandRef.current?.focus())
  }, [open, active])

  const create = async () => {
    if (creating || !workspace) return
    setCreating(true)
    try {
      const result = await api.createTerminal({
        host: selectedHost,
        command,
        workspace_id: workspace === NEW_WORKSPACE ? undefined : workspace,
        workspace_name: workspaceName.trim() || "~",
        tab_name: tabName.trim() || undefined,
        focus: true,
      })
      if (result.command_error) {
        toast.warning("Terminal created, but the command was not submitted", {
          description: result.command_error,
        })
      }
      if (result.tab_name_error) {
        toast.warning("Terminal created, but its tab could not be named", {
          description: result.tab_name_error,
        })
      }
      queryClient.invalidateQueries({
        queryKey: qk.workspaces(selectedHost),
      })
      onCreated()
    } catch (error) {
      toast.error(`Failed to create terminal: ${(error as Error).message}`)
      setCreating(false)
    }
  }

  const readError =
    configQuery.isError || workspacesQuery.isError
      ? ((configQuery.error ?? workspacesQuery.error) as Error).message
      : null

  return (
    <form
      className="flex min-h-0 flex-1 flex-col gap-4"
      onSubmit={(event) => {
        event.preventDefault()
        void create()
      }}
      onKeyDown={(event) => {
        if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
          event.preventDefault()
          void create()
        }
      }}
    >
      <div className="flex flex-col gap-3">
        <Field
          label="Command (optional)"
          htmlFor="terminal-command"
          hint="Leave blank to open an interactive shell."
        >
          <Input
            ref={commandRef}
            id="terminal-command"
            value={command}
            maxLength={MAX_COMMAND_LENGTH}
            disabled={creating}
            className="font-mono"
            onChange={(event) => setCommand(event.target.value)}
            placeholder="git status"
          />
        </Field>

        <Field label="Workspace" htmlFor="terminal-workspace">
          <select
            id="terminal-workspace"
            value={workspace}
            disabled={creating || workspacesQuery.isLoading}
            className="h-9 w-full rounded-lg border border-input bg-background px-2.5 text-sm shadow-well outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:opacity-50"
            onChange={(event) => {
              const next = event.target.value
              selectionTouchedRef.current = true
              tabNameTouchedRef.current = false
              setWorkspace(next)
              if (next !== NEW_WORKSPACE) {
                setWorkspaceName(
                  workspaces.find((item) => item.workspace_id === next)
                    ?.label || "~"
                )
              }
            }}
          >
            {!workspace && <option value="">Loading workspaces…</option>}
            {workspaces.map((item) => (
              <option key={item.workspace_id} value={item.workspace_id}>
                {item.label} · {item.tab_count}{" "}
                {item.tab_count === 1 ? "terminal" : "terminals"}
              </option>
            ))}
            <option value={NEW_WORKSPACE}>+ New workspace…</option>
          </select>
        </Field>

        {workspace === NEW_WORKSPACE && (
          <Field
            label="Workspace name (optional)"
            htmlFor="terminal-workspace-name"
            hint='Leave blank to use "~".'
          >
            <Input
              id="terminal-workspace-name"
              value={workspaceName}
              disabled={creating}
              onChange={(event) => setWorkspaceName(event.target.value)}
              placeholder="~"
            />
          </Field>
        )}
        <Field label="Tab name" htmlFor="terminal-tab-name">
          <Input
            id="terminal-tab-name"
            value={tabName}
            disabled={creating}
            onChange={(event) => {
              tabNameTouchedRef.current = true
              setTabName(event.target.value)
            }}
          />
        </Field>

        {readError && (
          <p className="text-[11px] text-destructive">
            Couldn&apos;t load workspaces: {readError}
          </p>
        )}
      </div>

      <DialogFooter className="mt-auto border-t-0 bg-transparent pt-0">
        <Button type="button" variant="outline" onClick={onCancel}>
          Cancel
        </Button>
        <Button type="submit" disabled={creating || !workspace}>
          {creating ? "Creating…" : "Create terminal"}
        </Button>
      </DialogFooter>
    </form>
  )
}
