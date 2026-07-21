import * as React from "react"
import { DiffTab } from "@/components/DiffTab"
import { type FileChange, FilesTab } from "@/components/FilesTab"
import { useApp } from "@/lib/app-store"
import { useDiff } from "@/lib/git"
import { usePaneFocusPending } from "@/lib/pane-focus"
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
export function FilesPanel() {
  const { activeCwd } = useApp()
  const [sub, setSub] = React.useState<SubView>("files")
  const [viewerPath, setViewerPath] = React.useState<string | null>(null)
  // A pane focus (possibly a multi-second cross-host switch) is in flight —
  // this panel follows the focused pane's cwd, so veil the stale content with
  // a loading state until the switch lands rather than looking desynchronized.
  const focusing = usePaneFocusPending()

  // The changed-file metadata is fetched + polled app-wide via the shared
  // useDiff query (so the top-level tab badge stays live even while this panel
  // is hidden); here we just read it. react-query's structural sharing keeps the
  // `data` reference stable across polls when nothing changed.
  const diff = useDiff()
  const data = diff.data ?? null
  const error = diff.error ? (diff.error as Error).message : null

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
          {/* Git status shown by tinting the label itself (bold, underlined
              gold when dirty, theme "good" when clean) instead of a separate
              count badge. */}
          <span
            className={cn(
              data != null &&
                (dirty > 0 ? "font-semibold text-warn underline" : "text-good")
            )}
          >
            Diff
          </span>
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
            <FileViewer path={viewerPath} onClose={() => setViewerPath(null)} />
          </React.Suspense>
        )}

        {focusing && (
          <div className="absolute inset-0 z-10 flex items-center justify-center gap-2 bg-background/70 text-muted-foreground text-xs">
            <span
              className="termcell-spinner"
              role="status"
              aria-label="loading"
            />
            following pane…
          </div>
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
