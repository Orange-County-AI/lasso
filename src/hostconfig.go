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
// any other host is reached by running its sqlite3 over the SSH control master
// against that host's database (see remoteDB). The lasso binary isn't assumed to
// be installed on remotes — only sqlite3, which the fleet already has — so this
// works uniformly across a cross-OS fleet without shipping binaries.
//
// "Settings" means the creator defaults (repos_root, branch_prefix,
// default_agent, scratch_setup) and per-repo copy-files/setup. The agent log and
// last-used selections remain this lasso's local memory (keyed by host name), so
// loadLassoConfig still sources those; only the editable settings live per-host.
// On a host's own db its data is keyed as host "local" (every host is local to
// itself), matching how that host's lasso records its own state.

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
	// Disambiguate name collisions: when two repos share a basename (e.g.
	// .../52labs/bot and .../ocai/bot), prepend the parent directory so the
	// picker/search shows "52labs/bot" vs "ocai/bot" instead of two bare "bot"s.
	counts := map[string]int{}
	for _, re := range repos {
		counts[re.Name]++
	}
	for i := range repos {
		if counts[repos[i].Name] > 1 {
			parent := filepath.Base(filepath.Dir(repos[i].Path))
			if parent != "" && parent != "." && parent != string(filepath.Separator) {
				repos[i].Name = parent + "/" + repos[i].Name
			}
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
// provider — route a host-scoped request to the local db or a remote sqlite3
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
	raw, err := remoteDB(host, true, `SELECT key, value FROM settings;`)
	if err != nil {
		return creatorDefaults{}, err
	}
	rows, err := parseKVRows(raw)
	if err != nil {
		return creatorDefaults{}, fmt.Errorf("read settings on %s: %w", host, err)
	}
	return defaultsFromRows(rows), nil
}

// hostSetDefaults writes host's creator defaults to host's own lasso.db.
func hostSetDefaults(host string, p defaultsPatch) error {
	// repos_root decides what the repo scan finds, so any defaults write drops
	// host's cached listing rather than serving the pre-change one for a whole TTL.
	defer invalidateRepoCache(host)
	if isLocalHost(host) {
		return applyDefaults(p)
	}
	var b strings.Builder
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
		fmt.Fprintf(&b, "INSERT INTO settings(key,value) VALUES(%s,%s) "+
			"ON CONFLICT(key) DO UPDATE SET value=excluded.value;\n",
			sqlQuote(u.key), sqlQuote(*u.val))
	}
	if b.Len() == 0 {
		return nil
	}
	_, err := remoteDB(host, false, b.String())
	return err
}

// hostReposList lists host's repos (scanned on host's fs, merged with host's
// per-repo state) from host's own state.
func hostReposList(host string) (string, []repoEntry, error) {
	if isLocalHost(host) {
		return reposList(localFsBackend(), "local")
	}
	def, err := hostDefaults(host)
	if err != nil {
		return "", nil, err
	}
	state, err := remoteRepoState(host)
	if err != nil {
		return "", nil, err
	}
	be, err := gridHostBackend(host)
	if err != nil {
		return "", nil, err
	}
	root, repos := reposListWith(be, def.ReposRoot, state)
	return root, repos, nil
}

// hostRepoConfig reads one repo's per-repo settings from host's own state.
func hostRepoConfig(host, path string) (RepoConfig, error) {
	if isLocalHost(host) {
		return getRepoState("local", path)
	}
	raw, err := remoteDB(host, true, fmt.Sprintf(
		`SELECT copy_files, setup, last_base_branch FROM repo_state `+
			`WHERE host='local' AND repo_path=%s;`, sqlQuote(path)))
	if err != nil {
		return RepoConfig{}, err
	}
	rcs, err := parseRepoConfigRows(raw)
	if err != nil || len(rcs) == 0 {
		return RepoConfig{}, err
	}
	return rcs[0], nil
}

// hostSetRepoConfig writes one repo's per-repo settings to host's own state.
func hostSetRepoConfig(host, path string, copyFiles, setup *string) (RepoConfig, error) {
	// copy_files/setup are merged into each repoEntry, so the listing is now stale.
	defer invalidateRepoCache(host)
	if isLocalHost(host) {
		return applyRepoConfig("local", path, copyFiles, setup)
	}
	var b strings.Builder
	if copyFiles != nil {
		fmt.Fprintf(&b, "INSERT INTO repo_state(host,repo_path,copy_files) VALUES('local',%s,%s) "+
			"ON CONFLICT(host,repo_path) DO UPDATE SET copy_files=excluded.copy_files;\n",
			sqlQuote(path), sqlQuote(*copyFiles))
	}
	if setup != nil {
		fmt.Fprintf(&b, "INSERT INTO repo_state(host,repo_path,setup) VALUES('local',%s,%s) "+
			"ON CONFLICT(host,repo_path) DO UPDATE SET setup=excluded.setup;\n",
			sqlQuote(path), sqlQuote(*setup))
	}
	fmt.Fprintf(&b, "SELECT copy_files, setup, last_base_branch FROM repo_state "+
		"WHERE host='local' AND repo_path=%s;", sqlQuote(path))
	raw, err := remoteDB(host, true, b.String())
	if err != nil {
		return RepoConfig{}, err
	}
	rcs, err := parseRepoConfigRows(raw)
	if err != nil || len(rcs) == 0 {
		return RepoConfig{}, err
	}
	return rcs[0], nil
}

