package main

import (
	"testing"
	"time"
)

// resetAgentReapMiss clears the cross-test miss streaks (the counter is a
// process global, and every test here starts from "never missed").
func resetAgentReapMiss(t *testing.T) {
	t.Helper()
	agentReapMiss.mu.Lock()
	agentReapMiss.n = map[string]int{}
	agentReapMiss.mu.Unlock()
}

// reapRec builds a recorded agent sitting in pane, boot-complete.
func reapRec(id, pane string) AgentRecord {
	ws, _, _ := cutPane(pane)
	return AgentRecord{
		ID: id, Title: id, Type: "git", Agent: "claude",
		WorkDir: "/w/" + id, WorkspaceID: ws, RootPane: pane,
		CreatedAt: time.Now(), BootStatus: BootReady,
	}
}

// cutPane splits "w1:p2" into its workspace and pane halves.
func cutPane(pane string) (ws, p string, ok bool) {
	for i := 0; i < len(pane); i++ {
		if pane[i] == ':' {
			return pane[:i], pane[i+1:], true
		}
	}
	return pane, "", false
}

func gp(host, pane string) hostPane {
	ws, _, _ := cutPane(pane)
	return hostPane{Host: host, PaneID: pane, WorkspaceID: ws}
}

// liveIDs is the set of agent ids listAgents still returns for a host.
func liveIDs(t *testing.T, host string) map[string]bool {
	t.Helper()
	recs, err := listAgents(host)
	if err != nil {
		t.Fatalf("listAgents(%q): %v", host, err)
	}
	out := map[string]bool{}
	for _, r := range recs {
		out[r.ID] = true
	}
	return out
}

