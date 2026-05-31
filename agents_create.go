package main

import (
	"encoding/json"
	"fmt"
	"io"
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
	CopyFiles    string   `json:"copy_files"`
	Setup        string   `json:"setup"`
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

		workDir := uniqueChildDir(lassoWorktreesDir(), slug)
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
			"focus":  false,
		})
		if err != nil {
			http.Error(w, "worktree.create: "+err.Error(), http.StatusBadGateway)
			return
		}
		ws, pane := parseCreateResult(res)
		rec.Repo, rec.BaseBranch, rec.Branch = repo, base, branch
		rec.WorkDir, rec.WorkspaceID, rec.RootPane = workDir, ws, pane
		rootPane = pane

		// Copy configured files from the repo into the worktree (best effort).
		copyRepoFiles(cur, repo, workDir, req.CopyFiles)
		setup = req.Setup

	case "scratch":
		workDir := uniqueChildDir(lassoScratchDir(), slug)
		if err := cur.MkdirAll(workDir, 0o755); err != nil {
			http.Error(w, "mkdir "+workDir+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		res, err := herdrCall("workspace.create", map[string]any{
			"cwd":   workDir,
			"label": req.Title,
			"focus": false,
		})
		if err != nil {
			http.Error(w, "workspace.create: "+err.Error(), http.StatusBadGateway)
			return
		}
		ws, pane := parseCreateResult(res)
		rec.WorkDir, rec.WorkspaceID, rec.RootPane = workDir, ws, pane
		rootPane = pane
		setup = req.Setup

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
	}

	// Persist: remember the repo/base-branch + copy/setup edits, append the record.
	configMu.Lock()
	defer configMu.Unlock()
	c, err := loadLassoConfig()
	if err == nil {
		if req.Type == "git" {
			c.LastRepo = rec.Repo
			rc := c.repoConf(rec.Repo)
			rc.LastBaseBranch = rec.BaseBranch
			rc.CopyFiles = req.CopyFiles
			rc.Setup = req.Setup
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
func copyRepoFiles(cur Backend, repo, dest, spec string) {
	for _, pattern := range splitGlobs(spec) {
		matches, err := filepath.Glob(filepath.Join(repo, pattern))
		if err != nil {
			continue
		}
		for _, src := range matches {
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

// moveAttachments moves the named files from the staging upload dir into the
// work dir (best effort).
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
		src := filepath.Join(staging, base)
		dst := filepath.Join(dest, base)
		if err := cur.Rename(src, dst); err != nil {
			// Cross-device or remote backend: fall back to a stream copy.
			copyFile(cur, src, dst)
		}
	}
	_ = os.RemoveAll(staging)
}

// agentPrompt builds the prompt handed to the agent: the description, plus a
// pointer to any notes/attachments that landed in the work dir.
func agentPrompt(rec AgentRecord) string {
	var b strings.Builder
	b.WriteString(rec.Description)
	if rec.Notes != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("See NOTES.md for additional notes.")
	}
	if len(rec.Attachments) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("Attachments: " + strings.Join(rec.Attachments, ", "))
	}
	return b.String()
}

// agentCommand builds the shell command that launches the chosen agent. A
// non-empty prompt is passed as the agent's initial instruction; plan mode is
// requested when supported.
func agentCommand(agent string, planMode bool, prompt string) string {
	switch agent {
	case "codex":
		// Codex has no documented plan-mode flag; launch in its default mode.
		cmd := "codex"
		if prompt != "" {
			cmd += " " + shellQuote(prompt)
		}
		return cmd
	default: // claude
		cmd := "claude --dangerously-skip-permissions"
		if planMode {
			cmd += " --permission-mode plan"
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
