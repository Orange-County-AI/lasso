package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Host probe states, carried in HostInfo.State. The zero value (empty string)
// means the probe COMPLETED and the Reachable/Running/Compatible booleans are
// authoritative — so old clients that don't know the field see exactly what
// they saw before. A non-empty state marks a row whose booleans are *not* a
// verdict, only a default:
//
//	hostProbing — a probe is in flight and has never completed for this host.
//	hostTimedOut — the probe hit its deadline; reachability is unknown.
//
// Keeping "we don't know yet" distinct from "we asked and it's down" is the
// point: a sleeping laptop and a laptop that merely answers slowly used to be
// indistinguishable in this payload, and a slow host got libelled as broken.
const (
	hostProbing  = "probing"
	hostTimedOut = "timeout"
)

// HostInfo describes one ssh-config host as a candidate Luvus target. A host is
// usable (selectable in the footer switcher) when Reachable && Running &&
// Compatible; otherwise the UI greys it out and shows Err (or, for a non-empty
// State, a pending/unknown affordance rather than a failure).
//
// Compatible is decided by CAPABILITY VALIDATION, not by an integer comparison:
// the host's own `uhp.capabilities` has to name the UHP protocol lasso speaks
// and offer every method lasso's features call (validateLuvusCaps). A remote
// Luvus that merely reports a number lasso recognizes is not enough — a build
// missing `pane.get` breaks the pane list whatever its version says.
type HostInfo struct {
	Alias      string `json:"alias"`
	Hostname   string `json:"hostname"`   // effective ssh HostName (for grouping aliases on one box)
	User       string `json:"user"`       // effective ssh User (distinguishes users on one host)
	Reachable  bool   `json:"reachable"`  // ssh connected and ran the probe
	Running    bool   `json:"running"`    // a Luvus server session is up on the host
	Version    string `json:"version"`    // remote Luvus version
	Protocol   string `json:"protocol"`   // remote UHP protocol identity, e.g. "luvus-uhp/1"
	Socket     string `json:"socket"`     // remote UHP endpoint address (absolute unix socket path)
	Compatible bool   `json:"compatible"` // the remote's capabilities satisfy validateLuvusCaps
	Err        string `json:"err,omitempty"`
	// State is "" once a probe has completed (booleans authoritative), else
	// hostProbing / hostTimedOut. CheckedAt is when the last probe COMPLETED
	// (RFC3339), so a caller can judge staleness itself; it is absent for a host
	// that has never finished one.
	State     string `json:"state,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

// hostsPayload is the body served at GET /api/hosts. Probing reports whether any
// host in Hosts is still being probed — the signal for a client to poll again
// shortly rather than treat this snapshot as final.
type hostsPayload struct {
	Active string `json:"active"` // currently driven host ("local" or an alias)
	Local  struct {
		Version  string `json:"version"`
		Protocol string `json:"protocol"` // UHP protocol identity label, same form as HostInfo.Protocol
		Hostname string `json:"hostname"` // machine hostname, shown in place of "local"
		User     string `json:"user"`     // the user lasso runs as (labels the local row when a host groups >1 user)
	} `json:"local"`
	Hosts   []HostInfo `json:"hosts"`
	Probing bool       `json:"probing"`
}

// localHostname is the short machine hostname (first label) used as the display
// label for the local host, falling back to "local" if it can't be resolved.
func localHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "local"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i] // strip any domain suffix for a compact label
	}
	return h
}

// localUsername returns the name of the user lasso runs as, used to label the
// local row when a physical host groups more than one user (e.g. the local
// session alongside a loopback ssh alias for another account). Falls back to
// the $USER env var, then "local", if the OS lookup fails.
func localUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "local"
}

// resolveSSHTarget returns the effective HostName and User ssh would use for an
// alias, via `ssh -G` (which expands the full config — Host/Match blocks,
// defaults, includes — without connecting). The frontend groups aliases whose
// HostName is the same physical box (and folds loopback aliases under the local
// host), so two accounts on one machine cluster together. Best-effort: on any
// failure the fields stay empty and the frontend falls back to the alias.
func resolveSSHTarget(alias string) (hostname, username string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-G", alias).Output()
	if err != nil {
		return "", ""
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "hostname "); ok {
			hostname = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "user "); ok {
			username = strings.TrimSpace(v)
		}
	}
	return hostname, username
}

// ---------------------------------------------------------------------------
// local UHP identity + endpoint discovery
// ---------------------------------------------------------------------------

// luvusUHPName is the protocol identity a Luvus server reports in
// uhp.capabilities. Anything else on the far end of a socket is not a Luvus we
// can drive, however well-formed its frames are.
const luvusUHPName = "luvus-uhp"

// uhpLabel renders a protocol identity for display: "luvus-uhp/1". The major is
// the whole compatibility question (UHP 1 is stable and additive within its
// major, and capability validation covers what a minor would tell us), so the
// minor is deliberately left out rather than implying it matters.
func uhpLabel(name string, major int) string {
	if name == "" || major <= 0 {
		return ""
	}
	return name + "/" + strconv.Itoa(major)
}

var localProto struct {
	once     sync.Once
	version  string
	protocol int
}

// localProtocol returns this machine's Luvus version and UHP protocol major,
// validating the local socket's capabilities once and caching the result (see
// luvusPing, which refuses a server whose capabilities lasso can't use).
func localProtocol() (string, int) {
	localProto.once.Do(func() {
		if v, p, err := luvusPing(*luvusSock); err == nil {
			localProto.version, localProto.protocol = v, p
		}
	})
	return localProto.version, localProto.protocol
}

// luvusSession is one entry of session.list / session.status: a Luvus server
// session and the endpoint it answers UHP on. Endpoint.Address is the
// platform-native address to actually dial, which is NOT always SocketPath:
// Luvus maps an over-long LUVUS_HOME onto a short socket alias in the sticky
// temp root, and only the endpoint reports that mapping. Prefer it and treat
// SocketPath as the fallback, so nothing has to reproduce Luvus's own rule.
type luvusSession struct {
	Name     string `json:"name"`
	Default  bool   `json:"default"`
	Running  bool   `json:"running"`
	Endpoint struct {
		Transport string `json:"transport"`
		Address   string `json:"address"`
	} `json:"endpoint"`
	SocketPath string `json:"socket_path"`
	SessionDir string `json:"session_dir"`
}

// unixAddr returns the session's dialable unix socket path, or "" when its
// endpoint is not a unix socket (a Windows named pipe, which lasso can neither
// dial nor forward over ssh).
func (s luvusSession) unixAddr() string {
	if s.Endpoint.Transport != "" && s.Endpoint.Transport != "unix_socket" {
		return ""
	}
	if s.Endpoint.Address != "" {
		return s.Endpoint.Address
	}
	return s.SocketPath
}

// luvusSessionList is the session.list result.
type luvusSessionList struct {
	Sessions []luvusSession `json:"sessions"`
}

// pick returns the session lasso drives: the one $LUVUS_SESSION names when it
// names one, else the default session. A named session that isn't there is NOT
// silently replaced by the default — a caller asking for "review" and being
// handed "default" would drive the wrong machine's worth of panes.
func (l luvusSessionList) pick(want string) (luvusSession, bool) {
	if want != "" {
		for _, s := range l.Sessions {
			if s.Name == want {
				return s, true
			}
		}
		return luvusSession{}, false
	}
	for _, s := range l.Sessions {
		if s.Default {
			return s, true
		}
	}
	if len(l.Sessions) == 1 {
		return l.Sessions[0], true
	}
	return luvusSession{}, false
}

// uhpEnvelope is one reply frame of the UHP wire format (newline-delimited JSON,
// one reply per request), as it arrives from a socket or from `luvus uhp proxy`.
type uhpEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// resultType reads the discriminator every UHP result carries ("session_list",
// "host_info", "uhp_capabilities", …). It is what lets a batch of replies be
// matched to the calls that produced them without depending on their order.
func (e uhpEnvelope) resultType() string {
	if len(e.Result) == 0 {
		return ""
	}
	var t struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(e.Result, &t) != nil {
		return ""
	}
	return t.Type
}

// luvusCLI is the luvus binary the host-profile calls run: the exact one this
// process was launched beside when Luvus exported it, else whatever is on PATH.
// Deliberately not derived from -term-cmd: defaultSock() resolves before
// flag.Parse, so a flag read here would see its unparsed default.
func luvusCLI() string {
	if p := strings.TrimSpace(os.Getenv("LUVUS_BIN_PATH")); p != "" {
		return p
	}
	return "luvus"
}

// luvusProxyTimeout bounds a host-profile call. These run in one short-lived
// `luvus uhp proxy` process against local state (no daemon, no network), so a
// second is already generous; the bound exists so a wedged binary can't stall
// startup, which is where discoverLocalLuvusSocket runs.
const luvusProxyTimeout = 2 * time.Second

// luvusProxyCall runs one UHP request through `luvus uhp proxy` and returns its
// result. The proxy is the ONLY route to the host profile — host.info,
// host.doctor, session.*, integration.*, skill.*, host.update.* are answered
// inside that short-lived process and are deliberately not served by a session
// socket, so they work with no server running at all.
func luvusProxyCall(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"id": "lasso", "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, 4)
	if s := strings.TrimSpace(os.Getenv("LUVUS_SESSION")); s != "" {
		args = append(args, "--session", s)
	}
	args = append(args, "uhp", "proxy")
	cmd := exec.CommandContext(ctx, luvusCLI(), args...)
	cmd.Stdin = bytes.NewReader(append(body, '\n'))
	cmd.WaitDelay = probeWaitDelay
	out, err := cmd.Output()
	if err != nil && len(bytes.TrimSpace(out)) == 0 {
		return nil, err
	}
	line := out
	if i := bytes.IndexByte(out, '\n'); i >= 0 {
		line = out[:i]
	}
	var env uhpEnvelope
	if jerr := json.Unmarshal(line, &env); jerr != nil {
		return nil, fmt.Errorf("luvus %s: unreadable reply", method)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("luvus %s: %s", method, env.Error.Message)
	}
	return env.Result, nil
}

// discoverLocalLuvusSocket asks the local Luvus where its session answers,
// rather than rebuilding the path from a home dir. It is the fallback
// defaultSock() uses when no LUVUS_* env var pins the socket (a lasso started
// outside a pane), and it is what makes an over-long LUVUS_HOME work: the short
// socket alias Luvus binds in that case exists only in its own endpoint answer.
// Empty when luvus is absent, errors, names a session that isn't registered, or
// answers with a transport lasso cannot dial.
func discoverLocalLuvusSocket() string {
	ctx, cancel := context.WithTimeout(context.Background(), luvusProxyTimeout)
	defer cancel()
	res, err := luvusProxyCall(ctx, "session.list", nil)
	if err != nil {
		return ""
	}
	var list luvusSessionList
	if json.Unmarshal(res, &list) != nil {
		return ""
	}
	s, ok := list.pick(strings.TrimSpace(os.Getenv("LUVUS_SESSION")))
	if !ok {
		return ""
	}
	return s.unixAddr()
}

// ---------------------------------------------------------------------------
// ssh config parsing
// ---------------------------------------------------------------------------

// sshConfigHosts returns the concrete host aliases declared in ~/.ssh/config,
// skipping wildcard/negated patterns (*, ?, !) which aren't real targets.
// Include directives are not followed (v1).
func sshConfigHosts() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	f, err := os.Open(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return nil
	}
	defer f.Close()

	var hosts []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Keyword may be separated from values by spaces, tabs, or '='.
		fields := strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' || r == '=' })
		if len(fields) < 2 || !strings.EqualFold(fields[0], "Host") {
			continue
		}
		for _, tok := range fields[1:] {
			if strings.ContainsAny(tok, "*?!") {
				continue // wildcard / negation, not a concrete host
			}
			if !seen[tok] {
				seen[tok] = true
				hosts = append(hosts, tok)
			}
		}
	}
	return hosts
}

// ---------------------------------------------------------------------------
// probing
// ---------------------------------------------------------------------------

// remoteLuvusShell wraps a luvus command line so it runs in the remote user's
// login shell with the usual user-local install dirs (~/.local/bin and mise's
// shim dir) forced onto PATH first. `ssh host <cmd>` uses a non-login shell whose
// PATH omits those dirs, and even `$SHELL -lc` only finds luvus if the login
// profile happens to add them — a freshly provisioned host (luvus just dropped in
// ~/.local/bin, profile not yet wired) would otherwise still report "command not
// found" and keep showing "set up". Prefixing PATH ourselves — matching what the
// provision script does — makes detection and update robust regardless of how the
// host's profile is set up. $HOME/$PATH are left for the remote login shell to
// expand (the single-quoted body is opaque to the outer shell), which is also why
// luvusCmd MUST NOT contain a single quote.
func remoteLuvusShell(luvusCmd string) string {
	return `${SHELL:-sh} -lc 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"; ` + luvusCmd + `'`
}

