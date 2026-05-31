// CodeMirror 6 wiring for the file viewer: a language resolver and a theme that
// both track the live herdr palette (the --h-* CSS variables that lib/theme.ts
// rewrites at runtime). Kept out of FileViewer.tsx so the component stays lean.
import { cpp } from "@codemirror/lang-cpp"
import { css } from "@codemirror/lang-css"
import { go } from "@codemirror/lang-go"
import { html } from "@codemirror/lang-html"
import { java } from "@codemirror/lang-java"
import { javascript } from "@codemirror/lang-javascript"
import { json } from "@codemirror/lang-json"
import { markdown } from "@codemirror/lang-markdown"
import { php } from "@codemirror/lang-php"
import { python } from "@codemirror/lang-python"
import { rust } from "@codemirror/lang-rust"
import { sass } from "@codemirror/lang-sass"
import { sql } from "@codemirror/lang-sql"
import { yaml } from "@codemirror/lang-yaml"
import {
  HighlightStyle,
  StreamLanguage,
  type StreamParser,
  syntaxHighlighting,
} from "@codemirror/language"
import {
  csharp,
  dart,
  kotlin,
  scala,
} from "@codemirror/legacy-modes/mode/clike"
import { clojure } from "@codemirror/legacy-modes/mode/clojure"
import { diff } from "@codemirror/legacy-modes/mode/diff"
import { dockerFile } from "@codemirror/legacy-modes/mode/dockerfile"
import { erlang } from "@codemirror/legacy-modes/mode/erlang"
import { haskell } from "@codemirror/legacy-modes/mode/haskell"
import { lua } from "@codemirror/legacy-modes/mode/lua"
import { perl } from "@codemirror/legacy-modes/mode/perl"
import { powerShell } from "@codemirror/legacy-modes/mode/powershell"
import { properties } from "@codemirror/legacy-modes/mode/properties"
import { protobuf } from "@codemirror/legacy-modes/mode/protobuf"
import { r } from "@codemirror/legacy-modes/mode/r"
import { ruby } from "@codemirror/legacy-modes/mode/ruby"
import { shell } from "@codemirror/legacy-modes/mode/shell"
import { swift } from "@codemirror/legacy-modes/mode/swift"
import type { Extension } from "@codemirror/state"
import { EditorView } from "@codemirror/view"
import { tags as t } from "@lezer/highlight"
import { langForPath } from "@/lib/format"

const stream = <S>(parser: StreamParser<S>) => StreamLanguage.define(parser)

// Map highlight.js language ids (what langForPath returns) to a CM6 language
// extension. Ids with no good CM6 equivalent (elixir, graphql, makefile) are
// absent — the file still opens, just as plain text.
const BY_ID: Record<string, () => Extension> = {
  javascript: () => javascript({ jsx: true }),
  typescript: () => javascript({ jsx: true, typescript: true }),
  go: () => go(),
  python: () => python(),
  ruby: () => stream(ruby),
  rust: () => rust(),
  java: () => java(),
  kotlin: () => stream(kotlin),
  c: () => cpp(),
  cpp: () => cpp(),
  csharp: () => stream(csharp),
  php: () => php(),
  swift: () => stream(swift),
  scala: () => stream(scala),
  bash: () => stream(shell),
  powershell: () => stream(powerShell),
  sql: () => sql(),
  json: () => json(),
  yaml: () => yaml(),
  ini: () => stream(properties),
  xml: () => html(), // langForPath folds html/svg/vue/xml into "xml"
  css: () => css(),
  scss: () => sass(),
  less: () => css(),
  markdown: () => markdown(),
  lua: () => stream(lua),
  perl: () => stream(perl),
  r: () => stream(r),
  dart: () => stream(dart),
  erlang: () => stream(erlang),
  haskell: () => stream(haskell),
  clojure: () => stream(clojure),
  diff: () => stream(diff),
  protobuf: () => stream(protobuf),
  dockerfile: () => stream(dockerFile),
}

// The CM6 language extension for a path, or null when we have no mapping.
export function languageExtension(path: string): Extension | null {
  const make = BY_ID[langForPath(path)]
  return make ? make() : null
}

