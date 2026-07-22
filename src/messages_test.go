package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fixedPanes builds a paneLookup over a static host → panes fixture. Hosts
// absent from the fixture read as unreachable.
func fixedPanes(byHost map[string][]pane) paneLookup {
	return func(host string) (map[string]pane, error) {
		ps, ok := byHost[host]
		if !ok {
			return nil, fmt.Errorf("no route to host")
		}
		m := map[string]pane{}
		for _, p := range ps {
			m[p.PaneID] = p
		}
		return m, nil
	}
}

func msgRecords() []hostAgent {
	return []hostAgent{
		{Host: "local", Agent: AgentRecord{ID: "a1", Title: "clem", RootPane: "w1-1"}},
		{Host: "local", Agent: AgentRecord{ID: "a2", Title: "stub", RootPane: "w2-1"}},
		{Host: "gigachad", Agent: AgentRecord{ID: "b1", Title: "clem", RootPane: "w9-1"}},
		{Host: "local", Agent: AgentRecord{ID: "a3", Title: "dead", RootPane: "w3-1"}},
	}
}

func msgPanes() map[string][]pane {
	return map[string][]pane{
		"local": {
			{PaneID: "w1-1", Agent: "claude", AgentStatus: "idle"},
			{PaneID: "w2-1", Agent: "codex", AgentStatus: "working"},
			{PaneID: "w3-1"}, // agent exited: pane is a bare shell
		},
		"gigachad": {
			{PaneID: "w9-1", Agent: "claude", AgentStatus: "idle"},
		},
	}
}

func TestResolveRecipientByTitleAndID(t *testing.T) {
	panes := fixedPanes(msgPanes())
	// Unique live title resolves.
	rec, status, err := resolveRecipient("stub", msgRecords(), panes)
	if err != nil || rec.ID != "a2" || status != "working" {
		t.Fatalf("stub -> (%q, %q, %v), want (a2, working, nil)", rec.ID, status, err)
	}
	// Id resolves too, and titles match case-insensitively.
	if rec, _, err = resolveRecipient("a1", msgRecords(), panes); err != nil || rec.ID != "a1" {
		t.Fatalf("a1 -> (%q, %v), want (a1, nil)", rec.ID, err)
	}
	if rec, _, err = resolveRecipient("STUB", msgRecords(), panes); err != nil || rec.ID != "a2" {
		t.Fatalf("STUB -> (%q, %v), want (a2, nil)", rec.ID, err)
	}
}

func TestResolveRecipientHostQualification(t *testing.T) {
	panes := fixedPanes(msgPanes())
	// "clem" is live on two hosts — must be refused with the candidates listed.
	_, _, err := resolveRecipient("clem", msgRecords(), panes)
	if err == nil || !strings.Contains(err.Error(), "a1@local") || !strings.Contains(err.Error(), "b1@gigachad") {
		t.Fatalf("ambiguous clem: err = %v, want both candidates listed", err)
	}
	// Host-qualified forms disambiguate, by title and by id.
	rec, _, err := resolveRecipient("clem@gigachad", msgRecords(), panes)
	if err != nil || rec.ID != "b1" || rec.Host != "gigachad" {
		t.Fatalf("clem@gigachad -> (%q@%q, %v), want b1@gigachad", rec.ID, rec.Host, err)
	}
	if rec, _, err = resolveRecipient("a1@local", msgRecords(), panes); err != nil || rec.ID != "a1" {
		t.Fatalf("a1@local -> (%q, %v), want a1", rec.ID, err)
	}
}

func TestResolveRecipientDeadAndUnknown(t *testing.T) {
	panes := fixedPanes(msgPanes())
	// A recorded agent whose pane is a bare shell is dead, not addressable.
	_, _, err := resolveRecipient("dead", msgRecords(), panes)
	if err == nil || !strings.Contains(err.Error(), "none is running") {
		t.Fatalf("dead agent: err = %v, want 'none is running'", err)
	}
	if _, _, err = resolveRecipient("nobody", msgRecords(), panes); err == nil {
		t.Fatal("unknown recipient resolved, want error")
	}
	// An unreachable host is reported as such, not as a dead agent.
	_, _, err = resolveRecipient("clem@gigachad", msgRecords(), fixedPanes(map[string][]pane{}))
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("unreachable host: err = %v, want 'unreachable'", err)
	}
}

