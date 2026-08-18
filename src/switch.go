package main

import (
	"context"
	"encoding/json"
	"fmt"
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
// its OWN ttyd on its own socket path, so a host switch only re-points the role
// at another instance (unixSocketProxy resolves activeSock per request).
//
// The single-socket predecessor respawned in place on every switch, and that
// respawn was the dominant cost of switching: the new ttyd could not bind until
// the old one had released the shared path, which measured ~2.8s of a ~3.9s
// switch (ttyd drops its client, SIGHUPs the child `herdr --remote`, then
// unlinks), serially for both roles, on the request path. The browser's remount
// raced that gap and 502'd against a socket that no longer existed.
type ttydRole struct {
	parent      context.Context
	name        string // socket filename stem ("ttyd", "shell")
	basePath    string // proxy base path ("/terminal", "/shell")
	waitTimeout time.Duration

	mu     sync.Mutex
	inst   map[string]*ttydInstance // keyed by backend name ("local" or an alias)
	active string                   // the backend name this role currently serves
}

func newTtydRole(parent context.Context, name, basePath string) *ttydRole {
	return &ttydRole{
		parent:      parent,
		name:        name,
		basePath:    basePath,
		waitTimeout: ttydSpawnTimeout,
		inst:        map[string]*ttydInstance{},
	}
}

// sockPath is the private socket for one host's instance of this role. It
// carries lasso's pid (so concurrent prod/dev instances can't cross-connect)
// and the host, so two instances can never contend for a path — the property
// the old shared path lacked.
func (r *ttydRole) sockPath(host string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("lasso-%s-%d-%s.sock", r.name, os.Getpid(), sanitizeAlias(host)))
}

// activate points the role at host, spawning its ttyd (running command with
// env) only when it isn't already resident. A resident instance is reused as
// is: nothing is killed and the switch pays nothing for this role.
func (r *ttydRole) activate(host, command string, env []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.inst[host]; e != nil {
		e.lastActive = time.Now()
		r.active = host
		return nil
	}
	sock := r.sockPath(host)
	ctx, cancel := context.WithCancel(r.parent)
	if err := startTtyd(ctx, sock, r.basePath, command, env); err != nil {
		cancel()
		return err
	}
	if !waitSocketUp(sock, r.waitTimeout) {
		cancel()
		return fmt.Errorf("timed out waiting for ttyd socket %s to open", sock)
	}
	r.inst[host] = &ttydInstance{sock: sock, cancel: cancel, lastActive: time.Now()}
	r.active = host
	r.evictLocked()
	return nil
}

// activeSock is the socket the proxy dials right now — empty when the role has
// no instance yet (the mux is wired before the first spawn, so the proxy asks
// before there is an answer). Nil-receiver safe for the same reason.
func (r *ttydRole) activeSock() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.inst[r.active]; e != nil {
		return e.sock
	}
	return ""
}

// retireLocked stops one instance and forgets it. The teardown it triggers is
// asynchronous (startTtyd's ctx watcher signals the process group): it costs
// seconds, and with per-host socket paths nothing ever waits on that path again
// — the whole reason a switch no longer blocks on a terminal dying.
func (r *ttydRole) retireLocked(host string, why string) {
	e := r.inst[host]
	if e == nil {
		return
	}
	delete(r.inst, host)
	log.Printf("ttyd:     retired %s terminal for %s (%s)", r.basePath, host, why)
	e.cancel()
}

