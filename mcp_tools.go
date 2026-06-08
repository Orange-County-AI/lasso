package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP tool surface. Each tool is a thin wrapper over lasso's tmux-backed agent
// machinery. Three groups:
//   - discovery:   list_hosts, list_repos, list_branches
//   - spawning:    create_agent (loop it for the bulk "one per repo" case)
//   - interaction: list_agents, get_agent, send_agent, read_agent, wait_agent,
//                  close_agent  (the agent's tmux session is the stateful conversation)
//   - introspection: whoami (an agent maps its own $LASSO_TAB_ID back to its
//                  lasso record, typically to then close_agent itself)

// registerMCPTools wires every tool onto the server. The In/Out struct types
// drive the JSON Schemas the SDK advertises (field docs come from `jsonschema`
// tags; fields are optional iff their json tag carries `omitempty`).
func registerMCPTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_hosts",
		Description: "List the hosts lasso can drive (the local box plus reachable, protocol-compatible SSH hosts). Use the returned alias as the `host` argument of the other tools; omit `host` to target the local box.",
	}, listHostsTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_repos",
		Description: "List the git repositories under the host's configured repo roots. Use a returned `path` as `repo` when creating a git agent. You may also pass any absolute repo path to create_agent directly — this only enumerates the configured roots.",
	}, listReposTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_branches",
		Description: "List a repository's local and remote branches plus its default branch, so you can choose a base_branch for a new git agent.",
	}, listBranchesTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "create_agent",
		Description: "Spawn a coding agent (claude or codex) in its own lasso workspace (a tmux session). type=git creates a fresh git worktree off base_branch (default the repo's HEAD) under a new branch; type=scratch creates an empty workspace. The optional prompt becomes the agent's initial task. Returns immediately with the agent's id and workspace; the agent boots asynchronously. To bring many repos up to date, call this once per repo.",
	}, createAgentTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_agents",
		Description: "List the agents lasso has created on a host, each with its live status (working/idle/blocked/unknown) when the agent is running.",
	}, listAgentsTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "whoami",
		Description: "Identify the calling agent's OWN lasso agent record, so it can then act on itself — most commonly to call close_agent with the returned id once its work is done. Pass the value of your $LASSO_TAB_ID environment variable as tab_id; lasso maps that tab to the agent it created there. The lasso MCP server runs in lasso's own process, NOT your shell, so it cannot read your environment — you MUST supply $LASSO_TAB_ID yourself. On success returns found:true and the same fields as a list_agents entry under `agent`. If it can't resolve it returns found:false with a human-readable `detail` instead of erroring.",
	}, whoamiTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_agent",
		Description: "Get one agent's details, its live status, and a tail of its recent terminal output.",
	}, getAgentTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "send_agent",
		Description: "Send a message to a running agent — the text is typed into its pane and submitted (as if you typed it and pressed Enter). Use this to give follow-up instructions or answer a prompt the agent is blocked on. Works whether the agent is idle or busy: a message sent mid-turn is queued and the agent picks it up after its current turn (it does not interrupt). The call confirms the message actually submitted before returning, so you don't need to re-send; follow with wait_agent + read_agent to get the reply.",
	}, sendAgentTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "read_agent",
		Description: "Read an agent's terminal output. source 'recent' returns scrollback (default), 'visible' just the current screen. Pair with wait_agent to do request/response round-trips.",
	}, readAgentTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "wait_agent",
		Description: "Block until an agent reaches a status (default 'idle', i.e. done working and waiting) or the timeout elapses. Use after send_agent to wait for the agent to finish before reading its reply.",
	}, waitAgentTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "close_agent",
		Description: "Stop an agent: first kill the agent process (claude/codex) in its terminal, then — unless close_pane is false — close the associated tab (its tmux session). For a git agent, set remove_worktree=true to also delete its git worktree (this discards any uncommitted work, so it defaults to false, and implies closing the tab).",
	}, closeAgentTool)
}

// ---------------------------------------------------------------------------
// shared shapes + helpers
// ---------------------------------------------------------------------------

