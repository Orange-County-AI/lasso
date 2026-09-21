package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The chat used to resolve its pane against the tab's host and nothing else, so
// every way of putting another machine's session on screen read the wrong
// herdr: a selected herdr MACHINE showed the local box's focused pane outright,
// and an ssh ATTACH looked for the far side's transcript on a disk that never
// had it. These cover both, plus the ordinary pane that must stay untouched.

// chatScreenBackend is a fake herdr for one host: a pane listing, a process_info
// per pane (what makes an attach recoverable), an agent listing, and a home dir
// the herdr client's machine selection can be written into.
type chatScreenBackend struct {
	Backend
	name  string
	home  string
	panes string            // pane.list payload
	procs map[string]string // pane id → pane.process_info payload
	agent string            // agent.list payload
}

func (b *chatScreenBackend) Name() string { return b.name }

func (b *chatScreenBackend) HomeDir() (string, error) { return b.home, nil }

func (b *chatScreenBackend) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

func (b *chatScreenBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	switch method {
	case "pane.list":
		return json.RawMessage(b.panes), nil
	case "pane.process_info":
		id, _ := params.(map[string]any)["pane_id"].(string)
		if body, ok := b.procs[id]; ok {
			return json.RawMessage(body), nil
		}
		return json.RawMessage(`{"process_info":{}}`), nil
	case "agent.list":
		if b.agent != "" {
			return json.RawMessage(b.agent), nil
		}
		return json.RawMessage(`{"agents":[]}`), nil
	}
	return json.RawMessage(`{}`), nil
}

// chatPaneList is a one-pane listing, focused, with a claude session id — the
// shape paneTranscript reads.
func chatPaneList(paneID, session string) string {
	return fmt.Sprintf(`{"panes":[{"pane_id":%q,"workspace_id":"w1","tab_id":"t1","cwd":"/work","focused":true,`+
		`"agent":"claude","agent_status":"idle",`+
		`"agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":%q}}]}`,
		paneID, session)
}

// chatFleet stands a local backend and one remote up, both addressable, with the
// local one as the default. The pane.list cache is keyed by host and lives
// across tests, so it is cleared for each fixture.
func chatFleet(t *testing.T, local, remote *chatScreenBackend) {
	t.Helper()
	// The pane.list cache is keyed by host and outlives a test, so both fakes'
	// entries are stamped stale — otherwise the previous test's listing answers
	// for this one's host of the same name.
	clear := func() {
		invalidatePaneList(local.name)
		invalidatePaneList(remote.name)
	}
	clear()
	t.Cleanup(clear)
	stubSSHHosts(t, remote.name)
	stubProbedHosts(t, remote.name)
	prevFn := hostBackendFn
	hostBackendFn = func(host string) (Backend, error) {
		if host == remote.name {
			return remote, nil
		}
		return nil, fmt.Errorf("host %q not available", host)
	}
	prev := defaultBackend()
	setDefaultBackend(local)
	t.Cleanup(func() { hostBackendFn = prevFn; setDefaultBackend(prev) })
}

// writeMachineSelection points the herdr client's saved selection at target,
// under be's home. The selection is cached on a 1s TTL, so the caches go too.
func writeMachineSelection(t *testing.T, home, target string) {
	t.Helper()
	dir := filepath.Join(home, ".local", "state", "herdr", "client")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "3f87b9ed37174500ee3dcbc5f75b1616"
	files := map[string]string{
		"endpoint-selection.json": fmt.Sprintf(`{"version":1,"selected_profile":%q}`, id),
		"endpoints.json": fmt.Sprintf(
			`{"version":1,"ssh":[{"id":%q,"label":%q,"target":%q,"session":"default","enabled":true}]}`,
			id, target, target),
	}
	for f, body := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resetMachineCaches()
	t.Cleanup(resetMachineCaches)
}

// The reported bug: the terminal is drawing norm's session (a saved herdr
// machine), so the conversation on screen — and the transcript behind it — is
// norm's, not the local herdr's focused pane.
func TestChatScreenPaneFollowsSelectedMachine(t *testing.T) {
	local := &chatScreenBackend{name: "local", home: t.TempDir(), panes: chatPaneList("wL:p1", "local-session")}
	remote := &chatScreenBackend{name: "norm", panes: chatPaneList("wR:p9", "norm-session")}
	chatFleet(t, local, remote)
	writeMachineSelection(t, local.home, "norm")

	// The local pane id the browser sends, which must NOT be carried across:
	// herdr's ids are unique per server only.
	scr, err := chatScreenPane(local, "wL:p1")
	if err != nil {
		t.Fatalf("chatScreenPane: %v", err)
	}
	if scr.note != "" {
		t.Fatalf("note = %q, want the remote pane", scr.note)
	}
	if scr.host != "norm" || scr.be.Name() != "norm" {
		t.Errorf("host = %q / backend %q, want norm — the transcript is on the machine being drawn",
			scr.host, scr.be.Name())
	}
	if scr.pane.PaneID != "wR:p9" {
		t.Errorf("pane = %q, want wR:p9 — the machine's own focused pane", scr.pane.PaneID)
	}
}

