// Telling "this chunk is gone" apart from "this input is bad".
//
// One dynamic import survives in the app (mermaid, in components/Markdown) and
// it can fail for a reason that has nothing to do with the thing being
// rendered: every /assets/ filename carries a content hash, lasso self-updates
// by swapping its own binary — which swaps the embedded bundle with it — and a
// tab loaded before that asks for a name the new binary has never heard of. The
// /assets/ handler answers a plain 404, so the import rejects.
//
// Reported to the user as-is that reads as a broken diagram ("Failed to fetch
// dynamically imported module: …"), which is the wrong diagnosis and points at
// the wrong fix. A reload is the fix, because it fetches the new index.html and
// with it the names that exist now.
//
// The message is the only signal available — a module load failure is a plain
// TypeError with no status, no url field and no cause — and each engine words
// it differently, hence the three patterns. A stale Cloudflare Access session
// (which answers a login page instead of JavaScript) and a dropped connection
// land here too; all three want the same reload, so lumping them together is
// the honest grouping rather than a guess.
const CHUNK_LOAD_PATTERNS = [
  // Chromium
  "failed to fetch dynamically imported module",
  // Firefox
  "error loading dynamically imported module",
  // Safari / WebKit
  "importing a module script failed",
]

export function isChunkLoadError(err: unknown): boolean {
  const msg =
    err instanceof Error ? err.message : typeof err === "string" ? err : ""
  const low = msg.toLowerCase()
  return CHUNK_LOAD_PATTERNS.some((p) => low.includes(p))
}
