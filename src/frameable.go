package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// serveFrameable answers whether a page will let the Browser tab embed it.
// A site refusing to be framed (X-Frame-Options, CSP frame-ancestors) renders
// as a blank iframe, and the browser hides both the headers and the refusal
// from the embedding page — so only a server-side fetch can tell a refusal
// from a page that is merely slow. The answer is advisory: any failure to
// fetch reports "unknown" and the tab stays silent, since the browser (not
// titan) is the one that actually has to reach the page.
//
// GET /api/frameable?url=<http(s) url>&origin=<lasso's page origin>
func serveFrameable(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("url")
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		http.Error(w, "url must be an absolute http(s) URL", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		writeJSON(w, map[string]any{"frameable": nil})
		return
	}
	// Some sites answer a Go user agent differently from a browser.
	req.Header.Set("User-Agent", "Mozilla/5.0 (lasso frameable check)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, map[string]any{"frameable": nil})
		return
	}
	resp.Body.Close()
	ok, reason := frameableBy(resp.Header, r.URL.Query().Get("origin"))
	writeJSON(w, map[string]any{"frameable": ok, "reason": reason})
}

// frameableBy applies the browser's framing rules to a response's headers for
// an embedder at origin. CSP frame-ancestors, when present, supersedes
// X-Frame-Options, exactly as browsers treat it.
func frameableBy(h http.Header, origin string) (bool, string) {
	for _, csp := range h.Values("Content-Security-Policy") {
		for _, dir := range strings.Split(csp, ";") {
			f := strings.Fields(strings.TrimSpace(dir))
			if len(f) == 0 || !strings.EqualFold(f[0], "frame-ancestors") {
				continue
			}
			if frameAncestorsAllow(f[1:], origin) {
				return true, ""
			}
			return false, "frame-ancestors"
		}
	}
	switch strings.ToUpper(strings.TrimSpace(h.Get("X-Frame-Options"))) {
	case "DENY":
		return false, "x-frame-options"
	case "SAMEORIGIN":
		// Same-origin with lasso's own page is the one case it allows, and a
		// dev server on lasso's host is a different port, i.e. a different origin.
		return false, "x-frame-options"
	}
	return true, ""
}

// frameAncestorsAllow matches an embedder origin against a frame-ancestors
// source list: 'none', *, scheme sources, and host sources with an optional
// leading-wildcard subdomain and port. 'self' refers to the framed page's own
// origin, which lasso never is.
func frameAncestorsAllow(sources []string, origin string) bool {
	o, err := url.Parse(origin)
	if err != nil || o.Host == "" {
		return false
	}
	for _, s := range sources {
		s = strings.ToLower(s)
		switch {
		case s == "*":
			return true
		case s == "'none'" || s == "'self'":
			continue
		case strings.HasSuffix(s, ":") && !strings.Contains(s, "/"):
			if s == o.Scheme+":" {
				return true
			}
			continue
		}
		scheme, hostport := "", s
		if i := strings.Index(s, "://"); i >= 0 {
			scheme, hostport = s[:i], s[i+3:]
		}
		hostport = strings.TrimSuffix(strings.SplitN(hostport, "/", 2)[0], "/")
		if scheme != "" && scheme != o.Scheme {
			continue
		}
		host, port := hostport, ""
		if i := strings.LastIndex(hostport, ":"); i >= 0 && !strings.Contains(hostport[i:], "]") {
			host, port = hostport[:i], hostport[i+1:]
		}
		if port != "" && port != "*" && port != o.Port() {
			continue
		}
		oh := strings.ToLower(o.Hostname())
		if host == oh || (strings.HasPrefix(host, "*.") && strings.HasSuffix(oh, host[1:])) {
			return true
		}
	}
	return false
}
