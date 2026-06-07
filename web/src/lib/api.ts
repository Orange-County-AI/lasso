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
  // tab id → agent status (idle|working|blocked), pushed by the status poller.
  agent_statuses?: Record<string, string>
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
  pane_label?: string
  cwd?: string
  agent?: string
  agent_status?: string
  has_agent?: boolean
  focused?: boolean
  // The agent's initial prompt (creation description). Carried for search only —
  // the pane switcher matches against it but doesn't display the full text.
  prompt?: string
}

export interface GridPayload {
  panes: GridPane[]
  // host → why its panes couldn't be listed (unreachable, protocol drift, …).
  // The rest of the grid still renders; the UI shows these as per-host chips.
  errors?: Record<string, string>
}

// Persisted, global browser UI preferences (SQLite-backed): Grid tab filters +
// sidebar collapse. The client reads the whole object and writes the whole
// object back (merge happens client-side), so navigating away and back — or
// opening lasso elsewhere — restores the same view.
export interface UIState {
  grid_agents_only: boolean
  grid_hidden_hosts: string[]
  // host|pane_id keys of the Grid tab's multi-selected cells, so the selection
  // survives navigating away and back (or reloading).
  grid_selected: string[]
  sidebar_collapsed: boolean
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
  name: string
  resolved: string
  customized: boolean
  css: string
  // xterm.js ITheme — shape is opaque to us; we hand it straight to the iframe.
  xterm: Record<string, unknown>
}

// ---------------------------------------------------------------------------
// Sidebar tree (tmux era): repos → worktrees, plus a flat agent list.
// ---------------------------------------------------------------------------

export type AgentStatus = "idle" | "working" | "blocked" | "unknown"

export interface TreeTab {
  id: string
  title: string
  kind: "shell" | "agent"
  agent_id?: string
  agent?: string // claude | codex
  status?: AgentStatus
}

export interface TreeWorkspace {
  id: string
  title: string
  repo?: string
  work_dir: string
  kind: "git" | "scratch"
  pinned: boolean
  branch?: string
  tabs: TreeTab[]
  // Aggregate live-agent status for the sidebar dot (blocked > working > idle),
  // or absent when no tab is running an agent.
  agent_status?: AgentStatus
  agent_kind?: string
}

export interface TreeRepo {
  path: string
  name: string
  primary_branch: string
  pinned: boolean
  last_commit: number
  workspaces: TreeWorkspace[] // linked worktrees only
  // The repo row is itself the main checkout (primary branch). main_tab_id is
  // its tab if one exists; otherwise click calls openRepo to create one.
  // main_workspace carries that checkout's full workspace (with tabs) so the tab
  // strip can resolve it — it is not rendered as a child in the tree.
  main_workspace_id?: string
  main_tab_id?: string
  main_workspace?: TreeWorkspace
  agent_status?: AgentStatus
  agent_kind?: string
}

export interface TreePayload {
  repos: TreeRepo[]
  scratch: TreeWorkspace[]
}

export interface AgentRow {
  tab_id: string
  agent_id: string
  title: string
  agent: string
  status: AgentStatus
  workspace_id: string
  workspace_title: string
  repo?: string
  cwd: string
  prompt?: string
}

