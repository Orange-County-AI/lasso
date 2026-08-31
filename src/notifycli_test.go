package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The headline case: an agent passes its own $HERDR_PANE_ID and the notification
// is titled with its name and points at its host, so a lock screen says who is
// asking without the human reading the body.
func TestNotifyToolAttributesToTheCallingAgent(t *testing.T) {
	openTestDB(t)
	if err := appendAgent("gigachad", AgentRecord{ID: "dk3n97h1oxig", Type: "git",
		Title: "Port the auth tests", RootPane: "w1F:p1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stubCloseBackends(t, map[string]Backend{
		"gigachad": newCloseBackend("gigachad", map[string]string{"w1F:p1": "w1F:p1"}),
	})
	fake := &fakeTransport{live: true}
	useNotifTransport(t, fake)

	_, out, err := notifyTool(context.Background(), nil, notifyIn{
		Message: "  vitest is green — merge it?  ",
		PaneID:  "w1F:p1",
		Host:    "gigachad",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Sent || out.Title != "Port the auth tests" {
		t.Fatalf("out = %+v", out)
	}
	if out.Detail != "" {
		t.Errorf("detail = %q, want empty on a clean send", out.Detail)
	}
	if len(out.Transports) != 1 || out.Transports[0] != "fake" {
		t.Errorf("transports = %v", out.Transports)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.got) != 1 {
		t.Fatalf("published %d notifications, want 1", len(fake.got))
	}
	n := fake.got[0]
	if n.Kind != notifAgentMessage {
		t.Errorf("kind = %q", n.Kind)
	}
	if n.Body != "vitest is green — merge it?" {
		t.Errorf("body = %q (whitespace should be trimmed)", n.Body)
	}
	if n.Host != "gigachad" {
		t.Errorf("host = %q, want the agent's own host so opening it lands there", n.Host)
	}
	// A deliberate message must never collapse an earlier one: two messages from
	// one agent are two things it said.
	if n.Tag != "" {
		t.Errorf("tag = %q, want none", n.Tag)
	}
}

// The message is the point, so an unresolvable pane must not swallow it — it
// goes out unattributed, and the reply says why it could not be attributed.
func TestNotifyToolStillSendsWhenThePaneIsUnknown(t *testing.T) {
	openTestDB(t)
	stubCloseBackends(t, map[string]Backend{
		"local": newCloseBackend("local", map[string]string{"wZ:p9": "wZ:p9"}),
	})
	fake := &fakeTransport{live: true}
	useNotifTransport(t, fake)

	_, out, err := notifyTool(context.Background(), nil, notifyIn{
		Message: "the deploy finished",
		PaneID:  "wZ:p9",
		Host:    "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Sent {
		t.Fatalf("an unattributable notification must still be delivered: %+v", out)
	}
	if out.Title != "lasso" {
		t.Errorf("title = %q, want the fallback", out.Title)
	}
	if out.Detail == "" {
		t.Error("want a detail explaining why the sender could not be named")
	}
}

// An explicit title wins over the agent's name, and no pane id at all is fine.
func TestNotifyToolExplicitTitleNeedsNoPane(t *testing.T) {
	openTestDB(t)
	fake := &fakeTransport{live: true}
	useNotifTransport(t, fake)
	_, out, err := notifyTool(context.Background(), nil, notifyIn{
		Message: "staging is down",
		Title:   "Deploy failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Sent || out.Title != "Deploy failed" || out.Detail != "" {
		t.Fatalf("out = %+v", out)
	}
}

// "sent" has to be honest. An agent that reports "I notified you" when nothing
// is subscribed is worse than one that never tries, so the reply says so and
// names the fix.
func TestNotifyToolReportsThatNothingIsSubscribed(t *testing.T) {
	openTestDB(t)
	useNotifTransport(t) // no transports at all
	_, out, err := notifyTool(context.Background(), nil, notifyIn{Message: "anyone there?"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Sent {
		t.Fatal("nothing is subscribed; sent must be false")
	}
	if out.Detail != notifyNoDestination {
		t.Errorf("detail = %q, want the no-destination explanation", out.Detail)
	}
}

func TestNotifyToolRequiresAMessage(t *testing.T) {
	openTestDB(t)
	useNotifTransport(t, &fakeTransport{live: true})
	if _, _, err := notifyTool(context.Background(), nil, notifyIn{Message: "   "}); err == nil {
		t.Error("an empty message must be refused")
	}
}

// A long message is clipped rather than rejected: a push body is capped, and an
// agent that pasted a stack trace should still get its human's attention.
func TestNotifyToolClipsALongMessage(t *testing.T) {
	openTestDB(t)
	fake := &fakeTransport{live: true}
	useNotifTransport(t, fake)
	if _, _, err := notifyTool(context.Background(), nil, notifyIn{Message: strings.Repeat("x", 5000)}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if n := len([]rune(fake.got[0].Body)); n > 400 {
		t.Errorf("body is %d runes, want it clipped", n)
	}
}

// The CLI path, over the real /mcp handler: `lasso notify` is a client of the
// same tool, so this is the whole round trip minus the flag parsing —
// connect, initialize, call, decode.
func TestCallNotifyToolOverMCP(t *testing.T) {
	openTestDB(t)
	fake := &fakeTransport{live: true}
	useNotifTransport(t, fake)
	srv := httptest.NewServer(withMCPAuth(newMCPHandler(), "", "", false))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := callNotifyTool(ctx, srv.URL, http.DefaultClient, notifyIn{
		Message: "the migration needs a decision",
		Title:   "Schema",
	})
	if err != nil {
		t.Fatalf("callNotifyTool: %v", err)
	}
	if !out.Sent || out.Title != "Schema" {
		t.Fatalf("out = %+v", out)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.got) != 1 || fake.got[0].Body != "the migration needs a decision" {
		t.Errorf("published %+v", fake.got)
	}
}

// A tool-level refusal has to come back as an error the CLI can print, not as a
// silent success — the SDK puts a handler's error in the content, not in the
// protocol response.
func TestCallNotifyToolSurfacesAToolError(t *testing.T) {
	openTestDB(t)
	useNotifTransport(t, &fakeTransport{live: true})
	srv := httptest.NewServer(withMCPAuth(newMCPHandler(), "", "", false))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := callNotifyTool(ctx, srv.URL, http.DefaultClient, notifyIn{Message: ""})
	if err == nil {
		t.Fatal("expected an error for an empty message")
	}
	if !strings.Contains(err.Error(), "message is required") {
		t.Errorf("error = %v, want the tool's own explanation", err)
	}
}

func TestMCPEndpointFromEnvironment(t *testing.T) {
	t.Setenv("LASSO_URL", "")
	t.Setenv("LASSO_LISTEN", "")
	if got := mcpEndpoint(); got != "http://"+defaultListenAddr+"/mcp" {
		t.Errorf("default = %q", got)
	}
	t.Setenv("LASSO_LISTEN", "127.0.0.1:9999")
	if got := mcpEndpoint(); got != "http://127.0.0.1:9999/mcp" {
		t.Errorf("LASSO_LISTEN = %q", got)
	}
	// A lasso behind TLS is not reachable as http://host:port, so a full base URL
	// wins over the address.
	t.Setenv("LASSO_URL", "https://lasso.example.com/")
	if got := mcpEndpoint(); got != "https://lasso.example.com/mcp" {
		t.Errorf("LASSO_URL = %q", got)
	}
}

// Whatever credential the environment offers has to actually reach /mcp:
// a bearer token when one is minted, else the UI_AUTH basic credentials.
func TestMCPCLIClientCarriesCredentials(t *testing.T) {
	seen := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
	}))
	defer srv.Close()
	get := func() string {
		resp, err := mcpCLIClient().Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return <-seen
	}

	t.Setenv("LASSO_MCP_TOKEN", "")
	t.Setenv("UI_AUTH", "")
	if got := get(); got != "" {
		t.Errorf("no credentials configured, sent %q", got)
	}
	t.Setenv("UI_AUTH", "stephan:hunter2")
	if got := get(); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("UI_AUTH set, sent %q", got)
	}
	// A minted token outranks basic auth: it is the credential that carries a
	// caller's host scope, which basic auth does not.
	t.Setenv("LASSO_MCP_TOKEN", "tok123")
	if got := get(); got != "Bearer tok123" {
		t.Errorf("token set, sent %q", got)
	}
}
