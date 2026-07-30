package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// OAuth 2.1 authorization server for the MCP endpoint.
//
// /mcp is unauthenticated by default (see CLAUDE.md) — that's fine on loopback
// or behind Cloudflare Access, and it stays the default so existing setups
// don't break. Setting MCP_OAUTH=client_id:client_secret turns lasso into a
// small authorization server for its own /mcp resource, which is what remote
// MCP clients that speak OAuth need when there is no Access gate in front.
//
// Two grants, because the two kinds of caller want different things:
//
//	client_credentials — machine-to-machine. A script, a CLI, or a Claude Code
//	  session posts the configured client_id/secret and gets a bearer token. No
//	  human in the loop. This is the grant this feature was asked for.
//	authorization_code + PKCE — what claude.ai / Claude Desktop custom connectors
//	  actually use; their "Advanced settings → OAuth Client ID / Client Secret"
//	  fields are a *pre-registered* client for this flow (Anthropic's connector
//	  infrastructure does not support client_credentials — every connection
//	  requires user consent). Clients that don't pre-register get there through
//	  Dynamic Client Registration (RFC 7591) instead.
//
// The consent screen (/oauth/authorize) is deliberately NOT exempt from
// UI_AUTH, so approving a client requires the same credentials as the rest of
// the app — and behind Cloudflare Access it inherits the Access policy too.
// That is the human gate that makes open Dynamic Client Registration safe:
// anyone may register, nobody gets a token without passing the door.

const (
	// oauthScope is the single scope this resource understands. Kept to one so
	// clients that omit `scope` entirely (most do) still get a usable token.
	oauthScope = "mcp"
	// Access tokens are short so a leaked one ages out; refresh tokens are long
	// so a connector survives lasso restarts and self-updates.
	oauthAccessTTL  = time.Hour
	oauthRefreshTTL = 30 * 24 * time.Hour
	oauthCodeTTL    = 5 * time.Minute
)

// oauthConf is the resolved MCP_OAUTH configuration, set once at startup by
// loadOAuthConfig. Enabled=false means /mcp keeps its historical open behavior.
type oauthConf struct {
	Enabled      bool
	ClientID     string
	ClientSecret string
	// RedirectURIs, when non-empty (MCP_OAUTH_REDIRECT_URIS), restricts the
	// pre-registered client to these exact callbacks. Empty means "any https
	// or loopback URI, shown to the human on the consent screen" — necessary
	// because a connector's callback (e.g. https://claude.ai/api/mcp/auth_callback)
	// isn't known when the secret is minted.
	RedirectURIs []string
}

var oauthCfg oauthConf

// loadOAuthConfig reads MCP_OAUTH (and its optional redirect allowlist) from the
// environment, mirroring UI_AUTH: credentials come from the environment, never
// argv, so they don't leak through `ps`.
func loadOAuthConfig() oauthConf {
	id, secret, ok := parseAuth(os.Getenv("MCP_OAUTH"))
	if !ok || secret == "" {
		return oauthConf{}
	}
	c := oauthConf{Enabled: true, ClientID: id, ClientSecret: secret}
	for _, u := range strings.Split(os.Getenv("MCP_OAUTH_REDIRECT_URIS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			c.RedirectURIs = append(c.RedirectURIs, u)
		}
	}
	return c
}

// ---------------------------------------------------------------------------
// storage
// ---------------------------------------------------------------------------