func TestMessageEnvelope(t *testing.T) {
	m := AgentMessage{ID: "m_x1", SenderLabel: "clem", SenderAddr: "a1@titan", Body: "deploy is green"}
	e := messageEnvelope(m)
	for _, want := range []string{"m_x1", "clem", `"a1@titan"`, "\ndeploy is green"} {
		if !strings.Contains(e, want) {
			t.Errorf("envelope %q missing %q", e, want)
		}
	}
	// No agent sender: no reply address in the header.
	e = messageEnvelope(AgentMessage{ID: "m_x2", SenderLabel: "ci", Body: "hi"})
	if strings.Contains(e, "reply") {
		t.Errorf("envelope without sender addr should carry no reply hint: %q", e)
	}
}

func TestMessageQueueRoundTrip(t *testing.T) {
	openTestDB(t)
	m1 := AgentMessage{ID: "m_1", Host: "local", AgentID: "a1", SenderLabel: "u", Body: "first", CreatedAt: time.Now()}
	m2 := AgentMessage{ID: "m_2", Host: "local", AgentID: "a1", SenderLabel: "u", Body: "second", CreatedAt: time.Now().Add(time.Millisecond)}
	for _, m := range []AgentMessage{m1, m2} {
		if err := enqueueAgentMessage(m); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	pending, err := listPendingMessages()
	if err != nil || len(pending) != 2 || pending[0].ID != "m_1" || pending[1].ID != "m_2" {
		t.Fatalf("pending = %+v (err %v), want [m_1 m_2]", pending, err)
	}
	if err := markMessageDelivered("m_1"); err != nil {
		t.Fatal(err)
	}
	if err := markMessageFailed("m_2", "agent pane is gone"); err != nil {
		t.Fatal(err)
	}
	if pending, _ = listPendingMessages(); len(pending) != 0 {
		t.Fatalf("pending after resolve = %+v, want empty", pending)
	}
}

func TestUpdateAgentTitleByWorkspace(t *testing.T) {
	openTestDB(t)
	rec := AgentRecord{ID: "a1", Title: "old title", WorkspaceID: "w1", RootPane: "w1-1", CreatedAt: time.Now()}
	if err := appendAgent("local", rec); err != nil {
		t.Fatal(err)
	}
	if err := updateAgentTitleByWorkspace("local", "w1", "clem"); err != nil {
		t.Fatal(err)
	}
	recs, _ := listAgents("local")
	if len(recs) != 1 || recs[0].Title != "clem" {
		t.Fatalf("title after rename = %+v, want clem", recs)
	}
	// Empty labels and unknown workspaces are no-ops, not errors.
	if err := updateAgentTitleByWorkspace("local", "w1", "  "); err != nil {
		t.Fatal(err)
	}
	if recs, _ = listAgents("local"); recs[0].Title != "clem" {
		t.Fatalf("blank rename overwrote title: %+v", recs)
	}
}

// msgPaneBackend fakes the herdr surface the dispatcher drives: pane.list
// (status), pane.send_text (capture + composer emulation), and pane.read
// (composer state for paneSubmit's submit confirmation). The composer fills
// when text is pasted and clears when Enter arrives, so paneSubmit's
// paste-confirm/Enter loop completes quickly.
type msgPaneBackend struct {
	*memBackend
	paneID  string
	status  string
	agent   string
	sent    []string // non-Enter text received by pane.send_text
	drafted bool     // composer currently holds an unsubmitted draft
}

func (b *msgPaneBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	p, _ := params.(map[string]any)
	switch method {
	case "pane.list":
		return json.RawMessage(fmt.Sprintf(
			`{"panes":[{"pane_id":%q,"agent":%q,"agent_status":%q}]}`,
			b.paneID, b.agent, b.status)), nil
	case "pane.send_text":
		text, _ := p["text"].(string)
		if text == "\r" {
			b.drafted = false
		} else {
			b.sent = append(b.sent, text)
			b.drafted = true
		}
		return json.RawMessage(`{}`), nil
	case "pane.read":
		box := "❯"
		if b.drafted {
			box = "❯ draft"
		}
		text := "──────────────\n" + box + "\n──────────────"
		res, _ := json.Marshal(map[string]any{"read": map[string]any{"text": text}})
		return res, nil
	}
	return json.RawMessage(`{}`), nil
}

func TestDispatchDeliversOnIdleOnly(t *testing.T) {
	openTestDB(t)
	rec := AgentRecord{ID: "a1", Title: "clem", WorkspaceID: "w1", RootPane: "w1-1", CreatedAt: time.Now()}
	if err := appendAgent("local", rec); err != nil {
		t.Fatal(err)
	}
	b := &msgPaneBackend{memBackend: newMemBackend(), paneID: "w1-1", agent: "claude", status: "working"}
	resolver := func(string) (Backend, error) { return b, nil }

	for i, body := range []string{"first", "second"} {
		m := AgentMessage{ID: fmt.Sprintf("m_%d", i), Host: "local", AgentID: "a1",
			SenderLabel: "stub", SenderAddr: "a2@local", Body: body,
			CreatedAt: time.Now().Add(time.Duration(i) * time.Millisecond)}
		if err := enqueueAgentMessage(m); err != nil {
			t.Fatal(err)
		}
	}

	// Recipient mid-turn: nothing is delivered, messages stay queued.
	dispatchPendingMessages(resolver)
	if len(b.sent) != 0 {
		t.Fatalf("delivered while working: %q", b.sent)
	}
	if pending, _ := listPendingMessages(); len(pending) != 2 {
		t.Fatalf("pending while working = %d, want 2", len(pending))
	}

	// Recipient idle: both messages arrive as ONE submitted turn, in order.
	b.status = "idle"
	dispatchPendingMessages(resolver)
	if len(b.sent) != 1 {
		t.Fatalf("sent %d pastes, want 1 batched: %q", len(b.sent), b.sent)
	}
	got := b.sent[0]
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") ||
		strings.Index(got, "first") > strings.Index(got, "second") {
		t.Fatalf("batched delivery wrong: %q", got)
	}
	if !strings.Contains(got, "stub") || !strings.Contains(got, `"a2@local"`) {
		t.Fatalf("delivery missing sender envelope: %q", got)
	}
	if pending, _ := listPendingMessages(); len(pending) != 0 {
		t.Fatalf("pending after delivery = %+v, want empty", pending)
	}
}

func TestDispatchFailsMessagesForDeadAgent(t *testing.T) {
	openTestDB(t)
	rec := AgentRecord{ID: "a1", Title: "clem", WorkspaceID: "w1", RootPane: "w1-1", CreatedAt: time.Now()}
	if err := appendAgent("local", rec); err != nil {
		t.Fatal(err)
	}
	// The pane is now a bare shell — the agent exited.
	b := &msgPaneBackend{memBackend: newMemBackend(), paneID: "w1-1", agent: "", status: ""}
	m := AgentMessage{ID: "m_1", Host: "local", AgentID: "a1", SenderLabel: "u", Body: "hi", CreatedAt: time.Now()}
	if err := enqueueAgentMessage(m); err != nil {
		t.Fatal(err)
	}
	dispatchPendingMessages(func(string) (Backend, error) { return b, nil })
	if len(b.sent) != 0 {
		t.Fatalf("delivered to a dead agent: %q", b.sent)
	}
	if pending, _ := listPendingMessages(); len(pending) != 0 {
		t.Fatalf("dead agent's message still pending, want failed")
	}
	var status, detail string
	if err := db.QueryRow(`SELECT status, error FROM agent_messages WHERE id='m_1'`).Scan(&status, &detail); err != nil {
		t.Fatal(err)
	}
	if status != msgFailed || detail == "" {
		t.Fatalf("message state = (%q, %q), want failed with a reason", status, detail)
	}
}

func TestDispatchLeavesMessagesOnUnreachableHost(t *testing.T) {
	openTestDB(t)
	rec := AgentRecord{ID: "a1", Title: "clem", WorkspaceID: "w1", RootPane: "w1-1", CreatedAt: time.Now()}
	if err := appendAgent("local", rec); err != nil {
		t.Fatal(err)
	}
	m := AgentMessage{ID: "m_1", Host: "local", AgentID: "a1", SenderLabel: "u", Body: "hi", CreatedAt: time.Now()}
	if err := enqueueAgentMessage(m); err != nil {
		t.Fatal(err)
	}
	dispatchPendingMessages(func(string) (Backend, error) { return nil, fmt.Errorf("ssh: no route") })
	if pending, _ := listPendingMessages(); len(pending) != 1 {
		t.Fatalf("unreachable host should leave messages pending, got %d", len(pending))
	}
}
