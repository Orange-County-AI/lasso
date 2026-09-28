package main

import (
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP server: an unauthenticated Model Context Protocol endpoint mounted at /mcp
// (see main.go's route table + withAuthExcept). Its purpose is to let an agent
// session — typically a Claude Code session running inside herdr via lasso, or
// Claude desktop/mobile reaching the HTTP endpoint — orchestrate OTHER lasso
// agents: spawn them (in their own worktree/workspace, off a chosen base
// branch), list and inspect them, and close them. Conversing with an agent —
// prompting it, reading its screen, waiting on it — is deliberately NOT here:
// that is herdr's job (`herdr agent prompt/read/wait`), and a second way to do
// it only taught agents to reach for lasso when herdr was the right tool.
//
// Every tool reuses the same machinery the React UI drives (createAgent,
// hostBackend, listAgents, paneRun, …). Tools take an optional
// `host` and resolve it through hostBackend, so a session can drive agents
// on any reachable host without disturbing the UI's active host.
//
// That reach is bounded: an agent sees and manages agents on its own box or on
// a host with an alias in lasso's ssh config, and nowhere else. hostscope.go
// holds the rule and the reasons; every tool that answers a host-scoped question
// from the agents db runs it first.

// newMCPHandler builds the MCP server, registers the tools, and returns the
// Streamable-HTTP handler to mount at /mcp. The getServer closure hands every
// request the one shared server (lasso has a single global herdr/state surface,
// so there's nothing per-connection to scope).
// mcpInstructions is surfaced to the model once per MCP session through
// initialize, so shared guidance belongs here rather than repeated in every
// tool description.
const mcpInstructions = `Lasso orchestrates coding agents in herdr panes: spawn them, inspect them, and manage their lifecycle.

notify pushes a notification to the HUMAN who runs this lasso (their phone, if lasso is on its home screen). Use it only when you need them — a decision, a blocking question, a long job finishing while they are away — and check the reply's "sent": false means nobody received it.

Use lasso for create_agent, close_agent, whoami, list_hosts, list_repos, list_branches, list_agents, get_agent, notify, and shared_browser (a Chromium the human watches live in lasso's Browser tab: it answers the browser MCP URL, /browser-mcp, that gives you chrome-devtools-mcp's tools against it, and the raw CDP endpoint).

The shared browser has PROFILES — each its own Chromium with its own cookies, logins and optional proxy (socks5://…). Manage them with list_browser_profiles, create_browser_profile, update_browser_profile and delete_browser_profile. To put a page on the human's screen (optionally in a given profile), use open_browser_tab; show_browser_tab, list_browser_tabs and close_browser_tab manage the tabs that exist. Each profile has its own browser MCP URL, /browser-mcp/<id>, for driving its pages.

Lasso does not talk to agents. To prompt another agent, read its screen, or wait for it to finish, use herdr directly (herdr agent prompt / read / wait, or the herdr skill); a Claude Code session can also use its own native agent messaging.

Host reach is bounded by the calling credential, so an empty listing usually means containment is working as intended, not an outage.

If you are running in a lasso-created pane, $HERDR_PANE_ID is your pane id: pass it as pane_id to whoami to find your own agent record. To shut yourself or another lasso agent down, use close_agent — never herdr pane close on a pane lasso created, which leaves its agent record and staged prompt files behind.

To show the human what you are working on, put a one-line summary (under ~60 characters) on your pane's status card in the herdr sidebar and lasso's pane switcher, and update it when you change phase: herdr pane report-metadata "$HERDR_PANE_ID" --source agent:self --token summary="<what you're doing>" --ttl-ms 1800000. Don't use herdr pane report-agent; it overrides herdr's own idle/working/blocked detection and a stale claim sticks.`

// sharedMCPServer is the one *mcp.Server behind /mcp, kept so the plugin
// manager (plugins.go) can add and remove a plugin's mirrored tools at runtime;
// the SDK tells every connected session with tools/list_changed.
var sharedMCPServer atomic.Pointer[mcp.Server]

func newMCPHandler() *mcp.StreamableHTTPHandler {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "lasso",
		Title:   "Lasso agent orchestrator",
		Version: lassoSemver,
	}, &mcp.ServerOptions{Instructions: mcpInstructions})
	registerMCPTools(srv)
	sharedMCPServer.Store(srv)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		// lasso binds to loopback and is reached over the Cloudflare tunnel under a
		// public hostname (e.g. lasso.knowsuchagency.ai). The SDK's default DNS-
		// rebinding guard rejects any non-loopback Host header when the listener is
		// on loopback, which 403s ("Forbidden: invalid Host header") every tunnelled
		// request before it reaches a tool — the actual cause of remote MCP clients
		// (Claude desktop/mobile) failing *after* a successful Access OAuth login.
		// The trust gate here is Cloudflare Access (OAuth + policy) / the tailnet,
		// not the Host header, so disable the loopback guard. See CLAUDE.md.
		DisableLocalhostProtection: true,
	})
}

// resolveBackend maps a tool's optional `host` argument to a Backend. An empty
// host means "the box lasso runs on" (local) — the default the user asked for.
// hostBackend returns a backend for any reachable+compatible host without
// mutating the UI's active host.
//
// A host with no alias in lasso's ssh config is refused up front (see
// hostscope.go): hostBackend would fail on it anyway, but "not available"
// reads like a machine that is merely down, and the distinction between "asleep
// for now" and "not a host you may address at all" is the whole point of the
// scope rule.
// A var so a test can stand a fake herdr in for the pool (the same seam
// agentBackendResolver is for the close path).
var resolveBackend = func(host string) (Backend, error) {
	if host == "" {
		host = "local"
	}
	if err := requireAddressableHost(host); err != nil {
		return nil, err
	}
	return hostBackend(host)
}

// findAgentRecord looks up an agent created on host by its lasso id, so a
// caller (closeme) can recover its root pane (the herdr pane the agent runs in)
// from the persisted record.
func findAgentRecord(host, id string) (AgentRecord, error) {
	if host == "" {
		host = "local"
	}
	recs, err := listAgents(host)
	if err != nil {
		return AgentRecord{}, err
	}
	for _, r := range recs {
		if r.ID == id {
			return r, nil
		}
	}
	return AgentRecord{}, fmt.Errorf("no agent %q on host %q", id, host)
}
