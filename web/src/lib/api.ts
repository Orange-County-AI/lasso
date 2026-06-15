// Typed wrappers around lasso's Go HTTP API. Every endpoint the original
// index.html called via fetch() lives here, so components never build URLs by
// hand. Paths are same-origin (the Go server, or Vite's dev proxy onto it).

export interface ActiveState {
  cwd?: string
  pane_id?: string
  panes_rev?: number
  // The local host label and a counter that bumps when terminals must reload.
  host?: string
  term_rev?: number
  // A counter that bumps when /api/ui-state is written, so other clients
  // re-pull and apply the new layout.
  ui_rev?: number
  // tab id → agent status (idle|working|blocked), pushed by the status poller.
  agent_statuses?: Record<string, string>
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

// ---------------------------------------------------------------------------
// Multi-host: ssh-config hosts lasso can drive, and the cross-host grid.
// ---------------------------------------------------------------------------

// One ssh-config host. Usable (selectable, a valid agent/grid target) when
// reachable && has_tmux; otherwise the UI greys it out and shows `err`.
export interface HostInfo {
  alias: string
  reachable: boolean
  has_tmux: boolean
  tmux_version?: string
  home?: string
  err?: string
}

export interface HostsPayload {
  active: string // the host lasso currently drives ("local" or an alias)
  hostname: string // local machine label, shown for the local host
  hosts: HostInfo[]
}

// One live tab on one host, as rendered by the grid. A "pane" maps 1:1 to a tab
// (lasso's terminal granularity is the tab's tmux session).
export interface GridPane {
  host: string
  host_label: string
  tab_id: string
  workspace_id: string
  workspace_label: string
  tab_label: string
  cwd: string
  agent?: string
  agent_status?: AgentStatus
  has_agent: boolean
  prompt?: string
  git: boolean // pane lives in a git checkout (else no status dot)
  dirty?: number // working-tree changes (git status lines); absent/0 = clean
}

export interface GridPayload {
  panes: GridPane[]
  errors?: Record<string, string> // host → why it couldn't be reached
}

// Server-persisted, global UI preferences (the grid's filters, sidebar state,
// and the side panels' last-open widths as % of the panel group — zero/absent
// width means "never saved"). One blob shared across browsers/tabs, last write
// wins; write through patchUIState (lib/ui-state.ts) so partial updates never
// clobber the other fields.
export interface UIState {
  grid_agents_only: boolean
  grid_hidden_hosts: string[]
  grid_selected: string[] // "host|tab_id" keys of multi-selected cells
  sidebar_collapsed: boolean
  left_width?: number
  right_width?: number
  // Show every usable host's spaces/agents in the sidebar (grouped by host)
  // instead of only the active host's.
  sidebar_all_hosts?: boolean
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

// Version + update status for the Settings tab.
export interface VersionInfo {
  // This lasso build's own version (git revision from the Go VCS stamp, or
  // "dev"). Shown in Settings so a stale install is visible.
  lasso_version: string
  // The newest published GitHub release tag. When newer than lasso_version, the
  // Settings tab shows an "update available" hint pointing at `lasso update`.
  latest_version?: string
  // Whether the running build is behind the latest release.
  // "available" — a newer release is out; "current" — already up to date;
  // "unknown" — can't tell.
  update_state?: "available" | "current" | "unknown"
  err?: string
}

// ---------------------------------------------------------------------------
// Sidebar tree: repos → worktrees, plus a flat agent list.
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
  branch?: string
  tabs: TreeTab[]
  // Host this workspace lives on ("local" or an ssh alias) + its display label,
  // for grouping the sidebar by host in all-hosts mode.
  host?: string
  host_label?: string
  // Aggregate live-agent status for the sidebar dot (blocked > working > idle),
  // or absent when no tab is running an agent.
  agent_status?: AgentStatus
  agent_kind?: string
}

export interface TreeRepo {
  path: string
  name: string
  primary_branch: string
  last_commit: number
  // Primary branch's status vs its configured upstream. upstream is absent for
  // local-only repos; ahead/behind are commit counts (absent when zero).
  upstream?: string
  ahead?: number
  behind?: number
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
  // Host this repo lives on ("local" or an ssh alias) + its display label, for
  // grouping the sidebar by host in all-hosts mode.
  host?: string
  host_label?: string
}

export interface TreePayload {
  repos: TreeRepo[]
  scratch: TreeWorkspace[]
  // Authoritative top-level display order of the unified "spaces" list as stable
  // keys ("ws:<id>" for scratch, "repo:<path>" for repos). Rows absent from it
  // (e.g. just-created) are rendered at the bottom by the sidebar.
  order: string[]
  // Host keys ("local" or an ssh alias) whose tree was successfully queried this
  // round (present even when the host has zero workspaces). In all-hosts mode the
  // sidebar reconciles this against the usable host set to show a per-host loading
  // state for hosts still connecting, instead of remote workspaces trickling in.
  // errors maps a host key to why it couldn't be reached this round.
  hosts?: string[]
  errors?: Record<string, string>
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
  // Host this agent runs on ("local" or an ssh alias) + its display label, for
  // grouping the agents pane by host in all-hosts mode.
  host?: string
  host_label?: string
}

// httpError builds a concise Error from a non-OK response. The backend returns
// short text or JSON errors, but a proxy in front of the app (e.g. the Cloudflare
// tunnel exposing lasso.knowsuchagency.ai) answers with a full HTML error page
// when the origin is down or briefly unreachable — during a redeploy, etc.
// Dumping that raw HTML into the UI (the Diff tab, toasts) is just noise, so
// collapse HTML bodies (and empty ones) to the status line.
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

// Per-repo remembered creator state (lives in ~/.lasso/lasso.db, keyed by repo
// path).
export interface RepoConfig {
  last_base_branch?: string
  copy_files?: string
  setup?: string
}

// One agent lasso has spawned.
export interface AgentRecord {
  id: string
  host?: string
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

// The creator's settings + remembered selections + agent log (GET/POST
// /api/agent-config). `default_agent` may be "" — no preset default, in which
// case the creator falls back to `last_agent`.
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
  // Target host ("" / "local" = the local box, else an ssh alias).
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

// withHost appends ?host=/&host= to a config endpoint. The backend is
// local-only now, so callers pass "local"; omitted = the backend default.
function withHost(url: string, host?: string): string {
  if (!host) return url
  return `${url}${url.includes("?") ? "&" : "?"}host=${encodeURIComponent(host)}`
}

export const api = {
  active: () => getJSON<ActiveState>("/api/active"),

  panes: () => getJSON<{ panes?: Pane[] }>("/api/panes"),

  version: () => getJSON<VersionInfo>("/api/version"),

  // --- sidebar tree + workspace/tab/repo mutations ---
  // `all` aggregates every usable host's spaces/agents (grouped by host) instead
  // of just the active host's.
  tree: (all = false) =>
    getJSON<TreePayload>(`/api/tree${all ? "?all=1" : ""}`),
  agentsList: (all = false) =>
    getJSON<{ agents: AgentRow[] }>(`/api/agents${all ? "?all=1" : ""}`),

  // Point the single persistent viewport ttyd at a tab's tmux session (an empty
  // tab_id just warms the viewport). Returns the stable iframe base path — the
  // frontend keeps one iframe on it and re-POSTs to switch tabs.
  tabTerm: (tab_id: string) =>
    postJSON<{ base: string }>("/api/tab/term", { tab_id }),
  // Whether the viewport ttyd is still alive (respawn + reload base if not).
  tabTermTouch: () => postJSON<{ alive: boolean }>("/api/tab/term-touch", {}),
  // Whether a tab's session has painted content yet (drives the loading overlay).
  tabReady: (tab_id: string) =>
    postJSON<{ ready: boolean }>("/api/tab/term-ready", { tab_id }),

  newTab: (workspace_id: string, title?: string) =>
    postJSON<TreeTab>("/api/tab/new", { workspace_id, title }),
  renameTab: (tab_id: string, title: string) =>
    postJSON<{ ok: boolean }>("/api/tab/rename", { tab_id, title }),
  closeTab: (tab_id: string) =>
    postJSON<{ ok: boolean }>("/api/tab/close", { tab_id }),
  // Close an agent: closes its tab and, if that empties its workspace, closes the
  // workspace too (its worktree leaves the spaces pane; the on-disk worktree is
  // kept). Use this for the agents-pane "Close agent", not the bare closeTab.
  closeAgent: (tab_id: string) =>
    postJSON<{ ok: boolean }>("/api/agent/close", { tab_id }),
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
  // Persist the user's drag-and-drop ordering of the unified "spaces" list (the
  // full current key list in its new order). `host` scopes the order to one host
  // group in all-hosts mode (omitted = the active host).
  reorderSpaces: (order: string[], host?: string) =>
    postJSON<{ ok: boolean }>("/api/spaces/reorder", { order, host }),
  // Open (creating on first use) a terminal on a repo's primary branch — its
  // main checkout at the repo root. `host` targets the repo's host in all-hosts
  // mode (omitted = the active host).
  openRepo: (repo: string, host?: string) =>
    postJSON<{ tab_id: string; workspace_id: string }>("/api/repo/open", {
      repo,
      host,
    }),
  renameRepo: (repo: string, name: string, host?: string) =>
    postJSON<{ ok: boolean }>("/api/repo/rename", { repo, name, host }),
  // Close a whole repo: closes its main checkout and every linked worktree (and
  // their tabs/agents), dropping the repo from the spaces pane. The on-disk
  // checkout/worktrees are kept; reopen via New Agent / ⌘K.
  closeRepo: (repo: string, host?: string) =>
    postJSON<{ ok: boolean }>("/api/repo/close", { repo, host }),

  // Make a git worktree + workspace with a shell tab but NO agent.
  createWorktree: (body: {
    repo: string
    base_branch?: string
    branch_name?: string
    title?: string
    host?: string
  }) =>
    postJSON<{ workspace_id: string; work_dir: string; branch: string }>(
      "/api/create-worktree",
      body
    ),

  files: (path: string) =>
    getJSON<DirListing>(`/api/files?path=${encodeURIComponent(path)}`),

  // The live working directory of a tab's terminal (its tmux pane cwd), polled
  // by the Shell so the Files/Diff panel follows the active terminal as it cd's
  // around. Falls back to the tab's saved launch dir when no session is live.
  tabCwd: (tab: string) =>
    getJSON<{ cwd: string }>(`/api/tab-cwd?tab=${encodeURIComponent(tab)}`),

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

  // Write a pasted image to disk and return its path to insert into the
  // description.
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

  // The creator's settings + agent log (~/.lasso/lasso.db).
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

  // Stage attachment files before creating the agent; returns the staging dir
  // id + stored filenames to pass to createAgent, which moves them into the
  // work dir.
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

  // --- Multi-host + grid ---

  // List ssh-config hosts lasso can drive (refresh forces a re-probe).
  hosts: (refresh = false) =>
    getJSON<HostsPayload>(`/api/hosts${refresh ? "?refresh=1" : ""}`),

  // Switch the active host (the file browser, diff, repo picker, sidebar tree
  // and new-agent target follow it). Bumps term_rev so iframes reload.
  switchHost: (host: string) =>
    postJSON<{ active: string }>("/api/host", { host }),

  // The cross-host pane list (every host's live tabs).
  gridPanes: () => getJSON<GridPayload>("/api/grid"),

  // Ensure a ttyd attached to a grid pane and get its iframe base path.
  gridTerm: (host: string, tab_id: string) =>
    postJSON<{ base: string }>("/api/grid/term", { host, tab_id }),

  // Keepalive so a visible cell's ttyd isn't reaped.
  gridTermTouch: (host: string, tab_id: string) =>
    postJSON<{ alive: boolean }>("/api/grid/term-touch", { host, tab_id }),

  // Tear down a cell's ttyd when it scrolls out of view / the grid is left.
  // keepalive:true so it still fires during page unload.
  gridTermRelease: (host: string, tab_id: string) =>
    fetch("/api/grid/term-release", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ host, tab_id }),
      keepalive: true,
    }).then(() => undefined),

  // Rename a workspace from the grid.
  gridRename: (host: string, workspace_id: string, label: string) =>
    postJSON<{ ok: boolean }>("/api/grid/rename", {
      host,
      workspace_id,
      label,
    }),

  // Close one or more panes (kills their sessions, host-aware).
  gridClose: (panes: { host: string; tab_id: string }[]) =>
    postJSON<{ ok: boolean }>("/api/grid/close", { panes }),

  // Server-persisted UI prefs (grid filters + sidebar collapsed).
  uiState: () => getJSON<UIState>("/api/ui-state"),
  saveUIState: (s: UIState) => postJSON<UIState>("/api/ui-state", s),
}
