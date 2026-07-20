package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
)

// /api/agent/close — the soft-close behind `lasso closeme` (and the UI's agent
// close). The caller identifies the agent by pane_id (its own $HERDR_PANE_ID)
// or agent_id, plus an optional host. Everything here is host-aware: pane and
// agent ids are only unique per host, so resolution never guesses across hosts
// — an ambiguous id without a host fails loudly instead of closing the wrong
// agent — and the teardown always runs on the backend of the host the resolved
// record lives on (closeAgentRecord enforces the match).

// closeBackendResolver resolves a host name to the Backend the close path
// drives. A package var so tests can substitute fake hosts without a live
// herdr or ssh fleet.
var closeBackendResolver = resolveBackend

// serveAgentClose soft-closes a single agent: it kills the agent process and
// closes its pane (the same teardown the close_agent MCP tool performs), so a
// lasso-spawned agent can shut *itself* down with `lasso closeme` — no MCP
// round-trip and nothing to pass but the $HERDR_PANE_ID the pane already
// exports. The caller identifies the agent by pane_id or agent_id, with an
// optional host ("local" or an ssh-config alias) to pin which host's records
// to resolve against. remove_worktree (git only) also discards the worktree.
func serveAgentClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		PaneID         string `json:"pane_id"`
		AgentID        string `json:"agent_id"`
		Host           string `json:"host"`
		RemoveWorktree bool   `json:"remove_worktree"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	agentID := strings.TrimSpace(req.AgentID)
	paneID := strings.TrimSpace(req.PaneID)
	host := strings.TrimSpace(req.Host)
	if agentID == "" && paneID == "" {
		http.Error(w, "pane_id or agent_id required", http.StatusBadRequest)
		return
	}
	rec, status, err := resolveCloseTarget(r.Context(), host, agentID, paneID)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	// Close through the backend of the host the record lives on — never the
	// request's active host, and never blindly "local".
	b, err := closeBackendResolver(rec.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out, err := closeAgentRecord(b, rec, true, req.RemoveWorktree)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, out)
}

// resolveCloseTarget maps a close request to the one agent record it may act
// on. With a host, resolution is scoped to that host's records. Without one,
// every host in this lasso's own db is searched; a unique match wins, and a
// pane/agent id that matches records on several hosts is refused (the caller
// must pass host) rather than guessed at. A pane no host's records claim may
// still belong to a peer lasso that created the agent on this machine — the
// adoption path (adoptPeerAgent) covers that before giving up. The returned
// int is the HTTP status for a non-nil error.
func resolveCloseTarget(ctx context.Context, host, agentID, paneID string) (AgentRecord, int, error) {
	if agentID != "" {
		if host != "" {
			rec, err := findAgentRecord(host, agentID)
			if err != nil {
				return AgentRecord{}, http.StatusNotFound, err
			}
			return rec, 0, nil
		}
		all, err := listAllAgents()
		if err != nil {
			return AgentRecord{}, http.StatusInternalServerError, err
		}
		var matches []AgentRecord
		for _, ha := range all {
			if ha.Agent.ID == agentID {
				matches = append(matches, ha.Agent)
			}
		}
		switch len(matches) {
		case 0:
			return AgentRecord{}, http.StatusNotFound, fmt.Errorf("no agent %q on any host", agentID)
		case 1:
			return matches[0], 0, nil
		default:
			return AgentRecord{}, http.StatusConflict,
				fmt.Errorf("agent id %q exists on hosts %s — pass \"host\" to disambiguate", agentID, hostsOf(matches))
		}
	}

	// Pane path. With an explicit host, resolve the pane on that host only —
	// the caller has vouched for where the pane lives.
	if host != "" {
		b, err := closeBackendResolver(host)
		if err != nil {
			return AgentRecord{}, http.StatusBadGateway, err
		}
		recs, err := listAgents(host)
		if err != nil {
			return AgentRecord{}, http.StatusInternalServerError, err
		}
		who := resolveWhoami(b, host, recs, paneID)
		if who.Found {
			rec, err := findAgentRecord(host, who.Agent.ID)
			if err != nil {
				return AgentRecord{}, http.StatusNotFound, err
			}
			return rec, 0, nil
		}
		// A local pane none of our own records claim may have been spawned here
		// by a peer lasso (closeme's case: the pane is by definition on the
		// machine it POSTed to, but the record lives with whoever created it).
		if isLocalHost(host) {
			if rec, ok, aerr := adoptPeerAgent(ctx, b, paneID); aerr != nil {
				return AgentRecord{}, http.StatusConflict, aerr
			} else if ok {
				return rec, 0, nil
			}
		}
		return AgentRecord{}, http.StatusNotFound, fmt.Errorf("%s", who.Detail)
	}

	// No host given: search every host's records in our own db.
	matches, err := paneMatchesAcrossHosts(paneID)
	if err != nil {
		return AgentRecord{}, http.StatusInternalServerError, err
	}
	switch len(matches) {
	case 1:
		return matches[0], 0, nil
	case 0:
		if b, err := closeBackendResolver("local"); err == nil {
			if rec, ok, aerr := adoptPeerAgent(ctx, b, paneID); aerr != nil {
				return AgentRecord{}, http.StatusConflict, aerr
			} else if ok {
				return rec, 0, nil
			}
		}
		return AgentRecord{}, http.StatusNotFound,
			fmt.Errorf("pane %q does not map to any lasso agent on any host this lasso knows — you may be in a pane lasso did not create, or the owning lasso is unreachable", paneID)
	default:
		return AgentRecord{}, http.StatusConflict,
			fmt.Errorf("pane id %q matches agents on hosts %s — pane ids are only unique per host; pass \"host\" to disambiguate", paneID, hostsOf(matches))
	}
}

// paneMatchesAcrossHosts finds, per host in this lasso's own db, the newest
// agent record whose root pane corresponds to paneID. The raw id is matched
// as-is on every host; canonicalization through herdr (raw $HERDR_PANE_ID form
// → public root_pane form) is done only against the LOCAL herdr — a raw pane id
// is an artifact of the machine the caller runs on, and "resolving" it through
// some other host's herdr would name an unrelated pane that happens to share
// the id (the exact collision this file exists to prevent).
func paneMatchesAcrossHosts(paneID string) ([]AgentRecord, error) {
	all, err := listAllAgents()
	if err != nil {
		return nil, err
	}
	byHost := map[string][]AgentRecord{}
	for _, ha := range all {
		byHost[ha.Host] = append(byHost[ha.Host], ha.Agent)
	}
	var matches []AgentRecord
	for host, recs := range byHost {
		forms := map[string]bool{paneID: true}
		if isLocalHost(host) {
			if b, err := closeBackendResolver(host); err == nil {
				if info, ok := paneGet(b, paneID); ok {
					forms[info.PaneID] = true
				}
			}
		}
		var match *AgentRecord
		for i := range recs {
			if recs[i].RootPane != "" && forms[recs[i].RootPane] {
				match = &recs[i] // recs are oldest-first; keep the newest match
			}
		}
		if match != nil {
			matches = append(matches, *match)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Host < matches[j].Host })
	return matches, nil
}

// hostsOf renders the distinct hosts of a set of records for error messages.
func hostsOf(recs []AgentRecord) string {
	seen := map[string]bool{}
	var hosts []string
	for _, r := range recs {
		if !seen[r.Host] {
			seen[r.Host] = true
			hosts = append(hosts, r.Host)
		}
	}
	sort.Strings(hosts)
	return strings.Join(hosts, ", ")
}

// ---------------------------------------------------------------------------
// peer adoption — close a local pane whose record lives on another lasso
// ---------------------------------------------------------------------------

// Two machines can each run a lasso, and either one can spawn agents on the
// other over ssh — the record then lives in the CREATING lasso's db (keyed by
// its alias for the target host), while the pane, worktree, and `lasso
// closeme` all live on the target machine. closeme POSTs to its own machine's
// lasso (loopback), which has no record of the pane. Rather than teach the
// agent about a second machine, this lasso adopts the close: it asks each
// reachable peer's own lasso.db (sqlite3 over the ssh control master — the
// same channel hostconfig uses) whether that peer created an agent on another
// machine with this root pane, verifies the claim against local reality, and
// then runs the ordinary teardown against the LOCAL backend — which is where
// the pane actually is.

