import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Keyboard, Monitor, Moon, Palette, RotateCw, Sun } from "lucide-react"
import * as React from "react"
import { toast } from "sonner"
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
import { getMode, type Mode, setMode } from "@/lib/mode"
import { qk } from "@/lib/query"
import { SHORTCUTS } from "@/lib/shortcuts"
import { refreshTheme } from "@/lib/theme"
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

type SaveState = "idle" | "saving" | "saved" | "error"

// Debounced autosave: fires `save` `delay`ms after the watched fields last
// changed, but only while `dirty`. Returns `flush` to save immediately (on blur).
function useDebouncedSave(
  dirty: boolean,
  save: () => void,
  deps: React.DependencyList,
  delay = 600
) {
  const timer = React.useRef<ReturnType<typeof setTimeout> | undefined>(
    undefined
  )
  const saveRef = React.useRef(save)
  saveRef.current = save
  const dirtyRef = React.useRef(dirty)
  dirtyRef.current = dirty
  React.useEffect(() => {
    if (!dirty) return
    timer.current = setTimeout(() => saveRef.current(), delay)
    return () => clearTimeout(timer.current)
  }, [dirty, delay, ...deps])
  return React.useCallback(() => {
    clearTimeout(timer.current)
    if (dirtyRef.current) saveRef.current()
  }, [])
}

// Replaces the old explicit Save button: a quiet inline status that reflects the
// autosave lifecycle, with a retry affordance when a save fails.
function SaveStatus({
  state,
  onRetry,
}: {
  state: SaveState
  onRetry: () => void
}) {
  if (state === "idle") return null
  if (state === "saving")
    return <span className="text-[11px] text-muted-foreground">Saving…</span>
  if (state === "saved")
    return <span className="text-[11px] text-muted-foreground">Saved ✓</span>
  return (
    <span className="text-[11px] text-destructive">
      Couldn't save —{" "}
      <button
        type="button"
        className="underline hover:no-underline"
        onClick={onRetry}
      >
        retry
      </button>
    </span>
  )
}

// The Settings tab: lasso↔herdr socket-protocol compatibility (top) plus the
// "New Agent" creator configuration. Lasso targets a fixed protocol (baked in
// at build time); the daemon reports its own over the socket, and when they
// drift terminals and RPC silently break, so we surface it here. The creator
// defaults (where to scan for repos, the default agent, the scratch setup
// script) are global; each repo's files-to-copy + setup commands are scoped to
// the active host. All of it persists in ~/.lasso/lasso.db.
export function SettingsTab({
  active,
  onOpenShortcuts,
}: {
  active: boolean
  onOpenShortcuts: () => void
}) {
  const versionQuery = useQuery({
    queryKey: qk.version,
    queryFn: () => api.version(),
    enabled: active,
  })
  const info = versionQuery.data ?? null
  const loading = versionQuery.isLoading
  const errored = versionQuery.isError

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
      <Pill tone={info.compatible ? "good" : "bad"} multiline>
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
          <Pill multiline>
            targets protocol{" "}
            {loading ? "…" : errored || !info ? "unknown" : info.lasso_protocol}
          </Pill>
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
            title="Keyboard shortcuts"
            onClick={onOpenShortcuts}
          >
            <Keyboard />
          </Button>
          <Button
            variant="outline"
            size="icon"
            className="size-7"
            title="re-check protocol compatibility"
            onClick={() => versionQuery.refetch()}
          >
            <RotateCw />
          </Button>
        </div>
      </header>

      <div className="@container min-h-0 flex-1 overflow-y-auto px-3 py-4">
        <AppearanceToggle />
        <HerdrThemeSelect active={active} />
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
    </div>
  )
}

