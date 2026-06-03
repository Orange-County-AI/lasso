import { EditorView } from "@codemirror/view"
import CodeMirror from "@uiw/react-codemirror"
import { Eye, Pencil, Save, X } from "lucide-react"
import * as React from "react"
import ReactMarkdown from "react-markdown"
import rehypeHighlight from "rehype-highlight"
import remarkGfm from "remark-gfm"
import { Button } from "@/components/ui/button"
import { api } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import {
  changedLinesHighlight,
  editorTheme,
  languageExtension,
} from "@/lib/codemirror"
import { changedNewLines } from "@/lib/diff"
import { isImage, isMarkdown, isPdf } from "@/lib/format"
import { useDiff } from "@/lib/git"

// Above this size we skip the language extension (and its parsing cost) but
// still open the file in the editor.
const HILITE_CAP = 400 * 1024

// The full-column file editor overlay: images stay view-only (click-to-zoom
// checkerboard), everything else opens in an editable textarea. Edits are only
// persisted on an explicit save (the Save button or ⌘/Ctrl+S); closing with
// unsaved changes prompts for confirmation. Markdown can toggle between the
// raw editor and a rendered preview.
export function FileViewer({
  path,
  onClose,
}: {
  path: string
  onClose: () => void
}) {
  const image = isImage(path)
  const pdf = isPdf(path)
  const markdown = isMarkdown(path)
  // Binary previews (images, PDFs) render straight from the file URL — no text
  // is fetched and there's nothing to edit or save.
  const binary = image || pdf

  // `text` is the last-saved content; `draft` is what's in the editor. They
  // diverge exactly when there are unsaved edits.
  const [text, setText] = React.useState<string | null>(null)
  const [draft, setDraft] = React.useState<string | null>(null)
  const [error, setError] = React.useState<string | null>(null)
  const [saving, setSaving] = React.useState(false)
  const [saveError, setSaveError] = React.useState<string | null>(null)
  // Markdown opens rendered; toggle into the raw editor to make changes.
  const [preview, setPreview] = React.useState(markdown)
  // Line numbers (1-based) that differ from HEAD, barred gold in the editor when
  // the working tree is dirty for this file.
  const [changedLines, setChangedLines] = React.useState<number[]>([])
  // Cache-bust counter for binary previews: bumped when the file's signature
  // changes on disk so the <img>/<iframe> reloads (their src is otherwise static
  // and the browser would keep serving the cached bytes).
  const [bust, setBust] = React.useState(0)

  const dirty = draft != null && text != null && draft !== text

  // Latest values read by the polling interval below without making it a
  // dependency (which would tear down and restart the timer on every keystroke).
  const textRef = React.useRef(text)
  textRef.current = text
  const dirtyRef = React.useRef(dirty)
  dirtyRef.current = dirty

  // Is this file dirty in the working tree? Derive its repo-relative path the
  // same way FilesPanel does and look it up in the shared (already-polled) diff
  // metadata. Deleted files aren't viewable, so we ignore that status.
  const { activeCwd } = useApp()
  const diffData = useDiff().data ?? null
  const rel = React.useMemo(() => {
    if (!activeCwd) return null
    const root = activeCwd.replace(/\/$/, "")
    return path.startsWith(`${root}/`) ? path.slice(root.length + 1) : null
  }, [activeCwd, path])
  const fileDirty =
    !binary &&
    rel != null &&
    (diffData?.dirty ?? 0) > 0 &&
    (diffData?.files ?? []).some(
      (f) =>
        f.path === rel &&
        (f.status === "modified" ||
          f.status === "added" ||
          f.status === "renamed" ||
          f.status === "untracked")
    )

  // Fetch the file text (binary previews load straight from the file URL).
  React.useEffect(() => {
    setPreview(isMarkdown(path))
    setBust(0)
    if (binary) {
      setText(null)
      setDraft(null)
      setError(null)
      return
    }
    let cancelled = false
    setText(null)
    setDraft(null)
    setError(null)
    setSaveError(null)
    api
      .fileText(path)
      .then((t) => {
        if (cancelled) return
        setText(t)
        setDraft(t)
      })
      .catch((e: Error) => !cancelled && setError(e.message))
    return () => {
      cancelled = true
    }
  }, [path, binary])

  // Fetch the working-tree diff (vs HEAD) for this file and bar its changed
  // lines. "working" mode lines up with the on-disk file the viewer loads, so
  // the new-side line numbers map directly onto the editor.
  React.useEffect(() => {
    if (!fileDirty || rel == null || !activeCwd) {
      setChangedLines([])
      return
    }
    let cancelled = false
    api
      .diffFile(activeCwd, rel, "working")
      .then((res) => !cancelled && setChangedLines(changedNewLines(res.diff)))
      .catch(() => !cancelled && setChangedLines([]))
    return () => {
      cancelled = true
    }
  }, [activeCwd, rel, fileDirty])

  // Poll the open text file so external rewrites (an agent editing it, a build
  // regenerating it) surface without a manual page reload — mirroring the Files
  // tree's 5s root poll. We never clobber unsaved edits: the poll is skipped
  // while the editor is dirty, and the result is re-checked against the same
  // guard after the async fetch in case the user started typing mid-flight.
  // Skipped for binary previews (refreshed separately, below) and while the tab
  // is backgrounded, to avoid needless reads (SFTP round-trips on a remote host).
  React.useEffect(() => {
    if (binary) return
    const id = setInterval(() => {
      if (document.hidden || dirtyRef.current) return
      api
        .fileText(path)
        .then((t) => {
          // Re-check the guards: the initial load must have landed, the editor
          // must still be clean, and the content must have actually changed.
          if (dirtyRef.current || textRef.current === null) return
          if (t === textRef.current) return
          setText(t)
          setDraft(t)
        })
        .catch(() => {
          /* transient (file gone / host blip); keep the last good content */
        })
    }, 5000)
    return () => clearInterval(id)
  }, [path, binary])

  // Poll a binary preview's on-disk signature (mtime + size) and bump the
  // cache-bust counter only when it actually changes, so a regenerated image or
  // PDF reloads without flickering the preview on every tick.
  React.useEffect(() => {
    if (!binary) return
    let alive = true
    let sig: string | null = null
    api.fileSig(path).then((s) => {
      if (alive) sig = s
    })
    const id = setInterval(() => {
      if (document.hidden) return
      api.fileSig(path).then((s) => {
        if (!alive || s === null) return
        if (sig === null) {
          sig = s
          return
        }
        if (s !== sig) {
          sig = s
          setBust((b) => b + 1)
        }
      })
    }, 5000)
    return () => {
      alive = false
      clearInterval(id)
    }
  }, [path, binary])

  const save = React.useCallback(async () => {
    if (draft == null || saving) return
    setSaving(true)
    setSaveError(null)
    try {
      await api.writeFile(path, draft)
      setText(draft)
    } catch (e) {
      setSaveError((e as Error).message)
    } finally {
      setSaving(false)
    }
  }, [path, draft, saving])

  // Closing discards unsaved edits, so confirm first.
  const requestClose = React.useCallback(() => {
    if (dirty && !window.confirm("Discard unsaved changes?")) return
    onClose()
  }, [dirty, onClose])

  // ⌘/Ctrl+S saves; Escape closes (Escape is ignored while typing so it
  // doesn't fight the textarea, but the close button / outer key still work).
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "s") {
        e.preventDefault()
        if (!binary) void save()
        return
      }
      if (e.key === "Escape") requestClose()
    }
    document.addEventListener("keydown", onKey)
    return () => document.removeEventListener("keydown", onKey)
  }, [binary, save, requestClose])

  // The binary preview URL, with a cache-bust suffix once the file has changed
  // on disk so the browser refetches instead of reusing the cached bytes.
  const mediaURL = bust ? `${api.fileURL(path)}&v=${bust}` : api.fileURL(path)

  // Warn before a full page unload (browser close / reload) when dirty.
  React.useEffect(() => {
    if (!dirty) return
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault()
    }
    window.addEventListener("beforeunload", onBeforeUnload)
    return () => window.removeEventListener("beforeunload", onBeforeUnload)
  }, [dirty])

  return (
    <div className="absolute inset-0 z-10 flex flex-col bg-background">
      <header className="flex flex-shrink-0 items-center gap-2 border-border border-b bg-card px-3 py-1">
        <span
          className="overflow-hidden text-ellipsis whitespace-nowrap text-[13px] text-foreground"
          title={path}
        >
          {path}
          {dirty && <span className="ml-1 text-warn">●</span>}
        </span>
        {saveError && (
          <span
            className="whitespace-nowrap rounded-full border border-warn px-1.5 py-px text-[13px] text-warn"
            title={saveError}
          >
            save failed
          </span>
        )}
        <div className="ml-auto flex items-center gap-2">
          {markdown && !binary && error == null && text != null && (
            <Button
              variant="outline"
              size="sm"
              className="h-6"
              title={preview ? "edit raw markdown" : "preview"}
              onClick={() => setPreview((p) => !p)}
            >
              {preview ? <Pencil /> : <Eye />}
            </Button>
          )}
          {!binary && (
            <Button
              variant="outline"
              size="sm"
              className="h-6"
              title="save (⌘/Ctrl+S)"
              disabled={!dirty || saving}
              onClick={() => void save()}
            >
              <Save />
              {saving ? "saving…" : "save"}
            </Button>
          )}
          <Button
            variant="outline"
            size="sm"
            className="h-6"
            title="close (Esc)"
            onClick={requestClose}
          >
            <X />
          </Button>
        </div>
      </header>

      <div className="vbody">
        {image ? (
          <div className="vimg">
            <img src={mediaURL} alt={path} />
          </div>
        ) : pdf ? (
          <iframe className="vpdf" src={mediaURL} title={path} />
        ) : error ? (
          <div className="vloading">error: {error}</div>
        ) : draft == null ? (
          <div className="vloading">loading…</div>
        ) : markdown && preview ? (
          <div className="md-body">
            <ReactMarkdown
              remarkPlugins={[remarkGfm]}
              rehypePlugins={[rehypeHighlight]}
            >
              {draft}
            </ReactMarkdown>
          </div>
        ) : (
          <CodeEditor
            value={draft}
            path={path}
            onChange={setDraft}
            changedLines={changedLines}
          />
        )}
      </div>
    </div>
  )
}

