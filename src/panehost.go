package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A pane is not always a window onto the machine it runs on. `ssh <host> herdr
// agent attach <name>` — what a fleet attach script runs — puts a remote agent's
// terminal inside a local pane, and `herdr --remote <host>` does the same for a
// whole remote session. Both leave the pane's own cwds pointing at the ssh
// client's directory (invariably ~, wherever the attach was launched), while
// everything the user is looking at lives on the far host. A file viewer
// following the pane then browses the wrong machine outright: /home/stephan on
// titan for an agent working in /home/dev/projects/norm on norm.
//
// herdr's pane.process_info carries each foreground process's argv, so the hop
// is recoverable from the pane alone: read the ssh destination, map it to an
// ssh-config alias lasso may drive (hostAliasFor), then ask THAT host's herdr
// where the attached pane is working (remoteAttachCwd). Only a herdr ATTACH
// counts. A plain `ssh host` shell has no far-side pane to ask about — nothing
// on either end knows where that shell has cd'd to — so those panes keep the
// local answer instead of a guess.

// sshHop is the far side of an attach: the alias lasso addresses that host by,
// plus the agent the attach targeted. An empty agent means a session-level
// attach, which shows whatever pane the far side has focused.
type sshHop struct {
	host  string
	agent string
}

// paneSSHHop reads a pane's foreground processes as an attach onto another
// host's herdr. ok is false for every ordinary pane — the common case, and it
// costs nothing beyond the process_info call activeCwd already makes.
func paneSSHHop(pi paneProcessInfo) (sshHop, bool) {
	for _, proc := range pi.ForegroundProcesses {
		var dest, agent string
		switch procBinary(proc) {
		case "herdr":
			// `herdr --remote <target>`: a local client driving a remote herdr
			// server over its own ssh forward. Session-level, so the far side's
			// focused pane is the one on screen.
			dest = argvFlagValue(proc.Argv, "--remote")
		case "ssh":
			d, cmd := sshDest(proc.Argv)
			a, ok := herdrAttachTarget(cmd)
			if !ok {
				continue
			}
			dest, agent = d, a
		default:
			continue
		}
		if dest == "" {
			continue
		}
		if host := hostAliasFor(dest); host != "" {
			return sshHop{host: host, agent: agent}, true
		}
	}
	return sshHop{}, false
}

// procBinary is the program a foreground process is running, by basename:
// herdr's `name` when it has one, else argv[0] (which may be an absolute path).
func procBinary(proc paneProcess) string {
	if proc.Name != "" {
		return filepath.Base(proc.Name)
	}
	if len(proc.Argv) > 0 {
		return filepath.Base(proc.Argv[0])
	}
	return ""
}

// sshOptFlags are the single-letter ssh options that take a value. They matter
// because the value is an operand-shaped argument: without this table
// `ssh -o ConnectTimeout=10 norm herdr …` reads as a connection to the host
// "ConnectTimeout=10". Anything not listed is treated as a boolean flag, which
// is how ssh itself would read an unknown letter's cluster.
const sshOptFlags = "BbcDEeFIiJLlmOopQRSWw"

// sshDest splits an ssh argv into its destination and the remote command that
// follows it. Both are empty when the argv names no destination (`ssh -V`).
func sshDest(argv []string) (dest string, cmd []string) {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			if i+1 < len(argv) {
				return argv[i+1], argv[i+2:]
			}
			return "", nil
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			return a, argv[i+1:]
		}
		// Walk the flag cluster: booleans bundle (`-tt`), and the first
		// value-taking letter consumes either the rest of the cluster
		// (`-oBatchMode=yes`) or the next argument.
		for j, c := range a[1:] {
			if strings.ContainsRune(sshOptFlags, c) {
				if j+1 == len(a)-1 {
					i++ // value is the next argument
				}
				break
			}
		}
	}
	return "", nil
}

// herdrValueFlags are the herdr flags whose value would otherwise be read as a
// subcommand (`herdr --session fleet` attaches, it does not run "fleet").
var herdrValueFlags = map[string]bool{
	"--session":            true,
	"--remote":             true,
	"--remote-keybindings": true,
	"--config":             true,
	"-s":                   true,
}

// herdrAttachTarget reads an ssh remote command as a herdr attach. It returns
// the agent `herdr agent attach <target>` names — a name, pane id or terminal
// id, all of which herdr accepts — and true for any attach; "" with true for a
// session-level attach (`herdr`, `herdr --session x`, `herdr session attach x`),
// whose far-side view is that session's focused pane. Everything else is false:
// a one-shot command (`herdr agent list`) or a login shell is not a window onto
// a herdr session.
func herdrAttachTarget(cmd []string) (string, bool) {
	// `ssh host "herdr agent attach x"` arrives as one argument, since the
	// remote command is a string the far shell parses.
	if len(cmd) == 1 {
		cmd = strings.Fields(cmd[0])
	}
	if len(cmd) == 0 || filepath.Base(cmd[0]) != "herdr" {
		return "", false
	}
	var words []string
	for i := 1; i < len(cmd); i++ {
		a := cmd[i]
		if strings.HasPrefix(a, "-") {
			if herdrValueFlags[a] {
				i++ // skip the flag's value, not a subcommand
			}
			continue
		}
		words = append(words, a)
	}
	switch {
	case len(words) == 0:
		return "", true
	case words[0] == "session" && len(words) >= 2 && words[1] == "attach":
		return "", true
	case words[0] == "agent" && len(words) >= 3 && words[1] == "attach":
		return words[2], true
	}
	return "", false
}