// AppearanceToggle picks the chrome theme: System (follow the OS), Light, Dark,
// or Herdr (match herdr's own theme — the default). The choice persists in
// localStorage and applies live: setMode sets the html dark/light class the
// Nothing --h-* tokens cascade from, then refreshTheme applies or clears the
// herdr --h-* override (see lib/theme.ts). The terminal palette is always
// herdr's and is unaffected by this control.
function AppearanceToggle() {
  const [mode, setModeState] = React.useState<Mode>(() => getMode())
  const choose = (m: Mode) => {
    setModeState(m)
    setMode(m)
    // Repaint the chrome for the new mode: applies herdr's palette when entering
    // "herdr", removes the override when leaving it.
    refreshTheme()
  }
  const opts: { m: Mode; label: string; Icon: typeof Monitor }[] = [
    { m: "herdr", label: "Herdr", Icon: Palette },
    { m: "system", label: "System", Icon: Monitor },
    { m: "light", label: "Light", Icon: Sun },
    { m: "dark", label: "Dark", Icon: Moon },
  ]
  return (
    <div className="mb-4 flex flex-col gap-1">
      <span className={labelClass}>Appearance</span>
      <div className="inline-flex w-fit gap-0.5 rounded-lg border border-border p-0.5">
        {opts.map(({ m, label, Icon }) => (
          <button
            key={m}
            type="button"
            onClick={() => choose(m)}
            className={cn(
              "inline-flex items-center gap-1.5 rounded-md px-2.5 py-1 text-[13px] transition-colors",
              mode === m
                ? "bg-primary text-primary-foreground"
                : "text-muted-foreground hover:text-foreground"
            )}
          >
            <Icon className="size-3.5" />
            {label}
          </button>
        ))}
      </div>
      <p className="text-[11px] text-muted-foreground">
        Sets the UI theme. Herdr matches herdr's own colors; System follows your
        OS; Light/Dark pin the Nothing palette. The terminal always keeps
        herdr's theme.
      </p>
    </div>
  )
}

// HerdrThemeSelect picks the herdr theme itself — distinct from Appearance,
// which only chooses whether the chrome follows it. Saving writes [theme].name
// in herdr's config.toml, the source of truth both already track: the herdr
// TUI reloads it, and lasso repaints the terminals (and, in Herdr mode, the
// chrome) off the theme_rev SSE bump. Local host only — the config lives on
// the machine running lasso.
function HerdrThemeSelect({ active }: { active: boolean }) {
  const { themeRev } = useApp()
  const themeQuery = useQuery({
    queryKey: qk.theme(themeRev),
    queryFn: () => api.theme(),
    enabled: active,
  })
  const t = themeQuery.data
  // Optimistic selection so the dropdown doesn't snap back while the config
  // write → theme_rev bump round-trips; cleared once the server agrees (or the
  // write fails).
  const [pending, setPending] = React.useState<string | null>(null)
  React.useEffect(() => {
    if (pending && t?.resolved === pending) setPending(null)
  }, [pending, t])
  const setMutation = useMutation({
    mutationFn: (name: string) => api.setTheme(name),
    onError: (e: Error) => {
      setPending(null)
      toast.error(`Couldn't set theme: ${e.message}`)
    },
  })
  const value = pending ?? t?.resolved ?? ""
  const group = (light: boolean) =>
    (t?.themes ?? [])
      .filter((o) => o.light === light)
      .map((o) => (
        <option key={o.name} value={o.name}>
          {o.label}
        </option>
      ))
  return (
    <div className="mb-4 flex flex-col gap-1">
      <label className={labelClass} htmlFor="settings-herdr-theme">
        Herdr theme
      </label>
      <select
        id="settings-herdr-theme"
        className={cn(fieldClass, "max-w-xs")}
        value={value}
        disabled={!t}
        onChange={(e) => {
          setPending(e.target.value)
          setMutation.mutate(e.target.value)
        }}
      >
        {!t?.themes.some((o) => o.name === value) && (
          <option value={value}>{value || "…"}</option>
        )}
        <optgroup label="Dark">{group(false)}</optgroup>
        <optgroup label="Light">{group(true)}</optgroup>
      </select>
      <p className="text-[11px] text-muted-foreground">
        Sets herdr's own theme in its config.toml; herdr and the terminals
        follow it live.
        {t?.forced &&
          " This lasso was launched with a -theme override, so its terminals won't follow until that flag is dropped."}
      </p>
    </div>
  )
}

