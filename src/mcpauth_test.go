package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearerRT attaches a bearer token to every request, standing in for an agent's
// MCP client configured with its host's credential.
type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// mcpTestServer serves the real /mcp handler — withMCPAuth wrapping the real MCP
// server — over httptest, and returns a connected SDK client session presenting
// token. This is the whole path a spawned agent takes, so it proves the link the
// design hangs on: that the host resolved from the credential actually arrives at
// a tool handler (through auth.RequireBearerToken → the request context → the
// streamable transport → RequestExtra.TokenInfo), rather than only appearing to
// in a source read.
func mcpTestSession(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(withMCPAuth(newMCPHandler(), "", "", false))
	t.Cleanup(srv.Close)
	c := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             srv.URL,
		HTTPClient:           &http.Client{Transport: bearerRT{token: token}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect to /mcp with a bearer token: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// hostClientToken provisions a per-host credential and completes the
// client_credentials grant with it, exactly as an agent's MCP client would.
func hostClientToken(t *testing.T, host, scope string) string {
	t.Helper()
	id, secret := "cid-"+host, "secret-"+host
	if _, err := db.Exec(
		`INSERT INTO oauth_clients (client_id, secret_hash, redirect_uris, name, created_at, host, mcp_scope)
		 VALUES (?, ?, '[]', ?, ?, ?, ?)`,
		id, hashToken(secret), "agents on "+host, time.Now().UTC().Format(time.RFC3339), host, scope,
	); err != nil {
		t.Fatal(err)
	}
	resp, body := postForm(t, serveOAuthToken, url.Values{
		"grant_type": {"client_credentials"},
	}, [2]string{id, secret})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client_credentials for %s: status %d (%v)", host, resp.StatusCode, body)
	}
	tok, _ := body["access_token"].(string)
	if tok == "" {
		t.Fatalf("no access_token for %s: %v", host, body)
	}
	return tok
}

// callTool invokes one tool over the session, decoding its structured output
// into out and returning the tool's error text (empty when it succeeded).
func callTool(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any, out any) string {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool transport error: %v", err)
	}
	if res.IsError {
		var msg strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msg.WriteString(tc.Text)
			}
		}
		return msg.String()
	}
	if b, err := json.Marshal(res.StructuredContent); err == nil {
		_ = json.Unmarshal(b, out)
	}
	return ""
}

// callListAgents invokes the list_agents tool and returns its decoded output
// alongside whether the call came back as an error.
func callListAgents(t *testing.T, sess *mcp.ClientSession, args map[string]any) (listAgentsOut, string) {
	t.Helper()
	var out listAgentsOut
	if errMsg := callTool(t, sess, "list_agents", args, &out); errMsg != "" {
		return listAgentsOut{}, errMsg
	}
	return out, ""
}

