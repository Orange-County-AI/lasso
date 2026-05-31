// Typed wrappers around lasso's Go HTTP API. Every endpoint the original
// index.html called via fetch() lives here, so components never build URLs by
// hand. Paths are same-origin (the Go server, or Vite's dev proxy onto it).

export interface ActiveState {
  cwd?: string
  pane_id?: string
  panes_rev?: number
  theme_rev?: number
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
  panes: () => getJSON<{ panes?: Pane[] }>("/api/panes"),
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

  close: (pane_ids: string[]) =>
    postJSON<{ closed?: string[]; errors?: Record<string, string> }>(
      "/api/close",
      { pane_ids }
    ),

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
