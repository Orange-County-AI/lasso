// Typed wrappers around lasso's Go HTTP API. Every endpoint the original
// index.html called via fetch() lives here, so components never build URLs by
// hand. Paths are same-origin (the Go server, or Vite's dev proxy onto it).

export interface ActiveState {
  cwd?: string
  pane_id?: string
  panes_rev?: number
  theme_rev?: number
  // Active host ("local" or an ssh-config alias) and a counter that bumps on
  // every host switch so the browser can reload the terminal iframes.
  host?: string
  term_rev?: number
}

// One ssh-config host as a herdr target. Selectable in the footer switcher only
// when reachable && running && compatible; otherwise greyed out with `err`.
export interface HostInfo {
  alias: string
  reachable: boolean
  running: boolean
  version: string
  protocol: number
  socket: string
  compatible: boolean
  err?: string
}

export interface HostsPayload {
  active: string
  local: { version: string; protocol: number; hostname: string }
  hosts: HostInfo[]
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

// One pane in the Grid tab: a herdr pane on a specific host, enriched with
// workspace/tab labels and whether herdr detects an agent in it. `host` is
// "local" or an ssh-config alias and is the key for both attaching its terminal
// and focusing it (switching the active host first when it isn't already active).
// `terminal_id` is herdr's handle for a direct `terminal attach`.
export interface GridPane {
  host: string
  host_label: string
  pane_id: string
  terminal_id: string
  workspace_id?: string
  workspace_label?: string
  tab_id?: string
  tab_label?: string
  cwd?: string
  agent?: string
  agent_status?: string
  has_agent?: boolean
  focused?: boolean
}

export interface GridPayload {
  panes: GridPane[]
  // host → why its panes couldn't be listed (unreachable, protocol drift, …).
  // The rest of the grid still renders; the UI shows these as per-host chips.
  errors?: Record<string, string>
}

// A herdr-detected agent (claude, codex, …) running in a pane. `agent` is the
// detected kind; `tab_label` is the pane's (tab's) renamable label. `target` is
// the opaque handle to pass to agentFocus.
export interface Agent {
  target: string
  pane_id: string
  workspace_id?: string
  workspace_label?: string
  tab_id?: string
  tab_label?: string
  cwd?: string
  agent?: string
  agent_status?: string
  focused?: boolean
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
  // Whether this install can self-update (a pitchfork-supervised git checkout).
  // False for dev/worktree runs, where the "Update lasso" action is hidden.
  updatable: boolean
  err?: string
}

export interface ThemePayload {
  name: string
  resolved: string
  customized: boolean
  css: string
  // xterm.js ITheme — shape is opaque to us; we hand it straight to the iframe.
  xterm: Record<string, unknown>
}

// httpError builds a concise Error from a non-OK response. lasso/herdr return
// short text or JSON errors, but a proxy in front of the app (e.g. the Cloudflare
// tunnel exposing lasso.knowsuchagency.ai) answers with a full HTML error page
// when the origin is down or briefly unreachable — during a host switch, a
// redeploy, etc. Dumping that raw HTML into the UI (the Diff tab, toasts) is just
// noise, so collapse HTML bodies (and empty ones) to the status line.
async function httpError(r: Response): Promise<Error> {
  const body = (await r.text().catch(() => "")).trim()
  const isHTML =
    /^<(?:!doctype|html|head|body)\b/i.test(body) ||
    (r.headers.get("content-type") || "").includes("text/html")
  if (!body || isHTML) {
    return new Error(
      `HTTP ${r.status}${r.statusText ? ` ${r.statusText}` : ""}`
    )
  }
  return new Error(body.length > 300 ? `${body.slice(0, 300)}…` : body)
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
  last_agent_type?: "git" | "scratch"
  scratch_setup?: string
  repos?: Record<string, RepoConfig>
  agents?: AgentRecord[]
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
  type: "git" | "scratch"
  title: string
  repo?: string
  base_branch?: string
  branch_prefix?: string
  branch_name?: string
  agent: string
  description?: string
  notes?: string
  plan_mode: boolean
  attachments?: string[]
  upload_dir?: string
}

async function getJSON<T>(url: string): Promise<T> {
  const r = await fetch(url)
  if (!r.ok) throw await httpError(r)
  return (await r.json()) as T
}

async function postJSON<T>(url: string, body: unknown): Promise<T> {
  const r = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  })
  if (!r.ok) throw await httpError(r)
  return (await r.json()) as T
}

