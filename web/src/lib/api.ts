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
  local: { version: string; protocol: number }
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

export interface VersionInfo {
  installed?: string
  latest?: string
  update_available?: boolean
  latest_error?: string
}

export interface ThemePayload {
  name: string
  resolved: string
  customized: boolean
  css: string
  // xterm.js ITheme — shape is opaque to us; we hand it straight to the iframe.
  xterm: Record<string, unknown>
}

async function getJSON<T>(url: string): Promise<T> {
  const r = await fetch(url)
  if (!r.ok) throw new Error(await r.text())
  return (await r.json()) as T
}

async function postJSON<T>(url: string, body: unknown): Promise<T> {
  const r = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  })
  if (!r.ok) throw new Error(await r.text())
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

  panes: () => getJSON<{ panes?: Pane[] }>("/api/panes"),
  agents: () => getJSON<{ agents?: Agent[] }>("/api/agents"),
  version: () => getJSON<VersionInfo>("/api/version"),

  files: (path: string) =>
    getJSON<DirListing>(`/api/files?path=${encodeURIComponent(path)}`),

  fileURL: (path: string) => `/api/file?path=${encodeURIComponent(path)}`,

  fileText: async (path: string) => {
    const r = await fetch(api.fileURL(path))
    if (!r.ok) throw new Error(await r.text())
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
    if (!r.ok) throw new Error(await r.text())
    return r.json()
  },
}
