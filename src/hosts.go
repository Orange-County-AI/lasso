package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

// HostInfo describes one ssh-config host as a candidate herdr target. A host is
// usable (selectable in the footer switcher) when Reachable && Running &&
// Compatible; otherwise the UI greys it out and shows Err (or, for a non-empty
// State, a pending/unknown affordance rather than a failure).
type HostInfo struct {
	Alias      string `json:"alias"`
	Hostname   string `json:"hostname"`   // effective ssh HostName (for grouping aliases on one box)
	User       string `json:"user"`       // effective ssh User (distinguishes users on one host)
	Reachable  bool   `json:"reachable"`  // ssh connected and ran the probe
	Running    bool   `json:"running"`    // herdr server is up on the host
	Version    string `json:"version"`    // remote herdr version
	Protocol   int    `json:"protocol"`   // remote herdr protocol
	Socket     string `json:"socket"`     // absolute remote herdr socket path
	Compatible bool   `json:"compatible"` // Protocol == local protocol
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
		Protocol int    `json:"protocol"`
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
// local protocol (cached)
// ---------------------------------------------------------------------------

var localProto struct {
	once     sync.Once
	version  string
	protocol int
}

// localProtocol returns this machine's herdr protocol version (and version
// string), pinging the local socket once and caching the result. A host is
// "compatible" when its protocol equals this.
func localProtocol() (string, int) {
	localProto.once.Do(func() {
		if v, p, err := herdrPing(*herdrSock); err == nil {
			localProto.version, localProto.protocol = v, p
		}
	})
	return localProto.version, localProto.protocol
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

// remoteHerdrShell wraps a herdr command line so it runs in the remote user's
// login shell with the usual user-local install dirs (~/.local/bin and mise's
// shim dir) forced onto PATH first. `ssh host <cmd>` uses a non-login shell whose
// PATH omits those dirs, and even `$SHELL -lc` only finds herdr if the login
// profile happens to add them — a freshly provisioned host (herdr just dropped in
// ~/.local/bin, profile not yet wired) would otherwise still report "command not
// found" and keep showing "set up". Prefixing PATH ourselves — matching what the
// provision script does — makes detection and update robust regardless of how the
// host's profile is set up. $HOME/$PATH are left for the remote login shell to
// expand (the single-quoted body is opaque to the outer shell).
func remoteHerdrShell(herdrCmd string) string {
	return `${SHELL:-sh} -lc 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"; ` + herdrCmd + `'`
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

// probeHost asks a host whether it has a compatible herdr server running by
// running `herdr status server --json` over ssh. BatchMode makes hosts that
// would prompt (password / unknown key) fail fast rather than hang.
func probeHost(ctx context.Context, alias string, wantProto int) HostInfo {
	hi := HostInfo{Alias: alias}
	cctx, cancel := context.WithTimeout(ctx, hostProbeTimeout)
	defer cancel()
	// ClearAllForwardings drops any LocalForward/RemoteForward the user's config
	// attaches to this host (e.g. a tunnel that conflicts with a busy port) — the
	// probe only needs to run one command, no forwarding. remoteHerdrShell runs
	// herdr in a login shell with the user-local install dirs forced onto PATH.
	cmd := exec.CommandContext(cctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout="+probeConnectTimeout,
		"-o", "StrictHostKeyChecking=accept-new",
		alias, remoteHerdrShell("herdr status server --json"))
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
		// herdr not installed" and the UI offered to *install herdr* on a box we
		// never reached. Report the honest thing instead: unknown, timed out.
		if cctx.Err() != nil {
			hi.State = hostTimedOut
			hi.Err = "timed out probing (no answer in " + hostProbeTimeout.String() + ")"
			return hi
		}
		// ssh itself failed to connect (exit 255), or the process couldn't run at
		// all → unreachable. Any other exit code means the remote ran the command
		// (e.g. exit 127 "herdr: command not found") → reachable but no herdr.
		if !isExit || ee.ExitCode() == 255 {
			hi.Err = "unreachable"
			if stderr != "" {
				hi.Err = stderr
			}
			return hi
		}
		hi.Reachable = true
		if len(out) == 0 {
			hi.Err = "herdr not installed"
			if stderr != "" {
				hi.Err = stderr
			}
			return hi
		}
		// Non-zero exit but JSON on stdout (e.g. server stopped): fall through.
	}
	hi.Reachable = true
	var st struct {
		Running  bool   `json:"running"`
		Version  string `json:"version"`
		Protocol int    `json:"protocol"`
		Socket   string `json:"socket"`
	}
	if jerr := json.Unmarshal(out, &st); jerr != nil {
		hi.Err = "herdr not running"
		return hi
	}
	hi.Running = st.Running
	hi.Version = st.Version
	hi.Protocol = st.Protocol
	hi.Socket = st.Socket
	if !st.Running {
		hi.Err = "herdr not running"
		return hi
	}
	hi.Compatible = wantProto != 0 && st.Protocol == wantProto
	if !hi.Compatible {
		hi.Err = "protocol " + strconv.Itoa(st.Protocol) + " ≠ " + strconv.Itoa(wantProto)
	}
	return hi
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

	_, wantProto := localProtocol()
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
			hi := probeHostFn(ctx, alias, wantProto)
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
	p.Local.Protocol = proto
	p.Local.Hostname = localHostname()
	p.Local.User = localUsername()
	p.Hosts = hosts
	p.Probing = probing
	writeJSON(w, p)
}

// invalidateHostCache forces the next discoverHosts to re-probe (used after an
// action that changes a host's herdr — e.g. a remote update).
func invalidateHostCache() {
	hostStore.mu.Lock()
	hostStore.at = time.Time{}
	hostStore.mu.Unlock()
}

// ---------------------------------------------------------------------------
// POST /api/host-update — run `herdr update` on a remote host
// ---------------------------------------------------------------------------

// hostUpdateTimeout bounds the whole remote update (manifest fetch + binary
// download + install), generous because it pulls a release binary over the far
// host's network.
const hostUpdateTimeout = 3 * time.Minute

// serveHostUpdate runs `herdr update` on a remote ssh-config host to bring a
// host that's behind this lasso's herdr protocol back into compatibility.
//
// herdr's updater is interactive: when a protocol change forces running sessions
// to restart it asks "stop after installing? [y/N]" (stopping exits the old
// server's pane processes), and after a successful update it may ask to star the
// repo. Both prompts bail with an error unless stdin is a TTY, so we force a
// remote PTY with `ssh -tt` and feed the answers — "y" to stop the old server
// (the caller has accepted killing those processes), then "n" to decline the
// star prompt. Any fed line a prompt doesn't consume is discarded once herdr
// exits, so over-feeding is harmless.
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
	// Only update a host we've already probed as reachable with herdr running, so
	// a stray alias can't make us shell out to an arbitrary box. The alias rides
	// ssh's argv (not a shell), so it can't inject a command.
	hi, ok := findHost(req.Host)
	if !ok || !hi.Reachable || !hi.Running {
		http.Error(w, "host not reachable / no herdr running", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), hostUpdateTimeout)
	defer cancel()

	// -tt forces a remote PTY even though our stdin is a pipe, so herdr's updater
	// sees a terminal and runs its prompts (rather than erroring out). remoteHerdrShell
	// runs in a login shell with `herdr` forced onto PATH, matching probeHost. We
	// only run one command, so clear any forwardings the host's config attaches.
	cmd := exec.CommandContext(ctx, "ssh",
		"-tt",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new",
		req.Host, remoteHerdrShell("herdr update"))
	cmd.Stdin = strings.NewReader("y\nn\n")
	out, err := cmd.CombinedOutput()

	resp := map[string]any{"ok": err == nil, "output": strings.TrimSpace(string(out))}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			resp["error"] = "timed out"
		} else {
			resp["error"] = err.Error()
		}
	} else {
		// The host's herdr just changed; drop the cache so the next /api/hosts
		// re-probes and reflects the new version/protocol/compatibility.
		invalidateHostCache()
	}
	writeJSON(w, resp)
}