export const api = {
  active: () => getJSON<ActiveState>("/api/active"),
  theme: () => getJSON<ThemePayload>("/api/theme"),

  // The ssh-config hosts probed for a compatible herdr server. ?refresh=1 skips
  // the server-side cache (the footer's manual refresh).
  hosts: (refresh = false) =>
    getJSON<HostsPayload>(`/api/hosts${refresh ? "?refresh=1" : ""}`),

  // Switch the active host ("local" or an alias). The backend re-points herdr
  // RPC, file/diff ops, and respawns the terminals at the new host.
  switchHost: (host: string) =>
    postJSON<{ active: string; version: string; protocol: number }>(
      "/api/host",
      { host }
    ),

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
  // pitchfork (installing pitchfork via mise if needed). For hosts where herdr
  // is missing or its server isn't running. Slow — downloads binaries — and
  // returns a provisioning log.
  provisionHost: (host: string) =>
    postJSON<{ ok: boolean; output: string; error?: string }>(
      "/api/host-provision",
      { host }
    ),

  // Update lasso itself: pull the latest source and let the supervisor rebuild +
  // restart it. Only works on the pitchfork-supervised prod install (see
  // VersionInfo.updatable); the server bounces a moment after this returns.
  selfUpdate: () =>
    postJSON<{ started: boolean; src: string; daemon: string }>(
      "/api/self-update",
      {}
    ),

  panes: () => getJSON<{ panes?: Pane[] }>("/api/panes"),
  agents: () => getJSON<{ agents?: Agent[] }>("/api/agents"),

  // Every herdr pane across every reachable, protocol-compatible host (local +
  // remotes), for the Grid tab. Aggregated server-side; per-host failures come
  // back in `errors` rather than failing the whole request.
  gridPanes: () => getJSON<GridPayload>("/api/grid"),

  // Ensure a ttyd is attached to one pane's terminal and return its proxy base
  // path (the iframe src). Idempotent — re-calling bumps the server-side idle
  // timer, so the Grid re-POSTs this as a keepalive while a cell is mounted.
  gridTerm: (host: string, terminal_id: string) =>
    postJSON<{ base: string }>("/api/grid/term", { host, terminal_id }),
  version: () => getJSON<VersionInfo>("/api/version"),

  files: (path: string) =>
    getJSON<DirListing>(`/api/files?path=${encodeURIComponent(path)}`),

  fileURL: (path: string) => `/api/file?path=${encodeURIComponent(path)}`,

  // A URL that forces a browser download (Content-Disposition: attachment) and
  // skips the preview size cap.
  downloadURL: (path: string) =>
    `/api/file?path=${encodeURIComponent(path)}&download=1`,

  // Upload one or more files into an existing directory. Filenames are kept
  // (basename only) — the server drops them into `dir`.
  uploadFiles: async (
    dir: string,
    files: File[]
  ): Promise<{ ok: boolean; files: string[] }> => {
    const form = new FormData()
    form.append("dir", dir)
    for (const f of files) form.append("files", f, f.name)
    const r = await fetch("/api/file-upload", { method: "POST", body: form })
    if (!r.ok) throw await httpError(r)
    return r.json()
  },

  fileText: async (path: string) => {
    const r = await fetch(api.fileURL(path))
    if (!r.ok) throw await httpError(r)
    return r.text()
  },

  // Overwrite an existing file with new content (preserving its mode).
  writeFile: (path: string, content: string) =>
    postJSON<{ ok: boolean }>("/api/file-write", { path, content }),

  // Delete a file or directory (directories recursively).
  deleteFile: (path: string) =>
    postJSON<{ ok: boolean }>("/api/file-delete", { path }),

  // Rename an entry in place; `name` is a bare basename kept in the same dir.
  renameFile: (path: string, name: string) =>
    postJSON<{ ok: boolean; path: string }>("/api/file-rename", { path, name }),

  // Diff metadata: the complete changed-file list with per-file counts (no diff
  // text — that's fetched per file via diffFile).
  diff: (path: string) => {
    const params = new URLSearchParams({
      path,
      mode: "auto",
      ignoreWhitespace: "true",
    })
    return getJSON<DiffPayload>(`/api/diff?${params}`)
  },

  // The unified diff for a single file, pinned to the same comparison the list
  // is showing (mode "branch" | "working", plus the base branch in branch mode).
  diffFile: (
    path: string,
    file: string,
    mode: "branch" | "working",
    baseBranch?: string
  ) => {
    const params = new URLSearchParams({
      path,
      file,
      mode,
      ignoreWhitespace: "true",
    })
    if (baseBranch) params.set("baseBranch", baseBranch)
    return getJSON<FileDiff>(`/api/diff-file?${params}`)
  },

  focus: (workspace_id?: string, tab_id?: string) =>
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

  // Focus a herdr-detected agent (focuses its workspace + tab).
  agentFocus: (target: string) =>
    postJSON<unknown>("/api/agent-focus", { target }),

  pasteImage: async (file: Blob): Promise<{ path: string }> => {
    const r = await fetch("/api/paste-image", {
      method: "POST",
      headers: { "Content-Type": file.type || "image/png" },
      body: file,
    })
    if (!r.ok) throw await httpError(r)
    return r.json()
  },

  // --- Agent creation ---

  // The creator's settings + agent log.
  agentConfig: () => getJSON<AgentConfig>("/api/agent-config"),

  // Update the global creator defaults (repos_root, branch_prefix,
  // default_agent, scratch_setup); omitted fields are left unchanged.
  saveAgentConfig: (
    cfg: Partial<
      Pick<
        AgentConfig,
        "repos_root" | "branch_prefix" | "default_agent" | "scratch_setup"
      >
    >
  ) => postJSON<AgentConfig>("/api/agent-config", cfg),

  // Save a repo's per-repo creator settings (copy-files globs + setup script).
  // These live with the repo, not the agent, so they're edited in Settings.
  saveRepoConfig: (cfg: {
    path: string
    copy_files?: string
    setup?: string
  }) => postJSON<RepoConfig>("/api/repo-config", cfg),

  // Git repos discovered under repos_root, each with its remembered state.
  repos: () => getJSON<{ root: string; repos: RepoEntry[] }>("/api/repos"),

  // Local + remote branches of a repo, plus its detected default branch.
  repoBranches: (path: string) =>
    getJSON<RepoBranches>(
      `/api/repo-branches?path=${encodeURIComponent(path)}`
    ),

  // Stage attachment files before creating the agent; returns the staging dir id
  // + stored filenames to pass to createAgent.
  uploadAgentFiles: async (
    files: File[]
  ): Promise<{ upload_dir: string; files: string[] }> => {
    const form = new FormData()
    for (const f of files) form.append("files", f, f.name)
    const r = await fetch("/api/agent-upload", { method: "POST", body: form })
    if (!r.ok) throw new Error(await r.text())
    return r.json()
  },

  // Create + launch an agent (git worktree or scratch workspace).
  createAgent: (payload: CreateAgentPayload) =>
    postJSON<AgentRecord>("/api/create-agent", payload),
}
