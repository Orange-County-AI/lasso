import * as React from "react"

// A lazily-imported chunk can 404 out from under a tab that is already open.
// Every /assets/ filename carries a content hash, and lasso self-updates by
// swapping its own binary — which swaps the embedded bundle with it. So a tab
// loaded before the update asks for a chunk name the new binary has never
// heard of, the dynamic import rejects, and React.lazy turns that rejection
// into a render error. With no boundary above it that unmounts the entire app:
// a blank pane, recoverable only by the reload the user had to work out alone.
//
// A reload is genuinely the right answer here — it fetches the new index.html
// and with it the chunk names that exist now. So do it automatically, once.

const RELOAD_KEY = "lasso.chunk-reload"
// A reload that didn't help means the chunk is missing for some other reason,
// and reloading again would just loop. One attempt per minute per tab.
const RELOAD_WINDOW_MS = 60_000

function reloadedRecently(): boolean {
  try {
    const at = Number(sessionStorage.getItem(RELOAD_KEY) ?? 0)
    return Number.isFinite(at) && Date.now() - at < RELOAD_WINDOW_MS
  } catch {
    // Private-mode / blocked storage: without a stamp we can't prove we
    // haven't already tried, so don't reload at all — the boundary shows.
    return true
  }
}

function stampReload() {
  try {
    sessionStorage.setItem(RELOAD_KEY, String(Date.now()))
  } catch {}
}

// withChunkRecovery wraps a chunk loader with that recovery: one retry (a
// connection dropped mid-fetch looks identical to a chunk that moved, and
// ruling it out costs one request), then a single guarded reload. If the
// reload already happened, the error is rethrown for a boundary to render.
// Separate from lazyWithReload so it can be tested without rendering.
export function withChunkRecovery<T>(
  load: () => Promise<T>
): () => Promise<T> {
  return () =>
    load().catch(async (err: unknown) => {
      try {
        return await load()
      } catch {}
      if (reloadedRecently()) throw err
      stampReload()
      window.location.reload()
      // The page is on its way out. Resolving or rejecting now would render
      // something in the last frame before it goes; hang instead.
      return new Promise<never>(() => {})
    })
}

// lazyWithReload is React.lazy over that loader.
// The `any` mirrors React.lazy's own bound; anything narrower stops the
// component's real props from being inferred at the call site.
// biome-ignore lint/suspicious/noExplicitAny: see above
export function lazyWithReload<T extends React.ComponentType<any>>(
  load: () => Promise<{ default: T }>
): React.LazyExoticComponent<T> {
  return React.lazy<T>(withChunkRecovery(load))
}
