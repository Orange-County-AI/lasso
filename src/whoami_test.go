package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// whoamiBackend stubs the pane.get whoami uses to confirm a pane is live and
// read its status. UHP has one pane-id form — the decimal string $LUVUS_PANE_ID
// holds, which is also what lasso persists as root_pane — so `live` is simply
// the set of ids that name a pane. failGet forces pane.get to error, exercising
// the "runtime unreachable, match the id as given" path.
type whoamiBackend struct {
	*memBackend
	live    map[string]bool
	failGet bool
}

func (b *whoamiBackend) LuvusCall(method string, params any) (json.RawMessage, error) {
	p, _ := params.(map[string]any)
	switch method {
	case "pane.get":
		if b.failGet {
			return nil, &luvusError{Code: "internal", Message: "luvus down"}
		}
		id, _ := p["pane"].(string)
		if !b.live[id] {
			return nil, &luvusError{Code: "not_found", Message: "pane not found"}
		}
		return json.RawMessage(fmt.Sprintf(`{"type":"pane","pane":%q,"status":"working"}`, id)), nil
	}
	return uhpFixtureReply(nil, method, params)
}

func whoamiRecs() []AgentRecord {
	return []AgentRecord{
		{ID: "other", RootPane: "3", CreatedAt: time.Now()},
		{ID: "self", Title: "whoami", Type: "scratch", RootPane: "82", CreatedAt: time.Now()},
	}
}

// The headline case: an agent passes the $LUVUS_PANE_ID from its own shell and
// whoami resolves it to its lasso record, carrying the live status pane.get
// reported.
func TestResolveWhoamiMapsEnvPaneToAgent(t *testing.T) {
	b := &whoamiBackend{memBackend: newMemBackend(), live: map[string]bool{"82": true}}
	out := resolveWhoami(b, "local", whoamiRecs(), "82")
	if !out.Found || out.Agent == nil {
		t.Fatalf("expected found, got %+v", out)
	}
	if out.Agent.ID != "self" {
		t.Errorf("resolved to wrong agent: %q", out.Agent.ID)
	}
	if out.Agent.Status != "working" {
		t.Errorf("status = %q, want working (carried from pane.get)", out.Agent.Status)
	}
}

// An unreachable runtime must not make an agent unable to identify itself: the
// id is matched against root_pane as given, and only the live status is lost.
func TestResolveWhoamiFallsBackWhenRuntimeUnavailable(t *testing.T) {
	b := &whoamiBackend{memBackend: newMemBackend(), failGet: true}
	out := resolveWhoami(b, "local", whoamiRecs(), "82")
	if !out.Found || out.Agent == nil || out.Agent.ID != "self" {
		t.Fatalf("expected fallback match, got %+v", out)
	}
}

// No pane_id: a structured answer that tells the caller to pass $LUVUS_PANE_ID,
// not an opaque error.
func TestResolveWhoamiEmptyPaneID(t *testing.T) {
	b := &whoamiBackend{memBackend: newMemBackend()}
	out := resolveWhoami(b, "local", whoamiRecs(), "  ")
	if out.Found || out.Agent != nil {
		t.Fatalf("expected not found, got %+v", out)
	}
	if out.Detail == "" {
		t.Error("expected a detail explaining LUVUS_PANE_ID is required")
	}
}

// A pane lasso doesn't manage: found:false with an explanation, no error.
func TestResolveWhoamiUnknownPane(t *testing.T) {
	b := &whoamiBackend{memBackend: newMemBackend(), live: map[string]bool{"99": true}}
	out := resolveWhoami(b, "local", whoamiRecs(), "99")
	if out.Found || out.Agent != nil {
		t.Fatalf("expected not found, got %+v", out)
	}
	if out.Detail == "" {
		t.Error("expected a detail for an unmanaged pane")
	}
}

// ---------------------------------------------------------------------------
// whoami with no host — cross-host resolution through whoamiTool
// ---------------------------------------------------------------------------

