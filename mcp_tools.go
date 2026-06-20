package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP tool surface. Each tool is a thin wrapper over lasso's existing herdr
// machinery, resolved against an optional `host` (default "local") via
// resolveBackend. Three groups:
//   - discovery:   list_hosts, list_repos, list_branches
//   - spawning:    create_agent (loop it for the bulk "one per repo" case)
//   - interaction: list_agents, get_agent, send_agent, read_agent, wait_agent,
//                  close_agent  (the herdr pane is the stateful conversation)
//   - introspection: whoami (an agent maps its own $HERDR_PANE_ID back to its
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
		Description: "Spawn a coding agent (claude or codex) in its own herdr workspace. type=git creates a fresh git worktree off base_branch (default the repo's HEAD) under a new branch; type=scratch creates an empty workspace. The optional prompt becomes the agent's initial task. Returns immediately with the agent's id, workspace, and root pane; the agent boots asynchronously. By default it does NOT switch the herdr view to the new pane (so it won't yank a watching user away); pass focus:true to land on it. To bring many repos up to date, call this once per repo.",
	}, createAgentTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_agents",
		Description: "List the agents lasso has created on a host, each with its live status (working/idle/blocked/unknown) when the agent is running.",
	}, listAgentsTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "whoami",
		Description: "Identify the calling agent's OWN lasso agent record, so it can then act on itself — most commonly to call close_agent with the returned id once its work is done. Pass the value of your $HERDR_PANE_ID environment variable as pane_id (e.g. \"p_82\"); lasso maps that herdr pane to the agent it created there. The lasso MCP server runs in lasso's own process, NOT your shell, so it cannot read your environment — you MUST supply $HERDR_PANE_ID yourself. On success returns found:true and the same fields as a list_agents entry (id, type, title, repo, branch, work_dir, root_pane, workspace_id, status, host, ...) under `agent`. If it can't resolve (no pane_id given, or the pane isn't one lasso manages) it returns found:false with a human-readable `detail` instead of erroring.",
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
		Description: "Stop an agent: first kill the agent process (claude/codex) in its pane, then — unless close_pane is false — close the associated herdr pane. For a git agent, set remove_worktree=true to also delete its git worktree (this discards any uncommitted work, so it defaults to false, and implies closing the pane).",
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
		WorkDir: rec.WorkDir, WorkspaceID: rec.WorkspaceID, RootPane: rec.RootPane,
		Status: status, CreatedAt: rec.CreatedAt.Format(time.RFC3339),
	}
}

// paneAgentStatus returns the herdr agent_status for a pane (working/idle/
// blocked/unknown), or "" if the pane is gone or carries no agent.
func paneAgentStatus(b Backend, paneID string) string {
	res, err := b.HerdrCall("pane.list", map[string]any{})
	if err != nil {
		return ""
	}
	var pl struct {
		Panes []pane `json:"panes"`
	}
	if json.Unmarshal(res, &pl) != nil {
		return ""
	}
	for _, p := range pl.Panes {
		if p.PaneID == paneID {
			return p.AgentStatus
		}
	}
	return ""
}

// paneHasAgent reports whether herdr still sees an agent running in the pane. It
// returns false once the agent process has exited (the pane's agent field
// clears) or the pane is gone — the signal killPaneAgent waits on.
func paneHasAgent(b Backend, paneID string) bool {
	res, err := b.HerdrCall("pane.list", map[string]any{})
	if err != nil {
		return false
	}
	var pl struct {
		Panes []pane `json:"panes"`
	}
	if json.Unmarshal(res, &pl) != nil {
		return false
	}
	for _, p := range pl.Panes {
		if p.PaneID == paneID {
			return p.Agent != ""
		}
	}
	return false // pane gone → no agent left to kill
}

// killPaneAgent terminates the agent process running in a pane without closing
// the pane itself: it sends Ctrl-C (ETX) — claude and codex both exit on a
// double interrupt at their prompt — and polls until herdr reports the agent
// gone, so the pane drops back to a bare shell. Returns whether the agent is
// confirmed gone. Deliberately avoids Ctrl-D (EOF), which would also exit the
// shell and close the pane — the caller decides separately whether to close it.
func killPaneAgent(b Backend, paneID string) bool {
	if paneID == "" || !paneHasAgent(b, paneID) {
		return true
	}
	interrupt := func() {
		_, _ = b.HerdrCall("pane.send_text", map[string]any{"pane_id": paneID, "text": "\x03"})
	}
	for attempt := 0; attempt < 3; attempt++ {
		interrupt()
		time.Sleep(400 * time.Millisecond)
		interrupt() // the second interrupt is what makes claude/codex exit
		for i := 0; i < 5; i++ {
			time.Sleep(300 * time.Millisecond)
			if !paneHasAgent(b, paneID) {
				return true
			}
		}
	}
	return !paneHasAgent(b, paneID)
}

