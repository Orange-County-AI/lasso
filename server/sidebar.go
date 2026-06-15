package main

import (
	"encoding/json"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The sidebar's data + mutations. The React sidebar shows git repos from
// repos_root with their lasso worktrees nested as a tree, ordered by latest
// commit to the primary branch (pinned repos first), plus scratch workspaces,
// plus a flat agent list with live status. It's scoped to the ACTIVE host
// (curBackend): switching hosts re-scopes the whole tree to that host's
// workspaces/tabs/repos, all routed through curBackend().

// sbHost is the host the sidebar currently operates on — the active backend's
// name ("local" or an ssh alias). Guarded for tests that don't set a backend.
func sbHost() string {
	if b := curBackend(); b != nil {
		return b.Name()
	}
	return "local"
}

// startTabShellOn creates the tmux session backing a new shell tab on host (local
// or a remote host over SSH), rooted at workDir and tagged with LASSO_TAB_ID.
func startTabShellOn(host, tabID, workDir string) error {
	return tmuxNewSessionOn(host, tabSession(tabID), workDir, []string{"LASSO_TAB_ID=" + tabID})
}

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
	Branch  string    `json:"branch,omitempty"`
	Tabs    []treeTab `json:"tabs"`
	// Host this workspace lives on ("local" or an ssh alias) and its display
	// label, populated so the all-hosts sidebar can group by host. In the normal
	// single-host tree these just carry the active host.
	Host      string `json:"host,omitempty"`
	HostLabel string `json:"host_label,omitempty"`
	// AgentStatus is the workspace's aggregate live-agent status for the sidebar
	// dot (blocked > working > idle), or "" when no tab is running an agent.
	AgentStatus string `json:"agent_status,omitempty"`
	AgentKind   string `json:"agent_kind,omitempty"`
}

type treeRepo struct {
	Path          string          `json:"path"`
	Name          string          `json:"name"`
	PrimaryBranch string          `json:"primary_branch"`
	LastCommit    int64           `json:"last_commit"` // unix secs (ordering + display)
	Workspaces    []treeWorkspace `json:"workspaces"`  // linked worktrees only
	// Primary branch's status vs its configured upstream (origin tracking ref).
	// Upstream is "" for local-only repos; Ahead/Behind are commit counts.
	Upstream string `json:"upstream,omitempty"`
	Ahead    int    `json:"ahead,omitempty"`
	Behind   int    `json:"behind,omitempty"`
	// The repo row is itself the main checkout: clicking it opens a
	// terminal on the primary branch. MainTabID is its tab if one already exists,
	// else "" — the frontend then asks /api/repo/open to create one on click.
	// MainWorkspace carries the full main-checkout workspace (with its tabs) so
	// the tab strip can resolve it; it is NOT rendered as a child in the tree.
	MainWorkspaceID string         `json:"main_workspace_id,omitempty"`
	MainTabID       string         `json:"main_tab_id,omitempty"`
	MainWorkspace   *treeWorkspace `json:"main_workspace,omitempty"`
	AgentStatus     string         `json:"agent_status,omitempty"`
	AgentKind       string         `json:"agent_kind,omitempty"`
	// Host this repo lives on ("local" or an ssh alias) and its display label —
	// for grouping the all-hosts sidebar by host.
	Host      string `json:"host,omitempty"`
	HostLabel string `json:"host_label,omitempty"`
}

type treePayload struct {
	Repos   []treeRepo      `json:"repos"`
	Scratch []treeWorkspace `json:"scratch"`
	// Order is the authoritative top-level display order of the "spaces" list as
	// stable keys ("ws:<id>" for scratch, "repo:<path>" for repos). The frontend
	// renders one unified list from it; items absent from Order (e.g. just-created)
	// are appended at the bottom client-side. See serveTree + getSpacesOrder.
	Order []string `json:"order"`
	// Hosts are the host keys ("local" or an ssh alias) whose tree was SUCCESSFULLY
	// queried this round — even if a host has zero workspaces. Errors are the hosts
	// that couldn't be reached (host key → reason), mirroring grid's Errors. In
	// all-hosts mode buildHostTree dials each remote serially and a cold/unreachable
	// host yields nothing; reporting success here lets the sidebar tell "loaded &
	// empty" apart from "still connecting" and show a per-host loading state until
	// every usable host has reported, instead of remote workspaces trickling in over
	// successive polls. (Single-host mode just reports the active host.)
	Hosts  []string          `json:"hosts,omitempty"`
	Errors map[string]string `json:"errors,omitempty"`
}