// agentInfo is the MCP-facing view of an agent: the persisted record fields a
// caller needs to drive it, plus live status when known.
type agentInfo struct {
	ID          string `json:"id"`
	Host        string `json:"host"`
	Title       string `json:"title"`
	Type        string `json:"type"`
	Agent       string `json:"agent"`
	Repo        string `json:"repo,omitempty"`
	Branch      string `json:"branch,omitempty"`
	BaseBranch  string `json:"base_branch,omitempty"`
	WorkDir     string `json:"work_dir"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	RootPane    string `json:"root_pane,omitempty"`
	Status      string `json:"status,omitempty"`
	CreatedAt   string `json:"created_at"`
}

func agentInfoFrom(host string, rec AgentRecord, status string) agentInfo {
	return agentInfo{
		ID: rec.ID, Host: host, Title: rec.Title, Type: rec.Type, Agent: rec.Agent,
		Repo: rec.Repo, Branch: rec.Branch, BaseBranch: rec.BaseBranch,
		WorkDir: rec.WorkDir, WorkspaceID: rec.WorkspaceID, RootPane: agentSession(rec),
		Status: status, CreatedAt: rec.CreatedAt.Format(time.RFC3339),
	}
}

// agentSession is the tmux session backing an agent (its tab) — the runtime
// handle for read/send/wait/close.
func agentSession(rec AgentRecord) string {
	id := rec.TabID
	if id == "" {
		id = rec.ID // pre-migration records used the agent id as the tab id
	}
	return tabSession(id)
}

// recSession is agentSession plus a re-assertion of the session→host mapping from
// the record, so a remote agent's tmux commands route over SSH even after a lasso
// restart cleared the in-memory map. Use this for any RUNTIME tmux op on an agent.
func recSession(rec AgentRecord) string {
	s := agentSession(rec)
	setSessionHost(s, rec.Host)
	return s
}

// agentStatusNow detects an agent's status on demand by scraping its tmux pane.
// For a LOCAL agent it always does a fresh read (the poller pauses when no browser
// is connected). For a REMOTE agent each read is an ssh round trip, so it serves a
// recently-cached value when the poller has one and only sshes on a cache miss —
// bounding the ssh cost of wait_agent. A gone session / exited agent reads idle.
func agentStatusNow(rec AgentRecord) string {
	session := recSession(rec)
	if !isLocalHost(rec.Host) {
		if st, ok := agentStatuses.statusFresh(rec.Host, rec.TabID, 2*time.Second); ok {
			return string(st)
		}
		if !tmuxHasSession(session) {
			return string(StatusIdle)
		}
		screen, err := tmuxCapture(session)
		if err != nil {
			return string(StatusUnknown)
		}
		var lw time.Time
		st := detectAgentStatus(rec.Agent, screen, agentStatuses.status(rec.Host, rec.TabID), &lw, time.Now())
		agentStatuses.set(rec.Host, rec.TabID, st)
		return string(st)
	}
	if !tmuxHasSession(session) || sessionAgentKind(session) == "" {
		return string(StatusIdle)
	}
	screen, err := tmuxCapture(session)
	if err != nil {
		return string(StatusUnknown)
	}
	var lw time.Time
	return string(detectAgentStatus(rec.Agent, screen, agentStatuses.status(rec.Host, rec.TabID), &lw, time.Now()))
}

// agentReadText reads an agent's terminal: "visible" = the current screen,
// anything else = recent scrollback (lines, default 200).
func agentReadText(rec AgentRecord, source string, lines int) (string, error) {
	session := recSession(rec)
	if !tmuxHasSession(session) {
		return "", fmt.Errorf("agent %q has no live terminal", rec.ID)
	}
	if source == "visible" {
		return tmuxCapture(session)
	}
	if lines <= 0 {
		lines = 200
	}
	return tmuxCaptureScroll(session, lines)
}

// ---------------------------------------------------------------------------
// list_hosts
// ---------------------------------------------------------------------------

type listHostsIn struct{}

type hostEntry struct {
	Host       string `json:"host"`       // value to pass as `host` ("local" or an ssh alias)
	Label      string `json:"label"`      // display name (hostname / alias)
	Reachable  bool   `json:"reachable"`  // ssh connected (always true for local)
	Running    bool   `json:"running"`    // usable as a target: reachable + tmux present
	Compatible bool   `json:"compatible"` // same as running (no protocol to match in the tmux model)
	Version    string `json:"version,omitempty"`
	Err        string `json:"err,omitempty"`
}

type listHostsOut struct {
	Active string      `json:"active"` // the host the lasso UI currently drives
	Hosts  []hostEntry `json:"hosts"`
}

func listHostsTool(ctx context.Context, _ *mcp.CallToolRequest, _ listHostsIn) (*mcp.CallToolResult, listHostsOut, error) {
	out := listHostsOut{
		Active: curBackend().Name(),
		Hosts: []hostEntry{{
			Host: "local", Label: localHostname(), Reachable: true,
			Running: true, Compatible: true, Version: lassoVersion(),
		}},
	}
	for _, h := range discoverHosts(ctx, false) {
		out.Hosts = append(out.Hosts, hostEntry{
			Host: h.Alias, Label: h.Alias,
			Reachable: h.Reachable, Running: h.usable(), Compatible: h.usable(),
			Version: h.TmuxVersion, Err: h.Err,
		})
	}
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// list_repos
// ---------------------------------------------------------------------------

type listReposIn struct {
	Host string `json:"host,omitempty" jsonschema:"Host to list repos on; omit for the local box."`
}

type repoBrief struct {
	Path           string `json:"path"`
	Name           string `json:"name"`
	LastBaseBranch string `json:"last_base_branch,omitempty"`
}

type listReposOut struct {
	Root  string      `json:"root"` // the configured repo root(s) scanned
	Repos []repoBrief `json:"repos"`
}

func listReposTool(_ context.Context, _ *mcp.CallToolRequest, in listReposIn) (*mcp.CallToolResult, listReposOut, error) {
	host := in.Host
	if host == "" {
		host = "local"
	}
	root, repos, err := hostReposList(host)
	if err != nil {
		return nil, listReposOut{}, err
	}
	out := listReposOut{Root: root}
	for _, r := range repos {
		out.Repos = append(out.Repos, repoBrief{Path: r.Path, Name: r.Name, LastBaseBranch: r.LastBaseBranch})
	}
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// list_branches
// ---------------------------------------------------------------------------

type listBranchesIn struct {
	Host string `json:"host,omitempty" jsonschema:"Host the repo lives on; omit for the local box."`
	Repo string `json:"repo" jsonschema:"Absolute path to the git repository."`
}

type listBranchesOut struct {
	Branches       []string `json:"branches"`        // local branches
	RemoteBranches []string `json:"remote_branches"` // remote-tracking branches
	Default        string   `json:"default"`         // the repo's default branch
}

func listBranchesTool(_ context.Context, _ *mcp.CallToolRequest, in listBranchesIn) (*mcp.CallToolResult, listBranchesOut, error) {
	if strings.TrimSpace(in.Repo) == "" {
		return nil, listBranchesOut{}, fmt.Errorf("repo is required")
	}
	b, err := resolveBackend(in.Host)
	if err != nil {
		return nil, listBranchesOut{}, err
	}
	local, remote, def := branchList(b, expandTildeOn(b, in.Repo))
	return nil, listBranchesOut{Branches: local, RemoteBranches: remote, Default: def}, nil
}

// ---------------------------------------------------------------------------
// create_agent
// ---------------------------------------------------------------------------

type createAgentIn struct {
	Host         string `json:"host,omitempty" jsonschema:"Host to create the agent on; omit for the local box."`
	Type         string `json:"type" jsonschema:"\"git\" (a new worktree off base_branch) or \"scratch\" (an empty workspace)."`
	Title        string `json:"title,omitempty" jsonschema:"Optional short title for the agent/worktree; defaults to the prompt's first line."`
	Repo         string `json:"repo,omitempty" jsonschema:"Absolute path to the git repository. Required when type is \"git\"."`
	BaseBranch   string `json:"base_branch,omitempty" jsonschema:"Branch (or ref) to branch the new worktree off. Defaults to the repo's HEAD. Use list_branches to choose one."`
	BranchName   string `json:"branch_name,omitempty" jsonschema:"Name for the new branch. Defaults to a slug of the title."`
	BranchPrefix string `json:"branch_prefix,omitempty" jsonschema:"Optional prefix for the new branch, e.g. \"worktree\" -> worktree/<name>."`
	Agent        string `json:"agent,omitempty" jsonschema:"Which agent to launch: \"claude\" (default) or \"codex\"."`
	Prompt       string `json:"prompt,omitempty" jsonschema:"Initial task/instructions for the agent."`
	Notes        string `json:"notes,omitempty" jsonschema:"Extra notes; written to NOTES.md in the work dir and referenced in the prompt."`
	Focus        bool   `json:"focus,omitempty" jsonschema:"Switch the terminal view to the new agent as it boots. Defaults to false so spawning an agent doesn't yank you away from your current terminal."`
}

func createAgentTool(_ context.Context, _ *mcp.CallToolRequest, in createAgentIn) (*mcp.CallToolResult, agentInfo, error) {
	b, err := resolveBackend(in.Host)
	if err != nil {
		return nil, agentInfo{}, err
	}
	rec, err := createAgent(b, createAgentReq{
		Type:         in.Type,
		Title:        in.Title,
		Repo:         in.Repo,
		BaseBranch:   in.BaseBranch,
		BranchPrefix: in.BranchPrefix,
		BranchName:   in.BranchName,
		Agent:        in.Agent,
		Prompt:       in.Prompt, // the prompt rides into agentCommand via agentPrompt; its first line is the title
		Notes:        in.Notes,
		// Default to NOT focusing: an MCP-spawned agent shouldn't switch a watching
		// user away from their current pane. Opt in with focus:true.
		NoFocus: !in.Focus,
		// PlanMode intentionally omitted: agents started via MCP never run in plan mode.
	})
	if err != nil {
		return nil, agentInfo{}, err
	}
	return nil, agentInfoFrom(b.Name(), rec, ""), nil
}

// ---------------------------------------------------------------------------
// list_agents
// ---------------------------------------------------------------------------

type listAgentsIn struct {
	Host string `json:"host,omitempty" jsonschema:"Host to list agents on; omit for the local box."`
}

type listAgentsOut struct {
	Host   string      `json:"host"`
	Agents []agentInfo `json:"agents"`
}

func listAgentsTool(_ context.Context, _ *mcp.CallToolRequest, in listAgentsIn) (*mcp.CallToolResult, listAgentsOut, error) {
	host := in.Host
	if host == "" {
		host = "local"
	}
	recs, err := listAgents(host)
	if err != nil {
		return nil, listAgentsOut{}, err
	}
	out := listAgentsOut{Host: host}
	for _, rec := range recs {
		// The agents table is an append-only log; only surface agents whose tab is
		// still open (close_agent soft-closes the tab), so list_agents reflects LIVE
		// agents — matching the sidebar tree.
		if !agentTabLive(rec) {
			continue
		}
		out.Agents = append(out.Agents, agentInfoFrom(host, rec, agentStatusNow(rec)))
	}
	return nil, out, nil
}

// agentTabLive reports whether an agent record's backing tab is still open. Falls
// back to a live tmux session when the record predates the tabs table.
func agentTabLive(rec AgentRecord) bool {
	if rec.TabID != "" {
		if t, err := getTab(rec.TabID); err == nil {
			return t.ClosedAt.IsZero()
		}
	}
	return tmuxHasSession(recSession(rec))
}

// ---------------------------------------------------------------------------
// whoami
// ---------------------------------------------------------------------------

type whoamiIn struct {
	Host  string `json:"host,omitempty" jsonschema:"Host the calling agent runs on; omit for the local box."`
	TabID string `json:"tab_id,omitempty" jsonschema:"Your own lasso tab id — the value of the $LASSO_TAB_ID environment variable in your shell. The server cannot read your environment, so you must pass it. If unset you are not running inside a lasso-managed terminal."`
}

type whoamiOut struct {
	Found  bool       `json:"found"`            // true if the tab resolved to a lasso agent
	Agent  *agentInfo `json:"agent,omitempty"`  // the resolved agent (null when found is false)
	Detail string     `json:"detail,omitempty"` // why resolution failed, when found is false
}

func whoamiTool(_ context.Context, _ *mcp.CallToolRequest, in whoamiIn) (*mcp.CallToolResult, whoamiOut, error) {
	host := in.Host
	if host == "" {
		host = "local"
	}
	recs, err := listAgents(host)
	if err != nil {
		return nil, whoamiOut{}, err
	}
	return nil, resolveWhoami(host, recs, in.TabID), nil
}

// resolveWhoami maps a $LASSO_TAB_ID value to the lasso agent that owns that tab.
// Each agent's tmux session is created with LASSO_TAB_ID set, so an agent reads
// the var and passes it here. Never errors — an unresolvable id yields
// found:false with an explanation.
func resolveWhoami(host string, recs []AgentRecord, tabID string) whoamiOut {
	tabID = strings.TrimSpace(tabID)
	if tabID == "" {
		return whoamiOut{Detail: "no tab_id given: pass the value of your $LASSO_TAB_ID environment variable. If it's empty or unset, you are not running inside a lasso-managed terminal."}
	}
	for _, rec := range recs {
		if (rec.TabID != "" && rec.TabID == tabID) || rec.ID == tabID {
			ai := agentInfoFrom(host, rec, agentStatusNow(rec))
			return whoamiOut{Found: true, Agent: &ai}
		}
	}
	return whoamiOut{Detail: fmt.Sprintf("tab %q does not map to any lasso agent on host %q — you may be in a terminal lasso did not create.", tabID, host)}
}

// ---------------------------------------------------------------------------
// get_agent
// ---------------------------------------------------------------------------

type getAgentIn struct {
	Host    string `json:"host,omitempty" jsonschema:"Host the agent is on; omit for the local box."`
	AgentID string `json:"agent_id" jsonschema:"The agent's id (from create_agent / list_agents)."`
	Lines   int    `json:"lines,omitempty" jsonschema:"How many lines of recent output to include (default 50)."`
}

type getAgentOut struct {
	Agent  agentInfo `json:"agent"`
	Output string    `json:"output"` // tail of recent terminal output
}

func getAgentTool(_ context.Context, _ *mcp.CallToolRequest, in getAgentIn) (*mcp.CallToolResult, getAgentOut, error) {
	rec, err := findAgentRecord(in.Host, in.AgentID)
	if err != nil {
		return nil, getAgentOut{}, err
	}
	lines := in.Lines
	if lines == 0 {
		lines = 50
	}
	output, _ := agentReadText(rec, "recent", lines)
	return nil, getAgentOut{Agent: agentInfoFrom(hostOrLocal(in.Host), rec, agentStatusNow(rec)), Output: output}, nil
}

// ---------------------------------------------------------------------------
// send_agent
// ---------------------------------------------------------------------------

type sendAgentIn struct {
	Host    string `json:"host,omitempty" jsonschema:"Host the agent is on; omit for the local box."`
	AgentID string `json:"agent_id" jsonschema:"The agent's id."`
	Text    string `json:"text" jsonschema:"Message to send; it is typed into the agent's pane and submitted with Enter."`
}

type sendAgentOut struct {
	Sent bool `json:"sent"`
}

func sendAgentTool(_ context.Context, _ *mcp.CallToolRequest, in sendAgentIn) (*mcp.CallToolResult, sendAgentOut, error) {
	if strings.TrimSpace(in.Text) == "" {
		return nil, sendAgentOut{}, fmt.Errorf("text is required")
	}
	rec, err := findAgentRecord(in.Host, in.AgentID)
	if err != nil {
		return nil, sendAgentOut{}, err
	}
	session := recSession(rec)
	if !tmuxHasSession(session) {
		return nil, sendAgentOut{}, fmt.Errorf("agent %q has no live terminal to send to", in.AgentID)
	}
	tmuxSubmit(session, in.Text)
	return nil, sendAgentOut{Sent: true}, nil
}

// ---------------------------------------------------------------------------
// read_agent
// ---------------------------------------------------------------------------

type readAgentIn struct {
	Host    string `json:"host,omitempty" jsonschema:"Host the agent is on; omit for the local box."`
	AgentID string `json:"agent_id" jsonschema:"The agent's id."`
	Source  string `json:"source,omitempty" jsonschema:"\"recent\" (scrollback, default) or \"visible\" (current screen)."`
	Lines   int    `json:"lines,omitempty" jsonschema:"How many lines to return (default 100)."`
}

type readAgentOut struct {
	Text string `json:"text"`
}

func readAgentTool(_ context.Context, _ *mcp.CallToolRequest, in readAgentIn) (*mcp.CallToolResult, readAgentOut, error) {
	rec, err := findAgentRecord(in.Host, in.AgentID)
	if err != nil {
		return nil, readAgentOut{}, err
	}
	source := in.Source
	if source == "" {
		source = "recent"
	}
	lines := in.Lines
	if lines == 0 {
		lines = 100
	}
	text, err := agentReadText(rec, source, lines)
	if err != nil {
		return nil, readAgentOut{}, err
	}
	return nil, readAgentOut{Text: text}, nil
}

// ---------------------------------------------------------------------------
// wait_agent
// ---------------------------------------------------------------------------

type waitAgentIn struct {
	Host      string `json:"host,omitempty" jsonschema:"Host the agent is on; omit for the local box."`
	AgentID   string `json:"agent_id" jsonschema:"The agent's id."`
	Status    string `json:"status,omitempty" jsonschema:"Status to wait for: idle (default), working, blocked, or unknown."`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"Max time to wait in milliseconds (default 120000)."`
}

