// Typed wrappers around lasso's Go HTTP API. Every endpoint the original
// index.html called via fetch() lives here, so components never build URLs by
// hand. Paths are same-origin (the Go server, or Vite's dev proxy onto it).
//
// Every request goes through hostFetch, which attaches THIS tab's host
// (lib/host.ts). That is what lets two tabs sit on two machines: the server
// holds no active host any more, so a handler learns which one to run against
// from the request itself.

import { hostFetch } from "./host"

export interface ActiveState {
  cwd?: string
  pane_id?: string
  panes_rev?: number
  theme_rev?: number
  // The host this stream is for ("local" or an ssh-config alias) and its URL
  // path segment, which addresses that host's terminals at /terminal/<slug>/.
  // The slug is served rather than derived here because the server disambiguates
  // aliases that sanitize to the same string.
  host?: string
  host_slug?: string
  // Bumps whenever the persisted UI prefs change (any tab saving /api/ui-state)
  // so every open tab refetches and converges.
  ui_state_rev?: number
  // The host `cwd` lives on — normally the active host, but the focused pane
  // may be an ssh window onto another host's herdr, in which case this is that
  // host and file/diff requests must address it explicitly.
  cwd_host?: string
}

// One ssh-config host as a herdr target. Selectable in the footer switcher only
// when reachable && running && compatible; otherwise greyed out with `err`.
// A host row whose probe hasn't produced a verdict. Absent `state` means the
// probe completed and reachable/running/compatible are authoritative;
// "probing" means one is still in flight and "timeout" means it ran out of
// budget. Neither says the host is down — rendering them as failures is how a
// healthy-but-slow machine used to get dropped from the switcher.
export type HostState = "probing" | "timeout"

export interface HostInfo {
  alias: string
  // Effective ssh HostName / User the alias resolves to. hostname lets the UI
  // group aliases that point at one physical box (and fold loopback aliases
  // under the local host); user distinguishes multiple accounts on that box.
  hostname: string
  user: string
  reachable: boolean
  running: boolean
  version: string
  protocol: number
  socket: string
  compatible: boolean
  err?: string
  state?: HostState
  // RFC3339 timestamp of the last COMPLETED probe; absent until one finishes.
  checked_at?: string
}

export interface HostsPayload {
  active: string
  local: { version: string; protocol: number; hostname: string; user: string }
  hosts: HostInfo[]
  // True while at least one host is still being probed — the cue to poll again
  // shortly, since the list is a partial answer that will fill in.
  probing: boolean
}

// One usage quota window (5-hour block, weekly rolling, …). `percent` is 0–100.
// `resetsAt` is RFC3339 (the frontend formats it relative to now so countdowns
// stay live between polls). `countdown` marks short windows shown as "18m left"
// vs a reset date. `elapsedPct` is 0–100 for how far through the window we are
// (the pace notch on the usage bar), or -1 if the window length is unknown.
export interface UsageLimit {
  label: string
  percent: number
  resetsAt?: string
  countdown?: boolean
  elapsedPct: number
}

export interface UsageProvider {
  name: string
  plan?: string
  limits: UsageLimit[]
  err?: string
}

export interface UsagePayload {
  providers: UsageProvider[]
  updatedAt: string
}

// Providers the backend knows how to meter. Kept here so the footer and its
// Settings controls share the persisted provider names exactly.
export const USAGE_PROVIDER_NAMES = [
  "Claude Code",
  "Kimi Code",
  "Codex",
  "Z.ai",
] as const

// Normalize persisted order without hiding providers introduced by a newer
// build. Unknown and duplicate names are dropped; known missing names append in
// backend order.
export function completeUsageProviderOrder(
  saved: readonly string[] | undefined
): string[] {
  const known: readonly string[] = USAGE_PROVIDER_NAMES
  const completed = (saved ?? []).filter(
    (name, index, order) =>
      known.includes(name) && order.indexOf(name) === index
  )
  for (const name of known) {
    if (!completed.includes(name)) completed.push(name)
  }
  return completed
}

export interface Pane {
  pane_id: string
  workspace_id?: string
  workspace_label?: string
  tab_id?: string
  tab_label?: string
  cwd?: string
  focused?: boolean
  agent?: string
  agent_status?: string
}