// spacesKeyWorkspace / spacesKeyRepo build the stable keys used to order the
// unified sidebar "spaces" list (kept in sync with the frontend).
func spacesKeyWorkspace(id string) string { return "ws:" + id }
func spacesKeyRepo(path string) string    { return "repo:" + path }

func serveTree(w http.ResponseWriter, r *http.Request) {
	// All-hosts mode: aggregate every usable host's tree, grouped by host (local
	// first), each repo/workspace tagged with its host so the sidebar can render
	// host sections. The order is the per-host order of each host concatenated —
	// the frontend groups by host and orders within each group, so ordering stays
	// per-host (no cross-host reordering).
	if r.URL.Query().Get("all") == "1" {
		var allRepos []treeRepo
		var allScratch []treeWorkspace
		var order []string
		hosts := []string{}
		errs := map[string]string{}
		for _, t := range usableHostTargets() {
			repos, scratch, err := buildHostTree(t.host, t.label)
			if err != nil {
				// A cold/unreachable host contributes nothing this round; record why
				// so the sidebar shows it as loading rather than as empty.
				errs[hostOrLocal(t.host)] = err.Error()
				continue
			}
			hosts = append(hosts, hostOrLocal(t.host))
			order = append(order, spacesOrder(t.host, scratch, repos)...)
			allRepos = append(allRepos, repos...)
			allScratch = append(allScratch, scratch...)
		}
		if len(errs) == 0 {
			errs = nil
		}
		writeJSON(w, treePayload{Repos: allRepos, Scratch: allScratch, Order: order, Hosts: hosts, Errors: errs})
		return
	}
	host := sbHost()
	repos, scratch, _ := buildHostTree(host, hostLabelFor(host))
	writeJSON(w, treePayload{Repos: repos, Scratch: scratch, Order: spacesOrder(host, scratch, repos), Hosts: []string{hostOrLocal(host)}})
}

// hostLabelFor is a host's sidebar display label: the machine hostname for the
// local host, else the ssh alias.
func hostLabelFor(host string) string {
	if isLocalHost(host) {
		return localHostname()
	}
	return host
}

// buildHostTree builds one host's repos (sorted) and scratch workspaces for the
// sidebar, each tagged with the host + label. Returns the backend error if the
// host can't be reached (so the all-hosts caller can report it as still-loading
// rather than silently empty).
func buildHostTree(host, label string) ([]treeRepo, []treeWorkspace, error) {
	be, err := hostBackend(host)
	if err != nil {
		return nil, nil, err
	}
	hk := hostOrLocal(host)
	_, repos, _ := reposList(be, host)
	repoState, _ := listRepoState(host)
	wss, _ := listWorkspaces(host)
	statuses := agentStatuses.snapshot()
	kinds := agentKindsForHost(host) // tab id → agent kind (live /proc local, DB remote)

	byRepo := map[string][]treeWorkspace{}
	mainByRepo := map[string]treeWorkspace{} // the repo-root checkout, per repo
	scratch := []treeWorkspace{}
	for _, ws := range wss {
		tw := buildTreeWorkspace(be, ws, statuses, kinds, host)
		tw.Host = hk
		tw.HostLabel = label
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
	// main checkout) — workspaces are listed grouped by repo, not every repo under
	// repos_root. (New repos are reached via New Agent / ⌘K; a
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
		if rc := repoState[path]; rc != nil && rc.DisplayName != "" {
			name = rc.DisplayName
		}
		primary, ct := repoPrimaryBranchAndTime(be, path)
		upstream, ahead, behind := repoUpstreamStatus(be, path, primary)
		repoWss := byRepo[path]
		if repoWss == nil {
			repoWss = []treeWorkspace{}
		}
		tr := treeRepo{
			Path: path, Name: name, PrimaryBranch: primary,
			LastCommit: ct, Workspaces: repoWss,
			Upstream: upstream, Ahead: ahead, Behind: behind,
			Host: hk, HostLabel: label,
		}
		if main, ok := mainByRepo[path]; ok {
			m := main
			tr.MainWorkspace = &m
			tr.MainWorkspaceID = main.ID
			tr.AgentStatus = main.AgentStatus
			tr.AgentKind = main.AgentKind
			if len(main.Tabs) > 0 {
				tr.MainTabID = main.Tabs[0].ID
			}
		}
		out = append(out, tr)
	}
	// Seed (default) order for items the user hasn't manually placed: repos by
	// most-recently-committed, then name. Scratch keeps DB (creation) order. This
	// is only a tie-breaker for never-placed rows — once the user drags, the stored
	// spaces_order governs everything below.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].LastCommit != out[j].LastCommit {
			return out[i].LastCommit > out[j].LastCommit
		}
		return out[i].Name < out[j].Name
	})
	return out, scratch, nil
}

