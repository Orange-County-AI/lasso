import * as React from "react"
import { Pill } from "@/components/Pill"
import { Button } from "@/components/ui/button"
import { api, type DiffFileMeta, type DiffPayload } from "@/lib/api"
import { type DiffLine, parseDiff } from "@/lib/diff"
import { cn } from "@/lib/utils"

// Reduce the shared (absolute) path input to a repo-relative prefix used to
// filter the diff. Diff file paths are relative to repoPath, so a path that is
// repoPath itself (or empty, or outside the repo) means "no filter".
function relFilter(pathValue: string, repoPath: string | null): string {
  if (!pathValue || !repoPath) return ""
  const root = repoPath.replace(/\/$/, "")
  if (pathValue === root) return ""
  if (!pathValue.startsWith(`${root}/`)) return ""
  return pathValue.slice(root.length + 1).replace(/\/$/, "")
}

// The Diff view (a subtab of Files). The changed-file metadata is fetched and
// polled by the parent FilesPanel — which shares it with the file tree's
// change hints — and handed down here as `data`. This component owns only the
// presentation: collapse state plus the lazy per-file line diff fetched on
// expand. The complete file list + per-file counts come from the metadata
// endpoint (never byte-capped).
export function DiffTab({
  repoPath,
  data,
  error,
  pathValue,
}: {
  repoPath: string | null
  data: DiffPayload | null
  error: string | null
  // The path the file viewer is focused on (owned by FilesPanel, set from the
  // Files subtab's input). The diff is read-only here: it just filters to this
  // path — the diff view has no input of its own.
  pathValue: string
}) {
  const activeCwd = repoPath
  const [collapsed, setCollapsed] = React.useState<Set<string>>(new Set())
  const [allCollapsed, setAllCollapsed] = React.useState(true)
  const seenRef = React.useRef<Set<string>>(new Set())

  const allFiles = data?.files ?? []
  // Restrict to the focused path when one is set: an exact file match, or any
  // file beneath a directory prefix. An empty/repo-root path shows everything.
  const filter = relFilter(pathValue, repoPath)
  const files = filter
    ? allFiles.filter(
        (f) => f.path === filter || f.path.startsWith(`${filter}/`)
      )
    : allFiles
  const fileSig = files.map((f) => f.path).join("\n")

  // Reset collapse tracking when the repo changes.
  // biome-ignore lint/correctness/useExhaustiveDependencies: repoPath is the trigger
  React.useEffect(() => {
    seenRef.current = new Set()
    setCollapsed(new Set())
    setAllCollapsed(true)
  }, [repoPath])

  // Newly-appearing files start collapsed (the lazy split means a collapsed
  // file costs nothing). fileSig is the trigger; the paths are read from the
  // live `files` inside.
  // biome-ignore lint/correctness/useExhaustiveDependencies: fileSig captures files
  React.useEffect(() => {
    const fresh = files
      .map((f) => f.path)
      .filter((p) => !seenRef.current.has(p))
    if (!fresh.length) return
    for (const p of fresh) seenRef.current.add(p)
    setCollapsed((prev) => new Set([...prev, ...fresh]))
  }, [fileSig])

  const toggleAll = () => {
    const next = !allCollapsed
    setAllCollapsed(next)
    setCollapsed(next ? new Set(files.map((f) => f.path)) : new Set())
  }

  const toggleFile = (path: string) => {
    setCollapsed((prev) => {
      const n = new Set(prev)
      if (n.has(path)) n.delete(path)
      else n.add(path)
      return n
    })
  }

  let add = 0
  let del = 0
  for (const f of files) {
    add += f.add
    del += f.del
  }
  const mode: "branch" | "working" = data?.isBranchDiff ? "branch" : "working"

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <header className="border-border border-b bg-background px-3 py-2">
        <div className="flex flex-wrap items-center gap-2">
          <Pill tone="accent" multiline>
            {pathValue || activeCwd || "—"}
          </Pill>
          {data && (
            <Pill tone={data.dirty ? "warn" : "good"}>
              {data.dirty ? `${data.dirty} dirty` : "clean"}
            </Pill>
          )}
          {data?.isBranchDiff && data.baseBranch && (
            <Pill>vs {data.baseBranch}</Pill>
          )}
          {data?.isBranchDiff && files.length > 0 && (
            <Pill>
              {files.length} {files.length === 1 ? "file" : "files"}
            </Pill>
          )}
          {add > 0 && <Pill tone="good">+{add}</Pill>}
          {del > 0 && <Pill tone="bad">−{del}</Pill>}
          <Button
            variant="outline"
            size="sm"
            className="ml-auto h-6 rounded-full px-3 font-normal text-[13px]"
            onClick={toggleAll}
          >
            {allCollapsed ? "expand all" : "collapse all"}
          </Button>
        </div>
      </header>

      <div className="difflist wrap">
        {error ? (
          <div className="empty">
            cannot diff this directory
            <br />
            {error}
          </div>
        ) : !activeCwd ? (
          <div className="empty">no active directory yet</div>
        ) : !data ? (
          <div className="empty">loading diff…</div>
        ) : files.length === 0 ? (
          <div className="empty">
            {filter && allFiles.length > 0
              ? `no changes under “${filter}”`
              : data.isBranchDiff
                ? data.baseBranch
                  ? `no changes vs ${data.baseBranch}`
                  : "no base branch to compare against"
                : `no changes${data.branch ? ` on ${data.branch}` : ""}`}
          </div>
        ) : (
          files.map((f) => (
            <DiffFileBlock
              key={f.path}
              repoPath={activeCwd}
              file={f}
              mode={mode}
              baseBranch={data.baseBranch}
              collapsed={collapsed.has(f.path)}
              onToggle={() => toggleFile(f.path)}
            />
          ))
        )}
      </div>
    </div>
  )
}

