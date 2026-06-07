import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Keyboard, RotateCw } from "lucide-react"
import * as React from "react"
import { Pill } from "@/components/Pill"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { api } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { qk } from "@/lib/query"
import { SHORTCUTS } from "@/lib/shortcuts"
import { cn } from "@/lib/utils"

// Native textarea/select styled to match the shadcn <Input>.
const fieldClass =
  "w-full rounded-lg border border-input bg-transparent px-2.5 py-1.5 text-sm shadow-well outline-none transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 dark:bg-input/30"
const labelClass = "font-medium text-muted-foreground text-xs"

function Field({
  label,
  hint,
  htmlFor,
  children,
}: {
  label: string
  hint?: string
  htmlFor?: string
  children: React.ReactNode
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

// The Settings tab: the lasso build version + update status (top) plus the
// "New Agent" creator configuration. The creator defaults (where to scan for
// repos, the default agent, the scratch setup script) are global; each repo's
// files-to-copy + setup commands are scoped to the active host. All of it
// persists in ~/.lasso/lasso.db.
export function SettingsTab({ active }: { active: boolean }) {
  const [shortcutsOpen, setShortcutsOpen] = React.useState(false)
  const versionQuery = useQuery({
    queryKey: qk.version,
    queryFn: () => api.version(),
    enabled: active,
  })
  const info = versionQuery.data ?? null

  // Which host's settings to edit — each host stores them in its own lasso.db.
  // The picker lists the local machine plus every reachable, compatible remote
  // (those can answer `lasso cli` over SSH). Defaults to the active host.
  const { host: activeHost } = useApp()
  const hostsQuery = useQuery({
    queryKey: ["hosts"],
    queryFn: () => api.hosts(),
    enabled: active,
  })
  const hostOptions = React.useMemo(() => {
    const d = hostsQuery.data
    const opts = [{ value: "local", label: d?.local?.hostname || "local" }]
    for (const h of d?.hosts ?? []) {
      if (h.reachable && h.running && h.compatible)
        opts.push({ value: h.alias, label: h.alias })
    }
    return opts
  }, [hostsQuery.data])
  const [selectedHost, setSelectedHost] = React.useState<string | null>(null)
  // Default to the active host once it's known; keep the user's choice after.
  React.useEffect(() => {
    if (selectedHost == null && activeHost) setSelectedHost(activeHost)
  }, [activeHost, selectedHost])
  const host = selectedHost ?? activeHost ?? "local"

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <header className="border-border border-b bg-background px-3 py-2">
        <div className="flex flex-wrap items-center gap-2">
          <span className="mr-0.5 text-[13px] text-muted-foreground tracking-wide">
            lasso
          </span>
          {info?.lasso_version && (
            <Pill title="this lasso build's version" multiline>
              lasso {info.lasso_version}
            </Pill>
          )}
          {info?.latest_version && info.update_state === "available" && (
            <Pill
              tone="warn"
              title="a newer lasso release is available — run `lasso update`"
              multiline
            >
              update available → {info.latest_version}
            </Pill>
          )}
          <Button
            variant="outline"
            size="icon"
            className="ml-auto size-7"
            title="Keyboard shortcuts"
            onClick={() => setShortcutsOpen(true)}
          >
            <Keyboard />
          </Button>
          <Button
            variant="outline"
            size="icon"
            className="size-7"
            title="re-check for lasso updates"
            onClick={() => versionQuery.refetch()}
          >
            <RotateCw />
          </Button>
        </div>
      </header>

      <div className="@container min-h-0 flex-1 overflow-y-auto px-3 py-4">
        <div className="mb-4 flex flex-col gap-1">
          <label className={labelClass} htmlFor="settings-host">
            Configuring host
          </label>
          <select
            id="settings-host"
            className={cn(fieldClass, "max-w-xs")}
            value={host}
            onChange={(e) => setSelectedHost(e.target.value)}
          >
            {/* Ensure the current value is always selectable even before the
                host probe returns (e.g. an active remote not yet in the list). */}
            {!hostOptions.some((o) => o.value === host) && (
              <option value={host}>{host}</option>
            )}
            {hostOptions.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
                {o.value === activeHost ? " (active)" : ""}
              </option>
            ))}
          </select>
          <p className="text-[11px] text-muted-foreground">
            These settings live in {host}'s own ~/.lasso/lasso.db.
          </p>
        </div>
        <AgentCreatorSettings active={active} host={host} />
      </div>

      <ShortcutsDialog open={shortcutsOpen} onOpenChange={setShortcutsOpen} />
    </div>
  )
}

