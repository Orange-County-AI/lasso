package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postAgentClose must POST the calling agent's own herdr pane id to
// /api/agent/close, carrying basic auth when UI_AUTH is set — the same soft-close
// the UI and close_agent MCP tool use.
func TestPostAgentClose(t *testing.T) {
	var gotPath, gotPaneID, gotAuthUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if u, _, ok := r.BasicAuth(); ok {
			gotAuthUser = u
		}
		var body struct {
			PaneID string `json:"pane_id"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		gotPaneID = body.PaneID
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := postAgentClose(addr, "p_82", "alice", "secret", true); err != nil {
		t.Fatalf("postAgentClose: %v", err)
	}
	if gotPath != "/api/agent/close" {
		t.Errorf("path = %q, want /api/agent/close", gotPath)
	}
	if gotPaneID != "p_82" {
		t.Errorf("pane_id = %q, want p_82", gotPaneID)
	}
	if gotAuthUser != "alice" {
		t.Errorf("basic-auth user = %q, want alice", gotAuthUser)
	}
}

// serveAgentClose needs something to identify the agent by: with neither a
// pane_id nor an agent_id it must reject the request rather than guess.
func TestServeAgentCloseRequiresIdentifier(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/agent/close", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	serveAgentClose(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when no pane_id/agent_id given", rec.Code)
	}
}

// Only POST is accepted.
func TestServeAgentCloseRejectsGET(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/agent/close", nil)
	rec := httptest.NewRecorder()
	serveAgentClose(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 for GET", rec.Code)
	}
}

// A non-200 from the server is surfaced as an error, not swallowed.
func TestPostAgentCloseServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := postAgentClose(addr, "p_82", "", "", false); err == nil {
		t.Fatal("expected an error for a 500 response")
	}
}
