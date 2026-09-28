package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// /cdp — the shared browser's one public entry, for the Browser tab and every
// agent alike (see browser.go).
//
//	/cdp                 websocket → the BROWSER target (/devtools/browser/<id>)
//	/cdp/devtools/...    passthrough (page and browser targets)
//	/cdp/json[/...]      passthrough, with every websocket URL in the answer
//	                     rewritten to point back through /cdp
//
// /cdp itself is the stable address: Chromium's browser id changes on every
// launch (an idle stop, a proxy change), and an agent configured with
// --wsEndpoint ws://<lasso>/cdp keeps working across all of them.
//
// Every other browser profile (browserprofiles.go) is its own Chromium, served
// the same way one level down: /cdp/p/<id>, /cdp/p/<id>/devtools/...,
// /cdp/p/<id>/json/.... Bare /cdp stays the default profile's, so nothing that
// was configured before profiles existed changes meaning.

// cdpProfilePrefix is the /cdp prefix a request path names and the profile it
// belongs to: "/cdp" for the default profile, "/cdp/p/<id>" for another.
// ok=false is a /cdp/p/ path with no id.
func cdpProfilePrefix(p string) (prefix, profile string, ok bool) {
	rest, found := strings.CutPrefix(p, "/cdp/p/")
	if !found {
		return "/cdp", defaultBrowserProfile, true
	}
	id, _, _ := strings.Cut(rest, "/")
	if id == "" {
		return "", "", false
	}
	return "/cdp/p/" + id, id, true
}

// cdpPathFor is the /cdp address of a profile's browser target.
func cdpPathFor(profile string) string {
	if profile == "" || profile == defaultBrowserProfile {
		return "/cdp"
	}
	return "/cdp/p/" + profile
}

// cdpUpstreamPath maps an inbound /cdp path onto Chromium's own. ok=false is a
// path the proxy does not serve.
func cdpUpstreamPath(p, browserPath string) (string, bool) {
	return cdpUpstreamPathAt("/cdp", p, browserPath)
}

// cdpUpstreamPathAt is cdpUpstreamPath under a profile's prefix.
func cdpUpstreamPathAt(prefix, p, browserPath string) (string, bool) {
	switch {
	case p == prefix || p == prefix+"/":
		return browserPath, true
	case strings.HasPrefix(p, prefix+"/devtools/"):
		return strings.TrimPrefix(p, prefix), true
	case p == prefix+"/json" || strings.HasPrefix(p, prefix+"/json/"):
		return strings.TrimPrefix(p, prefix), true
	}
	return "", false
}

// cdpTLS says the client reached lasso over TLS — directly, or through a
// terminating proxy that said so (cloudflared sets X-Forwarded-Proto). Unlike
// externalBaseURL this does NOT assume https for a non-loopback host: lasso is
// routinely reached over plain http on a tailnet address, where a wss:// URL
// would simply fail to connect.
func cdpTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	p := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	return strings.EqualFold(p, "https")
}

// cdpPublicHost is the host:port the client used to reach lasso.
func cdpPublicHost(r *http.Request) string {
	if h := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); h != "" {
		return h
	}
	return r.Host
}

// cdpWSBase is ws(s)://<host> as the client addresses lasso.
func cdpWSBase(r *http.Request) string {
	if cdpTLS(r) {
		return "wss://" + cdpPublicHost(r)
	}
	return "ws://" + cdpPublicHost(r)
}

// cdpHTTPBase is http(s)://<host> as the client addresses lasso.
func cdpHTTPBase(r *http.Request) string {
	if cdpTLS(r) {
		return "https://" + cdpPublicHost(r)
	}
	return "http://" + cdpPublicHost(r)
}

// cdpOriginAllowed is the cross-site websocket hijacking guard, and it is not
// optional. A browser lets any web page open a websocket to any origin and
// sends that page's Origin with it; Chromium's own defense (--remote-allow-
// origins) is exactly what the proxy has to switch off by deleting Origin on
// the way out. Without this check any site the user visited could reach a
// loopback lasso and drive — read, type into, navigate — the shared browser.
//
// No Origin is allowed: that is a CLI or agent client (chrome-devtools-mcp,
// Playwright), which is not a browser acting on a stranger's behalf. An Origin
// is allowed only when it names the host the request was sent to (or the one a
// fronting proxy says it was sent to), which is lasso's own page.
func cdpOriginAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false // includes "null": a sandboxed frame, a file:// page
	}
	oh := normHostPort(u.Scheme, u.Host)
	if strings.EqualFold(oh, normHostPort(u.Scheme, r.Host)) {
		return true
	}
	if xf := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); xf != "" {
		return strings.EqualFold(oh, normHostPort(u.Scheme, xf))
	}
	return false
}