// oauthSchema is appended to dbSchema (db.go). Codes and tokens are stored as
// SHA-256 hashes: a stolen lasso.db yields no usable credential. Client secrets
// for dynamically-registered clients are hashed for the same reason.
const oauthSchema = `
CREATE TABLE IF NOT EXISTS oauth_clients (
  client_id     TEXT PRIMARY KEY,
  secret_hash   TEXT NOT NULL DEFAULT '',
  redirect_uris TEXT NOT NULL DEFAULT '[]',
  name          TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL DEFAULT '',
  -- host/mcp_scope make a credential carry WHO is calling and how far it may
  -- reach (see callerscope.go). Only the CLI (lasso mcp-client add) ever sets
  -- them, so a non-empty host also marks a client as operator-provisioned --
  -- which is what licenses it to use client_credentials, unlike an open-DCR
  -- registration.
  host          TEXT NOT NULL DEFAULT '',
  mcp_scope     TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS oauth_codes (
  code_hash        TEXT PRIMARY KEY,
  client_id        TEXT NOT NULL,
  redirect_uri     TEXT NOT NULL DEFAULT '',
  code_challenge   TEXT NOT NULL DEFAULT '',
  scope            TEXT NOT NULL DEFAULT '',
  resource         TEXT NOT NULL DEFAULT '',
  expires_at       TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS oauth_tokens (
  token_hash TEXT PRIMARY KEY,
  kind       TEXT NOT NULL DEFAULT 'access',
  client_id  TEXT NOT NULL,
  scope      TEXT NOT NULL DEFAULT '',
  expires_at TEXT NOT NULL DEFAULT ''
);
`

// oauthClient is a registered client. A blank SecretHash means a public client
// (PKCE-only, no secret) — claude.ai registers as one when it uses DCR.
type oauthClient struct {
	ID           string
	SecretHash   string
	RedirectURIs []string
	Name         string
	// Static marks the MCP_OAUTH client. It and operator-provisioned host
	// clients (Host != "") are the only ones allowed to use client_credentials —
	// a dynamically-registered client obtaining machine-to-machine tokens would
	// be an open door, but a credential an operator minted out-of-band for a
	// named host is exactly the machine-to-machine case.
	Static bool
	// Host is the lasso host whose agents this credential's callers run on, and
	// Scope is how far they may reach ("self" / "fleet"). Both empty means the
	// caller is not host-identified and keeps the historical fleet-wide view.
	// See callerscope.go — this is the whole point of the per-host clients: the
	// caller's host is derived from the token, never asserted by the caller.
	Host  string
	Scope string
}