// A machine on screen that lasso cannot read is a note, never the tab's own
// focused pane: that pane is a stranger's conversation, and the composer would
// type into it.
func TestChatScreenPaneUnreachableMachineRefusesTheLocalPane(t *testing.T) {
	local := &chatScreenBackend{name: "local", home: t.TempDir(), panes: chatPaneList("wL:p1", "local-session")}
	// Reachable enough for hostAliasFor, but its herdr answers nothing.
	remote := &chatScreenBackend{name: "norm", panes: `{"panes":[]}`}
	chatFleet(t, local, remote)
	writeMachineSelection(t, local.home, "norm")

	scr, err := chatScreenPane(local, "wL:p1")
	if err != nil {
		t.Fatalf("chatScreenPane: %v", err)
	}
	if scr.pane.PaneID != "" || scr.be != nil {
		t.Fatalf("resolved pane %q on %v, want none", scr.pane.PaneID, scr.be)
	}
	if scr.note == "" || scr.host != "norm" {
		t.Errorf("note = %q on host %q, want a note naming norm", scr.note, scr.host)
	}
}

// An ssh attach is a local pane that is a WINDOW onto a remote one. The
// transcript is the far side's, so the chat has to read it there.
func TestChatScreenPaneFollowsSSHAttach(t *testing.T) {
	local := &chatScreenBackend{
		name:  "local",
		home:  t.TempDir(),
		panes: chatPaneList("wL:p1", "sniffed-from-the-wire"),
		procs: map[string]string{
			"wL:p1": `{"process_info":{"foreground_process_group_id":42,"foreground_processes":[` +
				`{"pid":42,"name":"ssh","cwd":"/home/stephan","argv":["ssh","-t","norm","herdr","agent","attach","porter"]}]}}`,
		},
	}
	remote := &chatScreenBackend{
		name:  "norm",
		panes: chatPaneList("wR:p3", "norm-session"),
		agent: `{"agents":[{"name":"porter","pane_id":"wR:p3","workspace_id":"w1","tab_id":"t1","cwd":"/work",` +
			`"agent":"claude","agent_status":"idle",` +
			`"agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"norm-session"}}]}`,
	}
	chatFleet(t, local, remote)

	scr, err := chatScreenPane(local, "wL:p1")
	if err != nil {
		t.Fatalf("chatScreenPane: %v", err)
	}
	if scr.host != "norm" || scr.pane.PaneID != "wR:p3" {
		t.Fatalf("resolved %q on %q, want wR:p3 on norm — the attached agent's own pane",
			scr.pane.PaneID, scr.host)
	}
	if scr.pane.AgentSession == nil || scr.pane.AgentSession.Value != "norm-session" {
		t.Errorf("session = %+v, want the far side's", scr.pane.AgentSession)
	}
}

// The common case, unchanged: an ordinary pane on the tab's own host, with no
// selection file and no hop to find.
func TestChatScreenPaneOrdinaryPaneStaysLocal(t *testing.T) {
	local := &chatScreenBackend{name: "local", home: t.TempDir(), panes: chatPaneList("wL:p1", "local-session")}
	remote := &chatScreenBackend{name: "norm", panes: chatPaneList("wR:p9", "norm-session")}
	chatFleet(t, local, remote)

	for _, want := range []string{"", "wL:p1"} {
		scr, err := chatScreenPane(local, want)
		if err != nil {
			t.Fatalf("chatScreenPane(%q): %v", want, err)
		}
		if scr.host != "local" || scr.pane.PaneID != "wL:p1" {
			t.Errorf("chatScreenPane(%q) = %q on %q, want wL:p1 on local", want, scr.pane.PaneID, scr.host)
		}
	}
	if _, err := chatScreenPane(local, "no-such-pane"); err == nil {
		t.Error("chatScreenPane(unknown pane) = nil error, want errChatNoPane")
	}
}
