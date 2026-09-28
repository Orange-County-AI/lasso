package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// srvHub and srvCtx are set in main so the host-switch handler can reach the SSE
// hub (to re-subscribe events + bump the terminal-reload counter) and the root
// context (the lifetime of remote backends + their SSH masters).
var (
	srvHub *hub
	srvCtx context.Context
)

// switchMu serializes host switches: a switch re-points the herdr subscription
// and both terminals, so two in flight at once would race. A second concurrent
// request gets 409 (the footer also disables its control while one is pending).
var switchMu sync.Mutex

// ---------------------------------------------------------------------------
// ttyd terminals — one instance per (role, host), kept warm across switches
// ---------------------------------------------------------------------------

// ttydSpawnTimeout bounds the wait for a freshly spawned ttyd to bind its
// socket, so the browser's iframe remount can't beat it to the path.
const ttydSpawnTimeout = 5 * time.Second

// ttydWarmHosts is the hard ceiling on resident terminals per role: it bounds a
// burst (a script or a deep-link tour hopping every alias in the ssh config),
// past which the least-recently-active instance is retired. It is generous
// because a resident idle ttyd costs ~9MB and no child process at all, while
// re-spawning one costs ~200ms on the switch — the working set of hosts someone
// actually rotates between should never hit it. The active instance is never a
// candidate.
const ttydWarmHosts = 6

// ttydIdle retires a warm terminal nobody came back to, bounding the cost of a
// LONG session the way ttydWarmHosts bounds a burst: visit six hosts once and a
// quarter of an hour later you are paying only for the ones still in rotation.
// Mirrors the host pool's idle reaping (hostBackendIdle).
const ttydIdle = 15 * time.Minute

// ttydInstance is one running ttyd: the private socket it serves and the cancel
// that stops it. lastActive orders eviction.
type ttydInstance struct {
	sock       string
	cancel     context.CancelFunc
	lastActive time.Time
}

// ttydRole owns one terminal role — the left herdr terminal (/terminal) or the
// right shell tab (/shell) — across every host lasso has driven. Each host gets
// its OWN ttyd, on its own socket AND its own proxy path (/terminal/<slug>/),
// because the browser is what picks between them now: a tab viewing norm loads
// /terminal/norm/ while the tab beside it loads /terminal/titan/, and both are
// live at once.
//
// The role used to carry an `active` host and the proxy dialled whichever
// instance that named, which is exactly the global that made per-tab hosts
// impossible — one tab switching host re-pointed every other tab's terminal.
// The instances were already per host; only the pointer had to go.
//
// The single-socket predecessor to THAT respawned in place on every switch, and
// that respawn was the dominant cost of switching: the new ttyd could not bind
// until the old one had released the shared path, which measured ~2.8s of a
// ~3.9s switch (ttyd drops its client, SIGHUPs the child `herdr --remote`, then
// unlinks), serially for both roles, on the request path.
type ttydRole struct {
	parent      context.Context
	name        string // socket filename stem ("ttyd", "shell")
	basePath    string // proxy base path ("/terminal", "/shell")
	waitTimeout time.Duration

	mu     sync.Mutex
	inst   map[string]*ttydInstance // keyed by backend name ("local" or an alias)
	bySlug map[string]string        // url path segment -> backend name
}

func newTtydRole(parent context.Context, name, basePath string) *ttydRole {
	return &ttydRole{
		parent:      parent,
		name:        name,
		basePath:    basePath,
		waitTimeout: ttydSpawnTimeout,
		inst:        map[string]*ttydInstance{},
		bySlug:      map[string]string{},
	}
}

// hostSlug is the URL path segment (and socket filename component) for a host.
// sanitizeAlias alone is not injective — "a.b" and "a_b" collapse together — and
// a collision now means two hosts sharing one terminal rather than merely one
// odd filename, so an altered alias carries a short digest of the original.
func hostSlug(host string) string {
	s := sanitizeAlias(host)
	if s == host {
		return s
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(host))
	return fmt.Sprintf("%s-%08x", s, h.Sum32())
}

