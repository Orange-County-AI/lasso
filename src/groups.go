package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Host groups — the reach that sits between "self" and "fleet".
//
// callerscope.go gives one credential either its own host ("self") or the whole
// addressable set ("fleet"), with nothing in between. That is too coarse for the
// common shape: a handful of machines that are peers of each other, and a fleet
// host that may reach them but not the other way round. Groups fill the gap and
// are purely ADDITIVE on top of "self" — no new scope value, no change to
// oauth_clients, and `fleet` keeps its first short-circuit everywhere — so an
// install with no groups behaves exactly as it did before.
//
// Two rules, and only two:
//
//  1. MUTUAL inside a group. Every host in a group's member closure may see and
//     message every other one. There is no half-membership; if two hosts should
//     not be peers, they do not belong in one group.
//  2. DIRECTED between groups. A grant A→B lets A's hosts reach B's hosts and
//     NOT the reverse, and it does not chain: A→B plus B→C gives A nothing in C.
//     One hop is the whole model — transitivity would make the reach of a group
//     depend on edges its operator never looked at.
//
// Members are HOSTS (ssh aliases, or the literal "local"), never client
// credentials, so membership survives re-keying a host: `lasso mcp-client rm`
// followed by `add` lands the new credential in the same groups.
//
// Nesting is a convenience with a consequence worth stating: a host in subgroup
// H nested inside G is mutual with all of closure(G), not just with H. If that
// is not what you want, use a grant instead of nesting — and check the result
// with `lasso mcp-group reach <host>`, which prints every host and the rule that
// put it there.
//
// Everything here resolves in pure Go from two full-table reads. The tables are
// tiny (a handful of rows in the deployments this exists for), recursive CTEs
// appear nowhere else in this repo, and the injected-map form is what lets the
// closure and reach rules be tested without a db at all.

const (
	memberKindHost  = "host"
	memberKindGroup = "group"
)

// groupMember is one row of mcp_group_members: a host alias, or a child group
// to expand. Kind is what disambiguates them — a host and a group may share a
// name, and neither shadows the other.
type groupMember struct {
	Member string
	Kind   string
}

// groupGrant is one directed edge of mcp_group_grants: From's hosts may reach
// To's hosts.
type groupGrant struct {
	From string
	To   string
}

// reachReason is one host a caller may reach and the rule that put it there.
// The reason is the point: with nesting and grants in play, "why can this host
// see that one?" is a question an operator must be able to answer without
// re-deriving the graph in their head.
type reachReason struct {
	Host string
	Why  string
}

// groupClosure is the set of HOSTS a group resolves to: its direct kind='host'
// members plus the closure of every kind='group' child.
//
// Only kind='group' rows expand, so a host member that happens to share a name
// with a group stays a host. A visited set makes cycles (G contains H, H
// contains G) terminate rather than blow the stack — the CLI could refuse to
// create one, but a db written by an older/newer lasso, or edited by hand, is
// not something resolution gets to assume away. A group nobody defined resolves
// to nothing at all, which is what makes a dangling reference inert instead of
// an error, exactly like an ssh alias removed after a host joined a group.
func groupClosure(members map[string][]groupMember, group string) map[string]bool {
	out := map[string]bool{}
	visited := map[string]bool{}
	var walk func(string)
	walk = func(g string) {
		if visited[g] {
			return
		}
		visited[g] = true
		for _, m := range members[g] {
			switch m.Kind {
			case memberKindGroup:
				walk(m.Member)
			case memberKindHost:
				if m.Member != "" {
					out[m.Member] = true
				}
			}
		}
	}
	walk(group)
	return out
}