// ---------------------------------------------------------------------------
// POST /api/host-provision — install herdr + supervise it with systemd --user
// ---------------------------------------------------------------------------

// hostProvisionTimeout bounds the whole bootstrap: it may download the herdr
// release binary over the far host's network.
const hostProvisionTimeout = 5 * time.Minute

// provisionScript bootstraps herdr-under-systemd on a remote Linux host, end to
// end and idempotently: ensure herdr (herdr.dev/install.sh), write a systemd
// --user unit for the server, enable lingering so it survives logout/reboot,
// start it, and install the agent-state integrations for every harness lasso
// can spawn so herdr gets authoritative idle/working/blocked hooks instead of
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
// as herdr's `integration install` targets — the ids were chosen to match.
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
loginctl enable-linger "$(id -un)" 2>/dev/null || log "note: 'loginctl enable-linger' failed; the herdr server may stop at logout"

# 1. herdr -------------------------------------------------------------------
if ! command -v herdr >/dev/null 2>&1; then
  log "installing herdr (herdr.dev/install.sh)"
  curl -fsSL https://herdr.dev/install.sh | sh
  hash -r 2>/dev/null || true
fi
herdr_bin="$(command -v herdr 2>/dev/null || echo "$HOME/.local/bin/herdr")"
[ -x "$herdr_bin" ] || command -v herdr >/dev/null 2>&1 || { echo "ERROR: herdr not installed" >&2; exit 4; }
log "herdr $("$herdr_bin" --version 2>/dev/null)"

