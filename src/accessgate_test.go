package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// gatedHandler wraps a handler that records whether it was reached, so a test
// can tell "403 from the gate" apart from "the route answered 403 itself".
func gatedHandler(g accessGate, reached *bool) http.Handler {
	return g.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	}))
}

func doGate(t *testing.T, g accessGate, path, email string, hdr ...string) (code int, reached bool) {
	t.Helper()
	r := httptest.NewRequest("GET", "https://lasso.example"+path, nil)
	if email != "" {
		r.Header.Set(accessEmailHeader, email)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	gatedHandler(g, &reached).ServeHTTP(w, r)
	return w.Result().StatusCode, reached
}

func TestAccessGateOffIsInert(t *testing.T) {
	g := newAccessGate(false, "")
	// No header, gate off: the request goes through untouched — the header is
	// never trusted (or required) unless the flag is set.
	if code, reached := doGate(t, g, "/api/active", ""); code != 200 || !reached {
		t.Fatalf("gate off: code=%d reached=%v, want 200/true", code, reached)
	}
}

func TestAccessGateRequiresHeader(t *testing.T) {
	g := newAccessGate(true, "")

	for _, path := range []string{
		"/", "/api/active", "/api/file", "/api/file-write", "/api/file-upload",
		"/mcp", "/mcp/messages", "/terminal/", "/shell/",
		"/.well-known/oauth-authorization-server", "/oauth/token",
	} {
		code, reached := doGate(t, g, path, "")
		if code != http.StatusForbidden || reached {
			t.Errorf("%s without header: code=%d reached=%v, want 403/false", path, code, reached)
		}
		code, reached = doGate(t, g, path, "dev@example.com")
		if code != 200 || !reached {
			t.Errorf("%s with header: code=%d reached=%v, want 200/true", path, code, reached)
		}
	}

	// An empty / whitespace-only header is not an identity.
	for _, v := range []string{" ", "   "} {
		if code, reached := doGate(t, g, "/api/active", v); code != http.StatusForbidden || reached {
			t.Errorf("blank header %q: code=%d reached=%v, want 403/false", v, code, reached)
		}
	}
}

// A websocket upgrade is an ordinary HTTP request until a handler hijacks it,
// so the gate must answer first for /terminal/ and /shell/ upgrades too.
func TestAccessGateCoversWebsocketUpgrade(t *testing.T) {
	g := newAccessGate(true, "")
	ws := []string{"Connection", "Upgrade", "Upgrade", "websocket", "Sec-WebSocket-Version", "13"}

	for _, path := range []string{"/terminal/ws", "/shell/ws"} {
		if code, reached := doGate(t, g, path, "", ws...); code != http.StatusForbidden || reached {
			t.Errorf("%s upgrade without header: code=%d reached=%v, want 403/false", path, code, reached)
		}
		if code, reached := doGate(t, g, path, "dev@example.com", ws...); code != 200 || !reached {
			t.Errorf("%s upgrade with header: code=%d reached=%v, want 200/true", path, code, reached)
		}
	}
}

func TestAccessGateAllowlist(t *testing.T) {
	g := newAccessGate(true, " Dev@Example.com , ops@example.com ,, ")

	if code, _ := doGate(t, g, "/api/active", "dev@example.com"); code != 200 {
		t.Errorf("allowlisted email: code=%d, want 200", code)
	}
	// Case-insensitive on both sides, and surrounding space is trimmed.
	if code, _ := doGate(t, g, "/api/active", " OPS@Example.COM "); code != 200 {
		t.Errorf("allowlisted email (mixed case): code=%d, want 200", code)
	}
	if code, reached := doGate(t, g, "/mcp", "stranger@example.com"); code != http.StatusForbidden || reached {
		t.Errorf("non-allowlisted email on /mcp: code=%d reached=%v, want 403/false", code, reached)
	}
	if code, _ := doGate(t, g, "/api/active", ""); code != http.StatusForbidden {
		t.Errorf("no header with allowlist: code=%d, want 403", code)
	}
}

func TestNewAccessGateParsesList(t *testing.T) {
	if g := newAccessGate(true, ""); len(g.allowed) != 0 {
		t.Errorf("empty list should leave allowed empty, got %v", g.allowed)
	}
	if g := newAccessGate(true, " , , "); len(g.allowed) != 0 {
		t.Errorf("blank-only list should leave allowed empty, got %v", g.allowed)
	}
	g := newAccessGate(true, "a@x.com,B@X.com")
	if len(g.allowed) != 2 || !g.allowed["a@x.com"] || !g.allowed["b@x.com"] {
		t.Errorf("allowed = %v, want the two lowercased emails", g.allowed)
	}
}

func TestEnvOn(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " 1 "} {
		t.Setenv("LASSO_TEST_ENV_ON", v)
		if !envOn("LASSO_TEST_ENV_ON") {
			t.Errorf("envOn(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "maybe"} {
		t.Setenv("LASSO_TEST_ENV_ON", v)
		if envOn("LASSO_TEST_ENV_ON") {
			t.Errorf("envOn(%q) = true, want false", v)
		}
	}
}
