package main

import (
	"encoding/json"
	"strings"
	"testing"
)

type paneHostNamedBackend struct {
	Backend
	name string
}

func (b *paneHostNamedBackend) Name() string { return b.name }

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
		{
			cmd:    []string{`H=$(command -v herdr 2>/dev/null || printf %s "$HOME/.local/bin/herdr"); exec "$H" agent attach 'stub'`},
			target: "stub",
			ok:     true,
		},
		{
			cmd:    []string{`H=$(command -v herdr 2>/dev/null || printf %s "$HOME/.local/bin/herdr"); exec "$H" agent attach 'stub' --takeover`},
			target: "stub",
			ok:     true,
		},
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
		// Shell text is not attach authority: only agent-attach's exact resolver
		// wrapper and a safe literal target are accepted.
		{cmd: []string{`echo ready; H=$(command -v herdr 2>/dev/null || printf %s "$HOME/.local/bin/herdr"); exec "$H" agent attach 'stub' --takeover`}},
		{cmd: []string{`H=$(command -v herdr 2>/dev/null || printf %s "$HOME/.local/bin/herdr"); exec "$H" agent attach 'stub;touch' --takeover`}},
		{cmd: []string{`H=$(command -v herdr 2>/dev/null || printf %s "$HOME/.local/bin/herdr"); exec "$H" agent attach 'stub' --read-only`}},
		{cmd: []string{`H=$(command -v herdr 2>/dev/null || printf %s "$HOME/.local/bin/herdr"); exec "$H" agent attach 'stub' --takeover --read-only`}},
		{cmd: []string{`H=$(command -v herdr 2>/dev/null || printf %s "$HOME/.local/bin/herdr"); exec "$H" agent attach 'stub' `}},
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
	// hostAllowed compares against the active host, so a backend must be
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

// The production dashboard helper resolves herdr on the remote host before
// execing it. SSH preserves that resolver as one remote-command argv element;
// recognizing only this bounded shape keeps arbitrary shell text from becoming
// attach authority.
func TestPaneSSHHopAgentAttachResolverCommand(t *testing.T) {
	swapBackend(t, &paneHostNamedBackend{name: "ticket500"})
	remoteCommand := `H=$(command -v herdr 2>/dev/null || printf %s "$HOME/.local/bin/herdr"); exec "$H" agent attach 'stub'`
	attach := paneProcessInfo{
		ForegroundProcessGroupID: 10,
		ForegroundProcesses: []paneProcess{
			{PID: 10, Name: "bash", Cwd: "/home/stephan", Argv: []string{"bash", "/home/stephan/.local/bin/agent-attach", "ticket500", "stub"}},
			{PID: 11, Name: "ssh", Cwd: "/home/stephan", Argv: []string{"ssh", "-t", "-o", "ConnectTimeout=10", "ticket500", remoteCommand}},
		},
	}

	hop, ok := paneSSHHop(attach)
	if !ok || hop.host != "ticket500" || hop.agent != "stub" {
		t.Fatalf("paneSSHHop(production resolver attach) = (%+v, %v), want host ticket500 agent stub", hop, ok)
	}
}

// A herdr-mirror streamer is a window onto the far host's pane, so the viewer
// must follow THAT pane rather than the streamer's own directory (which is
// herdr-mirror's parking spot and describes nothing).
func TestPaneSSHHopMirror(t *testing.T) {
	swapBackend(t, &localBackend{})

	// The live argv, from titan: `herdr-mirror pane <ssh-target> <ws>:<pane>`.
	mirror := paneProcessInfo{
		ForegroundProcessGroupID: 60,
		ForegroundProcesses: []paneProcess{
			{PID: 60, Name: "herdr-mirror", Cwd: "/home/u/.local/state/herdr-mirror/.mirror-pane", Argv: []string{
				"/home/u/.config/herdr/plugins/github/mirror-0015/target/release/herdr-mirror",
				"pane", "local", "w5:p1", "--always-control",
				"--ctl-path", "/home/u/.local/state/herdr-mirror/ocai.ctl",
			}},
		},
	}
	hop, ok := paneSSHHop(mirror)
	if !ok || hop.host != "local" || hop.agent != "w5:p1" {
		t.Fatalf("paneSSHHop(mirror) = (%+v, %v), want host local pane w5:p1", hop, ok)
	}

	// The daemon and the one-shot subcommands are not windows onto anything.
	for _, argv := range [][]string{
		{"herdr-mirror", "daemon"},
		{"herdr-mirror", "status"},
		{"herdr-mirror", "pane"},
		{"herdr-mirror", "pane", "local"},
		{"herdr-mirror", "pane", "--help", "local"},
		// A pane argument that isn't a <ws>:<pane> id.
		{"herdr-mirror", "pane", "local", "w5"},
	} {
		pi := paneProcessInfo{
			ForegroundProcessGroupID: 61,
			ForegroundProcesses:      []paneProcess{{PID: 61, Name: "herdr-mirror", Argv: argv}},
		}
		if hop, ok := paneSSHHop(pi); ok {
			t.Errorf("paneSSHHop(%q) = (%+v, true), want no hop", argv, hop)
		}
	}

	// A mirror of a host lasso may not drive keeps the local answer.
	foreign := paneProcessInfo{
		ForegroundProcessGroupID: 62,
		ForegroundProcesses: []paneProcess{
			{PID: 62, Name: "herdr-mirror", Argv: []string{"herdr-mirror", "pane", "not-a-configured-host", "w5:p1"}},
		},
	}
	if hop, ok := paneSSHHop(foreign); ok {
		t.Errorf("paneSSHHop(unknown mirror host) = (%+v, true), want no hop", hop)
	}
}

// A mirrored pane need not hold an agent — agent.list cannot answer for a
// mirrored shell — so the far side is resolved from the pane listing too.
func TestAttachedPaneByPaneID(t *testing.T) {
	be := &fakeHerdrBackend{res: map[string]string{
		"agent.list": `{"agents":[{"name":"other","pane_id":"w9:p1","terminal_id":"term_9"}]}`,
		"pane.list": `{"panes":[
			{"pane_id":"w5:p1","cwd":"/home/dev/projects/norm","focused":false},
			{"pane_id":"w6:p1","cwd":"/home/dev","focused":true}]}`,
	}}
	p, ok := attachedPane(be, "w5:p1")
	if !ok || paneCwd(p) != "/home/dev/projects/norm" {
		t.Fatalf("attachedPane(w5:p1) = (%+v, %v), want the mirrored pane", p, ok)
	}
	// Still no substituting: a pane that is gone resolves to nothing, not to
	// whichever pane the far side happens to have focused.
	if p, ok := attachedPane(be, "w7:p1"); ok {
		t.Errorf("attachedPane(missing pane) = (%+v, true), want no pane", p)
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