// Probe budget. hostProbeTimeout bounds one host's whole probe and
// probeConnectTimeout is handed to ssh as ConnectTimeout (which bounds the TCP
// connect AND the banner exchange, so a host that accepts but never speaks
// still fails inside it).
//
// Both are far more generous than the 8s/4s they replace, because probing no
// longer sits on the request path: /api/hosts answers from the store within
// hostProbeGrace and probes land afterwards (see discoverHosts). A cold connect
// to a sleeping tailscale Mac genuinely costs 10-20s+ — under the old budget
// such a host was killed mid-handshake and reported as broken every single
// time, which is exactly the "healthy machine silently dropped" symptom. The
// only thing a tight budget bought was a faster wrong answer.
var (
	hostProbeTimeout = 30 * time.Second
	// Seams for tests, mirroring closeme.go's peerHostsFn: they let the sweep be
	// driven with synthetic hosts and synthetic probe latency, so the timing
	// behaviour can be asserted without real ssh.
	probeHostFn      = probeHost
	sshConfigHostsFn = sshConfigHosts
)

const (
	probeConnectTimeout = "10"
	// probeWaitDelay bounds how long Output() may keep waiting AFTER the context
	// kills ssh. Without it, Wait() blocks until every process holding the
	// inherited stdout pipe exits — and ssh forks a ControlMaster that outlives
	// the client (ControlPersist), so a probe could hang far past its deadline
	// regardless of the context. Measured: a killed command with a backgrounded
	// child returned only when the CHILD exited, 60s past a 2s deadline.
	probeWaitDelay = 2 * time.Second
)

