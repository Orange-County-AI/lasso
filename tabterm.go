package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Per-tab terminals: a dynamic pool of ttyds, each attached to one tab's tmux
// session (`tmux -S ~/.lasso/tmux.sock attach -t lasso_<tabID>`), proxied under
// /tab-term/<token>/. This replaced herdr's per-pane grid terminals (grid.go).
//
// Reaping a ttyd only DETACHES the viewer — the tmux session keeps running
// (destroy-unattached off), so an agent survives the cell being hidden, the Grid
// being left, and lasso itself restarting. ensureTabSession lazily recreates a
// session (a fresh shell) if it's gone, e.g. after a reboot.

type tabTermEntry struct {
	token    string
	sock     string
	base     string // "/tab-term/<token>/"
	proxy    *httputil.ReverseProxy
	cancel   context.CancelFunc
	lastUsed time.Time
}

var tabTerms struct {
	mu      sync.Mutex
	byTab   map[string]*tabTermEntry // tab id → entry
	byToken map[string]*tabTermEntry // token → entry
}

const (
	tabTermIdle = 30 * time.Second
	tabTermMax  = 48 // backstop; lazy viewport-mounting keeps the real count low
)

// tabIDRe restricts a tab id to characters safe to drop into the tmux/ttyd argv.
var tabIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tabTermSock(token string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("lasso-tabterm-%d-%s.sock", os.Getpid(), token))
}

// tmuxAttachArgv is the argv that attaches a ttyd to a tmux session, carrying our
// private socket + no-user-config flags (see tmuxio.go).
//
// We deliberately do NOT pass `-d` (detach-others): two legitimate viewers of the
// same session (a second browser tab/device) would then fight — each attach kicks
// the other, which reconnects and kicks back, flapping forever. Stale/orphaned
// clients that linger after a hard backend restart are handled by `window-size
// largest` instead (see tmuxEnsureServer): a small orphan is simply ignored.
func tmuxAttachArgv(session string) []string {
	return append([]string{"tmux"}, append(tmuxPrefix(), "attach", "-t", session)...)
}

// serveTabTerm (POST /api/tab/term {tab_id}) returns the proxy base path for a
// tab's terminal, spawning its ttyd (and lazily recreating the tmux session) on
// first use. A repeat call just bumps the idle timer, so the frontend re-POSTs as
// a keepalive.
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
	if !tabIDRe.MatchString(req.TabID) {
		http.Error(w, "valid tab_id required", http.StatusBadRequest)
		return
	}
	base, err := ensureTabTerm(req.TabID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"base": base})
}

func ensureTabTerm(tabID string) (string, error) {
	tabTerms.mu.Lock()
	defer tabTerms.mu.Unlock()
	if tabTerms.byTab == nil {
		tabTerms.byTab = map[string]*tabTermEntry{}
		tabTerms.byToken = map[string]*tabTermEntry{}
	}
	if e := tabTerms.byTab[tabID]; e != nil {
		e.lastUsed = time.Now()
		return e.base, nil
	}
	if len(tabTerms.byTab) >= tabTermMax {
		reapTabTermsLocked()
		if len(tabTerms.byTab) >= tabTermMax {
			return "", fmt.Errorf("too many live terminals (max %d)", tabTermMax)
		}
	}
	// Make sure the tmux session exists (recreates a fresh shell after a reboot).
	session, err := ensureTabSession(tabID)
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
	if err := startTtydArgv(ctx, sock, basePath, tmuxAttachArgv(session), nil); err != nil {
		cancel()
		return "", err
	}
	waitSocket(sock, true, 3*time.Second)
	// Once the browser attaches, force a repaint: the attach resizes the session
	// mid-startup and the SIGWINCH eats the shell's first prompt / agent's first
	// frame, leaving the pane blank until the user types (see nudgeRedraw).
	go nudgeRedrawWhenAttached(session)
	// A bare shell's prompt (starship) won't paint until the shell processes an
	// input event with a client attached to answer its terminal queries — a resize
	// isn't enough. Prime SHELL tabs with an Enter after attach. Never agent tabs:
	// the Enter could submit a half-typed agent command. (Uses the stored kind, so
	// a reconciled agent-turned-shell after a reboot isn't primed — rare; the user
	// can press a key. We deliberately don't probe the live process, which races a
	// freshly-launched agent.)
	if t, err := getTab(tabID); err == nil && t.Kind != "agent" {
		go primeShellPromptWhenAttached(session)
	}

	e := &tabTermEntry{
		token:    token,
		sock:     sock,
		base:     basePath + "/",
		proxy:    unixSocketProxy(sock),
		cancel:   cancel,
		lastUsed: time.Now(),
	}
	tabTerms.byTab[tabID] = e
	tabTerms.byToken[token] = e
	startTabTermReaper()
	return e.base, nil
}

// serveTabTermProxy routes /tab-term/<token>/… to that tab's ttyd (HTTP + WS).
func serveTabTermProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/tab-term/")
	token := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		token = rest[:i]
	}
	tabTerms.mu.Lock()
	e := tabTerms.byToken[token]
	if e != nil {
		e.lastUsed = time.Now() // proxied traffic keeps an actively-viewed cell alive
	}
	tabTerms.mu.Unlock()
	if e == nil {
		http.NotFound(w, r)
		return
	}
	e.proxy.ServeHTTP(w, r)
}

// serveTabTermTouch (POST /api/tab/term-touch {tab_id}) bumps a live terminal's
// idle timer WITHOUT spawning one — so a keepalive can't resurrect a just-
// released attach. Returns {alive}.
func serveTabTermTouch(w http.ResponseWriter, r *http.Request) {
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
	tabTerms.mu.Lock()
	alive := false
	if e := tabTerms.byTab[req.TabID]; e != nil {
		e.lastUsed = time.Now()
		alive = true
	}
	tabTerms.mu.Unlock()
	writeJSON(w, map[string]any{"alive": alive})
}

// serveTabTermRelease (POST /api/tab/term-release {tab_id}) detaches a tab's
// viewer (kills its ttyd) when the cell leaves the viewport, so a hidden thin
// attach doesn't clamp the session's width while it's viewed elsewhere. The tmux
// session keeps running. Best-effort.
func serveTabTermRelease(w http.ResponseWriter, r *http.Request) {
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
	releaseTabTerm(req.TabID)
	writeJSON(w, map[string]any{"ok": true})
}

func releaseTabTerm(tabID string) {
	tabTerms.mu.Lock()
	e := tabTerms.byTab[tabID]
	if e != nil {
		delete(tabTerms.byTab, tabID)
		delete(tabTerms.byToken, e.token)
	}
	tabTerms.mu.Unlock()
	if e != nil {
		e.cancel() // SIGTERMs the ttyd (detaches the tmux client); session lives on
	}
}

func reapTabTermsLocked() {
	now := time.Now()
	for tabID, e := range tabTerms.byTab {
		if now.Sub(e.lastUsed) > tabTermIdle {
			e.cancel()
			delete(tabTerms.byTab, tabID)
			delete(tabTerms.byToken, e.token)
		}
	}
}

var tabTermReaperOnce sync.Once

func startTabTermReaper() {
	tabTermReaperOnce.Do(func() {
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-srvCtx.Done():
					return
				case <-t.C:
					tabTerms.mu.Lock()
					reapTabTermsLocked()
					tabTerms.mu.Unlock()
				}
			}
		}()
	})
}
