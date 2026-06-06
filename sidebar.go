package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The sidebar's data + mutations. The React sidebar (replacing herdr's TUI) shows
// git repos from repos_root with their lasso worktrees nested as a tree, ordered
// by latest commit to the primary branch (pinned repos first), plus scratch
// workspaces, plus a flat agent list with live status. All local-only (multi-host
// is deferred): everything routes through curBackend() against host "local".

const sidebarHost = "local"

// newID returns a fresh base36 id (timestamp-based), the scheme used for agent,
// workspace, and tab ids throughout.
func newID() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }

// ---------------------------------------------------------------------------
// GET /api/tree — the repos→worktrees tree + scratch workspaces
// ---------------------------------------------------------------------------

type treeTab struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Kind    string `json:"kind"` // "shell" | "agent"
	AgentID string `json:"agent_id,omitempty"`
	Agent   string `json:"agent,omitempty"`  // "claude" | "codex"
	Status  string `json:"status,omitempty"` // agent tabs: idle|working|blocked|unknown
}

type treeWorkspace struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Repo    string    `json:"repo,omitempty"`
	WorkDir string    `json:"work_dir"`
	Kind    string    `json:"kind"`
	Pinned  bool      `json:"pinned"`
	Branch  string    `json:"branch,omitempty"`
	Tabs    []treeTab `json:"tabs"`
	// AgentStatus is the workspace's aggregate live-agent status for the sidebar
	// dot (blocked > working > idle), or "" when no tab is running an agent.
	AgentStatus string `json:"agent_status,omitempty"`
	AgentKind   string `json:"agent_kind,omitempty"`
}

type treeRepo struct {
	Path          string          `json:"path"`
	Name          string          `json:"name"`
	PrimaryBranch string          `json:"primary_branch"`
	Pinned        bool            `json:"pinned"`
	LastCommit    int64           `json:"last_commit"` // unix secs (ordering + display)
	Workspaces    []treeWorkspace `json:"workspaces"`  // linked worktrees only
	// The repo row is itself the main checkout (like herdr): clicking it opens a
	// terminal on the primary branch. MainTabID is its tab if one already exists,
	// else "" — the frontend then asks /api/repo/open to create one on click.
	MainWorkspaceID string `json:"main_workspace_id,omitempty"`
	MainTabID       string `json:"main_tab_id,omitempty"`
	AgentStatus     string `json:"agent_status,omitempty"`
	AgentKind       string `json:"agent_kind,omitempty"`
}

type treePayload struct {
	Repos   []treeRepo      `json:"repos"`
	Scratch []treeWorkspace `json:"scratch"`
}