// The end-to-end proof: a token minted for one host confines that caller to it,
// over the real transport, with no argument the caller could have lied about.
func TestMCPCallerScopeOverTheRealEndpoint(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	stubSSHHosts(t, "gigachad")
	if err := appendAgent("local", AgentRecord{ID: "loc1", Title: "titan agent", Type: "git",
		RootPane: "wR:p1", WorkDir: "/w/loc1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := appendAgent("gigachad", AgentRecord{ID: "gig1", Title: "pod agent", Type: "git",
		RootPane: "wR:p2", WorkDir: "/w/gig1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	sess := mcpTestSession(t, hostClientToken(t, "gigachad", scopeSelf))

	// Omitting host lands on the caller's OWN host, resolved from its credential.
	out, errMsg := callListAgents(t, sess, map[string]any{})
	if errMsg != "" {
		t.Fatalf("list_agents with no host failed: %s", errMsg)
	}
	if out.Host != "gigachad" {
		t.Errorf("host = %q, want gigachad — the credential's host, not lasso's", out.Host)
	}
	if len(out.Agents) != 1 || out.Agents[0].ID != "gig1" {
		t.Errorf("agents = %+v, want only gig1", out.Agents)
	}

	// Naming the lasso host is refused. This is the containment claim: the caller
	// asked for another host in the clear and did not get it.
	if _, errMsg = callListAgents(t, sess, map[string]any{"host": "local"}); errMsg == "" {
		t.Fatal("a self-scoped caller listed the lasso host over the real endpoint")
	}
	if !strings.Contains(errMsg, "credential") || !strings.Contains(errMsg, "gigachad") {
		t.Errorf("refusal = %q, want the credential explanation naming gigachad", errMsg)
	}

	// A fleet-scoped credential on the same fleet reaches both hosts.
	fleet := mcpTestSession(t, hostClientToken(t, "local", scopeFleet))
	if out, errMsg = callListAgents(t, fleet, map[string]any{"host": "gigachad"}); errMsg != "" {
		t.Fatalf("fleet caller refused gigachad: %s", errMsg)
	}
	if len(out.Agents) != 1 || out.Agents[0].ID != "gig1" {
		t.Errorf("fleet caller saw %+v on gigachad, want gig1", out.Agents)
	}
}

// Groups, end to end over the real endpoint: two hosts in one group address
// each other with credentials neither of them can lie about, a host outside it
// is refused, and an edit made with `lasso mcp-group` lands on the very next
// call — same session, same token, nothing re-minted.
func TestMCPHostGroupsOverTheRealEndpoint(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	stubSSHHosts(t, "norm", "norm-darren", "outsider")
	for _, a := range []struct{ host, id, title, pane string }{
		{"norm", "n1", "norm agent", "wN:p1"},
		{"norm-darren", "d1", "darren agent", "wD:p1"},
		{"outsider", "o1", "outsider agent", "wO:p1"},
	} {
		if err := appendAgent(a.host, AgentRecord{ID: a.id, Title: a.title, Type: "git",
			RootPane: a.pane, WorkDir: "/w/" + a.id, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := createGroup("norm-stack"); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"norm", "norm-darren"} {
		if _, err := addGroupMember("norm-stack", h, memberKindHost); err != nil {
			t.Fatal(err)
		}
	}

	norm := mcpTestSession(t, hostClientToken(t, "norm", scopeSelf))
	darren := mcpTestSession(t, hostClientToken(t, "norm-darren", scopeSelf))
	outsider := mcpTestSession(t, hostClientToken(t, "outsider", scopeSelf))

	// Mutual: each sees the other's agents, and neither had to say who it was.
	out, errMsg := callListAgents(t, norm, map[string]any{"host": "norm-darren"})
	if errMsg != "" {
		t.Fatalf("norm was refused its group-mate: %s", errMsg)
	}
	if len(out.Agents) != 1 || out.Agents[0].ID != "d1" {
		t.Errorf("norm saw %+v on norm-darren, want d1", out.Agents)
	}
	if out, errMsg = callListAgents(t, darren, map[string]any{"host": "norm"}); errMsg != "" {
		t.Fatalf("norm-darren was refused its group-mate: %s", errMsg)
	}
	if len(out.Agents) != 1 || out.Agents[0].ID != "n1" {
		t.Errorf("norm-darren saw %+v on norm, want n1", out.Agents)
	}

	// A host outside the group is refused in both directions, with a refusal that
	// points at the command that would change it.
	_, errMsg = callListAgents(t, norm, map[string]any{"host": "outsider"})
	if errMsg == "" {
		t.Fatal("a group caller reached a host outside its group")
	}
	if !strings.Contains(errMsg, "lasso mcp-group") {
		t.Errorf("refusal = %q, want it to name the group command", errMsg)
	}
	if _, errMsg = callListAgents(t, outsider, map[string]any{"host": "norm"}); errMsg == "" {
		t.Fatal("a host outside the group listed a group member's agents")
	}

	// message_agent resolves recipients against the same bound. There is no live
	// herdr behind these hosts, so an in-group recipient can only get as far as
	// dialing its host — but that is the distinction under test: the group-mate
	// resolves to its host, while the outsider is stopped by the credential.
	var msg messageAgentOut
	if errMsg = callTool(t, norm, "message_agent", map[string]any{
		"to":   []string{"darren agent@norm-darren", "outsider agent@outsider"},
		"text": "ping", "from": "norm",
	}, &msg); errMsg != "" {
		t.Fatalf("message_agent: %s", errMsg)
	}
	if len(msg.Results) != 2 {
		t.Fatalf("results = %+v, want two", msg.Results)
	}
	if !strings.Contains(msg.Results[0].Detail, "norm-darren") ||
		strings.Contains(msg.Results[0].Detail, "credential") {
		t.Errorf("group-mate detail = %q, want it resolved to the host rather than refused",
			msg.Results[0].Detail)
	}
	if !strings.Contains(msg.Results[1].Detail, "credential") {
		t.Errorf("outsider detail = %q, want the credential refusal", msg.Results[1].Detail)
	}

	// The live-edit claim: the verifier resolves reach per request, so removing a
	// member takes effect on the caller's next call — no re-mint, no reconnect.
	if _, err := removeGroupMember("norm-stack", "norm-darren", memberKindHost); err != nil {
		t.Fatal(err)
	}
	if _, errMsg = callListAgents(t, norm, map[string]any{"host": "norm-darren"}); errMsg == "" {
		t.Fatal("the group edit did not take effect on the next call over the same session")
	}
	if _, err := addGroupMember("norm-stack", "norm-darren", memberKindHost); err != nil {
		t.Fatal(err)
	}
	if _, errMsg = callListAgents(t, norm, map[string]any{"host": "norm-darren"}); errMsg != "" {
		t.Fatalf("re-adding the member did not restore reach: %s", errMsg)
	}
}

// An invalid token never reaches a tool: RequireBearerToken answers 401 with the
// RFC 9728 challenge, which is what starts a client's discovery.
func TestMCPRejectsAnUnknownToken(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	srv := httptest.NewServer(withMCPAuth(newMCPHandler(), "", "", false))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer not-a-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if wa := resp.Header.Get("WWW-Authenticate"); !strings.Contains(wa, "resource_metadata=") {
		t.Errorf("WWW-Authenticate = %q, want the RFC 9728 resource_metadata challenge", wa)
	}
}
