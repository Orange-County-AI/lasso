import * as React from "react"
import { DiffTab } from "@/components/DiffTab"
import { FilesTab, type FileChange } from "@/components/FilesTab"
import { api, type DiffPayload } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { cn } from "@/lib/utils"

// The file viewer pulls in CodeMirror + react-markdown; load it only on first
// file open so the initial page stays light.
const FileViewer = React.lazy(() =>
  import("@/components/FileViewer").then((m) => ({ default: m.FileViewer }))
)

type SubView = "files" | "diff"

// Map a raw git status to the change category the tree hints at. Deleted files
// don't appear in the tree, but we still classify them for completeness.
function classify(status: string): FileChange {
  switch (status) {
    case "added":
      return "added"
    case "untracked":
      return "untracked"
    case "deleted":
      return "deleted"
    case "renamed":
      return "renamed"
    default:
      return "modified"
  }
}

// FilesPanel merges the former Diff and Files tabs into one. It owns the
// changed-file metadata (fetched + polled here, once) and shares it two ways:
// the Diff subtab renders the line-by-line diff from it, and the Files subtab
// tints each row with the file's change status. The file viewer/editor overlay
// lives here too and opens by default (the Files subtab is active first).
export function FilesPanel({
  active,
  onDirty,
}: {
  active: boolean
  // Surface the uncommitted-change count to the parent so the top-level tab can
  // badge it even when this panel isn't open.
  onDirty: (n: number) => void
}) {
  const { activeCwd } = useApp()
  const [sub, setSub] = React.useState<SubView>("files")
  const [viewerPath, setViewerPath] = React.useState<string | null>(null)
  const [data, setData] = React.useState<DiffPayload | null>(null)
  const [error, setError] = React.useState<string | null>(null)

  const sigRef = React.useRef<string | null>(null)
  const baseRef = React.useRef<string | null>(null)
  const loadingRef = React.useRef(false)

  const load = React.useCallback(async () => {
    const base = activeCwd
    if (!base) {
      setData(null)
      setError(null)
      onDirty(0)
      return
    }
    if (loadingRef.current) return
    loadingRef.current = true
    if (base !== baseRef.current) {
      baseRef.current = base
      sigRef.current = null
    }
    try {
      const d = await api.diff(base)
      const files = d.files || []
      const sig = JSON.stringify([
        d.branch,
        d.baseBranch,
        d.isBranchDiff,
        d.dirty,
        files.map((f) => [f.path, f.status, f.add, f.del]),
      ])
      onDirty(d.dirty || 0)
      if (sig === sigRef.current) return // unchanged — keep state stable
      sigRef.current = sig
      setError(null)
      setData(d)
    } catch (e) {
      sigRef.current = null
      onDirty(0)
      setError((e as Error).message)
      setData(null)
    } finally {
      loadingRef.current = false
    }
  }, [activeCwd, onDirty])

  // Poll while the panel is visible and the viewer isn't covering it. The map
  // feeds both subviews, so we poll regardless of which one is active.
  React.useEffect(() => {
    if (!active) return
    load()
    const t = setInterval(() => {
      if (active && !document.hidden && !viewerPath) load()
    }, 2500)
    const onVis = () => {
      if (!document.hidden && active) load()
    }
    document.addEventListener("visibilitychange", onVis)
    return () => {
      clearInterval(t)
      document.removeEventListener("visibilitychange", onVis)
    }
  }, [active, viewerPath, load])

  // Absolute-path → change status, for the file tree's hints. Diff paths are
  // repo-relative; the tree keys on absolute paths rooted at activeCwd.
  const changes = React.useMemo(() => {
    const m = new Map<string, FileChange>()
    if (!activeCwd || !data?.files) return m
    const root = activeCwd.replace(/\/$/, "")
    for (const f of data.files) m.set(`${root}/${f.path}`, classify(f.status))
    return m
  }, [activeCwd, data])

  const dirty = data?.dirty ?? 0

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex flex-none items-center gap-1 border-border border-b bg-background px-2 py-1">
        <SubTab active={sub === "files"} onClick={() => setSub("files")}>
          Files
        </SubTab>
        <SubTab active={sub === "diff"} onClick={() => setSub("diff")}>
          Diff
          {dirty > 0 && (
            <span
              className="ml-1.5 rounded-full bg-warn px-1.5 font-semibold text-[11px] text-background"
              title={`${dirty} uncommitted change${dirty === 1 ? "" : "s"}`}
            >
              {dirty}
            </span>
          )}
        </SubTab>
      </div>

      <div className="relative min-h-0 flex-1">
        <div
          className={cn(
            "absolute inset-0 flex flex-col",
            sub !== "files" && "hidden"
          )}
        >
          <FilesTab
            viewerPath={viewerPath}
            onOpenFile={setViewerPath}
            changes={changes}
          />
        </div>
        <div
          className={cn(
            "absolute inset-0 flex flex-col",
            sub !== "diff" && "hidden"
          )}
        >
          <DiffTab repoPath={activeCwd} data={data} error={error} />
        </div>

        {viewerPath && (
          <React.Suspense fallback={null}>
            <FileViewer
              path={viewerPath}
              onClose={() => setViewerPath(null)}
            />
          </React.Suspense>
        )}
      </div>
    </div>
  )
}

function SubTab({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "flex items-center rounded px-2 py-0.5 text-[12px] transition-colors",
        active
          ? "bg-accent text-foreground"
          : "text-muted-foreground hover:text-foreground"
      )}
    >
      {children}
    </button>
  )
}
