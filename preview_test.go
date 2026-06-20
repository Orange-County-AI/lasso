package main

import "testing"

func TestParseServeStatus(t *testing.T) {
	raw := []byte(`{
		"TCP": { "8443": {"HTTPS": true}, "8444": {"HTTPS": true} },
		"Web": {
			"citadel.tail9dd8e.ts.net:8444": {
				"Handlers": { "/": { "Proxy": "http://100.114.163.121:5173" } }
			}
		}
	}`)
	st, err := parseServeStatus(raw)
	if err != nil {
		t.Fatalf("parseServeStatus: %v", err)
	}
	if !st.httpsPorts[8443] || !st.httpsPorts[8444] {
		t.Errorf("expected 8443 and 8444 in httpsPorts, got %v", st.httpsPorts)
	}
	proxy, ok := st.proxies[8444]
	if !ok || proxy != "http://100.114.163.121:5173" {
		t.Errorf("expected proxy for 8444, got %q (ok=%v)", proxy, ok)
	}
	lp, ok := localPortFromProxy(proxy)
	if !ok || lp != 5173 {
		t.Errorf("localPortFromProxy(%q) = %d,%v; want 5173,true", proxy, lp, ok)
	}
}

func TestParseServeStatusEmpty(t *testing.T) {
	st, err := parseServeStatus(nil)
	if err != nil {
		t.Fatalf("parseServeStatus(nil): %v", err)
	}
	if len(st.httpsPorts) != 0 || len(st.proxies) != 0 {
		t.Errorf("expected empty state, got %+v", st)
	}
}

func TestAllocHTTPSPort(t *testing.T) {
	// 8500 and 8501 in use (plus an out-of-range port) -> first free is 8502.
	st := &serveState{
		httpsPorts: map[int]bool{8500: true, 8501: true, 8443: true},
		proxies:    map[int]string{},
	}
	p, err := allocHTTPSPort(st)
	if err != nil {
		t.Fatalf("allocHTTPSPort: %v", err)
	}
	if p != 8502 {
		t.Errorf("allocHTTPSPort = %d; want 8502", p)
	}
}

func TestAllocHTTPSPortExhausted(t *testing.T) {
	st := &serveState{httpsPorts: map[int]bool{}, proxies: map[int]string{}}
	for p := previewPortMin; p <= previewPortMax; p++ {
		st.httpsPorts[p] = true
	}
	if _, err := allocHTTPSPort(st); err == nil {
		t.Error("expected error when all managed ports are in use")
	}
}
