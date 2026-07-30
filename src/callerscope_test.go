package main

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callerReq builds the tool-call request a caller with a given credential would
// arrive as: the SDK copies the verified TokenInfo onto every request's Extra,
// and that is the only channel the caller's host travels through.
func callerReq(clientID, host, scope string) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{
		TokenInfo: &auth.TokenInfo{
			UserID:     clientID,
			Expiration: time.Now().Add(time.Hour),
			Extra:      map[string]any{tokenHostKey: host, tokenScopeKey: scope},
		},
	}}
}

func TestCallerFromToken(t *testing.T) {
	// No token at all — UI_AUTH basic, or the open /mcp — keeps the historical
	// fleet-wide view, so enabling this feature changes nothing until a per-host
	// credential exists.
	for _, req := range []*mcp.CallToolRequest{nil, {}, {Extra: &mcp.RequestExtra{}}} {
		if c := callerFrom(req); !c.Fleet || c.identified() {
			t.Errorf("callerFrom(%+v) = %+v, want an unidentified fleet caller", req, c)
		}
	}
	self := callerFrom(callerReq("c1", "gigachad", scopeSelf))
	if self.Host != "gigachad" || self.Fleet || !self.identified() {
		t.Errorf("self-scoped caller = %+v, want host gigachad, fleet false", self)
	}
	fleet := callerFrom(callerReq("c2", "gigachad", scopeFleet))
	if fleet.Host != "gigachad" || !fleet.Fleet {
		t.Errorf("fleet-scoped caller = %+v, want host gigachad, fleet true", fleet)
	}
	// A credential that names no host cannot be confined to one, whatever its
	// recorded scope says.
	hostless := callerFrom(callerReq("c3", "", scopeSelf))
	if !hostless.Fleet {
		t.Errorf("hostless caller = %+v, want fleet true — there is no host to confine it to", hostless)
	}
}

func TestCallerAllowsAndDefaultHost(t *testing.T) {
	stubSSHHosts(t, "gigachad", "minime")
	self := mcpCaller{ClientID: "c1", Host: "gigachad"}

	if !self.allows("gigachad") {
		t.Error("a self-scoped caller must reach its own host")
	}
	for _, h := range []string{"minime", "local", ""} {
		if self.allows(h) {
			t.Errorf("a self-scoped caller on gigachad must not reach %q", h)
		}
	}
	// The default host is the caller's OWN box, not lasso's — "local" would point
	// a remote agent at the lasso host's agents and call them its neighbours.
	if got := self.hostOr(""); got != "gigachad" {
		t.Errorf("hostOr(\"\") = %q, want gigachad", got)
	}
	if got := self.hostOr("minime"); got != "minime" {
		t.Errorf("an explicit host must win, got %q", got)
	}
	if got := anyCaller().hostOr(""); got != "local" {
		t.Errorf("unidentified hostOr(\"\") = %q, want local", got)
	}
	// hosts() intersects with what lasso can address, so a scope naming a host
	// with no ssh alias yields nothing rather than an unreachable promise.
	if hs := (mcpCaller{Host: "citadel"}).hosts(); len(hs) != 0 {
		t.Errorf("hosts() for an unaliased host = %v, want empty", sortedHosts(hs))
	}
	if hs := self.hosts(); len(hs) != 1 || !hs["gigachad"] {
		t.Errorf("self hosts() = %v, want [gigachad]", sortedHosts(hs))
	}
	if hs := anyCaller().hosts(); !hs["gigachad"] || !hs["minime"] || !hs["local"] {
		t.Errorf("fleet hosts() = %v, want the whole addressable set", sortedHosts(hs))
	}
}

func TestCallerAgentsFiltersByScope(t *testing.T) {
	stubSSHHosts(t, "gigachad", "minime")
	all := []hostAgent{
		{Host: "local", Agent: AgentRecord{ID: "a1"}},
		{Host: "gigachad", Agent: AgentRecord{ID: "b1"}},
		{Host: "minime", Agent: AgentRecord{ID: "c1"}},
		{Host: "citadel", Agent: AgentRecord{ID: "d1"}}, // no ssh alias
	}
	self := mcpCaller{Host: "gigachad"}
	got := self.agents(all)
	if len(got) != 1 || got[0].Agent.ID != "b1" {
		t.Fatalf("self-scoped agents = %v, want only b1@gigachad", ids(got))
	}
	// A fleet caller still gets the config bound applied — citadel stays hidden.
	got = anyCaller().agents(all)
	if len(got) != 3 {
		t.Fatalf("fleet agents = %v, want the three addressable records", ids(got))
	}
	for _, ha := range got {
		if ha.Host == "citadel" {
			t.Error("a fleet caller must still not see a host with no ssh alias")
		}
	}
}