# 2. systemd --user unit -----------------------------------------------------
# Written unconditionally (marked managed) so re-provisioning refreshes it.
unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
mkdir -p "$unit_dir"
log "writing $unit_dir/herdr.service"
cat > "$unit_dir/herdr.service" <<EOF
[Unit]
Description=herdr — headless terminal workspace server for AI coding agents
# managed by lasso host provisioning; edits may be overwritten on re-provision
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$HOME
Environment=PATH=$HOME/.local/bin:$HOME/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin
ExecStart=$herdr_bin server
# Graceful shutdown via herdr's own API so panes are torn down cleanly.
ExecStop=$herdr_bin server stop
# Only signal the main server process, not every pane in the cgroup.
KillMode=mixed
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload
log "starting herdr under systemd --user"
systemctl --user enable --now herdr.service || { echo "ERROR: 'systemctl --user enable --now herdr' failed" >&2; exit 5; }

# 3. agent-state integrations ------------------------------------------------
# Lifecycle hooks give herdr authoritative idle/working/blocked states for the
# agents lasso spawns; without them it falls back to screen-buffer detection.
# Best-effort: an integration for a CLI that isn't installed yet still stages
# its hook files and starts working once that CLI arrives.
for agent in @HARNESS_IDS@; do
  if "$herdr_bin" integration install "$agent" >/dev/null 2>&1; then
    log "integration $agent installed"
  else
    log "note: 'herdr integration install $agent' failed; agent state falls back to screen detection"
  fi
done

# 4. verify ------------------------------------------------------------------
sleep 1
if "$herdr_bin" status server --json 2>/dev/null | grep -Eq '"running"[[:space:]]*:[[:space:]]*true'; then
  log "herdr server running"
else
  echo "ERROR: herdr server not running after setup" >&2
  "$herdr_bin" status server --json 2>/dev/null || true
  exit 6
fi
log "done"
`

// serveHostProvision installs herdr on a remote host (if missing) and brings it
// up supervised by systemd --user, so a host that has no herdr — or has it but
// with no server running — can be made selectable. Unlike the update path this
// is fully non-interactive (the install scripts and systemctl commands don't
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
	// Only provision a host we've probed as reachable (herdr may be missing or its
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
