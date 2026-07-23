package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

// maxSlugLen caps a title-derived slug so the branch names and directory
// components built from it stay well under the filesystem's 255-byte limit. A
// prompt with no line breaks becomes its own title (see promptTitle), so a long
// single-paragraph prompt would otherwise slugify to a 300+ char directory name
// and fail mkdir with ENAMETOOLONG.
const maxSlugLen = 60

// titleSlug slugifies a title and truncates it to maxSlugLen, cutting back to
// the last dash so the name ends on a whole word rather than mid-token.
func titleSlug(s string) string {
	slug := slugify(s)
	if len(slug) <= maxSlugLen {
		return slug
	}
	slug = slug[:maxSlugLen]
	if i := strings.LastIndex(slug, "-"); i > 0 {
		slug = slug[:i]
	}
	return strings.Trim(slug, "-")
}

// imagePromptRe matches a pasted-image path anywhere in a prompt — the kind the
// UI inserts when you paste a screenshot. Mirrors the frontend's imagePathRE.
var imagePromptRe = regexp.MustCompile(`(?i)/[\w\-/.]+\.(?:png|jpe?g|gif|webp)`)

// promptTitle is the first meaningful line of a prompt — the source of the agent
// title (and thus its branch name, dir name, and workspace label). Pasted-image
// paths are stripped first, so a prompt that opens with a pasted screenshot
// still titles by the text the user typed rather than the image's file path.
func promptTitle(s string) string {
	s = imagePromptRe.ReplaceAllString(s, " ")
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
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
	// ?host= picks which host's settings to read/write — its OWN lasso.db, via
	// the provider (in-process for local, sqlite3 over SSH for a remote).
	// Defaults to the active host. Settings (defaults) come from that host's db;
	// last-used selections + the agent log are this lasso's local memory.
	host, ok := hostParam(r)
	if !ok {
		http.Error(w, "host not available", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		c, err := hostAgentConfig(host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, c)
	case http.MethodPost:
		// An empty default_agent is a valid choice ("no preset default, use the
		// last-used agent"), so each field is a pointer to tell "unset" from
		// "set to empty".
		var p defaultsPatch
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := hostSetDefaults(host, p); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		c, err := hostAgentConfig(host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
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
	host, ok := hostParam(r)
	if !ok {
		http.Error(w, "host not available", http.StatusBadRequest)
		return
	}
	var req repoConfigPatch
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	// Repo paths from the picker are absolute; the provider writes to the chosen
	// host's own db (and the remote CLI ~-expands against the remote home).
	rc, err := hostSetRepoConfig(host, path, req.CopyFiles, req.Setup)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
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
	host, ok := hostParam(r)
	if !ok {
		http.Error(w, "host not available", http.StatusBadRequest)
		return
	}
	root, repos, err := cachedHostReposList(host, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"root": root, "repos": repos})
}

// ---------------------------------------------------------------------------
// GET /api/repo-branches?path=… — local + remote branches and the default
// ---------------------------------------------------------------------------

func serveRepoBranches(w http.ResponseWriter, r *http.Request) {
	host, ok := hostParam(r)
	if !ok {
		http.Error(w, "host not available", http.StatusBadRequest)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	// Branches need only git on the host's filesystem (no db), so run them on the
	// host's backend directly — no lasso CLI required on the remote.
	be, err := gridHostBackend(host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	local, remote, def := cachedBranchList(host, be, path, r.URL.Query().Get("refresh") == "1")
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
	// Host is the box to create the agent on ("local" or an ssh-config alias);
	// empty means the active host. The web flow sends the host picked in the
	// dialog so the create targets it DIRECTLY (via its own backend) rather than
	// depending on the UI's active host having been switched there first — in the
	// Grid view the active host is often a remote you merely navigated to, whose
	// herdr connection may be flaky, and creating against it produced spurious
	// "server unreachable" retry loops.
	Host         string `json:"host"`
	Type         string `json:"type"`   // "git" | "scratch"
	Prompt       string `json:"prompt"` // the agent's instruction; its first line is the title
	Title        string `json:"title"`  // optional explicit title override; defaults to the prompt's first line
	Repo         string `json:"repo"`
	BaseBranch   string `json:"base_branch"`
	BranchPrefix string `json:"branch_prefix"`
	BranchName   string `json:"branch_name"`
	Agent        string `json:"agent"`
	// Model is the harness's model selection (e.g. "opus", "gpt-5.1-codex");
	// empty means the harness's own default. Free text — passed through to the
	// CLI's --model flag, never validated against a list (model names churn
	// faster than lasso releases).
	Model string `json:"model"`
	// ExtraArgs are free-form CLI flags appended verbatim to the launch
	// command, after the flags lasso builds and before the prompt. The escape
	// hatch for any harness knob lasso doesn't model.
	ExtraArgs   string   `json:"extra_args"`
	Notes       string   `json:"notes"`
	PlanMode    bool     `json:"plan_mode"`
	Attachments []string `json:"attachments"` // filenames staged under UploadDir
	UploadDir   string   `json:"upload_dir"`  // staging dir returned by /api/agent-upload
	// NoFocus suppresses focusing the new agent's herdr pane. The web "New Agent"
	// flow leaves this false (an explicit "take me there"); the MCP create_agent
	// tool sets it so spawning an agent doesn't yank a watching user away from
	// their current pane.
	NoFocus bool `json:"no_focus"`
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
	// Create on the requested host (default: active). When it's the active host we
	// use the active backend as before; when it's a DIFFERENT host we target that
	// host's own backend directly (like the MCP create_agent tool + the
	// upload/paste handlers), so the agent lands where the user picked without the
	// UI having to switch its active host there first. That switch requirement is
	// what let a Grid-view create ride whatever (possibly dead/remote) host the
	// grid last focused and 502-loop; picking the backend from the request removes
	// the dependency entirely.
	host := req.Host
	if host == "" {
		host = curBackend().Name()
	}
	if !gridHostAllowed(host) {
		// Not a transient-tunnel failure — surface it (a retry can't help), so the
		// browser shows a real error instead of the "resubmitting…" spinner.
		http.Error(w, "host not available", http.StatusBadRequest)
		return
	}
	be := curBackend()
	if host != be.Name() {
		var err error
		if be, err = gridHostBackend(host); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	rec, err := createAgent(be, req)
	if err != nil {
		// Log server-side too: a 502 the browser shows is otherwise invisible in
		// lasso's own log, which made the restart-mid-create race a forensic dig.
		log.Printf("create-agent: %v", err)
		var ce *createErr
		if errors.As(err, &ce) {
			http.Error(w, ce.Error(), ce.code)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rec)
}

// createErr carries an HTTP status hint so serveCreateAgent preserves the status
// codes the handler historically returned; the MCP create_agent tool ignores the
// code and surfaces only the message.
type createErr struct {
	code int
	err  error
}

func (e *createErr) Error() string { return e.err.Error() }

// createAgent runs the synchronous half of the New-Agent flow on backend b (host
// = b.Name()): it composes the branch, creates the worktree/workspace, and
// persists the agent record — then returns as soon as those durable facts exist
// (the id, workspace, and root pane the caller needs). The slow boot work —
// copying repo files, moving attachments, running setup, launching the agent CLI,
// and waiting for its pane — happens afterward in bootAgent, off the response's
// critical path, so the create_agent tool honors its "returns immediately" promise
// even when the boot is slow. Shared by serveCreateAgent (active host) and the MCP
// create_agent tool (any host, via gridHostBackend) — so every herdr/file call
// goes through b rather than the package-level helpers that always hit the active
// host.
func createAgent(b Backend, req createAgentReq) (AgentRecord, error) {
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		req.Title = promptTitle(req.Prompt)
	}
	if req.Title == "" {
		return AgentRecord{}, &createErr{http.StatusBadRequest, errors.New("prompt required")}
	}
	if req.Agent == "" {
		req.Agent = "claude"
	}
	host := b.Name()
	slug := titleSlug(req.Title)

	rec := AgentRecord{
		ID:          strconv.FormatInt(time.Now().UnixNano(), 36),
		Host:        host,
		Title:       req.Title,
		Type:        req.Type,
		Agent:       req.Agent,
		Model:       strings.TrimSpace(req.Model),
		ExtraArgs:   strings.TrimSpace(req.ExtraArgs),
		Description: req.Prompt,
		Notes:       strings.TrimSpace(req.Notes),
		Attachments: req.Attachments,
		PlanMode:    req.PlanMode,
		CreatedAt:   time.Now(),
		// The agent's CLI is launched asynchronously by bootAgent after we return,
		// so the record starts life "booting"; bootAgent flips it to ready/failed.
		BootStatus: BootBooting,
	}

	switch req.Type {
	case "git":
		repo := expandTildeOn(b, req.Repo)
		if repo == "" {
			return AgentRecord{}, &createErr{http.StatusBadRequest, errors.New("repo required for a git agent")}
		}
		// Compose the branch from prefix + name (auto-slug fallback).
		name := strings.TrimSpace(req.BranchName)
		if name == "" {
			name = slug
		}
		prefix := strings.TrimRight(strings.TrimSpace(req.BranchPrefix), "/")
		branch := name
		if prefix != "" {
			branch = prefix + "/" + name
		}

		// Resume-or-redo: a prior create of this exact branch that never reached a
		// workspace (lasso died mid-create, or the create RPC failed) left an
		// interrupted record — and possibly the branch + worktree on disk, since
		// herdr may well have finished the work before the response was lost. The
		// modal resends the same generated branch name on retry, so instead of
		// suffixing -2 next to an orphan, pick the interrupted attempt back up.
		var adoptDir string
		if old, ok := findInterruptedCreate(host, repo, branch); ok {
			_ = deleteAgentRecord(old.ID, host) // superseded by this attempt's record
			if branchExists(b, repo, branch) {
				if _, err := b.Stat(old.WorkDir); old.WorkDir != "" && err == nil {
					adoptDir = old.WorkDir // worktree already on disk: reattach, don't re-create
				} else {
					// Branch exists but its worktree dir is gone — not resumable;
					// treat it like any other branch collision.
					branch = uniqueBranch(b, repo, branch)
				}
			}
			// Branch absent → herdr never ran the create; the name is free to reuse.
		} else {
			branch = uniqueBranch(b, repo, branch)
		}

		base := strings.TrimSpace(req.BaseBranch)
		if base == "" {
			base = "HEAD"
		}
		workDir := adoptDir
		if workDir == "" {
			// Nest the worktree under the repo's name so worktrees from different
			// repos don't share one flat namespace (and don't collide on slug).
			repoSlug := slugify(filepath.Base(repo))
			if repoSlug == "" {
				repoSlug = "repo"
			}
			parent := filepath.Join(lassoWorktreesDirFor(b), repoSlug)
			workDir = uniqueChildDir(parent, worktreeDirSlug(branch, slug))
		}
		rec.Repo, rec.BaseBranch, rec.Branch, rec.WorkDir = repo, base, branch, workDir

		// Write-ahead: persist the record BEFORE the herdr call, so a create that
		// dies mid-flight (a lasso restart during `lasso update`, a dropped SSH
		// forward) leaves a visible, resumable record instead of an untracked
		// branch + worktree the next attempt then collides with.
		rec.BootStatus = BootCreating
		if err := appendAgent(host, rec); err != nil {
			return AgentRecord{}, &createErr{http.StatusInternalServerError, fmt.Errorf("save agent: %w", err)}
		}

		var ws, pane string
		var err error
		if adoptDir != "" {
			ws, pane, err = attachWorkspaceAt(b, workDir, req.Title, !req.NoFocus)
		} else {
			var res json.RawMessage
			res, err = b.HerdrCall("worktree.create", map[string]any{
				"cwd":    repo,
				"branch": branch,
				"base":   base,
				"path":   workDir,
				"label":  req.Title,
				// Focus the new worktree's pane so the user lands on the agent as it
				// boots (the New Agent flow is an explicit "take me there"); suppressed
				// for MCP-spawned agents so they don't yank a watching user away.
				"focus": !req.NoFocus,
			})
			if err == nil {
				ws, pane = parseCreateResult(res)
			}
		}
		if err != nil {
			// Keep the write-ahead record (as failed, still workspace-less) so the
			// orphan is visible and a retry can adopt whatever herdr got done.
			_ = updateAgentBootStatus(rec.ID, host, BootFailed, fmt.Sprintf("worktree.create: %v", err))
			// 500, not 502/503/504: those codes signal "the browser couldn't reach
			// lasso, resubmitting the same branch is safe" (see the client's
			// isTransientCreateError + the resume-or-redo path above). A herdr RPC
			// that lasso DID reach and that failed is a definitive error — retrying
			// it just loops, so keep it out of the transient set.
			return AgentRecord{}, &createErr{http.StatusInternalServerError, fmt.Errorf("worktree.create: %w", err)}
		}
		rec.WorkspaceID, rec.RootPane = ws, pane

	case "scratch":
		// A scratch agent has no branch to carry a random suffix, so append one to
		// the dir itself — two same-titled scratch agents then get distinct dirs
		// (e.g. hey-boss-a3f9), the way worktrees stay distinct via their branch.
		// uniqueChildDir still guards the (astronomically unlikely) suffix clash.
		// (No resume path here: each attempt gets a fresh suffix, and the most an
		// interrupted attempt leaves behind is an empty dir + a failed record.)
		workDir := uniqueChildDir(lassoScratchDirFor(b), slug+"-"+randSuffix())
		rec.WorkDir = workDir
		rec.BootStatus = BootCreating
		if err := appendAgent(host, rec); err != nil {
			return AgentRecord{}, &createErr{http.StatusInternalServerError, fmt.Errorf("save agent: %w", err)}
		}
		if err := b.MkdirAll(workDir, 0o755); err != nil {
			_ = updateAgentBootStatus(rec.ID, host, BootFailed, fmt.Sprintf("mkdir %s: %v", workDir, err))
			return AgentRecord{}, &createErr{http.StatusInternalServerError, fmt.Errorf("mkdir %s: %w", workDir, err)}
		}
		res, err := b.HerdrCall("workspace.create", map[string]any{
			"cwd":   workDir,
			"label": req.Title,
			"focus": !req.NoFocus, // land on the new agent's pane as it boots (web flow); suppressed for MCP
		})
		if err != nil {
			_ = updateAgentBootStatus(rec.ID, host, BootFailed, fmt.Sprintf("workspace.create: %v", err))
			// 500, not 502: a reached-but-failed herdr call is definitive, not the
			// "lost response, safe to resubmit" case the client retries (see the
			// matching note in the git branch above).
			return AgentRecord{}, &createErr{http.StatusInternalServerError, fmt.Errorf("workspace.create: %w", err)}
		}
		ws, pane := parseCreateResult(res)
		rec.WorkspaceID, rec.RootPane = ws, pane

	default:
		return AgentRecord{}, &createErr{http.StatusBadRequest, errors.New(`type must be "git" or "scratch"`)}
	}

	rootPane := rec.RootPane

	// The create's durable facts exist — flip the write-ahead record to booting
	// (with its workspace/pane) BEFORE the async boot starts, so bootAgent can
	// update its status without racing this write (and a failed boot is never
	// lost). A save failure is fatal — an agent we can't recover from the log is
	// worse than none.
	rec.BootStatus = BootBooting
	if err := updateAgentCreated(rec.ID, host, rec.WorkspaceID, rec.RootPane, BootBooting); err != nil {
		return AgentRecord{}, &createErr{http.StatusInternalServerError, fmt.Errorf("save agent: %w", err)}
	}

	// Persist this host's remembered selections.
	if req.Type == "git" {
		_ = setLastRepo(host, rec.Repo)
		_ = setLastBaseBranch(host, rec.Repo, rec.BaseBranch)
		// The remembered base branch rides along in the repo listing, and the
		// worktree just added a branch — both cached views are now stale.
		invalidateRepoCache(host)
		invalidateBranchCache(host, rec.Repo)
	}
	_ = setLastAgent(host, rec.Agent)
	_ = setLastAgentType(host, rec.Type)
	// Remember the model per harness (an empty model — "use the harness
	// default" — is itself a remembered choice, so always write it).
	_ = setLastModel(host, rec.Agent, rec.Model)

	// Everything past here — staging repo files/attachments/notes into the work
	// dir, running the setup script, launching the agent CLI, and waiting for its
	// pane to come up — is slow boot work, not a durable fact the caller needs. Run
	// it in the background so create returns as soon as the worktree, record, and
	// root pane exist (the contract the create_agent tool promises). bootAgent
	// records its own outcome onto the persisted row; b is captured so the boot
	// always targets the host the agent was created on, even if the active host
	// changes. A pane-less create (herdr returned no root pane) has nowhere to boot,
	// so there's nothing to launch.
	if rootPane != "" {
		go bootAgent(b, host, rec, req.UploadDir)
	}
	return rec, nil
}

// bootAgent runs an agent's boot after createAgent has returned: it stages the
// work dir (repo files, attachments, notes), runs the repo/scratch setup script,
// then launches the agent CLI in the root pane. It records the outcome on the
// persisted record — BootReady on success, BootFailed (with the error) otherwise
// — so a boot that never comes up surfaces as a "failed" agent in get_agent /
// list_agents instead of a phantom healthy one. All the file/setup reads are
// best-effort and host-scoped (host = b.Name()); only the CLI launch is treated
// as the boot's success/failure signal.
func bootAgent(b Backend, host string, rec AgentRecord, uploadDir string) {
	// The files-to-copy and setup commands are per-repo (and per-scratch-default)
	// Settings, read from the target host's OWN lasso.db. Best-effort: a config we
	// can't read just means no copy/setup runs, never a failed boot.
	var setup string
	switch rec.Type {
	case "git":
		rc, _ := hostRepoConfig(host, rec.Repo)
		copyRepoFiles(b, rec.Repo, rec.WorkDir, rc.CopyFiles)
		setup = rc.Setup
	case "scratch":
		defaults, _ := hostDefaults(host)
		setup = defaults.ScratchSetup
	}
	// Move staged attachments into the work dir; write notes to NOTES.md.
	moveAttachments(b, uploadDir, rec.Attachments, rec.WorkDir)
	if rec.Notes != "" {
		_ = b.WriteFile(filepath.Join(rec.WorkDir, "NOTES.md"), []byte(rec.Notes+"\n"), 0o644)
	}

	opts := launchOpts{
		planMode:  rec.PlanMode,
		model:     rec.Model,
		extraArgs: rec.ExtraArgs,
		prompt:    agentPrompt(rec),
	}
	// A long or multi-line prompt can't ride the typed launch line (paneRun
	// types raw bytes into the pane — see needsPromptFile), so stage it to a
	// file the command expands at exec time. If staging fails, typing the
	// prompt inline WOULD garble the launch, so fail the boot instead.
	if needsPromptFile(opts.prompt, agentCommand(rec.Agent, opts)) {
		path, err := stageAgentPrompt(b, rec.ID, opts.prompt)
		if err != nil {
			log.Printf("agent %s on %s: boot failed: stage prompt: %v", rec.ID, host, err)
			_ = updateAgentBootStatus(rec.ID, host, BootFailed, "stage prompt: "+err.Error())
			return
		}
		opts.prompt, opts.promptFile = "", path
	}
	cmd := agentCommand(rec.Agent, opts)
	if err := launchAgentInPane(b, rec.RootPane, setup, cmd); err != nil {
		log.Printf("agent %s on %s: boot failed: %v", rec.ID, host, err)
		_ = updateAgentBootStatus(rec.ID, host, BootFailed, err.Error())
		return
	}
	_ = updateAgentBootStatus(rec.ID, host, BootReady, "")
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

// branchExists reports whether branch exists in the repo on cur's host.
func branchExists(cur Backend, repo, branch string) bool {
	out, err := cur.GitOut(repo, "branch", "--list", branch)
	return err == nil && strings.TrimSpace(out) != ""
}

// uniqueBranch returns branch, suffixing -2, -3, … until it doesn't match an
// existing branch in the repo.
func uniqueBranch(cur Backend, repo, branch string) string {
	if !branchExists(cur, repo, branch) {
		return branch
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", branch, i)
		if !branchExists(cur, repo, cand) {
			return cand
		}
	}
}

// findWorkspaceForDir looks for a live herdr workspace already rooted at
// workDir (worktree checkout path) and returns it with one of its panes — the
// case where an interrupted create fully completed herdr-side before the
// response was lost. ok is false when no workspace matches or its panes can't
// be resolved (the caller then creates a fresh workspace at the dir).
func findWorkspaceForDir(b Backend, workDir string) (wsID, paneID string, ok bool) {
	res, err := b.HerdrCall("workspace.list", map[string]any{})
	if err != nil {
		return "", "", false
	}
	var wl struct {
		Workspaces []struct {
			WorkspaceID string `json:"workspace_id"`
			Worktree    *struct {
				CheckoutPath string `json:"checkout_path"`
			} `json:"worktree"`
		} `json:"workspaces"`
	}
	if json.Unmarshal(res, &wl) != nil {
		return "", "", false
	}
	for _, w := range wl.Workspaces {
		if w.Worktree != nil && w.Worktree.CheckoutPath == workDir {
			wsID = w.WorkspaceID
			break
		}
	}
	if wsID == "" {
		return "", "", false
	}
	pres, err := b.HerdrCall("pane.list", map[string]any{})
	if err != nil {
		return "", "", false
	}
	var pl struct {
		Panes []pane `json:"panes"`
	}
	if json.Unmarshal(pres, &pl) != nil {
		return "", "", false
	}
	for _, p := range pl.Panes {
		if p.WorkspaceID == wsID {
			return wsID, p.PaneID, true
		}
	}
	return "", "", false
}

// attachWorkspaceAt reattaches a herdr workspace to a worktree dir left by an
// interrupted create: reuse the workspace herdr may already have for the dir,
// else create a fresh one rooted there (mirroring serveAgentReopen).
func attachWorkspaceAt(b Backend, workDir, label string, focus bool) (wsID, paneID string, err error) {
	if wsID, paneID, ok := findWorkspaceForDir(b, workDir); ok {
		if focus {
			_, _ = b.HerdrCall("workspace.focus", map[string]any{"workspace_id": wsID})
		}
		return wsID, paneID, nil
	}
	res, err := b.HerdrCall("workspace.create", map[string]any{
		"cwd":   workDir,
		"label": label,
		"focus": focus,
	})
	if err != nil {
		return "", "", err
	}
	wsID, paneID = parseCreateResult(res)
	return wsID, paneID, nil
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
// reqHostBackend resolves the backend a request targets via its ?host= param,
// defaulting to the active host. Used by the upload + paste handlers so an
// attachment or pasted image lands on the SELECTED host (the one the agent will
// run on), not wherever the active backend happens to point during form editing.
func reqHostBackend(r *http.Request) (Backend, error) {
	host, ok := hostParam(r)
	if !ok {
		return nil, fmt.Errorf("host %q not available", host)
	}
	return gridHostBackend(host)
}

// moveAttachments moves staged attachments into the agent's work dir. Staging
// and the work dir live on the SAME host (cur), so this is a host-local copy
// (local disk, or SFTP within the remote host) — never a cross-host transfer.
func moveAttachments(cur Backend, uploadDir string, names []string, dest string) {
	if uploadDir == "" || len(names) == 0 {
		return
	}
	staging := filepath.Join(lassoUploadsDirFor(cur), filepath.Base(uploadDir))
	for _, n := range names {
		base := filepath.Base(n)
		if base == "" || base == "." {
			continue
		}
		copyOnBackend(cur, filepath.Join(staging, base), filepath.Join(dest, base))
	}
	_ = cur.RemoveAll(staging)
}

// copyOnBackend copies a file from src to dst, both on the same backend (the
// local disk, or one remote host over SFTP).
func copyOnBackend(cur Backend, src, dst string) {
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

// agentPrompt builds the prompt handed to the agent: the full prompt verbatim
// (stored in Description; its first line is also the title), plus a pointer to
// any notes/attachments that landed in the work dir. Falls back to the title
// when no prompt body was stored (e.g. a title-only record).
func agentPrompt(rec AgentRecord) string {
	var b strings.Builder
	body := rec.Description
	if body == "" {
		body = rec.Title
	}
	b.WriteString(body)
	if rec.Notes != "" {
		b.WriteString("\n\nSee NOTES.md for additional notes.")
	}
	if len(rec.Attachments) > 0 {
		b.WriteString("\n\nAttachments: " + strings.Join(rec.Attachments, ", "))
	}
	return b.String()
}

// maxTypedLaunch is the largest launch command paneRun will type inline.
// pane.send_text delivers raw bytes to the pane's PTY, and the kernel TTY
// input queue is small (MAX_INPUT is 1024 bytes on macOS) — bytes the shell
// hasn't drained past that are silently dropped, which unbalances the
// prompt's quoting and leaves the remainder executing as shell fragments.
// 512 leaves ample headroom for echo/redraw latency while the shell drains.
const maxTypedLaunch = 512

// needsPromptFile reports whether a launch command must deliver its prompt via
// a staged file instead of inline on the typed command line. Two typed-delivery
// hazards force the file path: an embedded newline (each "\n"/"\r" typed at a
// shell is an accept-line, so every prompt line would execute as its own broken
// command) and sheer size (see maxTypedLaunch).
func needsPromptFile(prompt, cmd string) bool {
	return strings.ContainsAny(prompt, "\n\r") || len(cmd) > maxTypedLaunch
}

// stageAgentPrompt writes an agent's prompt to a lasso-owned file on backend b
// — under <lasso dir>/prompts, NOT the work dir, so it never dirties the
// agent's worktree — and returns its path for the launch line's "$(cat …)".
// closeAgentRecord removes the file when the agent is closed.
func stageAgentPrompt(b Backend, agentID, prompt string) (string, error) {
	path := agentPromptPath(b, agentID)
	if err := b.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := b.WriteFile(path, []byte(prompt), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// agentPromptPath is the staged-prompt file for one agent on backend b.
// Deterministic (lasso dir + agent id) so closeAgentRecord can remove it
// without the path being persisted anywhere.
func agentPromptPath(b Backend, agentID string) string {
	return filepath.Join(lassoDirFor(b), "prompts", agentID+".md")
}

// launchAgentInPane runs the optional setup script then the agent command in a
// freshly-created pane, then auto-accepts the agent's trust dialog. It first
// waits for the pane's shell to settle (waitPaneReady): a new pane is still
// sourcing its rc, and characters typed before it's ready get their leading
// bytes eaten (e.g. "bun i" arriving as "i"). Runs on b — the backend the agent
// was created on — so it never targets the wrong host if the active one changes.
//
// It returns an error when a pane write fails — the pane is gone or the herdr RPC
// itself errored, i.e. the agent never got its launch command and won't come up.
// bootAgent turns that into a BootFailed status. waitPaneReady and the trust
// auto-accept stay best-effort (a slow shell or an absent trust dialog is normal),
// so they don't fail the boot on their own.
func launchAgentInPane(b Backend, paneID, setup, agentCmd string) error {
	waitPaneReady(b, paneID)
	if s := strings.TrimSpace(setup); s != "" {
		if err := paneRun(b, paneID, s); err != nil {
			return fmt.Errorf("run setup: %w", err)
		}
	}
	if err := paneRun(b, paneID, agentCmd); err != nil {
		return fmt.Errorf("launch agent: %w", err)
	}
	// Both claude and codex show a per-directory trust dialog at boot that their
	// --dangerously-* flags do NOT bypass, leaving the agent blocked. Auto-accept
	// it so the agent boots straight into the task (the prompt rode along as a CLI
	// arg, so it proceeds once trust is granted).
	confirmAgentTrust(b, paneID)
	return nil
}

// waitPaneReady blocks until the pane's visible output stops changing (the shell
// finished sourcing its rc and settled at a prompt) or a timeout elapses, so the
// command we type next isn't raced by shell startup. Prompt-agnostic: it watches
// for the screen to stabilize rather than matching any particular prompt string.
func waitPaneReady(b Backend, paneID string) {
	deadline := time.Now().Add(10 * time.Second)
	var prev string
	stable := 0
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		res, err := b.HerdrCall("pane.read", map[string]any{
			"pane_id": paneID,
			"source":  "visible",
		})
		if err != nil {
			continue
		}
		var r struct {
			Read struct {
				Text string `json:"text"`
			} `json:"read"`
		}
		if json.Unmarshal(res, &r) != nil {
			continue
		}
		t := strings.TrimRight(r.Read.Text, " \t\n")
		if t != "" && t == prev {
			if stable++; stable >= 2 { // ~600ms unchanged → settled
				return
			}
		} else {
			stable = 0
		}
		prev = t
	}
}

// paneRun sends a command line into a pane's shell (text + Enter) — the
// pane.send_text behind `herdr pane run`. Targets a cooked-mode shell, where a
// trailing "\n" ends the line. The bytes land on the PTY raw, so the command
// must be short and single-line (see needsPromptFile) — embedded newlines
// submit fragments, and anything past the kernel TTY input queue is dropped.
// For submitting to an interactive agent TUI use
// paneSubmit instead — see why there. Returns the herdr RPC error so the caller
// (launchAgentInPane) can tell a boot that never reached the pane from one that
// did.
func paneRun(b Backend, paneID, command string) error {
	_, err := b.HerdrCall("pane.send_text", map[string]any{
		"pane_id": paneID,
		"text":    command + "\n",
	})
	return err
}

// paneSubmit types text into an interactive agent's pane (claude/codex TUI) and
// submits it as a turn. Two things make this fragile, both handled here:
//
//  1. Bracketed paste vs Enter. The TUIs run in raw mode with bracketed paste, so
//     a "\r"/"\n" appended to the message is pasted as a literal newline and the
//     turn never submits — the message just stacks in the input box. The Enter
//     must be its own send_text so it lands as a real keypress (the same
//     mechanism confirmAgentTrust uses to accept the trust dialog).
//
//  2. A race between the paste committing and the Enter. herdr delivers the paste
//     and the Enter as separate PTY writes; when the TUI is busy (mid-turn,
//     streaming tool output) it applies the bracketed paste a beat late, so an
//     Enter sent immediately after hits a still-empty composer and is a no-op —
//     the message then sits there unsubmitted, even after the agent goes idle.
//     This is why sending to an idle agent appeared to work while sending to a
//     busy one silently failed.
//
// So: send the paste, wait until the composer actually shows it (the paste
// committed), then send Enter — re-sending until the composer is observed empty,
// i.e. the turn really went through. A repeat Enter is harmless: it's a no-op on
// an empty composer and on one whose draft was already submitted.
func paneSubmit(b Backend, paneID, text string) {
	_, _ = b.HerdrCall("pane.send_text", map[string]any{
		"pane_id": paneID,
		"text":    text,
	})
	// Wait for the paste to land in the composer before pressing Enter, so we
	// don't submit an empty box. If we never see it (read failures, an unfamiliar
	// composer), fall through and try Enter anyway rather than dropping the turn.
	commit := time.Now().Add(3 * time.Second)
	for time.Now().Before(commit) {
		time.Sleep(150 * time.Millisecond)
		if !paneInputEmpty(b, paneID) {
			break
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _ = b.HerdrCall("pane.send_text", map[string]any{
			"pane_id": paneID,
			"text":    "\r",
		})
		time.Sleep(300 * time.Millisecond)
		if paneInputEmpty(b, paneID) || time.Now().After(deadline) {
			return
		}
	}
}

// paneInputEmpty reports whether the agent TUI's composer currently holds no
// pending draft — paneSubmit uses it to confirm a turn submitted rather than
// leaving the message parked in the input box. The composer sits between the last
// pair of horizontal-rule lines the TUI draws above its status footer; an empty
// box is just the prompt marker ("❯"/"›"/">") with nothing after it. When the
// composer can't be located it returns false (don't claim empty), so paneSubmit
// errs toward an extra harmless Enter rather than a dropped message.
func paneInputEmpty(b Backend, paneID string) bool {
	res, err := b.HerdrCall("pane.read", map[string]any{
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
	lines := strings.Split(r.Read.Text, "\n")
	isRule := func(s string) bool {
		t := strings.TrimSpace(s)
		return len([]rune(t)) >= 10 && strings.Trim(t, "─") == ""
	}
	last, prev := -1, -1
	for i, ln := range lines {
		if isRule(ln) {
			prev, last = last, i
		}
	}
	if prev < 0 || last <= prev {
		return false // composer geometry not found
	}
	box := strings.TrimSpace(strings.Join(lines[prev+1:last], ""))
	box = strings.TrimSpace(strings.TrimLeft(box, "❯›> "))
	return box == ""
}

// confirmAgentTrust watches a freshly-launched agent pane for its per-directory
// trust dialog (claude's "trust this folder" / codex's "trust the contents of
// this directory") and accepts it — both default to "Yes" and confirm on Enter.
// Neither agent's --dangerously-* flag bypasses this gate, so without it the
// agent sits blocked. Polls rather than sleeping a fixed time so it survives a
// slow setup script running before the agent; if the dir is already trusted the
// dialog never appears and this simply times out without sending anything.
func confirmAgentTrust(b Backend, paneID string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if paneShowsTrustPrompt(b, paneID) {
			// Enter confirms the highlighted default ("Yes").
			_, _ = b.HerdrCall("pane.send_text", map[string]any{
				"pane_id": paneID,
				"text":    "\r",
			})
			return
		}
	}
}

// paneShowsTrustPrompt reports whether the pane's visible screen currently shows
// claude's or codex's directory-trust dialog.
func paneShowsTrustPrompt(b Backend, paneID string) bool {
	res, err := b.HerdrCall("pane.read", map[string]any{
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
// the SELECTED host's ~/.lasso/uploads (?host=, default active) and returns its
// id + the stored filenames. create-agent later moves these into the new agent's
// work dir on that same host, so the file never makes a cross-host hop and the
// flow is identical regardless of which host the agent runs on.
func serveAgentUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	be, err := reqHostBackend(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 200<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "parse multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := strconv.FormatInt(time.Now().UnixNano(), 36)
	staging := filepath.Join(lassoUploadsDirFor(be), id)
	if err := be.MkdirAll(staging, 0o755); err != nil {
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
		out, err := be.Create(filepath.Join(staging, name))
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
