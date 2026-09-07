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
  // The host the focused pane's `cwd` lives on, as Luvus reports the pane —
  // normally this tab's host. File and diff requests address it explicitly so a
  // save can never land on a machine the sidebar isn't showing.
  cwd_host?: string
}

// One ssh-config host as a Luvus target. Selectable in the footer switcher only
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
  // Whether a luvus server session is up on the host.
  running: boolean
  // The remote luvus release ("0.13.4"), the UHP protocol label it reports
  // ("luvus-uhp/1.0", empty when unknown), and its UHP endpoint address.
  version: string
  protocol: string
  socket: string
  // Capability validation passed: protocol luvus-uhp major 1, with every UHP
  // method lasso needs present. `err` carries the human reason when it didn't
  // ("protocol luvus-uhp/2.0 ≠ luvus-uhp/1.x", "missing UHP methods: …").
  compatible: boolean
  err?: string
  state?: HostState
  // RFC3339 timestamp of the last COMPLETED probe; absent until one finishes.
  checked_at?: string
}

export interface HostsPayload {
  active: string
  local: { version: string; protocol: string; hostname: string; user: string }
  hosts: HostInfo[]
  // True while at least one host is still being probed — the cue to poll again
  // shortly, since the list is a partial answer that will fill in.
  probing: boolean
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
export interface Workspace {
  workspace_id: string
  label: string
  number: number
  tab_count: number
  focused: boolean
}

// One Luvus pane on a specific host, enriched with workspace/tab labels and
// whether Luvus detects an agent in it — the row shape of /api/all-panes and
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
  // sidebar's footprint sets the shared Luvus pty's width. 0 = never set.
  sidebar_pct: number
  // Files tab folder-click behavior: true re-roots the tree into the folder,
  // false expands it in place. Defaults true (see getUIState in db.go).
  files_click_navigates: boolean
}

// What a POST /api/ui-state write sends beyond the preferences themselves: who
// is writing, and whether a human was behind it. Both feed the server's
// sidebar-layout claim (see uilock.go / lib/ui-state's patchUIState).
export interface UIStateWrite extends Partial<UIState> {
  client_id: string
  user_intent: boolean
}

