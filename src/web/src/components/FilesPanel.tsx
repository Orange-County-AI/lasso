import * as React from "react"
import { DiffTab } from "@/components/DiffTab"
import {
  type FileChange,
  FilesTab,
  type FilesTabState,
} from "@/components/FilesTab"
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

// The file the viewer has open, and the host it was opened on.
type ViewerTarget = { path: string; host: string | null }

// Per-pane state is kept only for the panes actually browsed, and capped so a
// long session across many agents can't grow without bound. Least-recently
// written goes first: a Map iterates in insertion order and every write
// re-inserts.
const PANE_CAP = 32
function remember<T>(m: Map<string, T>, key: string, val: T) {
  m.delete(key)
  m.set(key, val)
  for (const k of m.keys()) {
    if (m.size <= PANE_CAP) break
    m.delete(k)
  }
}

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
  const { activeCwd, cwdHost, activePaneID, host } = useApp()
  const [sub, setSub] = React.useState<SubView>("files")
  // The sidebar's file state belongs to the pane it was browsed in. Selecting
  // another agent (or workspace) in herdr swaps in THAT pane's open file, tree
  // root, browsed host and expansion, and switching back brings this one's
  // straight back. It used to be global: a newly selected agent's terminal sat
  // beside the previous agent's open file, and the tree only caught up at all
  // because "follow" happened to be on. Kept in memory, not persisted — it is
  // where you were this session, and pane ids don't outlive herdr anyway.
  // Keyed by host as well as pane id: ids are unique only within one herdr
  // session, so two hosts' panes would otherwise share a slot and hand each
  // other's file (on the wrong machine) to the viewer.
  const pane = `${host ?? ""}\u0000${activePaneID ?? ""}`
  // The open viewer per pane. Its file and host are captured together: the tree
  // can follow focus onto another host while the viewer is open, and an open
  // editor must keep reading — and saving — on the host it opened on rather
  // than wherever focus has since moved.
  const [viewers, setViewers] = React.useState<Map<string, ViewerTarget>>(
    () => new Map()
  )
  const viewer = viewers.get(pane) ?? null
  // The Files tab's own state, handed back to it on remount (it is keyed by
  // pane, so a switch remounts it). A ref, not state: nothing here renders it,
  // and it changes on every keystroke in the path box.
  const tabStates = React.useRef(new Map<string, FilesTabState>())
  // Unsaved editor buffers. Held here so a pane switch — which unmounts the
  // viewer — can't silently discard work that closing would have confirmed.
  // Uncapped, unlike the maps above: an entry exists only while that pane's
  // editor is actually dirty, and dropping one loses real work.
  const drafts = React.useRef(new Map<string, { path: string; text: string }>())
  const openPath = viewer?.path ?? null

  const saveTabState = React.useCallback(
    (s: FilesTabState) => remember(tabStates.current, pane, s),
    [pane]
  )
  const setViewer = React.useCallback(
    (v: ViewerTarget | null) => {
      if (!v) drafts.current.delete(pane)
      setViewers((prev) => {
        const next = new Map(prev)
        if (v) remember(next, pane, v)
        else next.delete(pane)
        return next
      })
    },
    [pane]
  )
  const saveDraft = React.useCallback(
    (text: string | null) => {
      if (text == null || openPath == null) drafts.current.delete(pane)
      else drafts.current.set(pane, { path: openPath, text })
    },
    [pane, openPath]
  )
  // Only hand a buffer back for the file it was actually typed into.
  const carried = drafts.current.get(pane)
  const initialDraft =
    openPath && carried?.path === openPath ? carried.text : null
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
            key={pane}
            viewerPath={viewer?.path ?? null}
            onOpenFile={(path, host) => setViewer({ path, host })}
            changes={changes}
            host={cwdHost}
            initial={tabStates.current.get(pane) ?? null}
            onStateChange={saveTabState}
          />
        </div>
        <div
          className={cn(
            "absolute inset-0 flex flex-col",
            sub !== "diff" && "hidden"
          )}
        >
          <DiffTab
            repoPath={activeCwd}
            host={cwdHost}
            data={data}
            error={error}
          />
        </div>

        {viewer && (
          <React.Suspense fallback={null}>
            <FileViewer
              key={pane}
              path={viewer.path}
              host={viewer.host}
              initialDraft={initialDraft}
              onDraftChange={saveDraft}
              onClose={() => setViewer(null)}
            />
          </React.Suspense>
        )}

        {focusing && (
          <div className="absolute inset-0 z-10 flex items-center justify-center gap-2 bg-background/70 text-muted-foreground text-xs">
            <span className="spinner" role="status" aria-label="loading" />
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