func serveTree(w http.ResponseWriter, r *http.Request) {
	be := curBackend()
	_, repos, _ := reposList(be, sidebarHost)
	repoState, _ := listRepoState(sidebarHost)
	wss, _ := listWorkspaces(sidebarHost)
	statuses := agentStatuses.snapshot()
	kinds := tabAgentKinds() // tab id → live agent kind (process-based)

	byRepo := map[string][]treeWorkspace{}
	mainByRepo := map[string]treeWorkspace{} // the repo-root checkout, per repo
	scratch := []treeWorkspace{}
	for _, ws := range wss {
		tw := buildTreeWorkspace(be, ws, statuses, kinds)
		switch {
		case ws.Kind == "git" && ws.Repo != "" && ws.WorkDir == ws.Repo:
			// The main checkout (work_dir == repo root) IS the repo row, not a child.
			mainByRepo[ws.Repo] = tw
		case ws.Kind == "git" && ws.Repo != "":
			byRepo[ws.Repo] = append(byRepo[ws.Repo], tw) // linked worktree
		default:
			scratch = append(scratch, tw)
		}
	}

	// Repos shown = only those with a live workspace (a linked worktree or a
	// main checkout) — like herdr, which lists workspaces grouped by repo, not
	// every repo under repos_root. (New repos are reached via New Agent / ⌘K; a
	// settings allowlist to pin extra repos can layer on later.) reposList only
	// supplies display names + a stable order seed.
	nameOf := map[string]string{}
	for _, re := range repos {
		nameOf[re.Path] = re.Name
	}
	order := []string{}
	seen := map[string]bool{}
	for path := range byRepo {
		if !seen[path] {
			order = append(order, path)
			seen[path] = true
		}
	}
	for path := range mainByRepo {
		if !seen[path] {
			order = append(order, path)
			seen[path] = true
		}
	}

	out := make([]treeRepo, 0, len(order))
	for _, path := range order {
		name := nameOf[path]
		if name == "" {
			name = filepath.Base(path)
		}
		pinned := false
		if rc := repoState[path]; rc != nil {
			pinned = rc.Pinned
			if rc.DisplayName != "" {
				name = rc.DisplayName
			}
		}
		primary, ct := repoPrimaryBranchAndTime(be, path)
		repoWss := byRepo[path]
		if repoWss == nil {
			repoWss = []treeWorkspace{}
		}
		tr := treeRepo{
			Path: path, Name: name, PrimaryBranch: primary, Pinned: pinned,
			LastCommit: ct, Workspaces: repoWss,
		}
		if main, ok := mainByRepo[path]; ok {
			tr.MainWorkspaceID = main.ID
			tr.AgentStatus = main.AgentStatus
			tr.AgentKind = main.AgentKind
			if len(main.Tabs) > 0 {
				tr.MainTabID = main.Tabs[0].ID
			}
		}
		out = append(out, tr)
	}
	// Pinned first, then most-recently-committed, then name.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		if out[i].LastCommit != out[j].LastCommit {
			return out[i].LastCommit > out[j].LastCommit
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, treePayload{Repos: out, Scratch: scratch})
}

func buildTreeWorkspace(be Backend, ws Workspace, statuses, kinds map[string]string) treeWorkspace {
	tw := treeWorkspace{
		ID: ws.ID, Title: ws.Title, Repo: ws.Repo, WorkDir: ws.WorkDir,
		Kind: ws.Kind, Pinned: ws.Pinned, Tabs: []treeTab{},
	}
	if ws.Kind == "git" && ws.WorkDir != "" {
		if out, err := be.GitOut(ws.WorkDir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			tw.Branch = strings.TrimSpace(out)
		}
	}
	tabs, _ := listTabs(ws.ID)
	for _, t := range tabs {
		// Agent-ness is live (a process is running now), not the stored kind: a
		// tab whose agent exited renders as a shell; a shell where the user ran
		// claude renders as an agent.
		tt := treeTab{ID: t.ID, Title: t.Title, Kind: "shell"}
		if kind := kinds[t.ID]; kind != "" {
			tt.Kind = "agent"
			tt.Agent = kind
			tt.AgentID = t.AgentID
			tt.Status = statuses[t.ID]
			if tt.Status == "" {
				tt.Status = string(StatusIdle)
			}
			tw.AgentStatus = mergeAgentStatus(tw.AgentStatus, tt.Status)
			if tw.AgentKind == "" {
				tw.AgentKind = kind
			}
		}
		tw.Tabs = append(tw.Tabs, tt)
	}
	return tw
}

// mergeAgentStatus keeps the most "attention-worthy" status across a workspace's
// tabs for its sidebar dot: blocked > working > idle.
func mergeAgentStatus(cur, next string) string {
	rank := map[string]int{
		string(StatusBlocked): 3, string(StatusWorking): 2, string(StatusIdle): 1,
	}
	if rank[next] > rank[cur] {
		return next
	}
	return cur
}

