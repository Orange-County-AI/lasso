package main

// Reconciliation of lasso's agent records against herdr's live panes.
//
// The agents table is an append-only log; herdr's panes are not. Left alone the
// two diverge in one direction only — every agent whose pane is closed leaves a
// record that still reads as a live, "ready" agent — and the gap only ever
// grows. It is not cosmetic: such a record is listed, cannot be messaged, and
// cannot be closed (pane.close answers pane_not_found, worktree.remove answers
// workspace_not_found), so a caller cannot tell a live agent from a tombstone
// and has no way to clear one.
//
// So reconciliation lives here, at the seam where lasso already learns herdr's
// truth for a host: every place that enumerates a host's panes hands the result
// to reconcileHostAgents, which stamps closed_at on the records those panes
// contradict. It is a write, not a read-time filter, and it is idempotent — a
// tombstoned record is out of the live queries and never reconsidered, so the
// store stops growing in the dimension that mattered.
//
// The whole design problem is that "no pane" has two causes — the agent is gone,
// or herdr could not be asked properly — and only the first may condemn a
// record. Hence the rules below, each of which exists because getting it wrong
// deletes a live agent:
//
//  1. Only a SUCCESSFUL enumeration reconciles. A herdr that is restarting, a
//     host briefly unreachable, an SSH master caught mid-heal — none of those are
//     evidence about any agent. Callers pass panes only when their enumeration
//     returned no error; list_agents already reports the failure as a partial
//     listing instead.
//  2. Per host, always. Ids, pane ids and workspace ids are unique only within a
//     host, so one host's herdr state says nothing about another's records. The
//     UPDATE is scoped by host as well as id.
//  3. An empty pane list never condemns. A host lasso is talking to has panes by
//     construction (herdr is what lasso drives); zero is far likelier to be
//     protocol drift than a truthful "nothing is running".
//  4. Agents mid-create are exempt — for agentBootGrace. A record at
//     BootCreating/BootBooting is legitimately pane-less, or has a pane herdr is
//     still materializing, so reaping on "no pane yet" would kill agents during
//     create. But the exemption has to expire: boot_status only advances while
//     the lasso that started the boot is alive, so a process that dies mid-boot
//     strands the record at "booting" forever — one such record, three days old,
//     was in titan's 139. Past the grace it is not a booting agent, it is the
//     wreck of one, and the ordinary rules apply.
//  5. A record with no root_pane is exempt — there is no claim to falsify.
//  6. A pane must be missing from agentReapMisses CONSECUTIVE successful
//     enumerations of its host. One miss is already good evidence; requiring two
//     costs a poll interval and covers the one racy window left (reopen
//     re-points a record at a workspace herdr has only just created).
//
// Foreign herdr sessions — panes lasso did not create, like a long-lived bot in
// its own pane — are untouched by all of this: they have no record to tombstone,
// they are derived from the pane listing itself on every call, and so they
// appear and disappear with the panes they are.

import (
	"log"
	"sync"
	"time"
)

// agentReapMisses is how many consecutive successful enumerations of a host must
// fail to show a record's pane before the record is tombstoned. See rule 6.
const agentReapMisses = 2

// agentBootGrace is how long a record may sit at creating/booting before it
// stops being exempt from reconciliation (rule 4). Generous next to a real boot,
// which takes seconds — and which in any case holds a pane herdr can see for all
// but the first instant of it, so a slow boot is protected by having a pane, not
// by this. What the grace is really sized against is a lasso restart landing
// mid-boot: long enough that no live create is caught, short enough that the
// stranded record it leaves behind does not become permanent.
const agentBootGrace = 10 * time.Minute

// agentReapMiss counts those consecutive misses per host|id. In memory on
// purpose: the count is evidence about the current process's view of herdr, and
// a restarted lasso should start counting again rather than act on what a
// previous one thought it saw.
var agentReapMiss = struct {
	mu sync.Mutex
	n  map[string]int
}{n: map[string]int{}}

func agentReapKey(host, id string) string { return host + "|" + id }

// agentReapSeen resets a record's miss streak — its pane is there.
func agentReapSeen(host, id string) {
	agentReapMiss.mu.Lock()
	delete(agentReapMiss.n, agentReapKey(host, id))
	agentReapMiss.mu.Unlock()
}

// agentReapMissed records one miss and reports whether the record has now missed
// enough consecutive enumerations to be condemned. The counter is dropped at the
// threshold: the record is about to leave the live set, so nothing will ask again.
func agentReapMissed(host, id string) bool {
	agentReapMiss.mu.Lock()
	defer agentReapMiss.mu.Unlock()
	k := agentReapKey(host, id)
	n := agentReapMiss.n[k] + 1
	if n >= agentReapMisses {
		delete(agentReapMiss.n, k)
		return true
	}
	agentReapMiss.n[k] = n
	return false
}

// reconcileHostAgents tombstones the records on host whose herdr pane is gone,
// given a SUCCESSFUL enumeration of that host's panes (see rule 1 — never call
// this with the panes from a failed or partial listing). Returns how many records
// it closed. Best effort throughout: this runs on the grid poll and on
// list_agents, so a db hiccup is logged and retried on the next pass rather than
// failing the listing that triggered it.
func reconcileHostAgents(host string, panes []gridPane) int {
	if db == nil || host == "" || len(panes) == 0 { // rule 3
		return 0
	}
	live := make(map[string]bool, len(panes))
	for _, gp := range panes {
		if gp.PaneID != "" {
			live[gp.PaneID] = true
		}
	}
	recs, err := listAgents(host) // live records only — tombstones are already done
	if err != nil {
		log.Printf("agents:   reconcile %s: %v", host, err)
		return 0
	}
	closed := 0
	for _, rec := range recs {
		if rec.RootPane == "" { // rule 5
			continue
		}
		if rec.BootStatus == BootCreating || rec.BootStatus == BootBooting { // rule 4
			// A zero CreatedAt (an unparseable legacy timestamp) reads as
			// long-expired, which is the safe direction: such a record predates
			// this process by definition, so it cannot be a boot in flight.
			if time.Since(rec.CreatedAt) < agentBootGrace {
				continue
			}
		}
		if live[rec.RootPane] {
			agentReapSeen(host, rec.ID)
			continue
		}
		if !agentReapMissed(host, rec.ID) { // rule 6
			continue
		}
		if err := markAgentClosed(rec.ID, host); err != nil {
			log.Printf("agents:   reconcile %s: close %s: %v", host, rec.ID, err)
			continue
		}
		closed++
	}
	if closed > 0 {
		log.Printf("agents:   reconciled %s: closed %d record(s) whose herdr pane is gone", host, closed)
	}
	return closed
}
