// URL handling shared by the Browser tab's two modes. Embed and Live differ on
// exactly one thing — which machine a bare port means — and agree on the rest.

export const LOOPBACK = ["localhost", "127.0.0.1", "0.0.0.0", "::1", "[::1]"]

// normalize defaults a schemeless URL to https://, except for a loopback host,
// which gets http:// since a local server almost never has a certificate.
export function normalize(raw: string): string {
  const u = raw.trim()
  if (!u) return ""
  if (/^https?:\/\//i.test(u)) return u
  try {
    if (LOOPBACK.includes(new URL(`https://${u}`).hostname)) {
      return `http://${u}`
    }
  } catch {
    // Unparseable: fall through and let the page report it.
  }
  return `https://${u}`
}

// bareLocalPort matches the port-only shorthands — "5173" and ":5173" — and
// nothing else. What machine they mean is the caller's call.
export function bareLocalPort(raw: string): number | null {
  const s = raw.trim()
  const m = s.match(/^:?(\d{2,5})$/)
  if (!m) return null
  const n = Number(m[1])
  return Number.isInteger(n) && n >= 1 && n <= 65535 ? n : null
}

// resolveLive maps user input to a URL for the SHARED browser. That Chromium
// runs on lasso's own machine, so a bare port is that machine's localhost —
// not this page's hostname, which is what Embed means by it — and loopback
// URLs stay loopback. A URL with another scheme (about:blank, data:, file:,
// chrome://) is meant literally — a real Chromium can open those, where
// prefixing https:// would make "about" a hostname. Everything else follows
// normalize.
export function resolveLive(raw: string): string {
  const s = raw.trim()
  const port = bareLocalPort(s)
  if (port != null) return `http://localhost:${port}`
  // A named list rather than "letters then a colon", which would also match
  // "localhost:5173" and "myhost:8080".
  if (/^(about|data|file|chrome|view-source|blob):/i.test(s)) return s
  return normalize(s)
}