// The field bug this guards against: the MCP server ran on one box while the
// caller's pane lived on another, and BOTH hosts had a pane "31" mapped to
// a lasso agent. whoami defaulted host to "local" and identified the caller as
// the other host's agent — which the caller would then close. Without a host,
// a cross-host pane-id collision must be refused, naming the candidate hosts.
func TestWhoamiOmittedHostRefusesCrossHostCollision(t *testing.T) {
	openTestDB(t)
	if err := appendAgent("local", AgentRecord{ID: "djexrfh3p79z", Type: "git",
		RootPane: "31", WorkDir: "/w/citadel", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := appendAgent("gigachad", AgentRecord{ID: "dk3n97h1oxig", Type: "git",
		RootPane: "31", WorkDir: "/w/gigachad", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stubCloseBackends(t, map[string]Backend{
		"local":    newCloseBackend("local", map[string]string{"31": "31"}),
		"gigachad": newCloseBackend("gigachad", map[string]string{"31": "31"}),
	})

	_, out, err := whoamiTool(context.Background(), nil, whoamiIn{PaneID: "31"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Found || out.Agent != nil {
		t.Fatalf("expected refusal on a cross-host pane collision, got %+v", out)
	}
	for _, h := range []string{"local", "gigachad"} {
		if !strings.Contains(out.Detail, h) {
			t.Errorf("detail %q should name candidate host %q", out.Detail, h)
		}
	}
}

// The same collision with an explicit host resolves to THAT host's own agent.
func TestWhoamiExplicitHostResolvesCollision(t *testing.T) {
	openTestDB(t)
	if err := appendAgent("local", AgentRecord{ID: "djexrfh3p79z", Type: "git",
		RootPane: "31", WorkDir: "/w/citadel", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := appendAgent("gigachad", AgentRecord{ID: "dk3n97h1oxig", Type: "git",
		RootPane: "31", WorkDir: "/w/gigachad", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stubCloseBackends(t, map[string]Backend{
		"local":    newCloseBackend("local", map[string]string{"31": "31"}),
		"gigachad": newCloseBackend("gigachad", map[string]string{"31": "31"}),
	})

	_, out, err := whoamiTool(context.Background(), nil, whoamiIn{Host: "gigachad", PaneID: "31"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Found || out.Agent == nil {
		t.Fatalf("expected found with an explicit host, got %+v", out)
	}
	if out.Agent.ID != "dk3n97h1oxig" || out.Agent.Host != "gigachad" {
		t.Errorf("resolved to %s@%s, want dk3n97h1oxig@gigachad", out.Agent.ID, out.Agent.Host)
	}
}

// A pane id that exists on exactly one host resolves without a host hint —
// even when that host is NOT the box the MCP server runs on — and the returned
// record carries the owning host so close_agent can be pointed at it.
func TestWhoamiOmittedHostResolvesUniqueRemotePane(t *testing.T) {
	openTestDB(t)
	if err := appendAgent("local", AgentRecord{ID: "loc1", Type: "git",
		RootPane: "wA:p1", WorkDir: "/w/loc1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := appendAgent("gigachad", AgentRecord{ID: "dk3n97h1oxig", Type: "git",
		RootPane: "31", WorkDir: "/w/gigachad", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stubCloseBackends(t, map[string]Backend{
		"local":    newCloseBackend("local", map[string]string{"wA:p1": "wA:p1"}),
		"gigachad": newCloseBackend("gigachad", map[string]string{"31": "31"}),
	})

	_, out, err := whoamiTool(context.Background(), nil, whoamiIn{PaneID: "31"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Found || out.Agent == nil {
		t.Fatalf("expected a unique remote pane to resolve, got %+v", out)
	}
	if out.Agent.ID != "dk3n97h1oxig" || out.Agent.Host != "gigachad" {
		t.Errorf("resolved to %s@%s, want dk3n97h1oxig@gigachad", out.Agent.ID, out.Agent.Host)
	}
}

// Without a host, a $LUVUS_PANE_ID is resolved by searching every host — via
// LOCAL luvus only — so a local caller keeps resolving as before.
func TestWhoamiOmittedHostCanonicalizesLocalRawID(t *testing.T) {
	openTestDB(t)
	if err := appendAgent("local", AgentRecord{ID: "self", Type: "scratch",
		RootPane: "55", WorkDir: "/w/self", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stubCloseBackends(t, map[string]Backend{
		"local": newCloseBackend("local", map[string]string{"55": "55"}),
	})

	_, out, err := whoamiTool(context.Background(), nil, whoamiIn{PaneID: "55"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Found || out.Agent == nil || out.Agent.ID != "self" {
		t.Fatalf("expected the id to resolve to the local agent, got %+v", out)
	}
}

// A local pane no record of ours claims falls back to peer adoption (the
// closeme topology: the agent was spawned here by another machine's lasso), so
// whoami still answers with the adopted record.
func TestWhoamiOmittedHostAdoptsPeerRecord(t *testing.T) {
	openTestDB(t) // no local records at all
	local := newCloseBackend("local", map[string]string{"55": "55"})
	_ = local.MkdirAll("/w/peer-agent", 0o755)
	stubCloseBackends(t, map[string]Backend{"local": local})
	stubPeers(t, []string{"citadel"}, func(peer, rootPane string) ([]AgentRecord, error) {
		if peer == "citadel" && rootPane == "55" {
			return []AgentRecord{{ID: "dk33", Type: "git", RootPane: "55",
				WorkspaceID: "w55", WorkDir: "/w/peer-agent"}}, nil
		}
		return nil, nil
	})

	_, out, err := whoamiTool(context.Background(), nil, whoamiIn{PaneID: "55"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Found || out.Agent == nil || out.Agent.ID != "dk33" {
		t.Fatalf("expected the peer's record to be adopted, got %+v", out)
	}
	if out.Agent.Host != "local" {
		t.Errorf("adopted agent host = %q, want local (the pane lives here)", out.Agent.Host)
	}
}