// A record whose pane herdr no longer has is tombstoned — but only after
// agentReapMisses consecutive sightings of its absence, and the record with a
// live pane is never touched.
func TestReconcileClosesOnlyVanishedPanes(t *testing.T) {
	openTestDB(t)
	resetAgentReapMiss(t)
	if err := appendAgent("local", reapRec("live", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	if err := appendAgent("local", reapRec("gone", "w2:p1")); err != nil {
		t.Fatal(err)
	}
	panes := []hostPane{gp("local", "w1:p1")}

	// One miss is not enough — a single absence must not condemn.
	if n := reconcileHostAgents("local", panes); n != 0 {
		t.Fatalf("first pass closed %d, want 0 (needs %d consecutive misses)", n, agentReapMisses)
	}
	if ids := liveIDs(t, "local"); !ids["gone"] {
		t.Fatal("record was closed on a single miss")
	}
	if n := reconcileHostAgents("local", panes); n != 1 {
		t.Fatalf("second pass closed %d, want 1", n)
	}
	ids := liveIDs(t, "local")
	if ids["gone"] {
		t.Error("record whose pane is gone is still listed")
	}
	if !ids["live"] {
		t.Error("record whose pane is live was closed")
	}
	// Idempotent: a tombstone is out of the live set and never reconsidered.
	if n := reconcileHostAgents("local", panes); n != 0 {
		t.Errorf("third pass closed %d, want 0", n)
	}
}

// The streak is CONSECUTIVE: a pane that reappears between two misses resets it,
// so a record that flickers out of one listing is never condemned.
func TestReconcileMissStreakResetsOnSight(t *testing.T) {
	openTestDB(t)
	resetAgentReapMiss(t)
	if err := appendAgent("local", reapRec("flicker", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	with := []hostPane{gp("local", "w1:p1")}
	without := []hostPane{gp("local", "w9:p9")}

	reconcileHostAgents("local", without) // miss 1
	reconcileHostAgents("local", with)    // seen — streak resets
	if n := reconcileHostAgents("local", without); n != 0 {
		t.Fatalf("closed %d after a reset streak, want 0", n)
	}
	if !liveIDs(t, "local")["flicker"] {
		t.Error("a flickering pane got its record closed")
	}
}

// A failed herdr enumeration must never reap: callers pass no panes at all in
// that case, and an empty listing is treated as no evidence rather than as
// "nothing is running".
func TestReconcileIgnoresEmptyEnumeration(t *testing.T) {
	openTestDB(t)
	resetAgentReapMiss(t)
	if err := appendAgent("local", reapRec("a1", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < agentReapMisses+2; i++ {
		if n := reconcileHostAgents("local", nil); n != 0 {
			t.Fatalf("closed %d records on an empty enumeration, want 0", n)
		}
	}
	if !liveIDs(t, "local")["a1"] {
		t.Error("an empty pane enumeration closed a record")
	}
}

// An agent mid-boot is legitimately pane-less for a moment; reaping on "no pane
// yet" would kill agents during create.
func TestReconcileSparesBootingAgents(t *testing.T) {
	openTestDB(t)
	resetAgentReapMiss(t)
	for _, st := range []string{BootCreating, BootBooting} {
		rec := reapRec("boot-"+st, "w7:p1")
		rec.BootStatus = st
		if err := appendAgent("local", rec); err != nil {
			t.Fatal(err)
		}
	}
	// Same, but past the grace — one of these (three days at "booting") was in
	// titan's 139, stranded by a lasso that died mid-boot.
	for _, st := range []string{BootCreating, BootBooting} {
		rec := reapRec("stale-"+st, "w7:p2")
		rec.BootStatus = st
		rec.CreatedAt = time.Now().Add(-agentBootGrace - time.Minute)
		if err := appendAgent("local", rec); err != nil {
			t.Fatal(err)
		}
	}
	// A record that never got a pane at all has nothing to falsify either.
	noPane := reapRec("nopane", "")
	noPane.WorkspaceID = ""
	if err := appendAgent("local", noPane); err != nil {
		t.Fatal(err)
	}
	panes := []hostPane{gp("local", "w1:p1")}
	for i := 0; i < agentReapMisses+2; i++ {
		reconcileHostAgents("local", panes)
	}
	ids := liveIDs(t, "local")
	for _, id := range []string{"boot-" + BootCreating, "boot-" + BootBooting, "nopane"} {
		if !ids[id] {
			t.Errorf("record %q was reaped while mid-boot / pane-less", id)
		}
	}
	for _, id := range []string{"stale-" + BootCreating, "stale-" + BootBooting} {
		if ids[id] {
			t.Errorf("record %q is stranded past the boot grace and was not reaped", id)
		}
	}
}

// Ids and pane ids are unique only per host, so one host's herdr state must
// never condemn another host's records — even when the pane ids collide.
func TestReconcileIsPerHost(t *testing.T) {
	openTestDB(t)
	resetAgentReapMiss(t)
	if err := appendAgent("local", reapRec("l1", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	if err := appendAgent("citadel", reapRec("r1", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	// local's herdr has no w1:p1 — but citadel's record names the same pane id.
	panes := []hostPane{gp("local", "w5:p1")}
	for i := 0; i < agentReapMisses+1; i++ {
		reconcileHostAgents("local", panes)
	}
	if liveIDs(t, "local")["l1"] {
		t.Error("local record with a vanished pane was not closed")
	}
	if !liveIDs(t, "citadel")["r1"] {
		t.Error("citadel's record was condemned by local's herdr state")
	}
}

// A tombstone stays in the history view and can be reopened, which revives it.
// This is the reason records are marked rather than deleted.
func TestTombstoneKeptForHistoryAndReopen(t *testing.T) {
	openTestDB(t)
	resetAgentReapMiss(t)
	if err := appendAgent("local", reapRec("hist", "w2:p1")); err != nil {
		t.Fatal(err)
	}
	panes := []hostPane{gp("local", "w1:p1")}
	for i := 0; i < agentReapMisses; i++ {
		reconcileHostAgents("local", panes)
	}
	if liveIDs(t, "local")["hist"] {
		t.Fatal("record was not closed")
	}

	all, err := listAllAgentsIncludingClosed()
	if err != nil {
		t.Fatal(err)
	}
	var found *AgentRecord
	for i := range all {
		if all[i].Agent.ID == "hist" {
			found = &all[i].Agent
		}
	}
	if found == nil {
		t.Fatal("tombstoned record vanished from the history listing")
	}
	if found.ClosedAt == "" {
		t.Error("history record has no closed_at stamp")
	}
	if found.WorkDir != "/w/hist" {
		t.Errorf("history record lost its work dir: %q", found.WorkDir)
	}
	// The live cross-host listing, by contrast, must not offer it as a target.
	live, err := listAllAgents()
	if err != nil {
		t.Fatal(err)
	}
	for _, ha := range live {
		if ha.Agent.ID == "hist" {
			t.Error("tombstone is still offered by listAllAgents")
		}
	}
	// Reopen resolves it and re-points it at a fresh pane, which revives it.
	if _, err := findAgentRecordAny("local", "hist"); err != nil {
		t.Fatalf("findAgentRecordAny on a tombstone: %v", err)
	}
	if err := updateAgentPane("hist", "local", "w8", "w8:p1"); err != nil {
		t.Fatal(err)
	}
	if !liveIDs(t, "local")["hist"] {
		t.Error("reopening a closed agent did not revive its record")
	}
}