// remoteUHPCall is one request in a batch sent to a remote host's Luvus.
type remoteUHPCall struct {
	Method string
	Params map[string]any
}

// mustRemoteUHPCmd renders a remote shell command that pushes each call through
// its own `luvus uhp proxy`, writing one reply frame per call to stdout in
// order. The proxy is a one-request-per-process stdio profile, hence one
// pipeline each rather than a stream; they all ride a single ssh round trip.
//
// The JSON is embedded with escaped double quotes because remoteLuvusShell
// single-quotes the whole body. It panics on a payload carrying a single quote
// (which would break out of that quoting) — the call lists are static literals
// in this file, so it can only fire in development, like regexp.MustCompile.
func mustRemoteUHPCmd(calls ...remoteUHPCall) string {
	var b strings.Builder
	for _, c := range calls {
		params := c.Params
		if params == nil {
			params = map[string]any{}
		}
		payload, err := json.Marshal(map[string]any{"id": "lasso", "method": c.Method, "params": params})
		if err != nil {
			panic("remoteUHPCmd: " + c.Method + ": " + err.Error())
		}
		if bytes.ContainsAny(payload, "'$\\") {
			panic("remoteUHPCmd: " + c.Method + ": payload is not shell-embeddable: " + string(payload))
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(`printf "%s\n" "` + strings.ReplaceAll(string(payload), `"`, `\"`) + `" | luvus uhp proxy`)
	}
	return b.String()
}

// luvusProbeCmd is the remote command the probe runs: the three UHP calls that
// answer "can lasso drive this host".
//
// host.info and session.list belong to the HOST PROFILE, which the proxy answers
// out of local state with no server running — so a host with Luvus installed but
// stopped is reported as exactly that (and can be provisioned) instead of looking
// like a host with no Luvus. uhp.capabilities is forwarded to the session and is
// the only one that needs a live server; when it fails the other two have
// already said why.
//
// The replies are matched by their `type` discriminator rather than by position,
// so a failing call in the middle cannot shift the meaning of the others.
var luvusProbeCmd = mustRemoteUHPCmd(
	remoteUHPCall{Method: "host.info"},
	remoteUHPCall{Method: "session.list"},
	remoteUHPCall{Method: "uhp.capabilities"},
)

// probeHost asks a host whether it runs a Luvus server whose UHP capabilities
// lasso can actually use. BatchMode makes hosts that would prompt (password /
// unknown key) fail fast rather than hang.
func probeHost(ctx context.Context, alias string) HostInfo {
	hi := HostInfo{Alias: alias}
	cctx, cancel := context.WithTimeout(ctx, hostProbeTimeout)
	defer cancel()
	// ClearAllForwardings drops any LocalForward/RemoteForward the user's config
	// attaches to this host (e.g. a tunnel that conflicts with a busy port) — the
	// probe only needs to run one command, no forwarding. remoteLuvusShell runs
	// luvus in a login shell with the user-local install dirs forced onto PATH.
	cmd := exec.CommandContext(cctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout="+probeConnectTimeout,
		"-o", "StrictHostKeyChecking=accept-new",
		alias, remoteLuvusShell(luvusProbeCmd))
	cmd.WaitDelay = probeWaitDelay
	out, err := cmd.Output()
	if err != nil {
		ee, isExit := err.(*exec.ExitError)
		stderr := ""
		if isExit {
			stderr = firstLine(strings.TrimSpace(string(ee.Stderr)))
		}
		// We ran out of budget rather than getting an answer. This MUST be checked
		// before the exit-code branches below: a context kill surfaces as an
		// ExitError whose ExitCode() is -1 (killed by signal), which is neither
		// "not an ExitError" nor 255 — so it used to fall through to "reachable,
		// luvus not installed" and the UI offered to *install luvus* on a box we
		// never reached. Report the honest thing instead: unknown, timed out.
		if cctx.Err() != nil {
			hi.State = hostTimedOut
			hi.Err = "timed out probing (no answer in " + hostProbeTimeout.String() + ")"
			return hi
		}
		// ssh itself failed to connect (exit 255), or the process couldn't run at
		// all → unreachable. Any other exit code means the remote ran the command
		// (e.g. exit 127 "luvus: command not found") → reachable but no luvus.
		if !isExit || ee.ExitCode() == 255 {
			hi.Err = "unreachable"
			if stderr != "" {
				hi.Err = stderr
			}
			return hi
		}
		hi.Reachable = true
		if len(bytes.TrimSpace(out)) == 0 {
			hi.Err = "luvus not installed"
			if stderr != "" {
				hi.Err = stderr
			}
			return hi
		}
		// Non-zero exit but replies on stdout — the normal shape for a host whose
		// server is stopped, since only the last call (uhp.capabilities) fails.
	}
	hi.Reachable = true
	applyProbeReplies(&hi, out)
	return hi
}

// applyProbeReplies fills hi from the probe's reply frames. Split out so the
// classification — which reply means what, and which failure wins the Err line
// — is testable without an ssh server.
func applyProbeReplies(hi *HostInfo, out []byte) {
	var (
		info struct {
			Version string `json:"version"`
		}
		sessions  luvusSessionList
		caps      uhpCaps
		haveInfo  bool
		haveSess  bool
		haveCaps  bool
		firstFail string
	)
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var env uhpEnvelope
		if json.Unmarshal(line, &env) != nil {
			continue // a login-shell banner or other noise, not a reply frame
		}
		if env.Error != nil {
			if firstFail == "" {
				firstFail = env.Error.Message
			}
			continue
		}
		switch env.resultType() {
		case "host_info":
			haveInfo = json.Unmarshal(env.Result, &info) == nil
		case "session_list":
			haveSess = json.Unmarshal(env.Result, &sessions) == nil
		case "uhp_capabilities":
			haveCaps = json.Unmarshal(env.Result, &caps) == nil
		}
	}

	if !haveInfo && !haveSess {
		// Nothing readable came back from a host we DID reach and run a command
		// on: luvus is missing, or too old to speak the host profile at all.
		hi.Err = "luvus not installed"
		if firstFail != "" {
			hi.Err = firstFail
		}
		return
	}
	hi.Version = info.Version

	// A remote is always driven through ITS default session: $LUVUS_SESSION names
	// a session on THIS machine, and carrying that name across the fleet would
	// point at whatever a namesake session happens to hold over there — or at
	// nothing at all on every host that doesn't run one.
	s, ok := sessions.pick("")
	if !ok {
		hi.Err = "no luvus session on this host"
		return
	}
	hi.Running = s.Running
	hi.Socket = s.unixAddr()
	if !s.Running {
		hi.Err = "luvus server not running"
		return
	}
	if hi.Socket == "" {
		// A running server whose endpoint lasso cannot forward over ssh (a
		// Windows named pipe). Reachable and running, but not drivable.
		hi.Err = "luvus endpoint " + s.Endpoint.Transport + " cannot be forwarded over ssh"
		return
	}
	if !haveCaps {
		hi.Err = "luvus did not answer uhp.capabilities"
		if firstFail != "" {
			hi.Err = firstFail
		}
		return
	}
	hi.Protocol = uhpLabel(caps.Protocol.Name, caps.Protocol.Major)
	// The same validation the socket handshake applies (validateLuvusCaps), so a
	// host cannot pass discovery and then be refused at connect — the state that
	// used to leave a host listed as usable and every call to it failing.
	if err := validateLuvusCaps(caps); err != nil {
		hi.Err = err.Error()
		return
	}
	hi.Compatible = true
}

