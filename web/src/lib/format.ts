// Shorten a $HOME path to ~ for display (matches the original UI).
export function tilde(p: string | undefined): string {
  return (p || "").replace(/^\/home\/[^/]+/, "~")
}

// Human-readable byte size: 1.2K, 3.4M, … (matches the original fmtSize).
export function fmtSize(n: number | undefined): string {
  if (n == null) return ""
  const u = ["B", "K", "M", "G"]
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024
    i++
  }
  return (i ? v.toFixed(1) : v) + u[i]
}

const IMG_RE = /\.(png|jpe?g|gif|webp|svg|bmp|ico|avif)$/i
const MD_RE = /\.(md|markdown|mdown|mkd|mkdn|mdwn)$/i
const PDF_RE = /\.pdf$/i

export const isImage = (p: string) => IMG_RE.test(p)
export const isMarkdown = (p: string) => MD_RE.test(p)
export const isPdf = (p: string) => PDF_RE.test(p)

// file extension -> highlight.js language id (falls back to auto-detect)
const LANGS: Record<string, string> = {
  js: "javascript",
  mjs: "javascript",
  cjs: "javascript",
  jsx: "javascript",
  ts: "typescript",
  tsx: "typescript",
  mts: "typescript",
  cts: "typescript",
  go: "go",
  py: "python",
  pyw: "python",
  rb: "ruby",
  rs: "rust",
  java: "java",
  kt: "kotlin",
  c: "c",
  h: "c",
  cpp: "cpp",
  cc: "cpp",
  cxx: "cpp",
  hpp: "cpp",
  hh: "cpp",
  cs: "csharp",
  php: "php",
  swift: "swift",
  scala: "scala",
  sh: "bash",
  bash: "bash",
  zsh: "bash",
  fish: "bash",
  ps1: "powershell",
  sql: "sql",
  json: "json",
  jsonc: "json",
  json5: "json",
  yaml: "yaml",
  yml: "yaml",
  toml: "ini",
  ini: "ini",
  cfg: "ini",
  conf: "ini",
  properties: "ini",
  xml: "xml",
  html: "xml",
  htm: "xml",
  svg: "xml",
  vue: "xml",
  css: "css",
  scss: "scss",
  sass: "scss",
  less: "less",
  md: "markdown",
  markdown: "markdown",
  lua: "lua",
  pl: "perl",
  r: "r",
  dart: "dart",
  ex: "elixir",
  exs: "elixir",
  erl: "erlang",
  hs: "haskell",
  clj: "clojure",
  diff: "diff",
  patch: "diff",
  graphql: "graphql",
  gql: "graphql",
  proto: "protobuf",
  tf: "ini",
  hcl: "ini",
  dockerfile: "dockerfile",
  makefile: "makefile",
  mk: "makefile",
}

export function langForPath(p: string): string {
  const base = (p.split("/").pop() || "").toLowerCase()
  if (base === "dockerfile") return "dockerfile"
  if (base === "makefile") return "makefile"
  const ext = base.includes(".") ? base.split(".").pop()! : ""
  return LANGS[ext] || ""
}
