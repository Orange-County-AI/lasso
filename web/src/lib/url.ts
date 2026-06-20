// Small helpers for reflecting app state in the URL query string (e.g.
// ?view=herdr&host=minime). We use real query params rather than the hash so
// links are conventional and the fragment stays free. Updates use
// replaceState so they don't pile up history entries on every tab/host change.

export function getQueryParam(key: string): string | null {
  return new URLSearchParams(window.location.search).get(key)
}

// setQueryParam sets (or, when value is null/empty, removes) one query param,
// leaving the path and other params untouched. The hash is intentionally
// dropped — we've migrated off fragment-based state. Uses replaceState so plain
// tab/host changes don't pile up history entries.
export function setQueryParam(key: string, value: string | null) {
  writeQueryParam(key, value, false)
}

// pushQueryParam is like setQueryParam but adds a history entry instead of
// replacing the current one — for navigations the browser Back button should
// reverse (e.g. focusing a Grid pane in Herdr should let Back return to Grid).
export function pushQueryParam(key: string, value: string | null) {
  writeQueryParam(key, value, true)
}

// setQueryParams / pushQueryParams set (or remove, when null/empty) several
// params in one history operation — so a multi-param navigation (e.g. focusing
// a pane writes view+host+pane at once) is a single Back step, not one per key.
export function setQueryParams(params: Record<string, string | null>) {
  writeQueryParams(params, false)
}

export function pushQueryParams(params: Record<string, string | null>) {
  writeQueryParams(params, true)
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
  if (push) window.history.pushState(null, "", next)
  else window.history.replaceState(null, "", next)
}