// firstLine trims a multi-line ssh error to its first line for a compact tooltip.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// ---------------------------------------------------------------------------
// discovery cache
// ---------------------------------------------------------------------------

// The store holds one entry per ssh-config alias, updated INDEPENDENTLY as each
// probe lands. That independence is the whole design: the old cache was a
// single []HostInfo published only after wg.Wait(), so the switcher showed
// nothing until the slowest host resolved — nine healthy hosts sat probed and
// undelivered behind one sleeping laptop. Now a slow host degrades its own row
// and nothing else.
//
// Reads (discoverHosts) never wait for a sweep to finish; they wait at most
// hostProbeGrace for whatever is in flight and then serve the snapshot, rows
// still outstanding marked hostProbing. Correctness catches up in the
// background: results keep landing after the response is written, and the
// refresher (startHostRefresher) keeps the store warm so the common read is a
// map lookup.
var hostStore struct {
	mu      sync.Mutex
	entries map[string]HostInfo // alias -> latest known row
	order   []string            // aliases in ssh-config order (authoritative membership)
	sweep   chan struct{}       // non-nil while a sweep runs; closed when it ends
	at      time.Time           // when the last sweep COMPLETED
}

const (
	// hostProbeGrace bounds how long a plain read blocks on an in-flight sweep
	// before serving what it has. Warm hosts answer in ~0.1-0.9s, so a 2s grace
	// usually returns a complete list anyway; a slow host just isn't allowed to
	// hold the response hostage.
	hostProbeGrace = 2 * time.Second
	// hostForceGrace is the same bound for an explicit re-probe (the footer's
	// refresh button, list_hosts refresh:true). Longer, because the caller asked
	// for fresh data and an MCP client has no polling UI to catch up with — but
	// still bounded, and still returns partial results rather than nothing.
	hostForceGrace = 10 * time.Second
	// hostStaleAfter is when a plain read kicks a new sweep. Comfortably longer
	// than a cold sweep costs, so the cache can actually be refilled before it
	// expires — the old 30s TTL expired faster than a 16s+ sweep could refill it,
	// so every switcher open a minute apart paid full freight. In practice the
	// background refresher re-probes well before this.
	hostStaleAfter = 2 * time.Minute
	// hostRefreshInterval is the background refresher's period.
	hostRefreshInterval = 45 * time.Second
	// hostProbeConcurrency bounds concurrent ssh probes. Above the fleet size we
	// see in practice, so a typical sweep is one wave rather than two — under the
	// old semaphore of 8, an 11-host config needed two waves and the second wave
	// could not even start until the slowest host of the first finished.
	hostProbeConcurrency = 16
)

