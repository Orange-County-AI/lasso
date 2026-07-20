package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// closeBackend wraps whoamiBackend with a host name and a record of pane.close
// calls — one fake host's herdr for the close-path tests. pane.list reports no
// panes, so killPaneAgent sees no agent and returns immediately.
type closeBackend struct {
	*whoamiBackend
	host   string
	closed []string
}

func newCloseBackend(host string, resolve map[string]string) *closeBackend {
	return &closeBackend{
		whoamiBackend: &whoamiBackend{memBackend: newMemBackend(), resolve: resolve},
		host:          host,
	}
}

func (b *closeBackend) Name() string { return b.host }

func (b *closeBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	if method == "pane.close" {
		p, _ := params.(map[string]any)
		id, _ := p["pane_id"].(string)
		b.closed = append(b.closed, id)
		return json.RawMessage(`{}`), nil
	}
	return b.whoamiBackend.HerdrCall(method, params)
}

// stubCloseBackends points the close path's backend resolution at fakes; a
// host absent from the map behaves like an unreachable one.
func stubCloseBackends(t *testing.T, backends map[string]Backend) {
	t.Helper()
	prev := closeBackendResolver
	closeBackendResolver = func(host string) (Backend, error) {
		if host == "" {
			host = "local"
		}
		if b, ok := backends[host]; ok {
			return b, nil
		}
		return nil, fmt.Errorf("host %q not available", host)
	}
	t.Cleanup(func() { closeBackendResolver = prev })
}

// stubPeers fakes the peer-lasso fleet the adoption path consults.
func stubPeers(t *testing.T, peers []string, query func(peer, rootPane string) ([]AgentRecord, error)) {
	t.Helper()
	prevH, prevQ := peerHostsFn, peerAgentQueryFn
	peerHostsFn = func(context.Context) []string { return peers }
	if query != nil {
		peerAgentQueryFn = query
	}
	t.Cleanup(func() { peerHostsFn = prevH; peerAgentQueryFn = prevQ })
}

func postClose(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/agent/close", strings.NewReader(body))
	rec := httptest.NewRecorder()
	serveAgentClose(rec, req)
	return rec
}