// sockPath is the private socket for one host's instance of this role. It
// carries lasso's pid (so concurrent prod/dev instances can't cross-connect)
// and the host, so two instances can never contend for a path.
func (r *ttydRole) sockPath(host string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("lasso-%s-%d-%s.sock", r.name, os.Getpid(), hostSlug(host)))
}

// ensure makes host's ttyd resident, spawning it (running command with env) only
// when it isn't already. A resident instance is reused as is: a tab arriving on
// a host someone else already has open pays nothing.
//
// Each instance is spawned with its OWN base path, /<role>/<slug>, so ttyd's
// asset and websocket URLs resolve back to the same instance the iframe loaded.
func (r *ttydRole) ensure(host, command string, env []string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.inst[host]; e != nil {
		e.lastActive = time.Now()
		return nil
	}
	slug := hostSlug(host)
	sock := r.sockPath(host)
	ctx, cancel := context.WithCancel(r.parent)
	if err := startTtyd(ctx, sock, r.basePath+"/"+slug, command, env); err != nil {
		cancel()
		return err
	}
	if !waitSocketUp(sock, r.waitTimeout) {
		cancel()
		return fmt.Errorf("timed out waiting for ttyd socket %s to open", sock)
	}
	r.inst[host] = &ttydInstance{sock: sock, cancel: cancel, lastActive: time.Now()}
	r.bySlug[slug] = host
	r.evictLocked()
	return nil
}

// sockForSlug is the socket the proxy dials for a /<role>/<slug>/… request, and
// marks that instance as freshly used so idle reaping leaves it alone. Empty
// when no such instance exists (a stale iframe for a retired host, or a request
// that beat the first spawn). Nil-receiver safe: the mux is wired before any
// spawn.
func (r *ttydRole) sockForSlug(slug string) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	host, ok := r.bySlug[slug]
	if !ok {
		return ""
	}
	e := r.inst[host]
	if e == nil {
		return ""
	}
	e.lastActive = time.Now()
	return e.sock
}

// resident reports whether host's terminal is currently spawned. hostInUse asks,
// so a host with a live terminal keeps its connection.
func (r *ttydRole) resident(host string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inst[host] != nil
}

// retireLocked stops one instance and forgets it. The teardown it triggers is
// asynchronous (startTtyd's ctx watcher signals the process group): it costs
// seconds, and with per-host socket paths nothing ever waits on that path again.
func (r *ttydRole) retireLocked(host string, why string) {
	e := r.inst[host]
	if e == nil {
		return
	}
	delete(r.inst, host)
	delete(r.bySlug, hostSlug(host))
	log.Printf("ttyd:     retired %s terminal for %s (%s)", r.basePath, host, why)
	e.cancel()
}

// evictLocked retires the least-recently-active instances above ttydWarmHosts.
// A host a tab is actually watching is never a candidate, however long its
// terminal has sat idle — with several tabs open, "least recently active" is no
// longer a proxy for "nobody is looking at it".
func (r *ttydRole) evictLocked() {
	for len(r.inst) > ttydWarmHosts {
		oldest := ""
		for host, e := range r.inst {
			if hostWatched(host) {
				continue
			}
			if oldest == "" || e.lastActive.Before(r.inst[oldest].lastActive) {
				oldest = host
			}
		}
		if oldest == "" {
			return
		}
		r.retireLocked(oldest, "warm-host ceiling")
	}
}

// retireIdleLocked retires every instance nobody returned to within ttydIdle. A
// watched host is exempt however long it sits: it is what some tab's iframe is
// pointed at.
func (r *ttydRole) retireIdleLocked(now time.Time) {
	for host, e := range r.inst {
		if !hostWatched(host) && now.Sub(e.lastActive) > ttydIdle {
			r.retireLocked(host, "idle")
		}
	}
}

// hostWatched reports whether host is the default one or has a tab watching it.
// Deliberately NOT hostInUse, which also counts a resident terminal — that would
// make every terminal exempt from the reaping this decides.
func hostWatched(host string) bool {
	// Nil before main installs the default backend, and in tests that exercise
	// eviction without standing a host up.
	if b := defaultBackend(); b != nil && b.Name() == host {
		return true
	}
	if srvHub == nil {
		return false
	}
	for _, h := range srvHub.feedHosts() {
		if h == host {
			return true
		}
	}
	return false
}

