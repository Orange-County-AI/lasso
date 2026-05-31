import * as React from "react"
import { toast } from "sonner"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuTrigger,
} from "@/components/ui/context-menu"
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { api, type FileEntry } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { fmtSize } from "@/lib/format"
import { cn } from "@/lib/utils"

// An entry the user has targeted with a context-menu action. `parent` is the
// directory holding it — refreshed after the action so the tree updates.
type Target = { name: string; full: string; dir: boolean; parent: string }

const INDENT = 14 // px added to the row's left padding per nesting level

const join = (dir: string, name: string) => `${dir.replace(/\/$/, "")}/${name}`

// The Files tab: an inline, lazily-loaded directory tree rooted at herdr's
// active pane (by default). Clicking a directory expands/collapses it in place;
// clicking a file opens it in the full-column viewer (owned by the parent so
// its highlight clears on close). Right-clicking an entry offers rename/delete.
export function FilesView({
  viewerPath,
  onOpenFile,
}: {
  viewerPath: string | null
  onOpenFile: (path: string) => void
}) {
  const { activeCwd } = useApp()
  const [curPath, setCurPath] = React.useState<string | null>(null)
  const [follow, setFollow] = React.useState(true)
  const [pathValue, setPathValue] = React.useState("")
  // The canonical root path (as the server cleaned it) and its parent, for the
  // ".." re-root row.
  const [rootPath, setRootPath] = React.useState<string | null>(null)
  const [rootParent, setRootParent] = React.useState<string | null>(null)
  // Per-directory lazy state, all keyed by absolute path.
  const [expanded, setExpanded] = React.useState<Set<string>>(new Set())
  const [childrenByPath, setChildrenByPath] = React.useState<
    Record<string, FileEntry[]>
  >({})
  const [errorByPath, setErrorByPath] = React.useState<Record<string, string>>(
    {}
  )
  const [renameTarget, setRenameTarget] = React.useState<Target | null>(null)
  const [renameValue, setRenameValue] = React.useState("")
  const [deleteTarget, setDeleteTarget] = React.useState<Target | null>(null)
  const inputRef = React.useRef<HTMLInputElement>(null)

  // Follow the active pane's cwd while "follow" is on.
  React.useEffect(() => {
    if (follow && activeCwd && activeCwd !== curPath) setCurPath(activeCwd)
  }, [follow, activeCwd, curPath])

  // (Re)load the root whenever it changes — collapse everything and refetch.
  React.useEffect(() => {
    if (!curPath) return
    let cancelled = false
    setExpanded(new Set())
    setChildrenByPath({})
    setErrorByPath({})
    setRootPath(null)
    api
      .files(curPath)
      .then((data) => {
        if (cancelled) return
        setRootPath(data.path)
        setRootParent(
          data.parent && data.parent !== data.path ? data.parent : null
        )
        setChildrenByPath({ [data.path]: data.entries })
        if (document.activeElement !== inputRef.current) setPathValue(data.path)
      })
      .catch((e: Error) => {
        if (cancelled) return
        setRootPath(curPath)
        setRootParent(null)
        setErrorByPath({ [curPath]: e.message })
      })
    return () => {
      cancelled = true
    }
  }, [curPath])

  // Fetch a directory's children into the cache (used on expand and refresh).
  const loadDir = React.useCallback(async (dir: string) => {
    try {
      const data = await api.files(dir)
      setChildrenByPath((prev) => ({ ...prev, [dir]: data.entries }))
      setErrorByPath((prev) => {
        if (!(dir in prev)) return prev
        const next = { ...prev }
        delete next[dir]
        return next
      })
    } catch (e) {
      setErrorByPath((prev) => ({ ...prev, [dir]: (e as Error).message }))
    }
  }, [])

  const toggleDir = (full: string) => {
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(full)) {
        next.delete(full)
      } else {
        next.add(full)
        if (!(full in childrenByPath) && !(full in errorByPath))
          void loadDir(full)
      }
      return next
    })
  }

  // Re-root the tree (the ".." row and the path input). This is the user
  // steering, so stop following the active pane.
  const navigate = (path: string) => {
    setFollow(false)
    setCurPath(path)
  }

  const onPathKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    const v = e.currentTarget.value.trim()
    if (e.key === "Enter" && v) navigate(v)
  }

  // Drop a path and any of its descendants from the expanded set (after the
  // entry is renamed or deleted).
  const pruneExpanded = (prefix: string) =>
    setExpanded((prev) => {
      const next = new Set<string>()
      for (const p of prev)
        if (p !== prefix && !p.startsWith(`${prefix}/`)) next.add(p)
      return next
    })

  const submitRename = async () => {
    if (!renameTarget) return
    const name = renameValue.trim()
    if (!name || name === renameTarget.name) {
      setRenameTarget(null)
      return
    }
    try {
      await api.renameFile(renameTarget.full, name)
      pruneExpanded(renameTarget.full)
      setRenameTarget(null)
      void loadDir(renameTarget.parent)
    } catch (e) {
      toast.error((e as Error).message)
    }
  }

  const doDelete = async () => {
    if (!deleteTarget) return
    try {
      await api.deleteFile(deleteTarget.full)
      pruneExpanded(deleteTarget.full)
      setDeleteTarget(null)
      void loadDir(deleteTarget.parent)
    } catch (e) {
      toast.error((e as Error).message)
    }
  }

  // Recursively render a directory's entries. `dir` is absolute; `depth` drives
  // indentation. Children render only for expanded directories.
  const renderDir = (dir: string, depth: number): React.ReactNode => {
    const err = errorByPath[dir]
    if (err) return <Note depth={depth}>{err}</Note>
    const entries = childrenByPath[dir]
    if (!entries) return <Note depth={depth}>loading…</Note>
    if (entries.length === 0) return <Note depth={depth}>(empty)</Note>
    return entries.map((e) => {
      const full = join(dir, e.name)
      const open = e.dir && expanded.has(full)
      return (
        <React.Fragment key={e.name}>
          <FileRow
            name={e.name}
            dir={e.dir}
            size={e.size}
            depth={depth}
            expanded={open}
            selected={full === viewerPath}
            onClick={() => (e.dir ? toggleDir(full) : onOpenFile(full))}
            onRename={() => {
              setRenameTarget({ name: e.name, full, dir: e.dir, parent: dir })
              setRenameValue(e.name)
            }}
            onDelete={() =>
              setDeleteTarget({ name: e.name, full, dir: e.dir, parent: dir })
            }
          />
          {open && renderDir(full, depth + 1)}
        </React.Fragment>
      )
    })
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <header className="flex items-center gap-2 border-border border-b bg-background px-3 py-2">
        <Input
          ref={inputRef}
          value={pathValue}
          spellCheck={false}
          autoComplete="off"
          placeholder="go to path…  (Enter)"
          className="h-7 flex-1 text-xs"
          onChange={(e) => {
            setPathValue(e.target.value)
            setFollow(false) // editing the path means the user is steering
          }}
          onKeyDown={onPathKeyDown}
        />
        <label className="flex cursor-pointer items-center gap-1.5 whitespace-nowrap text-[11px] text-muted-foreground">
          <Checkbox
            checked={follow}
            onCheckedChange={(v) => setFollow(v === true)}
          />
          follow active pane
        </label>
      </header>

      <div className="filelist">
        {!curPath ? (
          <div className="empty">waiting for herdr…</div>
        ) : !rootPath ? (
          <div className="empty">loading…</div>
        ) : (
          <>
            {rootParent && (
              <FileRow
                name=".."
                dir
                isUp
                depth={0}
                onClick={() => navigate(rootParent)}
              />
            )}
            {renderDir(rootPath, 0)}
          </>
        )}
      </div>

      {/* rename — replaces window.prompt */}
      <Dialog
        open={renameTarget != null}
        onOpenChange={(o) => !o && setRenameTarget(null)}
      >
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>
              Rename {renameTarget?.dir ? "folder" : "file"}
            </DialogTitle>
          </DialogHeader>
          <Input
            autoFocus
            value={renameValue}
            spellCheck={false}
            autoComplete="off"
            onChange={(e) => setRenameValue(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") submitRename()
            }}
          />
          <DialogFooter>
            <Button variant="outline" onClick={() => setRenameTarget(null)}>
              Cancel
            </Button>
            <Button onClick={submitRename}>Rename</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* delete confirmation — replaces window.confirm */}
      <AlertDialog
        open={deleteTarget != null}
        onOpenChange={(o) => !o && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Delete {deleteTarget?.dir ? "folder" : "file"} “
              {deleteTarget?.name}”?
            </AlertDialogTitle>
            <AlertDialogDescription className="min-w-0">
              <span className="block break-all font-mono text-xs">
                {deleteTarget?.full}
              </span>
              <span className="mt-3 block">
                {deleteTarget?.dir
                  ? "This permanently removes the folder and everything inside it."
                  : "This permanently removes the file."}{" "}
                It cannot be undone.
              </span>
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={doDelete}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

// A muted, depth-indented status line (loading / empty / error) shown in place
// of a directory's children.
function Note({
  depth,
  children,
}: {
  depth: number
  children: React.ReactNode
}) {
  return (
    <div className="empty" style={{ paddingLeft: 12 + depth * INDENT }}>
      {children}
    </div>
  )
}

function FileRow({
  name,
  dir,
  size,
  depth,
  expanded,
  isUp,
  selected,
  onClick,
  onRename,
  onDelete,
}: {
  name: string
  dir: boolean
  size?: number
  depth: number
  expanded?: boolean
  isUp?: boolean
  selected?: boolean
  onClick: () => void
  onRename?: () => void
  onDelete?: () => void
}) {
  const row = (
    <div
      className={cn("entry", dir ? "d" : "f", selected && "sel")}
      style={{ paddingLeft: 12 + depth * INDENT }}
      onClick={onClick}
    >
      <span className="ico">
        {dir ? (isUp ? "↑" : expanded ? "▾" : "▸") : "·"}
      </span>
      <span className="nm">{name}</span>
      {!dir && <span className="sz">{fmtSize(size)}</span>}
    </div>
  )

  // The parent ("..") row gets no menu — there's nothing to act on.
  if (!onRename && !onDelete) return row

  return (
    <ContextMenu>
      <ContextMenuTrigger asChild>{row}</ContextMenuTrigger>
      <ContextMenuContent>
        {onRename && (
          <ContextMenuItem onSelect={onRename}>Rename…</ContextMenuItem>
        )}
        {onDelete && (
          <ContextMenuItem variant="destructive" onSelect={onDelete}>
            Delete
          </ContextMenuItem>
        )}
      </ContextMenuContent>
    </ContextMenu>
  )
}