func hashToken(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// randToken returns 32 bytes of crypto-random data as an unpadded base64url
// string — used for authorization codes, access/refresh tokens, and generated
// DCR client credentials.
func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// lookupOAuthClient resolves a client_id, checking the MCP_OAUTH client first so
// it can't be shadowed by a DCR registration of the same id.
func lookupOAuthClient(id string) (oauthClient, bool) {
	if id == "" {
		return oauthClient{}, false
	}
	if oauthCfg.Enabled && subtle.ConstantTimeCompare([]byte(id), []byte(oauthCfg.ClientID)) == 1 {
		return oauthClient{
			ID:           oauthCfg.ClientID,
			SecretHash:   hashToken(oauthCfg.ClientSecret),
			RedirectURIs: oauthCfg.RedirectURIs,
			Name:         "lasso (pre-registered)",
			Static:       true,
			// No Host: the pre-registered credential is shared by whatever the
			// operator points at it (their own connectors, the CLI), so it names no
			// machine and keeps the fleet-wide view it has always had.
			Scope: scopeFleet,
		}, true
	}
	var c oauthClient
	var uris string
	err := db.QueryRow(
		`SELECT client_id, secret_hash, redirect_uris, name, host, mcp_scope FROM oauth_clients WHERE client_id = ?`, id,
	).Scan(&c.ID, &c.SecretHash, &uris, &c.Name, &c.Host, &c.Scope)
	if err != nil {
		return oauthClient{}, false
	}
	_ = json.Unmarshal([]byte(uris), &c.RedirectURIs)
	return c, true
}

// authenticateClient resolves the client from client_secret_basic (the RFC's
// preferred form) or client_secret_post, and verifies the secret when the client
// has one. Public clients (no secret) authenticate by PKCE alone.
func authenticateClient(r *http.Request) (oauthClient, bool) {
	id, secret, ok := r.BasicAuth()
	if ok {
		// RFC 6749 §2.3.1 form-urlencodes credentials before base64-ing them.
		if v, err := url.QueryUnescape(id); err == nil {
			id = v
		}
		if v, err := url.QueryUnescape(secret); err == nil {
			secret = v
		}
	} else {
		id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	c, found := lookupOAuthClient(id)
	if !found {
		return oauthClient{}, false
	}
	if c.SecretHash == "" {
		return c, secret == ""
	}
	if subtle.ConstantTimeCompare([]byte(hashToken(secret)), []byte(c.SecretHash)) != 1 {
		return oauthClient{}, false
	}
	return c, true
}

// redirectURIAllowed decides where an authorization code may be delivered.
// Registered clients get exact matching. The pre-registered MCP_OAUTH client
// gets exact matching too when MCP_OAUTH_REDIRECT_URIS is set; otherwise any
// https or loopback callback is accepted and the exact URI is rendered on the
// consent screen, so the human approving it sees where the code will go.
func redirectURIAllowed(c oauthClient, redirect string) bool {
	if redirect == "" {
		return false
	}
	for _, u := range c.RedirectURIs {
		if u == redirect {
			return true
		}
	}
	if !c.Static || len(c.RedirectURIs) > 0 {
		return false
	}
	u, err := url.Parse(redirect)
	if err != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" && u.Host != "" {
		return true
	}
	return u.Scheme == "http" && isLoopback(u.Host)
}

// purgeExpiredOAuth drops codes and tokens whose lifetime has run out. Called
// opportunistically from the token endpoint — the tables are tiny and this
// keeps them from growing without a background sweeper.
func purgeExpiredOAuth() {
	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = db.Exec(`DELETE FROM oauth_codes WHERE expires_at < ?`, now)
	// expires_at = '' means "never expires" (see issueToken). It must be excluded
	// explicitly: SQLite compares strings, and '' sorts BEFORE any timestamp, so a
	// bare `expires_at < now` would delete exactly the tokens meant to outlive
	// everything.
	_, _ = db.Exec(`DELETE FROM oauth_tokens WHERE expires_at != '' AND expires_at < ?`, now)
}

// issueToken mints an access or refresh token and records its hash.
//
// A ttl of zero or less mints a token that NEVER expires, stored as an empty
// expires_at. That is the default for the machine tokens `lasso mcp-client
// token` hands to a host: they sit in that host's MCP client config
// unattended, where a rolling expiry is not a security win but an outage on a
// timer. Such a token is revoked by removing its client
// (`lasso mcp-client rm`), which drops every token issued to it.
func issueToken(kind, clientID, scope string, ttl time.Duration) (string, error) {
	tok, err := randToken()
	if err != nil {
		return "", err
	}
	expires := ""
	if ttl > 0 {
		expires = time.Now().UTC().Add(ttl).Format(time.RFC3339)
	}
	_, err = db.Exec(
		`INSERT INTO oauth_tokens (token_hash, kind, client_id, scope, expires_at) VALUES (?, ?, ?, ?, ?)`,
		hashToken(tok), kind, clientID, scope, expires,
	)
	if err != nil {
		return "", err
	}
	return tok, nil
}

// ---------------------------------------------------------------------------
// discovery metadata
// ---------------------------------------------------------------------------

// externalBaseURL reconstructs the origin the client actually reached, which is
// what every URL in the OAuth metadata has to be built from. lasso is normally
// behind the Cloudflare tunnel, so the forwarded headers are authoritative;
// falling back to r.Host covers direct loopback/tailnet use.
func externalBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.TrimSpace(strings.Split(p, ",")[0])
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	// A tunnelled request arrives over plain HTTP on loopback but the client
	// spoke HTTPS; without X-Forwarded-Proto, assume TLS for any non-loopback
	// host, since OAuth clients reject http:// endpoints that aren't loopback.
	if r.Header.Get("X-Forwarded-Proto") == "" && r.TLS == nil && !isLoopback(host) {
		scheme = "https"
	}
	return scheme + "://" + host
}

