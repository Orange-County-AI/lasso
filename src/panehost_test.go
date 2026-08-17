package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSSHDest(t *testing.T) {
	cases := []struct {
		argv []string
		dest string
		cmd  []string
	}{
		// The live shape: what a fleet attach script runs. The -o value must not
		// be mistaken for the destination.
		{
			argv: []string{"ssh", "-t", "-o", "ConnectTimeout=10", "norm", "herdr", "agent", "attach", "norm"},
			dest: "norm",
			cmd:  []string{"herdr", "agent", "attach", "norm"},
		},
		// Bundled booleans and an attached option value.
		{
			argv: []string{"ssh", "-tt", "-oBatchMode=yes", "-i", "/k/id", "dev@box", "herdr"},
			dest: "dev@box",
			cmd:  []string{"herdr"},
		},
		{argv: []string{"ssh", "norm"}, dest: "norm"},
		{argv: []string{"ssh", "--", "-weird-host", "herdr"}, dest: "-weird-host", cmd: []string{"herdr"}},
		// No destination at all.
		{argv: []string{"ssh", "-V"}},
		{argv: []string{"ssh", "-o", "SendEnv=LANG"}},
	}
	for _, c := range cases {
		dest, cmd := sshDest(c.argv)
		// Joined, so a trailing empty command compares equal to no command:
		// only the split point is under test.
		if dest != c.dest || strings.Join(cmd, " ") != strings.Join(c.cmd, " ") {
			t.Errorf("sshDest(%q) = (%q, %q), want (%q, %q)", c.argv, dest, cmd, c.dest, c.cmd)
		}
	}
}

func TestHerdrAttachTarget(t *testing.T) {
	cases := []struct {
		cmd    []string
		target string
		ok     bool
	}{
		{cmd: []string{"herdr", "agent", "attach", "norm"}, target: "norm", ok: true},
		{cmd: []string{"/home/u/.local/bin/herdr", "agent", "attach", "w5:p1"}, target: "w5:p1", ok: true},
		// The remote command arrives as one string when it was quoted.
		{cmd: []string{"herdr agent attach norm"}, target: "norm", ok: true},
		// Session-level attaches follow the far side's focused pane.
		{cmd: []string{"herdr"}, ok: true},
		{cmd: []string{"herdr", "--session", "fleet"}, ok: true},
		{cmd: []string{"herdr", "session", "attach", "fleet"}, ok: true},
		// Not an attach: a one-shot command, or no herdr at all.
		{cmd: []string{"herdr", "agent", "list"}},
		{cmd: []string{"herdr", "pane", "read", "--pane", "w5:p1"}},
		{cmd: nil},
		{cmd: []string{"tail", "-f", "/var/log/syslog"}},
		{cmd: []string{"bash", "-lc", "herdr agent attach norm"}},
	}
	for _, c := range cases {
		target, ok := herdrAttachTarget(c.cmd)
		if target != c.target || ok != c.ok {
			t.Errorf("herdrAttachTarget(%q) = (%q, %v), want (%q, %v)", c.cmd, target, ok, c.target, c.ok)
		}
	}
}

func TestSplitSSHDest(t *testing.T) {
	cases := map[string][2]string{
		"norm":                          {"", "norm"},
		"dev@ws.example.com":            {"dev", "ws.example.com"},
		"ssh://dev@ws.example.com:2222": {"dev", "ws.example.com"},
		// scp-style host:path is not an ssh destination — refusing it keeps a
		// stray argument from being read as a host.
		"box:/srv": {"", ""},
		"box:2222": {"", ""},
		"":         {"", ""},
	}
	for dest, want := range cases {
		user, host := splitSSHDest(dest)
		if user != want[0] || host != want[1] {
			t.Errorf("splitSSHDest(%q) = (%q, %q), want (%q, %q)", dest, user, host, want[0], want[1])
		}
	}
}

