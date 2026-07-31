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

// callerReqReach is callerReq with the group reach the verifier would have
// resolved for that credential's host attached.
func callerReqReach(clientID, host, scope string, reach ...string) *mcp.CallToolRequest {
	req := callerReq(clientID, host, scope)
	set := map[string]bool{}
	for _, h := range reach {
		set[h] = true
	}
	req.Extra.TokenInfo.Extra[tokenReachKey] = set
	return req
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
// groups — additive on top of "self"
// ---------------------------------------------------------------------------

// A group widens a self-scoped caller and changes nothing else: its own host is
// still its default, and hosts outside the group are still refused.
func TestGroupReachWidensASelfScopedCaller(t *testing.T) {
	stubSSHHosts(t, "norm", "norm-darren", "outsider")
	c := mcpCaller{ClientID: "c1", Host: "norm", Reach: map[string]bool{"norm-darren": true}}

	for _, h := range []string{"norm", "norm-darren"} {
		if !c.allows(h) {
			t.Errorf("a group caller on norm must reach %q", h)
		}
	}
	for _, h := range []string{"outsider", "local", ""} {
		if c.allows(h) {
			t.Errorf("a group caller on norm must NOT reach %q", h)
		}
	}
	// The default host is unchanged by groups — a group says where a caller MAY
	// go, never where it is.
	if got := c.hostOr(""); got != "norm" {
		t.Errorf("hostOr(\"\") = %q, want norm", got)
	}
	if hs := c.hosts(); !hs["norm"] || !hs["norm-darren"] || len(hs) != 2 {
		t.Errorf("hosts() = %v, want [norm norm-darren]", sortedHosts(hs))
	}
	if !c.reachesBeyondOwnHost() {
		t.Error("reachesBeyondOwnHost() = false for a caller with a group-mate")
	}
	if err := c.requireHost("norm-darren"); err != nil {
		t.Errorf("group-mate refused: %v", err)
	}
	// The refusal for an outsider now has to offer the group, not only --fleet:
	// an operator who only hears about --fleet will hand out the fleet.
	err := c.requireHost("outsider")
	if err == nil {
		t.Fatal("a group caller reached a host outside its groups")
	}
	for _, want := range []string{"outsider", "norm", "credential", "lasso mcp-group"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	// "local" is a member like any other, in both directions.
	toLocal := mcpCaller{Host: "norm", Reach: map[string]bool{"local": true}}
	for _, h := range []string{"local", ""} {
		if !toLocal.allows(h) {
			t.Errorf("a caller whose group includes the lasso host must reach %q", h)
		}
	}
	fromLocal := mcpCaller{Host: "local", Reach: map[string]bool{"norm": true}}
	if !fromLocal.allows("norm") || !fromLocal.allows("local") {
		t.Error("a local-host caller must reach its group-mates and itself")
	}
}

// Reach is intersected with what lasso can address, so a member whose ssh alias
// was removed goes inert rather than promising a host nothing can reach.
func TestGroupReachIsInertWithoutAnSSHAlias(t *testing.T) {
	stubSSHHosts(t, "norm") // norm-darren's alias has since been removed
	c := mcpCaller{ClientID: "c1", Host: "norm", Reach: map[string]bool{"norm-darren": true}}

	if hs := c.hosts(); len(hs) != 1 || !hs["norm"] {
		t.Errorf("hosts() = %v, want just norm — the group-mate has no alias", sortedHosts(hs))
	}
	err := c.requireHost("norm-darren")
	if err == nil || !strings.Contains(err.Error(), "ssh config") {
		t.Errorf("requireHost = %v, want the ssh-config refusal (both bounds apply)", err)
	}
	all := []hostAgent{
		{Host: "norm", Agent: AgentRecord{ID: "n1"}},
		{Host: "norm-darren", Agent: AgentRecord{ID: "d1"}},
	}
	if got := c.agents(all); len(got) != 1 || got[0].Agent.ID != "n1" {
		t.Errorf("agents() = %v, want only n1@norm", ids(got))
	}
}

// Fleet and unidentified callers are provably untouched by any group: Fleet is
// the first short-circuit in allows/hosts/agents, so a reach set on a fleet
// caller cannot widen OR narrow it.
func TestFleetAndUnidentifiedCallersIgnoreGroups(t *testing.T) {
	stubSSHHosts(t, "norm", "norm-darren")
	fleet := mcpCaller{ClientID: "c1", Host: "norm", Fleet: true, Reach: map[string]bool{"nonsense": true}}
	for _, h := range []string{"norm", "norm-darren", "local", ""} {
		if !fleet.allows(h) {
			t.Errorf("a fleet caller must still reach %q", h)
		}
	}
	if hs := fleet.hosts(); len(hs) != 3 {
		t.Errorf("fleet hosts() = %v, want the whole addressable set", sortedHosts(hs))
	}
	if any := anyCaller(); any.Reach != nil || !any.Fleet {
		t.Errorf("anyCaller() = %+v, want an unidentified fleet caller with no reach", any)
	}
}

// The reach rides the TOKEN, resolved from the db at verification time — so a
// group edit lands on the caller's next call without re-minting anything, and a
// fleet credential never carries one.
func TestTokenVerifierCarriesGroupReach(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	stubSSHHosts(t, "norm", "norm-darren")
	addHostClient(t, "norm-cid", "norm", scopeSelf)
	addHostClient(t, "fleet-cid", "norm", scopeFleet)
	if _, err := createGroup("norm-stack"); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"norm", "norm-darren"} {
		if _, err := addGroupMember("norm-stack", h, memberKindHost); err != nil {
			t.Fatal(err)
		}
	}
	tok, _, err := mintClientToken("norm-cid", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	callerFor := func(token string) mcpCaller {
		t.Helper()
		ti, err := mcpTokenVerifier(context.Background(), token, nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		return callerFrom(&mcp.CallToolRequest{Extra: &mcp.RequestExtra{TokenInfo: ti}})
	}
	c := callerFor(tok)
	if c.Fleet || !c.allows("norm-darren") {
		t.Fatalf("caller = %+v, want a self-scoped norm caller reaching norm-darren", c)
	}
	// A fleet credential resolves no reach — it needs none.
	ftok, _, err := mintClientToken("fleet-cid", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if f := callerFor(ftok); f.Reach != nil {
		t.Errorf("fleet caller carries a reach set %v, want none", sortedHosts(f.Reach))
	}
	// Edit the group; the SAME token resolves differently on its next request.
	if _, err := removeGroupMember("norm-stack", "norm-darren", memberKindHost); err != nil {
		t.Fatal(err)
	}
	if c := callerFor(tok); c.allows("norm-darren") {
		t.Error("the group edit did not take effect on the next verification")
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

// ---------------------------------------------------------------------------
// close_agent, the one tool a group widens
// ---------------------------------------------------------------------------

// groupedFleet is containedFleet plus a group-mate: three hosts with one agent
// each, and a caller on "norm" whose group reaches "norm-darren" but not the
// lasso host.
func groupedFleet(t *testing.T) *mcp.CallToolRequest {
	t.Helper()
	openTestDB(t)
	for _, a := range []struct{ host, id, pane string }{
		{"local", "loc1", "wL:p1"},
		{"norm", "norm1", "wN:p1"},
		{"norm-darren", "dar1", "wD:p1"},
	} {
		if err := appendAgent(a.host, AgentRecord{ID: a.id, Title: a.id, Type: "git",
			RootPane: a.pane, WorkDir: "/w/" + a.id, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	stubCloseBackends(t, map[string]Backend{
		"local":       newCloseBackend("local", map[string]string{"wL:p1": "wL:p1"}),
		"norm":        newCloseBackend("norm", map[string]string{"wN:p1": "wN:p1"}),
		"norm-darren": newCloseBackend("norm-darren", map[string]string{"wD:p1": "wD:p1"}),
	})
	stubPeers(t, nil, nil)
	return callerReqReach("c-norm", "norm", scopeSelf, "norm-darren")
}

// With no host, close_agent searches every host the caller may reach — which
// for a group caller now includes its group-mates. The search is bounded by
// cs.agents(), so the lasso host's agent stays out of it.
func TestCloseAgentReachesGroupMates(t *testing.T) {
	req := groupedFleet(t)
	darren := agentBackendResolverMust(t, "norm-darren").(*closeBackend)
	local := agentBackendResolverMust(t, "local").(*closeBackend)

	if _, _, err := closeAgentTool(context.Background(), req, closeAgentIn{AgentID: "dar1"}); err != nil {
		t.Fatalf("closing a group-mate's agent failed: %v", err)
	}
	if len(darren.closed) != 1 || darren.closed[0] != "wD:p1" {
		t.Errorf("norm-darren closed = %v, want [wD:p1]", darren.closed)
	}
	// The lasso host is outside the group and stays outside it.
	if _, _, err := closeAgentTool(context.Background(), req, closeAgentIn{AgentID: "loc1"}); err == nil {
		t.Fatal("a group caller closed an agent on a host outside its groups")
	}
	if len(local.closed) != 0 {
		t.Errorf("local closed = %v, want none", local.closed)
	}
	// Naming the out-of-scope host outright is refused with the reason.
	_, _, err := closeAgentTool(context.Background(), req, closeAgentIn{AgentID: "loc1", Host: "local"})
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Errorf("explicit out-of-scope host: err = %v, want the credential refusal", err)
	}
}

// Peer adoption resolves a LOCAL pane from another lasso's records, going
// around the db that bounds every other empty-host branch. A group caller whose
// reach excludes the lasso host must not get through it — otherwise widening
// close_agent's search would have handed it every local pane by pane id.
func TestCloseAgentGroupCallerCannotAdoptALocalPane(t *testing.T) {
	openTestDB(t) // no records of our own — adoption is the only way to resolve
	local := newCloseBackend("local", map[string]string{"p_82": "w55-1"})
	_ = local.MkdirAll("/w/peer-agent", 0o755)
	stubCloseBackends(t, map[string]Backend{"local": local, "norm": newCloseBackend("norm", nil),
		"norm-darren": newCloseBackend("norm-darren", nil)})
	stubPeers(t, []string{"citadel"}, func(_, rootPane string) ([]AgentRecord, error) {
		if rootPane != "w55-1" {
			return nil, nil
		}
		return []AgentRecord{{ID: "dk33", Type: "git", RootPane: "w55-1",
			WorkspaceID: "w55", WorkDir: "/w/peer-agent"}}, nil
	})

	req := callerReqReach("c-norm", "norm", scopeSelf, "norm-darren")
	if _, _, err := closeAgentTool(context.Background(), req, closeAgentIn{PaneID: "p_82"}); err == nil {
		t.Fatal("a group caller adopted and closed a pane on the lasso host")
	}
	if len(local.closed) != 0 {
		t.Errorf("local closed = %v, want none", local.closed)
	}
	// The same request from an unidentified caller still adopts — proving the
	// setup was adoptable and the refusal above came from the scope gate.
	if _, _, err := closeAgentTool(context.Background(), nil, closeAgentIn{PaneID: "p_82"}); err != nil {
		t.Fatalf("unidentified adoption broke: %v", err)
	}
	if len(local.closed) != 1 || local.closed[0] != "w55-1" {
		t.Errorf("local closed = %v, want [w55-1] for the unidentified caller", local.closed)
	}
}

// A plain self-scoped caller (no groups) keeps exactly its old close_agent
// behavior: the empty host collapses to its own, so a colliding pane id on
// another host is neither found nor refused as ambiguous.
func TestCloseAgentWithoutGroupsIsUnchanged(t *testing.T) {
	req := containedFleet(t)
	gigachad := agentBackendResolverMust(t, "gigachad").(*closeBackend)
	if _, _, err := closeAgentTool(context.Background(), req, closeAgentIn{PaneID: "wR:p1"}); err != nil {
		t.Fatalf("closing its own pane failed: %v", err)
	}
	if len(gigachad.closed) != 1 {
		t.Errorf("gigachad closed = %v, want its own pane", gigachad.closed)
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