// ShortcutsDialog shows the app's keyboard shortcuts (the SHORTCUTS the App key
// handler implements) in a modal. Reference only — nothing to configure.
function ShortcutsDialog({
  open,
  onOpenChange,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>Keyboard shortcuts</DialogTitle>
        </DialogHeader>
        <ul className="flex flex-col gap-1.5">
          {SHORTCUTS.map((s) => (
            <li key={s.keys} className="flex items-center gap-3 text-sm">
              <kbd className="min-w-10 rounded border border-border bg-muted px-1.5 py-0.5 text-center font-mono text-muted-foreground text-xs">
                {s.keys}
              </kbd>
              <span className="text-foreground">{s.label}</span>
            </li>
          ))}
        </ul>
      </DialogContent>
    </Dialog>
  )
}

// AgentCreatorSettings edits a host's creator defaults and each repo's
// copy-files + setup, persisted via /api/agent-config and /api/repo-config for
// the given host (its own lasso.db).
function AgentCreatorSettings({
  active,
  host,
}: {
  active: boolean
  host: string
}) {
  const queryClient = useQueryClient()

  const configQuery = useQuery({
    queryKey: qk.agentConfig(host),
    queryFn: () => api.agentConfig(host),
    enabled: active,
  })
  const reposQuery = useQuery({
    queryKey: qk.repos(host),
    queryFn: () => api.repos(host),
    enabled: active,
  })
  const repos = reposQuery.data?.repos ?? []

  // Defaults (editable copies, re-seeded whenever the selected host's config
  // arrives — tracked per host so switching hosts reloads, but a refetch of the
  // same host doesn't clobber in-progress edits).
  const [reposRoot, setReposRoot] = React.useState("")
  const [defaultAgent, setDefaultAgent] = React.useState("")
  const [scratchSetup, setScratchSetup] = React.useState("")
  const seededHostRef = React.useRef<string | null>(null)
  React.useEffect(() => {
    if (seededHostRef.current === host || !configQuery.data) return
    seededHostRef.current = host
    setReposRoot(configQuery.data.repos_root || "")
    // Empty string is meaningful: "Auto (use last used)". Don't coerce to claude.
    setDefaultAgent(configQuery.data.default_agent ?? "")
    setScratchSetup(configQuery.data.scratch_setup || "")
  }, [configQuery.data, host])

  // Per-repo settings.
  const [repoPath, setRepoPath] = React.useState("")
  const [copyFiles, setCopyFiles] = React.useState("")
  const [setup, setSetup] = React.useState("")
  const [savedRepo, setSavedRepo] = React.useState<string | null>(null)

  // Keep the selected repo valid against the (host-scoped) repo list, which
  // changes when the active host switches.
  React.useEffect(() => {
    if (repos.length === 0) return
    setRepoPath((prev) =>
      prev && repos.some((r) => r.path === prev) ? prev : repos[0].path
    )
  }, [repos])

  // Mirror the selected repo's saved copy-files/setup into the editors.
  React.useEffect(() => {
    const re = repos.find((r) => r.path === repoPath)
    setCopyFiles(re?.copy_files || "")
    setSetup(re?.setup || "")
    setSavedRepo(null)
  }, [repoPath, repos])

  const saveDefaultsMutation = useMutation({
    mutationFn: () =>
      api.saveAgentConfig(
        {
          repos_root: reposRoot,
          default_agent: defaultAgent,
          scratch_setup: scratchSetup,
        },
        host
      ),
    onSuccess: () => {
      // Repos root may have changed — refetch both config and the repo scan.
      queryClient.invalidateQueries({ queryKey: qk.agentConfig(host) })
      queryClient.invalidateQueries({ queryKey: qk.repos(host) })
    },
  })

  const saveRepoMutation = useMutation({
    mutationFn: () =>
      api.saveRepoConfig(
        { path: repoPath, copy_files: copyFiles, setup },
        host
      ),
    onSuccess: () => {
      setSavedRepo(repoPath)
      queryClient.invalidateQueries({ queryKey: qk.repos(host) })
    },
  })

  const dirtyRepo = (() => {
    const re = repos.find((r) => r.path === repoPath)
    return (re?.copy_files || "") !== copyFiles || (re?.setup || "") !== setup
  })()

  return (
    <div className="flex @2xl:flex-row flex-col gap-4">
      <section className="flex min-w-0 flex-1 flex-col gap-3 rounded-lg border border-border p-4 shadow-sm">
        <h3 className="font-medium text-foreground text-sm">
          New Agent defaults
        </h3>

        <Field
          label="Git repos directories"
          hint="One directory per line. The repo picker scans each (one level deep) for git repos."
          htmlFor="settings-repos-root"
        >
          <textarea
            id="settings-repos-root"
            className={cn(fieldClass, "resize-none")}
            rows={3}
            value={reposRoot}
            onChange={(e) => setReposRoot(e.target.value)}
            placeholder={"~/projects\n~/work"}
          />
        </Field>

        <Field
          label="Default agent"
          hint="Auto remembers the agent you picked last time instead of forcing one."
          htmlFor="settings-default-agent"
        >
          <select
            id="settings-default-agent"
            className={fieldClass}
            value={defaultAgent}
            onChange={(e) => setDefaultAgent(e.target.value)}
          >
            <option value="">Auto (use last used)</option>
            <option value="claude">Claude Code</option>
            <option value="codex">Codex</option>
          </select>
        </Field>

        <Field
          label="Scratch setup commands"
          hint="Run before the agent in scratch (non-git) workspaces."
          htmlFor="settings-scratch-setup"
        >
          <textarea
            id="settings-scratch-setup"
            className={cn(fieldClass, "resize-none font-mono")}
            rows={3}
            value={scratchSetup}
            onChange={(e) => setScratchSetup(e.target.value)}
            placeholder="uv venv"
          />
        </Field>

        <Button
          type="button"
          size="sm"
          variant="outline"
          className="self-start"
          disabled={saveDefaultsMutation.isPending}
          onClick={() => saveDefaultsMutation.mutate()}
        >
          {saveDefaultsMutation.isPending ? "Saving…" : "Save defaults"}
        </Button>
      </section>

      <section className="flex min-w-0 flex-1 flex-col gap-3 rounded-lg border border-border p-4 shadow-sm">
        <div className="flex flex-col gap-0.5">
          <h3 className="font-medium text-foreground text-sm">
            Per-repository setup
          </h3>
          <p className="text-[11px] text-muted-foreground">
            Files copied into a new worktree and commands run before the agent —
            both relative to the repo, applied to every agent created from it.
          </p>
        </div>

        <Field label="Repository" htmlFor="settings-repo">
          <select
            id="settings-repo"
            className={fieldClass}
            value={repoPath}
            onChange={(e) => setRepoPath(e.target.value)}
          >
            {repos.length === 0 && <option value="">No repos found</option>}
            {repos.map((r) => (
              <option key={r.path} value={r.path}>
                {r.name}
              </option>
            ))}
          </select>
        </Field>

        <Field
          label="Copy files into worktree (globs)"
          hint="Comma- or newline-separated. Matched in the repo, copied into the new worktree (e.g. .env, .env.local)."
          htmlFor="settings-copy-files"
        >
          <textarea
            id="settings-copy-files"
            className={cn(fieldClass, "resize-none")}
            rows={2}
            value={copyFiles}
            onChange={(e) => setCopyFiles(e.target.value)}
            placeholder=".env, .env.local"
            disabled={!repoPath}
          />
        </Field>

        <Field
          label="Setup commands"
          hint="Run in the worktree's shell before the agent starts."
          htmlFor="settings-setup"
        >
          <textarea
            id="settings-setup"
            className={cn(fieldClass, "resize-none font-mono")}
            rows={3}
            value={setup}
            onChange={(e) => setSetup(e.target.value)}
            placeholder="bun install"
            disabled={!repoPath}
          />
        </Field>

        <div className="flex items-center gap-2">
          <Button
            type="button"
            size="sm"
            variant="outline"
            className="self-start"
            disabled={!repoPath || saveRepoMutation.isPending || !dirtyRepo}
            onClick={() => saveRepoMutation.mutate()}
          >
            {saveRepoMutation.isPending ? "Saving…" : "Save repo setup"}
          </Button>
          {savedRepo === repoPath && !dirtyRepo && (
            <span className="text-[11px] text-muted-foreground">Saved</span>
          )}
        </div>
      </section>
    </div>
  )
}
