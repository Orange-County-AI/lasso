package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
)

// Per-host creator settings. Each host owns its settings in its OWN
// ~/.lasso/lasso.db: the machine lasso runs on uses the in-process db handle;
// any other host is reached by running `lasso cli …` on it over the SSH control
// master (see remoteBackend.runCLI), so the read/write happens against that
// host's database through its own lasso binary. This file holds the shared core
// logic (used by both the HTTP handlers here and the `lasso cli` subcommand) and
// the provider that routes a request to the local db or a remote CLI.
//
// "Settings" means the creator defaults (repos_root, branch_prefix,
// default_agent, scratch_setup) and per-repo copy-files/setup. The agent log and
// last-used selections remain this lasso's local memory (keyed by host name), so
// loadLassoConfig still sources those; only the editable settings live per-host.

// creatorDefaults is the JSON shape of the four global creator settings, shared
// by the CLI and the provider. The tags match LassoConfig so either decodes it.
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

// reposList scans repos_root on be (one level deep) for git repos, merged with
// host's per-repo state from the db. Mirrors the old serveRepos body.
func reposList(be Backend, host string) (string, []repoEntry, error) {
	s, err := getSettings()
	if err != nil {
		return "", nil, err
	}
	repoState, err := listRepoState(host)
	if err != nil {
		return "", nil, err
	}
	root := expandTilde(s.ReposRoot)
	ents, err := be.ReadDir(root)
	if err != nil {
		// An unreadable/missing root isn't fatal — return an empty list so the
		// picker still opens (the user can fix the root in settings).
		return root, []repoEntry{}, nil
	}
	repos := make([]repoEntry, 0, len(ents))
	for _, e := range ents {
		if !e.Dir {
			continue
		}
		repoPath := filepath.Join(root, e.Name)
		if _, err := be.Stat(filepath.Join(repoPath, ".git")); err != nil {
			continue // not a git repo
		}
		re := repoEntry{Path: repoPath, Name: e.Name}
		if rc := repoState[repoPath]; rc != nil {
			re.CopyFiles, re.Setup, re.LastBaseBranch = rc.CopyFiles, rc.Setup, rc.LastBaseBranch
		}
		repos = append(repos, re)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	return root, repos, nil
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
// provider — route a host-scoped request to the local db or a remote CLI
// ---------------------------------------------------------------------------

// isLocalHost reports whether host names the machine lasso runs on (where the
// in-process db is its own state). Empty defaults to local.
func isLocalHost(host string) bool { return host == "" || host == "local" }

// hostParam resolves the ?host= target for a host-scoped config request,
// defaulting to the active host, and reports whether we may drive it (local, the
// active host, or a reachable + compatible remote).
func hostParam(r *http.Request) (string, bool) {
	host := r.URL.Query().Get("host")
	if host == "" {
		host = curBackend().Name()
	}
	return host, gridHostAllowed(host)
}

// hostDefaults reads host's creator defaults from host's own lasso.db.
func hostDefaults(host string) (creatorDefaults, error) {
	if isLocalHost(host) {
		s, err := getSettings()
		return creatorDefaults{s.ReposRoot, s.BranchPrefix, s.DefaultAgent, s.ScratchSetup}, err
	}
	raw, err := remoteCLI(host, []string{"config-get"}, nil)
	if err != nil {
		return creatorDefaults{}, err
	}
	var d creatorDefaults
	if err := json.Unmarshal(raw, &d); err != nil {
		return creatorDefaults{}, fmt.Errorf("config-get %s: %w", host, err)
	}
	return d, nil
}

// hostSetDefaults writes host's creator defaults to host's own lasso.db.
func hostSetDefaults(host string, p defaultsPatch) error {
	if isLocalHost(host) {
		return applyDefaults(p)
	}
	body, _ := json.Marshal(p)
	_, err := remoteCLI(host, []string{"config-set"}, body)
	return err
}

// hostReposList lists host's repos (scanned on host's fs, merged with host's
// per-repo state) from host's own state.
func hostReposList(host string) (string, []repoEntry, error) {
	if isLocalHost(host) {
		return reposList(localFsBackend(), "local")
	}
	raw, err := remoteCLI(host, []string{"repos"}, nil)
	if err != nil {
		return "", nil, err
	}
	var out struct {
		Root  string      `json:"root"`
		Repos []repoEntry `json:"repos"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", nil, fmt.Errorf("repos %s: %w", host, err)
	}
	return out.Root, out.Repos, nil
}

// hostRepoConfig reads one repo's per-repo settings from host's own state.
func hostRepoConfig(host, path string) (RepoConfig, error) {
	if isLocalHost(host) {
		return getRepoState("local", path)
	}
	raw, err := remoteCLI(host, []string{"repo-config-get", path}, nil)
	if err != nil {
		return RepoConfig{}, err
	}
	var rc RepoConfig
	if err := json.Unmarshal(raw, &rc); err != nil {
		return RepoConfig{}, fmt.Errorf("repo-config-get %s: %w", host, err)
	}
	return rc, nil
}

// hostSetRepoConfig writes one repo's per-repo settings to host's own state.
func hostSetRepoConfig(host, path string, copyFiles, setup *string) (RepoConfig, error) {
	if isLocalHost(host) {
		return applyRepoConfig("local", path, copyFiles, setup)
	}
	body, _ := json.Marshal(repoConfigPatch{Path: path, CopyFiles: copyFiles, Setup: setup})
	raw, err := remoteCLI(host, []string{"repo-config-set"}, body)
	if err != nil {
		return RepoConfig{}, err
	}
	var rc RepoConfig
	if err := json.Unmarshal(raw, &rc); err != nil {
		return RepoConfig{}, fmt.Errorf("repo-config-set %s: %w", host, err)
	}
	return rc, nil
}

// hostAgentConfig returns the creator config the UI shows for host: defaults from
// host's own db (via the provider), merged with this lasso's local memory of
// what it did on that host (last-used selections + agent log). For the local
// host the two sources are the same db, so the merge is a no-op overlay.
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

// remoteCLI runs `lasso cli <args>` on host over its SSH control master, piping
// stdin and returning stdout. host must be a reachable, compatible remote.
func remoteCLI(host string, args []string, stdin []byte) ([]byte, error) {
	be, err := gridHostBackend(host)
	if err != nil {
		return nil, err
	}
	rb, ok := be.(*remoteBackend)
	if !ok {
		return nil, fmt.Errorf("host %s is not a remote backend", host)
	}
	return rb.runCLI(args, stdin)
}

// localFsBackend is a localBackend for reading the local machine's filesystem,
// independent of whichever host is currently active.
func localFsBackend() Backend { return &localBackend{sock: *herdrSock} }