// paneReadText reads a pane's output via herdr's pane.read.
func paneReadText(b Backend, paneID, source string, lines int) (string, error) {
	params := map[string]any{"pane_id": paneID, "source": source}
	if lines > 0 {
		params["lines"] = lines
	}
	res, err := b.HerdrCall("pane.read", params)
	if err != nil {
		return "", err
	}
	var r struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return "", err
	}
	return r.Read.Text, nil
}

// herdrPaneInfo is the slice of herdr's pane.get response whoami needs: the
// canonical public pane id and the live agent status.
type herdrPaneInfo struct {
	PaneID      string `json:"pane_id"`
	AgentStatus string `json:"agent_status"`
}

// paneGet resolves a pane id via herdr's pane.get. herdr accepts BOTH the raw
// form an agent reads from its $HERDR_PANE_ID env var (e.g. "p_82", herdr's
// internal global pane counter) AND the public form lasso persists as an agent's
// root_pane (e.g. "w<workspace>-<n>"), and echoes back the public id either way —
// so this is how whoami translates the env-reported pane id into the key it
// matches agents on. ok is false if herdr can't resolve the id (pane gone, or
// herdr unreachable).
func paneGet(b Backend, paneID string) (herdrPaneInfo, bool) {
	res, err := b.HerdrCall("pane.get", map[string]any{"pane_id": paneID})
	if err != nil {
		return herdrPaneInfo{}, false
	}
	var r struct {
		Pane herdrPaneInfo `json:"pane"`
	}
	if json.Unmarshal(res, &r) != nil || r.Pane.PaneID == "" {
		return herdrPaneInfo{}, false
	}
	return r.Pane, true
}

// ---------------------------------------------------------------------------
// list_hosts
// ---------------------------------------------------------------------------

type listHostsIn struct{}

type hostEntry struct {
	Host       string `json:"host"`       // value to pass as `host` ("local" or an alias)
	Label      string `json:"label"`      // display name
	Reachable  bool   `json:"reachable"`  // ssh reachable + probed (always true for local)
	Running    bool   `json:"running"`    // herdr server up on the host
	Compatible bool   `json:"compatible"` // herdr protocol matches this lasso
	Version    string `json:"version,omitempty"`
}

type listHostsOut struct {
	Active string      `json:"active"` // the host the lasso UI currently drives
	Hosts  []hostEntry `json:"hosts"`
}

