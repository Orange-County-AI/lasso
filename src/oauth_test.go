package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// enableOAuth points MCP_OAUTH at a known client for the duration of a test and
// restores the global config afterwards (it's process-wide, set once at startup
// in production).
func enableOAuth(t *testing.T, redirects string) {
	t.Helper()
	t.Setenv("MCP_OAUTH", "cid:sekret")
	t.Setenv("MCP_OAUTH_REDIRECT_URIS", redirects)
	prev := oauthCfg
	oauthCfg = loadOAuthConfig()
	t.Cleanup(func() { oauthCfg = prev })
}

func postForm(t *testing.T, h http.HandlerFunc, form url.Values, basic [2]string) (*http.Response, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", "https://lasso.example/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic[0] != "" {
		r.SetBasicAuth(basic[0], basic[1])
	}
	w := httptest.NewRecorder()
	h(w, r)
	resp := w.Result()
	var body map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &body)
	return resp, body
}

func TestClientCredentialsGrant(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")

	resp, body := postForm(t, serveOAuthToken, url.Values{
		"grant_type": {"client_credentials"},
	}, [2]string{"cid", "sekret"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", resp.StatusCode, body)
	}
	tok, _ := body["access_token"].(string)
	if tok == "" {
		t.Fatalf("no access_token in %v", body)
	}
	if body["token_type"] != "Bearer" {
		t.Errorf("token_type = %v, want Bearer", body["token_type"])
	}
	// RFC 6749 §4.4.3: no refresh token for this grant.
	if _, ok := body["refresh_token"]; ok {
		t.Errorf("client_credentials issued a refresh_token: %v", body)
	}
	if _, ok := verifyAccessToken(tok); !ok {
		t.Error("issued token does not verify")
	}
	if _, ok := verifyAccessToken(tok + "x"); ok {
		t.Error("a tampered token verified")
	}
}

func TestClientCredentialsAcceptsPostCredentials(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")

	resp, body := postForm(t, serveOAuthToken, url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {"cid"},
		"client_secret": {"sekret"},
	}, [2]string{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client_secret_post status = %d, want 200 (body %v)", resp.StatusCode, body)
	}
}

func TestClientCredentialsRejectsBadSecret(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")

	resp, body := postForm(t, serveOAuthToken, url.Values{
		"grant_type": {"client_credentials"},
	}, [2]string{"cid", "wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if body["error"] != "invalid_client" {
		t.Errorf("error = %v, want invalid_client", body["error"])
	}
}

// A dynamically-registered client has nobody vouching for it, and
// client_credentials has no consent step — so it must be refused that grant.
func TestClientCredentialsDeniedToDynamicClient(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	id, secret := registerTestClient(t, "https://claude.ai/api/mcp/auth_callback")
	if secret == "" {
		t.Fatal("expected a generated client_secret")
	}

	resp, body := postForm(t, serveOAuthToken, url.Values{
		"grant_type": {"client_credentials"},
	}, [2]string{id, secret})
	if resp.StatusCode != http.StatusBadRequest || body["error"] != "unauthorized_client" {
		t.Fatalf("status=%d error=%v, want 400/unauthorized_client", resp.StatusCode, body["error"])
	}
}

// registerTestClient drives the DCR endpoint and returns the issued credentials.
func registerTestClient(t *testing.T, redirect string) (id, secret string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"redirect_uris": []string{redirect},
		"client_name":   "Test Connector",
	})
	r := httptest.NewRequest("POST", "https://lasso.example/oauth/register", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	serveOAuthRegister(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("register status = %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	id, _ = out["client_id"].(string)
	secret, _ = out["client_secret"].(string)
	if id == "" {
		t.Fatalf("no client_id in %s", w.Body.String())
	}
	return id, secret
}

func TestAuthorizationCodeFlow(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	const redirect = "https://claude.ai/api/mcp/auth_callback"
	id, secret := registerTestClient(t, redirect)

	verifier := "a-high-entropy-code-verifier-value-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	form := url.Values{
		"response_type":         {"code"},
		"client_id":             {id},
		"redirect_uri":          {redirect},
		"state":                 {"xyz"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}

	// GET renders the consent screen and issues nothing.
	r := httptest.NewRequest("GET", "https://lasso.example/oauth/authorize?"+form.Encode(), nil)
	w := httptest.NewRecorder()
	serveOAuthAuthorize(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("consent GET = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), redirect) {
		t.Error("consent screen does not show the redirect target")
	}

	// POST with approval redirects back with a code.
	form.Set("approve", "yes")
	r = httptest.NewRequest("POST", "https://lasso.example/oauth/authorize", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	serveOAuthAuthorize(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("approve = %d, want 302 (%s)", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in %s", loc)
	}
	if got := loc.Query().Get("state"); got != "xyz" {
		t.Errorf("state = %q, want xyz", got)
	}

	// Redeeming with the wrong verifier must fail.
	resp, body := postForm(t, serveOAuthToken, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {"not-the-verifier"},
		"redirect_uri":  {redirect},
	}, [2]string{id, secret})
	if resp.StatusCode == http.StatusOK {
		t.Fatal("PKCE verification passed with the wrong verifier")
	}
	// …and that failed attempt must have burned the code (single use).
	resp, body = postForm(t, serveOAuthToken, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirect},
	}, [2]string{id, secret})
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("a redeemed code was accepted twice: %v", body)
	}

	// A fresh code, correct verifier, succeeds and yields a refresh token.
	form.Set("approve", "yes")
	r = httptest.NewRequest("POST", "https://lasso.example/oauth/authorize", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	serveOAuthAuthorize(w, r)
	loc, _ = url.Parse(w.Header().Get("Location"))
	code = loc.Query().Get("code")

	resp, body = postForm(t, serveOAuthToken, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirect},
	}, [2]string{id, secret})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token status = %d: %v", resp.StatusCode, body)
	}
	access, _ := body["access_token"].(string)
	refresh, _ := body["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("missing tokens in %v", body)
	}
	if _, ok := verifyAccessToken(access); !ok {
		t.Error("access token does not verify")
	}

	// Refresh rotates: the old refresh token must stop working.
	resp, body = postForm(t, serveOAuthToken, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	}, [2]string{id, secret})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d: %v", resp.StatusCode, body)
	}
	if newAccess, _ := body["access_token"].(string); newAccess == access {
		t.Error("refresh returned the same access token")
	}
	resp, _ = postForm(t, serveOAuthToken, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	}, [2]string{id, secret})
	if resp.StatusCode == http.StatusOK {
		t.Error("a rotated-away refresh token was accepted again")
	}
}

