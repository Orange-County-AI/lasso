package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The viewport: ONE persistent ttyd whose tmux client we re-point at the selected
// tab's session, instead of spawning a fresh ttyd + `tmux attach` per tab.
//
// Why: a brand-new ttyd/`tmux attach` pays a slow browser-side xterm⇄ttyd attach
// handshake (~5s) before the pane paints. The old per-tab pool paid it every time
// a tab was first viewed. Here the browser connects ONCE (warming that handshake
// a single time, ideally during app load), and every tab switch is just tmux
// repointing the already-warm client with `switch-client` — instant, no
// reconnect: one terminal, switched between sessions, feels fast.
//
// The client is kept glued to the selected tab by viewportWatcher (which also
// re-adopts a client that reconnected onto the park session). Each tab is still
// its own tmux session (lasso_<tabID>); we only change which one the single
// client is looking at. Reaping is gone — there's one ttyd, not a pool.

var viewport struct {
	mu      sync.Mutex
	token   string
	sock    string
	base    string // "/tab-term/<token>/"
	proxy   *httputil.ReverseProxy
	cancel  context.CancelFunc
	want    string          // session the client(s) should currently view
	clients map[string]bool // client ttys we've adopted (this instance's)
	watcher bool            // viewportWatcher started
}

// primePending holds tmux sessions that were JUST created as fresh shells and
// still need their first prompt primed (see primeShellPromptWhenAttached). Set by
// every shell-creating path (markPrimePending) and consumed once by the prime, so
// priming only ever types into a brand-new shell — never an existing one (where an
// Enter could submit a half-typed command) or an agent.
var primePending sync.Map // session name → struct{}

func markPrimePending(session string) { primePending.Store(session, struct{}{}) }

// tabIDRe restricts a tab id to characters safe to drop into the tmux/ttyd argv.
var tabIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tabTermSock(token string) string {
	return filepath.Join(os.TempDir(), "lasso-viewport-"+token+".sock")
}

// tmuxAttachArgv is the argv that attaches a ttyd to a tmux session, carrying our
// private socket + no-user-config flags (see tmuxio.go). The viewport attaches to
// the per-instance park session; viewportWatcher then switch-clients it onto the
// selected tab. A reconnecting ttyd re-runs this exact argv, so it always lands
// back on park where the watcher re-adopts it.
func tmuxAttachArgv(session string) []string {
	return append([]string{"tmux"}, append(tmuxPrefix(), "attach", "-t", session)...)
}

