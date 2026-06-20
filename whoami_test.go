package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// whoamiBackend stubs the two herdr RPCs whoami uses. pane.get mirrors how real
// herdr canonicalizes pane ids: it accepts both the raw $HERDR_PANE_ID form
// ("p_82") and the public form ("w<ws>-<n>") and echoes back the public id.
// pane.list backs the status fallback. failGet forces pane.get to error, to
// exercise the "herdr can't resolve, fall back to the raw id" path.
type whoamiBackend struct {
	*memBackend
	resolve map[string]string // raw pane id -> public pane id
	failGet bool
}

func (b *whoamiBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	p, _ := params.(map[string]any)
	switch method {
	case "pane.get":
		if b.failGet {
			return nil, &herdrError{Code: "internal", Message: "herdr down"}
		}
		id, _ := p["pane_id"].(string)
		pub, ok := b.resolve[id]
		if !ok {
			for _, v := range b.resolve { // public form passed directly: echo it
				if v == id {
					pub, ok = id, true
					break
				}
			}
		}
		if !ok {
			return nil, &herdrError{Code: "pane_not_found", Message: "pane not found"}
		}
		return json.RawMessage(fmt.Sprintf(`{"type":"pane_info","pane":{"pane_id":%q,"agent_status":"working"}}`, pub)), nil
	case "pane.list":
		return json.RawMessage(`{"panes":[]}`), nil
	}
	return json.RawMessage(`{}`), nil
}

func whoamiRecs() []AgentRecord {
	return []AgentRecord{
		{ID: "other", RootPane: "w0000000000000-1", CreatedAt: time.Now()},
		{ID: "self", Title: "whoami", Type: "scratch", RootPane: "w6535ed1dd256243-1", CreatedAt: time.Now()},
	}
}

// The headline case: an agent passes its raw $HERDR_PANE_ID and whoami resolves
// it — through herdr's pane.get translation — to its own lasso record.
func TestResolveWhoamiMapsEnvPaneToAgent(t *testing.T) {
	b := &whoamiBackend{
		memBackend: newMemBackend(),
		resolve:    map[string]string{"p_82": "w6535ed1dd256243-1"},
	}
	out := resolveWhoami(b, "local", whoamiRecs(), "p_82")
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

// The public form ("w<ws>-<n>") is accepted too — herdr echoes it back.
func TestResolveWhoamiAcceptsPublicPaneID(t *testing.T) {
	b := &whoamiBackend{
		memBackend: newMemBackend(),
		resolve:    map[string]string{"p_82": "w6535ed1dd256243-1"},
	}
	out := resolveWhoami(b, "local", whoamiRecs(), "w6535ed1dd256243-1")
	if !out.Found || out.Agent == nil || out.Agent.ID != "self" {
		t.Fatalf("expected to resolve public pane id, got %+v", out)
	}
}

// If herdr can't canonicalize the id, whoami still matches a public id passed
// directly against root_pane (status just comes back empty).
func TestResolveWhoamiFallsBackWhenHerdrUnavailable(t *testing.T) {
	b := &whoamiBackend{memBackend: newMemBackend(), failGet: true}
	out := resolveWhoami(b, "local", whoamiRecs(), "w6535ed1dd256243-1")
	if !out.Found || out.Agent == nil || out.Agent.ID != "self" {
		t.Fatalf("expected fallback match, got %+v", out)
	}
}

// No pane_id: a structured answer that tells the caller to pass $HERDR_PANE_ID,
// not an opaque error.
func TestResolveWhoamiEmptyPaneID(t *testing.T) {
	b := &whoamiBackend{memBackend: newMemBackend()}
	out := resolveWhoami(b, "local", whoamiRecs(), "  ")
	if out.Found || out.Agent != nil {
		t.Fatalf("expected not found, got %+v", out)
	}
	if out.Detail == "" {
		t.Error("expected a detail explaining HERDR_PANE_ID is required")
	}
}

// A pane lasso doesn't manage: found:false with an explanation, no error.
func TestResolveWhoamiUnknownPane(t *testing.T) {
	b := &whoamiBackend{
		memBackend: newMemBackend(),
		resolve:    map[string]string{"p_99": "w9999999999999-1"},
	}
	out := resolveWhoami(b, "local", whoamiRecs(), "p_99")
	if out.Found || out.Agent != nil {
		t.Fatalf("expected not found, got %+v", out)
	}
	if out.Detail == "" {
		t.Error("expected a detail for an unmanaged pane")
	}
}