// hostSnapshot returns the current rows in display order, plus whether any is
// still probing. Never blocks on the network.
func hostSnapshot() ([]HostInfo, bool) {
	hostStore.mu.Lock()
	defer hostStore.mu.Unlock()
	out := make([]HostInfo, 0, len(hostStore.order))
	probing := false
	for _, alias := range hostStore.order {
		hi, ok := hostStore.entries[alias]
		if !ok {
			continue
		}
		if hi.State == hostProbing {
			probing = true
		}
		out = append(out, hi)
	}
	// Stable order: usable hosts first, then by alias. Hosts still probing sort
	// with the not-yet-usable ones rather than jumping to the top and then
	// dropping down when their result lands.
	sort.SliceStable(out, func(i, j int) bool {
		ui := out[i].Reachable && out[i].Running && out[i].Compatible
		uj := out[j].Reachable && out[j].Running && out[j].Compatible
		if ui != uj {
			return ui
		}
		return out[i].Alias < out[j].Alias
	})
	return out, probing
}

// anyHostSettled reports whether at least one host has ever completed a probe,
// i.e. whether the store holds anything worth serving without waiting.
func anyHostSettled() bool {
	hostStore.mu.Lock()
	defer hostStore.mu.Unlock()
	for _, hi := range hostStore.entries {
		if hi.State == "" {
			return true
		}
	}
	return false
}

// putHost publishes one host's row, making it visible to every subsequent read
// immediately — this is what "hosts appear as they resolve" comes down to — and
// hands the fresh row to the theme converger, which is how a machine that missed
// a theme change catches up (see convergeThemeOnProbe).
func putHost(hi HostInfo) {
	hostStore.mu.Lock()
	if hostStore.entries == nil {
		hostStore.entries = map[string]HostInfo{}
	}
	hostStore.entries[hi.Alias] = hi
	hostStore.mu.Unlock()
	convergeThemeOnProbe(hi)
}

// beginSweep claims the right to run a sweep. It returns the channel that
// closes when the sweep in flight ends, and whether the caller owns it. Only
// one sweep runs at a time, so a burst of readers (mount + menu open + MCP
// call) shares one round of probes instead of forking a herd of ssh processes.
func beginSweep(force bool) (done chan struct{}, mine bool) {
	hostStore.mu.Lock()
	defer hostStore.mu.Unlock()
	if hostStore.sweep != nil {
		return hostStore.sweep, false // one already running — wait on it
	}
	fresh := !hostStore.at.IsZero() && time.Since(hostStore.at) < hostStaleAfter
	if fresh && !force {
		return nil, false // cache is good; no sweep needed
	}
	hostStore.sweep = make(chan struct{})
	return hostStore.sweep, true
}

// endSweep marks the sweep complete and wakes everyone waiting on it.
func endSweep(done chan struct{}) {
	hostStore.mu.Lock()
	hostStore.at = time.Now()
	hostStore.sweep = nil
	hostStore.mu.Unlock()
	close(done)
}

// runSweep re-reads the ssh config and probes every alias, publishing each row
// as it resolves. It returns only when every probe has landed — callers that
// must not block use waitFor/discoverHosts instead of calling this directly.
func runSweep(ctx context.Context, done chan struct{}) {
	defer endSweep(done)

	// No local-protocol lookup: a remote's usability is decided by ITS OWN
	// capabilities against lasso's contract, so a stopped local Luvus no longer
	// libels every host in the fleet as incompatible.
	aliases := sshConfigHostsFn()

	// Membership comes from the config, so an alias deleted from ~/.ssh/config
	// stops being reported. Rows already known are kept (and refreshed in place)
	// so a re-sweep never blanks the list it is refreshing.
	hostStore.mu.Lock()
	if hostStore.entries == nil {
		hostStore.entries = map[string]HostInfo{}
	}
	next := make(map[string]HostInfo, len(aliases))
	for _, alias := range aliases {
		if prev, ok := hostStore.entries[alias]; ok {
			next[alias] = prev
		} else {
			next[alias] = HostInfo{Alias: alias, State: hostProbing}
		}
	}
	hostStore.entries = next
	hostStore.order = aliases
	hostStore.mu.Unlock()

	// Phase 1 — resolve each alias's effective HostName/User. `ssh -G` only
	// expands config (no connection, no DNS), so this is cheap, and doing it up
	// front means a row that is still probing already groups under the right
	// physical box instead of appearing standalone and then jumping.
	var rwg sync.WaitGroup
	rsem := make(chan struct{}, hostProbeConcurrency)
	for _, alias := range aliases {
		rwg.Add(1)
		go func(alias string) {
			defer rwg.Done()
			rsem <- struct{}{}
			defer func() { <-rsem }()
			host, user := resolveSSHTarget(alias)
			hostStore.mu.Lock()
			if hi, ok := hostStore.entries[alias]; ok {
				hi.Hostname, hi.User = host, user
				hostStore.entries[alias] = hi
			}
			hostStore.mu.Unlock()
		}(alias)
	}
	rwg.Wait()

	// Phase 2 — probe. Each result is published the moment it lands.
	var wg sync.WaitGroup
	sem := make(chan struct{}, hostProbeConcurrency)
	for _, alias := range aliases {
		wg.Add(1)
		go func(alias string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			hi := probeHostFn(ctx, alias)
			hostStore.mu.Lock()
			prev := hostStore.entries[alias]
			hostStore.mu.Unlock()
			// Carry the resolved target forward; the probe doesn't know it.
			hi.Hostname, hi.User = prev.Hostname, prev.User
			hi.CheckedAt = time.Now().Format(time.RFC3339)
			putHost(hi)
		}(alias)
	}
	wg.Wait()
}

