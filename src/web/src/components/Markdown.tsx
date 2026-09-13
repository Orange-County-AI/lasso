import type { ElementContent } from "hast"
import * as React from "react"
import type { Components } from "react-markdown"
import ReactMarkdown from "react-markdown"
import rehypeHighlight from "rehype-highlight"
import rehypeRaw from "rehype-raw"
import rehypeSanitize, { defaultSchema } from "rehype-sanitize"
import remarkGfm from "remark-gfm"
import { api } from "@/lib/api"

// One markdown pipeline, two readers: the file viewer, where the source is a
// document on a machine, and the chat, where it is what an agent just said.
// Extracted rather than copied because the parts that matter here are the ones
// that are easy to get subtly wrong twice — the sanitize order, the mermaid
// fence, and the fact that a raw <img> has to be resolved before the browser
// resolves it. Wrap the output in `.md-body` (index.css) for the styling.

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
// someone's README would be running with all of it. The same holds for a chat:
// the text being rendered is model output.
//
// Order matters. rehype-raw first (parse the HTML), sanitize second (drop
// anything dangerous), rehype-highlight LAST -- highlighting after the
// sanitizer means its <span class=hljs-*> survive instead of being stripped.
const MD_SCHEMA = {
  ...defaultSchema,
  attributes: {
    ...defaultSchema.attributes,
    // Sizing and alignment are the whole reason a README reaches for HTML.
    img: [
      ...(defaultSchema.attributes?.img ?? []),
      "width",
      "height",
      "loading",
    ],
    div: [...(defaultSchema.attributes?.div ?? []), "align"],
    p: [...(defaultSchema.attributes?.p ?? []), "align"],
    h1: [...(defaultSchema.attributes?.h1 ?? []), "align"],
    h2: [...(defaultSchema.attributes?.h2 ?? []), "align"],
    table: [...(defaultSchema.attributes?.table ?? []), "align"],
  },
  tagNames: [...(defaultSchema.tagNames ?? []), "picture", "source"],
}