// serveTabTerm (POST /api/tab/term {tab_id}) ensures the viewport ttyd exists and
// points it at tab_id's session, returning the (stable) proxy base path. An empty
// tab_id just warms the viewport (used at app load to pay the attach handshake
// before any tab is selected). The frontend keeps a single iframe on this base
// and re-POSTs on every tab switch — the iframe is never recreated.
func serveTabTerm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TabID string `json:"tab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.TabID != "" && !tabIDRe.MatchString(req.TabID) {
		http.Error(w, "valid tab_id required", http.StatusBadRequest)
		return
	}
	// A tab on a REMOTE host can't be shown through the local warm viewport:
	// tmux `switch-client` only moves a client among sessions on the SAME tmux
	// server. So remote tabs get their own ttyd that `ssh -tt`-attaches the remote
	// session directly (ensureRemoteViewport), respawned when the wanted session
	// changes. Local tabs keep the warm, switch-client'd viewport.
	if req.TabID != "" {
		if host := tabHost(req.TabID); host != "" {
			session, _, err := ensureTabSession(req.TabID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			base, err := ensureRemoteViewport(host, session)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			writeJSON(w, map[string]any{"base": base})
			return
		}
	}

	base, err := ensureViewport()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if req.TabID != "" {
		session, _, err := ensureTabSession(req.TabID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		viewport.mu.Lock()
		viewport.want = session
		viewport.mu.Unlock()
		// Prime the first frame once a client lands on the session (the watcher
		// switches one over within a tick). Driven on EVERY switch, not only when
		// WE created the session: a new tab's session is usually created by
		// serveNewTab/createAgent/createWorkspace before the viewport ever points
		// here, so gating on "created" would skip priming exactly the fresh shells
		// that need it. Both primers are self-gating — a session that already
		// painted is left untouched (switch-client already streamed it).
		if t, err := getTab(req.TabID); err == nil && t.Kind == "agent" {
			go nudgeRedrawWhenAttached(session)
		} else {
			go primeShellPromptWhenAttached(session)
		}
	}
	writeJSON(w, map[string]any{"base": base})
}

// ensureViewport spawns the single viewport ttyd (attached to this instance's
// park session) if it isn't running, and starts the watcher. Idempotent; respawns
// with a fresh token if the previous ttyd died (its socket is gone).
func ensureViewport() (string, error) {
	viewport.mu.Lock()
	defer viewport.mu.Unlock()
	if viewport.token != "" {
		if _, err := os.Stat(viewport.sock); err == nil {
			return viewport.base, nil // still alive
		}
		// Dead ttyd — tear down and respawn below.
		if viewport.cancel != nil {
			viewport.cancel()
		}
		viewport.token, viewport.base, viewport.proxy = "", "", nil
		viewport.clients = nil
	}
	if err := tmuxEnsurePark(); err != nil {
		return "", err
	}

	var tok [9]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(tok[:])
	sock := tabTermSock(token)
	basePath := "/tab-term/" + token

	ctx, cancel := context.WithCancel(srvCtx)
	if err := startTtydArgv(ctx, sock, basePath, tmuxAttachArgv(tmuxParkSession()), nil); err != nil {
		cancel()
		return "", err
	}
	waitSocket(sock, true, 3*time.Second)

	viewport.token = token
	viewport.sock = sock
	viewport.base = basePath + "/"
	viewport.proxy = unixSocketProxy(sock)
	viewport.cancel = cancel
	viewport.clients = map[string]bool{}
	if !viewport.watcher {
		viewport.watcher = true
		go viewportWatcher()
	}
	return viewport.base, nil
}

// viewportWatcher keeps this instance's client(s) glued to the wanted session: it
// adopts a client that (re)connected onto our park and switches any of our
// clients not already on `want` over to it. Polling (not event-driven) because
// tmux gives no client-attach signal; only switches a client that's OFF-target,
// so it never causes redundant repaints/flicker on the steady state.
func viewportWatcher() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	park := tmuxParkSession()
	for {
		select {
		case <-srvCtx.Done():
			return
		case <-t.C:
		}
		// Keep the park session alive. The viewport ttyd runs `tmux attach -t
		// park`; if park ever dies the attach exits and the browser is stuck in a
		// "Reconnecting…" loop with nothing to attach to. Recreating it here lets
		// ttyd's next reconnect succeed (and gives a switched-away client a home to
		// fall back to, so killing a tab can't strand the viewport).
		_ = tmuxEnsurePark()
		viewport.mu.Lock()
		want := viewport.want
		viewport.mu.Unlock()
		if want == "" || !tmuxHasSession(want) {
			continue
		}
		sessByTTY := tmuxClientSessions()
		viewport.mu.Lock()
		if viewport.clients == nil {
			viewport.clients = map[string]bool{}
		}
		// Adopt fresh/reconnected clients sitting on our park.
		for tty, sess := range sessByTTY {
			if sess == park {
				viewport.clients[tty] = true
			}
		}
		// Re-point any of our clients not already viewing `want`; drop gone ones.
		for tty := range viewport.clients {
			sess, ok := sessByTTY[tty]
			if !ok {
				delete(viewport.clients, tty)
				continue
			}
			if sess != want {
				_ = tmuxSwitchClient(tty, want)
			}
		}
		viewport.mu.Unlock()
	}
}

// forceViewportRedraw makes every client currently viewing session take a clean
// full frame of it, by bouncing it to the park session and straight back.
// switch-client always pushes a full redraw (unlike an incremental stream to an
// already-attached client, which tmux delivers unreliably) — this is the same
// mechanism that makes switching to an existing tab render correctly. Used after
// a fresh shell's prompt is primed into the grid.
func forceViewportRedraw(session string) {
	park := tmuxParkSession()
	if park == session {
		return
	}
	_ = tmuxEnsurePark() // bounce target must exist
	for tty, sess := range tmuxClientSessions() {
		if sess == session {
			_ = tmuxSwitchClient(tty, park)
			_ = tmuxSwitchClient(tty, session)
		}
	}
}

