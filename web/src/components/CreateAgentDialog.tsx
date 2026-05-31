import { ChevronDown, Plus, Settings2, X } from "lucide-react"
import * as React from "react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import {
  type AgentConfig,
  api,
  type CreateAgentPayload,
  type RepoEntry,
} from "@/lib/api"
import { cn } from "@/lib/utils"

type AgentType = "git" | "scratch"

const AGENTS: { value: string; label: string }[] = [
  { value: "claude", label: "Claude Code" },
  { value: "codex", label: "Codex" },
]

function slugify(text: string): string {
  return text
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
}

// A short random suffix keeps auto-generated branch/dir names unique.
function randomSuffix(): string {
  return Math.random().toString(36).slice(2, 6)
}

function generateBranchName(title: string): string {
  const words = title.trim().split(/\s+/).slice(0, 4).join(" ")
  const slug = slugify(words)
  return slug ? `${slug}-${randomSuffix()}` : ""
}

// Shared styles for the native textarea/select so they match the shadcn inputs.
const fieldClass =
  "w-full rounded-md border border-input bg-background px-3 py-2 text-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
const labelClass = "text-xs font-medium text-muted-foreground"

function Field({
  label,
  children,
}: {
  label: string
  children: React.ReactNode
}) {
  return (
    <label className="flex flex-col gap-1">
      <span className={labelClass}>{label}</span>
      {children}
    </label>
  )
}