// normHostPort drops the scheme's default port, so https://h and a Host of
// h:443 compare equal.
func normHostPort(scheme, hp string) string {
	h, p, err := net.SplitHostPort(hp)
	if err != nil {
		return hp
	}
	if (strings.EqualFold(scheme, "http") && p == "80") || (strings.EqualFold(scheme, "https") && p == "443") {
		if strings.Contains(h, ":") {
			return "[" + h + "]"
		}
		return h
	}
	return hp
}

// rewriteCDPJSON rewrites the websocket URLs in a /json answer so a client
// follows them back through /cdp instead of dialing Chromium's loopback port —
// which, for anyone not on lasso's own machine, is unreachable, and for anyone
// who is, would bypass both the auth gate and the Origin guard.
func rewriteCDPJSON(body []byte, wsBase string) ([]byte, error) {
	return rewriteCDPJSONAt(body, wsBase, "/cdp")
}

// rewriteCDPJSONAt is rewriteCDPJSON for the browser served under prefix.
func rewriteCDPJSONAt(body []byte, wsBase, prefix string) ([]byte, error) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	switch t := v.(type) {
	case map[string]any:
		rewriteCDPTarget(t, wsBase, prefix)
	case []any:
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				rewriteCDPTarget(m, wsBase, prefix)
			}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // URLs carry & and = verbatim; & is legal but noisy
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func rewriteCDPTarget(m map[string]any, wsBase, prefix string) {
	if s, ok := m["webSocketDebuggerUrl"].(string); ok {
		if u, err := url.Parse(s); err == nil {
			m["webSocketDebuggerUrl"] = wsBase + cdpPublicPathAt(prefix, u.Path)
		} else {
			delete(m, "webSocketDebuggerUrl")
		}
	}
	if s, ok := m["devtoolsFrontendUrl"].(string); ok {
		if r, ok := rewriteFrontendURLAt(s, wsBase, prefix); ok {
			m["devtoolsFrontendUrl"] = r
		} else {
			delete(m, "devtoolsFrontendUrl")
		}
	}
}

// cdpPublicPath is the /cdp path for one of Chromium's websocket paths. The
// browser target maps to bare /cdp — the stable address — rather than to its
// per-launch id.
func cdpPublicPath(p string) string { return cdpPublicPathAt("/cdp", p) }

func cdpPublicPathAt(prefix, p string) string {
	if strings.HasPrefix(p, "/devtools/browser/") {
		return prefix
	}
	return prefix + p
}

// rewriteFrontendURL repoints a DevTools frontend link's ws= parameter (a
// scheme-less host/path) through /cdp, switching it to wss= when the client is
// on TLS. Anything not in that shape is dropped rather than half-rewritten: a
// link that still names 127.0.0.1:<port> is worse than no link.
func rewriteFrontendURL(s, wsBase string) (string, bool) {
	return rewriteFrontendURLAt(s, wsBase, "/cdp")
}

func rewriteFrontendURLAt(s, wsBase, prefix string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil {
		return "", false
	}
	q := u.Query()
	ws := q.Get("ws")
	if ws == "" {
		ws = q.Get("wss")
	}
	_, path, found := strings.Cut(ws, "/")
	if !found || !strings.HasPrefix("/"+path, "/devtools/") {
		return "", false
	}
	scheme, host, _ := strings.Cut(wsBase, "://")
	q.Del("ws")
	q.Del("wss")
	q.Set(scheme, host+cdpPublicPathAt(prefix, "/"+path))
	u.RawQuery = q.Encode()
	if u.Scheme == "" && strings.HasPrefix(u.Path, "/devtools/") {
		// A relative link to Chromium's bundled frontend: served through the
		// same passthrough as everything else under /devtools/.
		u.Path = prefix + u.Path
	}
	return u.String(), true
}

