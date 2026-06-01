package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
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

// switchMu serializes host switches: a switch tears down and rebuilds the herdr
// subscription and both terminals, so two in flight at once would race. A second
// concurrent request gets 409 (the footer also disables its control while one is
// pending).
var switchMu sync.Mutex

// ---------------------------------------------------------------------------
// ttyd manager — one per terminal role, supports respawn on host switch
// ---------------------------------------------------------------------------

// ttydManager owns one ttyd terminal (a fixed proxy socket + base path) and can
// restart it with a new command/env when the active host changes. The proxy
// (unixSocketProxy) always dials the same socket path, so a respawn under the
// same path is transparent to the HTTP routing — only the iframe needs a reload.
type ttydManager struct {
	parent   context.Context
	sock     string
	basePath string

	mu     sync.Mutex
	cancel context.CancelFunc // cancels the currently-running instance
}

func newTtydManager(parent context.Context, sock, basePath string) *ttydManager {
	return &ttydManager{parent: parent, sock: sock, basePath: basePath}
}

// restart kills the current ttyd (if any), waits for it to release the socket,
// then spawns a fresh one running command with env on the same socket path.
func (m *ttydManager) restart(command string, env []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
		// Wait for the old instance's deferred socket removal so the new ttyd
		// binds cleanly (and the old cleanup can't unlink the new socket).
		waitSocket(m.sock, false, 3*time.Second)
		m.cancel = nil
	}
	ctx, cancel := context.WithCancel(m.parent)
	if err := startTtyd(ctx, m.sock, m.basePath, command, env); err != nil {
		cancel()
		return err
	}
	m.cancel = cancel
	// Give ttyd a moment to bind so the first proxied request doesn't 502.
	waitSocket(m.sock, true, 3*time.Second)
	return nil
}

// terminals holds the two ttyd managers (nil when -spawn-ttyd=false). On a host
// switch both are restarted with the new host's commands.
var terminals struct {
	herdr *ttydManager // left "Herdr" terminal (/terminal)
	shell *ttydManager // right shell tab (/shell)
}

// applyBackendToTerminals respawns both terminals for backend b: the left
// terminal runs b.TermCmd() (local herdr, or `herdr --remote <host>`); the shell
// tab runs b.ShellCmd() (local shell, or `ssh <host>`) with the herdr session
// markers stripped, as before.
func applyBackendToTerminals(b Backend) {
	if terminals.herdr != nil {
		if err := terminals.herdr.restart(termPrefix()+b.TermCmd(), b.TermEnv()); err != nil {
			log.Printf("ttyd (terminal) restart: %v", err)
		}
	}
	if terminals.shell != nil {
		if err := terminals.shell.restart(b.ShellCmd(), outsideHerdrEnv()); err != nil {
			log.Printf("ttyd (shell) restart: %v", err)
		}
	}
}

// waitSocket polls until the socket file exists (want=true) or is gone
// (want=false), or the timeout elapses.
func waitSocket(sock string, want bool, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := os.Stat(sock)
		if (err == nil) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
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

	// Build the new backend. On failure, prev stays active (the half-built remote
	// is torn down inside newRemoteBackend) — the caller is unaffected.
	var newB Backend
	if target == "local" {
		newB = &localBackend{sock: *herdrSock}
	} else {
		hi, ok := findHost(target)
		if !ok || !hi.Reachable || !hi.Running || !hi.Compatible {
			http.Error(w, "host not available (no compatible herdr server)", http.StatusBadRequest)
			return
		}
		_, wantProto := localProtocol()
		rb, err := newRemoteBackend(srvCtx, target, hi.Socket, wantProto)
		if err != nil {
			http.Error(w, "connect "+target+": "+err.Error(), http.StatusBadGateway)
			return
		}
		newB = rb
	}

	// Swap, then re-point every host-bound subsystem at the new backend.
	setBackend(newB)
	invalidatePaneList()          // drop stale pane data from the old host
	applyBackendToTerminals(newB) // respawn ttyd terminals on the new host
	if srvHub != nil {
		srvHub.startSub()    // re-subscribe events against the new socket
		srvHub.bumpTermRev() // tell the browser to reload the terminal iframes
		srvHub.kick()        // push fresh state without waiting for the poll tick
	}

	// Tear down the previous backend after a short grace so in-flight requests
	// that captured it finish first. Local Close is a no-op.
	if prev != nil {
		go func() {
			time.Sleep(2 * time.Second)
			_ = prev.Close()
		}()
	}

	log.Printf("host:     switched to %s", newB.Name())
	writeHostResult(w, newB)
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
