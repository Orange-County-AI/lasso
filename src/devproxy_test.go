package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParsePortRange(t *testing.T) {
	pr, err := parsePortRange("1024-65535")
	if err != nil {
		t.Fatalf("parsePortRange: %v", err)
	}
	if pr.min != 1024 || pr.max != 65535 {
		t.Errorf("got %+v", pr)
	}
	for _, bad := range []string{"", "abc", "5000", "5000-", "-5000", "0-10", "10-99999", "9000-1000"} {
		if _, err := parsePortRange(bad); err == nil {
			t.Errorf("parsePortRange(%q) = nil err; want error", bad)
		}
	}
}

func TestPortFromHost(t *testing.T) {
	pr := portRange{min: 1024, max: 65535}
	const domain = "dev.example.com"

	ok := []struct {
		host string
		want int
	}{
		{"5173.dev.example.com", 5173},
		{"8787.dev.example.com", 8787},
		{"5173.dev.example.com:443", 5173}, // Host may carry a port
		{"5173.DEV.Example.Com", 5173},     // case-insensitive
		{"5173.dev.example.com.", 5173},    // trailing dot
	}
	for _, c := range ok {
		got, err := portFromHost(c.host, domain, pr)
		if err != nil || got != c.want {
			t.Errorf("portFromHost(%q) = %d,%v; want %d,nil", c.host, got, err, c.want)
		}
	}

	bad := []string{
		"dev.example.com",        // no port label
		"5173.other.com",         // wrong domain
		"app.dev.example.com",    // non-numeric label
		"a.5173.dev.example.com", // multi-component label
		"80.dev.example.com",     // below range
		"70000.dev.example.com",  // not a valid label number-in-range
	}
	for _, h := range bad {
		if _, err := portFromHost(h, domain, pr); err == nil {
			t.Errorf("portFromHost(%q) = nil err; want error", h)
		}
	}
}

// TestDevProxyHandlerRoutes verifies the handler proxies <port>.<domain> to the
// matching loopback port, and rejects hosts that don't resolve to a port.
func TestDevProxyHandlerRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Upstream", "hit")
		_, _ = w.Write([]byte("dev-server-body"))
	}))
	defer upstream.Close()

	// Extract the upstream's loopback port and drive the handler with a Host of
	// "<port>.dev.test".
	_, port, _ := splitHostPortStr(upstream.URL)
	pr := portRange{min: 1, max: 65535}
	h := devProxyHandler("dev.test", "127.0.0.1", pr)

	req := httptest.NewRequest("GET", "http://"+port+".dev.test/", nil)
	req.Host = port + ".dev.test"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "dev-server-body" {
		t.Fatalf("proxy did not reach upstream: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// A host that doesn't match the domain -> 404, no proxying.
	req2 := httptest.NewRequest("GET", "http://nope.example.com/", nil)
	req2.Host = "nope.example.com"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("bad host: code=%d; want 404", rec2.Code)
	}
}

// splitHostPortStr pulls host and port out of a "http://host:port" URL string
// without importing net/url at the call site.
func splitHostPortStr(rawURL string) (scheme, port string, ok bool) {
	const p = "http://127.0.0.1:"
	if len(rawURL) > len(p) && rawURL[:len(p)] == p {
		return "http", rawURL[len(p):], true
	}
	return "", "", false
}
