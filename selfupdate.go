package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Lasso self-update. Unlike herdr (which runs on each host and is updated there
// via the host switcher's "Update"), lasso runs only on the local machine, as a
// pitchfork daemon. New features change behavior and can shift the herdr
// protocol lasso targets, so the host switcher also offers "Update lasso": pull
// the latest source and let the supervisor rebuild + restart it, bringing lasso
// in line with a host running a newer herdr.
//
// The prod install is a pitchfork daemon (default name "lasso") whose run script
// does `git checkout main; go build; exec ./lasso` from the source checkout. So
// updating is: `git pull --ff-only` in that checkout, then `pitchfork restart
// <daemon>` — the restart rebuilds the pulled code. The restart SIGTERMs this
// very process, so the updater must be fully detached to outlive it.

// lassoSrcDir is the source checkout to update: LASSO_SRC_DIR if set, else the
// directory holding the running binary (prod builds to <checkout>/lasso).
func lassoSrcDir() string {
	if d := os.Getenv("LASSO_SRC_DIR"); d != "" {
		return d
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// lassoDaemon is the pitchfork daemon name to restart (override for non-default
// deployments via LASSO_PITCHFORK_DAEMON).
func lassoDaemon() string {
	if d := os.Getenv("LASSO_PITCHFORK_DAEMON"); d != "" {
		return d
	}
	return "lasso"
}

// selfUpdateAvailable reports whether this looks like the supervised prod
// install: a git checkout supervised by a pitchfork daemon. Dev/worktree runs
// (no pitchfork, or running from `go run`) return false so the UI can hide the
// action and the endpoint can refuse cleanly.
func selfUpdateAvailable() bool {
	// Never self-update a dev instance: it's served by Vite with hot reload and
	// its binary often lives in a throwaway worktree, so a pull+restart would
	// rebuild the wrong tree (and bounce the prod daemon).
	if *devMode {
		return false
	}
	src := lassoSrcDir()
	if src == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		return false
	}
	pf, err := exec.LookPath("pitchfork")
	if err != nil {
		return false
	}
	// `pitchfork status <daemon>` exits non-zero if the daemon isn't registered.
	return exec.Command(pf, "status", lassoDaemon()).Run() == nil
}

// selfUpdateStatus reports whether a newer lasso is waiting to be built, so the
// UI can show "update lasso" only when it would do something. The supervisor
// (lasso-serve) does `git checkout main; go build` on restart, so the running
// binary is stale exactly when its build commit is behind the tip of `main` in
// the source checkout. All git here is local (no fetch) — cheap enough to run
// per /api/version request, which the UI only fires while its menu is open.
//
// Returns one of:
//   - "available" (+ commits behind): build commit is an ancestor of main's tip.
//   - "current": build commit IS main's tip, or is ahead of / diverged from it
//     (nothing on main to move forward to).
//   - "unknown": can't tell — no VCS stamp (dev/worktree build), a dirty build
//     (running uncommitted code, not a clean main commit), or a git error. The UI
//     falls back to showing the button so the escape hatch never disappears.
func selfUpdateStatus() (state string, behind int) {
	rev, dirty, ok := buildCommit()
	return updateStateFrom(rev, dirty, ok, lassoSrcDir())
}

// updateStateFrom is the testable core of selfUpdateStatus: it takes the running
// build's commit (injected, so tests needn't fake a build stamp) and compares it
// to refs/heads/main in src. It reads the main branch ref directly, so the answer
// is correct even when the checkout is parked on another branch — main is what
// the supervisor builds regardless.
func updateStateFrom(rev string, dirty, hasStamp bool, src string) (state string, behind int) {
	if !hasStamp || dirty || src == "" {
		return "unknown", 0
	}
	mainTip, err := gitOutput(src, "rev-parse", "refs/heads/main")
	if err != nil {
		return "unknown", 0
	}
	if rev == mainTip {
		return "current", 0
	}
	// Only offer an update when main is genuinely ahead — i.e. rev is an ancestor
	// of main's tip. If it isn't (build is ahead of / diverged from main, or rev
	// is unknown to this repo), there's nothing to pull forward to.
	if exec.Command("git", "-C", src, "merge-base", "--is-ancestor", rev, "refs/heads/main").Run() != nil {
		return "current", 0
	}
	if n, err := gitOutput(src, "rev-list", "--count", rev+"..refs/heads/main"); err == nil {
		behind, _ = strconv.Atoi(n)
	}
	return "available", behind
}

// gitOutput runs `git -C dir args...` and returns its trimmed stdout.
func gitOutput(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// serveSelfUpdate kicks off a detached "git pull + pitchfork restart" so lasso
// rebuilds itself from the latest source. Returns immediately; the client sees
// the server bounce a moment later.
func serveSelfUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !selfUpdateAvailable() {
		http.Error(w, "lasso isn't a self-updatable install here (no pitchfork-supervised git checkout) — "+
			"update it the way it was deployed", http.StatusConflict)
		return
	}
	src := lassoSrcDir()
	daemon := lassoDaemon()

	// One detached shell does the whole update. setsid + Setpgid put it in its
	// own session/process group so `pitchfork restart`, which kills this process,
	// can't take the updater down mid-flight. Output is discarded — the caller is
	// about to be restarted and pitchfork logs capture the rebuild.
	script := fmt.Sprintf(
		"git -C %s pull --ff-only && pitchfork restart %s",
		shellQuote(src), shellQuote(daemon))
	cmd := exec.Command("setsid", "sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		http.Error(w, "start updater: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Release so we don't leave a zombie when we're restarted out from under it.
	_ = cmd.Process.Release()

	writeJSON(w, map[string]any{"started": true, "src": src, "daemon": daemon})
}
