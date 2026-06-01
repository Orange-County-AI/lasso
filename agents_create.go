package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Agent creation: a streamlined "New Agent" flow that replaces hand-typing the
// `herdr workspace/worktree create` + `herdr pane run "claude …"` recipe in the
// embedded terminal. Two flavors, mirroring fulcrum's git/scratch tasks:
//
//   - git agent     → a git worktree off a chosen repo/base branch. We call
//                     herdr's worktree.create, which also creates the repo's
//                     parent workspace if absent and returns the worktree's
//                     root pane. We then copy any configured files in, run the
//                     repo's setup script, and launch the agent — all in that
//                     pane's shell.
//   - scratch agent → a plain workspace rooted at a fresh ~/.lasso/scratch dir,
//                     then the scratch setup script + agent.
//
// Everything routes through curBackend() so it targets the active herdr host;
// settings + records persist locally via config.go.

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify lowercases text and collapses non-alphanumerics to single dashes.
func slugify(s string) string {
	s = slugRe.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "-")
}

// uniqueChildDir returns an absolute path under parent based on slug, suffixing
// -2, -3, … if the name is already taken on the active host.
func uniqueChildDir(parent, slug string) string {
	if slug == "" {
		slug = "agent"
	}
	cur := curBackend()
	candidate := filepath.Join(parent, slug)
	for i := 2; ; i++ {
		if _, err := cur.Stat(candidate); err != nil && os.IsNotExist(err) {
			return candidate
		} else if err != nil {
			// Non-"not exist" stat error (e.g. permission): take the name anyway;
			// the create step will surface a real failure.
			return candidate
		}
		candidate = filepath.Join(parent, fmt.Sprintf("%s-%d", slug, i))
	}
}

// randSuffix returns a short random alphanumeric tag, mirroring the frontend's
// branch-name suffix (generateBranchName). Used to keep scratch dir names unique
// the way a worktree's random-suffixed branch keeps worktrees distinct.
func randSuffix() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 4)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return string(b)
}

// worktreeDirSlug derives the directory seed from the final branch name so the
// branch's unique suffix is carried onto disk too. Branch prefixes are omitted
// to keep paths short (feature/foo-a1b2 -> foo-a1b2).
func worktreeDirSlug(branch, fallback string) string {
	leaf := branch
	if i := strings.LastIndex(leaf, "/"); i >= 0 {
		leaf = leaf[i+1:]
	}
	if slug := slugify(leaf); slug != "" {
		return slug
	}
	return fallback
}

// splitGlobs splits a comma/newline-separated copy-files spec into trimmed,
// non-empty glob patterns.
func splitGlobs(spec string) []string {
	fields := strings.FieldsFunc(spec, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// GET/POST /api/agent-config — creator settings + agent records
// ---------------------------------------------------------------------------

func serveAgentConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		configMu.Lock()
		c, err := loadLassoConfig()
		configMu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, c)
	case http.MethodPost:
		var req struct {
			ReposRoot    *string `json:"repos_root"`
			BranchPrefix *string `json:"branch_prefix"`
			DefaultAgent *string `json:"default_agent"`
			ScratchSetup *string `json:"scratch_setup"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		configMu.Lock()
		defer configMu.Unlock()
		c, err := loadLassoConfig()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if req.ReposRoot != nil {
			c.ReposRoot = *req.ReposRoot
		}
		if req.BranchPrefix != nil {
			c.BranchPrefix = *req.BranchPrefix
		}
		if req.DefaultAgent != nil {
			c.DefaultAgent = *req.DefaultAgent
		}
		if req.ScratchSetup != nil {
			c.ScratchSetup = *req.ScratchSetup
		}
		if err := saveLassoConfig(c); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, c)
	default:
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// POST /api/repo-config — save a repo's copy-files + setup (Settings tab)
// ---------------------------------------------------------------------------

func serveRepoConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Path      string  `json:"path"`
		CopyFiles *string `json:"copy_files"`
		Setup     *string `json:"setup"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path := expandTilde(strings.TrimSpace(req.Path))
	if path == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	configMu.Lock()
	defer configMu.Unlock()
	c, err := loadLassoConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rc := c.repoConf(path)
	if req.CopyFiles != nil {
		rc.CopyFiles = *req.CopyFiles
	}
	if req.Setup != nil {
		rc.Setup = *req.Setup
	}
	if err := saveLassoConfig(c); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rc)
}

