package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Lasso self-update. Unlike herdr (which runs on each host and is updated there
// via the host switcher's "Update"), lasso runs only on the local machine, as a
// systemd --user service. New features change behavior and can shift the herdr
// protocol lasso targets, so the host switcher also offers "Update lasso": pull
// the latest source and let the supervisor rebuild + restart it, bringing lasso
// in line with a host running a newer herdr.
//
// The supervised source install is a systemd --user unit (default name "lasso")
// whose start command does `git checkout main; go build; exec ./lasso` from the
// source checkout. So updating is: `git pull --ff-only` in that checkout, then
// `systemctl --user restart <unit>` — the restart rebuilds the pulled code. The
// restart SIGTERMs this very process (and everything in the unit's cgroup), so
// the updater must run outside the unit to outlive it.

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

// lassoUnit is the systemd --user unit name to restart (override for
// non-default deployments via LASSO_SYSTEMD_UNIT).
func lassoUnit() string {
	if d := os.Getenv("LASSO_SYSTEMD_UNIT"); d != "" {
		return d
	}
	return "lasso"
}

// selfUpdateAvailable reports whether this looks like the supervised prod
// install: a git checkout run by a systemd --user unit. Dev/worktree runs
// (no systemd unit, or running from `go run`) return false so the UI can hide
// the action and the endpoint can refuse cleanly.
func selfUpdateAvailable() bool {
	// Never self-update a dev instance: it's served by Vite with hot reload and
	// its binary often lives in a throwaway worktree, so a pull+restart would
	// rebuild the wrong tree (and bounce the prod daemon).
	if *devMode {
		return false
	}
	// Deliberately switched off for this deployment (fleet boxes: an agent must
	// not be able to git-pull and restart its own front door).
	if *disableSelfUpdate {
		return false
	}
	src := lassoSrcDir()
	if src == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		return false
	}
	sc, err := exec.LookPath("systemctl")
	if err != nil {
		return false
	}
	// is-active exits non-zero when the unit doesn't exist or isn't running —
	// and the supervised install is by definition running (it's serving us).
	return exec.Command(sc, "--user", "is-active", "--quiet", lassoUnit()).Run() == nil
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

// serveSelfUpdate kicks off a detached "git pull + systemctl --user restart" so
// lasso rebuilds itself from the latest source. Returns immediately; the client
// sees the server bounce a moment later.
func serveSelfUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if *disableSelfUpdate {
		http.Error(w, "self-update is disabled on this instance (-disable-self-update)", http.StatusForbidden)
		return
	}
	if !selfUpdateAvailable() {
		http.Error(w, "lasso isn't a self-updatable install here (no systemd-supervised git checkout) — "+
			"update it the way it was deployed", http.StatusConflict)
		return
	}
	src := lassoSrcDir()
	unit := lassoUnit()

	// One transient systemd unit does the whole update. A plain forked child
	// (even setsid'd) would still live in this service's cgroup, and
	// `systemctl --user restart` kills the whole cgroup — taking the updater
	// down mid-flight. systemd-run asks the user manager to spawn the updater
	// in its own unit, outside ours, so it survives the restart. Output is
	// discarded — the caller is about to be restarted and the journal captures
	// the rebuild.
	script := fmt.Sprintf(
		"git -C %s pull --ff-only && systemctl --user restart %s",
		shellQuote(src), shellQuote(unit))
	cmd := exec.Command("systemd-run", "--user", "--collect", "--quiet", "sh", "-c", script)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Run(); err != nil {
		http.Error(w, "start updater: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{"started": true, "src": src, "unit": unit})
}
