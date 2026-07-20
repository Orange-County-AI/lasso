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
import { EditableCombobox } from "@/components/ui/editable-combobox"
import { Input } from "@/components/ui/input"
import {
  api,
  type CreateAgentPayload,
  type HarnessDef,
  type HostInfo,
} from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { qk } from "@/lib/query"
import { focusHerdrTerminal } from "@/lib/terminal"
import { cn } from "@/lib/utils"

type AgentType = "git" | "scratch"

// Fallback registry while the config query is in flight (or against an older
// backend without `harnesses`); normally the list comes from the server's
// compiled-in harness table via /api/agent-config.
const FALLBACK_HARNESSES: HarnessDef[] = [
  {
    id: "claude",
    label: "Claude Code",
    supports_plan_mode: true,
    model_suggestions: [],
  },
  {
    id: "codex",
    label: "Codex",
    supports_plan_mode: false,
    model_suggestions: [],
  },
  {
    id: "opencode",
    label: "OpenCode",
    supports_plan_mode: true,
    model_suggestions: [],
  },
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

const imagePathRE = /\/[\w\-/.]+\.(?:png|jpe?g|gif|webp)/gi

// The prompt's first meaningful line acts as the title (branch/dir name,
// workspace label). Pasted-image paths are stripped first, so a prompt that
// opens with a pasted screenshot still titles by the text the user typed rather
// than the image's file path. Mirrors the backend's promptTitle.
function promptTitle(text: string): string {
  const cleaned = text.replace(imagePathRE, " ")
  for (const line of cleaned.split("\n")) {
    const t = line.trim()
    if (t) return t
  }
  return ""
}

// Native textarea/select styled to match the shadcn <Input> (same border,
// radius, and background) so every field in the form reads as one set. Fields
// use bg-background (not transparent) so they contrast against the dialog's
// bg-popover surface.
const fieldClass =
  "w-full rounded-lg border border-input bg-background px-2.5 py-1.5 text-sm shadow-well outline-none transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
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

function extractImagePaths(text: string): string[] {
  return [...new Set(text.match(imagePathRE) || [])]
}

// A host is selectable when it's reachable, running herdr, and protocol-
// compatible (mirror of HostSwitcher's helper). Unusable hosts are listed
// disabled — the footer switcher stays the place to provision/update them.
function hostUsable(h: HostInfo): boolean {
  return h.reachable && h.running && h.compatible
}

export function CreateAgentDialog({
  onCreated,
  variant = "button",
}: {
  onCreated?: () => void
  // "button" — the inline outline button on the Agents tab header.
  // "floating" — a pill matching the host switcher, for the bottom-left footer.
  // "header" — a compact button for the left column's tab-strip trailing slot.
  variant?: "button" | "floating" | "header"
}) {
  const [open, setOpen] = React.useState(false)
  const [showAdvanced, setShowAdvanced] = React.useState(false)
  const [placeholderIdx, setPlaceholderIdx] = React.useState(0)
  const queryClient = useQueryClient()
  // Set when the dialog closes because an agent was just created, so the close
  // handler hands keyboard focus to the herdr terminal instead of letting Radix
  // restore it to the trigger (which would force the user to click the pane).
  const createdRef = React.useRef(false)

  // Cmd+O opens the creator. Bound to the non-"button" variants (the
  // header / floating triggers) so the shortcut has a single owner even when the
  // Agents-tab button is also mounted.
  React.useEffect(() => {
    if (variant === "button") return
    const onKey = (e: KeyboardEvent) => {
      if (e.metaKey && !e.ctrlKey && e.key.toLowerCase() === "o") {
        e.preventDefault()
        setOpen(true)
      }
    }
    document.addEventListener("keydown", onKey)
    return () => document.removeEventListener("keydown", onKey)
  }, [variant])

  // Pick a fresh random Prompt placeholder each time the dialog opens — a little
  // personality on the blank canvas, without the distraction of it animating.
  React.useEffect(() => {
    if (!open) return
    setPlaceholderIdx(Math.floor(Math.random() * PROMPT_PLACEHOLDERS.length))
  }, [open])

  // The form targets a host the user picks (defaults to the active host). Each
  // host's config/repos live in its own lasso.db, so the queries are keyed and
  // fetched by selectedHost — picking another host previews that host's repos
  // (read from its db over SSH) WITHOUT switching the active backend. The switch
  // is deferred to create time so the Herdr tab isn't yanked while still editing.
  const { host: activeHost } = useApp()
  const [selectedHost, setSelectedHost] = React.useState("local")

  // Host list for the dropdown (local + ssh-config hosts), fetched while open.
  const hostsQuery = useQuery({
    queryKey: ["hosts"],
    queryFn: () => api.hosts(),
    enabled: open,
  })
  const localLabel = hostsQuery.data?.local?.hostname || "local"
  const remoteHosts = hostsQuery.data?.hosts ?? []

  // Server state via TanStack Query, fetched while the dialog is open, scoped to
  // selectedHost (each host's data comes from its own lasso.db / filesystem).
  const configQuery = useQuery({
    queryKey: qk.agentConfig(selectedHost),
    queryFn: () => api.agentConfig(selectedHost),
    enabled: open,
  })
  const reposQuery = useQuery({
    queryKey: qk.repos(selectedHost),
    queryFn: () => api.repos(selectedHost),
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
  const [model, setModel] = React.useState("")
  const [extraArgs, setExtraArgs] = React.useState("")
  const [pastingImage, setPastingImage] = React.useState(false)
  const [planMode, setPlanMode] = React.useState(false)
  const [files, setFiles] = React.useState<File[]>([])
  // Screenshots pasted into the Prompt are written to a host *immediately* (so a
  // path can be inserted), but the agent runs on the host chosen at create time —
  // and the Host dropdown sits below the Prompt, so the common flow is to paste
  // first and pick the host after. We keep each pasted blob and the host it
  // currently lives on so we can re-home it to the selected host before creating
  // (see rehomePastedImages). Keyed by the path currently in the prompt.
  const [pastedImages, setPastedImages] = React.useState<
    { path: string; blob: Blob; host: string }[]
  >([])
  const promptRef = React.useRef<HTMLTextAreaElement>(null)

  // The launchable-harness registry (compiled into the backend) drives the
  // AI-agent dropdown, plan-mode visibility, and model suggestions.
  const harnesses = config?.harnesses?.length
    ? config.harnesses
    : FALLBACK_HARNESSES
  const harness =
    harnesses.find((h) => h.id === agent) ??
    harnesses[0] ??
    FALLBACK_HARNESSES[0]

  const branchesQuery = useQuery({
    queryKey: qk.repoBranches(selectedHost, repo),
    queryFn: () => api.repoBranches(repo, selectedHost),
    enabled: open && type === "git" && !!repo,
  })
  const branches = React.useMemo(() => {
    const b = branchesQuery.data
    // branches/remoteBranches can be null (Go nil slice → JSON null) when the
    // repo path doesn't resolve on the host — e.g. transiently after switching
    // the host dropdown while `repo` is still the previous host's path.
    return b ? [...(b.branches ?? []), ...(b.remoteBranches ?? [])] : []
  }, [branchesQuery.data])

  // Default the form's host to the active host each time the dialog opens (the
  // dropdown is otherwise a free local selection that previews repos per host).
  // biome-ignore lint/correctness/useExhaustiveDependencies: seed from activeHost only on open, not when it later changes
  React.useEffect(() => {
    if (open && activeHost) setSelectedHost(activeHost)
  }, [open])

  // Seed the form from the remembered selections once per host per open (not on
  // every data change, so it never clobbers in-progress edits): the last agent
  // type, branch prefix, AI agent (default_agent, else last_agent, else claude),
  // and the last repo if it still exists on that host. Re-keyed on selectedHost
  // so switching the dropdown re-seeds from that host's state — gated on the
  // repos query having settled so it doesn't seed from the previous host's stale
  // list while the refetch is still in flight.
  const seededForHost = React.useRef<string | null>(null)
  React.useEffect(() => {
    if (!open) {
      seededForHost.current = null
      return
    }
    if (!config || !reposQuery.isSuccess || reposQuery.isFetching) return
    if (seededForHost.current === selectedHost) return
    seededForHost.current = selectedHost
    setType(config.last_agent_type || "git")
    setPrefix(config.branch_prefix || "")
    const seededAgent = config.default_agent || config.last_agent || "claude"
    setAgent(seededAgent)
    // Default the model to whatever the harness's CLI is itself configured with
    // (Claude Code's configured model). When the CLI has no model pinned in any
    // of its config files the field stays blank — the placeholder "default" then
    // reads as "let the CLI pick", which is exactly what launching with no
    // --model does.
    const seededDef = config.harnesses?.find((h) => h.id === seededAgent)
    setModel(seededDef?.default_model || "")
    setExtraArgs("")
    const last = config.last_repo
    setRepo(
      last && repos.some((r) => r.path === last) ? last : (repos[0]?.path ?? "")
    )
  }, [
    open,
    selectedHost,
    config,
    reposQuery.isSuccess,
    reposQuery.isFetching,
    repos,
  ])

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
    setAutoBranch(generateBranchName(promptTitle(v)))
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
      const { path } = await api.pasteImage(file, selectedHost)
      // Remember the blob + where it landed so we can re-home it if the user
      // picks a different host before creating.
      setPastedImages((prev) => [
        ...prev,
        { path, blob: file, host: selectedHost },
      ])
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

  // Re-upload any pasted screenshots that don't already live on `host` (the user
  // changed the Host dropdown after pasting) so the file is on the host the agent
  // will actually run on, and rewrite its path in `text`. Returns the updated
  // text. Best-effort per image: a re-upload failure leaves that path untouched
  // rather than blocking agent creation.
  const rehomePastedImages = React.useCallback(
    async (text: string, host: string): Promise<string> => {
      const stale = pastedImages.filter(
        (im) => im.host !== host && text.includes(im.path)
      )
      if (stale.length === 0) return text
      let next = text
      const moved: { path: string; blob: Blob; host: string }[] = []
      for (const im of stale) {
        try {
          const { path: newPath } = await api.pasteImage(im.blob, host)
          next = next.split(im.path).join(newPath)
          moved.push({ path: newPath, blob: im.blob, host })
        } catch (err) {
          toast.error("Failed to move pasted image to host", {
            description: err instanceof Error ? err.message : String(err),
          })
        }
      }
      if (moved.length > 0) {
        setPastedImages((prev) =>
          prev.map(
            (im) =>
              moved.find((m) => m.blob === im.blob && m.host === host) ?? im
          )
        )
      }
      return next
    },
    [pastedImages]
  )

  const reset = () => {
    setPrompt("")
    setPastingImage(false)
    setBranchName("")
    setAutoBranch("")
    setFiles([])
    setPastedImages([])
    setPlanMode(false)
    setExtraArgs("")
    setShowAdvanced(false)
  }

  const createMutation = useMutation({
    mutationFn: async () => {
      // Move any screenshots pasted before the host was picked onto the selected
      // host, rewriting their paths in the prompt we submit, so the agent on that
      // host can actually read them. No-op when they're already there.
      const finalPrompt = await rehomePastedImages(prompt, selectedHost)
      // Commit the host choice now: switch the active backend so the agent is
      // created on it, attachments land there, and the terminal points at it for
      // focus. Deferred to here (not on dropdown change) so previewing a host's
      // repos while editing doesn't yank the Herdr tab onto another host.
      if (selectedHost !== (activeHost ?? "local")) {
        await api.switchHost(selectedHost)
      }
      let uploadDir: string | undefined
      let attachments: string[] | undefined
      if (files.length > 0) {
        const up = await api.uploadAgentFiles(files, selectedHost)
        uploadDir = up.upload_dir
        attachments = up.files
      }
      const payload: CreateAgentPayload = {
        type,
        prompt: finalPrompt.trim(),
        agent,
        model: model.trim() || undefined,
        extra_args: extraArgs.trim() || undefined,
        // The checkbox is hidden for harnesses without a plan mode, but its
        // state survives harness switches — gate it so a codex agent never
        // records plan_mode.
        plan_mode: planMode && harness.supports_plan_mode,
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
      // so refetch them (prefix-match clears every host's cached config).
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
  // Preview a pasted image from the host it lives on; manually-typed paths fall
  // back to the selected host (where the agent — and presumably the path — is).
  const hostForImage = (p: string) =>
    pastedImages.find((im) => im.path === p)?.host ?? selectedHost

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        {variant === "header" ? (
          // Prominent (but unfilled) button for the left column's tab-strip
          // trailing slot — the accent carried by the border + text, with only a
          // faint tint, so it reads as the row's primary action without a solid fill.
          <button
            type="button"
            className="my-1 flex shrink-0 items-center gap-1 self-center rounded-md border border-primary/60 bg-primary/10 px-2.5 py-1 font-medium text-primary text-xs transition-colors hover:border-primary hover:bg-primary/20"
            title="create a new agent (⌘O)"
          >
            <Plus className="size-3.5" />
            <span>New Agent</span>
          </button>
        ) : variant === "floating" ? (
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
        // No DialogDescription — opt out so Radix doesn't warn about a missing one.
        aria-describedby={undefined}
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
                      : "border-border text-muted-foreground shadow-well hover:border-primary/50 hover:bg-primary/10 hover:text-foreground"
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
                        href={api.fileURL(path, hostForImage(path))}
                        target="_blank"
                        rel="noreferrer"
                        className="block rounded border border-border bg-card p-1 transition-opacity hover:opacity-80"
                      >
                        <img
                          src={api.fileURL(path, hostForImage(path))}
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
              {/* Plan mode only exists on harnesses that support it (claude);
                  hide the checkbox elsewhere rather than offering a no-op. */}
              {harness.supports_plan_mode && (
                <>
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
                </>
              )}
              <div className="ml-auto flex items-center gap-1.5">
                <label htmlFor="agent-host" className={labelClass}>
                  Host
                </label>
                <select
                  id="agent-host"
                  className={cn(fieldClass, "w-auto py-1")}
                  value={selectedHost}
                  onChange={(e) => setSelectedHost(e.target.value)}
                >
                  <option value="local">{localLabel}</option>
                  {remoteHosts.map((h) => (
                    <option
                      key={h.alias}
                      value={h.alias}
                      disabled={!hostUsable(h)}
                    >
                      {hostUsable(h) ? h.alias : `${h.alias} (unavailable)`}
                    </option>
                  ))}
                </select>
              </div>
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
                <div className="grid grid-cols-2 gap-3">
                  <Field label="AI agent" htmlFor="agent-agent">
                    <select
                      id="agent-agent"
                      className={fieldClass}
                      value={agent}
                      onChange={(e) => {
                        const next = e.target.value
                        setAgent(next)
                        // Swap in the newly-selected harness's configured default
                        // model (e.g. Claude Code's), or blank when its CLI has no
                        // model pinned in its config.
                        const nextDef = harnesses.find((h) => h.id === next)
                        setModel(nextDef?.default_model || "")
                      }}
                    >
                      {harnesses.map((h) => (
                        <option key={h.id} value={h.id}>
                          {h.label}
                        </option>
                      ))}
                    </select>
                  </Field>
                  <Field label="Model" htmlFor="agent-model">
                    {/* Free text + suggestions: model names churn faster than
                        releases, so the list is a hint, not a constraint. The
                        editable combobox always shows every suggestion on open
                        (unlike a native datalist, which hides them once the
                        field holds a complete value). */}
                    <EditableCombobox
                      id="agent-model"
                      value={model}
                      onValueChange={setModel}
                      suggestions={harness.model_suggestions ?? []}
                      placeholder="default"
                    />
                  </Field>
                </div>
                <Field label="Extra CLI args" htmlFor="agent-extra-args">
                  <Input
                    id="agent-extra-args"
                    className="bg-background font-mono dark:bg-background"
                    value={extraArgs}
                    onChange={(e) => setExtraArgs(e.target.value)}
                    placeholder="appended to the launch command"
                  />
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
