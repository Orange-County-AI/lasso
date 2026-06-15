package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postAgentClose must POST the agent's own tab id to /api/agent/close, carrying
// basic auth when UI_AUTH is set — the same soft-close the UI and close_agent
// MCP tool use.
func TestPostAgentClose(t *testing.T) {
	var gotPath, gotTabID, gotAuthUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if u, _, ok := r.BasicAuth(); ok {
			gotAuthUser = u
		}
		var body struct {
			TabID string `json:"tab_id"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		gotTabID = body.TabID
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := postAgentClose(addr, "dj8hus0qiduh", "alice", "secret", true); err != nil {
		t.Fatalf("postAgentClose: %v", err)
	}
	if gotPath != "/api/agent/close" {
		t.Errorf("path = %q, want /api/agent/close", gotPath)
	}
	if gotTabID != "dj8hus0qiduh" {
		t.Errorf("tab_id = %q, want dj8hus0qiduh", gotTabID)
	}
	if gotAuthUser != "alice" {
		t.Errorf("basic-auth user = %q, want alice", gotAuthUser)
	}
}

// A non-200 from the server is surfaced as an error, not swallowed.
func TestPostAgentCloseServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := postAgentClose(addr, "tab", "", "", false); err == nil {
		t.Fatal("expected an error for a 500 response")
	}
}