// discoverHosts returns what is known about every ssh-config host, kicking a
// background sweep when the data is stale (or when force is set) and waiting at
// most a grace period for it. It never blocks until the slowest host answers:
// rows whose probe is still outstanding come back marked hostProbing, and the
// caller can read again in a moment for the rest.
func discoverHosts(ctx context.Context, force bool) []HostInfo {
	hosts, _ := discoverHostsState(ctx, force)
	return hosts
}

// discoverHostsState is discoverHosts plus whether any row is still probing.
func discoverHostsState(ctx context.Context, force bool) (hosts []HostInfo, probing bool) {
	done, mine := beginSweep(force)
	if mine {
		// Run the sweep detached from this request: it must keep going (and keep
		// publishing rows) after we answer, and it must not be cancelled when the
		// client that happened to trigger it disconnects.
		go runSweep(sweepCtx(), done)
	}
	// Only ever block when waiting could change the answer: on an explicit
	// refresh, or when nothing has completed a probe yet and returning now would
	// mean returning an all-"probing" list. Once the store holds real results a
	// read is a map lookup — so the background refresher's sweeps never tax the
	// callers that poll discovery (the pane aggregation, the repo warmer) with a
	// grace period for data they already have.
	if done != nil && (force || !anyHostSettled()) {
		grace := hostProbeGrace
		if force {
			grace = hostForceGrace
		}
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-done: // whole sweep finished inside the grace — full answer
		case <-t.C: // out of grace — serve what has landed so far
		case <-ctx.Done(): // caller gave up
		}
	}
	return hostSnapshot()
}

// sweepCtx is the context sweeps run under: the server's lifetime, so a sweep
// outlives the request that triggered it. Falls back to Background in tests and
// CLI paths where the server context was never set.
func sweepCtx() context.Context {
	if srvCtx != nil {
		return srvCtx
	}
	return context.Background()
}

// startHostRefresher launches (once) the background re-probe loop, so the
// switcher reads a warm store instead of paying for a sweep on open. It runs a
// sweep immediately at startup, which is what makes the first switcher open of
// a fresh lasso fast.
var hostRefresherOnce sync.Once

func startHostRefresher() {
	hostRefresherOnce.Do(func() {
		go func() {
			t := time.NewTicker(hostRefreshInterval)
			defer t.Stop()
			for {
				if done, mine := beginSweep(true); mine {
					runSweep(sweepCtx(), done)
				}
				select {
				case <-sweepCtx().Done():
					return
				case <-t.C:
				}
			}
		}()
	})
}

// findHost returns the cached HostInfo for alias, if present.
func findHost(alias string) (HostInfo, bool) {
	hostStore.mu.Lock()
	defer hostStore.mu.Unlock()
	hi, ok := hostStore.entries[alias]
	return hi, ok
}

// ---------------------------------------------------------------------------
// GET /api/hosts
// ---------------------------------------------------------------------------

func serveHosts(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("refresh") == "1"
	hosts, probing := discoverHostsState(r.Context(), force)
	ver, proto := localProtocol()

	var p hostsPayload
	// "Active" is this tab's host — the switcher highlights what THIS tab is on,
	// not a process-wide selection, which no longer exists.
	p.Active = requestHost(r)
	if p.Active == "" {
		p.Active = defaultBackend().Name()
	}
	p.Local.Version = ver
	p.Local.Protocol = uhpLabel(luvusUHPName, proto)
	p.Local.Hostname = localHostname()
	p.Local.User = localUsername()
	p.Hosts = hosts
	p.Probing = probing
	writeJSON(w, p)
}

// invalidateHostCache forces the next discoverHosts to re-probe (used after an
// action that changes a host's Luvus — e.g. a remote update).
func invalidateHostCache() {
	hostStore.mu.Lock()
	hostStore.at = time.Time{}
	hostStore.mu.Unlock()
}

// ---------------------------------------------------------------------------
// POST /api/host-update — update Luvus on a remote host
// ---------------------------------------------------------------------------

// hostUpdateTimeout bounds the whole remote update (manifest fetch + binary
// download + install), generous because it pulls a release binary over the far
// host's network.
const hostUpdateTimeout = 3 * time.Minute

// luvusUpdateCheckCmd asks a host whether a newer Luvus release exists.
var luvusUpdateCheckCmd = mustRemoteUHPCmd(remoteUHPCall{Method: "host.update.check"})

