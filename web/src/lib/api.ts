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
  // Authoritative top-level display order of the unified "spaces" list as stable
  // keys ("ws:<id>" for scratch, "repo:<path>" for repos). Rows absent from it
  // (e.g. just-created) are rendered at the bottom by the sidebar.
  order: string[]
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
  tree: () => getJSON<TreePayload>("/api/tree"),
  agentsList: () => getJSON<{ agents: AgentRow[] }>("/api/agents"),

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
  // full current key list in its new order).
  reorderSpaces: (order: string[]) =>
    postJSON<{ ok: boolean }>("/api/spaces/reorder", { order }),
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
}