func TestRequireHostExplainsTheCredential(t *testing.T) {
	stubSSHHosts(t, "gigachad", "minime")
	self := mcpCaller{ClientID: "c1", Host: "gigachad"}
	if err := self.requireHost("gigachad"); err != nil {
		t.Fatalf("own host refused: %v", err)
	}
	err := self.requireHost("minime")
	if err == nil {
		t.Fatal("a self-scoped caller reached another host")
	}
	// The refusal has to say what the credential is, not just "no".
	for _, want := range []string{"gigachad", "minime", "credential"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	// Both bounds apply: in-scope but with no ssh alias is still refused, with the
	// hostscope reason rather than the credential one.
	if err := (mcpCaller{Host: "citadel"}).requireHost("citadel"); err == nil ||
		!strings.Contains(err.Error(), "ssh config") {
		t.Errorf("unaliased own host: err = %v, want the ssh-config refusal", err)
	}
}

// ---------------------------------------------------------------------------
// through the tools
// ---------------------------------------------------------------------------

// containedFleet sets up a two-host fleet with one agent each and returns the
// request a caller confined to "gigachad" would arrive as.
func containedFleet(t *testing.T) *mcp.CallToolRequest {
	t.Helper()
	openTestDB(t)
	if err := appendAgent("local", AgentRecord{ID: "loc1", Title: "titan agent", Type: "git",
		RootPane: "wR:p1", WorkDir: "/w/loc1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := appendAgent("gigachad", AgentRecord{ID: "gig1", Title: "pod agent", Type: "git",
		RootPane: "wR:p1", WorkDir: "/w/gig1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	local := newCloseBackend("local", map[string]string{"wR:p1": "wR:p1"})
	stubCloseBackends(t, map[string]Backend{
		"local":    local,
		"gigachad": newCloseBackend("gigachad", map[string]string{"wR:p1": "wR:p1"}),
	})
	stubPeers(t, nil, nil)
	// list_hosts reports the active host, which only a running server sets.
	prev := curBackend()
	setBackend(local)
	t.Cleanup(func() { setBackend(prev) })
	return callerReq("c-gigachad", "gigachad", scopeSelf)
}

func TestListAgentsScopedToTheCallersHost(t *testing.T) {
	req := containedFleet(t)

	// Naming another host is refused outright.
	if _, out, err := listAgentsTool(context.Background(), req, listAgentsIn{Host: "local"}); err == nil {
		t.Fatalf("a contained caller listed the lasso host: %+v", out)
	}
	// With no host it gets ITS OWN, not lasso's — the ergonomic half of the fix.
	_, out, err := listAgentsTool(context.Background(), req, listAgentsIn{})
	if err != nil {
		t.Fatalf("listing its own host failed: %v", err)
	}
	if out.Host != "gigachad" {
		t.Errorf("default host = %q, want gigachad", out.Host)
	}
	if len(out.Agents) != 1 || out.Agents[0].ID != "gig1" {
		t.Errorf("agents = %+v, want only gig1", out.Agents)
	}
}

func TestListHostsHidesTheRestOfTheFleet(t *testing.T) {
	req := containedFleet(t)
	_, out, err := listHostsTool(context.Background(), req, listHostsIn{})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range out.Hosts {
		if h.Host != "gigachad" {
			t.Errorf("contained caller was shown host %q", h.Host)
		}
	}
	// And the unidentified caller still sees the local row it always did.
	_, out, err = listHostsTool(context.Background(), nil, listHostsIn{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Hosts) == 0 || out.Hosts[0].Host != "local" {
		t.Errorf("unidentified list_hosts = %+v, want the local row first", out.Hosts)
	}
}

func TestMessageAgentCannotCrossOutOfScope(t *testing.T) {
	req := containedFleet(t)
	_, out, err := messageAgentTool(context.Background(), req, messageAgentIn{
		To: []string{"titan agent", "titan agent@local"}, Text: "ping", From: "pod",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Results {
		if r.Queued {
			t.Fatalf("queued a message out of scope: %+v", r)
		}
	}
	// Unqualified: the other host's agent is simply not there.
	if strings.Contains(out.Results[0].Detail, "local") {
		t.Errorf("detail leaked the out-of-scope host: %q", out.Results[0].Detail)
	}
	// Host-qualified: the caller named the host, so it learns why.
	if !strings.Contains(out.Results[1].Detail, "credential") {
		t.Errorf("detail = %q, want the credential refusal", out.Results[1].Detail)
	}
}

func TestCloseAgentCannotCrossOutOfScope(t *testing.T) {
	req := containedFleet(t)
	local := agentBackendResolverMust(t, "local").(*closeBackend)

	if _, _, err := closeAgentTool(context.Background(), req, closeAgentIn{AgentID: "loc1"}); err == nil {
		t.Fatal("a contained caller closed an agent on the lasso host")
	}
	if len(local.closed) != 0 {
		t.Errorf("local closed = %v, want none", local.closed)
	}
	// Its own host's agent still closes — containment, not paralysis.
	if _, _, err := closeAgentTool(context.Background(), req, closeAgentIn{AgentID: "gig1"}); err != nil {
		t.Fatalf("closing its own agent failed: %v", err)
	}
}

// A contained caller's pane id resolves without any cross-host search, so the
// collision that forces no-host whoami to give up cannot arise: both hosts here
// have an agent in a pane called "wR:p1", and it still resolves.
func TestWhoamiUsesTheCredentialsHost(t *testing.T) {
	req := containedFleet(t)
	_, out, err := whoamiTool(context.Background(), req, whoamiIn{PaneID: "wR:p1"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Found || out.Agent == nil {
		t.Fatalf("whoami did not resolve: %+v", out)
	}
	if out.Agent.ID != "gig1" || out.Agent.Host != "gigachad" {
		t.Errorf("resolved to %s@%s, want gig1@gigachad", out.Agent.ID, out.Agent.Host)
	}
}

// sortedHosts renders a host set deterministically for failure messages.
func sortedHosts(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// agentBackendResolverMust fetches a stubbed backend by host for assertions.
func agentBackendResolverMust(t *testing.T, host string) Backend {
	t.Helper()
	b, err := agentBackendResolver(host)
	if err != nil {
		t.Fatalf("no stubbed backend for %q: %v", host, err)
	}
	return b
}

// ---------------------------------------------------------------------------
// the credential itself
// ---------------------------------------------------------------------------

// The token, not the caller, carries the host: a token minted for a per-host
// client verifies into a TokenInfo naming that host and scope.
func TestTokenVerifierCarriesHostAndScope(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	if _, err := db.Exec(
		`INSERT INTO oauth_clients (client_id, secret_hash, redirect_uris, name, created_at, host, mcp_scope)
		 VALUES ('pod-cid', ?, '[]', 'pod', ?, 'gigachad', 'self')`,
		hashToken("pod-secret"), time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}
	tok, err := issueToken("access", "pod-cid", oauthScope, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ti, err := mcpTokenVerifier(context.Background(), tok, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ti.UserID != "pod-cid" {
		t.Errorf("UserID = %q, want the client id (the SDK pins the session to it)", ti.UserID)
	}
	if ti.Expiration.IsZero() {
		t.Error("Expiration must be set — RequireBearerToken rejects a token without one")
	}
	c := callerFrom(&mcp.CallToolRequest{Extra: &mcp.RequestExtra{TokenInfo: ti}})
	if c.Host != "gigachad" || c.Fleet {
		t.Errorf("caller = %+v, want a self-scoped gigachad caller", c)
	}
	// An unknown token is a 401, not a fleet caller.
	if _, err := mcpTokenVerifier(context.Background(), "nope", nil); err == nil {
		t.Error("an unknown token verified")
	}
	// The MCP_OAUTH client names no host and keeps the fleet view.
	stok, err := issueToken("access", "cid", oauthScope, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sti, err := mcpTokenVerifier(context.Background(), stok, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := callerFrom(&mcp.CallToolRequest{Extra: &mcp.RequestExtra{TokenInfo: sti}}); !c.Fleet || c.identified() {
		t.Errorf("MCP_OAUTH caller = %+v, want unidentified + fleet", c)
	}
}

// A per-host client may mint machine tokens (that is its whole purpose); a
// self-registered DCR client still may not.
func TestClientCredentialsForHostClientsOnly(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO oauth_clients (client_id, secret_hash, redirect_uris, name, created_at, host, mcp_scope)
		 VALUES ('pod-cid', ?, '[]', 'pod', ?, 'gigachad', 'self')`,
		hashToken("pod-secret"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO oauth_clients (client_id, secret_hash, redirect_uris, name, created_at)
		 VALUES ('dcr-cid', ?, '[]', 'some connector', ?)`,
		hashToken("dcr-secret"), now); err != nil {
		t.Fatal(err)
	}
	resp, body := postForm(t, serveOAuthToken, url.Values{
		"grant_type": {"client_credentials"},
	}, [2]string{"pod-cid", "pod-secret"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("host client status = %d, want 200 (body %v)", resp.StatusCode, body)
	}
	if tok, _ := body["access_token"].(string); tok == "" {
		t.Fatalf("no access_token for the host client: %v", body)
	}
	resp, body = postForm(t, serveOAuthToken, url.Values{
		"grant_type": {"client_credentials"},
	}, [2]string{"dcr-cid", "dcr-secret"})
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("a DCR client minted a machine token: %v", body)
	}
}
