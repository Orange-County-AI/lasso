package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

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
//	self  — may address only its own host, PLUS whatever host groups add to it
//	        (groups.go). The default for a per-host client, because containment
//	        is the reason to mint one.
//
// Groups are additive and nothing else: they only ever widen a self-scoped
// caller, they are resolved from the caller's host (which comes from the
// credential, not the caller), and `Fleet` stays the first short-circuit in
// allows/hosts/agents so a fleet or unidentified caller cannot be affected by
// any group edit.

const (
	scopeSelf  = "self"
	scopeFleet = "fleet"

	// Keys under which mcpTokenVerifier stashes the credential's host, scope, and
	// resolved group reach in auth.TokenInfo.Extra. Reverse-DNS-ish to stay clear
	// of anything the SDK or spec might put there.
	tokenHostKey  = "lasso.host"
	tokenScopeKey = "lasso.scope"
	tokenReachKey = "lasso.reach"
)

// mcpCaller is one caller's resolved identity and reach. The zero value is an
// unidentified fleet caller, which is what every pre-OAuth code path gets.
type mcpCaller struct {
	ClientID string // the OAuth client the token was issued to, when identified
	Host     string // the host that credential was provisioned for ("" = unknown)
	Fleet    bool   // may address every addressable host
	// Reach is the set of OTHER hosts this caller's host groups put within its
	// reach — nil for a fleet or unidentified caller (they are not narrowed in
	// the first place) and nil for a self-scoped host in no group, which is the
	// pre-groups shape. It is resolved per request at token-verification time,
	// so a group edit lands on the caller's very next tool call.
	Reach map[string]bool
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
	// The group reach the verifier resolved for this host, if any. Read as a
	// ready-made set rather than re-resolved here: a tool call must not turn into
	// two more db reads, and the verifier already ran once for this request.
	if reach, ok := ti.Extra[tokenReachKey].(map[string]bool); ok && len(reach) > 0 {
		c.Reach = reach
	}
	return c
}

// identified reports whether the caller's own host is known.
func (c mcpCaller) identified() bool { return c.Host != "" }

// reachesBeyondOwnHost reports whether a group has widened this caller past its
// own host. Only close_agent asks: it decides whether an empty `host` should
// stay empty (so the cross-host search runs) instead of collapsing to the
// caller's own host.
func (c mcpCaller) reachesBeyondOwnHost() bool {
	for h := range c.Reach {
		if h != c.Host {
			return true
		}
	}
	return false
}

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
//
// Groups do NOT widen this. whoami and message_agent's sender resolution stay
// pinned to the caller's own host — a caller's own pane is by definition on the
// host its credential names, and searching a group-mate for it would only
// reintroduce the cross-host pane-id collision that per-host identity retired.
// close_agent is the exception, and it overrides the result itself (see
// closeAgentTool) rather than bending the rule here for both.
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
	// Say what the credential is, what it currently reaches, and BOTH ways to
	// widen it. Since host groups exist, the answer to "it should reach further"
	// is usually a group rather than a fleet credential — and an operator who only
	// hears about --fleet will hand out the fleet.
	return fmt.Errorf("this credential may not address host %q: it was provisioned for agents running on %q with scope %q, so it reaches %s — put both hosts in one group (`lasso mcp-group add-member <group> %s %s`), grant one group access to the other (`lasso mcp-group grant`), or mint a fleet-scoped credential (`lasso mcp-client add --host %s --fleet`)",
		host, c.Host, scopeSelf, c.reachSummary(), c.Host, host, c.Host)
}

// reachSummary spells out what a scoped caller currently reaches, for refusals.
func (c mcpCaller) reachSummary() string {
	if !c.reachesBeyondOwnHost() {
		return fmt.Sprintf("only %q", c.Host)
	}
	others := make([]string, 0, len(c.Reach))
	for h := range c.Reach {
		if h != c.Host {
			others = append(others, strconv.Quote(h))
		}
	}
	sort.Strings(others)
	return fmt.Sprintf("%q plus its groups (%s)", c.Host, strings.Join(others, ", "))
}

// allows reports whether the caller may address host, ignoring the separate
// question of whether lasso can reach it.
func (c mcpCaller) allows(host string) bool {
	if c.Fleet {
		return true
	}
	if isLocalHost(host) {
		// "local" is the box lasso runs on. A self-scoped caller may only mean it
		// if that IS its host, or if a group put the lasso host within its reach —
		// otherwise the empty/"local" default would quietly hand every contained
		// caller the lasso host.
		return isLocalHost(c.Host) || c.Reach["local"]
	}
	return host == c.Host || c.Reach[host]
}

// hosts is the set of hosts this caller may address: the addressable set for a
// fleet caller, and its own host plus its group reach otherwise. Both are
// intersected with what lasso can address, so a caller whose host — or whose
// group-mate — has no ssh alias gets nothing for it: lasso cannot reach it, so
// it has nothing to offer about it.
func (c mcpCaller) hosts() map[string]bool {
	set := addressableHosts()
	if c.Fleet {
		return set
	}
	out := map[string]bool{}
	if c.Host != "" && set[c.Host] {
		out[c.Host] = true
	}
	for h := range c.Reach {
		if set[h] {
			out[h] = true
		}
	}
	return out
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