// repoPrimaryBranchAndTime resolves a repo's primary branch (origin HEAD, else
// main/master) and the unix time of its latest commit, for sidebar ordering.
func repoPrimaryBranchAndTime(be Backend, repo string) (string, int64) {
	primary := gitDefaultBranch(be, repo)
	cands := []string{}
	if primary != "" {
		cands = append(cands, primary)
	}
	cands = append(cands, "main", "master", "HEAD")
	for _, b := range cands {
		out, err := be.GitOut(repo, "log", "-1", "--format=%ct", b)
		if err != nil {
			continue
		}
		if ts := strings.TrimSpace(out); ts != "" {
			n, _ := strconv.ParseInt(ts, 10, 64)
			if primary == "" || primary == "HEAD" {
				primary = b
			}
			return primary, n
		}
	}
	if primary == "" {
		primary = "main"
	}
	return primary, 0
}

// ---------------------------------------------------------------------------
// GET /api/agents — flat agent list with live status (sidebar pane + ⌘K)
// ---------------------------------------------------------------------------

type agentRow struct {
	TabID          string `json:"tab_id"`
	AgentID        string `json:"agent_id"`
	Title          string `json:"title"`
	Agent          string `json:"agent"`
	Status         string `json:"status"`
	WorkspaceID    string `json:"workspace_id"`
	WorkspaceTitle string `json:"workspace_title"`
	Repo           string `json:"repo,omitempty"`
	Cwd            string `json:"cwd"`
	Prompt         string `json:"prompt,omitempty"` // initial prompt, for ⌘K search
}

func serveAgentsList(w http.ResponseWriter, r *http.Request) {
	statuses := agentStatuses.snapshot()
	kinds := tabAgentKinds() // only tabs running an agent right now
	agentByID := map[string]AgentRecord{}
	if agents, err := listAgents(sidebarHost); err == nil {
		for _, a := range agents {
			agentByID[a.ID] = a
		}
	}
	out := make([]agentRow, 0, len(kinds))
	for tabID, kind := range kinds {
		t, err := getTab(tabID)
		if err != nil {
			continue
		}
		rec := agentByID[t.AgentID]
		title := t.Title
		if title == "" {
			title = rec.Title
		}
		status := statuses[tabID]
		if status == "" {
			status = string(StatusIdle)
		}
		ws, _ := getWorkspace(t.WorkspaceID)
		out = append(out, agentRow{
			TabID: tabID, AgentID: t.AgentID, Title: title, Agent: kind, Status: status,
			WorkspaceID: t.WorkspaceID, WorkspaceTitle: ws.Title, Repo: ws.Repo,
			Cwd: t.Cwd, Prompt: rec.Description,
		})
	}
	// Stable order: workspace title, then tab title.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].WorkspaceTitle != out[j].WorkspaceTitle {
			return out[i].WorkspaceTitle < out[j].WorkspaceTitle
		}
		return out[i].Title < out[j].Title
	})
	writeJSON(w, map[string]any{"agents": out})
}

// ---------------------------------------------------------------------------
// mutations
// ---------------------------------------------------------------------------

// decodeJSON decodes the request body into v, writing a 400 on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// serveTabRename renames a tab (covers renaming an agent: its tab title drives
// both the sidebar render and ⌘K search).
func serveTabRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TabID string `json:"tab_id"`
		Title string `json:"title"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.TabID == "" || strings.TrimSpace(req.Title) == "" {
		http.Error(w, "tab_id and non-empty title required", http.StatusBadRequest)
		return
	}
	if err := renameTab(req.TabID, strings.TrimSpace(req.Title)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	kickHub()
	writeJSON(w, map[string]any{"ok": true})
}

// serveWorkspaceRenameDB renames a workspace in the DB (the new tmux-era handler;
// the old herdr serveWorkspaceRename is removed in cleanup).
func serveWorkspaceRenameDB(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		Title       string `json:"title"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.WorkspaceID == "" || strings.TrimSpace(req.Title) == "" {
		http.Error(w, "workspace_id and non-empty title required", http.StatusBadRequest)
		return
	}
	if err := renameWorkspace(req.WorkspaceID, strings.TrimSpace(req.Title)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	kickHub()
	writeJSON(w, map[string]any{"ok": true})
}

