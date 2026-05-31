import * as React from "react"
import { Pill } from "@/components/Pill"
import { Button } from "@/components/ui/button"
import { api, type DiffFileMeta, type DiffPayload } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { type DiffLine, parseDiff } from "@/lib/diff"
import { cn } from "@/lib/utils"

// The Diff tab. It always follows herdr's active pane and auto-picks the diff
// mode (working tree when dirty, branch-vs-primary when clean). The complete
// changed-file list + per-file counts come from the metadata endpoint (never
// byte-capped); each file's line-by-line diff is fetched lazily on expand. The
// list polls every 2.5s while visible so it tracks edits/commits/branch
// switches with no event.
export function DiffView({
  active,
  viewerOpen,
  onDirty,
}: {
  active: boolean
  viewerOpen: boolean
  onDirty: (n: number) => void
}) {
  const { activeCwd } = useApp()
  const [data, setData] = React.useState<DiffPayload | null>(null)
  const [error, setError] = React.useState<string | null>(null)
  const [collapsed, setCollapsed] = React.useState<Set<string>>(new Set())
  const [allCollapsed, setAllCollapsed] = React.useState(true)

  const seenRef = React.useRef<Set<string>>(new Set())
  const sigRef = React.useRef<string | null>(null)
  const baseRef = React.useRef<string | null>(null)
  const loadingRef = React.useRef(false)
  const renderedRef = React.useRef(false)

  // onDirty is the parent's setState (stable identity), so the callback can use
  // it directly without a "latest ref" written during render.
  const load = React.useCallback(async () => {
    const base = activeCwd
    if (!base) {
      setData(null)
      setError(null)
      return
    }
    if (loadingRef.current) return
    loadingRef.current = true
    if (base !== baseRef.current) {
      baseRef.current = base
      sigRef.current = null // force a fresh render when the repo changes
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
      if (sig === sigRef.current && renderedRef.current) return // no-op: keep state
      sigRef.current = sig
      const fresh = files
        .map((f) => f.path)
        .filter((p) => !seenRef.current.has(p))
      for (const p of fresh) seenRef.current.add(p)
      if (fresh.length) setCollapsed((prev) => new Set([...prev, ...fresh]))
      setError(null)
      setData(d)
      renderedRef.current = true
    } catch (e) {
      sigRef.current = null
      onDirty(0)
      setError((e as Error).message)
      setData(null)
    } finally {
      loadingRef.current = false
    }
  }, [activeCwd, onDirty])

  // Load + poll while the tab is visible and the file viewer isn't covering it.
  React.useEffect(() => {
    if (!active) return
    load()
    const t = setInterval(() => {
      if (active && !document.hidden && !viewerOpen) load()
    }, 2500)
    const onVis = () => {
      if (!document.hidden && active) load()
    }
    document.addEventListener("visibilitychange", onVis)
    return () => {
      clearInterval(t)
      document.removeEventListener("visibilitychange", onVis)
    }
  }, [active, viewerOpen, load])

  const files = data?.files ?? []

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
            {activeCwd || "—"}
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
            className="ml-auto h-6 rounded-full px-3 font-normal text-[11px]"
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
            {data.isBranchDiff
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
    <div className="dfile">
      <div className="dfhead" onClick={onToggle}>
        <span className="caret">{collapsed ? "▸" : "▾"}</span>
        <span className="dfname">{file.path || "(unknown)"}</span>
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