// writeOAuthJSON writes a discovery/response document with the permissive CORS
// headers OAuth clients need — these documents are public by definition, and
// browser-based clients fetch them cross-origin.
func writeOAuthJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// oauthCORS answers the preflight browser clients send before POSTing to the
// token and registration endpoints.
func oauthCORS(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodOptions {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
	w.Header().Set("Access-Control-Max-Age", "3600")
	w.WriteHeader(http.StatusNoContent)
	return true
}

// oauthDisabled 404s every OAuth route when MCP_OAUTH is unset, and reports
// whether it did. This is load-bearing, not tidiness: lasso's documented
// Cloudflare-Access deployment depends on the origin advertising NO OAuth
// metadata, so that Access itself acts as the authorization server (see
// CLAUDE.md). Serving discovery documents for an authorization server that
// can't issue tokens would send those clients down a dead end.
func oauthDisabled(w http.ResponseWriter) bool {
	if oauthCfg.Enabled {
		return false
	}
	http.Error(w, "404 page not found", http.StatusNotFound)
	return true
}

// serveProtectedResourceMetadata implements RFC 9728. The 401 from /mcp points
// here via WWW-Authenticate; the client reads it to discover which
// authorization server to talk to (lasso itself).
func serveProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if oauthCORS(w, r) || oauthDisabled(w) {
		return
	}
	base := externalBaseURL(r)
	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"scopes_supported":         []string{oauthScope},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Lasso agent orchestrator",
	})
}

// serveAuthServerMetadata implements RFC 8414. grant_types_supported is the
// field that tells a client client_credentials is on the table.
func serveAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	if oauthCORS(w, r) || oauthDisabled(w) {
		return
	}
	base := externalBaseURL(r)
	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"scopes_supported":                      []string{oauthScope},
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token", "client_credentials"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post", "none"},
	})
}

// ---------------------------------------------------------------------------
// dynamic client registration (RFC 7591)
// ---------------------------------------------------------------------------

