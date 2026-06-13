package main

import (
	"net/http"
	"os"
	"os/exec"
	"syscall"
)

// Lasso self-update over HTTP. The Settings tab surfaces "update available" by
// polling /api/version (serveVersion compares the running version to the latest
// GitHub release); the actual update is `lasso update`, which this endpoint runs
// detached so the server can be replaced + restarted out from under the request.

// selfUpdateAvailable reports whether lasso can update + restart itself here: a
// non-dev install supervised by a registered pitchfork daemon. Dev/worktree runs
// return false so /api/self-update refuses cleanly.
func selfUpdateAvailable() bool {
	if *devMode {
		return false
	}
	return pitchforkRegistered(lassoDaemon())
}

// serveSelfUpdate kicks off a detached `lasso update` so the running server can be
// replaced + restarted by pitchfork without the in-flight request killing the
// updater. Returns immediately; the client sees the server bounce a moment later.
func serveSelfUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !selfUpdateAvailable() {
		http.Error(w, "lasso isn't a self-updatable install here (no pitchfork-supervised daemon) — "+
			"update it the way it was deployed", http.StatusConflict)
		return
	}
	self, err := os.Executable()
	if err != nil {
		http.Error(w, "locate lasso: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Detach into a new session so `pitchfork restart` (which SIGTERMs this very
	// server) can't take the updater down mid-flight. Output is discarded — the
	// caller is about to be restarted and pitchfork logs capture the rest.
	cmd := exec.Command(self, "update")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		http.Error(w, "start updater: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = cmd.Process.Release()

	writeJSON(w, map[string]any{"started": true, "daemon": lassoDaemon()})
}
