import hljs from "highlight.js"
import { Eye, Pencil, Save, X } from "lucide-react"
import * as React from "react"
import ReactMarkdown from "react-markdown"
import rehypeHighlight from "rehype-highlight"
import remarkGfm from "remark-gfm"
import { Button } from "@/components/ui/button"
import { api } from "@/lib/api"
import { isImage, isMarkdown, isPdf, langForPath } from "@/lib/format"

const HILITE_CAP = 400 * 1024 // don't syntax-highlight files larger than this

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

  const dirty = draft != null && text != null && draft !== text

  // Fetch the file text (binary previews load straight from the file URL).
  React.useEffect(() => {
    setPreview(isMarkdown(path))
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
          className="overflow-hidden text-ellipsis whitespace-nowrap text-foreground text-xs"
          title={path}
        >
          {path}
          {dirty && <span className="ml-1 text-warn">●</span>}
        </span>
        {saveError && (
          <span
            className="whitespace-nowrap rounded-full border border-warn px-1.5 py-px text-[10px] text-warn"
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
            <img src={api.fileURL(path)} alt={path} />
          </div>
        ) : pdf ? (
          <iframe className="vpdf" src={api.fileURL(path)} title={path} />
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
            lang={langForPath(path)}
            onChange={setDraft}
          />
        )}
      </div>
    </div>
  )
}

// A textarea layered over a syntax-highlighted <pre>: the textarea is
// transparent (only its caret/selection show) and the highlighted code beneath
// it is what the user reads. Above HILITE_CAP we skip highlighting and fall
// back to a plain visible textarea.
function CodeEditor({
  value,
  lang,
  onChange,
}: {
  value: string
  lang: string
  onChange: (v: string) => void
}) {
  const highlight = value.length <= HILITE_CAP

  const html = React.useMemo(() => {
    if (!highlight) return null
    try {
      if (lang && hljs.getLanguage(lang))
        return hljs.highlight(value, { language: lang }).value
      return hljs.highlightAuto(value).value
    } catch {
      return null
    }
  }, [value, lang, highlight])

  if (!highlight) {
    return (
      <textarea
        className="veditor"
        value={value}
        spellCheck={false}
        onChange={(e) => onChange(e.target.value)}
      />
    )
  }

  return (
    <div className="veditor-wrap">
      <pre className="vcode wrap" aria-hidden="true">
        {html != null ? (
          <code className="hljs" dangerouslySetInnerHTML={{ __html: html }} />
        ) : (
          <code className="hljs">{value}</code>
        )}
      </pre>
      <textarea
        className="veditor-input"
        value={value}
        spellCheck={false}
        onChange={(e) => onChange(e.target.value)}
      />
    </div>
  )
}