// serveOAuthRegister lets a client register itself. Registration is open — the
// MCP spec expects that — and safe here because a registered client still can't
// obtain a token without a human passing UI_AUTH/Access and approving the
// consent screen, and it can never use client_credentials.
func serveOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if oauthCORS(w, r) || oauthDisabled(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeOAuthJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "invalid_request"})
		return
	}
	var req struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthJSON(w, http.StatusBadRequest, map[string]string{
			"error":             "invalid_redirect_uri",
			"error_description": "redirect_uris is required",
		})
		return
	}
	for _, u := range req.RedirectURIs {
		p, err := url.Parse(u)
		if err != nil || p.Fragment != "" || !(p.Scheme == "https" || (p.Scheme == "http" && isLoopback(p.Host))) {
			writeOAuthJSON(w, http.StatusBadRequest, map[string]string{
				"error":             "invalid_redirect_uri",
				"error_description": "redirect URIs must be https, or http on loopback",
			})
			return
		}
	}
	id, err := randToken()
	if err != nil {
		writeOAuthJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	// "none" means a public client: it proves itself with PKCE and gets no
	// secret. Anything else gets a generated secret we store only as a hash.
	var secret, secretHash string
	if req.TokenEndpointAuthMethod != "none" {
		if secret, err = randToken(); err != nil {
			writeOAuthJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		secretHash = hashToken(secret)
	}
	uris, _ := json.Marshal(req.RedirectURIs)
	if _, err := db.Exec(
		`INSERT INTO oauth_clients (client_id, secret_hash, redirect_uris, name, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, secretHash, string(uris), req.ClientName, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		writeOAuthJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	resp := map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        time.Now().Unix(),
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	if secret != "" {
		resp["client_secret"] = secret
		// 0 = never expires (RFC 7591 §3.2.1).
		resp["client_secret_expires_at"] = 0
		resp["token_endpoint_auth_method"] = "client_secret_post"
	}
	if req.ClientName != "" {
		resp["client_name"] = req.ClientName
	}
	writeOAuthJSON(w, http.StatusCreated, resp)
}

// ---------------------------------------------------------------------------
// authorization endpoint
// ---------------------------------------------------------------------------

// serveOAuthAuthorize renders the consent screen (GET) and issues an
// authorization code (POST). It is gated by UI_AUTH like the rest of the app —
// see the route table in main.go — so reaching it already required credentials.
//
// Per OAuth 2.1, errors that can't be safely redirected (unknown client, bad
// redirect_uri) are shown to the user; everything else goes back to the client
// as ?error= on the redirect URI.
func serveOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if !oauthCfg.Enabled {
		http.Error(w, "MCP OAuth is not enabled (set MCP_OAUTH=client_id:client_secret)", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var (
		clientID  = r.Form.Get("client_id")
		redirect  = r.Form.Get("redirect_uri")
		state     = r.Form.Get("state")
		challenge = r.Form.Get("code_challenge")
		method    = r.Form.Get("code_challenge_method")
		resource  = r.Form.Get("resource")
	)
	client, found := lookupOAuthClient(clientID)
	if !found {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}
	if !redirectURIAllowed(client, redirect) {
		http.Error(w, "redirect_uri is not registered for this client", http.StatusBadRequest)
		return
	}
	// From here on the client identity and callback are trusted, so protocol
	// errors can be reported by redirect the way clients expect.
	fail := func(code, desc string) {
		u, _ := url.Parse(redirect)
		q := u.Query()
		q.Set("error", code)
		q.Set("error_description", desc)
		if state != "" {
			q.Set("state", state)
		}
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	}
	if r.Form.Get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	// OAuth 2.1 makes PKCE mandatory, and S256 is the only method MCP allows.
	if challenge == "" || (method != "" && method != "S256") {
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}

	if r.Method != http.MethodPost {
		renderConsent(w, client, r.Form)
		return
	}
	if r.Form.Get("approve") != "yes" {
		fail("access_denied", "the request was denied")
		return
	}
	code, err := randToken()
	if err != nil {
		fail("server_error", "could not issue a code")
		return
	}
	if _, err := db.Exec(
		`INSERT INTO oauth_codes (code_hash, client_id, redirect_uri, code_challenge, scope, resource, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		hashToken(code), client.ID, redirect, challenge, oauthScope, resource,
		time.Now().UTC().Add(oauthCodeTTL).Format(time.RFC3339),
	); err != nil {
		fail("server_error", "could not persist the code")
		return
	}
	u, _ := url.Parse(redirect)
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// renderConsent shows what is being granted and, crucially, the exact callback
// the code would be sent to — the mitigation that lets the pre-registered
// client accept an unregistered https callback (see redirectURIAllowed).
func renderConsent(w http.ResponseWriter, c oauthClient, form url.Values) {
	name := c.Name
	if n := strings.TrimSpace(name); n == "" {
		name = c.ID
	}
	hidden := &strings.Builder{}
	for _, k := range []string{"client_id", "redirect_uri", "state", "code_challenge", "code_challenge_method", "response_type", "resource", "scope"} {
		if v := form.Get(k); v != "" {
			fmt.Fprintf(hidden, "<input type=\"hidden\" name=%q value=%q>\n", k, html.EscapeString(v))
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, consentPage, html.EscapeString(name), html.EscapeString(form.Get("redirect_uri")), hidden.String())
}

// consentPage is intentionally self-contained (no assets, no JS): it has to
// render before the SPA is reachable and while the browser is mid-OAuth.
const consentPage = `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize MCP access — lasso</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 15px/1.5 ui-sans-serif, system-ui, sans-serif; margin: 0;
         display: grid; place-items: center; min-height: 100vh; padding: 24px; }
  .card { max-width: 30rem; width: 100%%; border: 1px solid color-mix(in srgb, currentColor 20%%, transparent);
          border-radius: 12px; padding: 24px; }
  h1 { font-size: 1.15rem; margin: 0 0 12px; }
  dl { margin: 16px 0; display: grid; grid-template-columns: auto 1fr; gap: 6px 12px; font-size: 13px; }
  dt { opacity: .6; }
  dd { margin: 0; word-break: break-all; font-family: ui-monospace, monospace; }
  .row { display: flex; gap: 8px; margin-top: 20px; }
  button { flex: 1; padding: 10px 14px; border-radius: 8px; font: inherit; cursor: pointer;
           border: 1px solid color-mix(in srgb, currentColor 25%%, transparent); background: transparent; color: inherit; }
  button.primary { background: currentColor; }
  button.primary span { filter: invert(1); }
  p.warn { font-size: 13px; opacity: .7; margin: 12px 0 0; }
</style>
<div class="card">
  <h1>Authorize MCP access</h1>
  <p><strong>%s</strong> is asking to drive lasso's MCP server — it will be able to
     spawn, message, and close agents on every host this lasso can reach.</p>
  <dl>
    <dt>Redirect to</dt><dd>%s</dd>
    <dt>Scope</dt><dd>mcp</dd>
  </dl>
  <p class="warn">Only approve this if you recognise the redirect target above.</p>
  <form method="POST">
    %s
    <div class="row">
      <button type="submit" name="approve" value="no">Deny</button>
      <button class="primary" type="submit" name="approve" value="yes"><span>Approve</span></button>
    </div>
  </form>
</div>
`

// ---------------------------------------------------------------------------
// token endpoint
// ---------------------------------------------------------------------------

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeOAuthJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// serveOAuthToken implements the three grants. Client authentication happens
// per-grant because a public (PKCE-only) client legitimately sends no secret.
func serveOAuthToken(w http.ResponseWriter, r *http.Request) {
	if oauthCORS(w, r) {
		return
	}
	if !oauthCfg.Enabled {
		oauthError(w, http.StatusNotFound, "invalid_request", "MCP OAuth is not enabled")
		return
	}
	if r.Method != http.MethodPost {
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	purgeExpiredOAuth()

	switch r.PostFormValue("grant_type") {
	case "client_credentials":
		tokenClientCredentials(w, r)
	case "authorization_code":
		tokenAuthorizationCode(w, r)
	case "refresh_token":
		tokenRefresh(w, r)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"supported: client_credentials, authorization_code, refresh_token")
	}
}

// tokenClientCredentials is the machine-to-machine path: the pre-registered
// MCP_OAUTH client trades its secret for an access token, no human involved.
// Dynamically-registered clients are refused — they were never vouched for by
// anyone, and this grant has no consent step to vouch for them.
func tokenClientCredentials(w http.ResponseWriter, r *http.Request) {
	client, ok := authenticateClient(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="lasso-mcp"`)
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
	// Machine-to-machine is for credentials an operator minted deliberately: the
	// MCP_OAUTH client, and the per-host clients created by `lasso mcp-client
	// add` (which is the only writer of oauth_clients.host). A client that
	// registered itself through open DCR is still refused — that path takes no
	// human approval, so letting it mint machine tokens would be an open door.
	if !client.Static && client.Host == "" {
		oauthError(w, http.StatusBadRequest, "unauthorized_client",
			"client_credentials is only available to the MCP_OAUTH client and to per-host clients created with `lasso mcp-client add`")
		return
	}
	access, err := issueToken("access", client.ID, oauthScope, oauthAccessTTL)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	// No refresh token: this client can always mint another with its secret,
	// and RFC 6749 §4.4.3 says not to issue one here.
	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   int(oauthAccessTTL.Seconds()),
		"scope":        oauthScope,
	})
}

// tokenAuthorizationCode redeems a consent-screen code, verifying PKCE and that
// the code is being redeemed by the same client and callback it was issued for.
func tokenAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	code := r.PostFormValue("code")
	verifier := r.PostFormValue("code_verifier")
	if code == "" || verifier == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "code and code_verifier are required")
		return
	}
	var (
		clientID, redirect, challenge, scope, expires string
	)
	err := db.QueryRow(
		`SELECT client_id, redirect_uri, code_challenge, scope, expires_at FROM oauth_codes WHERE code_hash = ?`,
		hashToken(code),
	).Scan(&clientID, &redirect, &challenge, &scope, &expires)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown or already-redeemed code")
		return
	}
	// Single use: delete before doing anything else, so a replay finds nothing
	// even if verification below fails.
	_, _ = db.Exec(`DELETE FROM oauth_codes WHERE code_hash = ?`, hashToken(code))

	if exp, perr := time.Parse(time.RFC3339, expires); perr != nil || time.Now().After(exp) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code has expired")
		return
	}
	client, ok := authenticateClient(r)
	if !ok || client.ID != clientID {
		w.Header().Set("WWW-Authenticate", `Basic realm="lasso-mcp"`)
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
	if ru := r.PostFormValue("redirect_uri"); ru != "" && ru != redirect {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) != 1 {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	writeTokenPair(w, client.ID, scope)
}

// tokenRefresh rotates a refresh token: the presented one is consumed and a new
// pair is issued, so a stolen refresh token is detectable by the legitimate
// client suddenly failing.
func tokenRefresh(w http.ResponseWriter, r *http.Request) {
	tok := r.PostFormValue("refresh_token")
	if tok == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	var clientID, scope, expires string
	err := db.QueryRow(
		`SELECT client_id, scope, expires_at FROM oauth_tokens WHERE token_hash = ? AND kind = 'refresh'`,
		hashToken(tok),
	).Scan(&clientID, &scope, &expires)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown refresh token")
		return
	}
	_, _ = db.Exec(`DELETE FROM oauth_tokens WHERE token_hash = ?`, hashToken(tok))
	if exp, perr := time.Parse(time.RFC3339, expires); perr != nil || time.Now().After(exp) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token has expired")
		return
	}
	client, ok := authenticateClient(r)
	if !ok || client.ID != clientID {
		w.Header().Set("WWW-Authenticate", `Basic realm="lasso-mcp"`)
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
	writeTokenPair(w, client.ID, scope)
}