// ShortcutsDialog shows the app's keyboard shortcuts (the SHORTCUTS the App key
// handler implements) in a modal. Reference only — nothing to configure.
// Rendered by App (so ⌘? can open it from any tab); the Settings tab's keyboard
// button just toggles the same App-owned state.
export function ShortcutsDialog({
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

  // Mirror the selected repo's saved copy-files/setup into the editors. Runs on
  // repo switch and on repos refetch (incl. after an autosave) — but it must NOT
  // clear the saved status, or a save's own refetch would wipe the "Saved ✓".
  React.useEffect(() => {
    const re = repos.find((r) => r.path === repoPath)
    setCopyFiles(re?.copy_files || "")
    setSetup(re?.setup || "")
  }, [repoPath, repos])

  // Clear the saved status only when the user actually switches repos.
  // biome-ignore lint/correctness/useExhaustiveDependencies: reset on repo switch only
  React.useEffect(() => {
    setSavedRepo(null)
  }, [repoPath])

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
    onError: () => toast.error("Couldn't save agent defaults"),
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
    onError: () => toast.error("Couldn't save repository setup"),
  })

  const dirtyDefaults =
    !!configQuery.data &&
    ((configQuery.data.repos_root || "") !== reposRoot ||
      (configQuery.data.default_agent ?? "") !== defaultAgent ||
      (configQuery.data.scratch_setup || "") !== scratchSetup)

  const dirtyRepo = (() => {
    const re = repos.find((r) => r.path === repoPath)
    return (re?.copy_files || "") !== copyFiles || (re?.setup || "") !== setup
  })()

  // Autosave: debounce edits, flush on blur. No explicit Save button.
  const flushDefaults = useDebouncedSave(
    dirtyDefaults,
    () => saveDefaultsMutation.mutate(),
    [reposRoot, defaultAgent, scratchSetup]
  )
  const flushRepo = useDebouncedSave(dirtyRepo, () => {
    if (repoPath) saveRepoMutation.mutate()
  }, [copyFiles, setup, repoPath])

  const defaultsStatus: SaveState = saveDefaultsMutation.isError
    ? "error"
    : dirtyDefaults
      ? "saving"
      : saveDefaultsMutation.isSuccess
        ? "saved"
        : "idle"
  const repoStatus: SaveState = saveRepoMutation.isError
    ? "error"
    : dirtyRepo
      ? "saving"
      : savedRepo === repoPath
        ? "saved"
        : "idle"

  return (
    <div className="flex @2xl:flex-row flex-col gap-4">
      <section className="flex min-w-0 flex-1 flex-col gap-3 rounded-lg border border-border p-4 shadow-sm">
        <div className="flex items-center justify-between gap-2">
          <h3 className="font-medium text-foreground text-sm">
            New Agent defaults
          </h3>
          <SaveStatus
            state={defaultsStatus}
            onRetry={() => saveDefaultsMutation.mutate()}
          />
        </div>

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
            onBlur={flushDefaults}
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
            onBlur={flushDefaults}
          >
            <option value="">Auto (use last used)</option>
            <option value="claude">Claude Code</option>
            <option value="codex">Codex</option>
            <option value="opencode">OpenCode</option>
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
            onBlur={flushDefaults}
            placeholder="uv venv"
          />
        </Field>
      </section>

      <section className="flex min-w-0 flex-1 flex-col gap-3 rounded-lg border border-border p-4 shadow-sm">
        <div className="flex items-start justify-between gap-2">
          <div className="flex flex-col gap-0.5">
            <h3 className="font-medium text-foreground text-sm">
              Per-repository setup
            </h3>
            <p className="text-[11px] text-muted-foreground">
              Files copied into a new worktree and commands run before the agent
              — both relative to the repo, applied to every agent created from
              it.
            </p>
          </div>
          {repoPath && (
            <SaveStatus
              state={repoStatus}
              onRetry={() => saveRepoMutation.mutate()}
            />
          )}
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
            onBlur={flushRepo}
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
            onBlur={flushRepo}
            placeholder="bun install"
            disabled={!repoPath}
          />
        </Field>
      </section>
    </div>
  )
}