// Resolve a markdown image against the DOCUMENT it came from, not the browser.
//
// `docs/screenshots/diff.png` in a README is relative to that README's
// directory on that README's machine. Left alone the browser resolves it
// against lasso's own origin and asks the app for /docs/screenshots/diff.png,
// which is a 404 and renders as a broken image -- so every relative image in
// every repo silently failed to load. Route it through /api/file on the host
// the document lives on instead, the same way the binary preview already does.
//
// `from` is that document's own path (or any path in its directory: only the
// directory is read), and host is the machine it lives on.
export function resolveMarkdownSrc(
  src: string | undefined,
  from: string,
  host: string | null
): string | undefined {
  if (!src) return src
  // Absolute URLs and inline data stay exactly as written.
  if (/^[a-z][a-z0-9+.-]*:/i.test(src) || src.startsWith("//")) return src
  const dir = from.slice(0, from.lastIndexOf("/")) || "/"
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

// A URL a model wrote inside a code span. Deliberately the same two forms GFM
// autolinks in prose — an explicit scheme, or a www. host — so a URL reads the
// same whether or not it was wrapped in backticks, which is exactly the case
// that made them untappable: agents routinely write `https://…` as code, and on
// a phone a code chip is a string you cannot open.
//
// Not a bare-domain rule, on purpose: `README.md`, `go.mod` and `src/web/src`
// are code, and a false positive turns a filename into a link.
const codeURLRe = /(?:https?:\/\/|www\.)[^\s<>"'`\\]+/gi

// linkifyCode returns a code span's text with any URLs in it wrapped in links.
// The characters are untouched — only the parts that are addresses become
// anchors — so `curl http://x/y` stays a command whose URL happens to be
// tappable.
function linkifyCode(text: string): React.ReactNode[] {
  const out: React.ReactNode[] = []
  let last = 0
  for (const m of text.matchAll(codeURLRe)) {
    const start = m.index ?? 0
    // Trailing sentence punctuation is not part of the address, and a closing
    // paren only belongs to it when the URL opened one.
    let url = m[0].replace(/[.,;:!?]+$/, "")
    if (url.endsWith(")") && !url.includes("(")) url = url.slice(0, -1)
    if (url.length === 0) continue
    if (start > last) out.push(text.slice(last, start))
    out.push(
      <a
        key={`${start}-${url}`}
        href={/^www\./i.test(url) ? `http://${url}` : url}
        target="_blank"
        rel="noopener noreferrer"
      >
        {url}
      </a>
    )
    last = start + url.length
  }
  if (out.length === 0) return [text]
  if (last < text.length) out.push(text.slice(last))
  return out
}

// The only markdown component override: a ```mermaid fence renders as a diagram,
// every other fence falls through to the untouched <pre> that rehype-highlight
// produced. We hook <pre> rather than <code> so the diagram replaces the whole
// block (a <div>/<svg> inside a <pre> is invalid nesting, and the code panel's
// background would frame the diagram).
//
// resolveImageSrc is optional: a document has a directory to resolve against, a
// chat message often has none, and an unresolvable relative path is better left
// exactly as written than pointed at a 404 on lasso's own origin.
function mdComponents(
  resolveImageSrc?: (src: string | undefined) => string | undefined
) {
  return {
    pre({ node, children, ...rest }) {
      const code = node?.children?.[0]
      const isCode = code?.type === "element" && code.tagName === "code"
      if (isCode) {
        const cls = code.properties?.className
        const langs = Array.isArray(cls) ? cls.map(String) : []
        if (langs.includes("language-mermaid"))
          return <MermaidDiagram chart={hastText(code.children)} />
      }
      // A fence's own <code> is rebuilt here rather than passed through (which
      // would dispatch to the `code` override below). That override linkifies
      // inline code, and a BLOCK is where a tap has to place a caret and select
      // text — a config, a patch — so an anchor there would swallow it. This is
      // also the only place that knows a fence is around the element, so it is
      // where the two are kept apart; no hook, since these renderers are called
      // while the tree is built rather than as components.
      const inner = isCode ? React.Children.toArray(children)[0] : undefined
      if (
        React.isValidElement<{
          className?: string
          children?: React.ReactNode
        }>(inner)
      ) {
        return (
          <pre {...rest}>
            <code className={inner.props.className}>
              {inner.props.children}
            </code>
          </pre>
        )
      }
      return <pre {...rest}>{children}</pre>
    },
    // Inline code, where a URL the model wrote is a REFERENCE and not code.
    code({ node: _node, className, children, ...rest }) {
      if (typeof children !== "string") {
        return (
          <code className={className} {...rest}>
            {children}
          </code>
        )
      }
      return (
        <code className={className} {...rest}>
          {linkifyCode(children)}
        </code>
      )
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
          src={
            resolveImageSrc?.(
              typeof src === "string" ? src : undefined
            ) as string
          }
        />
      )
    },
    // Links LEAVE lasso, and a link followed in the app takes the app with it:
    // this is a single-page chat served to a phone, often as the installed PWA
    // where there is no back button to return with, so a tap on a URL an agent
    // printed would replace the session someone was reading. Handing it to the
    // browser keeps the conversation — and `noopener` is not ceremony: without
    // it the opened document gets a handle on this window.
    //
    // Set HERE rather than in the sanitize schema deliberately. This override
    // runs AFTER sanitizing, so the untrusted HTML rehype-raw parses back in
    // (a README's own markup, an agent's output) can never ask for these two
    // attributes itself — which is exactly what the sanitizer is for.
    a({ node: _node, children, ...rest }) {
      return (
        <a {...rest} target="_blank" rel="noopener noreferrer">
          {children}
        </a>
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

// A rendered mermaid diagram.
//
// Deliberately a dynamic import: mermaid is a ~2.5MB parser+renderer and a
// static import would put it in the bundle every page load pays for, while the
// great majority of markdown has no diagram in it at all. The specifier is a
// literal, so this is the lazy-loading exception the rule names rather than a
// runtime-selected module.
//
// securityLevel "strict" is load-bearing: the returned SVG is injected into the
// DOM and the markdown is untrusted content, so labels are escaped and
// click/script directives are dropped. suppressErrorRendering keeps mermaid
// from appending its own error graphic to the document body when a diagram
// doesn't parse — we show the message alongside the original source instead, so
// a bad block stays readable and doesn't take the rest of the view down.
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

// Rendered markdown. Memoised on the source string, which is what makes it
// affordable in the chat: that view re-renders on every poll, and without this
// every visible block would be re-parsed every couple of seconds. The default
// shallow compare is exactly right — source is the only prop that changes.
export const Markdown = React.memo(function Markdown({
  source,
  resolveImageSrc,
}: {
  source: string
  resolveImageSrc?: (src: string | undefined) => string | undefined
}) {
  // The components map is passed by identity to react-markdown, so it is built
  // once per resolver rather than per render.
  const components = React.useMemo(
    () => mdComponents(resolveImageSrc),
    [resolveImageSrc]
  )
  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      rehypePlugins={[rehypeRaw, [rehypeSanitize, MD_SCHEMA], rehypeHighlight]}
      components={components}
    >
      {source}
    </ReactMarkdown>
  )
})