// cdpTransport dials Chromium's loopback port. Keep-alives are pooled by the
// outbound host, which is 127.0.0.1:<port> — unique per launch, so a relaunch
// can never serve a request down the previous instance's connection.
var cdpTransport = &http.Transport{
	Proxy:                 nil, // loopback; never through an HTTP(S)_PROXY from the environment
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	ResponseHeaderTimeout: 30 * time.Second,
	IdleConnTimeout:       60 * time.Second,
}

// serveCDPRouted is the /cdp handler main mounts: it picks the profile the
// path names and hands the request to that profile's browser. The Origin guard
// runs first, before anything about the path is looked up.
func serveCDPRouted(w http.ResponseWriter, r *http.Request) {
	if !cdpOriginAllowed(r) {
		http.Error(w, "cross-origin request to /cdp refused", http.StatusForbidden)
		return
	}
	prefix, profile, ok := cdpProfilePrefix(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	m, err := browserFor(profile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	m.serveCDPAt(w, r, prefix)
}

// serveCDP is the default profile's /cdp handler.
func (m *browserManager) serveCDP(w http.ResponseWriter, r *http.Request) {
	m.serveCDPAt(w, r, "/cdp")
}

// serveCDPAt serves one profile's browser under prefix (/cdp or /cdp/p/<id>).
func (m *browserManager) serveCDPAt(w http.ResponseWriter, r *http.Request, prefix string) {
	if !cdpOriginAllowed(r) {
		http.Error(w, "cross-origin request to /cdp refused", http.StatusForbidden)
		return
	}
	if _, ok := cdpUpstreamPathAt(prefix, r.URL.Path, "/devtools/browser/x"); !ok {
		http.NotFound(w, r)
		return
	}
	if m == nil {
		http.Error(w, "the shared browser is not configured", http.StatusServiceUnavailable)
		return
	}
	m.begin()
	defer m.end()
	p, err := m.ensure(r.Context())
	if err != nil {
		http.Error(w, "shared browser unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	upstream, _ := cdpUpstreamPathAt(prefix, r.URL.Path, p.wsPath)
	isJSON := strings.HasPrefix(upstream, "/json")
	wsBase := cdpWSBase(r)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = p.target()
			pr.Out.URL.Path = upstream
			pr.Out.URL.RawPath = ""
			// Chromium answers only a Host that is an IP or "localhost" — a DNS
			// rebinding guard — so the client's own Host cannot pass through.
			pr.Out.Host = p.target()
			// Chromium ≥111 refuses a websocket whose Origin is not in
			// --remote-allow-origins. The Origin was checked above; Chromium
			// never needs to see it.
			pr.Out.Header.Del("Origin")
			// lasso's credentials are lasso's, not Chromium's.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del(internalCDPHeader)
			if isJSON {
				// The body is rewritten below, which needs it uncompressed.
				pr.Out.Header.Del("Accept-Encoding")
			}
		},
		Transport: cdpTransport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("cdp: %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "shared browser: "+err.Error(), http.StatusBadGateway)
		},
	}
	if isJSON {
		rp.ModifyResponse = func(resp *http.Response) error {
			if !strings.Contains(resp.Header.Get("Content-Type"), "json") {
				return nil
			}
			b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			if err != nil {
				return err
			}
			if nb, err := rewriteCDPJSONAt(b, wsBase, prefix); err == nil {
				b = nb
			}
			resp.Body = io.NopCloser(bytes.NewReader(b))
			resp.ContentLength = int64(len(b))
			resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
			return nil
		}
	}
	rp.ServeHTTP(w, r)
}

// withCDPAuth is /cdp's own gate. /cdp is exempt from withAuthExcept for the
// same reason /mcp is — its clients are agents that carry a token, not a
// browser holding UI_AUTH — so it has to apply the right rule itself:
//
//   - MCP_OAUTH set: what /mcp accepts (a lasso bearer token, or the UI_AUTH
//     basic credentials), and a token's host scope must reach lasso's own
//     machine, which is where the browser runs. Plus one case /mcp does not
//     have: with UI_AUTH unset, a same-origin browser request (lasso's own
//     Browser tab) passes without a token. That configuration leaves every
//     other UI route — /terminal/ included — open to whoever reaches lasso, so
//     demanding a bearer token here would lock out only lasso's own page, which
//     cannot mint one; and the Origin guard has already refused any page that
//     is not lasso's.
//   - UI_AUTH set: the UI_AUTH basic credentials. A browser sends the ones it
//     cached for the page on its websocket too, which is how /terminal/ works.
//   - neither: open, the same trust model as /mcp and /api/file.
//
// Cloudflare Access (gate.wrap) still fronts all of it, unchanged.
func withCDPAuth(next http.Handler, user, pass string, hasAuth bool) http.Handler {
	if oauthCfg.Enabled {
		gated := withMCPAuth(cdpScopeCheck(next), user, pass, hasAuth)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !hasAuth && r.Header.Get("Authorization") == "" &&
				r.Header.Get("Origin") != "" && cdpOriginAllowed(r) {
				next.ServeHTTP(w, r)
				return
			}
			gated.ServeHTTP(w, r)
		})
	}
	if hasAuth {
		return withAuth(next, user, pass, true)
	}
	return next
}