// spacesOrder resolves the unified top-level order of the "spaces" list: the
// user's stored order first (stale keys dropped), then any current rows not yet
// placed appended at the bottom in seed order (scratch creation order, then
// repos by recency). This is what lands a freshly-created workspace at the bottom.
func spacesOrder(host string, scratch []treeWorkspace, repos []treeRepo) []string {
	defaultKeys := make([]string, 0, len(scratch)+len(repos))
	for _, ws := range scratch {
		defaultKeys = append(defaultKeys, spacesKeyWorkspace(ws.ID))
	}
	for _, r := range repos {
		defaultKeys = append(defaultKeys, spacesKeyRepo(r.Path))
	}
	exists := make(map[string]bool, len(defaultKeys))
	for _, k := range defaultKeys {
		exists[k] = true
	}
	stored, _ := getSpacesOrder(host)
	order := make([]string, 0, len(defaultKeys))
	seen := make(map[string]bool, len(defaultKeys))
	for _, k := range stored {
		if exists[k] && !seen[k] {
			order = append(order, k)
			seen[k] = true
		}
	}
	for _, k := range defaultKeys {
		if !seen[k] {
			order = append(order, k)
			seen[k] = true
		}
	}
	return order
}

func buildTreeWorkspace(be Backend, ws Workspace, statuses, kinds map[string]string, host string) treeWorkspace {
	tw := treeWorkspace{
		ID: ws.ID, Title: ws.Title, Repo: ws.Repo, WorkDir: ws.WorkDir,
		Kind: ws.Kind, Tabs: []treeTab{},
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
			tt.Status = statuses[statusKey(host, t.ID)]
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

// repoUpstreamStatus resolves the primary branch's tracking ref and its
// ahead/behind commit counts. Returns ("", 0, 0) when no upstream is configured
// (local-only repo) or git errors.
func repoUpstreamStatus(be Backend, repo, primary string) (string, int, int) {
	if primary == "" {
		return "", 0, 0
	}
	up, err := be.GitOut(repo, "rev-parse", "--abbrev-ref", primary+"@{upstream}")
	if err != nil {
		return "", 0, 0
	}
	upstream := strings.TrimSpace(up)
	out, err := be.GitOut(repo, "rev-list", "--left-right", "--count",
		primary+"..."+primary+"@{upstream}")
	if err != nil {
		return upstream, 0, 0
	}
	// git prints "<ahead>\t<behind>" (left = primary-only, right = upstream-only)
	fields := strings.Fields(strings.TrimSpace(out))
	ahead, behind := 0, 0
	if len(fields) == 2 {
		ahead, _ = strconv.Atoi(fields[0])
		behind, _ = strconv.Atoi(fields[1])
	}
	return upstream, ahead, behind
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
	// Host this agent runs on ("local" or an ssh alias) + its display label, so
	// the all-hosts agents pane can group by host.
	Host      string `json:"host,omitempty"`
	HostLabel string `json:"host_label,omitempty"`
}

func serveAgentsList(w http.ResponseWriter, r *http.Request) {
	// All-hosts mode: concatenate every usable host's agents (local first), each
	// tagged with its host so the agents pane can group by host.
	if r.URL.Query().Get("all") == "1" {
		out := []agentRow{}
		for _, t := range usableHostTargets() {
			out = append(out, buildHostAgents(t.host, t.label)...)
		}
		writeJSON(w, map[string]any{"agents": out})
		return
	}
	host := sbHost()
	writeJSON(w, map[string]any{"agents": buildHostAgents(host, hostLabelFor(host))})
}

// buildHostAgents returns one host's flat agent list (sorted by workspace then
// tab title), each row tagged with the host + label.
func buildHostAgents(host, label string) []agentRow {
	hk := hostOrLocal(host)
	statuses := agentStatuses.snapshot()
	kinds := agentKindsForHost(host) // tabs running an agent (live local, DB remote)
	agentByID := map[string]AgentRecord{}
	if agents, err := listAgents(host); err == nil {
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
		ws, _ := getWorkspace(t.WorkspaceID)
		// A lasso-created agent and its workspace are 1:1 with one name, so the
		// workspace title is authoritative — the agent row and the spaces tree
		// can't drift apart. Ad-hoc agents (claude run by hand in a shell tab, no
		// AgentID) keep their tab title: their workspace may hold other tabs.
		title := t.Title
		if t.AgentID != "" && ws.Title != "" {
			title = ws.Title
		}
		if title == "" {
			title = rec.Title
		}
		status := statuses[statusKey(host, tabID)]
		if status == "" {
			status = string(StatusIdle)
		}
		out = append(out, agentRow{
			TabID: tabID, AgentID: t.AgentID, Title: title, Agent: kind, Status: status,
			WorkspaceID: t.WorkspaceID, WorkspaceTitle: ws.Title, Repo: ws.Repo,
			Cwd: t.Cwd, Prompt: rec.Description, Host: hk, HostLabel: label,
		})
	}
	// Stable order: workspace title, then tab title.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].WorkspaceTitle != out[j].WorkspaceTitle {
			return out[i].WorkspaceTitle < out[j].WorkspaceTitle
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// ---------------------------------------------------------------------------
// mutations
// ---------------------------------------------------------------------------

// hostOrActive resolves a handler's optional `host` body field, defaulting to
// the active host. In all-hosts mode the frontend sends the target row's host so
// a mutation lands on the right machine; single-host callers omit it.
func hostOrActive(host string) string {
	if host == "" {
		return sbHost()
	}
	return host
}

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

// An agent and its workspace are created 1:1 with the same name, and the
// sidebar treats those names as synonymous. Renames therefore write through in
// both directions (renameTabSynced / renameWorkspaceSynced) so the agent list,
// spaces tree, and tab strip always agree. AgentRecord.Title is left alone —
// it's the historical first line of the prompt, not a display name.

// renameTabSynced renames a tab; when it's an agent's tab, the new title also
// becomes its workspace's title.
func renameTabSynced(tabID, title string) error {
	if err := renameTab(tabID, title); err != nil {
		return err
	}
	if t, err := getTab(tabID); err == nil && t.AgentID != "" && t.WorkspaceID != "" {
		return renameWorkspace(t.WorkspaceID, title)
	}
	return nil
}

// renameWorkspaceSynced renames a workspace; any agent tabs in it follow.
func renameWorkspaceSynced(wsID, title string) error {
	if err := renameWorkspace(wsID, title); err != nil {
		return err
	}
	tabs, err := listTabs(wsID)
	if err != nil {
		return err
	}
	for _, t := range tabs {
		if t.AgentID != "" {
			if err := renameTab(t.ID, title); err != nil {
				return err
			}
		}
	}
	return nil
}

// serveTabRename renames a tab (covers renaming an agent: its tab title drives
// both the sidebar render and ⌘K search, and syncs to its workspace).
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
	if err := renameTabSynced(req.TabID, strings.TrimSpace(req.Title)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	kickHub()
	writeJSON(w, map[string]any{"ok": true})
}

// serveWorkspaceRenameDB renames a workspace in the DB (agent tabs follow).
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
	if err := renameWorkspaceSynced(req.WorkspaceID, strings.TrimSpace(req.Title)); err != nil {
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
	primePending.Delete(tabSession(tabID)) // drop any unconsumed prime mark
	// No per-tab ttyd to detach now (one shared viewport); killing the session is
	// enough. If this tab was the viewport's current target, the frontend repoints
	// it at the next selected tab (and the watcher follows).
	host := tabHost(tabID)
	// Re-assert the session→host mapping from the DB before the kill: after a
	// lasso restart a never-viewed remote tab isn't in sessionHosts yet, and an
	// unrouted kill-session would hit the local server and leak the remote one.
	setSessionHost(tabSession(tabID), host)
	_ = tmuxKillSession(tabSession(tabID))
	agentStatuses.forget(host, tabID)
	_ = closeTab(tabID)
}

// serveAgentClose closes an agent from the sidebar: close its tab, then — if
// that leaves its workspace empty — close the workspace too, so the agent's
// worktree disappears from the spaces pane rather than lingering as an empty
// shell. Mirrors the default path of the close_agent MCP tool. (A soft close:
// the git worktree on disk is left intact; only the sidebar row goes away.)
func serveAgentClose(w http.ResponseWriter, r *http.Request) {
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
	// Capture the workspace before the tab row is gone.
	wsID := ""
	if t, err := getTab(req.TabID); err == nil {
		wsID = t.WorkspaceID
	}
	closeOneTab(req.TabID)
	if wsID != "" && len(mustTabs(wsID)) == 0 {
		_ = closeWorkspace(wsID)
	}
	kickHub()
	writeJSON(w, map[string]any{"ok": true})
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

// serveRepoClose closes an entire repo from the sidebar: every workspace tied to
// it — the main checkout (work_dir == repo root) and all its linked worktrees —
// along with their tabs. With no live workspace left, the repo row drops out of
// the tree (serveTree only shows repos that have one). A soft close like the
// others: the git checkout/worktrees on disk are untouched; only the sidebar
// rows go away, and reopening the repo via New Agent / ⌘K brings it back.
func serveRepoClose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo string `json:"repo"`
		Host string `json:"host"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	host := hostOrActive(req.Host)
	be, err := hostBackend(host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	repo := expandTildeOn(be, req.Repo)
	if repo == "" {
		http.Error(w, "repo required", http.StatusBadRequest)
		return
	}
	wss, _ := listWorkspaces(host)
	for _, ws := range wss {
		if ws.Kind != "git" || ws.Repo != repo {
			continue
		}
		tabs, _ := listTabs(ws.ID)
		for _, t := range tabs {
			closeOneTab(t.ID)
		}
		_ = closeWorkspace(ws.ID)
	}
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
	ord := nextTabOrdinal(ws.ID)
	title := strings.TrimSpace(req.Title)
	if title == "" {
		// Default to numeric naming (ordinal + 1, monotonic).
		title = strconv.Itoa(ord + 1)
	}
	tabID := newID()
	if err := startTabShellOn(ws.Host, tabID, ws.WorkDir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tab := Tab{
		ID: tabID, WorkspaceID: ws.ID, Title: title, Cwd: ws.WorkDir,
		Kind: "shell", Ordinal: ord, CreatedAt: time.Now(),
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
// checkout (work_dir == repo root). A repo row isn't just a grouping of
// worktrees: it's itself a workspace. Returns the tab to select, creating the
// main-checkout workspace + a shell tab on first open.
func serveOpenRepo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo string `json:"repo"`
		Host string `json:"host"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	host := hostOrActive(req.Host)
	be, err := hostBackend(host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	repo := expandTildeOn(be, req.Repo)
	if repo == "" {
		http.Error(w, "repo required", http.StatusBadRequest)
		return
	}
	// Reuse the existing main-checkout workspace if there is one.
	if wss, err := listWorkspaces(host); err == nil {
		for _, ws := range wss {
			if ws.Kind == "git" && ws.WorkDir == repo {
				if tabs, _ := listTabs(ws.ID); len(tabs) > 0 {
					writeJSON(w, map[string]any{"tab_id": tabs[0].ID, "workspace_id": ws.ID})
					return
				}
				// Workspace exists but has no live tab — fall through to add one.
				tabID := newID()
				if err := startTabShellOn(ws.Host, tabID, repo); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				_ = insertTab(Tab{ID: tabID, WorkspaceID: ws.ID, Title: nextTabName(ws.ID), Cwd: repo, Kind: "shell", CreatedAt: time.Now()})
				kickHub()
				writeJSON(w, map[string]any{"tab_id": tabID, "workspace_id": ws.ID})
				return
			}
		}
	}
	// Create the main-checkout workspace + an initial shell tab at the repo root.
	wsID := "w" + newID()
	tabID := newID()
	if err := startTabShellOn(host, tabID, repo); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	title := filepath.Base(repo)
	if rc, err := getRepoState(host, repo); err == nil && rc.DisplayName != "" {
		title = rc.DisplayName
	}
	_ = insertWorkspace(Workspace{ID: wsID, Host: host, Title: title, Repo: repo, WorkDir: repo, Kind: "git", CreatedAt: now})
	_ = insertTab(Tab{ID: tabID, WorkspaceID: wsID, Title: nextTabName(wsID), Cwd: repo, Kind: "shell", CreatedAt: now})
	kickHub()
	writeJSON(w, map[string]any{"tab_id": tabID, "workspace_id": wsID})
}

// serveSpacesReorder persists the user's drag-and-drop ordering of the unified
// "spaces" list. The client sends the full current key list ("ws:<id>" /
// "repo:<path>") in its new order; we store it verbatim (serveTree drops any
// stale keys and appends new rows at the bottom on read).
func serveSpacesReorder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Order []string `json:"order"`
		Host  string   `json:"host"` // the host group being reordered (all-hosts mode)
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := setSpacesOrder(hostOrActive(req.Host), req.Order); err != nil {
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
		Host string `json:"host"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Repo == "" {
		http.Error(w, "repo required", http.StatusBadRequest)
		return
	}
	if err := setRepoDisplayName(hostOrActive(req.Host), req.Repo, strings.TrimSpace(req.Name)); err != nil {
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
		Host         string `json:"host"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	host := hostOrActive(req.Host)
	be, err := hostBackend(host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
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
	if err := startTabShellOn(host, tabID, workDir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	_ = insertWorkspace(Workspace{ID: wsID, Host: host, Title: title, Repo: repo, WorkDir: workDir, Kind: "git", CreatedAt: now})
	_ = insertTab(Tab{ID: tabID, WorkspaceID: wsID, Title: nextTabName(wsID), Cwd: workDir, Kind: "shell", CreatedAt: now})
	_ = setLastBaseBranch(host, repo, base)
	kickHub()
	writeJSON(w, map[string]any{"workspace_id": wsID, "work_dir": workDir, "branch": branch})
}

// serveCreateWorkspace makes a bare SCRATCH workspace — a shell in a fresh
// ~/.lasso/scratch dir, with NO agent. This is the "New workspace" affordance in
// the spaces sidebar (distinct from /api/create-agent, which launches an agent).
// The configured scratch setup script (if any) runs in the shell so the env
// matches a scratch agent's, but nothing else is started.
func serveCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	be := curBackend()
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "scratch"
	}
	slug := slugify(title)
	if slug == "" {
		slug = "scratch"
	}
	workDir := uniqueChildDir(lassoScratchDirFor(be), slug+"-"+randSuffix())
	if err := be.MkdirAll(workDir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	wsID := "w" + newID()
	tabID := newID()
	session := tabSession(tabID)
	// Create the tab's shell session on the active host (local, or remote over
	// SSH). Like serveNewTab.
	if err := startTabShellOn(sbHost(), tabID, workDir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	if err := insertWorkspace(Workspace{ID: wsID, Host: sbHost(), Title: title, WorkDir: workDir, Kind: "scratch", CreatedAt: now}); err != nil {
		_ = tmuxKillSession(session)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := insertTab(Tab{ID: tabID, WorkspaceID: wsID, Title: nextTabName(wsID), Cwd: workDir, Kind: "shell", CreatedAt: now}); err != nil {
		_ = tmuxKillSession(session)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Run the configured scratch setup in the shell (no agent). Best-effort and
	// backgrounded so create returns immediately. The session starts in workDir, so
	// just wait for the rc to settle (so leading chars aren't eaten) before sending.
	if defaults, derr := hostDefaults(be.Name()); derr == nil {
		if s := strings.TrimSpace(defaults.ScratchSetup); s != "" {
			go func(sess, setup string) {
				if !tmuxHasSession(sess) {
					return
				}
				tmuxWaitReady(sess)
				_ = tmuxSendLine(sess, setup)
			}(session, s)
		}
	}
	kickHub()
	writeJSON(w, map[string]any{"workspace_id": wsID, "tab_id": tabID, "work_dir": workDir})
}

// kickHub nudges the SSE hub so a tree mutation is pushed immediately.
func kickHub() {
	if srvHub != nil {
		srvHub.kick()
	}
}

// ---------------------------------------------------------------------------
// GET/POST /api/ui-state — persisted browser UI prefs (sidebar layout)
// ---------------------------------------------------------------------------

// clampPanelPct sanitizes a stored panel width (% of the panel group). Zero
// means "never saved" and passes through; anything else is clamped to a usable
// open range so a garbage write can't wedge a panel invisible or full-bleed.
func clampPanelPct(p float64) float64 {
	if p == 0 {
		return 0
	}
	return math.Min(95, math.Max(5, p))
}

func serveUIState(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		us, err := getUIState()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, us)
	case http.MethodPost:
		var us uiState
		if err := json.NewDecoder(r.Body).Decode(&us); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if us.GridHiddenHosts == nil {
			us.GridHiddenHosts = []string{}
		}
		if us.GridSelected == nil {
			us.GridSelected = []string{}
		}
		us.LeftWidth = clampPanelPct(us.LeftWidth)
		us.RightWidth = clampPanelPct(us.RightWidth)
		if err := saveUIState(us); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sidebarAllHosts.Store(us.SidebarAllHosts) // keep the poller's view in sync
		// Push the change to every other connected client (last write wins).
		if srvHub != nil {
			srvHub.bumpUI()
		}
		writeJSON(w, us)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}