// sweepIdle runs retireIdleLocked until the role's parent context (the server's
// lifetime) ends. Started explicitly by the owner rather than by newTtydRole, so
// constructing a role in a test doesn't leave a ticker behind.
func (r *ttydRole) sweepIdle() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-r.parent.Done():
			return
		case now := <-t.C:
			r.mu.Lock()
			r.retireIdleLocked(now)
			r.mu.Unlock()
		}
	}
}

// terminals holds the two roles (nil when -spawn-ttyd=false).
var terminals struct {
	herdr *ttydRole // left "Herdr" terminal (/terminal)
	shell *ttydRole // right shell tab (/shell)
}

// ensureTerminals makes backend b's pair of terminals resident: the left one
// runs b.TermCmd() (local herdr, or `herdr --remote <host>`); the shell tab runs
// b.ShellCmd() (local shell, or `ssh <host>`) with the herdr session markers
// stripped. Idempotent, so a tab arriving on a warm host pays nothing.
func ensureTerminals(b Backend) error {
	if err := terminals.herdr.ensure(b.Name(), termPrefix()+b.TermCmd(), b.TermEnv()); err != nil {
		return fmt.Errorf("terminal on %s: %w", b.Name(), err)
	}
	if err := terminals.shell.ensure(b.Name(), b.ShellCmd(), outsideHerdrEnv()); err != nil {
		return fmt.Errorf("shell on %s: %w", b.Name(), err)
	}
	return nil
}

// waitSocketUp polls until the socket file exists, or the timeout elapses.
func waitSocketUp(sock string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, err := os.Stat(sock)
	return err == nil
}

// ---------------------------------------------------------------------------
// POST /api/host — attach THIS tab to a host
// ---------------------------------------------------------------------------