// serveTabClose closes one tab: kill its tmux session, detach its viewer, drop
// its cached status, and soft-close the row.
func serveTabClose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TabID string `json:"tab_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.TabID == "" {
		http.Error(w, "tab_id required", http.StatusBadRequest)
		return
	}
	closeOneTab(req.TabID)
	kickHub()
	writeJSON(w, map[string]any{"ok": true})
}

// closeOneTab tears down a single tab's runtime + persistence. Unsees the tab so
// the exit watcher doesn't treat this deliberate close as a shell-exit (which
// would also close the workspace).
func closeOneTab(tabID string) {
	unsee(tabID)
	releaseTabTerm(tabID)
	_ = tmuxKillSession(tabSession(tabID))
	agentStatuses.forget(tabID)
	_ = closeTab(tabID)
}

// serveWorkspaceClose closes a workspace and all its tabs.
func serveWorkspaceClose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.WorkspaceID == "" {
		http.Error(w, "workspace_id required", http.StatusBadRequest)
		return
	}
	tabs, _ := listTabs(req.WorkspaceID)
	for _, t := range tabs {
		closeOneTab(t.ID)
	}
	_ = closeWorkspace(req.WorkspaceID)
	kickHub()
	writeJSON(w, map[string]any{"ok": true})
}

// serveNewTab adds a plain shell tab to a workspace (a new tmux session in the
// workspace's directory).
func serveNewTab(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		Title       string `json:"title"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ws, err := getWorkspace(req.WorkspaceID)
	if err != nil {
		http.Error(w, "workspace not found", http.StatusNotFound)
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "shell"
	}
	tabID := newID()
	if err := tmuxNewSession(tabSession(tabID), ws.WorkDir, []string{"LASSO_TAB_ID=" + tabID}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tab := Tab{
		ID: tabID, WorkspaceID: ws.ID, Title: title, Cwd: ws.WorkDir,
		Kind: "shell", Ordinal: nextTabOrdinal(ws.ID), CreatedAt: time.Now(),
	}
	if err := insertTab(tab); err != nil {
		_ = tmuxKillSession(tabSession(tabID))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	kickHub()
	writeJSON(w, tab)
}

// serveOpenRepo opens a terminal on a repo's primary branch — the repo's main
// checkout (work_dir == repo root). Like herdr, a repo row isn't just a grouping
// of worktrees: it's itself a workspace. Returns the tab to select, creating the
// main-checkout workspace + a shell tab on first open.
func serveOpenRepo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo string `json:"repo"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	be := curBackend()
	repo := expandTildeOn(be, req.Repo)
	if repo == "" {
		http.Error(w, "repo required", http.StatusBadRequest)
		return
	}
	// Reuse the existing main-checkout workspace if there is one.
	if wss, err := listWorkspaces(sidebarHost); err == nil {
		for _, ws := range wss {
			if ws.Kind == "git" && ws.WorkDir == repo {
				if tabs, _ := listTabs(ws.ID); len(tabs) > 0 {
					writeJSON(w, map[string]any{"tab_id": tabs[0].ID, "workspace_id": ws.ID})
					return
				}
				// Workspace exists but has no live tab — fall through to add one.
				tabID := newID()
				if err := tmuxNewSession(tabSession(tabID), repo, []string{"LASSO_TAB_ID=" + tabID}); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				_ = insertTab(Tab{ID: tabID, WorkspaceID: ws.ID, Title: "shell", Cwd: repo, Kind: "shell", CreatedAt: time.Now()})
				kickHub()
				writeJSON(w, map[string]any{"tab_id": tabID, "workspace_id": ws.ID})
				return
			}
		}
	}
	// Create the main-checkout workspace + an initial shell tab at the repo root.
	wsID := "w" + newID()
	tabID := newID()
	if err := tmuxNewSession(tabSession(tabID), repo, []string{"LASSO_TAB_ID=" + tabID}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	title := filepath.Base(repo)
	if rc, err := getRepoState(sidebarHost, repo); err == nil && rc.DisplayName != "" {
		title = rc.DisplayName
	}
	_ = insertWorkspace(Workspace{ID: wsID, Host: sidebarHost, Title: title, Repo: repo, WorkDir: repo, Kind: "git", CreatedAt: now})
	_ = insertTab(Tab{ID: tabID, WorkspaceID: wsID, Title: "shell", Cwd: repo, Kind: "shell", CreatedAt: now})
	kickHub()
	writeJSON(w, map[string]any{"tab_id": tabID, "workspace_id": wsID})
}

// serveRepoPin toggles a repo's pinned flag (floats it to the top of the tree).
func serveRepoPin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo   string `json:"repo"`
		Pinned bool   `json:"pinned"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Repo == "" {
		http.Error(w, "repo required", http.StatusBadRequest)
		return
	}
	if err := pinRepo(sidebarHost, req.Repo, req.Pinned); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	kickHub()
	writeJSON(w, map[string]any{"ok": true})
}