// One herdr pane on a specific host, enriched with workspace/tab labels and
// whether herdr detects an agent in it — the row shape of /api/all-panes and
// /api/agent-history. `host` is "local" or an ssh-config alias and is the key
// for focusing the pane (switching the active host first when it isn't already
// active).
export interface HostPane {
  host: string
  host_label: string
  pane_id: string
  workspace_id?: string
  workspace_label?: string
  tab_id?: string
  tab_label?: string
  pane_label?: string
  // The pane's OSC title with the agent's state glyphs stripped — for an agent
  // pane, what it is currently working on. The only name a session whose
  // workspace was never labelled has.
  terminal_title?: string
  cwd?: string
  agent?: string
  agent_status?: string
  has_agent?: boolean
  focused?: boolean
  // The agent's initial prompt (creation description). Carried for search only —
  // the pane switcher matches against it but doesn't display the full text.
  prompt?: string
  // Set only on rows from /api/agent-history (past agents). agent_id identifies the
  // record for reopenAgent; closed is derived client-side (its pane is no longer
  // live) so the switcher renders it distinctly and reopens rather than focuses.
  agent_id?: string
  closed?: boolean
  // Set when this pane is a herdr-mirror stream of another machine's pane (the
  // plugin turns each mirrored remote workspace into a real LOCAL workspace, so
  // `host` still says "local"). mirror_host is the herdr-mirror host key,
  // mirror_label the workspace's label as it reads over there — the mirror's
  // sidebar label minus its "<host>: " prefix — and mirror_workspace /
  // mirror_pane the remote herdr's own ids. See src/mirror.go.
  mirror_host?: string
  mirror_label?: string
  mirror_workspace?: string
  mirror_pane?: string
}

export interface PanesPayload {
  panes: HostPane[]
  // host → why its panes couldn't be listed (unreachable, protocol drift, …).
  // Every other host's panes still come back; the UI reports these separately.
  errors?: Record<string, string>
}

// Persisted, global browser UI preferences (SQLite-backed): sidebar layout, the
// Files tab's click behavior, and footer preferences. The client reads the whole
// object and writes patches, so navigating away and back — or opening lasso
// elsewhere — restores the same view.
export interface UIState {
  sidebar_collapsed: boolean
  // The sidebar's open width (% of the panel group). Synced because the
  // sidebar's footprint sets the shared herdr pty's width. 0 = never set.
  sidebar_pct: number
  // Files tab folder-click behavior: true re-roots the tree into the folder,
  // false expands it in place. Defaults true (see getUIState in db.go).
  files_click_navigates: boolean
  // Provider names omitted from the bottom usage footer. Empty = show all.
  usage_hidden: string[]
  // Preferred provider order; providers absent here append automatically.
  usage_order: string[]
  // Use abbreviated provider names and metrics without pace bars.
  usage_compact: boolean
}

export interface FileEntry {
  name: string
  dir: boolean
  size?: number
}

export interface DirListing {
  path: string
  parent?: string
  entries: FileEntry[]
}

// One changed file in the diff metadata. The line-by-line diff is fetched
// lazily per file (api.diffFile) when the user expands it.
export interface DiffFileMeta {
  path: string
  status: string
  staged?: boolean
  add: number
  del: number
}

export interface DiffPayload {
  branch?: string
  baseBranch?: string
  isBranchDiff?: boolean
  dirty?: number
  files: DiffFileMeta[]
  // False when the active pane's cwd is not a git repo (a plain directory, or a
  // scratch agent's workdir). Not an error — the diff view just has nothing to
  // show, so the backend answers 200 with this flag rather than a 502 the client
  // would retry forever.
  isRepo: boolean
}

export interface FileDiff {
  diff: string
  truncated: boolean
}

// Protocol-compatibility check for the Settings tab: the herdr socket protocol
// this lasso build targets vs. the protocol the installed herdr daemon reports
// over its socket. `err` is set (and herdr_protocol is 0) when the daemon can't
// be reached, so the tab shows "herdr unreachable" instead of a false mismatch.
export interface VersionInfo {
  lasso_protocol: number
  // This lasso build's own version (git revision from the Go VCS stamp, or
  // "dev"). Shown in the host switcher so a stale install is visible.
  lasso_version: string
  herdr_protocol: number
  herdr_version?: string
  compatible: boolean
  // Whether this install can self-update (a systemd-supervised git checkout).
  // False for dev/worktree runs, where the "Update lasso" action is hidden.
  updatable: boolean
  // Only meaningful when `updatable`: whether the running build is behind main.
  // "available" — a newer commit is waiting to be built (see commits_behind);
  // "current" — already on main's tip; "unknown" — can't tell, so the UI still
  // offers the button. Absent on non-updatable installs.
  update_state?: "available" | "current" | "unknown"
  commits_behind?: number
  // The newest published GitHub release tag — set only for a release-binary
  // install (not the supervised checkout). When newer than lasso_version, the
  // Settings tab shows an "update available" hint pointing at `lasso update`.
  latest_version?: string
  err?: string
}