// The save response: the stored preferences, plus whether this client's write
// to the sidebar layout was REFUSED because another client owns it. When it
// was, the preferences in this body are the owner's — adopt them.
export interface UIStateResponse extends UIState {
  layout_denied?: boolean
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

// Protocol-compatibility check for the Settings tab: the UHP major this lasso
// build targets vs. the one the installed luvus server reports over its socket.
// `err` is set (and luvus_protocol is 0) when the server can't be reached, so
// the tab shows "luvus unreachable" instead of a false mismatch.
export interface VersionInfo {
  lasso_protocol: number
  // This lasso build's own version (git revision from the Go VCS stamp, or
  // "dev"). Shown in the host switcher so a stale install is visible.
  lasso_version: string
  luvus_protocol: number
  luvus_version?: string
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

export interface ThemePayload {
  // The ACTIVE Luvus theme, which lasso only ever reads: its id, its display
  // name, and whether Luvus calls the palette dark or light. `luvus theme use
  // <id>` (or Luvus's own Settings) is the only way to change it — lasso never
  // writes a theme into Luvus's config.
  name: string
  label: string
  appearance: "dark" | "light" | "terminal"
  // The palette actually in force: `name`, or the last good one when Luvus
  // couldn't be reached on this tick.
  resolved: string
  // The active theme isn't one of Luvus's built-ins (an installed or virtual
  // theme from its themes directory).
  customized: boolean
  css: string
  // xterm.js ITheme — shape is opaque to us; we hand it straight to the iframe.
  xterm: Record<string, unknown>
  // Whether lasso mirrors the theme into agent CLIs' theme files (opencode,
  // Claude Code, omp) — the "Sync agent themes" toggle.
  sync_agent_themes: boolean
  // Hosts ("local" or ssh aliases) lasso writes no theme to at all — none of
  // their agents' theme files. Everything else syncs.
  theme_sync_off: string[]
}

// httpError builds a concise Error from a non-OK response. lasso/Luvus return
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
  default_terminal_workspace: string
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
export interface CreateTerminalPayload {
  host?: string
  command: string
  workspace_id?: string
  workspace_name?: string
  tab_name?: string
  focus?: boolean
}

export interface CreateTerminalResult {
  workspace_id: string
  tab_id?: string
  root_pane: string
  command_error?: string
  tab_name_error?: string
}

// GET /api/push: the key a subscription must be minted with, and the devices
// lasso currently pushes to. Endpoints are deliberately absent — an endpoint is
// a bearer capability to push to that device — so a row is identified by a short
// digest of it.
export interface PushDevice {
  id: string
  label: string
  created_at?: string
  last_ok?: string
  last_error?: string
}

export interface PushConfig {
  public_key: string
  devices: PushDevice[]
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
  promise: Promise<{ active: string; version: string; protocol: string }>
} | null = null

export const api = {
  active: () => getJSON<ActiveState>("/api/active"),
  // The ACTIVE Luvus theme. Read-only by design: lasso NEVER writes a theme
  // into Luvus — `luvus theme use <id>` is the only way to change it, and lasso
  // follows via the theme_rev SSE bump.
  theme: () => getJSON<ThemePayload>("/api/theme"),
  // Flips the server-level "sync agent themes" toggle (no theme change).
  setSyncAgentThemes: (enabled: boolean) =>
    postJSON<{ ok: boolean; sync_agent_themes: boolean }>("/api/theme-set", {
      sync_agent_themes: enabled,
    }),
  // Switches every theme write lasso makes to ONE host on or off ("local" or an
  // ssh alias): the agent theme files there. Turning it back on pushes the
  // current theme to that host right away when it's reachable.
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

  // Notifications (webpush.go). The config carries the VAPID public key a
  // subscription must be minted with plus the devices already registered; the
  // subscribe body is the browser's own PushSubscription JSON, passed through
  // verbatim so nothing here has to understand its key encoding (lib/push.ts).
  pushConfig: () => getJSON<PushConfig>("/api/push"),
  pushSubscribe: (sub: PushSubscriptionJSON, origin: string) =>
    postJSON<{ ok: boolean; devices: number }>("/api/push/subscribe", {
      ...sub,
      origin,
    }),
  pushUnsubscribe: (endpoint: string) =>
    postJSON<{ ok: boolean; devices: number }>("/api/push/unsubscribe", {
      endpoint,
    }),
  // Sends one notification down the real pipeline — same encryption, same
  // service worker — and reports per-device failures instead of a bare 200, so
  // "it says it's on but nothing arrives" has an answer.
  pushTest: () =>
    postJSON<{ ok: boolean; devices?: number; error?: string }>(
      "/api/push/test",
      {}
    ),

  // The ssh-config hosts probed for a compatible luvus server. ?refresh=1 skips
  // the server-side cache (the footer's manual refresh).
  hosts: (refresh = false) =>
    getJSON<HostsPayload>(`/api/hosts${refresh ? "?refresh=1" : ""}`),

  // Attach THIS tab to a host ("local" or an alias): the server resolves and
  // pools its connection and makes sure its terminals are spawned, then reports
  // the luvus version/UHP protocol to expect. It mutates nothing shared — the tab
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
      postJSON<{ active: string; version: string; protocol: string }>(
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

  // Run `luvus update` on a remote host whose luvus this lasso can't drive (or
  // is simply older), auto-answering its interactive prompts. Slow — it
  // downloads a release binary on the far side — and returns the captured output.
  updateHost: (host: string) =>
    postJSON<{ ok: boolean; output: string; error?: string }>(
      "/api/host-update",
      { host }
    ),

  // Install luvus on a remote host (if missing) and bring it up supervised by
  // systemd --user (also installing its agent integrations). For hosts where
  // luvus is missing or its server session isn't running. Slow — downloads
  // binaries — and returns a provisioning log.
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

  // Every luvus pane across every reachable, compatible host (local +
  // remotes), for the ⌘K pane switcher. Aggregated server-side; per-host
  // failures come back in `errors` rather than failing the whole request.
  allPanes: () => getJSON<PanesPayload>("/api/all-panes", aggregateTimeout),

  // Every agent lasso ever spawned (across hosts), shaped as HostPane rows so the
  // ⌘K switcher can list past agents next to live panes. AgentID is set; the
  // switcher treats a row whose host+pane_id isn't currently live as "closed" and
  // reopens it via reopenAgent on select.
  agentHistory: () =>
    getJSON<{ agents: HostPane[] }>("/api/agent-history", aggregateTimeout),

  // Re-open a past session's workspace: re-creates a Luvus workspace at its work
  // dir and focuses it unless focus:false (does NOT relaunch the agent). Identify
  // it by agent_id (a recorded agent — also re-points its record at the new pane)
  // or by work_dir (an orphan worktree/scratch dir with no record). Returns the
  // new pane so the caller can focus it through the normal pane-focus path.
  reopenAgent: (
    host: string,
    body: { agent_id?: string; work_dir?: string; focus?: boolean }
  ) => postJSON<HostPane>("/api/agent/reopen", { host, ...body }),

  // Persisted UI preferences (sidebar layout and the Files tab).
  uiState: () => getJSON<UIState>("/api/ui-state"),
  // Patch semantics: send only the changed fields; the server merges into the
  // stored state (so stale tabs can't clobber fields they didn't touch) and
  // returns the merged whole.
  saveUIState: (write: UIStateWrite) =>
    postJSON<UIStateResponse>("/api/ui-state", write),
  version: () => getJSON<VersionInfo>("/api/version"),

  // List a directory. `host` (omitted = the active backend) is the host the
  // path lives on — the sidebar browses the focused pane's host, which the
  // request must name rather than inherit.
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
    const r = await hostFetch("/api/file-upload", {
      method: "POST",
      body: form,
    })
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

  // Focus one pane. Luvus's `pane.focus` takes the pane itself and jumps to its
  // workspace/tab, so the pane id is both necessary and sufficient — a
  // workspace/tab pair would land on whichever split that tab last had.
  focus: (pane_id: string) => postJSON<unknown>("/api/focus", { pane_id }),

  // Label the tab a pane sits in. Sent as the pane id: Luvus 0.13.4's
  // `tab.rename` renames the ACTIVE tab of the pane's workspace, so the server
  // resolves and targets the tab from the pane rather than trusting a tab id.
  rename: (pane_id: string, label: string) =>
    postJSON<unknown>("/api/rename", { pane_id, label }),

  // Rename a workspace (relabels every pane/agent grouped under it).
  workspaceRename: (workspace_id: string | undefined, label: string) =>
    postJSON<unknown>("/api/workspace-rename", { workspace_id, label }),

  close: (pane_ids: string[]) =>
    postJSON<{ closed?: string[]; errors?: Record<string, string> }>(
      "/api/close",
      { pane_ids }
    ),

  // Write a file the browser is holding — a pasted screenshot, a picked photo
  // or document — to the target host (defaults to active) and return the path
  // on that host to insert into the prompt or the terminal. The name is sent so
  // the path stays recognizable; a body with none is stamped by content type.
  pasteFile: async (
    file: Blob,
    host?: string,
    name?: string
  ): Promise<{ path: string }> => {
    // name first, so withHost's own separator logic stays correct either way.
    const named = name
      ? `/api/paste-file?name=${encodeURIComponent(name)}`
      : "/api/paste-file"
    const r = await hostFetch(withHost(named, host), {
      method: "POST",
      headers: { "Content-Type": file.type || "application/octet-stream" },
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

  // Update the host-scoped creation defaults; omitted fields are unchanged.
  saveAgentConfig: (
    cfg: Partial<
      Pick<
        AgentConfig,
        | "repos_root"
        | "branch_prefix"
        | "default_agent"
        | "default_terminal_workspace"
        | "scratch_setup"
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

  // Live Luvus workspaces on one host, for terminal creation and defaults.
  workspaces: (host?: string) =>
    getJSON<{ workspaces: Workspace[] }>(withHost("/api/workspaces", host)),

  // Create a bare terminal in an existing workspace or as the root of a new
  // workspace. A blank command leaves the shell prompt untouched.
  createTerminal: (payload: CreateTerminalPayload) =>
    postJSON<CreateTerminalResult>("/api/create-terminal", payload),
}
