package main

import (
	"os/exec"
	"testing"
)

// git runs a git command in dir and fails the test on error, returning trimmed
// stdout.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutput(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return out
}

// newRepo makes a temp git repo with `main` as the default branch and one
// commit, returning its path and that commit's full sha.
func newRepo(t *testing.T) (dir, head string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir = t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "t@t")
	git(t, dir, "config", "user.name", "t")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	return dir, git(t, dir, "rev-parse", "HEAD")
}

// TestUpdateStateFrom exercises the behind/current/unknown comparison without a
// real build stamp.
func TestUpdateStateFrom(t *testing.T) {
	dir, first := newRepo(t)

	// Built from main's only commit → current, nothing behind.
	if st, n := updateStateFrom(first, false, true, dir); st != "current" || n != 0 {
		t.Errorf("on tip: state=%q behind=%d, want current/0", st, n)
	}

	// Advance main by two commits; the old build is now 2 behind → available.
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "second")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "third")
	tip := git(t, dir, "rev-parse", "HEAD")
	if st, n := updateStateFrom(first, false, true, dir); st != "available" || n != 2 {
		t.Errorf("behind: state=%q behind=%d, want available/2", st, n)
	}
	// Built from the new tip → current again.
	if st, _ := updateStateFrom(tip, false, true, dir); st != "current" {
		t.Errorf("on new tip: state=%q, want current", st)
	}

	// No VCS stamp, or a dirty build, can't be compared → unknown (UI keeps the
	// button as an escape hatch).
	if st, _ := updateStateFrom("", false, false, dir); st != "unknown" {
		t.Errorf("no stamp: state=%q, want unknown", st)
	}
	if st, _ := updateStateFrom(first, true, true, dir); st != "unknown" {
		t.Errorf("dirty build: state=%q, want unknown", st)
	}

	// A commit unknown to the repo isn't an ancestor of main → not offered as an
	// update (treated as current: nothing on main to move forward to).
	bogus := "0000000000000000000000000000000000000000"
	if st, _ := updateStateFrom(bogus, false, true, dir); st != "current" {
		t.Errorf("unknown commit: state=%q, want current", st)
	}

	// A bad source path is a git error → unknown.
	if st, _ := updateStateFrom(first, false, true, t.TempDir()); st != "unknown" {
		t.Errorf("non-repo dir: state=%q, want unknown", st)
	}
}