// evictLocked retires the least-recently-active instances above ttydWarmHosts.
func (r *ttydRole) evictLocked() {
	for len(r.inst) > ttydWarmHosts {
		oldest := ""
		for host, e := range r.inst {
			if host == r.active {
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

// retireIdleLocked retires every instance nobody returned to within ttydIdle.
// The active one is exempt however long it sits: it is what the proxy dials.
func (r *ttydRole) retireIdleLocked(now time.Time) {
	for host, e := range r.inst {
		if host != r.active && now.Sub(e.lastActive) > ttydIdle {
			r.retireLocked(host, "idle")
		}
	}
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

// terminals holds the two roles (nil when -spawn-ttyd=false). A host switch
// points both at the new host.
var terminals struct {
	herdr *ttydRole // left "Herdr" terminal (/terminal)
	shell *ttydRole // right shell tab (/shell)
}

// applyBackendToTerminals points both terminals at backend b, spawning that
// host's pair the first time it is visited: the left terminal runs b.TermCmd()
// (local herdr, or `herdr --remote <host>`); the shell tab runs b.ShellCmd()
// (local shell, or `ssh <host>`) with the herdr session markers stripped.
func applyBackendToTerminals(b Backend) {
	if terminals.herdr != nil {
		if err := terminals.herdr.activate(b.Name(), termPrefix()+b.TermCmd(), b.TermEnv()); err != nil {
			log.Printf("ttyd (terminal) on %s: %v", b.Name(), err)
		}
	}
	if terminals.shell != nil {
		if err := terminals.shell.activate(b.Name(), b.ShellCmd(), outsideHerdrEnv()); err != nil {
			log.Printf("ttyd (shell) on %s: %v", b.Name(), err)
		}
	}
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
// POST /api/host — switch the active host
// ---------------------------------------------------------------------------

func serveHostSwitch(w http.ResponseWriter, r *http.Request) {
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

	if !switchMu.TryLock() {
		http.Error(w, "a host switch is already in progress", http.StatusConflict)
		return
	}
	defer switchMu.Unlock()

	prev := curBackend()
	if target == prev.Name() {
		writeHostResult(w, prev) // no-op: already there
		return
	}

	// Resolve the new backend. On failure prev stays active — the caller is
	// unaffected.
	var newB Backend
	if target == "local" {
		newB = &localBackend{sock: *herdrSock}
	} else {
		hi, ok := findHost(target)
		if !ok || !hi.Reachable || !hi.Running || !hi.Compatible {
			http.Error(w, "host not available (no compatible herdr server)", http.StatusBadRequest)
			return
		}
		// Adopt the POOLED connection rather than dialing a second control
		// master to the same host. lasso used to hold two per host — one for the
		// active backend, one for host-addressed RPC/file/diff work — and tore
		// the active one down 2s after switching away, so switching back paid a
		// full ssh handshake (~1s of a measured 3.9s switch) while a healthy
		// connection to that very host sat in the pool. One connection per host,
		// owned by the pool, makes a switch back to a warm host free; the pool
		// liveness-checks it on access and idle-reaps it, and never reaps the
		// active host (see hostBackend / reapHostBackends).
		b, err := hostBackend(target)
		if err != nil {
			http.Error(w, "connect "+target+": "+err.Error(), http.StatusBadGateway)
			return
		}
		rb, ok := b.(*remoteBackend)
		if !ok { // only "local" is not a remote backend, and it took the branch above
			http.Error(w, "connect "+target+": no remote connection", http.StatusBadGateway)
			return
		}
		newB = rb
		// Mirror the local machine's theme onto the target host's herdr — but in
		// the BACKGROUND, off the switch's critical path. It's ~2 SSH round trips
		// (write config + reload_config), and blocking on it made every cross-host
		// focus feel ~2s slower for a purely cosmetic change: the ttyd palette comes
		// from lasso's LOCAL resolved theme (startTtyd's -t theme=), not from the
		// remote herdr, so terminals already render correctly; this only repaints
		// the remote herdr TUI's own chrome, which can lag a beat harmlessly.
		// A theme CHANGE reaches every host on its own (syncThemeEverywhere);
		// this covers a host that was asleep or unreachable when that happened.
		if srvHub != nil {
			go syncRemoteTheme(rb, srvHub.themeSnapshot().Resolved)
		}
	}

	// Swap, then re-point every host-bound subsystem at the new backend.
	setBackend(newB)
	invalidatePaneList()          // drop stale pane data from the old host
	applyBackendToTerminals(newB) // point both terminals at the new host
	if srvHub != nil {
		srvHub.startSub()    // re-subscribe events against the new socket
		srvHub.bumpTermRev() // tell the browser to reload the terminal iframes
		srvHub.kick()        // push fresh state without waiting for the poll tick
	}

	// The previous backend is NOT torn down: local Close is a no-op, and a
	// remote one is the host's pool entry, which outlives the switch so coming
	// back is free and so a long remote op (worktree.create, an agent boot, an
	// SFTP upload) started on the old host survives leaving it.

	log.Printf("host:     switched to %s", newB.Name())
	writeHostResult(w, newB)
}

// syncRemoteTheme writes theme name into the config.toml the remote herdr reads
// and asks that server to reload it, so the host renders in that theme.
// Best-effort: any failure is logged and never blocks the caller (a host switch
// or a theme change). name is a canonical theme key.
//
// A host switched off in the theme_sync_off deny-list is left entirely alone —
// its herdr config and its agents' theme files stay whatever that machine set
// them to (see agentsync.go).
func syncRemoteTheme(rb *remoteBackend, name string) {
	if rb == nil || name == "" {
		return
	}
	if !themeSyncEnabledFor(rb.alias) {
		log.Printf("host:     theme sync to %s off (disabled for this host)", rb.alias)
		return
	}
	// The path comes from the remote's environment, never from the socket's
	// directory: herdr picks its socket independently of its config dir, and on
	// every agent-workspace box (socket in /dev/shm/herdr/) the socket-adjacent
	// guess wrote a config.toml nothing reads — the sync logged success while the
	// remote TUI kept its old palette.
	cfg := rb.herdrConfigPath()
	if cfg == "" {
		log.Printf("host:     theme sync to %s skipped: herdr config path unknown", rb.alias)
		return
	}
	if err := writeHerdrThemeNameVia(rb, cfg, name); err != nil {
		log.Printf("host:     theme sync to %s failed: %v", rb.alias, err)
		return
	}
	if _, err := rb.HerdrCall("server.reload_config", map[string]any{}); err != nil {
		log.Printf("host:     theme reload on %s failed: %v", rb.alias, err)
		return
	}
	log.Printf("host:     synced theme %q -> %s", name, rb.alias)
	// Mirror the theme into the remote host's agent CLIs too (opencode, Claude
	// Code, omp). Resolved by name only — the remote's own [theme.custom] tokens
	// stay herdr's business.
	syncAgentThemesVia(rb, resolveThemeByName(name))
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
