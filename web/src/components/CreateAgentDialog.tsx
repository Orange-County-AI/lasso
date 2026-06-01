import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
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
import { api, type CreateAgentPayload } from "@/lib/api"
import { qk } from "@/lib/query"
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
const imagePathRE = /\/[\w\-/.]+\.(?:png|jpe?g|gif|webp)/gi

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

function extractImagePaths(text: string): string[] {
  return [...new Set(text.match(imagePathRE) || [])]
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
  const [showAdvanced, setShowAdvanced] = React.useState(false)
  const queryClient = useQueryClient()
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

  // Server state via TanStack Query, fetched while the dialog is open. All three
  // are host-scoped on the backend and invalidated on a host switch (see
  // lib/query), so reopening on a new host pulls that host's remembered state.
  const configQuery = useQuery({
    queryKey: qk.agentConfig,
    queryFn: () => api.agentConfig(),
    enabled: open,
  })
  const reposQuery = useQuery({
    queryKey: qk.repos,
    queryFn: () => api.repos(),
    enabled: open,
  })
  const config = configQuery.data ?? null
  const repos = reposQuery.data?.repos ?? []

  // Form state.
  const [type, setType] = React.useState<AgentType>("git")
  const [title, setTitle] = React.useState("")
  const [repo, setRepo] = React.useState("")
  const [baseBranch, setBaseBranch] = React.useState("")
  const [prefix, setPrefix] = React.useState("")
  const [branchName, setBranchName] = React.useState("")
  const [autoBranch, setAutoBranch] = React.useState("")
  const [agent, setAgent] = React.useState("claude")
  const [description, setDescription] = React.useState("")
  const [pastingImage, setPastingImage] = React.useState(false)
  const [planMode, setPlanMode] = React.useState(false)
  const [planTouched, setPlanTouched] = React.useState(false)
  const [files, setFiles] = React.useState<File[]>([])
  const descriptionRef = React.useRef<HTMLTextAreaElement>(null)

  const branchesQuery = useQuery({
    queryKey: qk.repoBranches(repo),
    queryFn: () => api.repoBranches(repo),
    enabled: open && type === "git" && !!repo,
  })
  const branches = React.useMemo(() => {
    const b = branchesQuery.data
    return b ? [...b.branches, ...b.remoteBranches] : []
  }, [branchesQuery.data])

  // Seed the form from the remembered selections once per open (not on every
  // data change, so it never clobbers in-progress edits): the last agent type,
  // branch prefix, AI agent (default_agent, else last_agent, else claude), and
  // the last repo if it still exists on this host.
  const seededRef = React.useRef(false)
  React.useEffect(() => {
    if (!open) {
      seededRef.current = false
      return
    }
    if (seededRef.current || !config || !reposQuery.isSuccess) return
    seededRef.current = true
    setType(config.last_agent_type || "git")
    setPrefix(config.branch_prefix || "")
    setAgent(config.default_agent || config.last_agent || "claude")
    const last = config.last_repo
    setRepo(
      last && repos.some((r) => r.path === last) ? last : (repos[0]?.path ?? "")
    )
  }, [open, config, reposQuery.isSuccess, repos])

  // When the selected repo's branches load, pick its remembered base branch (if
  // still present) else the repo's default. Keyed on the branches data so it
  // settles once per repo rather than fighting a manual pick.
  // biome-ignore lint/correctness/useExhaustiveDependencies: repos is stable for the dialog's lifetime
  React.useEffect(() => {
    if (!open || type !== "git" || !repo) return
    const re = repos.find((r) => r.path === repo)
    const b = branchesQuery.data
    if (!b) {
      if (branchesQuery.isError) setBaseBranch(re?.last_base_branch || "main")
      return
    }
    const all = [...b.branches, ...b.remoteBranches]
    const preferred = re?.last_base_branch
    setBaseBranch(
      preferred && all.includes(preferred)
        ? preferred
        : b.default || b.branches[0] || "main"
    )
  }, [repo, open, type, branchesQuery.data, branchesQuery.isError])

  const onTitleChange = (v: string) => {
    setTitle(v)
    setAutoBranch(generateBranchName(v))
  }

  const onDescriptionChange = (v: string) => {
    setDescription(v)
    // Description present ⇒ plan mode, unless the user toggled it themselves.
    if (!planTouched) setPlanMode(v.trim().length > 0)
  }

  const onDescriptionPaste = async (
    e: React.ClipboardEvent<HTMLTextAreaElement>
  ) => {
    const item = Array.from(e.clipboardData?.items ?? []).find(
      (it) => it.kind === "file" && it.type.startsWith("image/")
    )
    if (!item) return
    const file = item.getAsFile()
    if (!file) return

    e.preventDefault()
    setPastingImage(true)
    try {
      const { path } = await api.pasteImage(file)
      const textarea = descriptionRef.current
      if (!textarea) {
        onDescriptionChange(description + (description ? "\n" : "") + path)
        return
      }

      const start = textarea.selectionStart
      const end = textarea.selectionEnd
      const before = description.slice(0, start)
      const after = description.slice(end)
      const prefix = before && !before.endsWith("\n") ? "\n" : ""
      const suffix = after && !after.startsWith("\n") ? "\n" : ""
      const inserted = `${prefix}${path}${suffix}`
      const next = before + inserted + after
      onDescriptionChange(next)
      requestAnimationFrame(() => {
        textarea.focus()
        const cursor = before.length + inserted.length
        textarea.setSelectionRange(cursor, cursor)
      })
    } catch (err) {
      toast.error("Failed to paste image", {
        description: err instanceof Error ? err.message : String(err),
      })
    } finally {
      setPastingImage(false)
    }
  }

  const reset = () => {
    setTitle("")
    setDescription("")
    setPastingImage(false)
    setBranchName("")
    setAutoBranch("")
    setFiles([])
    setPlanMode(false)
    setPlanTouched(false)
    setShowAdvanced(false)
  }

  const createMutation = useMutation({
    mutationFn: async () => {
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
      return api.createAgent(payload)
    },
    onSuccess: (rec) => {
      toast.success(`Created agent “${rec.title}”`)
      if (rec.workspace_id) {
        api.focus(rec.workspace_id).catch(() => {})
      }
      createdRef.current = true
      setOpen(false)
      reset()
      // The creator just updated this host's remembered selections + agent log,
      // so refetch them.
      queryClient.invalidateQueries({ queryKey: qk.agentConfig })
      queryClient.invalidateQueries({ queryKey: qk.repos })
      onCreated?.()
    },
    onError: (err) => {
      toast.error("Failed to create agent", {
        description: err instanceof Error ? err.message : String(err),
      })
    },
  })

  const canSubmit =
    !createMutation.isPending &&
    !pastingImage &&
    title.trim().length > 0 &&
    (type === "scratch" || !!repo)

  const submit = () => {
    if (!canSubmit) return
    createMutation.mutate()
  }

  const effectiveBranch = (() => {
    const raw = branchName.trim() || autoBranch
    const p = prefix.trim().replace(/\/+$/, "")
    return p && raw ? `${p}/${raw}` : raw
  })()
  const pastedImagePaths = extractImagePaths(description)

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
              <div className="flex flex-col gap-2">
                <div className="relative">
                  <textarea
                    ref={descriptionRef}
                    id="agent-description"
                    className={cn(
                      fieldClass,
                      "resize-none",
                      pastingImage && "opacity-50"
                    )}
                    rows={3}
                    value={description}
                    onChange={(e) => onDescriptionChange(e.target.value)}
                    onPaste={onDescriptionPaste}
                    disabled={pastingImage}
                    placeholder="Optional — fills the agent's first prompt. If set, the agent starts in plan mode."
                  />
                  {pastingImage && (
                    <div className="absolute inset-0 flex items-center justify-center rounded-lg bg-background/60 text-muted-foreground text-sm">
                      Uploading image…
                    </div>
                  )}
                </div>
                {pastedImagePaths.length > 0 && (
                  <div className="flex flex-wrap gap-2">
                    {pastedImagePaths.map((path) => (
                      <a
                        key={path}
                        href={api.fileURL(path)}
                        target="_blank"
                        rel="noreferrer"
                        className="block rounded border border-border bg-card p-1 transition-opacity hover:opacity-80"
                      >
                        <img
                          src={api.fileURL(path)}
                          alt={path}
                          className="h-16 w-auto max-w-32 object-contain"
                        />
                      </a>
                    ))}
                  </div>
                )}
              </div>
            </Field>

            <div className="flex items-center gap-2">
              <Checkbox
                id="agent-plan-mode"
                checked={planMode}
                onCheckedChange={(v) => {
                  setPlanMode(v === true)
                  setPlanTouched(true)
                }}
              />
              <label
                htmlFor="agent-plan-mode"
                className="cursor-pointer text-sm"
              >
                Start in plan mode
              </label>
            </div>

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
              {createMutation.isPending ? "Creating…" : "Create agent"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
