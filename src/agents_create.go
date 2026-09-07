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
// `git worktree add` + `luvus workspace open` + `luvus pane run "claude …"`
// recipe in the embedded terminal. Two flavors, mirroring fulcrum's git/scratch
// tasks:
//
//   - git agent     → a git worktree off a chosen repo/base branch. lasso cuts
//                     the worktree itself (createWorktree) and then opens it as
//                     a workspace (openWorkspaceAt), which is where the root
//                     pane comes from. We then copy any configured files in, run
//                     the repo's setup script, and launch the agent — all in
//                     that pane's shell.
//   - scratch agent → a plain workspace rooted at a fresh ~/.lasso/scratch dir,
//                     then the scratch setup script + agent.
//
// Every runtime call goes through the request's own Backend so it targets the
// host the agent was asked for; settings + records persist locally via config.go.

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

// untitledAgent names an agent created with neither a prompt nor a title. The
// prompt is optional — spawning an agent that just sits at its CLI waiting for
// you is a legitimate create — but the title is not: it is the workspace label,
// the branch name and the work dir's slug. So a promptless create gets this
// placeholder instead of a 400, and uniqueBranch/uniqueChildDir keep successive
// ones apart (untitled-agent, untitled-agent-2, …). It is deliberately NOT
// handed to the agent as an instruction — see agentPrompt.
const untitledAgent = "Untitled agent"