// peerHostsFn and peerAgentQueryFn are the adoption path's two external
// touchpoints (ssh host discovery + remote sqlite), package vars so tests can
// fake a fleet.
var (
	peerHostsFn      = reachablePeerHosts
	peerAgentQueryFn = queryPeerAgents
)

// reachablePeerHosts lists the ssh-config hosts a peer lasso could be asked
// on: probed reachable with a running, protocol-compatible herdr — the same
// bar gridHostBackend (and thus remoteDB) requires.
func reachablePeerHosts(ctx context.Context) []string {
	var out []string
	for _, h := range discoverHosts(ctx, false) {
		if h.Reachable && h.Running && h.Compatible {
			out = append(out, h.Alias)
		}
	}
	return out
}

// queryPeerAgents asks one peer's own lasso.db for records of agents the peer
// created on some OTHER machine (host<>'local') with the given root pane.
// The peer's own local agents are excluded deliberately: pane ids are only
// unique per machine, so the peer's local pane with the same id is a different
// pane entirely — matching it is how an unrelated agent gets killed.
func queryPeerAgents(peer, rootPane string) ([]AgentRecord, error) {
	raw, err := remoteDB(peer, true, fmt.Sprintf(
		`SELECT id, title, type, repo, base_branch, branch, agent, workspace_id, root_pane, work_dir `+
			`FROM agents WHERE root_pane=%s AND host<>'local' ORDER BY created_at;`, sqlQuote(rootPane)))
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Type        string `json:"type"`
		Repo        string `json:"repo"`
		BaseBranch  string `json:"base_branch"`
		Branch      string `json:"branch"`
		Agent       string `json:"agent"`
		WorkspaceID string `json:"workspace_id"`
		RootPane    string `json:"root_pane"`
		WorkDir     string `json:"work_dir"`
	}
	if err := unmarshalRows(raw, &rows); err != nil {
		return nil, fmt.Errorf("read agents on %s: %w", peer, err)
	}
	out := make([]AgentRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, AgentRecord{
			ID: r.ID, Title: r.Title, Type: r.Type, Repo: r.Repo, BaseBranch: r.BaseBranch,
			Branch: r.Branch, Agent: r.Agent, WorkspaceID: r.WorkspaceID,
			RootPane: r.RootPane, WorkDir: r.WorkDir,
		})
	}
	return out, nil
}