// serveHostAttach prepares a host for a browser tab that is moving onto it:
// resolves (and pools) its connection, spawns its terminals if they aren't warm
// already, and reports the herdr version/protocol the tab should expect.
//
// It replaces a handler that SWITCHED a process-wide active host. Nothing here
// mutates shared state any more — the tab records its own choice and sends it on
// every subsequent request (reqhost.go) — so two tabs can attach to two hosts
// and neither disturbs the other. The default host is unaffected either way: a
// fresh tab and an MCP whoami still see the host lasso booted on.
func serveHostAttach(w http.ResponseWriter, r *http.Request) {
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
	target := req.Host
	if target == "" {
		http.Error(w, "host required", http.StatusBadRequest)
		return
	}

	var b Backend
	if target == "local" {
		b = &localBackend{sock: *herdrSock}
	} else {
		hi, ok := findHost(target)
		if !ok || !hi.Reachable || !hi.Running || !hi.Compatible {
			http.Error(w, "host not available (no compatible herdr server)", http.StatusBadRequest)
			return
		}
		// One pooled connection per host, shared by every tab on it and by all
		// host-addressed RPC/file/diff work. Attaching a second tab to a host
		// someone already has open is free.
		pooled, err := hostBackend(target)
		if err != nil {
			http.Error(w, "connect "+target+": "+err.Error(), http.StatusBadGateway)
			return
		}
		rb, ok := pooled.(*remoteBackend)
		if !ok { // only "local" is not a remote backend, and it took the branch above
			http.Error(w, "connect "+target+": no remote connection", http.StatusBadGateway)
			return
		}
		b = rb
		// Mirror the local machine's theme onto the target host's herdr — but in
		// the BACKGROUND, off the attach's critical path. It's ~2 SSH round trips
		// (write config + reload_config), and blocking on it made every cross-host
		// move feel ~2s slower for a purely cosmetic change: the ttyd palette comes
		// from lasso's LOCAL resolved theme (startTtyd's -t theme=), not from the
		// remote herdr, so terminals already render correctly; this only repaints
		// the remote herdr TUI's own chrome, which can lag a beat harmlessly.
		// A theme CHANGE reaches every host on its own (syncThemeEverywhere);
		// this covers a host that was asleep or unreachable when that happened.
		if srvHub != nil {
			go syncRemoteTheme(rb, srvHub.themeSnapshot().Resolved)
		}
	}

	// Spawn this host's terminals before answering, so the iframe the tab is
	// about to point at /terminal/<slug>/ finds a socket bound rather than a 502.
	// Serialized per host inside ensure(); two tabs attaching at once share one
	// spawn.
	if *spawnTtyd {
		if err := ensureTerminals(b); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	// Start the host's feed now (and kick it) so the tab's SSE stream primes with
	// real state instead of the seeded empty frame.
	if srvHub != nil {
		if f, err := srvHub.feed(b.Name()); err == nil {
			f.kick()
		}
	}

	log.Printf("host:     tab attached to %s", b.Name())
	writeHostResult(w, b)
}

// syncRemoteTheme writes theme name into the config.toml the remote herdr reads,
// mirrors it into that host's agent CLI theme files, and asks its herdr to reload
// so the TUI repaints. name is a canonical theme key. It returns the joined
// failure of the file writes; every caller treats it as best-effort (a host
// switch, a theme change, a convergence push) and lets the next pass retry.
//
// The three steps are independent and all three are attempted: an unwritable
// herdr config must not cost the host its ghostty palette, and the reload is the
// LAST thing, not a gate — asking a herdr to reload used to happen before the
// agent themes were written, so any host whose herdr could not be reached (one
// speaking a protocol this build refuses, reached over a files-only connection)
// silently kept a month-old ghostty theme. A reload lasso cannot make is a stale
// TUI until that herdr restarts, nothing more, so it is logged and not returned.
//
// A host switched off in the theme_sync_off deny-list is left entirely alone —
// its herdr config and its agents' theme files stay whatever that machine set
// them to (see agentsync.go).
func syncRemoteTheme(t themeTarget, name string) error {
	if t == nil || name == "" {
		return nil
	}
	host := t.Name()
	if !themeSyncEnabledFor(host) {
		log.Printf("host:     theme sync to %s off (disabled for this host)", host)
		return nil
	}
	var errs []error
	// The path comes from the remote's environment, never from the socket's
	// directory: herdr picks its socket independently of its config dir, and on
	// every agent-workspace box (socket in /dev/shm/herdr/) the socket-adjacent
	// guess wrote a config.toml nothing reads — the sync logged success while the
	// remote TUI kept its old palette.
	switch cfg := t.herdrConfigPath(); {
	case cfg == "":
		log.Printf("host:     herdr theme name on %s skipped: config path unknown", host)
		errs = append(errs, errors.New("herdr config path unknown"))
	default:
		if err := writeHerdrThemeNameVia(t, cfg, name); err != nil {
			log.Printf("host:     herdr theme name on %s failed: %v", host, err)
			errs = append(errs, err)
		}
	}
	// Mirror the theme into the host's agent CLIs too (opencode, Claude Code,
	// omp, ghostty). Resolved by name only — the remote's own [theme.custom]
	// tokens stay herdr's business.
	if err := syncAgentThemesVia(t, resolveThemeByName(name)); err != nil {
		errs = append(errs, err)
	}
	// Only a host with a herdr this lasso can speak to has a socket to ask.
	if t.HerdrSock() != "" {
		if _, err := t.HerdrCall("server.reload_config", map[string]any{}); err != nil {
			log.Printf("host:     theme reload on %s failed: %v", host, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	log.Printf("host:     synced theme %q -> %s", name, host)
	return nil
}

// writeHostResult reports the now-active host plus its herdr version/protocol.
func writeHostResult(w http.ResponseWriter, b Backend) {
	var version string
	var protocol int
	if rb, ok := b.(*remoteBackend); ok {
		version, protocol = rb.version, rb.protocol
	} else {
		version, protocol = localProtocol()
	}
	writeJSON(w, map[string]any{"active": b.Name(), "version": version, "protocol": protocol})
}