// luvusUpdateInstallCmd installs the release and restarts the host's default
// session onto it. Both need confirm:true — Luvus refuses an unconfirmed
// installation or lifecycle change by design, which is also what makes this
// safe to drive without a terminal.
//
// The restart is not optional: the running server keeps serving the OLD
// binary's capability set, which is precisely what made the host unusable, so
// installing without restarting would report success and change nothing lasso
// can see. It stops that host's panes' processes — the caller (the switcher's
// update button) says so before asking.
var luvusUpdateInstallCmd = mustRemoteUHPCmd(
	remoteUHPCall{Method: "host.update.install", Params: map[string]any{"confirm": true}},
	remoteUHPCall{Method: "session.restart", Params: map[string]any{"name": "default", "confirm": true}},
)

// serveHostUpdate brings a remote host's Luvus up to a release whose UHP
// capabilities this lasso can use.
//
// It drives the host profile over `luvus uhp proxy` rather than the interactive
// `luvus update`: every step is a confirmed UHP call with a structured reply, so
// there is no PTY to allocate and no prompt to guess the wording of. The check
// runs first and an already-current host is left completely alone — the install
// path restarts its session, and doing that to change nothing would kill a
// fleet's panes for a no-op.
func serveHostUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host string `json:"host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Host == "" || req.Host == "local" {
		http.Error(w, "remote host required", http.StatusBadRequest)
		return
	}
	// Only update a host we've already probed as reachable with a Luvus server
	// running, so a stray alias can't make us shell out to an arbitrary box. The
	// alias rides ssh's argv (not a shell), so it can't inject a command.
	hi, ok := findHost(req.Host)
	if !ok || !hi.Reachable || !hi.Running {
		http.Error(w, "host not reachable / no luvus server running", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), hostUpdateTimeout)
	defer cancel()

	out, err := runRemoteLuvus(ctx, req.Host, luvusUpdateCheckCmd)
	if err != nil {
		writeJSON(w, remoteFailure(ctx, out, err))
		return
	}
	var chk struct {
		Available bool   `json:"available"`
		Current   string `json:"current"`
		Latest    string `json:"latest"`
	}
	if res, cerr := firstUHPResult(out, "host_update_status"); cerr != nil {
		writeJSON(w, map[string]any{"ok": false, "output": strings.TrimSpace(string(out)), "error": cerr.Error()})
		return
	} else if json.Unmarshal(res, &chk) != nil {
		writeJSON(w, map[string]any{"ok": false, "output": strings.TrimSpace(string(out)), "error": "unreadable update status"})
		return
	}
	if !chk.Available {
		writeJSON(w, map[string]any{
			"ok":     true,
			"output": "luvus " + chk.Current + " on " + req.Host + " is already the latest release (" + chk.Latest + "); nothing was installed or restarted",
		})
		return
	}

	log.Printf("host:     updating luvus on %s (%s → %s)", req.Host, chk.Current, chk.Latest)
	out, err = runRemoteLuvus(ctx, req.Host, luvusUpdateInstallCmd)
	if err != nil {
		writeJSON(w, remoteFailure(ctx, out, err))
		return
	}
	resp := map[string]any{"ok": true, "output": strings.TrimSpace(string(out))}
	if uerr := uhpBatchError(out); uerr != nil {
		resp["ok"], resp["error"] = false, uerr.Error()
	} else {
		// The host's Luvus just changed; drop the cache so the next /api/hosts
		// re-probes and reflects the new version/capabilities/compatibility.
		invalidateHostCache()
	}
	writeJSON(w, resp)
}

// runRemoteLuvus runs a rendered UHP batch on a host over a one-shot ssh,
// matching the probe's options: no forwardings (one command, nothing to
// tunnel), BatchMode so anything that would prompt fails instead of hanging,
// and the login-shell PATH wrapper so a freshly provisioned ~/.local/bin/luvus
// is found.
func runRemoteLuvus(ctx context.Context, alias, cmdline string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new",
		alias, remoteLuvusShell(cmdline))
	cmd.WaitDelay = probeWaitDelay
	return cmd.CombinedOutput()
}

// remoteFailure renders the response body for a remote command that failed to
// run at all (ssh died, the budget ran out), keeping the captured output —
// which is the only thing that explains a remote failure.
func remoteFailure(ctx context.Context, out []byte, err error) map[string]any {
	resp := map[string]any{"ok": false, "output": strings.TrimSpace(string(out))}
	if ctx.Err() == context.DeadlineExceeded {
		resp["error"] = "timed out"
	} else {
		resp["error"] = err.Error()
	}
	return resp
}

// firstUHPResult returns the result of the first reply frame in out whose type
// matches, or the first error any frame reported — so a refused call explains
// itself instead of surfacing as "unreadable".
func firstUHPResult(out []byte, wantType string) (json.RawMessage, error) {
	var failure error
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var env uhpEnvelope
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		if env.Error != nil {
			if failure == nil {
				failure = errors.New(env.Error.Message)
			}
			continue
		}
		if env.resultType() == wantType {
			return env.Result, nil
		}
	}
	if failure != nil {
		return nil, failure
	}
	return nil, fmt.Errorf("no %s reply", wantType)
}

// uhpBatchError reports the first error any reply frame in out carried. A batch
// rides one ssh whose exit status is the LAST pipeline's, so a refused call in
// the middle is invisible without reading the frames.
func uhpBatchError(out []byte) error {
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var env uhpEnvelope
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		if env.Error != nil {
			return errors.New(env.Error.Message)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// POST /api/host-provision — install Luvus + supervise it with systemd --user
// ---------------------------------------------------------------------------

// hostProvisionTimeout bounds the whole bootstrap: it may download the Luvus
// release archive over the far host's network.
const hostProvisionTimeout = 5 * time.Minute

// provisionScript bootstraps Luvus-under-systemd on a remote Linux host, end to
// end and idempotently: ensure luvus (luvus.dev/install.sh), write a systemd
// --user unit for the server, enable lingering so it survives logout/reboot,
// start it, and install the agent-state integrations for every harness lasso
// can spawn so Luvus gets authoritative idle/working/blocked reports instead of
// screen-scraping. It's shell-agnostic — rather than trust the login shell's
// PATH wiring, it puts the user-local bin dirs on PATH itself. Every step logs
// a line so the captured output reads as a provisioning log.
//
// The integration list is substituted from the harness table rather than
// spelled out, so adding a harness can't leave newly-spawnable agents
// screen-scraped on every remote host until someone notices.
var provisionScript = strings.Replace(provisionScriptTemplate, harnessIDsPlaceholder, strings.Join(harnessIDs(), " "), 1)

// harnessIDsPlaceholder marks where provisionScriptTemplate wants the harness
// list. Its `@`s keep it from being mistaken for shell syntax if substitution
// were ever skipped.
const harnessIDsPlaceholder = "@HARNESS_IDS@"

// harnessIDs lists every launchable harness id, in registry order. These double
// as Luvus's `integration install` targets — the ids were chosen to match.
func harnessIDs() []string {
	ids := make([]string, 0, len(harnesses))
	for _, h := range harnesses {
		ids = append(ids, h.ID)
	}
	return ids
}

const provisionScriptTemplate = `set -u
log() { printf '==> %s\n' "$*"; }

