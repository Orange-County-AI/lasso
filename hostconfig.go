package main

import (
	"path/filepath"
	"sort"
	"strings"
)

// Creator settings, stored in the local ~/.lasso/lasso.db. "Settings" means the
// creator defaults (repos_root, branch_prefix, default_agent, scratch_setup) and
// per-repo copy-files/setup, plus the agent log and last-used selections. All
// state is keyed as host "local".

// creatorDefaults is the JSON shape of the four creator settings, shared by the
// CLI and the provider. The tags match LassoConfig so either decodes it.
type creatorDefaults struct {
	ReposRoot    string `json:"repos_root"`
	BranchPrefix string `json:"branch_prefix"`
	DefaultAgent string `json:"default_agent"`
	ScratchSetup string `json:"scratch_setup"`
}

// defaultsPatch is the POST body for updating creator defaults; a nil field is
// left unchanged, while a non-nil empty string clears it (e.g. "no preset
// default agent" — fall back to the last-used one).
type defaultsPatch struct {
	ReposRoot    *string `json:"repos_root"`
	BranchPrefix *string `json:"branch_prefix"`
	DefaultAgent *string `json:"default_agent"`
	ScratchSetup *string `json:"scratch_setup"`
}

// repoConfigPatch is the POST body for a repo's per-repo settings.
type repoConfigPatch struct {
	Path      string  `json:"path"`
	CopyFiles *string `json:"copy_files"`
	Setup     *string `json:"setup"`
}

// ---------------------------------------------------------------------------
// core logic — operates on the in-process db (this host's own) + a backend's fs
// ---------------------------------------------------------------------------

// applyDefaults writes the provided default fields to the settings table.
func applyDefaults(p defaultsPatch) error {
	for _, u := range []struct {
		key string
		val *string
	}{
		{"repos_root", p.ReposRoot},
		{"branch_prefix", p.BranchPrefix},
		{"default_agent", p.DefaultAgent},
		{"scratch_setup", p.ScratchSetup},
	} {
		if u.val == nil {
			continue
		}
		if err := setSetting(u.key, *u.val); err != nil {
			return err
		}
	}
	return nil
}

// reposList scans repos_root for git repos on be, merged with host's per-repo
// state from the in-process db.
func reposList(be Backend, host string) (string, []repoEntry, error) {
	s, err := getSettings()
	if err != nil {
		return "", nil, err
	}
	repoState, err := listRepoState(host)
	if err != nil {
		return "", nil, err
	}
	root, repos := reposListWith(be, s.ReposRoot, repoState)
	return root, repos, nil
}