// Token colors, mirroring the .hljs-* rules in index.css so highlighting looks
// identical to the old highlight.js overlay and tracks the live theme.
const highlightStyle = HighlightStyle.define([
  {
    tag: [t.comment, t.lineComment, t.blockComment, t.docComment],
    color: "var(--h-muted)",
    fontStyle: "italic",
  },
  {
    tag: [
      t.keyword,
      t.modifier,
      t.operatorKeyword,
      t.tagName,
      t.standard(t.tagName),
      t.standard(t.name),
    ],
    color: "var(--h-accent)",
  },
  {
    tag: [t.string, t.special(t.string), t.regexp, t.inserted],
    color: "var(--h-good)",
  },
  { tag: [t.number, t.bool, t.atom, t.literal], color: "var(--h-warn)" },
  {
    tag: [
      t.function(t.variableName),
      t.function(t.propertyName),
      t.definition(t.function(t.variableName)),
    ],
    color: "var(--h-dir)",
  },
  {
    tag: [t.typeName, t.className, t.propertyName, t.attributeValue],
    color: "var(--h-warn)",
  },
  { tag: [t.variableName, t.attributeName], color: "var(--h-fg)" },
  { tag: [t.meta, t.processingInstruction], color: "var(--h-muted)" },
  { tag: t.deleted, color: "var(--h-bad)" },
  { tag: t.heading, color: "var(--h-dir)", fontWeight: "700" },
  { tag: t.emphasis, fontStyle: "italic" },
  { tag: t.strong, fontWeight: "700" },
  {
    tag: [t.link, t.url],
    color: "var(--h-accent)",
    textDecoration: "underline",
  },
])

// Editor chrome themed against the --h-* palette, matching the old editor's
// font metrics (12.5px / 1.5 / monospace, 14px padding).
const baseTheme = EditorView.theme(
  {
    "&": {
      color: "var(--h-fg)",
      backgroundColor: "var(--h-bg)",
      height: "100%",
    },
    ".cm-scroller": {
      overflow: "auto",
      fontFamily: "var(--font-mono, ui-monospace, monospace)",
      fontSize: "12.5px",
      lineHeight: "1.5",
    },
    ".cm-content": { padding: "14px 0", caretColor: "var(--h-fg)" },
    ".cm-cursor, .cm-dropCursor": { borderLeftColor: "var(--h-fg)" },
    "&.cm-focused .cm-selectionBackground, .cm-selectionBackground, .cm-content ::selection":
      {
        backgroundColor: "var(--h-accent-dim)",
      },
    ".cm-activeLine": { backgroundColor: "var(--h-hover)" },
    ".cm-gutters": {
      backgroundColor: "var(--h-bg)",
      color: "var(--h-muted)",
      border: "none",
    },
    ".cm-activeLineGutter": {
      backgroundColor: "var(--h-hover)",
      color: "var(--h-fg)",
    },
    ".cm-foldPlaceholder": {
      backgroundColor: "var(--h-hover)",
      color: "var(--h-muted)",
      border: "none",
    },
    ".cm-matchingBracket, &.cm-focused .cm-matchingBracket": {
      backgroundColor: "var(--h-accent-dim)",
      outline: "1px solid var(--h-accent)",
    },
    ".cm-tooltip": {
      backgroundColor: "var(--h-panel)",
      border: "1px solid var(--h-border)",
      color: "var(--h-fg)",
    },
    ".cm-tooltip-autocomplete ul li[aria-selected]": {
      backgroundColor: "var(--h-hover)",
      color: "var(--h-fg)",
    },
    ".cm-searchMatch": {
      backgroundColor: "var(--h-accent-dim)",
      outline: "1px solid var(--h-accent)",
    },
    ".cm-searchMatch.cm-searchMatch-selected": {
      backgroundColor: "var(--h-accent)",
      color: "var(--h-bg)",
    },
    ".cm-panels, .cm-panel": {
      backgroundColor: "var(--h-panel)",
      color: "var(--h-fg)",
    },
    ".cm-panels.cm-panels-bottom": { borderTop: "1px solid var(--h-border)" },
    ".cm-panel input, .cm-panel button": {
      backgroundColor: "var(--h-bg)",
      color: "var(--h-fg)",
      border: "1px solid var(--h-border)",
    },
  },
  { dark: true }
)

// Combined theme: chrome + token colors. Pass this in the editor's extensions.
export const editorTheme: Extension = [
  baseTheme,
  syntaxHighlighting(highlightStyle),
]
