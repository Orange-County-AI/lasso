// Minimal unified-diff parser: split `git diff` output into per-file blocks of
// typed lines. Ported verbatim from the original index.html parseDiff().

export type DiffLineType = "hunk" | "add" | "del" | "ctx" | "meta"

export interface DiffLine {
  t: DiffLineType
  s: string
}

export interface DiffFile {
  path: string
  lines: DiffLine[]
  add: number
  del: number
}

export function parseDiff(text: string): DiffFile[] {
  const files: DiffFile[] = []
  let cur: DiffFile | null = null
  for (const line of text.split("\n")) {
    if (line.startsWith("diff --git")) {
      cur = { path: "", lines: [], add: 0, del: 0 }
      files.push(cur)
      const m = line.match(/ b\/(.+)$/)
      if (m) cur.path = m[1]
      continue
    }
    if (!cur) continue
    if (line.startsWith("--- ")) continue
    if (line.startsWith("+++ ")) {
      const m = line.match(/^\+\+\+ b\/(.+)$/)
      if (m) cur.path = m[1]
      continue
    }
    if (line.startsWith("@@")) {
      cur.lines.push({ t: "hunk", s: line })
      continue
    }
    if (line.startsWith("+")) {
      cur.lines.push({ t: "add", s: line.slice(1) })
      cur.add++
      continue
    }
    if (line.startsWith("-")) {
      cur.lines.push({ t: "del", s: line.slice(1) })
      cur.del++
      continue
    }
    if (line.startsWith(" ")) {
      cur.lines.push({ t: "ctx", s: line.slice(1) })
      continue
    }
    if (line.startsWith("Binary ")) {
      cur.lines.push({ t: "meta", s: line })
    }
    // index/new file/mode/rename/"\ No newline" lines — not shown
  }
  return files
}