// writeTokenPair issues the access+refresh pair shared by the code and refresh
// grants.
func writeTokenPair(w http.ResponseWriter, clientID, scope string) {
	if scope == "" {
		scope = oauthScope
	}
	access, err := issueToken("access", clientID, scope, oauthAccessTTL)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	refresh, err := issueToken("refresh", clientID, scope, oauthRefreshTTL)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(oauthAccessTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         scope,
	})
}

// ---------------------------------------------------------------------------
// resource protection
// ---------------------------------------------------------------------------

// verifyAccessToken looks up a presented bearer token. Returns the client it was
// issued to and when the token expires, or false if it is unknown or expired.
//
// A ZERO expiresAt with ok=true means the token never expires (issueToken stores
// that as an empty expires_at). A value that is present but unparseable is
// treated as invalid, not as immortal — a corrupt row must fail closed.
func verifyAccessToken(tok string) (clientID string, expiresAt time.Time, ok bool) {
	if tok == "" {
		return "", time.Time{}, false
	}
	var expires string
	err := db.QueryRow(
		`SELECT client_id, expires_at FROM oauth_tokens WHERE token_hash = ? AND kind = 'access'`,
		hashToken(tok),
	).Scan(&clientID, &expires)
	if err != nil {
		return "", time.Time{}, false
	}
	if expires == "" {
		return clientID, time.Time{}, true
	}
	exp, perr := time.Parse(time.RFC3339, expires)
	if perr != nil || time.Now().After(exp) {
		return "", time.Time{}, false
	}
	return clientID, exp, true
}