// uniqueChildDir returns an absolute path under parent based on slug, suffixing
// -2, -3, … if the name is already taken on the active host.
func uniqueChildDir(parent, slug string) string {
	if slug == "" {
		slug = "agent"
	}
	cur := defaultBackend()
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
	be, err := hostBackend(host)
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
	// empty means the active host. The New Agent dialog and MCP create_agent tool
	// name a host explicitly, so creation targets that host's backend directly
	// rather than requiring an active-host switch first. That prevents a request
	// from riding an unrelated, possibly unavailable active backend.
	Host string `json:"host"`
	Type string `json:"type"` // "git" | "scratch"
	// Prompt is the agent's initial instruction, and its first line is the
	// title. Optional: an agent created with no prompt boots its CLI idle,
	// waiting for whoever opens the pane (or a send_agent).
	Prompt string `json:"prompt"`
	// Title is an optional explicit override; it defaults to the prompt's first
	// line, and to untitledAgent when there is no prompt to take one from.
	Title        string `json:"title"`
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
	// Effort is the harness's thinking/reasoning-effort level (e.g. "high").
	// Unlike Model it IS validated — against the harness's EffortLevels — since
	// a level the CLI doesn't know kills the launch (see normalizeEffort).
	// Empty means the CLI's own default.
	Effort string `json:"effort"`
	// ExtraArgs are free-form CLI flags appended verbatim to the launch
	// command, after the flags lasso builds and before the prompt. The escape
	// hatch for any harness knob lasso doesn't model.
	ExtraArgs   string   `json:"extra_args"`
	Notes       string   `json:"notes"`
	PlanMode    bool     `json:"plan_mode"`
	Attachments []string `json:"attachments"` // filenames staged under UploadDir
	UploadDir   string   `json:"upload_dir"`  // staging dir returned by /api/agent-upload
	// NoFocus suppresses focusing the new agent's luvus pane. The web "New Agent"
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
	// Create on the host the request names, else the calling tab's own host, else
	// the default. Every host uses its own pooled backend directly, as do the MCP
	// create_agent tool and the upload/paste handlers, so creation never depended
	// on a UI host switch and does not now depend on which tab is asking.
	host := req.Host
	if host == "" {
		host = requestHost(r)
	}
	if host == "" {
		host = defaultBackend().Name()
	}
	if !hostAllowed(host) {
		// Not a transient-tunnel failure — surface it (a retry can't help), so the
		// browser shows a real error instead of the "resubmitting…" spinner.
		http.Error(w, "host not available", http.StatusBadRequest)
		return
	}
	be, err := namedHostBackend(host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
// create_agent tool (any host, via hostBackend) — so every luvus/file call
// goes through b rather than the package-level helpers that always hit the active
// host.
func createAgent(b Backend, req createAgentReq) (AgentRecord, error) {
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.Title = strings.TrimSpace(req.Title)
	// A title the caller chose is theirs to keep; only a title we derived from
	// the prompt's first line is a candidate for auto-titling (see autotitle.go).
	derivedTitle := req.Title == ""
	if req.Title == "" {
		req.Title = promptTitle(req.Prompt)
	}
	if req.Title == "" {
		// Neither a prompt nor a title: the agent still needs a name to hang its
		// workspace label, branch and work dir on (see untitledAgent). Nothing to
		// auto-title from either — autoTitleAgent reads the prompt body, and the
		// guard below skips it when there is none.
		req.Title = untitledAgent
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
		Effort:      normalizeEffort(req.Agent, req.Effort),
		ExtraArgs:   strings.TrimSpace(req.ExtraArgs),
		Description: req.Prompt,
		Notes:       strings.TrimSpace(req.Notes),
		Attachments: req.Attachments,
		PlanMode:    normalizePlanMode(req.Agent, req.PlanMode),
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
		// luvus may well have finished the work before the response was lost. The
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
			// Branch absent → luvus never ran the create; the name is free to reuse.
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

		// Write-ahead: persist the record BEFORE the worktree exists, so a create
		// that dies mid-flight (a lasso restart during `lasso update`, a dropped
		// SSH forward) leaves a visible, resumable record instead of an untracked
		// branch + worktree the next attempt then collides with.
		rec.BootStatus = BootCreating
		if err := appendAgent(host, rec); err != nil {
			return AgentRecord{}, &createErr{http.StatusInternalServerError, fmt.Errorf("save agent: %w", err)}
		}

		var ws, pane string
		var err error
		if adoptDir != "" {
			ws, pane, err = openWorkspaceAt(b, workDir, req.Title, !req.NoFocus)
		} else {
			// lasso cuts the worktree itself rather than through the runtime.
			// UHP's worktree.create takes ONLY a branch name: it derives the
			// repository from whichever workspace happens to be active, plants the
			// checkout at its own ~/.luvus/worktrees/<repo>/<branch>, ignores any
			// base revision, and answers with neither a workspace nor a pane. All
			// four are things this flow decides — the repo the user picked, the
			// base branch they picked, a path under lasso's own worktrees tree, and
			// the workspace + root pane the record is built from. So the git plumbing
			// runs on the backend directly and the result is then OPENED as a
			// workspace, which is the same two facts luvus's worktree.create used to
			// return in one call.
			if err = createWorktree(b, repo, branch, base, workDir); err == nil {
				ws, pane, err = openWorkspaceAt(b, workDir, req.Title, !req.NoFocus)
			}
		}
		if err != nil {
			// Keep the write-ahead record (as failed, still workspace-less) so the
			// orphan is visible and a retry can adopt whatever got done.
			_ = updateAgentBootStatus(rec.ID, host, BootFailed, fmt.Sprintf("create worktree: %v", err))
			// 500, not 502/503/504: those codes signal "the browser couldn't reach
			// lasso, resubmitting the same branch is safe" (see the client's
			// isTransientCreateError + the resume-or-redo path above). A git command
			// or RPC that lasso DID reach and that failed is a definitive error —
			// retrying it just loops, so keep it out of the transient set.
			return AgentRecord{}, &createErr{http.StatusInternalServerError, fmt.Errorf("create worktree: %w", err)}
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
		ws, pane, err := openWorkspaceAt(b, workDir, req.Title, !req.NoFocus)
		if err != nil {
			_ = updateAgentBootStatus(rec.ID, host, BootFailed, fmt.Sprintf("workspace.open: %v", err))
			// 500, not 502: a reached-but-failed RPC is definitive, not the "lost
			// response, safe to resubmit" case the client retries (see the matching
			// note in the git branch above).
			return AgentRecord{}, &createErr{http.StatusInternalServerError, fmt.Errorf("workspace.open: %w", err)}
		}
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

	// Everything past here — staging repo files/attachments/notes into the work
	// dir, running the setup script, launching the agent CLI, and waiting for its
	// pane to come up — is slow boot work, not a durable fact the caller needs. Run
	// it in the background so create returns as soon as the worktree, record, and
	// root pane exist (the contract the create_agent tool promises). bootAgent
	// records its own outcome onto the persisted row; b is captured so the boot
	// always targets the host the agent was created on, even if the active host
	// changes. A pane-less create (luvus returned no root pane) has nowhere to boot,
	// so there's nothing to launch.
	if rootPane != "" {
		go bootAgent(b, host, rec, req.UploadDir)
	}

	// Rename the agent to something a sidebar can show, off the prompt's whole
	// content rather than its first line. Concurrent with the boot: it asks a
	// local agent CLI (seconds), touches nothing the boot touches, and no caller
	// waits on the result. Only the display name moves — see autotitle.go.
	if derivedTitle && rec.WorkspaceID != "" && rec.Description != "" && autoTitleEnabled() {
		go autoTitleAgent(b, host, rec)
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
		effort:    rec.Effort,
		extraArgs: rec.ExtraArgs,
		prompt:    agentPrompt(rec),
	}
	// A harness lasso reaches through a config overlay (omp) needs it on disk
	// before its launch line can point --config at it. Staged first, because the
	// resulting path lengthens the command and so feeds the needsPromptFile budget
	// below. Every omp agent gets one — it carries the theme pin, which has to
	// beat whatever a running omp last wrote into the user's config.yml — and a
	// plan agent's also carries the plan keys.
	//
	// So the failure handling splits: for a PLAN agent, launching anyway would
	// produce an agent that never planned under a record that says it did, the
	// exact lie normalizePlanMode exists to prevent, so the boot fails. For
	// everyone else the overlay is cosmetic, and an agent that boots in the wrong
	// theme beats an agent that doesn't boot.
	if harnessByID(rec.Agent).stagesConfigOverlay {
		path, err := stageOmpConfig(b, rec.ID, opts.planMode)
		switch {
		case err == nil:
			opts.configOverlay = path
		case opts.planMode:
			log.Printf("agent %s on %s: boot failed: stage omp config: %v", rec.ID, host, err)
			_ = updateAgentBootStatus(rec.ID, host, BootFailed, "stage omp config: "+err.Error())
			return
		default:
			log.Printf("agent %s on %s: stage omp config: %v (launching without the theme pin)", rec.ID, host, err)
		}
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

// createWorktree cuts a git worktree for branch at path, off base, in repo — on
// cur's host, through the same `git -C` channel the branch listing already uses.
//
// This is deliberately git rather than UHP's worktree.create, which accepts a
// branch name and nothing else: the repository it acts on is whichever
// workspace is active, the checkout lands in its own state directory, `base` has
// no representation at all, and the reply carries no workspace or pane. Every
// one of those is a decision this flow has already made.
//
// git creates the leaf directory but not its parents, and the path is nested
// per repo (worktrees/<repo>/<dir>), so the parent is made first.
func createWorktree(cur Backend, repo, branch, base, path string) error {
	if err := cur.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	// GitOut already folds git's stderr into the error (see gitOutLocal), which
	// is where "branch already exists" / "path already used by worktree" arrive.
	if _, err := cur.GitOut(repo, "worktree", "add", "-b", branch, path, base); err != nil {
		return err
	}
	return nil
}

// parseWorkspaceIndex reads the 0-based workspace position out of a
// workspace.open reply (`{"type":"workspace","workspace":"2"}`). That index is
// the ONLY identifier the reply carries — the stable workspace_id has to be
// resolved from it — and it is a JSON string, matching workspace.list's own
// stringly-typed `workspace`/`display_position`.
func parseWorkspaceIndex(res json.RawMessage) (int, bool) {
	var r struct {
		Workspace json.Number `json:"workspace"`
	}
	if json.Unmarshal(res, &r) != nil || r.Workspace == "" {
		return 0, false
	}
	n, err := strconv.Atoi(r.Workspace.String())
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
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

// openWorkspaceAt opens or adopts dir and returns its stable workspace/pane IDs.
// Reusing an existing workspace is intentional for interrupted agent creation
// and history reopen; fresh bare terminals use terminal.backend.create instead.
//
// Three properties of the UHP call shape drive the implementation:
//
//   - It takes no label. Naming is a second call (workspace.rename), whose name
//     is capped at 40 characters, so an agent title has to be trimmed to fit.
//   - Its reply names only the workspace's 0-based POSITION, so the stable id
//     comes from matching that position in the workspace listing. Position, not
//     cwd: two workspaces may legitimately be rooted at one directory, and the
//     position is the one answer that is unambiguous.
//   - It ALWAYS focuses. There is no no-focus form, so honoring focus=false
//     means remembering the focused pane first and putting focus back after.
//
// The whole sequence therefore runs under the host's mutation lock: it reads
// focus, changes focus, and reads a position-indexed listing, none of which
// survives another client (or another lasso create) opening a workspace
// concurrently. openWorkspaceLocked is the same thing for a caller that already
// holds that lock (the lock is not reentrant), and which therefore also owns
// restoring focus.
func openWorkspaceAt(b Backend, dir, label string, focus bool) (wsID, paneID string, err error) {
	unlock := runtimeMutationLock(b)
	defer unlock()

	var prevFocus string
	if !focus {
		prevFocus = focusedPaneID(b)
	}
	wsID, paneID, err = openWorkspaceLocked(b, dir, label)
	if prevFocus != "" && prevFocus != paneID {
		// A restore that fails leaves focus on the workspace this call just
		// opened, which is exactly what the caller asked not to happen — so it
		// is reported rather than swallowed. It does not mask an earlier
		// failure: that one already explains why there is nothing to return.
		if _, ferr := b.LuvusCall("pane.focus", map[string]any{"pane": prevFocus}); ferr != nil && err == nil {
			err = fmt.Errorf("opened %s but could not restore focus to pane %s: %w", dir, prevFocus, ferr)
		}
	}
	return wsID, paneID, err
}

func openWorkspaceLocked(b Backend, dir, label string) (wsID, paneID string, err error) {
	res, err := b.LuvusCall("workspace.open", map[string]any{"path": dir})
	if err != nil {
		return "", "", err
	}
	idx, ok := parseWorkspaceIndex(res)
	if !ok {
		return "", "", fmt.Errorf("workspace.open answered without a workspace position: %s", res)
	}
	wss, err := runtimeWorkspaces(b)
	if err != nil {
		return "", "", fmt.Errorf("resolve the opened workspace: %w", err)
	}
	// The position must be checked against the directory before anything is
	// done with it. The host mutex serializes only LASSO's own mutations: any
	// other UHP client — the TUI, a second lasso, a person running `luvus
	// workspace close` — may reorder or close a workspace between the open's
	// index reply and this listing, at which point that index names somebody
	// else's workspace. Renaming it, and then launching an agent into its pane,
	// is the worst possible outcome, so a mismatch is refused outright.
	want := filepath.Clean(dir)
	for _, ws := range wss {
		if ws.Number == idx {
			if filepath.Clean(ws.Cwd) != want {
				return "", "", fmt.Errorf("workspace.open reported position %d, which now holds %q rather than %q — the workspace order changed underneath the open", idx, ws.Cwd, dir)
			}
			wsID = ws.WorkspaceID
			break
		}
	}
	if wsID == "" {
		return "", "", fmt.Errorf("workspace.open reported position %d, which no workspace holds", idx)
	}

	if name := workspaceName(label); name != "" {
		// Cosmetic, and deliberately not fatal: an agent whose workspace kept the
		// directory's own name is still the agent that was asked for.
		if _, err := b.LuvusCall("workspace.rename", map[string]any{
			"workspace_id": wsID,
			"name":         name,
		}); err != nil {
			log.Printf("workspace.rename %s to %q: %v", wsID, name, err)
		}
	}

	panes, err := runtimePanes(b)
	if err != nil {
		return wsID, "", fmt.Errorf("resolve the opened workspace's pane: %w", err)
	}
	for _, p := range panes {
		if p.WorkspaceID != wsID {
			continue
		}
		if paneID == "" || p.Focused {
			paneID = p.PaneID
		}
	}
	// A workspace with no pane is not a usable result: every caller here needs
	// somewhere to launch, and returning a nil error with an empty pane would
	// hand back a record pointing at nothing.
	if paneID == "" {
		return wsID, "", fmt.Errorf("workspace %s at %s holds no pane to run in", wsID, dir)
	}
	return wsID, paneID, nil
}

// workspaceName trims a label to something workspace.rename will accept: it
// rejects an empty name outright and caps at 40 characters. Agent titles are
// written for a sidebar row rather than for that budget (autoTitleMaxLen alone
// is 60), so the cap is cut back to a word boundary instead of mid-token.
func workspaceName(label string) string {
	label = strings.TrimSpace(label)
	if len([]rune(label)) <= workspaceNameMaxLen {
		return label
	}
	runes := []rune(label)[:workspaceNameMaxLen]
	if i := strings.LastIndex(string(runes), " "); i > 0 {
		return strings.TrimSpace(string(runes)[:i])
	}
	return strings.TrimSpace(string(runes))
}

// workspaceNameMaxLen is UHP's cap on a workspace (and tab) name; a longer one
// is refused with invalid_request rather than truncated for us.
const workspaceNameMaxLen = 40

// focusedPaneID is the pane the runtime currently has focused, or "" when that
// cannot be read. Used to put focus back after a workspace.open that a caller
// asked not to be taken to.
func focusedPaneID(b Backend) string {
	panes, err := runtimePanes(b)
	if err != nil {
		return ""
	}
	for _, p := range panes {
		if p.Focused {
			return p.PaneID
		}
	}
	return ""
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
// falling back to the calling tab's own host. Used by the upload + paste
// handlers so an attachment or pasted image lands on the SELECTED host (the one
// the agent will run on), not on whichever host the tab is currently viewing —
// and by the sidebar's file/diff endpoints, whose host selector arrives the same
// way. Since ?host= is also how a tab announces itself, the two collapse into
// one lookup here.
func reqHostBackend(r *http.Request) (Backend, error) {
	return namedHostBackend(requestHost(r))
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
// any notes/attachments that landed in the work dir.
//
// A record with no prompt body yields no prompt at all — every harness builder
// omits the operand for an empty prompt, so the CLI comes up idle. The title is
// NOT substituted for a missing body: it is a display label, and for a
// promptless create a placeholder one (untitledAgent), so typing it at the
// agent would invent a task nobody asked for. Notes and attachments still
// stand on their own — they were staged into the work dir, and saying so is
// the whole instruction when the user gave no other.
func agentPrompt(rec AgentRecord) string {
	var parts []string
	if rec.Description != "" {
		parts = append(parts, rec.Description)
	}
	if rec.Notes != "" {
		parts = append(parts, "See NOTES.md for additional notes.")
	}
	if len(rec.Attachments) > 0 {
		parts = append(parts, "Attachments: "+strings.Join(rec.Attachments, ", "))
	}
	return strings.Join(parts, "\n\n")
}

// maxTypedLaunch leaves headroom inside UHP's 1 MiB request frame. Larger
// prompts are staged in files; multiline prompts are always staged because
// pane.run submits newline-separated commands individually.
const maxTypedLaunch = 64 << 10

// needsPromptFile reports whether a launch command must deliver its prompt via
// a staged file instead of inline on the submitted command line. Two hazards
// force the file path: an embedded newline (pane.run submits a multi-line
// command as one command PER LINE, so every prompt line would execute as its
// own broken command — verified against UHP 0.13.4) and sheer size (see
// maxTypedLaunch).
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
// It returns an error when a pane write fails — the pane is gone or the luvus RPC
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
	// arg, so it proceeds once trust is granted). pi has a gate of the same shape
	// but a flag that settles it (--approve, on its launch line), so it never
	// reaches here; omp has none at all.
	confirmAgentTrust(b, paneID)
	return nil
}

// waitPaneReady blocks until the pane's visible output stops changing (the shell
// finished sourcing its rc and settled at a prompt) or a timeout elapses, so the
// command we submit next isn't raced by shell startup. Prompt-agnostic: it
// watches for the screen to stabilize rather than matching any particular prompt
// string.
func waitPaneReady(b Backend, paneID string) {
	deadline := time.Now().Add(10 * time.Second)
	var prev string
	stable := 0
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		text, ok := paneVisibleText(b, paneID)
		if !ok {
			continue
		}
		t := strings.TrimRight(text, " \t\n")
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

// paneRun submits one single-line shell command through UHP's queued action
// channel. Interactive agent composers use paneSubmit's raw input instead.
//
// Returns the RPC error so the caller (launchAgentInPane) can tell a boot that
// never reached the pane from one that did.
func paneRun(b Backend, paneID, command string) error {
	_, err := b.LuvusCall("pane.run", map[string]any{
		"pane":    paneID,
		"command": command,
	})
	return err
}

// paneSubmit types text into an interactive agent's pane and submits it as a
// turn. The caller supplies agentKind from the runtime's agent metadata so
// composer reads use the matching harness geometry.
//
// It goes through pane.send_input, UHP's raw-byte channel, NOT pane.run: the
// target is a raw-mode TUI's composer, not a shell reading a command line, and
// pane.run would submit the text as a shell action instead of pasting it.
//
// The text and Enter must be separate PTY writes: raw-mode TUIs treat a newline
// appended to bracketed paste as input, not an Enter key. After the paste lands,
// Enter is retried until the shared detector observes an empty composer; this
// handles a busy TUI applying the paste after its first Enter. It returns false
// without sending bytes when a positive composer-draft read catches a human's
// unsent input between the caller's guard and this submission.
func paneSubmit(b Backend, paneID, agentKind, text string) bool {
	if composerGuardEnabled() && paneComposerState(b, paneID, agentKind) == ComposerDraft {
		return false
	}
	_, _ = b.LuvusCall("pane.send_input", map[string]any{
		"pane": paneID,
		"text": text,
	})
	// Wait for the paste to land in the composer before pressing Enter, so we
	// don't submit an empty box. If we never see it (read failures, an unfamiliar
	// composer), fall through and try Enter anyway rather than dropping the turn.
	commit := time.Now().Add(3 * time.Second)
	for time.Now().Before(commit) {
		time.Sleep(150 * time.Millisecond)
		if !paneInputEmpty(b, paneID, agentKind) {
			break
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _ = b.LuvusCall("pane.send_input", map[string]any{
			"pane": paneID,
			"text": "\r",
		})
		time.Sleep(300 * time.Millisecond)
		if paneInputEmpty(b, paneID, agentKind) || time.Now().After(deadline) {
			return true
		}
	}
}

// paneComposerState reads only the pane's visible screen. A failed read remains
// unknown so callers fail open rather than starving a durable message queue.
func paneComposerState(b Backend, paneID, agentKind string) ComposerState {
	text, ok := paneVisibleText(b, paneID)
	if !ok {
		return ComposerUnknown
	}
	return detectComposer(agentKind, text)
}

// paneInputEmpty reports whether the shared detector positively recognizes an
// empty composer. Unknown deliberately remains false: paneSubmit then tries an
// extra Enter rather than treating an unparsed screen as a submitted turn.
func paneInputEmpty(b Backend, paneID, agentKind string) bool {
	return paneComposerState(b, paneID, agentKind) == ComposerEmpty
}

// confirmAgentTrust watches a freshly-launched agent pane for its per-directory
// trust dialog (claude's "trust this folder" / codex's "trust the contents of
// this directory") and accepts it. Neither agent's --dangerously-* flag bypasses
// this gate, so without it the agent sits blocked. Polls rather than sleeping a
// fixed time so it survives a slow setup script running before the agent; if the
// dir is already trusted the dialog never appears and this simply times out
// without sending anything.
//
// Enter alone is not enough: claude highlights "No, exit" by default when the
// folder pre-approves tool permissions (a .claude/settings.local.json), so a
// blind Enter quits the agent instead of launching it. We read which option the
// caret sits on and move it onto "Yes" first.
func confirmAgentTrust(b Backend, paneID string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if paneShowsTrustPrompt(b, paneID) {
			acceptTrustPrompt(b, paneID)
			return
		}
	}
}

// trustPromptNudges caps how many times we press Down looking for the "Yes"
// option. Both dialogs are two or three items, so a few presses cover a list
// that grew or wrapped around without hammering an unfamiliar screen.
const trustPromptNudges = 4

// acceptTrustPrompt answers a visible trust dialog. It presses Down until the
// caret sits on the "Yes" option, then Enter. A screen it cannot read, or one
// with no caret at all (codex, older claude), falls back to a bare Enter — the
// behavior that worked before the default flipped. A caret that stays on "No"
// after every nudge is left alone: pressing Enter there would exit the agent,
// so a human gets to answer instead.
func acceptTrustPrompt(b Backend, paneID string) {
	for i := 0; ; i++ {
		text, ok := paneVisibleText(b, paneID)
		if !ok {
			break
		}
		yes, found := trustPromptYesHighlighted(text)
		if !found || yes {
			break
		}
		if i >= trustPromptNudges {
			return
		}
		sendPaneKey(b, paneID, "\x1b[B") // Down
		time.Sleep(300 * time.Millisecond)
	}
	sendPaneKey(b, paneID, "\r")
}

// sendPaneKey types raw bytes into a pane, ignoring transport errors the way
// the rest of this dialog handling does — a missed keystroke shows up as a
// dialog still on screen, which the caller is already polling for. Raw bytes,
// so pane.send_input rather than pane.run: an escape sequence is a keypress in
// a TUI, not a shell command to submit.
func sendPaneKey(b Backend, paneID, text string) {
	_, _ = b.LuvusCall("pane.send_input", map[string]any{
		"pane": paneID,
		"text": text,
	})
}

// trustPromptYesHighlighted reports whether the trust dialog's selection caret
// sits on its "Yes" option. found is false when no caret line is on screen, so
// callers can tell "the No option is selected" from "this screen doesn't parse".
//
// The caret is claude's and codex's "❯"; ">" is accepted too, for terminals
// whose font substitutes it. Option lines can be wrapped in the dialog's box
// border, so leading border glyphs are trimmed, and a caret line only counts
// when it names a yes/no choice — that keeps a composer's "> " prompt from
// reading as a selection.
func trustPromptYesHighlighted(text string) (yes bool, found bool) {
	for _, line := range strings.Split(text, "\n") {
		s := strings.TrimSpace(strings.TrimLeft(line, " \t\r│┃|"))
		var rest string
		switch {
		case strings.HasPrefix(s, "❯"):
			rest = strings.TrimPrefix(s, "❯")
		case strings.HasPrefix(s, ">"):
			rest = strings.TrimPrefix(s, ">")
		default:
			continue
		}
		low := strings.ToLower(rest)
		switch {
		case strings.Contains(low, "yes"):
			return true, true
		case strings.Contains(low, "no"):
			return false, true
		}
	}
	return false, false
}

// paneVisibleText returns the pane's current screen (not its scrollback) as
// text. ok is false when the pane is gone, the RPC failed, or the reply didn't
// parse — so callers that scan the screen for a dialog answer "no dialog" on a
// read they couldn't make, rather than inventing one either way.
//
// pane.read answers with the visible screen and carries its text flat on the
// result; it takes no source selector (UHP ignores one).
func paneVisibleText(b Backend, paneID string) (string, bool) {
	res, err := b.LuvusCall("pane.read", map[string]any{"pane": paneID})
	if err != nil {
		return "", false
	}
	var r struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(res, &r) != nil {
		return "", false
	}
	return r.Text, true
}

// paneShowsTrustPrompt reports whether the pane's visible screen currently shows
// claude's or codex's directory-trust dialog.
func paneShowsTrustPrompt(b Backend, paneID string) bool {
	t, ok := paneVisibleText(b, paneID)
	if !ok {
		return false
	}
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