// One selectable built-in theme (the server's canonical list, in display
// order: dark schemes first, then light variants).
export interface ThemeOption {
  name: string
  label: string
  light: boolean
}

export interface ThemePayload {
  name: string
  resolved: string
  customized: boolean
  css: string
  // xterm.js ITheme — shape is opaque to us; we hand it straight to the iframe.
  xterm: Record<string, unknown>
  themes: ThemeOption[]
  // True when lasso was launched with a -theme override, so writing herdr's
  // config restyles herdr but this lasso instance won't follow.
  forced: boolean
  // Whether lasso mirrors the theme into agent CLIs' theme files (opencode,
  // Claude Code, omp) — the "Sync agent themes" toggle.
  sync_agent_themes: boolean
  // Hosts ("local" or ssh aliases) lasso writes no theme to at all — neither
  // herdr's config.toml nor any agent theme file. Everything else syncs.
  theme_sync_off: string[]
}

// httpError builds a concise Error from a non-OK response. lasso/herdr return
// short text or JSON errors, but a proxy in front of the app (e.g. the Cloudflare
// tunnel exposing lasso.knowsuchagency.ai) answers with a full HTML error page
// when the origin is down or briefly unreachable — during a host switch, a
// redeploy, etc. Dumping that raw HTML into the UI (the Diff tab, toasts) is just
// noise, so collapse HTML bodies (and empty ones) to the status line.
// ApiError carries the HTTP status alongside the message so callers can tell a
// gateway-style transient failure (502/503/504 — e.g. lasso restarting under
// `lasso update`) from a real rejection, and retry only the former.
export class ApiError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.status = status
  }
}

async function httpError(r: Response): Promise<Error> {
  const body = (await r.text().catch(() => "")).trim()
  const isHTML =
    /^<(?:!doctype|html|head|body)\b/i.test(body) ||
    (r.headers.get("content-type") || "").includes("text/html")
  if (!body || isHTML) {
    return new ApiError(
      `HTTP ${r.status}${r.statusText ? ` ${r.statusText}` : ""}`,
      r.status
    )
  }
  return new ApiError(
    body.length > 300 ? `${body.slice(0, 300)}…` : body,
    r.status
  )
}

// ---------------------------------------------------------------------------
// Agent creation ("New Agent")
// ---------------------------------------------------------------------------

// Per-repo remembered creator state (lives in ~/.lasso/lasso.db, keyed by the
// active host + repo path).
export interface RepoConfig {
  last_base_branch?: string
  copy_files?: string
  setup?: string
}

// One agent lasso has spawned.
export interface AgentRecord {
  id: string
  title: string
  type: "git" | "scratch"
  repo?: string
  base_branch?: string
  branch?: string
  agent: string
  model?: string
  effort?: string
  extra_args?: string
  description?: string
  notes?: string
  attachments?: string[]
  plan_mode: boolean
  work_dir: string
  workspace_id?: string
  root_pane?: string
  created_at: string
}

// The creator's settings + the active host's remembered selections + agent log
// (GET/POST /api/agent-config). `default_agent` may be "" — no preset default,
// in which case the creator falls back to `last_agent`. `last_repo`,
// `last_agent`, `last_agent_type`, `repos`, and `agents` are scoped to the
// active host.
export interface AgentConfig {
  repos_root: string
  branch_prefix: string
  default_agent: string
  last_repo?: string
  last_agent?: string
  // The server's compiled-in agent registry — drives the creator's AI-agent
  // dropdown, plan-mode visibility, effort levels, and model suggestions.
  harnesses?: HarnessDef[]
  last_agent_type?: "git" | "scratch"
  scratch_setup?: string
  repos?: Record<string, RepoConfig>
  agents?: AgentRecord[]
}