func listHostsTool(ctx context.Context, _ *mcp.CallToolRequest, _ listHostsIn) (*mcp.CallToolResult, listHostsOut, error) {
	ver, _ := localProtocol()
	out := listHostsOut{
		Active: curBackend().Name(),
		Hosts: []hostEntry{{
			Host: "local", Label: localHostname(), Reachable: true,
			Running: true, Compatible: true, Version: ver,
		}},
	}
	for _, h := range discoverHosts(ctx, false) {
		out.Hosts = append(out.Hosts, hostEntry{
			Host: h.Alias, Label: h.Alias, Reachable: h.Reachable,
			Running: h.Running, Compatible: h.Compatible, Version: h.Version,
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
	Focus        bool   `json:"focus,omitempty" jsonschema:"Switch the herdr view to the new agent's pane as it boots. Defaults to false so spawning an agent doesn't yank you away from your current pane."`
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
	// One pane.list for the whole host, so status enrichment is a single RPC.
	b, berr := resolveBackend(host)
	statuses := map[string]string{}
	if berr == nil {
		if res, e := b.HerdrCall("pane.list", map[string]any{}); e == nil {
			var pl struct {
				Panes []pane `json:"panes"`
			}
			if json.Unmarshal(res, &pl) == nil {
				for _, p := range pl.Panes {
					statuses[p.PaneID] = p.AgentStatus
				}
			}
		}
	}
	out := listAgentsOut{Host: host}
	for _, rec := range recs {
		out.Agents = append(out.Agents, agentInfoFrom(host, rec, statuses[rec.RootPane]))
	}
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// whoami
// ---------------------------------------------------------------------------

type whoamiIn struct {
	Host   string `json:"host,omitempty" jsonschema:"Host the calling agent runs on; omit for the local box."`
	PaneID string `json:"pane_id,omitempty" jsonschema:"Your own herdr pane id — the value of the $HERDR_PANE_ID environment variable in your shell (e.g. \"p_82\"). The server cannot read your environment, so you must pass it. The public form (\"w<workspace>-<n>\") is also accepted."`
}

type whoamiOut struct {
	Found  bool       `json:"found"`            // true if the pane resolved to a lasso agent
	Agent  *agentInfo `json:"agent,omitempty"`  // the resolved agent (null when found is false)
	Detail string     `json:"detail,omitempty"` // why resolution failed, when found is false
}

func whoamiTool(_ context.Context, _ *mcp.CallToolRequest, in whoamiIn) (*mcp.CallToolResult, whoamiOut, error) {
	host := in.Host
	if host == "" {
		host = "local"
	}
	b, err := resolveBackend(host)
	if err != nil {
		return nil, whoamiOut{}, err
	}
	recs, err := listAgents(host)
	if err != nil {
		return nil, whoamiOut{}, err
	}
	return nil, resolveWhoami(b, host, recs, in.PaneID), nil
}

// resolveWhoami maps a herdr pane id to the lasso agent that owns it. It asks
// herdr to canonicalize the id (so the raw $HERDR_PANE_ID form resolves to the
// public root_pane lasso stores), then matches it against the host's agents.
// Never errors — an unresolvable pane yields found:false with an explanation, so
// an agent calling whoami on itself gets a usable answer either way.
func resolveWhoami(b Backend, host string, recs []AgentRecord, paneID string) whoamiOut {
	paneID = strings.TrimSpace(paneID)
	if paneID == "" {
		return whoamiOut{Detail: "no pane_id given: pass the value of your $HERDR_PANE_ID environment variable (e.g. \"p_82\"). If that variable is empty or unset, you are not running inside a lasso-managed herdr pane."}
	}
	// herdr's pane.get accepts both the raw env form and the public form and
	// echoes the public pane id lasso records as root_pane. Fall back to the raw
	// id if herdr can't resolve it (so a caller that already passed the public
	// form still matches even when herdr is unreachable).
	match, status := paneID, ""
	if info, ok := paneGet(b, paneID); ok {
		match, status = info.PaneID, info.AgentStatus
	}
	for _, rec := range recs {
		if rec.RootPane != "" && rec.RootPane == match {
			if status == "" {
				status = paneAgentStatus(b, rec.RootPane)
			}
			ai := agentInfoFrom(host, rec, status)
			return whoamiOut{Found: true, Agent: &ai}
		}
	}
	return whoamiOut{Detail: fmt.Sprintf("pane %q does not map to any lasso agent on host %q — you may be in a herdr pane lasso did not create, or on a different host than the one you queried.", paneID, host)}
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
	b, err := resolveBackend(in.Host)
	if err != nil {
		return nil, getAgentOut{}, err
	}
	lines := in.Lines
	if lines == 0 {
		lines = 50
	}
	status := paneAgentStatus(b, rec.RootPane)
	output, _ := paneReadText(b, rec.RootPane, "recent", lines)
	return nil, getAgentOut{Agent: agentInfoFrom(b.Name(), rec, status), Output: output}, nil
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
	b, err := resolveBackend(in.Host)
	if err != nil {
		return nil, sendAgentOut{}, err
	}
	if rec.RootPane == "" {
		return nil, sendAgentOut{}, fmt.Errorf("agent %q has no pane to send to", in.AgentID)
	}
	paneSubmit(b, rec.RootPane, in.Text)
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
	b, err := resolveBackend(in.Host)
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
	text, err := paneReadText(b, rec.RootPane, source, lines)
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
	b, err := resolveBackend(in.Host)
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
		last = paneAgentStatus(b, rec.RootPane)
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
	ClosePane      *bool  `json:"close_pane,omitempty" jsonschema:"Close the agent's herdr pane after killing the process. Defaults to true; set false to leave the pane open as a bare shell."`
	RemoveWorktree bool   `json:"remove_worktree,omitempty" jsonschema:"For a git agent, also delete its git worktree (discards uncommitted work). Defaults to false. Implies closing the pane."`
}

type closeAgentOut struct {
	AgentKilled     bool `json:"agent_killed"`     // the agent process is confirmed gone
	PaneClosed      bool `json:"pane_closed"`      // the herdr pane was closed
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

	// 1. Always kill the agent process first, so it dies even if the pane is kept.
	killed := killPaneAgent(b, rec.RootPane)
	out := closeAgentOut{AgentKilled: killed}

	// 2. remove_worktree (git only) tears down the worktree, which also closes
	//    the pane — so it supersedes the close_pane choice.
	if in.RemoveWorktree && rec.Type == "git" {
		if rec.WorkspaceID == "" {
			return nil, out, fmt.Errorf("agent %q has no workspace to remove", in.AgentID)
		}
		if _, err := b.HerdrCall("worktree.remove", map[string]any{
			"workspace_id": rec.WorkspaceID,
			"force":        true,
		}); err != nil {
			return nil, out, fmt.Errorf("worktree.remove: %w", err)
		}
		out.PaneClosed, out.RemovedWorktree = true, true
		return nil, out, nil
	}

	// 3. Close the pane unless the caller opted to keep it (default: close).
	closePane := in.ClosePane == nil || *in.ClosePane
	if !closePane {
		return nil, out, nil
	}
	if rec.RootPane == "" {
		return nil, out, fmt.Errorf("agent %q has no pane to close", in.AgentID)
	}
	if _, err := b.HerdrCall("pane.close", map[string]any{"pane_id": rec.RootPane}); err != nil {
		return nil, out, fmt.Errorf("pane.close: %w", err)
	}
	out.PaneClosed = true
	return nil, out, nil
}