func TestAuthorizeRequiresPKCE(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	const redirect = "https://claude.ai/api/mcp/auth_callback"
	id, _ := registerTestClient(t, redirect)

	form := url.Values{
		"response_type": {"code"},
		"client_id":     {id},
		"redirect_uri":  {redirect},
	}
	r := httptest.NewRequest("GET", "https://lasso.example/oauth/authorize?"+form.Encode(), nil)
	w := httptest.NewRecorder()
	serveOAuthAuthorize(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want a 302 error redirect", w.Code)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Query().Get("error") != "invalid_request" {
		t.Errorf("error = %q, want invalid_request", loc.Query().Get("error"))
	}
}

func TestAuthorizeRejectsUnregisteredRedirect(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	id, _ := registerTestClient(t, "https://claude.ai/api/mcp/auth_callback")

	form := url.Values{
		"response_type":  {"code"},
		"client_id":      {id},
		"redirect_uri":   {"https://evil.example/steal"},
		"code_challenge": {"x"},
	}
	r := httptest.NewRequest("GET", "https://lasso.example/oauth/authorize?"+form.Encode(), nil)
	w := httptest.NewRecorder()
	serveOAuthAuthorize(w, r)
	// Not a redirect: an unvalidated redirect_uri must never be redirected to.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestStaticClientRedirectPolicy(t *testing.T) {
	openTestDB(t)

	// No allowlist: any https or loopback callback, nothing else.
	enableOAuth(t, "")
	c, _ := lookupOAuthClient("cid")
	for _, tc := range []struct {
		uri  string
		want bool
	}{
		{"https://claude.ai/api/mcp/auth_callback", true},
		{"http://127.0.0.1:8976/callback", true},
		{"http://evil.example/cb", false},
		{"https://ok.example/cb#frag", false},
		{"", false},
	} {
		if got := redirectURIAllowed(c, tc.uri); got != tc.want {
			t.Errorf("redirectURIAllowed(%q) = %v, want %v", tc.uri, got, tc.want)
		}
	}

	// With an allowlist, only the listed URIs pass.
	enableOAuth(t, "https://claude.ai/api/mcp/auth_callback")
	c, _ = lookupOAuthClient("cid")
	if !redirectURIAllowed(c, "https://claude.ai/api/mcp/auth_callback") {
		t.Error("allowlisted URI rejected")
	}
	if redirectURIAllowed(c, "https://other.example/cb") {
		t.Error("non-allowlisted https URI accepted despite MCP_OAUTH_REDIRECT_URIS")
	}
}

// ---------------------------------------------------------------------------
// resource protection
// ---------------------------------------------------------------------------

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
}