// A CodeMirror 6 editor themed to the live herdr palette (see lib/codemirror).
// basicSetup gives line numbers, the fold gutter, bracket matching and in-editor
// search (⌘/Ctrl+F). For very large files we drop the language extension to skip
// the parsing cost — editing still works, just without highlighting.
function CodeEditor({
  value,
  path,
  onChange,
  changedLines,
}: {
  value: string
  path: string
  onChange: (v: string) => void
  changedLines: number[]
}) {
  // Recompute only when the file, the large-file threshold, or the changed-line
  // set changes — not on every keystroke — so CodeMirror isn't reconfigured as
  // the user types.
  const big = value.length > HILITE_CAP
  const extensions = React.useMemo(() => {
    const lang = big ? null : languageExtension(path)
    return [
      editorTheme,
      EditorView.lineWrapping,
      ...(lang ? [lang] : []),
      ...(changedLines.length ? [changedLinesHighlight(changedLines)] : []),
    ]
  }, [path, big, changedLines])

  return (
    <CodeMirror
      value={value}
      onChange={onChange}
      theme="none"
      // Use the browser's native selection (styled in lib/codemirror) instead of
      // CodeMirror's drawn one — the drawn band can't recolor selected text and
      // read as nearly invisible on light themes.
      basicSetup={{ drawSelection: false }}
      extensions={extensions}
      height="100%"
      className="cm-host"
    />
  )
}