// internalCDPHeader carries internalCDPToken: how lasso's own
// chrome-devtools-mcp children (browsermcp.go) get through to /cdp.
const internalCDPHeader = "X-Lasso-Internal"

// internalCDPToken is minted once per process from crypto/rand and lives only
// in memory and in the argv of the children it is handed to — never logged,
// never stored, never sent to a client. A child needs it because the agent's
// credential stops at /browser-mcp: the child is lasso's process, dialing
// lasso's loopback, and has nothing of its own to present to /cdp's gate.
var internalCDPToken = newInternalCDPToken()

func newInternalCDPToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on a supported platform; a predictable
		// token would be a hole, so there is no fallback to one.
		log.Fatalf("cdp: minting the internal token: %v", err)
	}
	return hex.EncodeToString(b)
}

// internalCDPRequest reports whether r is one of lasso's own children calling
// /cdp: the path must be /cdp's and the header must match exactly (compared
// in constant time). Anything else — a wrong or empty header, the right header
// on another path — is an ordinary request and gets the ordinary rules.
func internalCDPRequest(r *http.Request) bool {
	if r.URL.Path != "/cdp" && !strings.HasPrefix(r.URL.Path, "/cdp/") {
		return false
	}
	got := r.Header.Get(internalCDPHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(internalCDPToken)) == 1
}

// withInternalCDP lets a request carrying the internal token straight through
// to /cdp, ahead of EVERY other gate — the Access header gate included, which
// is why this wraps the outermost handler instead of living in withCDPAuth: with
// -require-access-header on, a loopback dial from lasso's own child carries no
// Cloudflare identity and would be refused before withCDPAuth ever saw it.
// serveCDP still applies the Origin guard (the child sends no Origin).
func withInternalCDP(outer, cdp http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if internalCDPRequest(r) {
			cdp.ServeHTTP(w, r)
			return
		}
		outer.ServeHTTP(w, r)
	})
}

// cdpScopeCheck refuses a bearer token whose host scope does not include lasso's
// own machine. The shared_browser tool refuses such a caller too; without this
// the same credential could skip the tool and dial /cdp directly.
func cdpScopeCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ti := auth.TokenInfoFromContext(r.Context()); ti != nil {
			cs := callerFrom(&mcp.CallToolRequest{Extra: &mcp.RequestExtra{TokenInfo: ti}})
			if !cs.allows("local") {
				http.Error(w, "this credential's scope does not include lasso's own machine, where the shared browser runs", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// lassoBaseHeader carries the base URL an MCP request reached lasso on into the
// tool handlers. The SDK hands a tool the request's headers (req.Extra.Header)
// but not its Host, which Go keeps out of the header map — and the
// shared_browser tool needs it to hand back an absolute /cdp endpoint.
const lassoBaseHeader = "X-Lasso-Request-Base"

// withRequestBase stamps lassoBaseHeader onto every MCP request. It is SET, not
// added, so a client cannot supply its own and steer the endpoint an agent is
// told to connect to.
func withRequestBase(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(lassoBaseHeader, cdpHTTPBase(r))
		next.ServeHTTP(w, r)
	})
}
