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

// callListAgents invokes the list_agents tool and returns its decoded output
// alongside whether the call came back as an error.
func callListAgents(t *testing.T, sess *mcp.ClientSession, args map[string]any) (listAgentsOut, string) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_agents", Arguments: args,
	})
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
		return listAgentsOut{}, msg.String()
	}
	var out listAgentsOut
	if b, err := json.Marshal(res.StructuredContent); err == nil {
		_ = json.Unmarshal(b, &out)
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
