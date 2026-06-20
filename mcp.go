package main

import (
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP server: an unauthenticated Model Context Protocol endpoint mounted at /mcp
// (see main.go's route table + withAuthExcept). Its purpose is to let an agent
// session — typically a Claude Code session running inside herdr via lasso, or
// Claude desktop/mobile reaching the HTTP endpoint — orchestrate OTHER lasso
// agents: spawn them (in their own worktree/workspace, off a chosen base
// branch), then converse with them statefully through their herdr pane.
//
// Every tool reuses the same machinery the React UI drives (createAgent,
// gridHostBackend, listAgents, paneRun, pane.read, …). Tools take an optional
// `host` and resolve it through gridHostBackend, so a session can drive agents
// on any reachable host without disturbing the UI's active host.

// newMCPHandler builds the MCP server, registers the tools, and returns the
// Streamable-HTTP handler to mount at /mcp. The getServer closure hands every
// request the one shared server (lasso has a single global herdr/state surface,
// so there's nothing per-connection to scope).
func newMCPHandler() *mcp.StreamableHTTPHandler {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "lasso",
		Title:   "Lasso agent orchestrator",
		Version: lassoSemver,
	}, nil)
	registerMCPTools(srv)
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
// gridHostBackend returns a backend for any reachable+compatible host without
// mutating the UI's active host.
func resolveBackend(host string) (Backend, error) {
	if host == "" {
		host = "local"
	}
	return gridHostBackend(host)
}

// findAgentRecord looks up an agent created on host by its lasso id, so the
// interaction tools can recover its root pane (the herdr pane the agent runs in)
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
