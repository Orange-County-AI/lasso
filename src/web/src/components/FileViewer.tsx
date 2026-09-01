import { EditorView } from "@codemirror/view"
import CodeMirror from "@uiw/react-codemirror"
import type { ElementContent } from "hast"
import { Eye, Pencil, Save, X } from "lucide-react"
import * as React from "react"
import ReactMarkdown, { type Components } from "react-markdown"
import rehypeHighlight from "rehype-highlight"
import rehypeRaw from "rehype-raw"
import rehypeSanitize, { defaultSchema } from "rehype-sanitize"
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
import { isImage, isMarkdown, isPdf, isVideo } from "@/lib/format"
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
  host,
  onClose,
  initialDraft,
  onDraftChange,
}: {
  path: string
  // The host the file was opened from — captured at open time by the parent, so
  // the viewer keeps reading, polling, and saving on that machine even if pane
  // focus (and the tree) moves onto another host while it's open.
  host: string | null
  onClose: () => void
  // An unsaved buffer carried over from an earlier mount of this same file. The
  // sidebar keeps its state per herdr pane, so selecting another agent unmounts
  // this editor; without the hand-off, in-flight edits would vanish silently on
  // a pane switch — the close path at least confirms first.
  initialDraft?: string | null
  // Reports the unsaved buffer (null once it matches disk again) so the parent
  // can hold it for this pane.
  onDraftChange?: (draft: string | null) => void
}) {
  const image = isImage(path)
  const pdf = isPdf(path)
  const video = isVideo(path)
  const markdown = isMarkdown(path)
  // Binary previews (images, PDFs, videos) render straight from the file URL —
  // no text is fetched and there's nothing to edit or save. The Go handler
  // serves these via http.ServeContent, which honors Range requests so the
  // browser can seek/stream video.
  const binary = image || pdf || video
  // Consumed once, by the initial load below; after that the editor owns the
  // buffer and a reload means the file (or its host) actually changed.
  const carry = React.useRef(initialDraft ?? null)

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

  // Hold the unsaved buffer above this component, so a pane switch (which
  // unmounts the viewer) doesn't drop it. Cleared as soon as it matches disk.
  React.useEffect(() => {
    onDraftChange?.(dirty ? draft : null)
  }, [dirty, draft, onDraftChange])

  // Is this file dirty in the working tree? Derive its repo-relative path the
  // same way FilesPanel does and look it up in the shared (already-polled) diff
  // metadata. Deleted files aren't viewable, so we ignore that status. That
  // metadata describes the cwd on its own host, so it only speaks for this file
  // when the viewer was opened on that same host — a viewer left open across a
  // focus change must not borrow another host's status (or diff its cwd there).
  const { activeCwd, cwdHost } = useApp()
  const diffData = useDiff().data ?? null
  const rel = React.useMemo(() => {
    if (!activeCwd) return null
    const root = activeCwd.replace(/\/$/, "")
    return path.startsWith(`${root}/`) ? path.slice(root.length + 1) : null
  }, [activeCwd, path])
  const fileDirty =
    !binary &&
    host === cwdHost &&
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
      .fileText(path, host ?? undefined)
      .then((t) => {
        if (cancelled) return
        const restored = carry.current
        carry.current = null
        setText(t)
        setDraft(restored ?? t)
      })
      .catch((e: Error) => !cancelled && setError(e.message))
    return () => {
      cancelled = true
    }
  }, [path, host, binary])

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
      .diffFile(activeCwd, rel, "working", undefined, host ?? undefined)
      .then((res) => !cancelled && setChangedLines(changedNewLines(res.diff)))
      .catch(() => !cancelled && setChangedLines([]))
    return () => {
      cancelled = true
    }
  }, [activeCwd, rel, fileDirty, host])

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
        .fileText(path, host ?? undefined)
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
  }, [path, host, binary])

  // Poll a binary preview's on-disk signature (mtime + size) and bump the
  // cache-bust counter only when it actually changes, so a regenerated image or
  // PDF reloads without flickering the preview on every tick.
  React.useEffect(() => {
    if (!binary) return
    let alive = true
    let sig: string | null = null
    api.fileSig(path, host ?? undefined).then((s) => {
      if (alive) sig = s
    })
    const id = setInterval(() => {
      if (document.hidden) return
      api.fileSig(path, host ?? undefined).then((s) => {
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
  }, [path, host, binary])

  const save = React.useCallback(async () => {
    if (draft == null || saving) return
    setSaving(true)
    setSaveError(null)
    try {
      await api.writeFile(path, draft, host ?? undefined)
      setText(draft)
    } catch (e) {
      setSaveError((e as Error).message)
    } finally {
      setSaving(false)
    }
  }, [path, host, draft, saving])

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

  // Rebuilt only when the file or its host moves: a new components object on
  // every render would remount the entire preview tree (and every mermaid
  // diagram in it) on each keystroke in the raw editor.
  const mdComps = React.useMemo(() => mdComponents(path, host), [path, host])

  // The binary preview URL, with a cache-bust suffix once the file has changed
  // on disk so the browser refetches instead of reusing the cached bytes.
  const mediaURL = bust
    ? `${api.fileURL(path, host ?? undefined)}&v=${bust}`
    : api.fileURL(path, host ?? undefined)

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
        ) : video ? (
          <div className="vvideo">
            <video src={mediaURL} controls>
              <track kind="captions" />
            </video>
          </div>
        ) : error ? (
          <div className="vloading">error: {error}</div>
        ) : draft == null ? (
          <div className="vloading">loading…</div>
        ) : markdown && preview ? (
          <div className="md-body">
            <ReactMarkdown
              remarkPlugins={[remarkGfm]}
              rehypePlugins={[
                rehypeRaw,
                [rehypeSanitize, MD_SCHEMA],
                rehypeHighlight,
              ]}
              components={mdComps}
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

// Recursively collect the text of a hast subtree. rehype-highlight has already
// run by the time components render, so a fence's <code> may hold a tree of
// tokenized <span>s rather than a single text node — we want the source back.
function hastText(nodes: ElementContent[] | undefined): string {
  if (!nodes) return ""
  let out = ""
  for (const n of nodes) {
    if (n.type === "text") out += n.value
    else if (n.type === "element") out += hastText(n.children)
  }
  return out
}

// A README is mostly HTML in practice -- <div align="center">, <img width=…>,
// <picture> for theme-aware art -- and react-markdown drops raw HTML unless
// rehype-raw puts it back. That means rendering markup out of whatever file the
// user opened, so sanitizing is not optional: this origin holds /api/file (read
// AND write, on any host lasso can drive) and an open /mcp, so one <script> in
// someone's README would be running with all of it.
//
// Order matters. rehype-raw first (parse the HTML), sanitize second (drop
// anything dangerous), rehype-highlight LAST -- highlighting after the
// sanitizer means its <span class=hljs-*> survive instead of being stripped.
const MD_SCHEMA = {
  ...defaultSchema,
  attributes: {
    ...defaultSchema.attributes,
    // Sizing and alignment are the whole reason a README reaches for HTML.
    img: [...(defaultSchema.attributes?.img ?? []), "width", "height", "loading"],
    div: [...(defaultSchema.attributes?.div ?? []), "align"],
    p: [...(defaultSchema.attributes?.p ?? []), "align"],
    h1: [...(defaultSchema.attributes?.h1 ?? []), "align"],
    h2: [...(defaultSchema.attributes?.h2 ?? []), "align"],
    table: [...(defaultSchema.attributes?.table ?? []), "align"],
  },
  tagNames: [...(defaultSchema.tagNames ?? []), "picture", "source"],
}

// Resolve a markdown image against the FILE, not the browser.
//
// `docs/screenshots/diff.png` in a README is relative to that README's
// directory on that README's machine. Left alone the browser resolves it
// against lasso's own origin and asks the app for /docs/screenshots/diff.png,
// which is a 404 and renders as a broken image -- so every relative image in
// every repo silently failed to load. Route it through /api/file on the file's
// own host instead, the same way the binary preview already does.
function resolveMarkdownSrc(
  src: string | undefined,
  filePath: string,
  host: string | null
): string | undefined {
  if (!src) return src
  // Absolute URLs and inline data stay exactly as written.
  if (/^[a-z][a-z0-9+.-]*:/i.test(src) || src.startsWith("//")) return src
  const dir = filePath.slice(0, filePath.lastIndexOf("/")) || "/"
  const joined = src.startsWith("/") ? src : `${dir}/${src}`
  // Collapse . and .. so ../assets/x.png from a nested doc lands correctly;
  // the backend takes an absolute path and does not resolve traversal for us.
  const parts: string[] = []
  for (const seg of joined.split("/")) {
    if (!seg || seg === ".") continue
    if (seg === "..") parts.pop()
    else parts.push(seg)
  }
  return api.fileURL(`/${parts.join("/")}`, host ?? undefined)
}

// The only markdown component override: a ```mermaid fence renders as a diagram,
// every other fence falls through to the untouched <pre> that rehype-highlight
// produced. We hook <pre> rather than <code> so the diagram replaces the whole
// block (a <div>/<svg> inside a <pre> is invalid nesting, and the code panel's
// background would frame the diagram).
function mdComponents(path: string, host: string | null) {
  return {
  pre({ node, children, ...rest }) {
    const code = node?.children?.[0]
    if (code?.type === "element" && code.tagName === "code") {
      const cls = code.properties?.className
      const langs = Array.isArray(cls) ? cls.map(String) : []
      if (langs.includes("language-mermaid"))
        return <MermaidDiagram chart={hastText(code.children)} />
    }
    return <pre {...rest}>{children}</pre>
  },
  // Covers both ![](x) and a raw <img> from rehype-raw: react-markdown routes
  // the reconstructed HTML through this same components map.
  img({ node, src, alt, ...rest }) {
    return (
      <img
        {...rest}
        // An <img> in a README often carries no alt; empty marks it decorative
        // rather than leaving assistive tech to read out the file name.
        alt={alt ?? ""}
        src={resolveMarkdownSrc(
          typeof src === "string" ? src : undefined,
          path,
          host
        )}
      />
    )
  },
  } satisfies Components
}

// The resolved light/dark chrome, read off the html class that lib/mode.ts owns
// (the single chokepoint for the OS-, user- and herdr-driven answers alike) and
// kept live with an observer, since nothing publishes it to React.
function useDarkChrome(): boolean {
  const [dark, setDark] = React.useState(() =>
    document.documentElement.classList.contains("dark")
  )
  React.useEffect(() => {
    const el = document.documentElement
    const obs = new MutationObserver(() =>
      setDark(el.classList.contains("dark"))
    )
    obs.observe(el, { attributes: true, attributeFilter: ["class"] })
    return () => obs.disconnect()
  }, [])
  return dark
}

// A rendered mermaid diagram. mermaid is a ~2.5MB parser+renderer, so it's
// pulled in by a dynamic import inside the effect — a markdown file with no
// mermaid in it never loads it. securityLevel "strict" is load-bearing: the
// returned SVG is injected into the DOM and the markdown is untrusted repo
// content, so labels are escaped and click/script directives are dropped.
// suppressErrorRendering keeps mermaid from appending its own error graphic to
// the document body when a diagram doesn't parse — we show the message
// alongside the original source instead, so a bad block stays readable and
// doesn't take the rest of the preview down.
function MermaidDiagram({ chart }: { chart: string }) {
  const dark = useDarkChrome()
  const [svg, setSvg] = React.useState<string | null>(null)
  const [err, setErr] = React.useState<string | null>(null)
  // mermaid.render needs a DOM-id-safe, unique id; useId's own value contains
  // colons, which break the selectors mermaid builds from it.
  const id = `mmd-${React.useId().replace(/[^a-zA-Z0-9]/g, "")}`

  React.useEffect(() => {
    let cancelled = false
    void (async () => {
      try {
        const mermaid = (await import("mermaid")).default
        mermaid.initialize({
          startOnLoad: false,
          securityLevel: "strict",
          suppressErrorRendering: true,
          theme: dark ? "dark" : "default",
        })
        const { svg } = await mermaid.render(id, chart)
        if (cancelled) return
        setSvg(svg)
        setErr(null)
      } catch (e) {
        if (cancelled) return
        setSvg(null)
        setErr(e instanceof Error ? e.message : String(e))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [chart, dark, id])

  if (err != null)
    return (
      <div className="md-mermaid-error">
        <div className="md-mermaid-msg">mermaid: {err}</div>
        <pre>
          <code>{chart}</code>
        </pre>
      </div>
    )
  if (svg == null) return <div className="md-mermaid md-mermaid-loading" />
  return (
    // biome-ignore lint/security/noDangerouslySetInnerHtml: mermaid emits an SVG string, sanitized by its own securityLevel "strict"
    <div className="md-mermaid" dangerouslySetInnerHTML={{ __html: svg }} />
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
