package main

import (
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Caller scope — how far ONE MCP caller may reach.
//
// hostscope.go bounds what this lasso can address at all (local + the ssh-config
// aliases). That bound is the same for everybody, which is the problem this file
// solves: agents on every host share one /mcp endpoint, so a contained agent —
// one running in a pod with its own secrets and network access — could see and
// drive the whole fleet, escaping the containment its host was given.
//
// The fix is per-caller scope, and it hinges on identity coming from the
// CREDENTIAL rather than from the caller. MCP has nothing to offer here: as of
// revision 2025-11-25 `clientInfo` is a self-asserted name/version/title, no
// header identifies the caller, and the spec's own guidance on session hijacking
// is that the principal must be "derived from the user token and not provided by
// the client". So lasso mints one OAuth client per host (`lasso mcp-client
// add`), and mcpTokenVerifier hands the resolved host + scope to every tool call
// in the SDK's TokenInfo. A caller cannot spoof its host without stealing
// another host's credential.
//
// Two scopes:
//
//	fleet — may address every host hostscope.go allows. What the pre-registered
//	        MCP_OAUTH client, DCR connectors (a human passed the door and the
//	        consent screen), and unidentified callers get. Unidentified is the
//	        historical behavior, so an install with MCP_OAUTH unset — where /mcp
//	        is open anyway — behaves exactly as before.
//	self  — may address only its own host. The default for a per-host client,
//	        because containment is the reason to mint one.

const (
	scopeSelf  = "self"
	scopeFleet = "fleet"

	// Keys under which mcpTokenVerifier stashes the credential's host and scope
	// in auth.TokenInfo.Extra. Reverse-DNS-ish to stay clear of anything the SDK
	// or spec might put there.
	tokenHostKey  = "lasso.host"
	tokenScopeKey = "lasso.scope"
)

// mcpCaller is one caller's resolved identity and reach. The zero value is an
// unidentified fleet caller, which is what every pre-OAuth code path gets.
type mcpCaller struct {
	ClientID string // the OAuth client the token was issued to, when identified
	Host     string // the host that credential was provisioned for ("" = unknown)
	Fleet    bool   // may address every addressable host
}

// anyCaller is the unidentified, fleet-wide caller: the HTTP endpoints (which
// have their own UI_AUTH gate), the message dispatcher, and tests.
func anyCaller() mcpCaller { return mcpCaller{Fleet: true} }

// callerFrom resolves the caller behind a tool call from its bearer token.
//
// No token means no identity, and no identity means the historical fleet-wide
// view — /mcp is unauthenticated unless MCP_OAUTH is set, and the UI_AUTH basic
// credentials remain a deliberate escape hatch, so this must not be read as a
// containment boundary on its own. Containment requires MCP_OAUTH plus a
// per-host client; that is stated in CLAUDE.md and in `lasso mcp-client` help.
func callerFrom(req *mcp.CallToolRequest) mcpCaller {
	if req == nil || req.Extra == nil || req.Extra.TokenInfo == nil {
		return anyCaller()
	}
	ti := req.Extra.TokenInfo
	c := mcpCaller{ClientID: ti.UserID}
	host, _ := ti.Extra[tokenHostKey].(string)
	scope, _ := ti.Extra[tokenScopeKey].(string)
	c.Host = host
	// A credential with no host names no machine, so it cannot be confined to
	// one; only an explicit "self" on a host-bearing client narrows the view.
	c.Fleet = host == "" || scope != scopeSelf
	return c
}

// identified reports whether the caller's own host is known.
func (c mcpCaller) identified() bool { return c.Host != "" }

// hostOr resolves a tool's optional `host` argument for this caller. An explicit
// host wins. Otherwise an identified caller defaults to ITS OWN host rather than
// to "local" — "local" means the box lasso runs on, which for a remote caller is
// somebody else's machine entirely, so the old default silently pointed every
// remote agent at the lasso host's agents and called them its neighbours.
func (c mcpCaller) hostOr(host string) string {
	if host != "" {
		return host
	}
	if c.Host != "" {
		return c.Host
	}
	return "local"
}

// searchHost resolves the host for a tool whose EMPTY host means "search every
// host I may reach" rather than "the local box" — whoami and close_agent, which
// resolve a pane or agent id against every host's records. An identified caller
// collapses that search to its own host (its credential already says where it
// is); an unidentified one keeps the cross-host search it has always had.
//
// Deliberately not hostOr: defaulting these two to "local" would turn a
// documented fleet-wide search into a local-only lookup and stop finding the
// remote agent the caller asked about.
func (c mcpCaller) searchHost(host string) string {
	if host != "" {
		return host
	}
	return c.Host
}

// requireHost is the gate every host-scoped tool runs. Both bounds apply: the
// host must be one this lasso can address at all, and one this caller may reach.
func (c mcpCaller) requireHost(host string) error {
	if err := requireAddressableHost(host); err != nil {
		return err
	}
	if c.allows(host) {
		return nil
	}
	return fmt.Errorf("this credential may only address host %q, not %q: it was provisioned for agents running on %q with scope %q — ask the operator for a fleet-scoped credential (`lasso mcp-client add --host %s --fleet`) if it should reach further",
		c.Host, host, c.Host, scopeSelf, c.Host)
}

// allows reports whether the caller may address host, ignoring the separate
// question of whether lasso can reach it.
func (c mcpCaller) allows(host string) bool {
	if c.Fleet {
		return true
	}
	if isLocalHost(host) {
		// "local" is the box lasso runs on. A self-scoped caller may only mean it
		// if that IS its host — otherwise the empty/"local" default would quietly
		// hand every contained caller the lasso host.
		return isLocalHost(c.Host)
	}
	return host == c.Host
}

// hosts is the set of hosts this caller may address: the addressable set for a
// fleet caller, and at most its own host otherwise. A self-scoped caller whose
// host has no ssh alias gets an EMPTY set — lasso cannot reach it, so it has
// nothing to offer even about the caller's own machine.
func (c mcpCaller) hosts() map[string]bool {
	set := addressableHosts()
	if c.Fleet {
		return set
	}
	if c.Host != "" && set[c.Host] {
		return map[string]bool{c.Host: true}
	}
	return map[string]bool{}
}

// agents narrows a cross-host record set to what this caller may act on, so a
// resolution that spans hosts (message recipients, whoami/close with no host)
// can only ever land inside its scope. Order is preserved.
func (c mcpCaller) agents(all []hostAgent) []hostAgent {
	if c.Fleet {
		return addressableAgents(all)
	}
	set := c.hosts()
	out := make([]hostAgent, 0, len(all))
	for _, ha := range all {
		if set[ha.Host] {
			out = append(out, ha)
		}
	}
	return out
}
