package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// stubSSHHosts points the ssh-config reader at a fixed alias list, so the host
// scope under test is the one the test declares rather than whatever
// ~/.ssh/config the machine running the suite happens to have.
func stubSSHHosts(t *testing.T, aliases ...string) {
	t.Helper()
	prev := sshConfigHostsFn
	sshConfigHostsFn = func() []string { return aliases }
	t.Cleanup(func() { sshConfigHostsFn = prev })
}

func TestHostAddressableFollowsTheSSHConfig(t *testing.T) {
	stubSSHHosts(t, "gigachad", "minime")

	// The local box is always addressable, however it is spelled.
	for _, h := range []string{"", "local"} {
		if !hostAddressable(h) {
			t.Errorf("hostAddressable(%q) = false, want true — the local box is always in scope", h)
		}
	}
	if !hostAddressable("gigachad") {
		t.Error("an alias in the ssh config should be addressable")
	}
	// A host lasso once recorded agents on but has no alias for is out of scope,
	// which is the whole point: its agents can't be reached, so they can't be
	// seen either.
	if hostAddressable("citadel") {
		t.Error("a host with no ssh-config alias should NOT be addressable")
	}
}

func TestRequireAddressableHostExplainsTheRule(t *testing.T) {
	stubSSHHosts(t, "gigachad")
	if err := requireAddressableHost("gigachad"); err != nil {
		t.Fatalf("requireAddressableHost(gigachad) = %v, want nil", err)
	}
	err := requireAddressableHost("citadel")
	if err == nil {
		t.Fatal("requireAddressableHost(citadel) = nil, want a refusal")
	}
	// The message has to name the host and the rule — "not available" would read
	// as a machine that is merely asleep.
	for _, want := range []string{"citadel", "ssh config", "list_hosts"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
}

func TestAddressableAgentsDropsUnreachableHosts(t *testing.T) {
	stubSSHHosts(t, "gigachad")
	all := []hostAgent{
		{Host: "local", Agent: AgentRecord{ID: "a1"}},
		{Host: "gigachad", Agent: AgentRecord{ID: "b1"}},
		{Host: "citadel", Agent: AgentRecord{ID: "c1"}}, // alias since removed
	}
	got := addressableAgents(all)
	if len(got) != 2 || got[0].Agent.ID != "a1" || got[1].Agent.ID != "b1" {
		t.Fatalf("addressableAgents kept %v, want the local + gigachad records in order", ids(got))
	}
}

// ids renders a record set compactly for failure messages.
func ids(has []hostAgent) []string {
	out := make([]string, len(has))
	for i, ha := range has {
		out[i] = ha.Agent.ID + "@" + ha.Host
	}
	return out
}

// ---------------------------------------------------------------------------
// the rule, through the MCP tools
// ---------------------------------------------------------------------------

// offAliasAgent records one agent on a host lasso has no ssh alias for — the
// shape every test below is about: a real record, on a machine this lasso
// cannot connect to, which must therefore be invisible rather than listed as a
// peer no call could reach. Only "local" is declared reachable.
func offAliasAgent(t *testing.T, host, id, title, pane string) *closeBackend {
	t.Helper()
	openTestDB(t)
	if err := appendAgent(host, AgentRecord{ID: id, Title: title, Type: "git",
		RootPane: pane, WorkDir: "/w/" + id, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	local := newCloseBackend("local", map[string]string{pane: pane})
	stubCloseBackends(t, map[string]Backend{"local": local}) // ⇒ no remote aliases
	stubPeers(t, nil, nil)
	return local
}

// list_agents must refuse a host with no alias instead of answering from the
// agents db — the records would name agents that no send, read, or close could
// ever reach.
func TestListAgentsRefusesAHostWithNoSSHAlias(t *testing.T) {
	offAliasAgent(t, "citadel", "rem1", "remote agent", "wC:p3")

	_, out, err := listAgentsTool(context.Background(), nil, listAgentsIn{Host: "citadel"})
	if err == nil {
		t.Fatalf("list_agents on an unaliased host returned %+v, want a refusal", out)
	}
	if !strings.Contains(err.Error(), "citadel") || !strings.Contains(err.Error(), "ssh config") {
		t.Errorf("refusal %q should name the host and the rule", err)
	}
	if len(out.Agents) != 0 {
		t.Errorf("refused listing still carried %d agent(s)", len(out.Agents))
	}
}

// whoami with no host searches the addressable hosts only, so a pane recorded
// on an unaliased host does not resolve.
func TestWhoamiSkipsHostsWithNoSSHAlias(t *testing.T) {
	offAliasAgent(t, "citadel", "rem1", "remote agent", "wC:p3")

	_, out, err := whoamiTool(context.Background(), nil, whoamiIn{PaneID: "wC:p3"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Found {
		t.Fatalf("resolved to %+v, want found:false — citadel has no ssh alias", out.Agent)
	}
}

// close_agent with no host likewise never lands on an unaliased host's record.
func TestCloseAgentSkipsHostsWithNoSSHAlias(t *testing.T) {
	local := offAliasAgent(t, "citadel", "rem1", "remote agent", "wC:p3")

	_, _, err := closeAgentTool(context.Background(), nil, closeAgentIn{AgentID: "rem1"})
	if err == nil {
		t.Fatal("close_agent found an agent on a host with no ssh alias, want a refusal")
	}
	if len(local.closed) != 0 {
		t.Errorf("local closed = %v, want none — the close leaked onto the wrong host", local.closed)
	}
}

// message_agent resolves recipients against the addressable agents only: an
// agent on an unaliased host is not a candidate, and naming that host outright
// is refused with the rule rather than a bare "no agent matches".
func TestMessageAgentCannotAddressHostsWithNoSSHAlias(t *testing.T) {
	offAliasAgent(t, "citadel", "rem1", "clem", "wC:p3")

	_, out, err := messageAgentTool(context.Background(), nil, messageAgentIn{
		To: []string{"clem", "clem@citadel"}, Text: "ping", From: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(out.Results))
	}
	for _, r := range out.Results {
		if r.Queued || r.MessageID != "" {
			t.Errorf("queued %q for an unreachable host: %+v", r.To, r)
		}
	}
	// The unqualified spec simply finds nothing — the agent is invisible, not
	// advertised as an unreachable peer.
	if strings.Contains(out.Results[0].Detail, "citadel") {
		t.Errorf("detail for %q leaked the hidden host: %q", out.Results[0].To, out.Results[0].Detail)
	}
	// The host-qualified spec named citadel itself, so the answer says why.
	if !strings.Contains(out.Results[1].Detail, "ssh config") {
		t.Errorf("detail for %q = %q, want the scope rule", out.Results[1].To, out.Results[1].Detail)
	}
}

func TestSplitRecipientHost(t *testing.T) {
	cases := []struct{ spec, needle, host string }{
		{"clem", "clem", ""},
		{"clem@gigachad", "clem", "gigachad"},
		{"fix login flow @gigachad", "fix login flow", "gigachad"},
		{"a@b@gigachad", "a@b", "gigachad"}, // splits at the LAST @
		{"@gigachad", "@gigachad", ""},      // no needle: not a qualifier
		{"clem@", "clem@", ""},              // no host: not a qualifier
	}
	for _, c := range cases {
		needle, host := splitRecipientHost(c.spec)
		if needle != c.needle || host != c.host {
			t.Errorf("splitRecipientHost(%q) = (%q, %q), want (%q, %q)", c.spec, needle, host, c.needle, c.host)
		}
	}
}