// splitReposRoots splits a repos_root setting into individual directories. The
// value is one directory per line (newline-separated); blank lines and
// surrounding whitespace are dropped. A legacy single-path value is just one
// line, so it still yields exactly that path.
func splitReposRoots(reposRoot string) []string {
	var out []string
	for _, line := range strings.Split(reposRoot, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// reposListWith scans reposRoot on be (one level deep) for git repos and merges
// the already-fetched per-repo state. reposRoot may name several directories
// (one per line); each is scanned and the results merged, deduped by absolute
// path. Splitting the db read out lets the remote path supply repoState from
// sqlite while still scanning the remote fs over be. The returned string is the
// newline-joined expanded roots (informational; the picker uses repos).
func reposListWith(be Backend, reposRoot string, repoState map[string]*RepoConfig) (string, []repoEntry) {
	roots := splitReposRoots(reposRoot)
	expanded := make([]string, 0, len(roots))
	repos := make([]repoEntry, 0)
	seen := map[string]bool{}
	for _, r := range roots {
		root := expandTildeOn(be, r)
		expanded = append(expanded, root)
		ents, err := be.ReadDir(root)
		if err != nil {
			// An unreadable/missing root isn't fatal — skip it so the picker still
			// opens (the user can fix the roots in settings).
			continue
		}
		for _, e := range ents {
			if !e.Dir {
				continue
			}
			repoPath := filepath.Join(root, e.Name)
			if seen[repoPath] {
				continue // same repo reachable from two roots — list it once
			}
			if _, err := be.Stat(filepath.Join(repoPath, ".git")); err != nil {
				continue // not a git repo
			}
			seen[repoPath] = true
			re := repoEntry{Path: repoPath, Name: e.Name}
			if rc := repoState[repoPath]; rc != nil {
				re.CopyFiles, re.Setup, re.LastBaseBranch = rc.CopyFiles, rc.Setup, rc.LastBaseBranch
			}
			repos = append(repos, re)
		}
	}
	// Sort by name; break ties on path since two roots can hold same-named repos.
	sort.Slice(repos, func(i, j int) bool {
		if repos[i].Name != repos[j].Name {
			return repos[i].Name < repos[j].Name
		}
		return repos[i].Path < repos[j].Path
	})
	return strings.Join(expanded, "\n"), repos
}

// applyRepoConfig writes a repo's copy-files/setup for host, returning its state.
func applyRepoConfig(host, path string, copyFiles, setup *string) (RepoConfig, error) {
	if copyFiles != nil {
		if err := setRepoCopyFiles(host, path, *copyFiles); err != nil {
			return RepoConfig{}, err
		}
	}
	if setup != nil {
		if err := setRepoSetup(host, path, *setup); err != nil {
			return RepoConfig{}, err
		}
	}
	return getRepoState(host, path)
}

// branchList returns a repo's local + remote branches and detected default,
// mirroring the old serveRepoBranches body (git runs through be).
func branchList(be Backend, path string) (local, remote []string, def string) {
	local = gitBranchList(be, path, "refs/heads")
	remote = gitBranchList(be, path, "refs/remotes")
	filtered := remote[:0]
	for _, b := range remote {
		if strings.HasSuffix(b, "/HEAD") || strings.Contains(b, "->") {
			continue
		}
		filtered = append(filtered, b)
	}
	remote = filtered
	def = gitDefaultBranch(be, path)
	if def == "" {
		for _, cand := range []string{"main", "master"} {
			for _, b := range local {
				if b == cand {
					def = cand
				}
			}
		}
	}
	if def == "" && len(local) > 0 {
		def = local[0]
	}
	return local, remote, def
}

// ---------------------------------------------------------------------------
// host-scoped settings — all backed by the local lasso.db (host "local")
// ---------------------------------------------------------------------------

// hostDefaults reads the creator defaults from the local lasso.db.
func hostDefaults(host string) (creatorDefaults, error) {
	s, err := getSettings()
	return creatorDefaults{s.ReposRoot, s.BranchPrefix, s.DefaultAgent, s.ScratchSetup}, err
}

// hostSetDefaults writes the creator defaults to the local lasso.db.
func hostSetDefaults(host string, p defaultsPatch) error {
	return applyDefaults(p)
}

// hostReposList lists repos (scanned on the local fs, merged with per-repo state).
func hostReposList(host string) (string, []repoEntry, error) {
	return reposList(localFsBackend(), "local")
}

// hostRepoConfig reads one repo's per-repo settings.
func hostRepoConfig(host, path string) (RepoConfig, error) {
	return getRepoState("local", path)
}

// hostSetRepoConfig writes one repo's per-repo settings.
func hostSetRepoConfig(host, path string, copyFiles, setup *string) (RepoConfig, error) {
	return applyRepoConfig("local", path, copyFiles, setup)
}

// hostAgentConfig returns the creator config the UI shows: defaults from the
// local db merged with this lasso's last-used selections + agent log.
func hostAgentConfig(host string) (*LassoConfig, error) {
	def, err := hostDefaults(host)
	if err != nil {
		return nil, err
	}
	c, err := loadLassoConfig(host)
	if err != nil {
		return nil, err
	}
	c.ReposRoot = def.ReposRoot
	c.BranchPrefix = def.BranchPrefix
	c.DefaultAgent = def.DefaultAgent
	c.ScratchSetup = def.ScratchSetup
	return c, nil
}

// localFsBackend is the local backend for reading the local machine's filesystem.
func localFsBackend() Backend { return &localBackend{} }
