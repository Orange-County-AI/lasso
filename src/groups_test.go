package main

import (
	"sort"
	"strings"
	"testing"
)

// graph builds a member map from the CLI's own notation, so a test reads the
// way the command that produced the rows would have been typed: a plain entry
// is a host member, an @entry is a nested group.
func graph(spec map[string][]string) map[string][]groupMember {
	out := map[string][]groupMember{}
	for g, ms := range spec {
		for _, m := range ms {
			member, kind := splitMemberArg(m)
			out[g] = append(out[g], groupMember{Member: member, Kind: kind})
		}
	}
	return out
}

// setOf renders a host set deterministically for failure messages and compares.
func setOf(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func sameHosts(got map[string]bool, want ...string) bool {
	sort.Strings(want)
	g := setOf(got)
	if len(g) != len(want) {
		return false
	}
	for i := range g {
		if g[i] != want[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// closure
// ---------------------------------------------------------------------------

// Nesting is transitive for MEMBERSHIP (that is what makes it mutual all the
// way up), and only a kind='group' row expands — a host that happens to share a
// name with a group stays one host.
func TestGroupClosureNestsAndDoesNotExpandHosts(t *testing.T) {
	members := graph(map[string][]string{
		"outer": {"titan", "@inner"},
		"inner": {"norm", "norm-darren"},
	})
	if got := groupClosure(members, "outer"); !sameHosts(got, "titan", "norm", "norm-darren") {
		t.Errorf("closure(outer) = %v, want titan+norm+norm-darren", setOf(got))
	}
	if got := groupClosure(members, "inner"); !sameHosts(got, "norm", "norm-darren") {
		t.Errorf("closure(inner) = %v, want norm+norm-darren", setOf(got))
	}
	// Name collision: "inner" as a HOST member is a machine called inner, not the
	// group. Nothing expands, and the group's hosts stay out of the result.
	collide := graph(map[string][]string{
		"g":     {"inner"},
		"inner": {"norm"},
	})
	if got := groupClosure(collide, "g"); !sameHosts(got, "inner") {
		t.Errorf("closure(g) = %v, want just the host named inner", setOf(got))
	}
	// Exact matching, no NOCASE anywhere: Norm and norm are two hosts.
	cased := graph(map[string][]string{"g": {"norm", "Norm"}})
	if got := groupClosure(cased, "g"); !sameHosts(got, "norm", "Norm") {
		t.Errorf("closure(g) = %v, want both spellings kept distinct", setOf(got))
	}
}

// A cycle written by hand (or by an older lasso) must terminate, and a group
// nobody defined must contribute nothing rather than erroring — the same
// philosophy as an ssh alias removed after a host joined a group.
func TestGroupClosureSurvivesCyclesAndDanglingRefs(t *testing.T) {
	cyclic := graph(map[string][]string{
		"a": {"host-a", "@b"},
		"b": {"host-b", "@a"},
	})
	if got := groupClosure(cyclic, "a"); !sameHosts(got, "host-a", "host-b") {
		t.Errorf("closure(a) = %v, want host-a+host-b (and no infinite walk)", setOf(got))
	}
	// Self-reference is the degenerate cycle.
	selfRef := graph(map[string][]string{"a": {"host-a", "@a"}})
	if got := groupClosure(selfRef, "a"); !sameHosts(got, "host-a") {
		t.Errorf("closure(a) = %v, want host-a", setOf(got))
	}
	dangling := graph(map[string][]string{"a": {"host-a", "@ghost"}})
	if got := groupClosure(dangling, "a"); !sameHosts(got, "host-a") {
		t.Errorf("closure(a) = %v, want the dangling @ghost to be inert", setOf(got))
	}
	if got := groupClosure(dangling, "nobody"); len(got) != 0 {
		t.Errorf("closure of an undefined group = %v, want empty", setOf(got))
	}
}

// ---------------------------------------------------------------------------
// reach
// ---------------------------------------------------------------------------

// Membership is mutual, and reach is only ever the ADDITION: a host never
// appears in its own reach, because its credential already covers its own host.
func TestReachIsMutualInsideAGroup(t *testing.T) {
	members := graph(map[string][]string{"norm-stack": {"norm", "norm-darren"}})
	if got := hostReach(members, nil, "norm"); !sameHosts(got, "norm-darren") {
		t.Errorf("reach(norm) = %v, want norm-darren", setOf(got))
	}
	if got := hostReach(members, nil, "norm-darren"); !sameHosts(got, "norm") {
		t.Errorf("reach(norm-darren) = %v, want norm", setOf(got))
	}
	// A host in no group reaches nothing extra.
	if got := hostReach(members, nil, "titan"); len(got) != 0 {
		t.Errorf("reach(titan) = %v, want empty", setOf(got))
	}
	// Nesting is mutual all the way up: a host in @inner is a peer of everything
	// in outer, which is the consequence the help text warns about.
	nested := graph(map[string][]string{
		"outer": {"titan", "@inner"},
		"inner": {"norm", "norm-darren"},
	})
	if got := hostReach(nested, nil, "norm"); !sameHosts(got, "titan", "norm-darren") {
		t.Errorf("reach(norm) = %v, want titan+norm-darren", setOf(got))
	}
	// And the reason says which group did it, since that is the only way an
	// operator can undo a reach they did not intend.
	reasons := hostReachWhy(nested, nil, "norm")
	for _, r := range reasons {
		if !strings.Contains(r.Why, "outer") && !strings.Contains(r.Why, "inner") {
			t.Errorf("reach(norm) entry %+v does not name the group responsible", r)
		}
	}
}

// A grant is one-way and one hop. Both halves matter: the reverse must be
// refused, and A→B, B→C must give A nothing in C — otherwise a group's reach
// would depend on edges its operator never looked at.
func TestReachGrantsAreDirectedAndNotTransitive(t *testing.T) {
	members := graph(map[string][]string{
		"a": {"host-a"},
		"b": {"host-b"},
		"c": {"host-c"},
	})
	grants := []groupGrant{{From: "a", To: "b"}, {From: "b", To: "c"}}

	if got := hostReach(members, grants, "host-a"); !sameHosts(got, "host-b") {
		t.Errorf("reach(host-a) = %v, want host-b only — A→B→C must not chain", setOf(got))
	}
	if got := hostReach(members, grants, "host-b"); !sameHosts(got, "host-c") {
		t.Errorf("reach(host-b) = %v, want host-c only — the grant from a is one-way", setOf(got))
	}
	if got := hostReach(members, grants, "host-c"); len(got) != 0 {
		t.Errorf("reach(host-c) = %v, want empty — c grants nobody anything", setOf(got))
	}
	// The explanation names the grant, not a group membership.
	reasons := hostReachWhy(members, grants, "host-a")
	if len(reasons) != 1 || !strings.Contains(reasons[0].Why, "granted") {
		t.Errorf("reach(host-a) reasons = %+v, want one grant explanation", reasons)
	}
}

// "local" is a member like any other, in both directions — the lasso host is
// just another machine as far as groups are concerned.
func TestReachHandlesLocalBothDirections(t *testing.T) {
	members := graph(map[string][]string{"ops": {"local", "norm"}})
	if got := hostReach(members, nil, "local"); !sameHosts(got, "norm") {
		t.Errorf("reach(local) = %v, want norm", setOf(got))
	}
	if got := hostReach(members, nil, "norm"); !sameHosts(got, "local") {
		t.Errorf("reach(norm) = %v, want local", setOf(got))
	}
	// An empty host is nobody: it must not silently resolve as the local box, or
	// a credential with no host would inherit local's groups.
	if got := hostReach(members, nil, ""); len(got) != 0 {
		t.Errorf("reach(\"\") = %v, want empty", setOf(got))
	}
}

// ---------------------------------------------------------------------------
// the stored graph
// ---------------------------------------------------------------------------

// groupFixture writes one nested group and a directed grant through the same
// helpers the CLI uses.
func groupFixture(t *testing.T) {
	t.Helper()
	openTestDB(t)
	stubSSHHosts(t, "norm", "norm-darren", "outsider")
	for _, g := range []string{"norm-stack", "inner", "ops"} {
		if _, err := createGroup(g); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []struct{ group, member, kind string }{
		{"norm-stack", "norm", memberKindHost},
		{"norm-stack", "inner", memberKindGroup},
		{"inner", "norm-darren", memberKindHost},
		{"ops", "local", memberKindHost},
	} {
		if _, err := addGroupMember(m.group, m.member, m.kind); err != nil {
			t.Fatalf("addGroupMember(%+v): %v", m, err)
		}
	}
	if _, err := addGroupGrant("ops", "norm-stack"); err != nil {
		t.Fatal(err)
	}
}

func TestStoredGraphResolvesAndCascades(t *testing.T) {
	groupFixture(t)

	if got := hostReachFromDB("norm"); !sameHosts(got, "norm-darren") {
		t.Errorf("reach(norm) = %v, want norm-darren through the nested group", setOf(got))
	}
	// The grant is one-way: local reaches the norm hosts, they do not reach local.
	if got := hostReachFromDB("local"); !sameHosts(got, "norm", "norm-darren") {
		t.Errorf("reach(local) = %v, want both norm hosts via the grant", setOf(got))
	}
	if got := hostReachFromDB("norm-darren"); !sameHosts(got, "norm") {
		t.Errorf("reach(norm-darren) = %v, want norm only — the grant does not run backwards", setOf(got))
	}

	// rm cascades in one transaction: the group row, its members, its nesting
	// inside another group, and its grants in both directions. A group that kept
	// granting access after being removed is the failure this prevents.
	if existed, err := deleteGroup("inner"); err != nil || !existed {
		t.Fatalf("deleteGroup(inner) = (%v, %v), want (true, nil)", existed, err)
	}
	if got := hostReachFromDB("norm"); len(got) != 0 {
		t.Errorf("reach(norm) = %v, want empty once inner is gone", setOf(got))
	}
	if existed, err := deleteGroup("ops"); err != nil || !existed {
		t.Fatalf("deleteGroup(ops) = (%v, %v), want (true, nil)", existed, err)
	}
	grants, err := loadGroupGrants()
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Errorf("grants = %+v, want none after the granting group was removed", grants)
	}
	if got := hostReachFromDB("local"); len(got) != 0 {
		t.Errorf("reach(local) = %v, want empty once its group is gone", setOf(got))
	}
}

// Writes normalize and validate; reads stay permissive. A typo'd alias must
// fail while the operator is looking at it, an empty member must never be
// stored (it would match no host on any lookup), and the local box has exactly
// one spelling.
func TestGroupWritesValidateMembers(t *testing.T) {
	openTestDB(t)
	stubSSHHosts(t, "norm")
	if _, err := createGroup("g"); err != nil {
		t.Fatal(err)
	}
	if _, err := createGroup("  "); err == nil {
		t.Error("an empty group name was accepted")
	}
	if _, err := addGroupMember("g", "  ", memberKindHost); err == nil {
		t.Error("an empty host member was accepted — it would match nothing forever")
	}
	if _, err := addGroupMember("g", "citadel", memberKindHost); err == nil {
		t.Error("a host with no ssh alias was accepted")
	}
	if _, err := addGroupMember("g", "g", memberKindGroup); err == nil {
		t.Error("a group was allowed to contain itself")
	}
	if _, err := addGroupGrant("g", "g"); err == nil {
		t.Error("a group was granted access to itself")
	}
	// "local" is stored as the literal "local", whichever way it was spelled.
	if _, err := addGroupMember("g", " local ", memberKindHost); err != nil {
		t.Fatal(err)
	}
	members, err := loadGroupMembers()
	if err != nil {
		t.Fatal(err)
	}
	if len(members["g"]) != 1 || members["g"][0].Member != "local" {
		t.Errorf("members = %+v, want exactly one member stored as \"local\"", members["g"])
	}
	// Adding the same member twice is idempotent, not an error.
	if added, err := addGroupMember("g", "local", memberKindHost); err != nil || added {
		t.Errorf("re-adding a member = (%v, %v), want (false, nil)", added, err)
	}
	// A subgroup that does not exist yet is accepted on purpose, so a hierarchy
	// can be written in any order; it contributes no hosts until it does.
	if added, err := addGroupMember("g", "later", memberKindGroup); err != nil || !added {
		t.Fatalf("forward reference to a subgroup = (%v, %v), want (true, nil)", added, err)
	}
	if got := hostReachFromDB("local"); len(got) != 0 {
		t.Errorf("reach(local) = %v, want empty — the forward reference is inert", setOf(got))
	}
}

// The reach explanation is what an operator reads before trusting a group, so
// it has to survive the round trip through the db intact.
func TestReachWhyFromDBNamesTheRule(t *testing.T) {
	groupFixture(t)
	reasons, err := hostReachWhyFromDB("local")
	if err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 2 {
		t.Fatalf("reasons = %+v, want two hosts", reasons)
	}
	for _, r := range reasons {
		if !strings.Contains(r.Why, "ops") || !strings.Contains(r.Why, "norm-stack") {
			t.Errorf("reason %+v should name the grant that produced it", r)
		}
	}
}
