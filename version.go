package main

import "runtime/debug"

// lassoSemver is lasso's own version — the single source of truth, bumped by
// hand in a commit (the `mise run bump` task edits this line). It's independent
// of lassoHerdrProtocol (which tracks the herdr socket wire format, not lasso's
// release). Bump it when shipping a notable change; the build commit is appended
// automatically (see lassoVersion).
//
// A var (not const) so release builds can stamp the exact tag via
// `-ldflags "-X main.lassoSemver=<version>"` (see .github/workflows/release.yml);
// the committed value is the source/dev default.
var lassoSemver = "2.3.3"

// lassoVersion is the human-facing build identity: the hand-set semver plus the
// exact commit it was built from, e.g. "0.1.0 (dc1e696)". The semver says
// "which release", the commit says "which build of it" — useful when several
// builds share a version (a dirty rebuild, a hotfix between bumps). Shown in the
// Settings tab and host switcher.
func lassoVersion() string {
	return lassoSemver + " (" + lassoCommit() + ")"
}

// buildCommit reports the full commit revision this binary was built from and
// whether the working tree was dirty, read from the Go VCS stamp (`go build`
// records vcs.revision/vcs.modified automatically). ok is false when no stamp is
// present — `go run`, and linked git worktrees, which Go deliberately doesn't
// stamp. The full revision (not the short form) is what the update check compares
// against `main`'s tip.
func buildCommit() (rev string, dirty, ok bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false, false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "", false, false
	}
	return rev, dirty, true
}

// lassoCommit is the short, human-facing form of buildCommit: the 12-char
// revision, suffixed "-dirty" if the tree had uncommitted changes, or "dev" when
// no stamp is present (e.g. `go run`).
func lassoCommit() string {
	rev, dirty, ok := buildCommit()
	if !ok {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