// hostReachWhy is the reach of one host, with the rule behind each entry:
//
//	⋃ closure(G) for every G whose closure contains host   (mutual membership)
//	⋃ closure(B) for every grant A→B with host ∈ closure(A) (directed, one hop)
//
// The host itself is never in the result — a credential already reaches its own
// host through "self", and reach is only ever the ADDITION. Output is sorted so
// the CLI and any test see a stable order; the first rule that admits a host
// wins its explanation, with membership (the stronger, mutual relation) checked
// before grants.
//
// Nothing here consults the ssh config: enforcement intersects reach with
// addressableHosts() at the point of use, so a member whose alias was removed
// goes inert without a rewrite of the group.
func hostReachWhy(members map[string][]groupMember, grants []groupGrant, host string) []reachReason {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil
	}
	cache := map[string]map[string]bool{}
	closureOf := func(g string) map[string]bool {
		if c, ok := cache[g]; ok {
			return c
		}
		c := groupClosure(members, g)
		cache[g] = c
		return c
	}
	why := map[string]string{}
	add := func(h, reason string) {
		if h == "" || h == host {
			return
		}
		if _, seen := why[h]; !seen {
			why[h] = reason
		}
	}
	for _, g := range sortedGroupNames(members, grants) {
		if !closureOf(g)[host] {
			continue
		}
		for h := range closureOf(g) {
			add(h, fmt.Sprintf("in group %q together", g))
		}
	}
	for _, gr := range grants {
		if !closureOf(gr.From)[host] {
			continue
		}
		for h := range closureOf(gr.To) {
			add(h, fmt.Sprintf("group %q is granted access to group %q", gr.From, gr.To))
		}
	}
	out := make([]reachReason, 0, len(why))
	for h, reason := range why {
		out = append(out, reachReason{Host: h, Why: reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// hostReach is hostReachWhy as a set, which is the form the enforcement path
// wants.
func hostReach(members map[string][]groupMember, grants []groupGrant, host string) map[string]bool {
	reasons := hostReachWhy(members, grants, host)
	if len(reasons) == 0 {
		return nil
	}
	set := make(map[string]bool, len(reasons))
	for _, r := range reasons {
		set[r.Host] = true
	}
	return set
}

// sortedGroupNames is every group name the graph mentions — defined, referenced
// as a subgroup, or named by a grant — in a stable order.
func sortedGroupNames(members map[string][]groupMember, grants []groupGrant) []string {
	seen := map[string]bool{}
	for g, ms := range members {
		seen[g] = true
		for _, m := range ms {
			if m.Kind == memberKindGroup {
				seen[m.Member] = true
			}
		}
	}
	for _, gr := range grants {
		seen[gr.From], seen[gr.To] = true, true
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// db wrappers
// ---------------------------------------------------------------------------

// loadGroupMembers reads mcp_group_members whole, keyed by group.
func loadGroupMembers() (map[string][]groupMember, error) {
	rows, err := db.Query(`SELECT group_name, member, kind FROM mcp_group_members ORDER BY group_name, kind, member`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]groupMember{}
	for rows.Next() {
		var g string
		var m groupMember
		if err := rows.Scan(&g, &m.Member, &m.Kind); err != nil {
			return nil, err
		}
		out[g] = append(out[g], m)
	}
	return out, rows.Err()
}

// loadGroupGrants reads mcp_group_grants whole, in a stable order.
func loadGroupGrants() ([]groupGrant, error) {
	rows, err := db.Query(`SELECT from_group, to_group FROM mcp_group_grants ORDER BY from_group, to_group`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []groupGrant
	for rows.Next() {
		var g groupGrant
		if err := rows.Scan(&g.From, &g.To); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// hostReachWhyFromDB resolves one host's reach against the stored graph.
func hostReachWhyFromDB(host string) ([]reachReason, error) {
	members, err := loadGroupMembers()
	if err != nil {
		return nil, err
	}
	grants, err := loadGroupGrants()
	if err != nil {
		return nil, err
	}
	return hostReachWhy(members, grants, host), nil
}

// hostReachFromDB is the enforcement path's entry point, called once per
// verified request so a group edit lands on the caller's very next tool call —
// no token re-mint, no session restart.
//
// A read error yields an empty reach rather than an error: groups only ever ADD
// to what a credential may address, so failing this way costs the caller its
// group hosts and leaves its own host — the pre-groups behavior — intact. The
// alternative (failing the request) would turn an unreadable groups table into
// an outage for callers that never used a group.
func hostReachFromDB(host string) map[string]bool {
	if db == nil || strings.TrimSpace(host) == "" {
		return nil
	}
	members, err := loadGroupMembers()
	if err != nil {
		return nil
	}
	grants, err := loadGroupGrants()
	if err != nil {
		return nil
	}
	return hostReach(members, grants, host)
}

// mcpGroupCount counts the defined groups, for the startup log. Zero on any
// error — this only feeds a log line.
func mcpGroupCount() int {
	if db == nil {
		return 0
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM mcp_groups`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// ---------------------------------------------------------------------------
// writes — normalization and validation live here, not in the CLI
// ---------------------------------------------------------------------------

// normalizeGroupName trims and refuses an empty name. Names are matched exactly
// (no NOCASE collation anywhere), so "Norm" and "norm" are two groups.
func normalizeGroupName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", fmt.Errorf("a group name is required")
	}
	return n, nil
}

// normalizeHostMember trims a host member and pins the local box to the single
// spelling "local". An empty member is refused outright: isLocalHost("") is
// true, so requireAddressableHost would happily pass "" through and store a key
// that matches no host on any lookup — a member that silently does nothing.
func normalizeHostMember(host string) (string, error) {
	h := strings.TrimSpace(host)
	if h == "" {
		return "", fmt.Errorf("an empty host is not a member — pass an ssh-config alias, or \"local\" for the box lasso runs on")
	}
	if isLocalHost(h) {
		return "local", nil
	}
	return h, nil
}

// createGroup adds a group. It reports false when the group already existed, so
// the CLI can say so rather than pretending it did something.
func createGroup(name string) (bool, error) {
	n, err := normalizeGroupName(name)
	if err != nil {
		return false, err
	}
	res, err := db.Exec(`INSERT OR IGNORE INTO mcp_groups (name, created_at) VALUES (?, ?)`,
		n, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

// deleteGroup removes a group and every trace of it in one transaction: its own
// row, its members, its appearances as a subgroup of other groups, and its
// grants in both directions. sqlite has no cascade here (no FKs, by design), so
// the cascade is spelled out — the alternative is a group that keeps granting
// access after it is gone.
func deleteGroup(name string) (bool, error) {
	n, err := normalizeGroupName(name)
	if err != nil {
		return false, err
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`DELETE FROM mcp_groups WHERE name = ?`, n)
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM mcp_group_members WHERE group_name = ?`, []any{n}},
		{`DELETE FROM mcp_group_members WHERE member = ? AND kind = ?`, []any{n, memberKindGroup}},
		{`DELETE FROM mcp_group_grants WHERE from_group = ? OR to_group = ?`, []any{n, n}},
	} {
		if _, err := tx.Exec(stmt.sql, stmt.args...); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return rows > 0, nil
}

// addGroupMember adds one host or subgroup to a group. Host members are checked
// against the ssh config here (requireAddressableHost) rather than at read time:
// a typo'd alias should fail while the operator is looking at it, and a member
// whose alias is removed LATER goes inert on its own.
//
// A subgroup that does not exist yet is accepted deliberately — it lets a
// hierarchy be written in any order, and until it exists it contributes no
// hosts.
func addGroupMember(group, member, kind string) (bool, error) {
	g, err := normalizeGroupName(group)
	if err != nil {
		return false, err
	}
	var m string
	switch kind {
	case memberKindGroup:
		if m, err = normalizeGroupName(member); err != nil {
			return false, err
		}
		if m == g {
			return false, fmt.Errorf("a group cannot contain itself")
		}
	case memberKindHost:
		if m, err = normalizeHostMember(member); err != nil {
			return false, err
		}
		if err := requireAddressableHost(m); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("unknown member kind %q", kind)
	}
	res, err := db.Exec(
		`INSERT OR IGNORE INTO mcp_group_members (group_name, member, kind) VALUES (?, ?, ?)`, g, m, kind)
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

// removeGroupMember drops one member. The member is normalized the same way it
// was stored, so "lasso mcp-group rm-member g ”" cannot be used to delete the
// row a bad write might have left.
func removeGroupMember(group, member, kind string) (bool, error) {
	g, err := normalizeGroupName(group)
	if err != nil {
		return false, err
	}
	m := strings.TrimSpace(member)
	if kind == memberKindHost {
		if m, err = normalizeHostMember(member); err != nil {
			return false, err
		}
	}
	if m == "" {
		return false, fmt.Errorf("a member name is required")
	}
	res, err := db.Exec(
		`DELETE FROM mcp_group_members WHERE group_name = ? AND member = ? AND kind = ?`, g, m, kind)
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

// addGroupGrant records the directed edge from → to.
func addGroupGrant(from, to string) (bool, error) {
	f, err := normalizeGroupName(from)
	if err != nil {
		return false, err
	}
	t, err := normalizeGroupName(to)
	if err != nil {
		return false, err
	}
	if f == t {
		return false, fmt.Errorf("a group's hosts already reach each other — a grant is between two different groups")
	}
	res, err := db.Exec(
		`INSERT OR IGNORE INTO mcp_group_grants (from_group, to_group) VALUES (?, ?)`, f, t)
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

// removeGroupGrant drops one directed edge, leaving the reverse (if any) alone.
func removeGroupGrant(from, to string) (bool, error) {
	f, err := normalizeGroupName(from)
	if err != nil {
		return false, err
	}
	t, err := normalizeGroupName(to)
	if err != nil {
		return false, err
	}
	res, err := db.Exec(`DELETE FROM mcp_group_grants WHERE from_group = ? AND to_group = ?`, f, t)
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

// listGroupNames returns the defined groups, oldest first.
func listGroupNames() ([]string, error) {
	rows, err := db.Query(`SELECT name FROM mcp_groups ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// groupExists reports whether a group has a row of its own — as opposed to only
// being referenced by some member or grant, which resolution tolerates but an
// operator usually wants to hear about.
func groupExists(name string) bool {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM mcp_groups WHERE name = ?`, name).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