// A pane id that matches agent records on TWO hosts must not be guessed at:
// without a host the server refuses (409) and closes nothing — closing the
// wrong host's pane would kill an unrelated agent.
func TestServeAgentClosePaneCollisionAcrossHostsFailsLoudly(t *testing.T) {
	openTestDB(t)
	if err := appendAgent("local", AgentRecord{ID: "loc1", Title: "local agent", Type: "git",
		RootPane: "wR:p1", WorkDir: "/w/loc1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := appendAgent("citadel", AgentRecord{ID: "rem1", Title: "remote agent", Type: "git",
		RootPane: "wR:p1", WorkDir: "/w/rem1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	local := newCloseBackend("local", map[string]string{"wR:p1": "wR:p1"})
	citadel := newCloseBackend("citadel", map[string]string{"wR:p1": "wR:p1"})
	stubCloseBackends(t, map[string]Backend{"local": local, "citadel": citadel})
	stubPeers(t, nil, nil)

	rr := postClose(t, `{"pane_id":"wR:p1"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a cross-host pane collision; body: %s", rr.Code, rr.Body.String())
	}
	if n := len(local.closed) + len(citadel.closed); n != 0 {
		t.Errorf("closed %d panes on an ambiguous request, want 0 (local=%v citadel=%v)", n, local.closed, citadel.closed)
	}
}

// The same collision with an explicit host closes exactly the named host's
// agent, through that host's backend — never the local one.
func TestServeAgentCloseExplicitHostTargetsThatHost(t *testing.T) {
	openTestDB(t)
	if err := appendAgent("local", AgentRecord{ID: "loc1", Type: "git", RootPane: "wR:p1",
		WorkDir: "/w/loc1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := appendAgent("citadel", AgentRecord{ID: "rem1", Type: "git", RootPane: "wR:p1",
		WorkDir: "/w/rem1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	local := newCloseBackend("local", map[string]string{"wR:p1": "wR:p1"})
	citadel := newCloseBackend("citadel", map[string]string{"wR:p1": "wR:p1"})
	stubCloseBackends(t, map[string]Backend{"local": local, "citadel": citadel})

	rr := postClose(t, `{"pane_id":"wR:p1","host":"citadel"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if len(citadel.closed) != 1 || citadel.closed[0] != "wR:p1" {
		t.Errorf("citadel closed = %v, want [wR:p1]", citadel.closed)
	}
	if len(local.closed) != 0 {
		t.Errorf("local closed = %v, want none — the close leaked onto the wrong host", local.closed)
	}
}

// A record that lives only on a remote host resolves without a host hint and
// is torn down through that host's backend.
func TestServeAgentCloseRemoteRecordUsesRemoteBackend(t *testing.T) {
	openTestDB(t)
	if err := appendAgent("citadel", AgentRecord{ID: "rem1", Type: "git", RootPane: "wC:p3",
		WorkDir: "/w/rem1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	local := newCloseBackend("local", nil)
	citadel := newCloseBackend("citadel", map[string]string{"wC:p3": "wC:p3"})
	stubCloseBackends(t, map[string]Backend{"local": local, "citadel": citadel})
	stubPeers(t, nil, nil)

	rr := postClose(t, `{"agent_id":"rem1"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if len(citadel.closed) != 1 || citadel.closed[0] != "wC:p3" {
		t.Errorf("citadel closed = %v, want [wC:p3]", citadel.closed)
	}
	if len(local.closed) != 0 {
		t.Errorf("local closed = %v, want none", local.closed)
	}
}

// closeme's case: the pane is local (host "local" is sent), but the record was
// created by a peer lasso. The server adopts the peer's record — verified
// against the local pane and work dir — and closes the LOCAL pane.
func TestServeAgentCloseAdoptsPeerRecord(t *testing.T) {
	openTestDB(t) // no local records at all
	local := newCloseBackend("local", map[string]string{"p_82": "w55-1"})
	_ = local.MkdirAll("/w/peer-agent", 0o755)
	stubCloseBackends(t, map[string]Backend{"local": local})
	stubPeers(t, []string{"citadel"}, func(peer, rootPane string) ([]AgentRecord, error) {
		if peer == "citadel" && rootPane == "w55-1" {
			return []AgentRecord{{ID: "dk33", Type: "git", RootPane: "w55-1",
				WorkspaceID: "w55", WorkDir: "/w/peer-agent"}}, nil
		}
		return nil, nil
	})

	rr := postClose(t, `{"pane_id":"p_82","host":"local"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if len(local.closed) != 1 || local.closed[0] != "w55-1" {
		t.Errorf("local closed = %v, want [w55-1]", local.closed)
	}
}

// A peer row whose work dir does not exist on this machine is a record for
// some OTHER host that happens to share the pane id — it must not be adopted.
func TestServeAgentCloseAdoptionRejectsForeignWorkDir(t *testing.T) {
	openTestDB(t)
	local := newCloseBackend("local", map[string]string{"p_82": "w55-1"})
	stubCloseBackends(t, map[string]Backend{"local": local})
	stubPeers(t, []string{"citadel"}, func(_, _ string) ([]AgentRecord, error) {
		return []AgentRecord{{ID: "dk33", Type: "git", RootPane: "w55-1",
			WorkspaceID: "w55", WorkDir: "/does/not/exist/here"}}, nil
	})

	rr := postClose(t, `{"pane_id":"p_82","host":"local"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rr.Code, rr.Body.String())
	}
	if len(local.closed) != 0 {
		t.Errorf("closed = %v, want none", local.closed)
	}
}

// Two peers both claiming the pane is unresolvable — refuse (409), close nothing.
func TestServeAgentCloseAdoptionAmbiguousPeersFailsLoudly(t *testing.T) {
	openTestDB(t)
	local := newCloseBackend("local", map[string]string{"p_82": "w55-1"})
	_ = local.MkdirAll("/w/peer-agent", 0o755)
	stubCloseBackends(t, map[string]Backend{"local": local})
	stubPeers(t, []string{"citadel", "minime"}, func(peer, _ string) ([]AgentRecord, error) {
		return []AgentRecord{{ID: "agent-" + peer, Type: "git", RootPane: "w55-1",
			WorkDir: "/w/peer-agent"}}, nil
	})

	rr := postClose(t, `{"pane_id":"p_82","host":"local"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rr.Code, rr.Body.String())
	}
	if len(local.closed) != 0 {
		t.Errorf("closed = %v, want none on an ambiguous adoption", local.closed)
	}
}

// A pane the local herdr doesn't know can't be adopted — peers are never even
// asked (there is no local pane to verify a claim against), and the request 404s.
func TestServeAgentCloseUnknownPaneNeverAsksPeers(t *testing.T) {
	openTestDB(t)
	local := newCloseBackend("local", nil) // resolves nothing
	stubCloseBackends(t, map[string]Backend{"local": local})
	stubPeers(t, []string{"citadel"}, func(_, _ string) ([]AgentRecord, error) {
		t.Error("peer queried for a pane the local herdr doesn't know")
		return nil, nil
	})

	rr := postClose(t, `{"pane_id":"p_99","host":"local"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rr.Code, rr.Body.String())
	}
}

// postAgentClose must POST the calling agent's own herdr pane id — pinned to
// host "local", since the pane lives on the machine closeme runs on — to
// /api/agent/close, carrying basic auth when UI_AUTH is set. The same
// soft-close the UI and close_agent MCP tool use.
func TestPostAgentClose(t *testing.T) {
	var gotPath, gotPaneID, gotHost, gotAuthUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if u, _, ok := r.BasicAuth(); ok {
			gotAuthUser = u
		}
		var body struct {
			PaneID string `json:"pane_id"`
			Host   string `json:"host"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		gotPaneID, gotHost = body.PaneID, body.Host
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
	if gotHost != "local" {
		t.Errorf("host = %q, want local (the pane lives where closeme runs)", gotHost)
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

// A non-200 from the server is surfaced as an error carrying the server's own
// explanation (an ambiguity message is useless if closeme swallows it).
func TestPostAgentCloseServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "pane is ambiguous", http.StatusConflict)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	err := postAgentClose(addr, "p_82", "", "", false)
	if err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
	if !strings.Contains(err.Error(), "pane is ambiguous") {
		t.Errorf("error %q should carry the server's detail", err)
	}
}