// The historical behavior — /mcp open — must survive untouched when MCP_OAUTH
// is unset, or every existing loopback/tailnet/Access deployment breaks.
func TestMCPStaysOpenWithoutConfig(t *testing.T) {
	openTestDB(t)
	prev := oauthCfg
	oauthCfg = oauthConf{}
	t.Cleanup(func() { oauthCfg = prev })

	h := withMCPAuth(okHandler(), "u", "p", true)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "https://lasso.example/mcp", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestMCPBearerGate(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	h := withMCPAuth(okHandler(), "u", "p", true)

	// No credentials → 401 carrying the RFC 9728 discovery pointer.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "https://lasso.example/mcp", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	challenge := w.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Bearer ") ||
		!strings.Contains(challenge, "/.well-known/oauth-protected-resource") {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge with resource_metadata", challenge)
	}

	// A token from the client_credentials grant gets in.
	_, body := postForm(t, serveOAuthToken, url.Values{"grant_type": {"client_credentials"}}, [2]string{"cid", "sekret"})
	tok, _ := body["access_token"].(string)
	r := httptest.NewRequest("POST", "https://lasso.example/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("bearer status = %d, want 200", w.Code)
	}

	// A bogus token does not.
	r = httptest.NewRequest("POST", "https://lasso.example/mcp", nil)
	r.Header.Set("Authorization", "Bearer nope")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bogus bearer status = %d, want 401", w.Code)
	}
}

// Both auth paths are live at once: OAuth clients use bearer tokens, and
// anything already holding UI_AUTH keeps working without an OAuth dance.
func TestMCPAcceptsUIAuthAlongsideBearer(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	h := withMCPAuth(okHandler(), "u", "p", true)

	r := httptest.NewRequest("POST", "https://lasso.example/mcp", nil)
	r.SetBasicAuth("u", "p")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("UI_AUTH basic status = %d, want 200", w.Code)
	}

	r = httptest.NewRequest("POST", "https://lasso.example/mcp", nil)
	r.SetBasicAuth("u", "wrong")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad basic status = %d, want 401", w.Code)
	}
}

// ---------------------------------------------------------------------------
// discovery
// ---------------------------------------------------------------------------

func TestDiscoveryMetadata(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")

	r := httptest.NewRequest("GET", "http://127.0.0.1:8190/.well-known/oauth-authorization-server", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "lasso.example.com")
	w := httptest.NewRecorder()
	serveAuthServerMetadata(w, r)
	var as map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &as)
	if as["issuer"] != "https://lasso.example.com" {
		t.Errorf("issuer = %v, want the forwarded origin", as["issuer"])
	}
	if as["token_endpoint"] != "https://lasso.example.com/oauth/token" {
		t.Errorf("token_endpoint = %v", as["token_endpoint"])
	}
	grants, _ := as["grant_types_supported"].([]any)
	var haveCC bool
	for _, g := range grants {
		if g == "client_credentials" {
			haveCC = true
		}
	}
	if !haveCC {
		t.Errorf("grant_types_supported = %v, want client_credentials advertised", grants)
	}

	r = httptest.NewRequest("GET", "http://127.0.0.1:8190/.well-known/oauth-protected-resource/mcp", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "lasso.example.com")
	w = httptest.NewRecorder()
	serveProtectedResourceMetadata(w, r)
	var prm map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &prm)
	if prm["resource"] != "https://lasso.example.com/mcp" {
		t.Errorf("resource = %v", prm["resource"])
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("discovery metadata is missing CORS headers")
	}
}

// A tunnelled request arrives as plain HTTP on loopback with a public Host and
// no forwarded-proto header; the metadata must still advertise https, or every
// OAuth client rejects the endpoints.
func TestExternalBaseURLAssumesTLSForPublicHosts(t *testing.T) {
	r := httptest.NewRequest("GET", "http://lasso.example.com/.well-known/oauth-authorization-server", nil)
	r.Host = "lasso.example.com"
	if got := externalBaseURL(r); got != "https://lasso.example.com" {
		t.Errorf("externalBaseURL = %q, want https://lasso.example.com", got)
	}
	r = httptest.NewRequest("GET", "http://127.0.0.1:8190/x", nil)
	r.Host = "127.0.0.1:8190"
	if got := externalBaseURL(r); got != "http://127.0.0.1:8190" {
		t.Errorf("externalBaseURL = %q, want the loopback origin unchanged", got)
	}
}

// With MCP_OAUTH unset, every OAuth route must 404. lasso's Cloudflare Access
// deployment relies on the origin advertising no OAuth at all so that Access is
// the authorization server (CLAUDE.md); half-present metadata would break it.
func TestOAuthRoutesAbsentWithoutConfig(t *testing.T) {
	openTestDB(t)
	prev := oauthCfg
	oauthCfg = oauthConf{}
	t.Cleanup(func() { oauthCfg = prev })

	for name, h := range map[string]http.HandlerFunc{
		"protected-resource":   serveProtectedResourceMetadata,
		"authorization-server": serveAuthServerMetadata,
		"register":             serveOAuthRegister,
		"authorize":            serveOAuthAuthorize,
	} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("GET", "https://lasso.example/x", nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404 when MCP_OAUTH is unset", name, w.Code)
		}
	}

	resp, _ := postForm(t, serveOAuthToken, url.Values{"grant_type": {"client_credentials"}}, [2]string{"cid", "sekret"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("token status = %d, want 404 when MCP_OAUTH is unset", resp.StatusCode)
	}
}