// mcpTokenVerifier is the auth.TokenVerifier behind /mcp: it turns a presented
// bearer token into the SDK's TokenInfo, carrying the caller's identity in
// fields the caller cannot influence.
//
//   - UserID is the client id. The SDK pins a session to it (streamable.go
//     captures it at initialize and 403s any later request on that session
//     whose token resolves to a different one), which is the spec's
//     session-hijacking mitigation — "derived from the user token and not
//     provided by the client" — obtained for free.
//   - Extra carries the host and scope the credential was provisioned with,
//     which callerFrom reads back on every tool call.
//
// Returning auth.ErrInvalidToken is what makes RequireBearerToken answer 401
// with the RFC 9728 challenge that starts discovery.
func mcpTokenVerifier(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	clientID, exp, ok := verifyAccessToken(token)
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	// A never-expiring token has a zero exp, but auth.RequireBearerToken rejects
	// any TokenInfo whose Expiration is zero ("token missing expiration") — so
	// present it as expiring far enough out to pass that check. lasso's db stays
	// the authority on validity; this value only satisfies the SDK's guard, and is
	// deliberately synthesized here at the boundary rather than stored, so nothing
	// inside lasso mistakes it for a real expiry.
	if exp.IsZero() {
		exp = time.Now().Add(oauthAccessTTL)
	}
	ti := &auth.TokenInfo{
		Scopes:     []string{oauthScope},
		Expiration: exp,
		UserID:     clientID,
	}
	// A token whose client has since been deleted stays valid until it expires
	// (it always did), but it identifies no host — so it falls back to the
	// unscoped view rather than being refused outright.
	if c, found := lookupOAuthClient(clientID); found {
		ti.Extra = map[string]any{tokenHostKey: c.Host, tokenScopeKey: c.Scope}
	}
	return ti, nil
}

