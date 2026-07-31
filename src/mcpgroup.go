package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

// `lasso mcp-group` — the host groups that sit between "self" and "fleet".
//
// Groups are made of HOSTS, not credentials (groups.go says why), so this
// command never touches oauth_clients: a host joins a group once and keeps its
// reach across every re-keying of the credential that host authenticates with.
//
// Like `lasso mcp-client`, it talks to the db directly rather than to a running
// server — sqlite is in WAL mode with a busy timeout, so writing alongside a
// live lasso is safe, and provisioning works whether or not the server is up.
// The server resolves reach on every verified request, so an edit made here
// lands on the affected callers' NEXT tool call; nothing has to be restarted
// and no token has to be re-minted.

// cliMCPGroup dispatches `lasso mcp-group <cmd>`.
func cliMCPGroup(args []string) {
	if err := openDB(); err != nil {
		fmt.Fprintf(os.Stderr, "lasso: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "add":
		mcpGroupAdd(args[1:])
	case "rm", "remove", "delete":
		mcpGroupRemove(args[1:])
	case "list", "ls":
		mcpGroupList()
	case "add-member":
		mcpGroupAddMember(args[1:])
	case "rm-member", "remove-member":
		mcpGroupRemoveMember(args[1:])
	case "grant":
		mcpGroupGrant(args[1:])
	case "revoke":
		mcpGroupRevoke(args[1:])
	case "reach":
		mcpGroupReach(args[1:])
	default:
		printMCPGroupUsage(os.Stderr)
		os.Exit(2)
	}
}

func printMCPGroupUsage(w *os.File) {
	fmt.Fprint(w, `lasso mcp-group — host groups: which hosts' agents may see and message each other

usage:
  lasso mcp-group add <name>
  lasso mcp-group rm <name>
  lasso mcp-group list
  lasso mcp-group add-member <group> <host|@group>...
  lasso mcp-group rm-member <group> <host|@group>
  lasso mcp-group grant <from-group> <to-group>
  lasso mcp-group revoke <from-group> <to-group>
  lasso mcp-group reach <host>

Members are HOSTS -- an ssh-config alias exactly as list_hosts shows it, or
"local" for the box lasso runs on. A member written with a leading @ is another
GROUP, nested inside this one. Membership follows the host, not the credential,
so re-keying a host with mcp-client keeps its reach.

Two rules, and only two:

  * mutual inside a group -- every host in a group may see and message every
    other host in it, including hosts that arrive via a nested @group.
  * directed between groups -- "grant A B" lets A's hosts reach B's hosts and
    NOT the reverse. It does not chain: A->B plus B->C gives A nothing in C.

Nesting is mutual all the way up: a host in @inner nested inside outer is a peer
of everything in outer, not just of @inner. If that is not what you want, use a
grant instead -- and check the result with "reach", which prints every host a
given host can address and the rule that put it there.

Groups only ADD to what a self-scoped credential may address; they never take
anything away, and a fleet-scoped credential is unaffected. A member whose ssh
alias is later removed goes inert rather than erroring, the same way an
unaliased host is invisible everywhere else.

NOTE: this only bites when MCP_OAUTH is set. With it unset /mcp is open and
every caller is unidentified, so it keeps the fleet-wide view regardless.
`)
}

// splitMemberArg reads one member argument: "@name" is a subgroup reference,
// anything else is a host. The sigil is CLI syntax only — the db stores the
// distinction in the kind column, which is what lets a host and a group share a
// name without either shadowing the other.
func splitMemberArg(arg string) (member, kind string) {
	arg = strings.TrimSpace(arg)
	if strings.HasPrefix(arg, "@") {
		return strings.TrimSpace(strings.TrimPrefix(arg, "@")), memberKindGroup
	}
	return arg, memberKindHost
}

// renderMember spells a member back the way it was typed.
func renderMember(m groupMember) string {
	if m.Kind == memberKindGroup {
		return "@" + m.Member
	}
	return m.Member
}

func mcpGroupUsageError(cmd, msg string) {
	fmt.Fprintf(os.Stderr, "lasso mcp-group %s: %s\n\n", cmd, msg)
	printMCPGroupUsage(os.Stderr)
	os.Exit(2)
}

func mcpGroupFail(cmd string, err error) {
	fmt.Fprintf(os.Stderr, "lasso mcp-group %s: %v\n", cmd, err)
	os.Exit(1)
}

func mcpGroupAdd(args []string) {
	if len(args) != 1 {
		mcpGroupUsageError("add", "one group name is required")
	}
	created, err := createGroup(args[0])
	if err != nil {
		mcpGroupFail("add", err)
	}
	if !created {
		fmt.Printf("group %q already exists\n", strings.TrimSpace(args[0]))
		return
	}
	fmt.Printf("created group %q — add hosts with `lasso mcp-group add-member %s <host>`\n",
		strings.TrimSpace(args[0]), strings.TrimSpace(args[0]))
}

func mcpGroupRemove(args []string) {
	if len(args) != 1 {
		mcpGroupUsageError("rm", "one group name is required")
	}
	name := strings.TrimSpace(args[0])
	existed, err := deleteGroup(name)
	if err != nil {
		mcpGroupFail("rm", err)
	}
	if !existed {
		// The cascade still ran: a group with no row of its own can still be
		// referenced by members and grants, and leaving those behind would be a
		// group that keeps granting access after it is gone.
		fmt.Printf("no group %q — removed any leftover memberships and grants naming it\n", name)
		return
	}
	fmt.Printf("removed group %q, its members, its nesting inside other groups, and its grants\n", name)
}

func mcpGroupAddMember(args []string) {
	if len(args) < 2 {
		mcpGroupUsageError("add-member", "a group and at least one member are required")
	}
	group := strings.TrimSpace(args[0])
	if !groupExists(group) {
		mcpGroupFail("add-member", fmt.Errorf("no group %q — create it first with `lasso mcp-group add %s`", group, group))
	}
	for _, arg := range args[1:] {
		member, kind := splitMemberArg(arg)
		added, err := addGroupMember(group, member, kind)
		if err != nil {
			mcpGroupFail("add-member", err)
		}
		what := "host " + member
		if kind == memberKindGroup {
			what = "group @" + member
		}
		if !added {
			fmt.Printf("%s is already a member of %q\n", what, group)
			continue
		}
		fmt.Printf("added %s to %q\n", what, group)
		if kind == memberKindGroup && !groupExists(member) {
			fmt.Printf("  note: group %q does not exist yet — it contributes no hosts until it does\n", member)
		}
	}
	fmt.Printf("\nCheck the effect with `lasso mcp-group reach <host>` — nesting is mutual all the way up.\n")
}

func mcpGroupRemoveMember(args []string) {
	if len(args) != 2 {
		mcpGroupUsageError("rm-member", "a group and one member are required")
	}
	group := strings.TrimSpace(args[0])
	member, kind := splitMemberArg(args[1])
	removed, err := removeGroupMember(group, member, kind)
	if err != nil {
		mcpGroupFail("rm-member", err)
	}
	if !removed {
		fmt.Printf("%q is not a member of %q\n", strings.TrimSpace(args[1]), group)
		return
	}
	fmt.Printf("removed %q from %q\n", strings.TrimSpace(args[1]), group)
}

func mcpGroupGrant(args []string) {
	if len(args) != 2 {
		mcpGroupUsageError("grant", "a from-group and a to-group are required")
	}
	from, to := strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
	added, err := addGroupGrant(from, to)
	if err != nil {
		mcpGroupFail("grant", err)
	}
	for _, g := range []string{from, to} {
		if !groupExists(g) {
			fmt.Printf("note: group %q does not exist yet — the grant is inert until it does\n", g)
		}
	}
	if !added {
		fmt.Printf("%q already reaches %q\n", from, to)
		return
	}
	fmt.Printf("granted %q access to %q — one-way, and not transitive\n", from, to)
}

func mcpGroupRevoke(args []string) {
	if len(args) != 2 {
		mcpGroupUsageError("revoke", "a from-group and a to-group are required")
	}
	from, to := strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
	removed, err := removeGroupGrant(from, to)
	if err != nil {
		mcpGroupFail("revoke", err)
	}
	if !removed {
		fmt.Printf("%q did not have a grant to %q\n", from, to)
		return
	}
	fmt.Printf("revoked %q's access to %q (any grant the other way is untouched)\n", from, to)
}

// mcpGroupList renders the whole graph: each group with its members as a tree,
// then the directed grants, then anything referenced but never defined — a
// dangling reference is silently inert at resolution time, so this listing is
// the only place it can be noticed.
func mcpGroupList() {
	names, err := listGroupNames()
	if err != nil {
		mcpGroupFail("list", err)
	}
	members, err := loadGroupMembers()
	if err != nil {
		mcpGroupFail("list", err)
	}
	grants, err := loadGroupGrants()
	if err != nil {
		mcpGroupFail("list", err)
	}
	if len(names) == 0 && len(members) == 0 && len(grants) == 0 {
		fmt.Println("no MCP host groups defined")
		fmt.Println("every per-host credential reaches only its own host; see `lasso mcp-group add`")
		return
	}
	addressable := addressableHosts()
	for _, g := range names {
		fmt.Printf("%s\n", g)
		ms := members[g]
		if len(ms) == 0 {
			fmt.Printf("  (no members)\n")
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, m := range ms {
			note := ""
			switch {
			case m.Kind == memberKindGroup && !groupExists(m.Member):
				note = "no such group — inert"
			case m.Kind == memberKindHost && !addressable[m.Member]:
				note = "no ssh alias — inert"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", m.Kind, renderMember(m), note)
		}
		_ = tw.Flush()
	}
	if len(grants) > 0 {
		fmt.Printf("\ngrants (directed: the left group's hosts may reach the right group's hosts)\n")
		for _, gr := range grants {
			fmt.Printf("  %s -> %s\n", gr.From, gr.To)
		}
	}
	var dangling []string
	defined := map[string]bool{}
	for _, g := range names {
		defined[g] = true
	}
	for _, g := range sortedGroupNames(members, grants) {
		if !defined[g] {
			dangling = append(dangling, g)
		}
	}
	if len(dangling) > 0 {
		fmt.Printf("\nreferenced but not defined (inert): %s\n", strings.Join(dangling, ", "))
	}
}

// mcpGroupReach answers the question groups make hard: which hosts can THIS
// host's agents address, and why. Reach is only the addition — a credential
// always reaches its own host — and it is intersected with the ssh config at
// enforcement time, so a member that lasso cannot address is shown as inert
// rather than quietly dropped.
func mcpGroupReach(args []string) {
	if len(args) != 1 {
		mcpGroupUsageError("reach", "one host is required")
	}
	host, err := normalizeHostMember(args[0])
	if err != nil {
		mcpGroupFail("reach", err)
	}
	reasons, err := hostReachWhyFromDB(host)
	if err != nil {
		mcpGroupFail("reach", err)
	}
	if len(reasons) == 0 {
		fmt.Printf("host %q reaches only its own agents — it is in no group with another host\n", host)
		return
	}
	addressable := addressableHosts()
	fmt.Printf("host %q reaches %d other host(s), on top of its own:\n\n", host, len(reasons))
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tWHY\tNOTE")
	for _, r := range reasons {
		note := ""
		if !addressable[r.Host] {
			note = "no ssh alias — inert"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Host, r.Why, note)
	}
	_ = tw.Flush()
	if !addressable[host] {
		fmt.Printf("\nnote: %q itself has no ssh alias, so lasso cannot address it at all\n", host)
	}
	fmt.Printf("\nThis is the reach of a credential minted with `lasso mcp-client add --host %s`.\n", host)
	fmt.Print("A fleet-scoped credential ignores groups entirely and reaches every addressable host.\n")
}
