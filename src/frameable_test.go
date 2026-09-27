package main

import (
	"net/http"
	"testing"
)

func TestFrameableBy(t *testing.T) {
	const origin = "http://100.86.22.100:5173"
	cases := []struct {
		name string
		h    map[string]string
		want bool
	}{
		{"no headers", nil, true},
		{"xfo deny", map[string]string{"X-Frame-Options": "DENY"}, false},
		{"xfo sameorigin", map[string]string{"X-Frame-Options": "sameorigin"}, false},
		{"csp none", map[string]string{"Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'"}, false},
		{"csp self", map[string]string{"Content-Security-Policy": "frame-ancestors 'self'"}, false},
		{"csp star", map[string]string{"Content-Security-Policy": "frame-ancestors *"}, true},
		{"csp host match", map[string]string{"Content-Security-Policy": "frame-ancestors http://100.86.22.100:5173"}, true},
		{"csp wrong port", map[string]string{"Content-Security-Policy": "frame-ancestors 100.86.22.100:8090"}, false},
		{"csp scheme source", map[string]string{"Content-Security-Policy": "frame-ancestors http:"}, true},
		// frame-ancestors supersedes X-Frame-Options.
		{"csp overrides xfo", map[string]string{"X-Frame-Options": "DENY", "Content-Security-Policy": "frame-ancestors *"}, true},
		{"csp without frame-ancestors", map[string]string{"Content-Security-Policy": "default-src 'self'"}, true},
	}
	for _, c := range cases {
		h := http.Header{}
		for k, v := range c.h {
			h.Set(k, v)
		}
		if got, _ := frameableBy(h, origin); got != c.want {
			t.Errorf("%s: frameable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFrameAncestorsWildcardSubdomain(t *testing.T) {
	if !frameAncestorsAllow([]string{"https://*.example.com"}, "https://lasso.example.com") {
		t.Error("*.example.com should allow lasso.example.com")
	}
	if frameAncestorsAllow([]string{"https://*.example.com"}, "https://example.org") {
		t.Error("*.example.com should not allow example.org")
	}
}
