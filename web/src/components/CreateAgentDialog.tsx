import { ChevronDown, Plus, X } from "lucide-react"
import * as React from "react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Combobox } from "@/components/ui/combobox"
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
import { focusHerdrTerminal } from "@/lib/terminal"
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

// Native textarea/select styled to match the shadcn <Input> (same border,
// radius, and background) so every field in the form reads as one set. Fields
// use bg-background (not transparent) so they contrast against the dialog's
// bg-popover surface.
const fieldClass =
  "w-full rounded-lg border border-input bg-background px-2.5 py-1.5 text-sm outline-none transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
const labelClass = "font-medium text-muted-foreground text-xs"

function Field({
  label,
  htmlFor,
  children,
}: {
  label: string
  htmlFor?: string
  children: React.ReactNode
}) {
  return (
    <div className="flex flex-col gap-1">
      <label className={labelClass} htmlFor={htmlFor}>
        {label}
      </label>
      {children}
    </div>
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
  // Set when the dialog closes because an agent was just created, so the close
  // handler hands keyboard focus to the herdr terminal instead of letting Radix
  // restore it to the trigger (which would force the user to click the pane).
  const createdRef = React.useRef(false)

  // Cmd/Ctrl+O opens the creator. Bound only to the floating variant so the
  // shortcut has a single owner even when the Agents-tab button is also mounted.
  React.useEffect(() => {
    if (variant !== "floating") return
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "o") {
        e.preventDefault()
        setOpen(true)
      }
    }
    document.addEventListener("keydown", onKey)
    return () => document.removeEventListener("keydown", onKey)
  }, [variant])

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
  const [files, setFiles] = React.useState<File[]>([])

  // Load config + repos when the dialog opens. We deliberately seed only on the
  // open transition, so `config` is read once and not in the dep list.
  // biome-ignore lint/correctness/useExhaustiveDependencies: seed once on open
  React.useEffect(() => {
    if (!open) return
    api
      .agentConfig()
      .then((c) => {
        setConfig(c)
        setPrefix(c.branch_prefix || "")
        setAgent(c.default_agent || "claude")
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

  // When the repo changes, load its branches + remembered base branch. `repos`
  // is read for the selected entry but intentionally not a dep — it's stable for
  // the dialog's lifetime.
  // biome-ignore lint/correctness/useExhaustiveDependencies: see comment above
  React.useEffect(() => {
    if (!open || type !== "git" || !repo) return
    const re = repos.find((r) => r.path === repo)
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

  const reset = () => {
    setTitle("")
    setDescription("")
    setBranchName("")
    setAutoBranch("")
    setFiles([])
    setPlanMode(false)
    setPlanTouched(false)
    setShowAdvanced(false)
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
        attachments,
        upload_dir: uploadDir,
      }
      if (type === "git") {
        payload.repo = repo
        payload.base_branch = baseBranch || undefined
        payload.branch_prefix = prefix.trim() || undefined
        payload.branch_name = branchName.trim() || autoBranch || undefined
      }
      const rec = await api.createAgent(payload)
      toast.success(`Created agent “${rec.title}”`)
      if (rec.workspace_id) {
        api.focus(rec.workspace_id).catch(() => {})
      }
      createdRef.current = true
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
            title="create a new agent (⌘O)"
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
      <DialogContent
        className="flex max-h-[85dvh] flex-col overflow-hidden sm:max-w-md"
        onCloseAutoFocus={(e) => {
          // On a create-driven close, send focus to the herdr terminal so the
          // user can type into the new agent immediately. Otherwise (cancel /
          // Esc) let Radix restore focus to the trigger as usual.
          if (createdRef.current) {
            createdRef.current = false
            e.preventDefault()
            focusHerdrTerminal()
          }
        }}
      >
        <DialogHeader>
          <DialogTitle>New Agent</DialogTitle>
          <DialogDescription>
            Spin up a coding agent in a herdr worktree or scratch workspace.
          </DialogDescription>
        </DialogHeader>

        {/* A real <form> gives us the "Enter submits unless on the description"
            behavior for free: a native <input> (e.g. Title) submits on Enter
            while the description <textarea> inserts a newline. Comboboxes keep
            their own Enter (item select). The keydown handler adds Cmd/Ctrl+Enter
            as an always-submit, including from the description and comboboxes. */}
        <form
          onSubmit={(e) => {
            e.preventDefault()
            submit()
          }}
          onKeyDown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
              e.preventDefault()
              submit()
            }
          }}
          className="flex min-h-0 flex-1 flex-col gap-4 overflow-hidden"
        >
          <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto pr-1">
            {/* Type toggle */}
            <div className="flex gap-2">
              {(["git", "scratch"] as const).map((t) => (
                <button
                  key={t}
                  type="button"
                  onClick={() => setType(t)}
                  className={cn(
                    "flex-1 rounded-md border bg-background px-3 py-1.5 text-sm capitalize transition-colors",
                    type === t
                      ? "border-primary bg-primary/15 text-primary"
                      : "border-border text-muted-foreground hover:bg-accent hover:text-foreground"
                  )}
                >
                  {t}
                </button>
              ))}
            </div>

            <Field label="Title" htmlFor="agent-title">
              <Input
                id="agent-title"
                className="bg-background dark:bg-background"
                value={title}
                onChange={(e) => onTitleChange(e.target.value)}
                placeholder="What should this agent work on?"
                autoFocus
              />
            </Field>

            <Field label="Description" htmlFor="agent-description">
              <textarea
                id="agent-description"
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

            {type === "git" && (
              <>
                <Field label="Repository" htmlFor="agent-repo">
                  <Combobox
                    id="agent-repo"
                    items={repos.map((r) => ({ value: r.path, label: r.name }))}
                    value={repo}
                    onValueChange={setRepo}
                    placeholder="Select a repository…"
                    filterPlaceholder="Filter repositories…"
                    emptyText="No repos found."
                  />
                </Field>

                <Field label="Base branch" htmlFor="agent-base">
                  <Combobox
                    id="agent-base"
                    items={branches.map((b) => ({ value: b, label: b }))}
                    value={baseBranch}
                    onValueChange={setBaseBranch}
                    placeholder="Select a base branch…"
                    filterPlaceholder="Filter branches…"
                    emptyText="No branches found."
                  />
                </Field>
              </>
            )}

            <Field label="AI agent" htmlFor="agent-agent">
              <select
                id="agent-agent"
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
                    <Field label="Branch prefix" htmlFor="agent-prefix">
                      <Input
                        id="agent-prefix"
                        className="bg-background dark:bg-background"
                        value={prefix}
                        onChange={(e) => setPrefix(e.target.value)}
                        placeholder="feat/"
                      />
                    </Field>
                    <Field label="Branch name" htmlFor="agent-branch">
                      <Input
                        id="agent-branch"
                        className="bg-background dark:bg-background"
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
                  </>
                )}
                <Field label="Attachments" htmlFor="agent-files">
                  <input
                    id="agent-files"
                    type="file"
                    multiple
                    className={cn(
                      fieldClass,
                      "cursor-pointer text-muted-foreground file:mr-2.5 file:cursor-pointer file:rounded file:border-0 file:bg-accent file:px-2 file:py-0.5 file:font-medium file:text-foreground file:text-sm"
                    )}
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
          </div>

          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => setOpen(false)}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={!canSubmit}>
              {submitting ? "Creating…" : "Create agent"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