// serveRepoRename overrides a repo's display name in the sidebar ("" clears it).
func serveRepoRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo string `json:"repo"`
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Repo == "" {
		http.Error(w, "repo required", http.StatusBadRequest)
		return
	}
	if err := setRepoDisplayName(sidebarHost, req.Repo, strings.TrimSpace(req.Name)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	kickHub()
	writeJSON(w, map[string]any{"ok": true})
}

// serveCreateWorktreeOnly makes a git worktree + workspace with an initial shell
// tab, but NO agent — for working a branch by hand. (The New Agent modal's
// "create worktree" path goes through /api/create-agent instead.)
func serveCreateWorktreeOnly(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo         string `json:"repo"`
		BaseBranch   string `json:"base_branch"`
		BranchPrefix string `json:"branch_prefix"`
		BranchName   string `json:"branch_name"`
		Title        string `json:"title"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	be := curBackend()
	repo := expandTildeOn(be, req.Repo)
	if repo == "" {
		http.Error(w, "repo required", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(req.Title)
	name := strings.TrimSpace(req.BranchName)
	if name == "" {
		name = slugify(title)
	}
	if name == "" {
		http.Error(w, "branch_name or title required", http.StatusBadRequest)
		return
	}
	prefix := strings.TrimRight(strings.TrimSpace(req.BranchPrefix), "/")
	branch := name
	if prefix != "" {
		branch = prefix + "/" + name
	}
	branch = uniqueBranch(be, repo, branch)
	base := strings.TrimSpace(req.BaseBranch)
	if base == "" {
		base = "HEAD"
	}
	workDir, err := createWorktree(be, repo, base, branch, slugify(title))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if title == "" {
		title = branch
	}
	wsID := "w" + newID()
	tabID := newID()
	if err := tmuxNewSession(tabSession(tabID), workDir, []string{"LASSO_TAB_ID=" + tabID}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	_ = insertWorkspace(Workspace{ID: wsID, Host: sidebarHost, Title: title, Repo: repo, WorkDir: workDir, Kind: "git", CreatedAt: now})
	_ = insertTab(Tab{ID: tabID, WorkspaceID: wsID, Title: "shell", Cwd: workDir, Kind: "shell", CreatedAt: now})
	_ = setLastBaseBranch(sidebarHost, repo, base)
	kickHub()
	writeJSON(w, map[string]any{"workspace_id": wsID, "work_dir": workDir, "branch": branch})
}

// kickHub nudges the SSE hub so a tree mutation is pushed immediately.
func kickHub() {
	if srvHub != nil {
		srvHub.kick()
	}
}