// remoteRepoState returns host's per-repo state keyed by absolute repo path.
func remoteRepoState(host string) (map[string]*RepoConfig, error) {
	raw, err := remoteDB(host, true, `SELECT repo_path, copy_files, setup, last_base_branch `+
		`FROM repo_state WHERE host='local';`)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		RepoPath       string `json:"repo_path"`
		CopyFiles      string `json:"copy_files"`
		Setup          string `json:"setup"`
		LastBaseBranch string `json:"last_base_branch"`
	}
	if err := unmarshalRows(raw, &rows); err != nil {
		return nil, fmt.Errorf("read repo_state on %s: %w", host, err)
	}
	out := make(map[string]*RepoConfig, len(rows))
	for _, r := range rows {
		out[r.RepoPath] = &RepoConfig{CopyFiles: r.CopyFiles, Setup: r.Setup, LastBaseBranch: r.LastBaseBranch}
	}
	return out, nil
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
	// Fill each harness's DefaultModel from the target host's own CLI config
	// (e.g. Claude Code's configured model) so the creator can default the model
	// field to it. Best-effort: if the host's backend isn't reachable we keep the
	// static registry (DefaultModel empty → the CLI's own default).
	if be, err := gridHostBackend(host); err == nil {
		c.Harnesses = resolveHarnesses(be)
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// remote sqlite3 access
// ---------------------------------------------------------------------------

// remoteSchemaSQL ensures the tables this feature touches exist before any
// read/write, so a host with no lasso.db yet (or one created by something else)
// just works. Mirrors the relevant slice of db.go's dbSchema.
const remoteSchemaSQL = `CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS repo_state (host TEXT NOT NULL, repo_path TEXT NOT NULL, copy_files TEXT NOT NULL DEFAULT '', setup TEXT NOT NULL DEFAULT '', last_base_branch TEXT NOT NULL DEFAULT '', PRIMARY KEY (host, repo_path));
`

// sqlQuote renders s as a SQLite string literal (doubling embedded single
// quotes), safe to embed in SQL we build for the remote sqlite3. The SQL itself
// is piped on stdin, so this is the only layer of quoting that matters.
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// remoteDB runs SQL against host's own ~/.lasso/lasso.db using the remote's
// sqlite3 over the SSH control master. The SQL (schema-ensure prefix + sql) is
// piped on stdin so nothing rides the shell; jsonRows requests -json output for
// a trailing SELECT. host must be a reachable, compatible remote.
func remoteDB(host string, jsonRows bool, sql string) ([]byte, error) {
	be, err := gridHostBackend(host)
	if err != nil {
		return nil, err
	}
	rb, ok := be.(*remoteBackend)
	if !ok {
		return nil, fmt.Errorf("host %s is not a remote backend", host)
	}
	flags := "-batch"
	if jsonRows {
		flags = "-batch -json"
	}
	// Login shell so sqlite3 is on PATH; mkdir so a brand-new host's db opens.
	inner := `mkdir -p "$HOME/.lasso" && sqlite3 ` + flags + ` "$HOME/.lasso/lasso.db"`
	remoteCmd := `${SHELL:-sh} -lc ` + shellQuote(inner)
	return rb.runStdin(remoteCmd, []byte(remoteSchemaSQL+sql))
}

// unmarshalRows decodes sqlite3 -json output into v. sqlite3 prints nothing for
// an empty result set, which we treat as an empty array.
func unmarshalRows(raw []byte, v any) error {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// parseKVRows decodes [{"key":…,"value":…}] from a settings SELECT.
func parseKVRows(raw []byte) ([]struct{ Key, Value string }, error) {
	var rows []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := unmarshalRows(raw, &rows); err != nil {
		return nil, err
	}
	out := make([]struct{ Key, Value string }, len(rows))
	for i, r := range rows {
		out[i] = struct{ Key, Value string }{r.Key, r.Value}
	}
	return out, nil
}

// parseRepoConfigRows decodes RepoConfig rows from a repo_state SELECT (the
// column names match RepoConfig's json tags).
func parseRepoConfigRows(raw []byte) ([]RepoConfig, error) {
	var rows []RepoConfig
	if err := unmarshalRows(raw, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// defaultsFromRows folds settings key/value rows into creatorDefaults, applying
// the same repos_root default getSettings uses.
func defaultsFromRows(rows []struct{ Key, Value string }) creatorDefaults {
	d := creatorDefaults{ReposRoot: "~/projects"}
	for _, r := range rows {
		switch r.Key {
		case "repos_root":
			if r.Value != "" {
				d.ReposRoot = r.Value
			}
		case "branch_prefix":
			d.BranchPrefix = r.Value
		case "default_agent":
			d.DefaultAgent = r.Value
		case "scratch_setup":
			d.ScratchSetup = r.Value
		}
	}
	return d
}

// localFsBackend is a localBackend for reading the local machine's filesystem,
// independent of whichever host is currently active.
func localFsBackend() Backend { return &localBackend{sock: *herdrSock} }