export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"
hash -r 2>/dev/null || true

# 0. systemd -----------------------------------------------------------------
# Supervision is systemd --user; a non-interactive ssh session may lack the
# runtime dir env the user manager is addressed by.
command -v systemctl >/dev/null 2>&1 || { echo "ERROR: systemctl not found — provisioning requires a Linux host with systemd" >&2; exit 3; }
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
systemctl --user is-enabled default.target >/dev/null 2>&1 || true
loginctl enable-linger "$(id -un)" 2>/dev/null || log "note: 'loginctl enable-linger' failed; the luvus server may stop at logout"

# 1. luvus -------------------------------------------------------------------
if ! command -v luvus >/dev/null 2>&1; then
  log "installing luvus (luvus.dev/install.sh)"
  curl -fsSL https://luvus.dev/install.sh | sh
  hash -r 2>/dev/null || true
fi
luvus_bin="$(command -v luvus 2>/dev/null || echo "$HOME/.local/bin/luvus")"
[ -x "$luvus_bin" ] || { echo "ERROR: luvus not installed" >&2; exit 4; }
log "luvus $("$luvus_bin" --version 2>/dev/null)"

# 2. systemd --user unit -----------------------------------------------------
# Written unconditionally (marked managed) so re-provisioning refreshes it.
# ExecStart is the bare 'luvus server', which runs the server in the FOREGROUND
# — 'luvus server start' daemonizes and writes a pid file carrying more than a
# pid, so neither Type=simple nor a PIDFile= could track it.
unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
mkdir -p "$unit_dir"
log "writing $unit_dir/luvus.service"
cat > "$unit_dir/luvus.service" <<EOF
[Unit]
Description=luvus — headless terminal workspace server for AI coding agents
# managed by lasso host provisioning; edits may be overwritten on re-provision
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$HOME
Environment=PATH=$HOME/.local/bin:$HOME/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin
ExecStart=$luvus_bin server
# Graceful shutdown via Luvus's own API so panes are torn down cleanly.
ExecStop=$luvus_bin server stop
# Only signal the main server process, not every pane in the cgroup.
KillMode=mixed
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload
log "starting luvus under systemd --user"
systemctl --user enable --now luvus.service || { echo "ERROR: 'systemctl --user enable --now luvus' failed" >&2; exit 5; }

# 3. agent-state integrations ------------------------------------------------
# Session-resume hooks give Luvus authoritative agent identity and state for the
# agents lasso spawns; without them it falls back to process/screen detection.
# Best-effort: an integration for a CLI that isn't installed yet still stages
# its hook files and starts working once that CLI arrives, and an id Luvus does
# not know is reported rather than fatal.
for agent in @HARNESS_IDS@; do
  if "$luvus_bin" integration install "$agent" >/dev/null 2>&1; then
    log "integration $agent installed"
  else
    log "note: 'luvus integration install $agent' failed; agent state falls back to process detection"
  fi
done

# 4. verify ------------------------------------------------------------------
# session.list is a host-profile method, answered by a short-lived 'uhp proxy'
# out of local state, so this asks the machine itself rather than trusting the
# unit's exit status.
sleep 1
if printf '{"id":"verify","method":"session.list","params":{}}\n' | "$luvus_bin" uhp proxy 2>/dev/null | grep -Eq '"running"[[:space:]]*:[[:space:]]*true'; then
  log "luvus server running"
else
  echo "ERROR: luvus server not running after setup" >&2
  printf '{"id":"verify","method":"session.list","params":{}}\n' | "$luvus_bin" uhp proxy 2>&1 || true
  exit 6
fi
log "done"
`

// serveHostProvision installs Luvus on a remote host (if missing) and brings it
// up supervised by systemd --user, so a host that has no Luvus — or has it but
// with no server running — can be made selectable. Every step is
// non-interactive (the install script, systemctl and the UHP calls never
// prompt), so no PTY is needed: we pipe provisionScript to `bash -s`.
func serveHostProvision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host string `json:"host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Host == "" || req.Host == "local" {
		http.Error(w, "remote host required", http.StatusBadRequest)
		return
	}
	// Only provision a host we've probed as reachable (luvus may be missing or its
	// server down — that's the point). The alias rides ssh's argv, not a shell.
	hi, ok := findHost(req.Host)
	if !ok || !hi.Reachable {
		http.Error(w, "host not reachable", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), hostProvisionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new",
		req.Host, "bash -s")
	cmd.Stdin = strings.NewReader(provisionScript)
	out, err := cmd.CombinedOutput()

	resp := map[string]any{"ok": err == nil, "output": strings.TrimSpace(string(out))}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			resp["error"] = "timed out"
		} else {
			resp["error"] = err.Error()
		}
	} else {
		invalidateHostCache()
	}
	writeJSON(w, resp)
}