// argvFlagValue returns the value of a long flag written either as
// `--flag value` or `--flag=value`; "" when the flag is absent or has no value.
func argvFlagValue(argv []string, flag string) string {
	for i, a := range argv {
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v
		}
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// hostAliasFor maps an ssh destination as written on a command line to the alias
// lasso addresses that box by. The literal destination wins when it is a host we
// may drive; otherwise the probed hosts are matched on what ssh actually
// resolves to, so a bare hostname (or a second alias for the same account) still
// finds its row. "" when lasso has no driveable host for the destination — an
// unreachable box has no filesystem to show either, so the caller keeps the
// local answer.
func hostAliasFor(dest string) string {
	user, host := splitSSHDest(dest)
	if host == "" {
		return ""
	}
	if hostAllowed(host) {
		return host
	}
	rows, _ := hostSnapshot()
	for _, hi := range rows {
		if !hi.Reachable || !hi.Running || !hi.Compatible {
			continue
		}
		if hi.Hostname == host && (user == "" || hi.User == user) {
			return hi.Alias
		}
	}
	return ""
}

// splitSSHDest splits an ssh destination into its user and host parts, accepting
// both `user@host` and the URI form `ssh://user@host:port`. The port is only
// stripped for the URI form, because a bare `host:port` is not an ssh
// destination (it names a path in scp syntax) and must not be silently accepted.
func splitSSHDest(dest string) (user, host string) {
	host, uri := dest, false
	if v, ok := strings.CutPrefix(host, "ssh://"); ok {
		host, uri = v, true
	}
	if i := strings.LastIndex(host, "@"); i >= 0 {
		user, host = host[:i], host[i+1:]
	}
	if uri {
		if i := strings.LastIndex(host, ":"); i > strings.LastIndex(host, "]") {
			host = host[:i]
		}
	}
	if strings.ContainsAny(host, "/:") {
		return "", "" // not a plain destination — a path, or a port we won't guess at
	}
	return user, strings.Trim(host, "[]")
}

const (
	// How long a resolved far-side cwd is served before the remote herdr is
	// asked again. fetchActive runs on every herdr event, which bursts while an
	// agent streams output; each miss here is an ssh round-trip (plus, for a
	// claude agent, a transcript read over SFTP), so a burst must not pay per
	// event.
	remoteCwdTTL = 2 * time.Second
	// A failed resolve is remembered longer: learning it again costs a full dial
	// attempt against a host that is asleep or has no such agent.
	remoteCwdMissTTL = 20 * time.Second
)

type remoteCwdEntry struct {
	cwd    string
	source string
	at     time.Time
}

var remoteCwdCache = struct {
	sync.Mutex
	m map[string]*remoteCwdEntry
}{m: map[string]*remoteCwdEntry{}}

// remoteAttachCwd resolves the working directory behind an attach, as
// (cwd, source) on h.host. Empty cwd means the far side could not be read — the
// caller then keeps the pane's local answer rather than showing a path that
// doesn't exist on the host it would query.
func remoteAttachCwd(h sshHop) (cwd, source string) {
	key := h.host + "|" + h.agent
	remoteCwdCache.Lock()
	if e := remoteCwdCache.m[key]; e != nil {
		ttl := remoteCwdTTL
		if e.cwd == "" {
			ttl = remoteCwdMissTTL
		}
		if time.Since(e.at) < ttl {
			cwd, source = e.cwd, e.source
			remoteCwdCache.Unlock()
			return cwd, source
		}
	}
	remoteCwdCache.Unlock()

	// Resolved outside the lock: this is an ssh round-trip, and two refreshes
	// racing here merely do the work twice.
	cwd, source = resolveAttachCwd(h)
	remoteCwdCache.Lock()
	remoteCwdCache.m[key] = &remoteCwdEntry{cwd: cwd, source: source, at: time.Now()}
	remoteCwdCache.Unlock()
	return cwd, source
}

func resolveAttachCwd(h sshHop) (string, string) {
	be, err := hostBackend(h.host)
	if err != nil {
		return "", ""
	}
	p, ok := attachedPane(be, h.agent)
	if !ok {
		return "", ""
	}
	// Same precedence as a local pane (see activeCwd): the harness's own cwd
	// beats the pane's, because a claude agent that cd'd inside its Bash tool
	// leaves both of herdr's cwds on the launch directory.
	if c := harnessCwd(be, p, ""); c != "" {
		return c, "harness"
	}
	if paneCwdUsesForeground(p) {
		return paneCwd(p), "foreground"
	}
	return paneCwd(p), "shell"
}

// attachedPane finds the pane an attach lands on: the named agent's pane for
// `agent attach <target>`, or the far side's focused pane for a session attach.
// A named agent that is no longer there resolves to nothing rather than to some
// other pane — following the wrong tree is worse than falling back.
func attachedPane(be Backend, agent string) (pane, bool) {
	if agent == "" {
		res, err := be.HerdrCall("pane.list", map[string]any{})
		if err != nil {
			return pane{}, false
		}
		var pl struct {
			Panes []pane `json:"panes"`
		}
		if json.Unmarshal(res, &pl) != nil {
			return pane{}, false
		}
		for _, p := range pl.Panes {
			if p.Focused {
				return p, true
			}
		}
		return pane{}, false
	}
	res, err := be.HerdrCall("agent.list", map[string]any{})
	if err != nil {
		return pane{}, false
	}
	var al struct {
		Agents []remoteAgent `json:"agents"`
	}
	if json.Unmarshal(res, &al) != nil {
		return pane{}, false
	}
	for _, a := range al.Agents {
		if a.Name == agent || a.PaneID == agent || a.TerminalID == agent {
			return a.pane, true
		}
	}
	return pane{}, false
}

// remoteAgent is one row of herdr's agent.list: a pane plus the agent name
// `herdr agent attach <name>` addresses it by.
type remoteAgent struct {
	pane
	Name string `json:"name"`
}
