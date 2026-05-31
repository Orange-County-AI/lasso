import hljs from "highlight.js"
import { ArrowLeft, ExternalLink, X } from "lucide-react"
import * as React from "react"
import ReactMarkdown from "react-markdown"
import rehypeHighlight from "rehype-highlight"
import remarkGfm from "remark-gfm"
import { Button } from "@/components/ui/button"
import { api } from "@/lib/api"
import { lsGet, lsSet } from "@/lib/app-store"
import { isImage, isMarkdown, langForPath } from "@/lib/format"
import { cn } from "@/lib/utils"

const HILITE_CAP = 400 * 1024 // don't syntax-highlight files larger than this

// The full-column file viewer overlay: images (click-to-zoom checkerboard),
// markdown (rendered ↔ raw toggle), or syntax-highlighted code (wrap toggle).
export function FileViewer({
  path,
  onClose,
}: {
  path: string
  onClose: () => void
}) {
  const [text, setText] = React.useState<string | null>(null)
  const [error, setError] = React.useState<string | null>(null)
  const [mdRendered, setMdRendered] = React.useState(true)
  const [wrap, setWrap] = React.useState(lsGet("codeWrap") !== "0")

  const image = isImage(path)
  const markdown = isMarkdown(path)
  const tooLarge = text != null && text.length > HILITE_CAP

  // Fetch the file text (images load straight from the <img> src).
  React.useEffect(() => {
    if (image) {
      setText(null)
      setError(null)
      return
    }
    let cancelled = false
    setText(null)
    setError(null)
    setMdRendered(true)
    api
      .fileText(path)
      .then((t) => !cancelled && setText(t))
      .catch((e: Error) => !cancelled && setError(e.message))
    return () => {
      cancelled = true
    }
  }, [path, image])

  // Escape closes the viewer.
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose()
    }
    document.addEventListener("keydown", onKey)
    return () => document.removeEventListener("keydown", onKey)
  }, [onClose])

  const toggleWrap = () => {
    setWrap((w) => {
      lsSet("codeWrap", w ? "0" : "1")
      return !w
    })
  }

  return (
    <div className="absolute inset-0 z-10 flex flex-col bg-background">
      <header className="flex flex-shrink-0 items-center gap-2 border-border border-b bg-card px-3 py-1.5">
        <Button variant="outline" size="sm" className="h-7" onClick={onClose}>
          <ArrowLeft data-icon="inline-start" />
          files
        </Button>
        <span
          className="overflow-hidden text-ellipsis whitespace-nowrap text-foreground text-xs"
          title={path}
        >
          {path}
        </span>
        {tooLarge && (
          <span className="whitespace-nowrap rounded-full border border-warn px-1.5 py-px text-[10px] text-warn">
            large file — no highlight
          </span>
        )}
        <span className="ml-auto" />
        {markdown && (
          <Button
            variant="outline"
            size="sm"
            className="h-7"
            onClick={() => setMdRendered((v) => !v)}
          >
            {mdRendered ? "Raw" : "Rendered"}
          </Button>
        )}
        {!markdown && !image && (
          <Button
            variant="outline"
            size="sm"
            className={cn("h-7", wrap && "border-primary text-primary")}
            onClick={toggleWrap}
          >
            Wrap
          </Button>
        )}
        <Button
          variant="outline"
          size="sm"
          className="h-7"
          title="open raw file in a new tab"
          onClick={() => window.open(api.fileURL(path), "_blank")}
        >
          <ExternalLink />
        </Button>
        <Button
          variant="outline"
          size="sm"
          className="h-7"
          title="close (Esc)"
          onClick={onClose}
        >
          <X />
        </Button>
      </header>

      <div className="vbody">
        {image ? (
          <div className="vimg">
            <img src={api.fileURL(path)} alt={path} />
          </div>
        ) : error ? (
          <div className="vloading">error: {error}</div>
        ) : text == null ? (
          <div className="vloading">loading…</div>
        ) : markdown && mdRendered ? (
          <div className="md-body">
            <ReactMarkdown
              remarkPlugins={[remarkGfm]}
              rehypePlugins={[rehypeHighlight]}
            >
              {text}
            </ReactMarkdown>
          </div>
        ) : (
          <CodeBlock
            text={text}
            lang={markdown ? "markdown" : langForPath(path)}
            wrap={wrap}
            highlight={!tooLarge}
          />
        )}
      </div>
    </div>
  )
}

function CodeBlock({
  text,
  lang,
  wrap,
  highlight,
}: {
  text: string
  lang: string
  wrap: boolean
  highlight: boolean
}) {
  const html = React.useMemo(() => {
    if (!highlight) return null
    try {
      if (lang && hljs.getLanguage(lang))
        return hljs.highlight(text, { language: lang }).value
      return hljs.highlightAuto(text).value
    } catch {
      return null
    }
  }, [text, lang, highlight])

  return (
    <pre className={cn("vcode", wrap && "wrap")}>
      {html != null ? (
        <code className="hljs" dangerouslySetInnerHTML={{ __html: html }} />
      ) : (
        <code>{text}</code>
      )}
    </pre>
  )
}
