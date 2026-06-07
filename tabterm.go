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
// reconnect. This is how herdr's terminal felt fast: one terminal, switched.
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
	base, err := ensureViewport()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if req.TabID != "" {
		session, created, err := ensureTabSession(req.TabID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		viewport.mu.Lock()
		viewport.want = session
		viewport.mu.Unlock()
		// A freshly created session is a blank shell/agent; prime its first frame
		// once a client lands on it (the watcher switches one over within a tick).
		if created {
			if t, err := getTab(req.TabID); err == nil && t.Kind == "agent" {
				go nudgeRedrawWhenAttached(session)
			} else {
				go primeShellPromptWhenAttached(session)
			}
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
	if proxy == nil {
		http.NotFound(w, r)
		return
	}
	proxy.ServeHTTP(w, r)
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