export function CreateAgentDialog({
  onCreated,
  variant = "button",
}: {
  onCreated?: () => void
  // "button" — the inline outline button on the Agents tab header.
  // "floating" — a pill matching the host switcher, for the bottom-left footer.
  variant?: "button" | "floating"
}) {
  const [open, setOpen] = React.useState(false)
  const [submitting, setSubmitting] = React.useState(false)
  const [showAdvanced, setShowAdvanced] = React.useState(false)
  const [showSettings, setShowSettings] = React.useState(false)

  // Settings / defaults (from ~/.lasso/config.yaml).
  const [config, setConfig] = React.useState<AgentConfig | null>(null)
  const [repos, setRepos] = React.useState<RepoEntry[]>([])

  // Form state.
  const [type, setType] = React.useState<AgentType>("git")
  const [title, setTitle] = React.useState("")
  const [repo, setRepo] = React.useState("")
  const [baseBranch, setBaseBranch] = React.useState("")
  const [branches, setBranches] = React.useState<string[]>([])
  const [prefix, setPrefix] = React.useState("")
  const [branchName, setBranchName] = React.useState("")
  const [autoBranch, setAutoBranch] = React.useState("")
  const [agent, setAgent] = React.useState("claude")
  const [description, setDescription] = React.useState("")
  const [planMode, setPlanMode] = React.useState(false)
  const [planTouched, setPlanTouched] = React.useState(false)
  const [notes, setNotes] = React.useState("")
  const [copyFiles, setCopyFiles] = React.useState("")
  const [setup, setSetup] = React.useState("")
  const [files, setFiles] = React.useState<File[]>([])

  // Editable settings (mirrors config; saved on demand).
  const [reposRoot, setReposRoot] = React.useState("")
  const [scratchSetup, setScratchSetup] = React.useState("")

  const selectedRepo = repos.find((r) => r.path === repo)

  // Load config + repos when the dialog opens. We deliberately seed only on the
  // open transition, so `type`/`config` are read once and not in the dep list.
  // biome-ignore lint/correctness/useExhaustiveDependencies: seed once on open
  React.useEffect(() => {
    if (!open) return
    api
      .agentConfig()
      .then((c) => {
        setConfig(c)
        setPrefix(c.branch_prefix || "")
        setAgent(c.default_agent || "claude")
        setReposRoot(c.repos_root || "")
        setScratchSetup(c.scratch_setup || "")
        if (type === "scratch") setSetup(c.scratch_setup || "")
      })
      .catch(() => {
        /* creator still works with defaults */
      })
    api
      .repos()
      .then((res) => {
        setRepos(res.repos)
        // Preselect the last-used repo, else the first one.
        setRepo((prev) => {
          if (prev) return prev
          const last = config?.last_repo
          if (last && res.repos.some((r) => r.path === last)) return last
          return res.repos[0]?.path ?? ""
        })
      })
      .catch(() => setRepos([]))
  }, [open])

  // When the repo changes, load its branches + remembered per-repo state.
  // `repos` is read for the selected entry but intentionally not a dep — it's
  // stable for the dialog's lifetime and re-running on every repos change would
  // clobber the user's edits.
  // biome-ignore lint/correctness/useExhaustiveDependencies: see comment above
  React.useEffect(() => {
    if (!open || type !== "git" || !repo) return
    const re = repos.find((r) => r.path === repo)
    setCopyFiles(re?.copy_files || "")
    setSetup(re?.setup || "")
    api
      .repoBranches(repo)
      .then((b) => {
        const all = [...b.branches, ...b.remoteBranches]
        setBranches(all)
        const preferred = re?.last_base_branch
        setBaseBranch(
          preferred && all.includes(preferred)
            ? preferred
            : b.default || b.branches[0] || "main"
        )
      })
      .catch(() => {
        setBranches([])
        setBaseBranch(re?.last_base_branch || "main")
      })
  }, [repo, open, type])

  const onTitleChange = (v: string) => {
    setTitle(v)
    setAutoBranch(generateBranchName(v))
  }

  const onDescriptionChange = (v: string) => {
    setDescription(v)
    // Description present ⇒ plan mode, unless the user toggled it themselves.
    if (!planTouched) setPlanMode(v.trim().length > 0)
  }

  const onTypeChange = (t: AgentType) => {
    setType(t)
    if (t === "scratch") setSetup(scratchSetup)
    else setSetup(selectedRepo?.setup || "")
  }

  const reset = () => {
    setTitle("")
    setDescription("")
    setNotes("")
    setBranchName("")
    setAutoBranch("")
    setFiles([])
    setPlanMode(false)
    setPlanTouched(false)
    setShowAdvanced(false)
    setShowSettings(false)
  }

  const canSubmit =
    !submitting && title.trim().length > 0 && (type === "scratch" || !!repo)

  const submit = async () => {
    if (!canSubmit) return
    setSubmitting(true)
    try {
      let uploadDir: string | undefined
      let attachments: string[] | undefined
      if (files.length > 0) {
        const up = await api.uploadAgentFiles(files)
        uploadDir = up.upload_dir
        attachments = up.files
      }
      const payload: CreateAgentPayload = {
        type,
        title: title.trim(),
        agent,
        plan_mode: planMode,
        description: description.trim() || undefined,
        notes: notes.trim() || undefined,
        setup: setup.trim() || undefined,
        attachments,
        upload_dir: uploadDir,
      }
      if (type === "git") {
        payload.repo = repo
        payload.base_branch = baseBranch || undefined
        payload.branch_prefix = prefix.trim() || undefined
        payload.branch_name = branchName.trim() || autoBranch || undefined
        payload.copy_files = copyFiles.trim() || undefined
      }
      const rec = await api.createAgent(payload)
      toast.success(`Created agent “${rec.title}”`)
      if (rec.workspace_id) {
        api.focus(rec.workspace_id).catch(() => {})
      }
      setOpen(false)
      reset()
      onCreated?.()
    } catch (err) {
      toast.error("Failed to create agent", {
        description: err instanceof Error ? err.message : String(err),
      })
    } finally {
      setSubmitting(false)
    }
  }

  const saveSettings = async () => {
    try {
      const c = await api.saveAgentConfig({
        repos_root: reposRoot,
        branch_prefix: prefix,
        default_agent: agent,
        scratch_setup: scratchSetup,
      })
      setConfig(c)
      toast.success("Saved defaults")
      // Refresh repos in case the root changed.
      api
        .repos()
        .then((res) => setRepos(res.repos))
        .catch(() => {})
    } catch (err) {
      toast.error("Failed to save defaults", {
        description: err instanceof Error ? err.message : String(err),
      })
    }
  }

  const effectiveBranch = (() => {
    const raw = branchName.trim() || autoBranch
    const p = prefix.trim().replace(/\/+$/, "")
    return p && raw ? `${p}/${raw}` : raw
  })()

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        {variant === "floating" ? (
          // Mirrors the HostSwitcher pill so the two footer controls read as a
          // pair (see App.tsx, where this sits to the host button's right).
          <button
            type="button"
            className="flex items-center gap-1 rounded-full border border-border bg-card/90 px-2 py-0.5 text-[11px] text-muted-foreground shadow-md backdrop-blur transition-colors hover:bg-accent hover:text-foreground"
            title="create a new agent"
          >
            <Plus className="size-3" />
            <span className="font-medium">New Agent</span>
          </button>
        ) : (
          <Button size="sm" variant="outline" className="gap-1">
            <Plus className="size-4" />
            New Agent
          </Button>
        )}
      </DialogTrigger>
      <DialogContent className="flex max-h-[85dvh] flex-col overflow-hidden sm:max-w-md">
        <DialogHeader>
          <DialogTitle>New Agent</DialogTitle>
          <DialogDescription>
            Spin up a coding agent in a herdr worktree or scratch workspace.
          </DialogDescription>
        </DialogHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto pr-1">
          {/* Type toggle */}
          <div className="flex gap-2">
            {(["git", "scratch"] as const).map((t) => (
              <button
                key={t}
                type="button"
                onClick={() => onTypeChange(t)}
                className={cn(
                  "flex-1 rounded-md border px-3 py-1.5 text-sm capitalize",
                  type === t
                    ? "border-primary bg-primary/10 text-primary"
                    : "border-border text-muted-foreground hover:text-foreground"
                )}
              >
                {t}
              </button>
            ))}
          </div>

          <Field label="Title">
            <Input
              value={title}
              onChange={(e) => onTitleChange(e.target.value)}
              placeholder="What should this agent work on?"
              autoFocus
            />
          </Field>

          {type === "git" && (
            <>
              <Field label="Repository">
                <select
                  className={fieldClass}
                  value={repo}
                  onChange={(e) => setRepo(e.target.value)}
                >
                  {repos.length === 0 && (
                    <option value="">No repos found</option>
                  )}
                  {repos.map((r) => (
                    <option key={r.path} value={r.path}>
                      {r.name}
                    </option>
                  ))}
                </select>
              </Field>

              <Field label="Base branch">
                <select
                  className={fieldClass}
                  value={baseBranch}
                  onChange={(e) => setBaseBranch(e.target.value)}
                >
                  {branches.length === 0 && (
                    <option value={baseBranch}>{baseBranch || "main"}</option>
                  )}
                  {branches.map((b) => (
                    <option key={b} value={b}>
                      {b}
                    </option>
                  ))}
                </select>
              </Field>
            </>
          )}

          <Field label="AI agent">
            <select
              className={fieldClass}
              value={agent}
              onChange={(e) => setAgent(e.target.value)}
            >
              {AGENTS.map((a) => (
                <option key={a.value} value={a.value}>
                  {a.label}
                </option>
              ))}
            </select>
          </Field>

          <Field label="Description">
            <textarea
              className={cn(fieldClass, "resize-none")}
              rows={3}
              value={description}
              onChange={(e) => onDescriptionChange(e.target.value)}
              placeholder="Optional — fills the agent's first prompt. If set, the agent starts in plan mode."
            />
          </Field>

          <label className="flex cursor-pointer items-center gap-2">
            <Checkbox
              checked={planMode}
              onCheckedChange={(v) => {
                setPlanMode(v === true)
                setPlanTouched(true)
              }}
            />
            <span className="text-sm">Start in plan mode</span>
          </label>

          {/* Advanced */}
          <button
            type="button"
            className="flex items-center gap-1 text-muted-foreground text-sm hover:text-foreground"
            onClick={() => setShowAdvanced((s) => !s)}
          >
            <ChevronDown
              className={cn(
                "size-4 transition-transform",
                showAdvanced && "rotate-180"
              )}
            />
            Advanced
          </button>

          {showAdvanced && (
            <div className="flex flex-col gap-3 border-border border-l pl-3">
              {type === "git" && (
                <>
                  <Field label="Branch prefix">
                    <Input
                      value={prefix}
                      onChange={(e) => setPrefix(e.target.value)}
                      placeholder="feat/"
                    />
                  </Field>
                  <Field label="Branch name">
                    <Input
                      value={branchName}
                      onChange={(e) => setBranchName(e.target.value)}
                      placeholder={autoBranch || "auto-generated"}
                    />
                  </Field>
                  {effectiveBranch && (
                    <p className="-mt-1 font-mono text-muted-foreground text-xs">
                      branch: {effectiveBranch}
                    </p>
                  )}
                  <Field label="Copy files into worktree (globs)">
                    <textarea
                      className={cn(fieldClass, "resize-none")}
                      rows={2}
                      value={copyFiles}
                      onChange={(e) => setCopyFiles(e.target.value)}
                      placeholder=".env, .env.local"
                    />
                  </Field>
                </>
              )}
              <Field label="Setup commands (run before the agent)">
                <textarea
                  className={cn(fieldClass, "resize-none font-mono")}
                  rows={3}
                  value={setup}
                  onChange={(e) => setSetup(e.target.value)}
                  placeholder={type === "git" ? "bun install" : "uv venv"}
                />
              </Field>
              <Field label="Notes (saved to NOTES.md)">
                <textarea
                  className={cn(fieldClass, "resize-none")}
                  rows={2}
                  value={notes}
                  onChange={(e) => setNotes(e.target.value)}
                />
              </Field>
              <Field label="Attachments">
                <input
                  type="file"
                  multiple
                  className="text-muted-foreground text-sm"
                  onChange={(e) =>
                    setFiles((prev) => [
                      ...prev,
                      ...Array.from(e.target.files ?? []),
                    ])
                  }
                />
              </Field>
              {files.length > 0 && (
                <div className="flex flex-wrap gap-1">
                  {files.map((f, i) => (
                    <span
                      key={`${f.name}-${f.size}-${f.lastModified}`}
                      className="inline-flex items-center gap-1 rounded border border-border bg-card px-1.5 py-0.5 text-xs"
                    >
                      {f.name}
                      <button
                        type="button"
                        onClick={() =>
                          setFiles((prev) => prev.filter((_, j) => j !== i))
                        }
                        className="text-muted-foreground hover:text-foreground"
                      >
                        <X className="size-3" />
                      </button>
                    </span>
                  ))}
                </div>
              )}
            </div>
          )}

          {/* Settings / defaults */}
          <button
            type="button"
            className="flex items-center gap-1 text-muted-foreground text-sm hover:text-foreground"
            onClick={() => setShowSettings((s) => !s)}
          >
            <Settings2 className="size-4" />
            Defaults
          </button>

          {showSettings && (
            <div className="flex flex-col gap-3 border-border border-l pl-3">
              <Field label="Repos root">
                <Input
                  value={reposRoot}
                  onChange={(e) => setReposRoot(e.target.value)}
                  placeholder="~/projects"
                />
              </Field>
              <Field label="Default scratch setup">
                <textarea
                  className={cn(fieldClass, "resize-none font-mono")}
                  rows={2}
                  value={scratchSetup}
                  onChange={(e) => setScratchSetup(e.target.value)}
                />
              </Field>
              <Button
                type="button"
                size="sm"
                variant="outline"
                onClick={saveSettings}
              >
                Save defaults
              </Button>
            </div>
          )}
        </div>

        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            onClick={() => setOpen(false)}
          >
            Cancel
          </Button>
          <Button type="button" disabled={!canSubmit} onClick={submit}>
            {submitting ? "Creating…" : "Create agent"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