// withMCPAuth gates the /mcp handler.
//
// With MCP_OAUTH unset this is a no-op, preserving the documented "unauthenticated
// /mcp" behavior that loopback, tailnet, and Cloudflare-Access deployments rely on.
//
// With it set, a request passes if it carries EITHER a valid bearer token OR the
// UI_AUTH basic credentials — both at once, deliberately: OAuth clients get the
// standards path, while the lasso CLI, curl, and anything already holding UI_AUTH
// keep working unchanged. A failure returns the RFC 9728 challenge that starts
// discovery.
func withMCPAuth(next http.Handler, uiUser, uiPass string, uiEnabled bool) http.Handler {
	if !oauthCfg.Enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No CORS handling here, deliberately: the MCP SDK's handler sets no
		// Access-Control-* headers on the data path, so answering the preflight
		// would promise a browser something the POST then fails to honor. MCP
		// clients that reach /mcp (Claude Desktop, claude.ai connectors, agent
		// sessions) are server-side and don't preflight. The OAuth discovery and
		// token endpoints DO send CORS — those are public documents.
		fields := strings.Fields(r.Header.Get("Authorization"))
		if len(fields) == 2 && strings.EqualFold(fields[0], "bearer") {
			// Delegate the bearer path to the SDK's own middleware rather than
			// verifying inline: it is what puts the TokenInfo into the request
			// context, which is the ONLY way the resolved identity reaches a tool
			// handler (the streamable transport copies it onto every request's
			// Extra) and the only way its session pinning engages. The options are
			// built per-request because the resource metadata URL depends on the
			// origin the client actually reached. Scopes are deliberately not
			// enforced here — they never were, and a token minted before this
			// existed would 403 rather than 401 into a re-auth.
			auth.RequireBearerToken(mcpTokenVerifier, &auth.RequireBearerTokenOptions{
				ResourceMetadataURL: externalBaseURL(r) + "/.well-known/oauth-protected-resource",
			})(next).ServeHTTP(w, r)
			return
		} else if uiEnabled {
			// Basic auth on /mcp is the escape hatch for callers that already
			// hold UI_AUTH — they shouldn't have to run an OAuth dance to reach
			// an endpoint they can already reach everywhere else.
			u, p, ok := r.BasicAuth()
			if ok && subtle.ConstantTimeCompare([]byte(u), []byte(uiUser)) == 1 &&
				subtle.ConstantTimeCompare([]byte(p), []byte(uiPass)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(
			`Bearer realm="lasso-mcp", resource_metadata=%q, scope=%q`,
			externalBaseURL(r)+"/.well-known/oauth-protected-resource", oauthScope))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// logOAuthStatus reports the configuration at startup, next to the other
// one-line startup logs.
func logOAuthStatus() {
	if !oauthCfg.Enabled {
		log.Printf("mcp:      /mcp unauthenticated (set MCP_OAUTH=client_id:client_secret to require OAuth)")
		// A provisioned per-host credential is INERT while /mcp is open: every
		// caller is unidentified and keeps the fleet-wide view, so an operator who
		// set up containment and left MCP_OAUTH unset has none. Say so loudly —
		// silently ignoring the scoping they configured is the worst outcome here.
		if hosts := hostScopedClientCount(); hosts > 0 {
			log.Printf("mcp:      WARNING: %d per-host MCP credential(s) provisioned but NOT enforced — /mcp is open, so every caller is unidentified and fleet-scoped. Set MCP_OAUTH to enforce them.", hosts)
		}
		return
	}
	if hosts := hostScopedClientCount(); hosts > 0 {
		log.Printf("mcp:      %d per-host MCP credential(s) — those callers are scoped by the host they authenticate as (lasso mcp-client list)", hosts)
	}
	scope := "any https/loopback redirect (shown on the consent screen)"
	if len(oauthCfg.RedirectURIs) > 0 {
		scope = strings.Join(oauthCfg.RedirectURIs, ", ")
	}
	log.Printf("mcp:      OAuth on — client_id=%s, grants=client_credentials+authorization_code, redirects: %s",
		oauthCfg.ClientID, scope)
}
