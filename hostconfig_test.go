package main

import (
	"strings"
	"testing"
)

// addRepo registers an absolute repo dir (with a .git subdir) in the mem fs.
func (b *memBackend) addRepo(path string) { b.mkdirAllAncestors(path + "/.git") }

// addDir registers a plain (non-git) directory.
func (b *memBackend) addDir(path string) { b.mkdirAllAncestors(path) }

func names(repos []repoEntry) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r.Name
	}
	return out
}

func TestReposListWithMultipleRoots(t *testing.T) {
	be := newMemBackend()
	be.addRepo("/root1/alpha")
	be.addRepo("/root1/beta")
	be.addDir("/root1/notgit") // no .git — skipped
	be.addRepo("/root2/gamma")

	// Two directories, one per line, plus an unreadable/empty extra root.
	_, repos := reposListWith(be, "/root1\n/root2\n/does-not-exist", nil)
	got := strings.Join(names(repos), ",")
	if got != "alpha,beta,gamma" {
		t.Errorf("repos = %q, want alpha,beta,gamma", got)
	}
}

func TestReposListWithDedupesRepeatedRoot(t *testing.T) {
	be := newMemBackend()
	be.addRepo("/root1/alpha")

	// The same root listed twice must not duplicate the repo.
	_, repos := reposListWith(be, "/root1\n/root1", nil)
	if len(repos) != 1 || repos[0].Name != "alpha" {
		t.Errorf("repos = %v, want a single alpha", names(repos))
	}
}

func TestSplitReposRoots(t *testing.T) {
	got := splitReposRoots("  ~/projects \n\n ~/work \n")
	if len(got) != 2 || got[0] != "~/projects" || got[1] != "~/work" {
		t.Errorf("splitReposRoots = %v, want [~/projects ~/work]", got)
	}
	// A legacy single-path value yields exactly that path.
	if got := splitReposRoots("~/projects"); len(got) != 1 || got[0] != "~/projects" {
		t.Errorf("splitReposRoots single = %v", got)
	}
}