// ---------------------------------------------------------------------------
// GET /api/repos — list git repos under repos_root (one level deep)
// ---------------------------------------------------------------------------

type repoEntry struct {
	Path           string `json:"path"`
	Name           string `json:"name"`
	CopyFiles      string `json:"copy_files"`
	Setup          string `json:"setup"`
	LastBaseBranch string `json:"last_base_branch"`
}

func serveRepos(w http.ResponseWriter, r *http.Request) {
	configMu.Lock()
	c, err := loadLassoConfig()
	configMu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	root := expandTilde(c.ReposRoot)
	cur := curBackend()
	ents, err := cur.ReadDir(root)
	if err != nil {
		// An unreadable/missing repos root is not fatal — return an empty list so
		// the picker still opens (the user can browse/fix the root in settings).
		writeJSON(w, map[string]any{"root": root, "repos": []repoEntry{}})
		return
	}
	repos := make([]repoEntry, 0, len(ents))
	for _, e := range ents {
		if !e.Dir {
			continue
		}
		repoPath := filepath.Join(root, e.Name)
		if _, err := cur.Stat(filepath.Join(repoPath, ".git")); err != nil {
			continue // not a git repo
		}
		re := repoEntry{Path: repoPath, Name: e.Name}
		if rc := c.Repos[repoPath]; rc != nil {
			re.CopyFiles, re.Setup, re.LastBaseBranch = rc.CopyFiles, rc.Setup, rc.LastBaseBranch
		}
		repos = append(repos, re)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	writeJSON(w, map[string]any{"root": root, "repos": repos})
}

// ---------------------------------------------------------------------------
// GET /api/repo-branches?path=… — local + remote branches and the default
// ---------------------------------------------------------------------------

func serveRepoBranches(w http.ResponseWriter, r *http.Request) {
	path := expandTilde(r.URL.Query().Get("path"))
	if path == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	cur := curBackend()
	local := gitBranchList(cur, path, "refs/heads")
	remote := gitBranchList(cur, path, "refs/remotes")
	// Drop the symbolic "origin/HEAD -> …" entry from the remote list.
	filtered := remote[:0]
	for _, b := range remote {
		if strings.HasSuffix(b, "/HEAD") || strings.Contains(b, "->") {
			continue
		}
		filtered = append(filtered, b)
	}
	remote = filtered

	def := gitDefaultBranch(cur, path)
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
	writeJSON(w, map[string]any{"branches": local, "remoteBranches": remote, "default": def})
}

// gitBranchList returns short branch names under a ref namespace (refs/heads or
// refs/remotes), empty on error.
func gitBranchList(cur Backend, repo, ns string) []string {
	out, err := cur.GitOut(repo, "for-each-ref", "--format=%(refname:short)", ns)
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

// gitDefaultBranch resolves the repo's default branch via origin/HEAD, "" on
// failure.
func gitDefaultBranch(cur Backend, repo string) string {
	out, err := cur.GitOut(repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return ""
	}
	short := strings.TrimSpace(out)
	if i := strings.LastIndex(short, "/"); i >= 0 {
		short = short[i+1:]
	}
	return short
}

// ---------------------------------------------------------------------------
// POST /api/create-agent — the main flow
// ---------------------------------------------------------------------------

type createAgentReq struct {
	Type         string   `json:"type"` // "git" | "scratch"
	Title        string   `json:"title"`
	Repo         string   `json:"repo"`
	BaseBranch   string   `json:"base_branch"`
	BranchPrefix string   `json:"branch_prefix"`
	BranchName   string   `json:"branch_name"`
	Agent        string   `json:"agent"`
	Description  string   `json:"description"`
	Notes        string   `json:"notes"`
	PlanMode     bool     `json:"plan_mode"`
	Attachments  []string `json:"attachments"` // filenames staged under UploadDir
	UploadDir    string   `json:"upload_dir"`  // staging dir returned by /api/agent-upload
}

func serveCreateAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req createAgentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		http.Error(w, "title required", http.StatusBadRequest)
		return
	}
	if req.Agent == "" {
		req.Agent = "claude"
	}
	// The files-to-copy and setup commands are properties of the repo (and the
	// scratch default), configured in Settings — not per-agent. Read them from
	// the config rather than the request.
	configMu.Lock()
	cfg, cfgErr := loadLassoConfig()
	configMu.Unlock()
	if cfgErr != nil {
		http.Error(w, cfgErr.Error(), http.StatusInternalServerError)
		return
	}
	cur := curBackend()
	slug := slugify(req.Title)

	rec := AgentRecord{
		ID:          strconv.FormatInt(time.Now().UnixNano(), 36),
		Title:       req.Title,
		Type:        req.Type,
		Agent:       req.Agent,
		Description: strings.TrimSpace(req.Description),
		Notes:       strings.TrimSpace(req.Notes),
		Attachments: req.Attachments,
		PlanMode:    req.PlanMode,
		CreatedAt:   time.Now(),
	}

	var rootPane string
	var setup string

	switch req.Type {
	case "git":
		repo := expandTilde(req.Repo)
		if repo == "" {
			http.Error(w, "repo required for a git agent", http.StatusBadRequest)
			return
		}
		// Compose the branch from prefix + name (auto-slug fallback), then make it
		// unique against existing branches in the repo.
		name := strings.TrimSpace(req.BranchName)
		if name == "" {
			name = slug
		}
		prefix := strings.TrimRight(strings.TrimSpace(req.BranchPrefix), "/")
		branch := name
		if prefix != "" {
			branch = prefix + "/" + name
		}
		branch = uniqueBranch(cur, repo, branch)

		workDir := uniqueChildDir(lassoWorktreesDir(), worktreeDirSlug(branch, slug))
		base := strings.TrimSpace(req.BaseBranch)
		if base == "" {
			base = "HEAD"
		}
		res, err := herdrCall("worktree.create", map[string]any{
			"cwd":    repo,
			"branch": branch,
			"base":   base,
			"path":   workDir,
			"label":  req.Title,
			// Focus the new worktree's pane so the user lands on the agent as it
			// boots (the New Agent flow is an explicit "take me there").
			"focus": true,
		})
		if err != nil {
			http.Error(w, "worktree.create: "+err.Error(), http.StatusBadGateway)
			return
		}
		ws, pane := parseCreateResult(res)
		rec.Repo, rec.BaseBranch, rec.Branch = repo, base, branch
		rec.WorkDir, rec.WorkspaceID, rec.RootPane = workDir, ws, pane
		rootPane = pane

		// Copy the repo's configured files into the worktree and run its setup
		// script before the agent (both per-repo settings from config).
		rc := cfg.repoConf(repo)
		copyRepoFiles(cur, repo, workDir, rc.CopyFiles)
		setup = rc.Setup

	case "scratch":
		// A scratch agent has no branch to carry a random suffix, so append one to
		// the dir itself — two same-titled scratch agents then get distinct dirs
		// (e.g. hey-boss-a3f9), the way worktrees stay distinct via their branch.
		// uniqueChildDir still guards the (astronomically unlikely) suffix clash.
		workDir := uniqueChildDir(lassoScratchDir(), slug+"-"+randSuffix())
		if err := cur.MkdirAll(workDir, 0o755); err != nil {
			http.Error(w, "mkdir "+workDir+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		res, err := herdrCall("workspace.create", map[string]any{
			"cwd":   workDir,
			"label": req.Title,
			"focus": true, // land on the new agent's pane as it boots
		})
		if err != nil {
			http.Error(w, "workspace.create: "+err.Error(), http.StatusBadGateway)
			return
		}
		ws, pane := parseCreateResult(res)
		rec.WorkDir, rec.WorkspaceID, rec.RootPane = workDir, ws, pane
		rootPane = pane
		setup = cfg.ScratchSetup

	default:
		http.Error(w, `type must be "git" or "scratch"`, http.StatusBadRequest)
		return
	}

	// Move staged attachments into the work dir; write notes to NOTES.md.
	moveAttachments(cur, req.UploadDir, req.Attachments, rec.WorkDir)
	if rec.Notes != "" {
		_ = cur.WriteFile(filepath.Join(rec.WorkDir, "NOTES.md"), []byte(rec.Notes+"\n"), 0o644)
	}

	// Launch: run the setup script (if any), then the agent command, in the
	// root pane's shell — the equivalent of the `herdr pane run` recipe.
	if rootPane != "" {
		if s := strings.TrimSpace(setup); s != "" {
			paneRun(rootPane, s)
		}
		paneRun(rootPane, agentCommand(req.Agent, rec.PlanMode, agentPrompt(rec)))
		// Both claude and codex show a per-directory trust dialog at boot that
		// their --dangerously-* flags do NOT bypass, leaving the agent blocked.
		// Auto-accept it in the background so the agent boots straight into the
		// task; the prompt rides along as a CLI arg, so the agent proceeds with it
		// once trust is granted.
		go confirmAgentTrust(rootPane)
	}

	// Persist: remember the repo/base-branch + copy/setup edits, append the record.
	configMu.Lock()
	defer configMu.Unlock()
	c, err := loadLassoConfig()
	if err == nil {
		if req.Type == "git" {
			c.LastRepo = rec.Repo
			c.repoConf(rec.Repo).LastBaseBranch = rec.BaseBranch
		}
		c.Agents = append(c.Agents, rec)
		if err := saveLassoConfig(c); err != nil {
			http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, rec)
}

// parseCreateResult pulls the workspace_id and root pane_id out of a
// worktree.create / workspace.create response.
func parseCreateResult(res json.RawMessage) (workspaceID, rootPane string) {
	var r struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	_ = json.Unmarshal(res, &r)
	return r.Workspace.WorkspaceID, r.RootPane.PaneID
}

// uniqueBranch returns branch, suffixing -2, -3, … until it doesn't match an
// existing branch in the repo.
func uniqueBranch(cur Backend, repo, branch string) string {
	exists := func(b string) bool {
		out, err := cur.GitOut(repo, "branch", "--list", b)
		return err == nil && strings.TrimSpace(out) != ""
	}
	if !exists(branch) {
		return branch
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", branch, i)
		if !exists(cand) {
			return cand
		}
	}
}

// copyRepoFiles copies files matching the comma/newline-separated globs from the
// repo into the worktree, skipping files that already exist. Best effort — a
// failing pattern is silently ignored (it mirrors fulcrum's copyFilesToWorktree).
// Globbing goes through the active backend (backendGlob), not the local os, so a
// repo on a remote host is matched on that host rather than missed entirely.
func copyRepoFiles(cur Backend, repo, dest, spec string) {
	for _, pattern := range splitGlobs(spec) {
		for _, src := range backendGlob(cur, filepath.Join(repo, pattern)) {
			rel, err := filepath.Rel(repo, src)
			if err != nil {
				continue
			}
			dst := filepath.Join(dest, rel)
			if _, err := cur.Stat(dst); err == nil {
				continue // don't overwrite
			}
			info, err := cur.Stat(src)
			if err != nil || info.IsDir() {
				continue // directories not handled
			}
			_ = cur.MkdirAll(filepath.Dir(dst), 0o755)
			copyFile(cur, src, dst)
		}
	}
}

// copyFile streams src → dst through the active backend.
func copyFile(cur Backend, src, dst string) {
	in, err := cur.Open(src)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := cur.Create(dst)
	if err != nil {
		return
	}
	defer out.Close()
	_, _ = io.Copy(out, in)
}

// backendGlob evaluates a filepath.Glob-style pattern on the active backend, so
// the same copy-files spec works whether the repo is local or on a remote host.
// It ports the stdlib's segment-by-segment matching (no recursive `**`), but
// lists directories via cur.ReadDir / cur.Lstat instead of the local os. The
// pattern is expected to be an absolute path (callers join it onto the repo).
func backendGlob(cur Backend, pattern string) []string {
	if !globHasMeta(pattern) {
		if _, err := cur.Lstat(pattern); err != nil {
			return nil // pattern names a file that doesn't exist
		}
		return []string{pattern}
	}
	dir, file := filepath.Split(pattern)
	dir = filepath.Clean(dir)
	if !globHasMeta(dir) {
		return backendGlobDir(cur, dir, file, nil)
	}
	if dir == pattern {
		return nil // no separator to split on — avoid infinite recursion
	}
	var out []string
	for _, d := range backendGlob(cur, dir) {
		out = backendGlobDir(cur, d, file, out)
	}
	return out
}

// backendGlobDir appends the entries of dir on the backend whose names match the
// (meta-bearing) final pattern segment.
func backendGlobDir(cur Backend, dir, pattern string, matches []string) []string {
	fi, err := cur.Stat(dir)
	if err != nil || !fi.IsDir() {
		return matches
	}
	ents, err := cur.ReadDir(dir)
	if err != nil {
		return matches
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name < ents[j].Name })
	for _, e := range ents {
		if ok, _ := filepath.Match(pattern, e.Name); ok {
			matches = append(matches, filepath.Join(dir, e.Name))
		}
	}
	return matches
}

// globHasMeta reports whether path contains any glob metacharacter.
func globHasMeta(path string) bool { return strings.ContainsAny(path, "*?[") }

// moveAttachments copies the named files from the staging upload dir into the
// work dir, then clears the staging dir (best effort). serveAgentUpload always
// stages on the lasso-local disk (os.*), so the source is read locally while the
// destination is written through the active backend — which streams each file
// onto the remote host over SFTP when one is selected. A plain Rename can't do
// this: on a remote backend it would look for the local staging path on the
// remote box and silently drop every attachment.
func moveAttachments(cur Backend, uploadDir string, names []string, dest string) {
	if uploadDir == "" || len(names) == 0 {
		return
	}
	staging := filepath.Join(lassoUploadsDir(), filepath.Base(uploadDir))
	for _, n := range names {
		base := filepath.Base(n)
		if base == "" || base == "." {
			continue
		}
		copyLocalToBackend(cur, filepath.Join(staging, base), filepath.Join(dest, base))
	}
	_ = os.RemoveAll(staging)
}

// copyLocalToBackend streams a file from the lasso-local filesystem to dst on
// the active backend (the local disk, or a remote host over SFTP).
func copyLocalToBackend(cur Backend, src, dst string) {
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := cur.Create(dst)
	if err != nil {
		return
	}
	defer out.Close()
	_, _ = io.Copy(out, in)
}

// agentPrompt builds the prompt handed to the agent: the title (always present —
// it's the task's headline), then the fuller description, plus a pointer to any
// notes/attachments that landed in the work dir. Leading with the title mirrors
// fulcrum's "<title>: <description>" so the agent never starts on description
// alone with no idea what it's building.
func agentPrompt(rec AgentRecord) string {
	var b strings.Builder
	b.WriteString(rec.Title)
	if rec.Description != "" {
		b.WriteString(": ")
		b.WriteString(rec.Description)
	}
	if rec.Notes != "" {
		b.WriteString("\n\nSee NOTES.md for additional notes.")
	}
	if len(rec.Attachments) > 0 {
		b.WriteString("\n\nAttachments: " + strings.Join(rec.Attachments, ", "))
	}
	return b.String()
}

// agentCommand builds the shell command that launches the chosen agent. A
// non-empty prompt is passed as the agent's initial instruction; plan mode is
// requested when supported.
func agentCommand(agent string, planMode bool, prompt string) string {
	switch agent {
	case "codex":
		// --dangerously-bypass-approvals-and-sandbox is codex's analog of claude's
		// --dangerously-skip-permissions (lasso worktrees are already isolated), so
		// the agent runs autonomously instead of prompting per command. It does NOT
		// skip codex's boot-time "Do you trust this directory?" gate, though — that
		// dialog is auto-accepted via the trust goroutine in serveCreateAgent (a
		// config-file/-c pre-trust is fragile across the pane's shell). No
		// documented plan-mode flag, so plan agents launch in the default mode.
		cmd := "codex --dangerously-bypass-approvals-and-sandbox"
		if prompt != "" {
			cmd += " " + shellQuote(prompt)
		}
		return cmd
	default: // claude
		// --dangerously-skip-permissions forces bypass mode and silently overrides
		// --permission-mode plan, so plan agents never actually plan. In plan mode
		// use --allow-dangerously-skip-permissions instead, which only *enables*
		// bypassing and coexists with plan. Mirrors fulcrum's agent-commands.ts.
		cmd := "claude --dangerously-skip-permissions"
		if planMode {
			cmd = "claude --allow-dangerously-skip-permissions --permission-mode plan"
		}
		if prompt != "" {
			cmd += " " + shellQuote(prompt)
		}
		return cmd
	}
}

// paneRun sends a command line into a pane's shell (text + Enter) — the
// pane.send_text behind `herdr pane run`.
func paneRun(paneID, command string) {
	_, _ = herdrCall("pane.send_text", map[string]any{
		"pane_id": paneID,
		"text":    command + "\n",
	})
}

// confirmAgentTrust watches a freshly-launched agent pane for its per-directory
// trust dialog (claude's "trust this folder" / codex's "trust the contents of
// this directory") and accepts it — both default to "Yes" and confirm on Enter.
// Neither agent's --dangerously-* flag bypasses this gate, so without it the
// agent sits blocked. Polls rather than sleeping a fixed time so it survives a
// slow setup script running before the agent; if the dir is already trusted the
// dialog never appears and this simply times out without sending anything.
func confirmAgentTrust(paneID string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if paneShowsTrustPrompt(paneID) {
			// Enter confirms the highlighted default ("Yes").
			_, _ = herdrCall("pane.send_text", map[string]any{
				"pane_id": paneID,
				"text":    "\r",
			})
			return
		}
	}
}

// paneShowsTrustPrompt reports whether the pane's visible screen currently shows
// claude's or codex's directory-trust dialog.
func paneShowsTrustPrompt(paneID string) bool {
	res, err := herdrCall("pane.read", map[string]any{
		"pane_id": paneID,
		"source":  "visible",
	})
	if err != nil {
		return false
	}
	var r struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	if json.Unmarshal(res, &r) != nil {
		return false
	}
	t := r.Read.Text
	return strings.Contains(t, "trust this folder") || // claude
		strings.Contains(t, "trust the contents of this directory") // codex
}

// ---------------------------------------------------------------------------
// POST /api/agent-upload — stage attachments before the agent is created
// ---------------------------------------------------------------------------

// serveAgentUpload accepts multipart files into a fresh staging directory under
// ~/.lasso/uploads and returns its id + the stored filenames. create-agent later
// moves these into the new agent's work dir. Staging happens on the lasso-local
// host (the same host the config lives on); create-agent copies them onto the
// active backend when it moves them.
func serveAgentUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 200<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "parse multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := strconv.FormatInt(time.Now().UnixNano(), 36)
	staging := filepath.Join(lassoUploadsDir(), id)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var saved []string
	for _, fh := range r.MultipartForm.File["files"] {
		name := filepath.Base(fh.Filename)
		if name == "" || name == "." || name == "/" {
			continue
		}
		src, err := fh.Open()
		if err != nil {
			http.Error(w, "open upload: "+err.Error(), http.StatusInternalServerError)
			return
		}
		out, err := os.Create(filepath.Join(staging, name))
		if err != nil {
			src.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, err = io.Copy(out, src)
		out.Close()
		src.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		saved = append(saved, name)
	}
	writeJSON(w, map[string]any{"upload_dir": id, "files": saved})
}
