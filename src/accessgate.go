package main

import (
	"log"
	"net/http"
	"os"
	"strings"
)

// Cloudflare Access edge-identity gate.
//
// On the fleet workspace boxes lasso is published on a hostname fronted by
// Cloudflare Access, exactly like the ttyd browser terminal already is (see
// workspace-ttyd-paste-proxy.service, which runs the paste proxy with
// --auth-header Cf-Access-Authenticated-User-Email, and start-ttyd.sh, which
// hands the same header name to ttyd when it has to own the front door itself).
// This file gives lasso the same contract.
//
// THE HEADER IS NEVER TRUSTED UNLESS THE FLAG IS SET. Cf-Access-* headers are
// client-supplied bytes to any HTTP server; they only mean anything because
// Cloudflare STRIPS client-supplied Cf-Access-* headers on a hostname it
// protects and re-adds its own after verifying the JWT. So:
//
//   - Turning the gate on is opt-in per deployment (-require-access-header, or
//     LASSO_REQUIRE_ACCESS_HEADER=1) and it is ONLY safe when every path to the
//     listener passes through that edge. Behind anything else — a bare port, a
//     reverse proxy that forwards client headers verbatim — the gate is a
//     formality anyone can satisfy with `curl -H`.
//   - Nothing else in lasso reads the header. When the flag is off it is inert.
//
// The gate runs OUTSIDE every other handler (before UI_AUTH, before the MCP
// OAuth check, before the ttyd reverse proxies), so it covers /api/*, /mcp,
// /terminal/, /shell/, their websocket upgrades, /api/file*, the OAuth
// endpoints and the SPA alike — a websocket upgrade is an ordinary HTTP request
// until the handler that hijacks it sees it, and this gate answers first.

const accessEmailHeader = "Cf-Access-Authenticated-User-Email"

// accessGate is the resolved configuration: whether the header is required at
// all, and (optionally) the exact set of identities allowed through it.
type accessGate struct {
	require bool
	// allowed, when non-empty, restricts to these lowercased emails. Empty
	// means "any identity Access vouched for".
	allowed map[string]bool
}

// newAccessGate builds the gate from the flag/env pair. emails is a
// comma-separated list; blanks are ignored and case is normalised.
func newAccessGate(require bool, emails string) accessGate {
	g := accessGate{require: require}
	for _, e := range strings.Split(emails, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if g.allowed == nil {
			g.allowed = map[string]bool{}
		}
		g.allowed[e] = true
	}
	return g
}

// allows reports whether a request carrying this header value gets through.
func (g accessGate) allows(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	if len(g.allowed) == 0 {
		return true
	}
	return g.allowed[email]
}

// wrap gates every request behind a non-empty (and, when an allowlist is set,
// permitted) Cf-Access-Authenticated-User-Email header. A no-op when the gate
// is off, so the header stays untrusted by default.
func (g accessGate) wrap(next http.Handler) http.Handler {
	if !g.require {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.allows(r.Header.Get(accessEmailHeader)) {
			// No WWW-Authenticate: there is no credential the client can
			// supply here — the identity has to come from the edge.
			http.Error(w, "forbidden: this lasso requires a Cloudflare Access identity", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// envOn reads a boolean-ish environment variable ("1", "true", "yes", "on").
func envOn(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// logAccessGateStatus prints the one startup line that says which gates are
// live, so a box's journal answers "what is guarding this front door?" without
// anyone guessing from flags.
func (g accessGate) logStatus(listen string, hasBasicAuth bool) {
	if !g.require {
		return
	}
	who := "any Access-authenticated identity"
	if len(g.allowed) > 0 {
		who = strings.Join(sortedKeys(g.allowed), ", ")
	}
	log.Printf("access:   REQUIRED — every route (incl. /mcp, /terminal/, /shell/, websockets, /api/file*) needs %s: %s",
		accessEmailHeader, who)
	log.Printf("access:   this is only sound behind an edge that STRIPS client-supplied Cf-Access-* headers (Cloudflare Access on a protected hostname). listen=%s basic-auth=%v",
		listen, hasBasicAuth)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// small n; insertion sort keeps the log deterministic without a new import
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
