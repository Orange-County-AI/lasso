package main

import (
	"encoding/json"
	"net/http"
	"sync"
)

// Host switching changes which host the file browser, diff view, repo picker and
// new-agent target operate on. lasso drives the chosen host's tmux/files/git over
// SSH (the Backend); the active backend is swapped atomically. Already-open tabs
// keep working because tmux routing is per-session (sessionHosts), independent of
// the active backend — switching only redirects NEW work and the file/diff panes.

var switchMu sync.Mutex

// serveHostSwitch (POST /api/host {host}) makes host the active backend. "local"
// (or this machine's hostname) selects the local backend; any other name is an
// ssh-config alias resolved to a pooled remote connection. On success it bumps
// the terminal-reload revision so browsers reload their iframes onto the new host.
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

	switchMu.Lock()
	defer switchMu.Unlock()

	if curBackend().Name() == hostOrLocal(req.Host) || (isLocalHost(req.Host) && isLocalHost(curBackend().Name())) {
		writeJSON(w, map[string]any{"active": curBackend().Name()})
		return
	}

	if isLocalHost(req.Host) {
		setBackend(localBE)
	} else {
		// Refuse a host we haven't probed as usable, so a stray alias can't make us
		// open an SSH connection to an arbitrary box. The alias rides ssh's argv
		// (not a shell), so it can't inject a command.
		if hi, ok := findHost(req.Host); ok && !hi.usable() {
			http.Error(w, "host not reachable / no tmux", http.StatusBadGateway)
			return
		}
		be, err := hostBackend(req.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		setBackend(be)
	}

	if srvHub != nil {
		srvHub.bumpTermRev()
	}
	writeJSON(w, map[string]any{"active": curBackend().Name()})
}