// adoptPeerAgent resolves a pane on THIS machine that no local record claims
// by asking peer lassos for the agent they created here. A candidate must
// clear three bars: its root pane (canonicalized by the LOCAL herdr, which is
// authoritative for local panes) matches the peer's recorded root_pane; the
// peer recorded it as a non-local agent (see queryPeerAgents); and its work
// dir actually exists on this machine. Exactly one surviving candidate is
// adopted — its Host becomes "local" so the teardown runs on the local
// backend, where the pane is. Multiple claimants is an error, never a guess.
// ok is false when there is simply nothing to adopt (not-a-pane, no peers, no
// claims), letting the caller fall through to its own not-found handling.
func adoptPeerAgent(ctx context.Context, localB Backend, paneID string) (AgentRecord, bool, error) {
	info, ok := paneGet(localB, paneID)
	if !ok {
		return AgentRecord{}, false, nil // not a live local pane — nothing to adopt
	}
	canonical := info.PaneID
	type candidate struct {
		peer string
		rec  AgentRecord
	}
	var cands []candidate
	for _, peer := range peerHostsFn(ctx) {
		rows, err := peerAgentQueryFn(peer, canonical)
		if err != nil {
			log.Printf("closeme: asking peer %s about pane %s: %v", peer, canonical, err)
			continue // best effort: an unreachable peer just can't claim the pane
		}
		var best *AgentRecord
		for i := range rows {
			if rows[i].RootPane != canonical || rows[i].WorkDir == "" {
				continue
			}
			if _, err := localB.Stat(rows[i].WorkDir); err != nil {
				continue // the claimed work dir isn't on this machine — a record for some other host
			}
			best = &rows[i] // rows are oldest-first; keep the newest claim
		}
		if best != nil {
			cands = append(cands, candidate{peer, *best})
		}
	}
	switch len(cands) {
	case 0:
		return AgentRecord{}, false, nil
	case 1:
		rec := cands[0].rec
		rec.Host = "local" // the pane and worktree are on this machine; the peer only holds the record
		rec.RootPane = canonical
		log.Printf("closeme: pane %s belongs to agent %s recorded on peer %s — closing locally", canonical, rec.ID, cands[0].peer)
		return rec, true, nil
	default:
		var peers []string
		for _, c := range cands {
			peers = append(peers, c.peer)
		}
		sort.Strings(peers)
		return AgentRecord{}, false,
			fmt.Errorf("pane %q is claimed by agent records on multiple peer lassos (%s) — refusing to guess; close the agent from its owning lasso", canonical, strings.Join(peers, ", "))
	}
}
