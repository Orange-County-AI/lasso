import { RotateCw } from "lucide-react"
import * as React from "react"
import { Pill } from "@/components/Pill"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { api, type RepoEntry, type VersionInfo } from "@/lib/api"
import { cn } from "@/lib/utils"

// Native textarea/select styled to match the shadcn <Input>.
const fieldClass =
  "w-full rounded-lg border border-input bg-transparent px-2.5 py-1.5 text-sm outline-none transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 dark:bg-input/30"
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

// The Settings tab: lasso↔herdr socket-protocol compatibility (top) plus the
// "New Agent" creator configuration. Lasso targets a fixed protocol (baked in
// at build time); the daemon reports its own over the socket, and when they
// drift terminals and RPC silently break, so we surface it here. The creator
// defaults (where to scan for repos, the default agent, the scratch setup
// script) and each repo's files-to-copy + setup commands describe the repo and
// environment — not a one-off agent — so they persist in ~/.lasso/config.yaml.
export function SettingsTab({ active }: { active: boolean }) {
  const [info, setInfo] = React.useState<VersionInfo | null>(null)
  const [state, setState] = React.useState<"idle" | "loading" | "error">("idle")
  const loadedOnce = React.useRef(false)

  const load = React.useCallback(async () => {
    setState("loading")
    try {
      setInfo(await api.version())
      setState("idle")
    } catch {
      setInfo(null)
      setState("error")
    }
  }, [])

  // Lazily load on first open, like the original initSettings().
  React.useEffect(() => {
    if (active && !loadedOnce.current) {
      loadedOnce.current = true
      load()
    }
  }, [active, load])

  const loading = state === "loading"
  const errored = state === "error"

  // The herdr-side pill: the daemon's protocol and how it compares to lasso's.
  let herdr: React.ReactNode
  if (loading) {
    herdr = <Pill>herdr …</Pill>
  } else if (errored || !info) {
    herdr = <Pill tone="warn">herdr unavailable</Pill>
  } else if (info.err) {
    herdr = (
      <Pill tone="warn" title={info.err}>
        herdr unreachable
      </Pill>
    )
  } else {
    const ver = info.herdr_version ? ` (${info.herdr_version})` : ""
    herdr = (
      <Pill tone={info.compatible ? "good" : "bad"}>
        herdr protocol {info.herdr_protocol}
        {ver} · {info.compatible ? "compatible" : "incompatible"}
      </Pill>
    )
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <header className="border-border border-b bg-background px-3 py-2">
        <div className="flex flex-wrap items-center gap-2">
          <span className="mr-0.5 text-[13px] text-muted-foreground tracking-wide">
            lasso
          </span>
          <Pill>
            targets protocol{" "}
            {loading ? "…" : errored || !info ? "unknown" : info.lasso_protocol}
          </Pill>
          {herdr}
          {!loading && !errored && info && !info.err && !info.compatible && (
            <span className="text-[13px] text-warn">
              rebuild lasso (or update herdr) so both speak the same protocol
            </span>
          )}
          <Button
            variant="outline"
            size="icon"
            className="ml-auto size-7"
            title="re-check protocol compatibility"
            onClick={load}
          >
            <RotateCw />
          </Button>
        </div>
      </header>

      <div className="@container min-h-0 flex-1 overflow-y-auto px-3 py-4">
        <AgentCreatorSettings active={active} />
      </div>
    </div>
  )
}

// AgentCreatorSettings edits the creator's global defaults and each repo's
// copy-files + setup, persisted via /api/agent-config and /api/repo-config.
function AgentCreatorSettings({ active }: { active: boolean }) {
  const [repos, setRepos] = React.useState<RepoEntry[]>([])
  const loadedOnce = React.useRef(false)

  // Global defaults.
  const [reposRoot, setReposRoot] = React.useState("")
  const [defaultAgent, setDefaultAgent] = React.useState("claude")
  const [scratchSetup, setScratchSetup] = React.useState("")
  const [savingDefaults, setSavingDefaults] = React.useState(false)

  // Per-repo settings.
  const [repoPath, setRepoPath] = React.useState("")
  const [copyFiles, setCopyFiles] = React.useState("")
  const [setup, setSetup] = React.useState("")
  const [savingRepo, setSavingRepo] = React.useState(false)
  const [savedRepo, setSavedRepo] = React.useState<string | null>(null)

  const loadRepos = React.useCallback(() => {
    api
      .repos()
      .then((res) => {
        setRepos(res.repos)
        setRepoPath((prev) => prev || res.repos[0]?.path || "")
      })
      .catch(() => setRepos([]))
  }, [])

  React.useEffect(() => {
    if (!active || loadedOnce.current) return
    loadedOnce.current = true
    api
      .agentConfig()
      .then((c) => {
        setReposRoot(c.repos_root || "")
        setDefaultAgent(c.default_agent || "claude")
        setScratchSetup(c.scratch_setup || "")
      })
      .catch(() => {
        /* leave defaults blank; saving still works */
      })
    loadRepos()
  }, [active, loadRepos])

  // Mirror the selected repo's saved copy-files/setup into the editors.
  React.useEffect(() => {
    const re = repos.find((r) => r.path === repoPath)
    setCopyFiles(re?.copy_files || "")
    setSetup(re?.setup || "")
    setSavedRepo(null)
  }, [repoPath, repos])

  const saveDefaults = async () => {
    setSavingDefaults(true)
    try {
      await api.saveAgentConfig({
        repos_root: reposRoot,
        default_agent: defaultAgent,
        scratch_setup: scratchSetup,
      })
      // Repos root may have changed — rescan.
      loadRepos()
    } catch {
      /* the next save retries; surfacing a toast here is noisy in Settings */
    } finally {
      setSavingDefaults(false)
    }
  }

  const saveRepo = async () => {
    if (!repoPath) return
    setSavingRepo(true)
    try {
      await api.saveRepoConfig({
        path: repoPath,
        copy_files: copyFiles,
        setup,
      })
      // Reflect the save in the in-memory list so switching away and back keeps
      // the edits without a refetch.
      setRepos((prev) =>
        prev.map((r) =>
          r.path === repoPath ? { ...r, copy_files: copyFiles, setup } : r
        )
      )
      setSavedRepo(repoPath)
    } finally {
      setSavingRepo(false)
    }
  }

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
          label="Git repos directory"
          hint="The repo picker scans this directory (one level deep) for git repos."
          htmlFor="settings-repos-root"
        >
          <Input
            id="settings-repos-root"
            value={reposRoot}
            onChange={(e) => setReposRoot(e.target.value)}
            placeholder="~/projects"
          />
        </Field>

        <Field label="Default agent" htmlFor="settings-default-agent">
          <select
            id="settings-default-agent"
            className={fieldClass}
            value={defaultAgent}
            onChange={(e) => setDefaultAgent(e.target.value)}
          >
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
          disabled={savingDefaults}
          onClick={saveDefaults}
        >
          {savingDefaults ? "Saving…" : "Save defaults"}
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
            disabled={!repoPath || savingRepo || !dirtyRepo}
            onClick={saveRepo}
          >
            {savingRepo ? "Saving…" : "Save repo setup"}
          </Button>
          {savedRepo === repoPath && !dirtyRepo && (
            <span className="text-[11px] text-muted-foreground">Saved</span>
          )}
        </div>
      </section>
    </div>
  )
}