// paneSSHHop must recognise the attach shapes and stay silent on everything
// else: a false positive repoints the file viewer at another machine.
func TestPaneSSHHop(t *testing.T) {
	// hostAliasFor only ever answers for a host lasso may drive; "local" is the
	// one alias that is always allowed, so it stands in for a real one here.
	// gridHostAllowed compares against the active host, so a backend must be
	// installed (in production one always is, from main).
	swapBackend(t, &localBackend{})

	attach := paneProcessInfo{
		ForegroundProcessGroupID: 10,
		ForegroundProcesses: []paneProcess{
			{PID: 10, Name: "bash", Cwd: "/home/u", Argv: []string{"bash", "/home/u/.local/bin/agent-attach", "local", "clem"}},
			{PID: 11, Name: "ssh", Cwd: "/home/u", Argv: []string{"ssh", "-t", "-o", "ConnectTimeout=10", "local", "herdr", "agent", "attach", "clem"}},
		},
	}
	hop, ok := paneSSHHop(attach)
	if !ok || hop.host != "local" || hop.agent != "clem" {
		t.Fatalf("paneSSHHop(agent attach) = (%+v, %v), want host local agent clem", hop, ok)
	}

	remote := paneProcessInfo{
		ForegroundProcessGroupID: 20,
		ForegroundProcesses: []paneProcess{
			{PID: 20, Name: "herdr", Cwd: "/home/u", Argv: []string{"herdr", "--remote", "local"}},
		},
	}
	hop, ok = paneSSHHop(remote)
	if !ok || hop.host != "local" || hop.agent != "" {
		t.Fatalf("paneSSHHop(--remote) = (%+v, %v), want host local, session attach", hop, ok)
	}

	// A plain ssh shell: nobody can say where it has cd'd to, so it must not be
	// treated as a window onto the far host.
	shell := paneProcessInfo{
		ForegroundProcessGroupID: 30,
		ForegroundProcesses: []paneProcess{
			{PID: 30, Name: "ssh", Cwd: "/home/u", Argv: []string{"ssh", "local"}},
		},
	}
	if hop, ok := paneSSHHop(shell); ok {
		t.Errorf("paneSSHHop(plain ssh) = (%+v, true), want no hop", hop)
	}

	// An ordinary agent pane, and a destination lasso has no host for.
	agent := paneProcessInfo{
		ForegroundProcessGroupID: 40,
		ForegroundProcesses: []paneProcess{
			{PID: 40, Name: "claude", Cwd: "/home/u/app", Argv: []string{"claude"}},
		},
	}
	if hop, ok := paneSSHHop(agent); ok {
		t.Errorf("paneSSHHop(agent pane) = (%+v, true), want no hop", hop)
	}
	unknown := paneProcessInfo{
		ForegroundProcessGroupID: 50,
		ForegroundProcesses: []paneProcess{
			{PID: 50, Name: "ssh", Cwd: "/home/u", Argv: []string{"ssh", "not-a-configured-host", "herdr"}},
		},
	}
	if hop, ok := paneSSHHop(unknown); ok {
		t.Errorf("paneSSHHop(unknown host) = (%+v, true), want no hop", hop)
	}
}

// attachedPane picks the named agent's pane out of agent.list, and refuses to
// substitute another pane when the named one is gone.
func TestAttachedPaneByAgent(t *testing.T) {
	be := &fakeHerdrBackend{res: map[string]string{
		"agent.list": `{"agents":[
			{"name":"norm","pane_id":"w5:p1","terminal_id":"term_1","cwd":"/home/dev/projects/norm","foreground_cwd":"/home/dev/projects/norm","agent":"claude","agent_status":"idle"},
			{"name":"other","pane_id":"w6:p1","terminal_id":"term_2","cwd":"/home/dev/other"}]}`,
	}}
	p, ok := attachedPane(be, "norm")
	if !ok || p.PaneID != "w5:p1" {
		t.Fatalf("attachedPane(norm) = (%+v, %v), want pane w5:p1", p, ok)
	}
	if p, ok := attachedPane(be, "term_2"); !ok || p.PaneID != "w6:p1" {
		t.Errorf("attachedPane(by terminal id) = (%+v, %v), want pane w6:p1", p, ok)
	}
	if p, ok := attachedPane(be, "gone"); ok {
		t.Errorf("attachedPane(missing agent) = (%+v, true), want no pane", p)
	}
}

// A session-level attach follows the far side's FOCUSED pane.
func TestAttachedPaneFocused(t *testing.T) {
	be := &fakeHerdrBackend{res: map[string]string{
		"pane.list": `{"panes":[
			{"pane_id":"w1:p1","cwd":"/home/dev","focused":false},
			{"pane_id":"w2:p1","cwd":"/home/dev/app","focused":true}]}`,
	}}
	p, ok := attachedPane(be, "")
	if !ok || p.PaneID != "w2:p1" || paneCwd(p) != "/home/dev/app" {
		t.Fatalf("attachedPane(session) = (%+v, %v), want the focused pane", p, ok)
	}
	// herdr up but nothing focused: no answer rather than an arbitrary pane.
	empty := &fakeHerdrBackend{res: map[string]string{"pane.list": `{"panes":[{"pane_id":"w1:p1"}]}`}}
	if p, ok := attachedPane(empty, ""); ok {
		t.Errorf("attachedPane(no focus) = (%+v, true), want no pane", p)
	}
}

// fakeHerdrBackend answers herdr RPCs from a canned map; every other Backend
// method panics, so a test that strays off the RPC path says so loudly.
type fakeHerdrBackend struct {
	Backend
	res map[string]string
}

func (f *fakeHerdrBackend) Name() string { return "fake" }

func (f *fakeHerdrBackend) HerdrCall(method string, _ any) (json.RawMessage, error) {
	body, ok := f.res[method]
	if !ok {
		return nil, &herdrError{Code: "method_not_found", Message: method}
	}
	return json.RawMessage(body), nil
}