type waitAgentOut struct {
	Status  string `json:"status"`  // the status observed when waiting ended
	Matched bool   `json:"matched"` // true if it reached the requested status before timeout
}

func waitAgentTool(ctx context.Context, _ *mcp.CallToolRequest, in waitAgentIn) (*mcp.CallToolResult, waitAgentOut, error) {
	rec, err := findAgentRecord(in.Host, in.AgentID)
	if err != nil {
		return nil, waitAgentOut{}, err
	}
	want := in.Status
	if want == "" {
		want = "idle"
	}
	timeout := time.Duration(in.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = agentStatusNow(rec)
		if last == want {
			return nil, waitAgentOut{Status: last, Matched: true}, nil
		}
		select {
		case <-ctx.Done():
			return nil, waitAgentOut{Status: last}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, waitAgentOut{Status: last, Matched: false}, nil
}

// ---------------------------------------------------------------------------
// close_agent
// ---------------------------------------------------------------------------

type closeAgentIn struct {
	Host           string `json:"host,omitempty" jsonschema:"Host the agent is on; omit for the local box."`
	AgentID        string `json:"agent_id" jsonschema:"The agent's id."`
	ClosePane      *bool  `json:"close_pane,omitempty" jsonschema:"Close the agent's tab (its tmux session) after killing the process. Defaults to true; set false to leave the terminal open as a bare shell."`
	RemoveWorktree bool   `json:"remove_worktree,omitempty" jsonschema:"For a git agent, also delete its git worktree (discards uncommitted work). Defaults to false. Implies closing the pane."`
}

type closeAgentOut struct {
	AgentKilled     bool `json:"agent_killed"`     // the agent process is confirmed gone
	PaneClosed      bool `json:"pane_closed"`      // the agent's tab was closed
	RemovedWorktree bool `json:"removed_worktree"` // the git worktree was deleted
}

func closeAgentTool(_ context.Context, _ *mcp.CallToolRequest, in closeAgentIn) (*mcp.CallToolResult, closeAgentOut, error) {
	rec, err := findAgentRecord(in.Host, in.AgentID)
	if err != nil {
		return nil, closeAgentOut{}, err
	}
	b, err := resolveBackend(in.Host)
	if err != nil {
		return nil, closeAgentOut{}, err
	}
	session := recSession(rec)

	// 1. Always kill the agent process first (Ctrl-C), so it dies even if the
	//    tab/shell is kept.
	killed := tmuxKillAgent(session)
	out := closeAgentOut{AgentKilled: killed}

	// 2. remove_worktree (git only) tears down the whole workspace + git worktree,
	//    which supersedes the close_pane choice.
	if in.RemoveWorktree && rec.Type == "git" {
		if rec.WorkspaceID != "" {
			for _, t := range mustTabs(rec.WorkspaceID) {
				closeOneTab(t.ID)
			}
			_ = closeWorkspace(rec.WorkspaceID)
		} else {
			closeOneTab(rec.TabID)
		}
		if rec.Repo != "" && rec.WorkDir != "" {
			if _, err := b.GitOut(rec.Repo, "worktree", "remove", "--force", rec.WorkDir); err != nil {
				return nil, out, fmt.Errorf("git worktree remove: %w", err)
			}
		}
		// Killing the tabs' sessions (closeOneTab) takes the agent process with them.
		out.AgentKilled, out.PaneClosed, out.RemovedWorktree = true, true, true
		kickHub()
		return nil, out, nil
	}

	// 3. Close the tab unless the caller opted to keep it as a bare shell.
	closePane := in.ClosePane == nil || *in.ClosePane
	if !closePane {
		return nil, out, nil // agent killed; tab/shell left running
	}
	closeOneTab(rec.TabID)
	if len(mustTabs(rec.WorkspaceID)) == 0 {
		_ = closeWorkspace(rec.WorkspaceID)
	}
	// closeOneTab kills the tab's tmux session, which ends the agent process even
	// if the earlier in-band Ctrl-C didn't (agents often ignore a lone SIGINT).
	out.AgentKilled, out.PaneClosed = true, true
	kickHub()
	return nil, out, nil
}

// mustTabs lists a workspace's live tabs, ignoring errors (best-effort teardown).
func mustTabs(workspaceID string) []Tab {
	tabs, _ := listTabs(workspaceID)
	return tabs
}
