import { useQuery, useQueryClient } from "@tanstack/react-query"
import * as React from "react"
import { toast } from "sonner"

import { Button } from "@/components/ui/button"
import { DialogFooter } from "@/components/ui/dialog"
import { Input, NO_AUTOCORRECT } from "@/components/ui/input"
import { Orb } from "@/components/ui/orb"
import { SCRATCH_WORKSPACE } from "@/lib/agents"
import { api } from "@/lib/api"
import { moveTabToHost } from "@/lib/app-store"
import { qk } from "@/lib/query"

const NEW_WORKSPACE = "__new_workspace__"
const MAX_COMMAND_LENGTH = 512

// The shadcn <Input>'s own field classes, worn by the native <textarea> that
// holds the command, so it reads as one set with the Inputs below it. An
// <input> cannot hold a newline, and the point of this field is a multi-line
// block: h-8 becomes a box that starts at a few lines and can be dragged
// taller.
const commandClass =
  "min-h-[4.5rem] w-full min-w-0 resize-y rounded-lg border border-input bg-transparent px-2.5 py-1 font-mono text-base shadow-well outline-none transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:bg-input/50 disabled:opacity-50 md:text-sm dark:bg-input/30 dark:disabled:bg-input/80"

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
  selectedHost,
  creating,
  setCreating,
  onCreated,
  footerLead,
}: {
  open: boolean
  active: boolean
  selectedHost: string
  creating: boolean
  setCreating: (creating: boolean) => void
  onCreated: () => void
  // The shared host picker (wrapped in its footer group), owned by NewDialog
  // and rendered in this footer.
  footerLead: React.ReactNode
}) {
  const queryClient = useQueryClient()
  const [command, setCommand] = React.useState("")
  const [workspace, setWorkspace] = React.useState("")
  const [workspaceName, setWorkspaceName] = React.useState(SCRATCH_WORKSPACE)
  const [tabName, setTabName] = React.useState("")
  const commandRef = React.useRef<HTMLTextAreaElement>(null)
  const selectionTouchedRef = React.useRef(false)

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
    setWorkspace("")
    setWorkspaceName(SCRATCH_WORKSPACE)
    setTabName("")
  }, [open])

  React.useEffect(() => {
    if (!open) return
    setCommand("")
    setCreating(false)
  }, [open, setCreating])

  React.useEffect(() => {
    if (!open || !configQuery.data || !workspacesQuery.data) return
    const preferred = selectionTouchedRef.current
      ? workspaceName.trim() || SCRATCH_WORKSPACE
      : configQuery.data.default_terminal_workspace?.trim() || SCRATCH_WORKSPACE
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
  // A blank tab name is named by the server after the command (the command
  // itself when trivial, an AI-written name otherwise); a bare shell gets the
  // next number, as it always did.
  const nextTabNumber =
    workspace === NEW_WORKSPACE
      ? "1"
      : String(
          (workspaces.find((item) => item.workspace_id === workspace)
            ?.tab_count ?? 0) + 1
        )
  const autoTabName = !command.trim()

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
        workspace_name: workspaceName.trim() || SCRATCH_WORKSPACE,
        tab_name: tabName.trim() || (autoTabName ? nextTabNumber : undefined),
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
      try {
        await moveTabToHost(selectedHost)
        // The new terminal's own pane, so a split lands on it rather than on
        // its tab's previously active sibling. workspace_id/tab_id ride along
        // as the fallback for a pane herdr has not surfaced yet.
        await api.focus({
          pane_id: result.root_pane,
          workspace_id: result.workspace_id,
          tab_id: result.tab_id,
        })
      } catch (error) {
        toast.warning("Terminal created, but navigation failed", {
          description: (error as Error).message,
        })
      }
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
      className="flex min-h-0 flex-1 flex-col gap-4 overflow-hidden"
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
      <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto pr-1">
        <Field
          label="Command (optional)"
          htmlFor="terminal-command"
          hint="Leave blank to open an interactive shell. A multi-line command runs as one script in the new shell."
        >
          <textarea
            ref={commandRef}
            id="terminal-command"
            {...NO_AUTOCORRECT}
            className={commandClass}
            rows={3}
            value={command}
            maxLength={MAX_COMMAND_LENGTH}
            disabled={creating}
            onChange={(event) => setCommand(event.target.value)}
            placeholder={"git status\nbun test"}
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
              setWorkspace(next)
              if (next !== NEW_WORKSPACE) {
                setWorkspaceName(
                  workspaces.find((item) => item.workspace_id === next)
                    ?.label || SCRATCH_WORKSPACE
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
            hint={`Leave blank to use "${SCRATCH_WORKSPACE}".`}
          >
            <Input
              id="terminal-workspace-name"
              value={workspaceName}
              disabled={creating}
              onChange={(event) => setWorkspaceName(event.target.value)}
              placeholder={SCRATCH_WORKSPACE}
            />
          </Field>
        )}
        <Field
          label="Tab name (optional)"
          htmlFor="terminal-tab-name"
          hint="Leave blank to name it after the command: a short one as typed, a longer one by AI."
        >
          <Input
            id="terminal-tab-name"
            value={tabName}
            disabled={creating}
            onChange={(event) => setTabName(event.target.value)}
            placeholder={autoTabName ? nextTabNumber : "Named from the command"}
          />
        </Field>

        {readError && (
          <p className="text-[11px] text-destructive">
            Couldn&apos;t load workspaces: {readError}
          </p>
        )}
      </div>

      <DialogFooter className="mt-auto gap-3 border-t-0 bg-transparent pt-0">
        {footerLead}
        <Button type="submit" disabled={creating || !workspace}>
          {creating ? (
            <>
              <Orb state="working" px={16} on="accent" />
              Creating…
            </>
          ) : (
            "Create terminal"
          )}
        </Button>
      </DialogFooter>
    </form>
  )
}