export interface ThemeMeta {
  name: string
  light: boolean
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
  tab_id?: string
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
  // The agent's instruction; its first line becomes the title (branch/dir name,
  // workspace label, list/toast headline).
  prompt: string
  repo?: string
  base_branch?: string
  branch_prefix?: string
  branch_name?: string
  agent: string
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

// withHost appends ?host=/&host= to a config endpoint so it targets a specific
// host's own settings (its lasso.db). Omitted = the backend's active host.
function withHost(url: string, host?: string): string {
  if (!host) return url
  return `${url}${url.includes("?") ? "&" : "?"}host=${encodeURIComponent(host)}`
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

  // Every herdr pane across every reachable, protocol-compatible host (local +
  // remotes), for the Grid tab. Aggregated server-side; per-host failures come
  // back in `errors` rather than failing the whole request.
  gridPanes: () => getJSON<GridPayload>("/api/grid"),

  // Ensure a ttyd is attached to one pane's terminal and return its proxy base
  // path (the iframe src). Used to first-attach a visible cell; creates the ttyd
  // if needed. Keepalives use gridTermTouch instead (see below).
  gridTerm: (host: string, terminal_id: string) =>
    postJSON<{ base: string }>("/api/grid/term", { host, terminal_id }),

  // Bump a live grid terminal's idle timer WITHOUT creating one — the keepalive a
  // mounted cell fires every KEEPALIVE_MS. Unlike gridTerm it can't resurrect an
  // attach the cell just released, which is what kept a thin grid attach clamping
  // the focused pane's width in the wide Herdr terminal. `alive` is false if the
  // entry was reaped (the caller can re-attach via gridTerm while still visible).
  gridTermTouch: (host: string, terminal_id: string) =>
    postJSON<{ alive: boolean }>("/api/grid/term-touch", { host, terminal_id }),

  // Detach a pane's grid terminal (kills its ttyd). Called when a cell leaves
  // the grid so the pane isn't held to the cell's narrow width while it's viewed
  // full-size in the Herdr terminal. `keepalive` lets it complete even when fired
  // from a React unmount/teardown. Best-effort — failures are ignored.
  gridTermRelease: (host: string, terminal_id: string) =>
    fetch("/api/grid/term-release", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ host, terminal_id }),
      keepalive: true,
    }).catch(() => {}),

  // Tear down every grid terminal at once — the authoritative backstop fired when
  // the Grid view is left, so no thin attach survives in the background to clamp a
  // pane viewed full-size in Herdr (even if a per-cell release was dropped or raced
  // a keepalive). `keepalive` lets it complete from a React unmount/teardown.
  gridTermReleaseAll: () =>
    fetch("/api/grid/term-release-all", {
      method: "POST",
      keepalive: true,
    }).catch(() => {}),

  // Rename the workspace a grid pane belongs to, on that pane's host.
  gridRename: (host: string, workspace_id: string, label: string) =>
    postJSON<{ ok: boolean }>("/api/grid/rename", {
      host,
      workspace_id,
      label,
    }),

  // Close one or more grid panes (each tagged with its host — the selection can
  // span hosts). Reports per-pane failures rather than failing the whole batch.
  gridClose: (panes: { host: string; pane_id: string }[]) =>
    postJSON<{ closed: number; errors?: Record<string, string> }>(
      "/api/grid/close",
      { panes }
    ),

  // Persisted UI prefs (grid filters + sidebar collapse).
  uiState: () => getJSON<UIState>("/api/ui-state"),
  saveUIState: (s: UIState) => postJSON<UIState>("/api/ui-state", s),
  version: () => getJSON<VersionInfo>("/api/version"),

  // --- sidebar tree + workspace/tab/repo mutations (tmux era) ---
  tree: () => getJSON<TreePayload>("/api/tree"),
  agentsList: () => getJSON<{ agents: AgentRow[] }>("/api/agents"),

  // Point the single persistent viewport ttyd at a tab's tmux session (an empty
  // tab_id just warms the viewport). Returns the stable iframe base path — the
  // frontend keeps one iframe on it and re-POSTs to switch tabs.
  tabTerm: (tab_id: string) =>
    postJSON<{ base: string }>("/api/tab/term", { tab_id }),
  // Whether the viewport ttyd is still alive (respawn + reload base if not).
  tabTermTouch: () => postJSON<{ alive: boolean }>("/api/tab/term-touch", {}),

  newTab: (workspace_id: string, title?: string) =>
    postJSON<TreeTab>("/api/tab/new", { workspace_id, title }),
  renameTab: (tab_id: string, title: string) =>
    postJSON<{ ok: boolean }>("/api/tab/rename", { tab_id, title }),
  closeTab: (tab_id: string) =>
    postJSON<{ ok: boolean }>("/api/tab/close", { tab_id }),
  // Create a bare scratch workspace (a shell, no agent).
  createWorkspace: (title?: string) =>
    postJSON<{ workspace_id: string; tab_id: string; work_dir: string }>(
      "/api/workspace/create",
      { title }
    ),
  renameWorkspace: (workspace_id: string, title: string) =>
    postJSON<{ ok: boolean }>("/api/workspace/rename", { workspace_id, title }),
  closeWorkspace: (workspace_id: string) =>
    postJSON<{ ok: boolean }>("/api/workspace/close", { workspace_id }),
  pinRepo: (repo: string, pinned: boolean) =>
    postJSON<{ ok: boolean }>("/api/repo/pin", { repo, pinned }),
  // Open (creating on first use) a terminal on a repo's primary branch — its
  // main checkout at the repo root.
  openRepo: (repo: string) =>
    postJSON<{ tab_id: string; workspace_id: string }>("/api/repo/open", {
      repo,
    }),
  renameRepo: (repo: string, name: string) =>
    postJSON<{ ok: boolean }>("/api/repo/rename", { repo, name }),

  // Make a git worktree + workspace with a shell tab but NO agent.
  createWorktree: (body: {
    repo: string
    base_branch?: string
    branch_name?: string
    title?: string
  }) =>
    postJSON<{ workspace_id: string; work_dir: string; branch: string }>(
      "/api/create-worktree",
      body
    ),

  // Themes: list available + select one (the select repaints via SSE).
  themes: () =>
    getJSON<{ themes: ThemeMeta[]; selected: string }>("/api/themes"),
  setTheme: (name: string) => postJSON<{ ok: boolean }>("/api/theme", { name }),

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

  // A cheap change signature (Last-Modified + size) fetched via HEAD — no body
  // download — so a binary preview can poll for on-disk changes and only reload
  // when the file actually changed. Returns null on any failure (the caller
  // treats that as "no change observed").
  fileSig: async (path: string): Promise<string | null> => {
    try {
      const r = await fetch(api.fileURL(path), { method: "HEAD" })
      if (!r.ok) return null
      const lm = r.headers.get("last-modified") ?? ""
      const len = r.headers.get("content-length") ?? ""
      return `${lm}:${len}`
    } catch {
      return null
    }
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

  // Write a pasted image to the target host (defaults to active) and return the
  // path on that host to insert into the description.
  pasteImage: async (file: Blob, host?: string): Promise<{ path: string }> => {
    const r = await fetch(withHost("/api/paste-image", host), {
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
    const r = await fetch(withHost("/api/agent-upload", host), {
      method: "POST",
      body: form,
    })
    if (!r.ok) throw new Error(await r.text())
    return r.json()
  },

  // Create + launch an agent (git worktree or scratch workspace).
  createAgent: (payload: CreateAgentPayload) =>
    postJSON<AgentRecord>("/api/create-agent", payload),

  // Create a bare herdr workspace running just a shell (no agent) and focus it.
  // The backend focuses the new workspace server-side; the caller surfaces the
  // Herdr tab and hands the keyboard to its terminal.
  createTerminal: (label: string) =>
    postJSON<{ workspace_id: string; root_pane: string }>(
      "/api/create-terminal",
      { label }
    ),
}