type Body =
  | { state: "loading" }
  | { state: "error"; message: string }
  | { state: "ready"; lines: DiffLine[]; truncated: boolean }

function DiffFileBlock({
  repoPath,
  file,
  mode,
  baseBranch,
  collapsed,
  onToggle,
}: {
  repoPath: string
  file: DiffFileMeta
  mode: "branch" | "working"
  baseBranch?: string
  collapsed: boolean
  onToggle: () => void
}) {
  const [body, setBody] = React.useState<Body | null>(null)

  // Added/removed files get a colored accent + label so they stand out from
  // ordinary edits. "untracked" is a brand-new file git hasn't staged yet.
  const kind =
    file.status === "added" || file.status === "untracked"
      ? "added"
      : file.status === "deleted"
        ? "deleted"
        : file.status === "renamed"
          ? "renamed"
          : null
  const label = file.status === "untracked" ? "new" : kind

  // Fetch this file's diff only while it's expanded; refetch when its content
  // changes (add/del move) or the comparison changes. Collapsed files cost
  // nothing — that's the whole point of the lazy split.
  React.useEffect(() => {
    if (collapsed) return
    let cancelled = false
    setBody({ state: "loading" })
    api
      .diffFile(repoPath, file.path, mode, baseBranch)
      .then((r) => {
        if (cancelled) return
        const parsed = parseDiff(r.diff)
        const lines = parsed.length ? parsed[0].lines : []
        setBody({ state: "ready", lines, truncated: r.truncated })
      })
      .catch((e: Error) => {
        if (!cancelled) setBody({ state: "error", message: e.message })
      })
    return () => {
      cancelled = true
    }
    // file.add/file.del in deps so a polled content change refetches an open file.
  }, [collapsed, repoPath, file.path, mode, baseBranch])

  return (
    <div className={cn("dfile", kind && `dfile-${kind}`)}>
      <div className="dfhead" onClick={onToggle}>
        <span className="caret">{collapsed ? "▸" : "▾"}</span>
        <span className="dfname">{file.path || "(unknown)"}</span>
        {label && (
          <span className={cn("dfbadge", `dfbadge-${kind}`)}>{label}</span>
        )}
        <span className="dfstat">
          <span className="add">+{file.add}</span>{" "}
          <span className="del">−{file.del}</span>
        </span>
      </div>
      {!collapsed && (
        <div className="dfbody">
          {!body || body.state === "loading" ? (
            <div className="dline ctx">
              <span className="sign" />
              <span className="txt">loading…</span>
            </div>
          ) : body.state === "error" ? (
            <div className="dline ctx">
              <span className="sign" />
              <span className="txt">error: {body.message}</span>
            </div>
          ) : body.lines.length === 0 ? (
            <div className="dline ctx">
              <span className="sign" />
              <span className="txt">(no textual changes)</span>
            </div>
          ) : (
            <>
              {body.lines.map((ln, i) => (
                <div key={i} className={cn("dline", ln.t)}>
                  <span className="sign">
                    {ln.t === "add" ? "+" : ln.t === "del" ? "-" : ""}
                  </span>
                  <span className="txt">{ln.s}</span>
                </div>
              ))}
              {body.truncated && (
                <div className="dline meta">
                  <span className="sign" />
                  <span className="txt">… diff truncated (file too large)</span>
                </div>
              )}
            </>
          )}
        </div>
      )}
    </div>
  )
}
