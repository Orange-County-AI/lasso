import { Upload } from "lucide-react"
import * as React from "react"
import { toast } from "sonner"
import { DiffTab } from "@/components/DiffTab"
import { ErrorBoundary } from "@/components/ErrorBoundary"
import { FileViewer } from "@/components/FileViewer"
import {
  type FileChange,
  FilesTab,
  type FilesTabState,
} from "@/components/FilesTab"
import { Orb } from "@/components/ui/orb"
import { useApp } from "@/lib/app-store"
import { useDiff } from "@/lib/git"
import {
  baseName,
  type OpenFileRequest,
  onOpenFileRequest,
  revealFiles,
} from "@/lib/open-file"
import { usePaneFocusPending } from "@/lib/pane-focus"
import { cn } from "@/lib/utils"

type SubView = "files" | "diff"

// The file the viewer has open, and the host it was opened on. `line` and
// `seq` are set only by an agent's open_file: the line to scroll to, and a
// counter so the same line asked for twice still re-scrolls.
type ViewerTarget = {
  path: string
  host: string | null
  line?: number | null
  seq?: number
}

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
  // FilesTab owns the tree's root and the upload call; the button below just
  // picks files and hands them over.
  const uploadRef = React.useRef<((files: File[]) => void) | null>(null)
  const uploadInput = React.useRef<HTMLInputElement>(null)
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
  // Bumped to remount the Files tab onto a state written from outside — an
  // agent opening a directory re-roots the tree there (see openRequested).
  const [tabGen, setTabGen] = React.useState(0)

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

  // An agent asked to show the human a file (lib/open-file.ts — already gated
  // on this tab being visible). It lands in the CURRENT pane's slot, like a
  // click in the tree would. What it must never do is throw away unsaved
  // edits: when the open editor is dirty and the request would replace it, the
  // human gets a toast offering to open it instead, and taking that offer goes
  // through the same discard confirmation the viewer's close button asks.
  // Re-opening the file already on screen replaces nothing, so it just scrolls.
  // Read through a ref so the subscription below is made once, yet always acts
  // on the pane and viewer showing at the moment the event (or the toast's
  // Open) arrives.
  const openRequested = React.useRef<
    (req: OpenFileRequest, offered?: boolean) => void
  >(() => {})
  openRequested.current = (req, offered = false) => {
    const current = viewers.get(pane) ?? null
    const same =
      current != null &&
      current.path === req.path &&
      (current.host ?? null) === req.host
    // A directory shows the tree, which means closing any open viewer.
    const replaces = req.dir ? current != null : current != null && !same
    const unsaved = replaces && drafts.current.has(pane)
    const name = baseName(req.path)
    if (unsaved && !offered) {
      toast.info(`${req.from} wants to open ${name}`, {
        id: `open-file:${req.path}`,
        description: "You have unsaved changes open, so it was not replaced.",
        duration: 20000,
        action: {
          label: "Open",
          onClick: () => openRequested.current(req, true),
        },
      })
      return
    }
    if (unsaved && !window.confirm("Discard unsaved changes?")) return
    setSub("files")
    revealFiles()
    if (req.dir) {
      // Rooted where the agent pointed and NOT following the focused pane —
      // the same frozen state a human navigating there by hand leaves, or the
      // follow effect would snap it straight back to the pane's cwd.
      remember(tabStates.current, pane, {
        path: req.path,
        host: req.host,
        follow: false,
        pathValue: req.path,
        expanded: [],
      })
      setTabGen((g) => g + 1)
      setViewer(null)
    } else {
      if (replaces) drafts.current.delete(pane)
      setViewer({
        path: req.path,
        host: req.host,
        line: req.line ?? null,
        seq: req.seq,
      })
    }
    toast.info(`${req.from} opened ${name}`, { description: req.path })
  }
  React.useEffect(
    () => onOpenFileRequest((req) => openRequested.current(req)),
    []
  )

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
        {sub === "files" && (
          <>
            <button
              type="button"
              title="Upload files to the open directory"
              aria-label="Upload files"
              className="ml-auto flex items-center rounded p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
              onClick={() => {
                if (!uploadRef.current) {
                  toast.error("No directory is open yet")
                  return
                }
                uploadInput.current?.click()
              }}
            >
              <Upload className="size-3.5" />
            </button>
            <input
              ref={uploadInput}
              type="file"
              multiple
              hidden
              onChange={(e) => {
                const files = Array.from(e.target.files ?? [])
                // Cleared so picking the same file again still fires onChange.
                e.target.value = ""
                if (files.length > 0) uploadRef.current?.(files)
              }}
            />
          </>
        )}
      </div>

      <div className="relative min-h-0 flex-1">
        <div
          className={cn(
            "absolute inset-0 flex flex-col",
            sub !== "files" && "hidden"
          )}
        >
          <FilesTab
            key={`${pane}\u0000${tabGen}`}
            viewerPath={viewer?.path ?? null}
            onOpenFile={(path, host) => setViewer({ path, host })}
            changes={changes}
            host={cwdHost}
            initial={tabStates.current.get(pane) ?? null}
            onStateChange={saveTabState}
            uploadRef={uploadRef}
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
          // Keyed on the pane so browsing elsewhere clears a failure rather
          // than carrying it to the next file. The viewer is imported
          // statically — see the note in FileViewer.tsx — so what this catches
          // is a render error from the file itself (a mermaid fence, a preview
          // that throws), never a missing chunk.
          <ErrorBoundary
            key={pane}
            label="file viewer"
            fallback={(_err, retry) => (
              <div className="vsurface absolute inset-0 z-20 flex flex-col items-center justify-center gap-2 bg-background text-muted-foreground text-xs">
                <div>the file viewer hit an error.</div>
                <div className="flex gap-2">
                  <button
                    type="button"
                    className="rounded border border-border px-2 py-1 hover:bg-accent"
                    onClick={() => window.location.reload()}
                  >
                    reload lasso
                  </button>
                  <button
                    type="button"
                    className="rounded border border-border px-2 py-1 hover:bg-accent"
                    onClick={retry}
                  >
                    try again
                  </button>
                  <button
                    type="button"
                    className="rounded border border-border px-2 py-1 hover:bg-accent"
                    onClick={() => setViewer(null)}
                  >
                    close
                  </button>
                </div>
              </div>
            )}
          >
            <FileViewer
              key={pane}
              path={viewer.path}
              host={viewer.host}
              line={viewer.line ?? null}
              lineSeq={viewer.seq ?? 0}
              initialDraft={initialDraft}
              onDraftChange={saveDraft}
              onClose={() => setViewer(null)}
            />
          </ErrorBoundary>
        )}

        {focusing && (
          <div className="absolute inset-0 z-10 flex items-center justify-center gap-2 bg-background/70 text-muted-foreground text-xs">
            <Orb state="working" px={16} label="following pane" />
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
