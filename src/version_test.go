package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubPinger swaps luvusPinger for the duration of a test.
func stubPinger(t *testing.T, fn func() (string, int, error)) {
	t.Helper()
	prev := luvusPinger
	luvusPinger = fn
	t.Cleanup(func() { luvusPinger = prev })
}

// callVersion drives serveVersion and decodes the payload.
func callVersion(t *testing.T) versionInfo {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	rec := httptest.NewRecorder()
	serveVersion(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var vi versionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &vi); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return vi
}

// TestVersionCompatibleExactMatch: a luvus speaking exactly the protocol this
// build targets is compatible. This also pins lassoLuvusProtocol to the current
// target — if the constant drifts off the luvus release we ship against, the
// matching arm here changes and the test fails.
func TestVersionCompatibleExactMatch(t *testing.T) {
	stubPinger(t, func() (string, int, error) {
		return "0.8.2", lassoLuvusProtocol, nil
	})
	vi := callVersion(t)
	if !vi.Compatible {
		t.Errorf("exact protocol match must be compatible, got %+v", vi)
	}
	if vi.LassoProtocol != lassoLuvusProtocol || vi.LuvusProtocol != lassoLuvusProtocol {
		t.Errorf("protocols = lasso %d / luvus %d", vi.LassoProtocol, vi.LuvusProtocol)
	}
	if vi.LuvusVersion != "0.8.2" {
		t.Errorf("luvus_version = %q", vi.LuvusVersion)
	}
	if vi.Err != "" {
		t.Errorf("unexpected err %q", vi.Err)
	}
}

// TestVersionIncompatibleMismatch: we target one protocol exactly with no
// backwards compatibility, so a luvus one behind OR one ahead is incompatible.
func TestVersionIncompatibleMismatch(t *testing.T) {
	for _, p := range []int{lassoLuvusProtocol - 1, lassoLuvusProtocol + 1} {
		t.Run(fmt.Sprintf("luvus_protocol_%d", p), func(t *testing.T) {
			stubPinger(t, func() (string, int, error) { return "0.0.0", p, nil })
			vi := callVersion(t)
			if vi.Compatible {
				t.Errorf("protocol %d vs target %d must be incompatible", p, lassoLuvusProtocol)
			}
			if vi.LuvusProtocol != p {
				t.Errorf("luvus_protocol = %d, want %d", vi.LuvusProtocol, p)
			}
		})
	}
}

// TestVersionPingError: when the daemon can't be reached the tab reports the
// error (not a false mismatch) and leaves the luvus protocol unset.
func TestVersionPingError(t *testing.T) {
	stubPinger(t, func() (string, int, error) {
		return "", 0, fmt.Errorf("dial unix: connection refused")
	})
	vi := callVersion(t)
	if vi.Err == "" {
		t.Error("expected err to be surfaced")
	}
	if vi.Compatible {
		t.Error("ping failure must not read as compatible")
	}
	if vi.LuvusProtocol != 0 {
		t.Errorf("luvus_protocol = %d, want 0 on error", vi.LuvusProtocol)
	}
	if vi.LassoProtocol != lassoLuvusProtocol {
		t.Errorf("lasso_protocol should still report the target, got %d", vi.LassoProtocol)
	}
}