// serveTabReady (POST /api/tab/term-ready {tab_id}) reports whether a tab's
// session has painted content yet (its prompt / agent frame). The frontend polls
// this after switching to a tab to drive the "starting…" loading overlay, so a
// freshly created shell shows a spinner during its rc boot instead of a blank
// pane with a bare cursor.
func serveTabReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TabID string `json:"tab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ready := false
	if req.TabID != "" {
		if out, err := tmuxCapture(tabSession(req.TabID)); err == nil && strings.TrimSpace(out) != "" {
			ready = true
		}
	}
	writeJSON(w, map[string]any{"ready": ready})
}

// serveTabTermProxy routes /tab-term/<token>/… to the viewport ttyd (HTTP + WS).
func serveTabTermProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/tab-term/")
	token := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		token = rest[:i]
	}
	viewport.mu.Lock()
	var proxy *httputil.ReverseProxy
	if token == viewport.token {
		proxy = viewport.proxy
	}
	viewport.mu.Unlock()
	if proxy == nil { // not the local viewport — try the remote viewport
		remoteViewport.mu.Lock()
		if token == remoteViewport.token {
			proxy = remoteViewport.proxy
		}
		remoteViewport.mu.Unlock()
	}
	if proxy == nil {
		http.NotFound(w, r)
		return
	}
	proxy.ServeHTTP(w, r)
}

// --- the remote viewport: one ttyd that ssh-attaches a remote tab's session ----
//
// A remote tab can't ride the warm local viewport (switch-client can't cross tmux
// servers). Instead a single dedicated ttyd runs `ssh -tt host 'tmux … attach -t
// <session>'`. It's respawned whenever the wanted remote session changes (a fresh
// token/sock each time), so the frontend re-points its iframe at the new base.
// Only one is kept at a time — switching back to a local tab leaves it idle until
// the next remote switch reclaims it. Slower than the local warm path (each
// remote switch pays the attach handshake), but remote is inherently slower.
var remoteViewport struct {
	mu      sync.Mutex
	token   string
	sock    string
	base    string
	proxy   *httputil.ReverseProxy
	cancel  context.CancelFunc
	host    string // host the current attach targets
	session string // session the current attach targets
}

// ensureRemoteViewport makes the remote-viewport ttyd attach host:session,
// respawning it if it's pointed elsewhere or dead, and returns its proxy base.
func ensureRemoteViewport(host, session string) (string, error) {
	remoteViewport.mu.Lock()
	defer remoteViewport.mu.Unlock()

	if remoteViewport.token != "" && remoteViewport.host == host && remoteViewport.session == session {
		if _, err := os.Stat(remoteViewport.sock); err == nil {
			return remoteViewport.base, nil // still attached to the right session
		}
	}
	// Tear down whatever's there (wrong target or dead ttyd).
	if remoteViewport.cancel != nil {
		remoteViewport.cancel()
		remoteViewport.cancel = nil
	}
	remoteViewport.token, remoteViewport.base, remoteViewport.proxy = "", "", nil

	be, err := hostBackend(host)
	if err != nil {
		return "", err
	}

	var tok [9]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(tok[:])
	sock := tabTermSock(token)
	basePath := "/tab-term/" + token

	ctx, cancel := context.WithCancel(srvCtx)
	if err := startTtydArgv(ctx, sock, basePath, be.TmuxAttachArgv(session), nil); err != nil {
		cancel()
		return "", err
	}
	waitSocket(sock, true, 3*time.Second)

	remoteViewport.token = token
	remoteViewport.sock = sock
	remoteViewport.base = basePath + "/"
	remoteViewport.proxy = unixSocketProxy(sock)
	remoteViewport.cancel = cancel
	remoteViewport.host = host
	remoteViewport.session = session
	return remoteViewport.base, nil
}

// serveTabTermTouch (POST /api/tab/term-touch) reports whether the viewport ttyd
// is still alive, so the frontend can re-POST /api/tab/term to respawn it (and
// pick up the new base) if it died — e.g. after a crash. tab_id is ignored now
// (one viewport, not a per-tab pool) but kept for request-shape compatibility.
func serveTabTermTouch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	viewport.mu.Lock()
	alive := viewport.token != ""
	sock := viewport.sock
	viewport.mu.Unlock()
	if alive {
		if _, err := os.Stat(sock); err != nil {
			alive = false
		}
	}
	writeJSON(w, map[string]any{"alive": alive})
}
