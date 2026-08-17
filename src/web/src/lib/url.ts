// Small helpers for reflecting app state in the URL query string (e.g.
// ?host=minime). We use real query params rather than the hash so links are
// conventional and the fragment stays free. Updates use replaceState so they
// don't pile up history entries on every host change.
//
// Only state lasso OWNS belongs here, which is now just the active host.
// herdr's focused pane does not — it is one global per herdr session, shared
// with the TUI and every other lasso client, so a URL that named it would let a
// browser Back re-point it for everyone (and a shared link steal focus on open).

export function getQueryParam(key: string): string | null {
  return new URLSearchParams(window.location.search).get(key)
}

// setQueryParam sets (or, when value is null/empty, removes) one query param,
// leaving the path and other params untouched. The hash is intentionally
// dropped — we've migrated off fragment-based state. Uses replaceState so a
// plain host change doesn't pile up history entries.
export function setQueryParam(key: string, value: string | null) {
  writeQueryParam(key, value, false)
}

// pushQueryParam is like setQueryParam but adds a history entry instead of
// replacing the current one — for navigations the browser Back button should
// reverse (focusing a pane on another host, say).
export function pushQueryParam(key: string, value: string | null) {
  writeQueryParam(key, value, true)
}

// setQueryParams sets (or removes, when null/empty) several params in one
// history operation — so clearing the params we no longer honor is a single
// replace rather than one per key.
export function setQueryParams(params: Record<string, string | null>) {
  writeQueryParams(params, false)
}

function writeQueryParam(key: string, value: string | null, push: boolean) {
  writeQueryParams({ [key]: value }, push)
}

function writeQueryParams(
  params: Record<string, string | null>,
  push: boolean
) {
  const url = new URL(window.location.href)
  for (const [key, value] of Object.entries(params)) {
    if (value == null || value === "") url.searchParams.delete(key)
    else url.searchParams.set(key, value)
  }
  const qs = url.searchParams.toString()
  const next = url.pathname + (qs ? `?${qs}` : "")
  // A push that lands on the URL we're already at would be a dead Back step:
  // the user presses Back and nothing changes. Replace instead — the caller's
  // navigation was real (it focused a pane), it just left lasso's own state
  // where it was.
  if (push && next !== window.location.pathname + window.location.search) {
    window.history.pushState(null, "", next)
  } else {
    window.history.replaceState(null, "", next)
  }
}
