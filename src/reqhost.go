package main

import "net/http"

// hostHeader is how a browser tab tells lasso which host THIS tab is looking at.
//
// The active host used to be one process-wide pointer, so two tabs could not sit
// on two machines: switching in one yanked the other. It is now per client, and
// a client announces itself on every request. Two carriers, because one cannot
// cover both call shapes: fetch() can set a header, but an EventSource and an
// <iframe> src can only carry a query string.
const hostHeader = "X-Lasso-Host"

// requestHost is the host a request is addressed to, or "" for the default one.
// The query string wins over the header so a hand-typed or deep-linked URL
// (?host=norm) is not overridden by whatever the page's fetch wrapper attaches.
func requestHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	if h := r.URL.Query().Get("host"); h != "" {
		return h
	}
	return r.Header.Get(hostHeader)
}

// reqBackend resolves the backend a request should run against. explicit is a
// host named by the request's own payload — the sidebar's `host` body/form field,
// which follows the FOCUSED pane and is deliberately independent of the tab's
// selected host (see panehost.go) — and wins when present. Otherwise the tab's
// own host answers, and an unidentified caller gets the default host.
func reqBackend(r *http.Request, explicit string) (Backend, error) {
	if explicit != "" {
		return namedHostBackend(explicit)
	}
	return namedHostBackend(requestHost(r))
}