// One launchable agent CLI, as served by the backend's harness registry.
export interface HarnessDef {
  id: string
  label: string
  supports_plan_mode: boolean
  // Thinking/reasoning-effort levels this harness's CLI accepts, cheapest
  // first. Absent/empty = no effort knob, so the creator hides the select.
  effort_levels?: string[] | null
  model_suggestions: string[] | null
}

// One git repo discovered under repos_root, with its remembered per-repo state.
export interface RepoEntry {
  path: string
  name: string
  copy_files: string
  setup: string
  last_base_branch: string
}

export interface RepoBranches {
  branches: string[]
  remoteBranches: string[]
  default: string
}

// The body POSTed to /api/create-agent.
export interface CreateAgentPayload {
  // Host to create on ("local" or an ssh-config alias); omit for the active
  // host. Sent so the create targets the picked host's backend directly instead
  // of depending on the UI's active host having been switched there first.
  host?: string
  type: "git" | "scratch"
  // The agent's instruction; its first line becomes the title (branch/dir name,
  // workspace label, list/toast headline).
  prompt: string
  repo?: string
  base_branch?: string
  branch_prefix?: string
  branch_name?: string
  agent: string
  // Model for the agent's CLI (its --model flag); omit for the harness default.
  model?: string
  // Thinking effort level, one of the harness's effort_levels; omit for the
  // CLI's own default. The server drops anything the harness doesn't list.
  effort?: string
  // Free-form CLI flags appended verbatim to the launch command.
  extra_args?: string
  notes?: string
  plan_mode: boolean
  attachments?: string[]
  upload_dir?: string
}

async function getJSON<T>(url: string, timeoutMs?: number): Promise<T> {
  let r: Response
  try {
    r = await hostFetch(
      url,
      timeoutMs ? { signal: AbortSignal.timeout(timeoutMs) } : undefined
    )
  } catch (e) {
    // A request that never lands must surface as an error, not as a spinner the
    // user stares at forever — see aggregateTimeout's callers.
    if (e instanceof DOMException && e.name === "TimeoutError") {
      throw new Error(`${url} timed out after ${(timeoutMs ?? 0) / 1000}s`)
    }
    throw e
  }
  if (!r.ok) throw await httpError(r)
  return (await r.json()) as T
}

// aggregateTimeout caps the two cross-host aggregations (every host's panes and
// the agent history). They fan out over ssh, so they are the slowest reads in
// the app and the ones with the most ways to stall; without a client bound, a
// backend that stops answering leaves the ⌘K palette on "Loading…" indefinitely
// with nothing to retry and nothing to report.
const aggregateTimeout = 30_000

async function postJSON<T>(url: string, body: unknown): Promise<T> {
  const r = await hostFetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  })
  if (!r.ok) throw await httpError(r)
  return (await r.json()) as T
}

// withHost appends ?host=/&host= to a config endpoint so it targets a specific
// host's own settings (its lasso.db). Omitted = this tab's own host.
function withHost(url: string, host?: string): string {
  if (!host) return url
  return `${url}${url.includes("?") ? "&" : "?"}host=${encodeURIComponent(host)}`
}

// The host attach currently in flight (if any) — see api.attachHost.
let hostAttach: {
  host: string
  promise: Promise<{ active: string; version: string; protocol: number }>
} | null = null

