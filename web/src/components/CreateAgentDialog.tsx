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
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { api, type CreateAgentPayload } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { qk, treeAddScratchWorkspace, treeAddWorktree } from "@/lib/query"
import { cn } from "@/lib/utils"

type AgentType = "git" | "scratch"

const AGENTS: { value: string; label: string }[] = [
  { value: "claude", label: "Claude Code" },
  { value: "codex", label: "Codex" },
]

// One of these is picked at random for the Prompt field's placeholder each time
// the dialog opens — a little personality on an otherwise blank canvas.
const PROMPT_PLACEHOLDERS = [
  "Let's go!",
  "What do you want?",
  "What are we doing here?",
  "What are we working on today?",
  "Shouldn't you be outside?",
  "My body is ready.",
  "What's the mission?",
  "Point me at something.",
  "What's broken?",
  "Describe the dream.",
  "Make it so.",
  "The first line becomes the title…",
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

// The prompt's first line acts as the title (branch/dir name, workspace label).
function firstLine(text: string): string {
  return text.trim().split("\n", 1)[0].trim()
}

// Native textarea/select styled to match the shadcn <Input> (same border,
// radius, and background) so every field in the form reads as one set. Fields
// use bg-background (not transparent) so they contrast against the dialog's
// bg-popover surface.
const fieldClass =
  "w-full rounded-lg border border-input bg-background px-2.5 py-1.5 text-sm shadow-well outline-none transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
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
  // "floating" — a pill for the bottom-left footer.
  // "header" — a compact button for the left column's tab-strip trailing slot.
  variant?: "button" | "floating" | "header"
}) {
  const [open, setOpen] = React.useState(false)
  const [showAdvanced, setShowAdvanced] = React.useState(false)
  const [placeholderIdx, setPlaceholderIdx] = React.useState(0)
  const queryClient = useQueryClient()
  // Set when the dialog closes because an agent was just created, so the close
  // handler hands keyboard focus to the terminal instead of letting Radix
  // restore it to the trigger (which would force the user to click the pane).
  const createdRef = React.useRef(false)

  // A repo (+ base) the sidebar's "New agent…" wants the form prefilled with,
  // applied in the seed effect on the next open. Cleared once consumed.
  const pendingPrefill = React.useRef<{ repo?: string; base?: string } | null>(
    null
  )

  // Cmd/Ctrl+O opens the creator, and the "lasso:new-agent" event opens it
  // prefilled (from the sidebar's repo right-click). Bound to the non-"button"
  // variants so the shortcut/event has a single owner.
  React.useEffect(() => {
    if (variant === "button") return
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "o") {
        e.preventDefault()
        setOpen(true)
      }
    }
    const onNew = (e: Event) => {
      pendingPrefill.current =
        ((e as CustomEvent).detail as { repo?: string; base?: string }) ?? null
      setOpen(true)
    }
    document.addEventListener("keydown", onKey)
    window.addEventListener("lasso:new-agent", onNew)
    return () => {
      document.removeEventListener("keydown", onKey)
      window.removeEventListener("lasso:new-agent", onNew)
    }
  }, [variant])

  // Pick a fresh random Prompt placeholder each time the dialog opens — a little
  // personality on the blank canvas, without the distraction of it animating.
  React.useEffect(() => {
    if (!open) return
    setPlaceholderIdx(Math.floor(Math.random() * PROMPT_PLACEHOLDERS.length))
  }, [open])

  // Target host for the new agent. Seeded from the active host each open; the
  // host picker (shown only when reachable remote hosts exist) can override it.
  const { host: activeHost } = useApp()
  const [host, setHost] = React.useState("local")
  React.useEffect(() => {
    if (open) setHost(activeHost || "local")
  }, [open, activeHost])

  const hostsQuery = useQuery({
    queryKey: qk.hosts,
    queryFn: () => api.hosts(),
    enabled: open,
    staleTime: 20_000,
  })
  const remoteHosts = (hostsQuery.data?.hosts ?? []).filter(
    (h) => h.reachable && h.has_tmux
  )

  // Server state via TanStack Query, fetched while the dialog is open — re-keyed
  // by the selected host so the repo/branch pickers reflect that host's fs.
  const configQuery = useQuery({
    queryKey: qk.agentConfig(host),
    queryFn: () => api.agentConfig(host),
    enabled: open,
  })
  const reposQuery = useQuery({
    queryKey: qk.repos(host),
    queryFn: () => api.repos(host),
    enabled: open,
  })
  const config = configQuery.data ?? null
  const repos = reposQuery.data?.repos ?? []

  // Form state.
  const [type, setType] = React.useState<AgentType>("git")
  const [prompt, setPrompt] = React.useState("")
  const [repo, setRepo] = React.useState("")
  const [baseBranch, setBaseBranch] = React.useState("")
  const [prefix, setPrefix] = React.useState("")
  const [branchName, setBranchName] = React.useState("")
  const [autoBranch, setAutoBranch] = React.useState("")
  const [agent, setAgent] = React.useState("claude")
  const [pastingImage, setPastingImage] = React.useState(false)
  const [planMode, setPlanMode] = React.useState(false)
  const [files, setFiles] = React.useState<File[]>([])
  const promptRef = React.useRef<HTMLTextAreaElement>(null)

  const branchesQuery = useQuery({
    queryKey: qk.repoBranches(host, repo),
    queryFn: () => api.repoBranches(repo, host),
    enabled: open && type === "git" && !!repo,
  })
  const branches = React.useMemo(() => {
    const b = branchesQuery.data
    // branches/remoteBranches can be null (Go nil slice → JSON null) when the
    // repo path doesn't resolve.
    return b ? [...(b.branches ?? []), ...(b.remoteBranches ?? [])] : []
  }, [branchesQuery.data])

  // Seed the form from the remembered selections once per open (not on every
  // data change, so it never clobbers in-progress edits): the last agent type,
  // branch prefix, AI agent (default_agent, else last_agent, else claude), and
  // the last repo if it still exists — gated on the repos query having settled so
  // it doesn't seed from a stale list while the refetch is still in flight.
  const seeded = React.useRef(false)
  React.useEffect(() => {
    if (!open) {
      seeded.current = false
      return
    }
    if (!config || !reposQuery.isSuccess || reposQuery.isFetching) return
    if (seeded.current) return
    seeded.current = true
    const pf = pendingPrefill.current
    pendingPrefill.current = null
    setType(pf?.repo ? "git" : config.last_agent_type || "git")
    setPrefix(config.branch_prefix || "")
    setAgent(config.default_agent || config.last_agent || "claude")
    if (pf?.repo) {
      setRepo(pf.repo) // base auto-resolves via the branches effect below
    } else {
      const last = config.last_repo
      setRepo(
        last && repos.some((r) => r.path === last)
          ? last
          : (repos[0]?.path ?? "")
      )
    }
  }, [open, config, reposQuery.isSuccess, reposQuery.isFetching, repos])

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
    const all = [...(b.branches ?? []), ...(b.remoteBranches ?? [])]
    const preferred = re?.last_base_branch
    // Fall back to the repo's detected default, else a present main/master, else
    // the first branch. (b.default already resolves main vs master via origin/HEAD;
    // this guards the case where it's empty but main/master still exists.)
    const fallback =
      b.default ||
      (all.includes("main")
        ? "main"
        : all.includes("master")
          ? "master"
          : "") ||
      all[0] ||
      "main"
    setBaseBranch(preferred && all.includes(preferred) ? preferred : fallback)
  }, [repo, open, type, branchesQuery.data, branchesQuery.isError])

  const onPromptChange = (v: string) => {
    setPrompt(v)
    // The first line drives the auto-generated branch/dir name.
    setAutoBranch(generateBranchName(firstLine(v)))
  }

  const onPromptPaste = async (
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
      const { path } = await api.pasteImage(file, host)
      const textarea = promptRef.current
      if (!textarea) {
        onPromptChange(prompt + (prompt ? "\n" : "") + path)
        return
      }

      const start = textarea.selectionStart
      const end = textarea.selectionEnd
      const before = prompt.slice(0, start)
      const after = prompt.slice(end)
      const prefix = before && !before.endsWith("\n") ? "\n" : ""
      const suffix = after && !after.startsWith("\n") ? "\n" : ""
      const inserted = `${prefix}${path}${suffix}`
      const next = before + inserted + after
      onPromptChange(next)
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
    setPrompt("")
    setPastingImage(false)
    setBranchName("")
    setAutoBranch("")
    setFiles([])
    setPlanMode(false)
    setShowAdvanced(false)
  }

  const createMutation = useMutation({
    mutationFn: async () => {
      let uploadDir: string | undefined
      let attachments: string[] | undefined
      if (files.length > 0) {
        const up = await api.uploadAgentFiles(files, host)
        uploadDir = up.upload_dir
        attachments = up.files
      }
      const payload: CreateAgentPayload = {
        host,
        type,
        prompt: prompt.trim(),
        agent,
        plan_mode: planMode,
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
      // If the agent was created on a different host than the active one, switch
      // to it so the sidebar tree re-scopes and the new tab resolves (the focus
      // dispatch below then lands on it once the re-scoped tree arrives).
      const created = rec.host || host
      if (created !== (activeHost || "local")) {
        void api.switchHost(created)
      } else if (rec.workspace_id && rec.tab_id) {
        // Same host: surface the new workspace optimistically before the refetch.
        const tab = {
          id: rec.tab_id,
          title: rec.title,
          kind: "agent" as const,
          agent: rec.agent,
          status: "idle" as const,
        }
        if (rec.type === "git" && rec.repo) {
          treeAddWorktree(rec.repo, {
            id: rec.workspace_id,
            title: rec.title,
            repo: rec.repo,
            work_dir: rec.work_dir,
            kind: "git",
            branch: rec.branch,
            tabs: [tab],
          })
        } else {
          treeAddScratchWorkspace({
            id: rec.workspace_id,
            title: rec.title,
            work_dir: rec.work_dir,
            kind: "scratch",
            tabs: [tab],
          })
        }
      }
      // Focus the new agent in the UI — select its tab (and show its terminal).
      // Only the UI creator does this; agents created via MCP must NOT steal the
      // user's focus, and they don't (MCP never dispatches this).
      if (rec.tab_id) {
        window.dispatchEvent(
          new CustomEvent("lasso:select-tab", { detail: rec.tab_id })
        )
      }
      createdRef.current = true
      setOpen(false)
      reset()
      // The creator just updated the remembered selections + agent log, so
      // refetch them.
      queryClient.invalidateQueries({ queryKey: ["agent-config"] })
      queryClient.invalidateQueries({ queryKey: ["repos"] })
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
    prompt.trim().length > 0 &&
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
  const pastedImagePaths = extractImagePaths(prompt)

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        {variant === "header" ? (
          // Prominent (but unfilled) button for the left column's tab-strip
          // trailing slot — the accent carried by the border + text, with only a
          // faint tint, so it reads as the row's primary action without a solid fill.
          <button
            type="button"
            className="mx-2 my-1 flex shrink-0 items-center gap-1 self-center rounded-md border border-primary/60 bg-primary/10 px-2.5 py-1 font-medium text-primary text-xs transition-colors hover:border-primary hover:bg-primary/20"
            title="create a new agent (⌘O)"
          >
            <Plus className="size-3.5" />
            <span className="max-md:hidden">New Agent</span>
          </button>
        ) : variant === "floating" ? (
          // Footer pill (see App.tsx, bottom-left).
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
        // No DialogDescription — opt out so Radix doesn't warn about a missing one.
        aria-describedby={undefined}
        onCloseAutoFocus={(e) => {
          // On a create-driven close, don't let Radix restore focus to the
          // trigger — the newly-selected tab's terminal takes focus on mount, so
          // the user can type into the agent immediately. Cancel/Esc keep the
          // default (focus returns to the trigger).
          if (createdRef.current) {
            createdRef.current = false
            e.preventDefault()
          }
        }}
      >
        <DialogHeader>
          <DialogTitle>New Agent</DialogTitle>
        </DialogHeader>

        {/* A real <form> lets the comboboxes keep their own Enter (item select)
            while the prompt <textarea> inserts a newline. The keydown handler
            adds Cmd/Ctrl+Enter as an always-submit, including from the prompt and
            comboboxes. */}
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
                    "flex-1 rounded-md border bg-background px-3 py-1.5 text-sm capitalize transition-all",
                    type === t
                      ? "border-primary bg-primary/15 text-primary shadow-elev-sm"
                      : "border-border text-muted-foreground shadow-well hover:bg-accent hover:text-foreground"
                  )}
                >
                  {t}
                </button>
              ))}
            </div>

            <Field label="Prompt" htmlFor="agent-prompt">
              <div className="flex flex-col gap-2">
                <div className="relative">
                  <textarea
                    ref={promptRef}
                    id="agent-prompt"
                    className={cn(
                      fieldClass,
                      "resize-none",
                      pastingImage && "opacity-50"
                    )}
                    rows={6}
                    value={prompt}
                    onChange={(e) => onPromptChange(e.target.value)}
                    onPaste={onPromptPaste}
                    disabled={pastingImage}
                    placeholder={PROMPT_PLACEHOLDERS[placeholderIdx]}
                    autoFocus
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
                onCheckedChange={(v) => setPlanMode(v === true)}
                // Checkboxes toggle on Space by ARIA convention; this form is
                // otherwise Enter-driven, so accept Enter to toggle too.
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault()
                    setPlanMode((v) => !v)
                  }
                }}
              />
              <label
                htmlFor="agent-plan-mode"
                className="cursor-pointer text-sm"
              >
                Start in plan mode
              </label>
              {/* Host picker — only when reachable remote hosts exist. The new
                  agent's worktree/session is created on the chosen host. */}
              {remoteHosts.length > 0 && (
                <select
                  id="agent-host"
                  aria-label="Host"
                  className={cn(fieldClass, "ml-auto w-auto py-1")}
                  value={host}
                  onChange={(e) => setHost(e.target.value)}
                >
                  <option value="local">
                    {hostsQuery.data?.hostname ?? "local"}
                  </option>
                  {remoteHosts.map((h) => (
                    <option key={h.alias} value={h.alias}>
                      {h.alias}
                    </option>
                  ))}
                </select>
              )}
            </div>

            {type === "git" && (
              <div className="grid grid-cols-2 gap-3">
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
              </div>
            )}

            {/* Advanced */}
            <button
              type="button"
              className="flex w-fit items-center gap-1 self-start rounded-md border border-border bg-background px-2 py-1 text-muted-foreground text-sm shadow-elev-sm transition-all hover:bg-accent hover:text-foreground"
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

          {/* Drop the shared footer's muted bar/border — it reads as a stray
              block once the form's content is short. */}
          <DialogFooter className="border-t-0 bg-transparent pt-0">
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