export const api = {
  active: () => getJSON<ActiveState>("/api/active"),
  theme: () => getJSON<ThemePayload>("/api/theme"),
  // Writes [theme].name in herdr's config.toml (the shared source of truth) —
  // herdr reloads it and lasso follows via the theme_rev SSE bump.
  setTheme: (name: string) =>
    postJSON<{ ok: boolean; name: string }>("/api/theme-set", { name }),
  // Flips the server-level "sync agent themes" toggle (no theme change).
  setSyncAgentThemes: (enabled: boolean) =>
    postJSON<{ ok: boolean; sync_agent_themes: boolean }>("/api/theme-set", {
      sync_agent_themes: enabled,
    }),
  // Switches every theme write lasso makes to ONE host on or off ("local" or an
  // ssh alias): herdr's config.toml there plus its agent theme files. Turning it
  // back on pushes the current theme to that host right away when it's reachable.
  setHostThemeSync: (host: string, enabled: boolean) =>
    postJSON<{ ok: boolean; theme_sync_off: string[] }>("/api/theme-set", {
      theme_sync_host: host,
      theme_sync: enabled,
    }),

  // Whether new agents are re-titled from their prompt by a local agent CLI
  // (autotitle.go). A server-level setting of the box lasso runs on — the CLI
  // runs there, not on the host the agent was created on — so unlike the
  // creator defaults it isn't host-scoped.
  autoTitle: () => getJSON<{ enabled: boolean }>("/api/auto-title"),
  setAutoTitle: (enabled: boolean) =>
    postJSON<{ enabled: boolean }>("/api/auto-title", { enabled }),

  // The ssh-config hosts probed for a compatible herdr server. ?refresh=1 skips
  // the server-side cache (the footer's manual refresh).
  hosts: (refresh = false) =>
    getJSON<HostsPayload>(`/api/hosts${refresh ? "?refresh=1" : ""}`),

  // Attach THIS tab to a host ("local" or an alias): the server resolves and
  // pools its connection and makes sure its terminals are spawned, then reports
  // the herdr version/protocol to expect. It mutates nothing shared — the tab
  // records its own choice (setTabHost) and sends it on every later request —
  // so a second tab on another machine is unaffected.
  //
  // Client-side, attaches are still coalesced: a same-host request while one is
  // in flight shares its promise, and a different-host request queues behind it.
  // A remote host's first attach spawns two ttyds and can take a beat, and focus
  // paths judge "already there?" from SSE state that lags it — so without this,
  // clicking into a cell mid-attach fired a duplicate.
  attachHost: (host: string) => {
    if (hostAttach?.host === host) return hostAttach.promise
    const prev = hostAttach?.promise.catch(() => {}) ?? Promise.resolve()
    const promise = prev.then(() =>
      postJSON<{ active: string; version: string; protocol: number }>(
        "/api/host",
        { host }
      )
    )
    const entry = { host, promise }
    hostAttach = entry
    const clear = () => {
      if (hostAttach === entry) hostAttach = null
    }
    promise.then(clear, clear)
    return promise
  },

  // Run `herdr update` on a remote host that's behind this lasso's protocol,
  // auto-answering its interactive prompts (stop the old server = yes, which
  // exits that host's pane processes; decline the star prompt = no). Slow — it
  // downloads a release binary on the far side — and returns the captured output.
  updateHost: (host: string) =>
    postJSON<{ ok: boolean; output: string; error?: string }>(
      "/api/host-update",
      { host }
    ),

  // Install herdr on a remote host (if missing) and bring it up supervised by
  // systemd --user (also installing herdr's agent-state integrations). For hosts where herdr
  // is missing or its server isn't running. Slow — downloads binaries — and
  // returns a provisioning log.
  provisionHost: (host: string) =>
    postJSON<{ ok: boolean; output: string; error?: string }>(
      "/api/host-provision",
      { host }
    ),

  // Update lasso itself: pull the latest source and let the supervisor rebuild +
  // restart it. Only works on the systemd-supervised prod install (see
  // VersionInfo.updatable); the server bounces a moment after this returns.
  selfUpdate: () =>
    postJSON<{ started: boolean; src: string; unit: string }>(
      "/api/self-update",
      {}
    ),

  panes: () => getJSON<{ panes?: Pane[] }>("/api/panes"),

  // Every herdr pane across every reachable, protocol-compatible host (local +
  // remotes), for the ⌘K pane switcher. Aggregated server-side; per-host
  // failures come back in `errors` rather than failing the whole request.
  allPanes: () => getJSON<PanesPayload>("/api/all-panes", aggregateTimeout),

  // Every agent lasso ever spawned (across hosts), shaped as HostPane rows so the
  // ⌘K switcher can list past agents next to live panes. AgentID is set; the
  // switcher treats a row whose host+pane_id isn't currently live as "closed" and
  // reopens it via reopenAgent on select.
  agentHistory: () =>
    getJSON<{ agents: HostPane[] }>("/api/agent-history", aggregateTimeout),

  // Re-open a past session's workspace: re-creates a herdr workspace at its work
  // dir and focuses it unless focus:false (does NOT relaunch the agent). Identify
  // it by agent_id (a recorded agent — also re-points its record at the new pane)
  // or by work_dir (an orphan worktree/scratch dir with no record). Returns the
  // new pane so the caller can focus it through the normal pane-focus path.
  reopenAgent: (
    host: string,
    body: { agent_id?: string; work_dir?: string; focus?: boolean }
  ) => postJSON<HostPane>("/api/agent/reopen", { host, ...body }),

  // Persisted UI preferences (sidebar layout, Files tab, and usage footer).
  uiState: () => getJSON<UIState>("/api/ui-state"),
  // Patch semantics: send only the changed fields; the server merges into the
  // stored state (so stale tabs can't clobber fields they didn't touch) and
  // returns the merged whole.
  saveUIState: (patch: Partial<UIState>) =>
    postJSON<UIState>("/api/ui-state", patch),
  version: () => getJSON<VersionInfo>("/api/version"),
  // Subscription usage limits (Claude Code / Kimi Code / Codex / Z.ai),
  // rendered in the bottom UsageFooter.
  usage: () => getJSON<UsagePayload>("/api/usage"),

  // List a directory. `host` (omitted = the active backend) is the host the
  // path lives on — the sidebar browses the focused pane's host, which can
  // differ from the active one when the pane is an ssh window.
  files: (path: string, host?: string) =>
    getJSON<DirListing>(
      withHost(`/api/files?path=${encodeURIComponent(path)}`, host)
    ),

  // Optionally pass a host to read the file from that host (?host=); omitted =
  // the active backend. Used for previewing a file that lives on another host
  // (e.g. a screenshot pasted onto the host an agent will run on).
  fileURL: (path: string, host?: string) =>
    withHost(`/api/file?path=${encodeURIComponent(path)}`, host),

  // A URL that forces a browser download (Content-Disposition: attachment) and
  // skips the preview size cap. `host` targets the machine the path lives on.
  downloadURL: (path: string, host?: string) =>
    withHost(`/api/file?path=${encodeURIComponent(path)}&download=1`, host),

  // Upload one or more files into an existing directory on `host` (omitted =
  // the active backend). Filenames are kept (basename only) — the server drops
  // them into `dir`.
  uploadFiles: async (
    dir: string,
    files: File[],
    host?: string
  ): Promise<{ ok: boolean; files: string[] }> => {
    const form = new FormData()
    form.append("dir", dir)
    if (host) form.append("host", host)
    for (const f of files) form.append("files", f, f.name)
    const r = await hostFetch("/api/file-upload", { method: "POST", body: form })
    if (!r.ok) throw await httpError(r)
    return r.json()
  },

  fileText: async (path: string, host?: string) => {
    const r = await hostFetch(api.fileURL(path, host))
    if (!r.ok) throw await httpError(r)
    return r.text()
  },

  // A cheap change signature (Last-Modified + size) fetched via HEAD — no body
  // download — so a binary preview can poll for on-disk changes and only reload
  // when the file actually changed. Returns null on any failure (the caller
  // treats that as "no change observed").
  fileSig: async (path: string, host?: string): Promise<string | null> => {
    try {
      const r = await hostFetch(api.fileURL(path, host), { method: "HEAD" })
      if (!r.ok) return null
      const lm = r.headers.get("last-modified") ?? ""
      const len = r.headers.get("content-length") ?? ""
      return `${lm}:${len}`
    } catch {
      return null
    }
  },

  // Overwrite an existing file on `host` (omitted = the active backend) with
  // new content (preserving its mode).
  writeFile: (path: string, content: string, host?: string) =>
    postJSON<{ ok: boolean }>("/api/file-write", { path, content, host }),

  // Delete a file or directory on `host` (directories recursively).
  deleteFile: (path: string, host?: string) =>
    postJSON<{ ok: boolean }>("/api/file-delete", { path, host }),

  // Rename an entry in place on `host`; `name` is a bare basename kept in the
  // same dir.
  renameFile: (path: string, name: string, host?: string) =>
    postJSON<{ ok: boolean; path: string }>("/api/file-rename", {
      path,
      name,
      host,
    }),

  // Diff metadata: the complete changed-file list with per-file counts (no diff
  // text — that's fetched per file via diffFile). `host` (omitted = the active
  // backend) is the machine the repo lives on — the cwd the sidebar follows can
  // sit on another host than the active one.
  diff: (path: string, host?: string) => {
    const params = new URLSearchParams({
      path,
      mode: "auto",
      ignoreWhitespace: "true",
    })
    return getJSON<DiffPayload>(withHost(`/api/diff?${params}`, host))
  },

  // The unified diff for a single file, pinned to the same comparison the list
  // is showing (mode "branch" | "working", plus the base branch in branch mode).
  // `host` (omitted = the active backend) is the machine the repo lives on.
  diffFile: (
    path: string,
    file: string,
    mode: "branch" | "working",
    baseBranch?: string,
    host?: string
  ) => {
    const params = new URLSearchParams({
      path,
      file,
      mode,
      ignoreWhitespace: "true",
    })
    if (baseBranch) params.set("baseBranch", baseBranch)
    return getJSON<FileDiff>(withHost(`/api/diff-file?${params}`, host))
  },

  // Both ids are required — /api/focus 400s on a missing tab_id.
  focus: (workspace_id: string, tab_id: string) =>
    postJSON<unknown>("/api/focus", { workspace_id, tab_id }),

  rename: (tab_id: string | undefined, label: string) =>
    postJSON<unknown>("/api/rename", { tab_id, label }),

  // Rename a workspace (relabels every pane/agent grouped under it).
  workspaceRename: (workspace_id: string | undefined, label: string) =>
    postJSON<unknown>("/api/workspace-rename", { workspace_id, label }),

  close: (pane_ids: string[]) =>
    postJSON<{ closed?: string[]; errors?: Record<string, string> }>(
      "/api/close",
      { pane_ids }
    ),

  // Write a pasted image to the target host (defaults to active) and return the
  // path on that host to insert into the description.
  pasteImage: async (file: Blob, host?: string): Promise<{ path: string }> => {
    const r = await hostFetch(withHost("/api/paste-image", host), {
      method: "POST",
      headers: { "Content-Type": file.type || "image/png" },
      body: file,
    })
    if (!r.ok) throw await httpError(r)
    return r.json()
  },

  // --- Agent creation ---

  // The creator's settings + agent log for a host (its own lasso.db; defaults to
  // the active host). Settings come from that host; last-used/agent log are this
  // lasso's local memory of what it did there.
  agentConfig: (host?: string) =>
    getJSON<AgentConfig>(withHost("/api/agent-config", host)),

  // Update the global creator defaults (repos_root, branch_prefix,
  // default_agent, scratch_setup); omitted fields are left unchanged.
  saveAgentConfig: (
    cfg: Partial<
      Pick<
        AgentConfig,
        "repos_root" | "branch_prefix" | "default_agent" | "scratch_setup"
      >
    >,
    host?: string
  ) => postJSON<AgentConfig>(withHost("/api/agent-config", host), cfg),

  // Save a repo's per-repo creator settings (copy-files globs + setup script).
  // These live with the repo, not the agent, so they're edited in Settings.
  saveRepoConfig: (
    cfg: {
      path: string
      copy_files?: string
      setup?: string
    },
    host?: string
  ) => postJSON<RepoConfig>(withHost("/api/repo-config", host), cfg),

  // Git repos discovered under repos_root, each with its remembered state.
  repos: (host?: string) =>
    getJSON<{ root: string; repos: RepoEntry[] }>(withHost("/api/repos", host)),

  // Local + remote branches of a repo, plus its detected default branch.
  repoBranches: (path: string, host?: string) =>
    getJSON<RepoBranches>(
      withHost(`/api/repo-branches?path=${encodeURIComponent(path)}`, host)
    ),

  // Stage attachment files on the target host (defaults to active) before
  // creating the agent; returns the staging dir id + stored filenames to pass to
  // createAgent, which moves them into the work dir on that same host.
  uploadAgentFiles: async (
    files: File[],
    host?: string
  ): Promise<{ upload_dir: string; files: string[] }> => {
    const form = new FormData()
    for (const f of files) form.append("files", f, f.name)
    const r = await hostFetch(withHost("/api/agent-upload", host), {
      method: "POST",
      body: form,
    })
    if (!r.ok) throw new Error(await r.text())
    return r.json()
  },

  // Create + launch an agent (git worktree or scratch workspace).
  createAgent: (payload: CreateAgentPayload) =>
    postJSON<AgentRecord>("/api/create-agent", payload),

  // Create a bare herdr workspace running just a shell (no agent). Interactive
  // callers keep focus=true so the user can type immediately; automation can
  // pass false without moving herdr's session-global focus.
  createTerminal: (label: string, focus = true) =>
    postJSON<{ workspace_id: string; root_pane: string }>(
      "/api/create-terminal",
      { label, focus }
    ),
}
