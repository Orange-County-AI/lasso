package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// sidebarAllHosts mirrors the persisted uiState.SidebarAllHosts so the status
// poller can scrape every usable host (not just the active one) while the
// all-hosts sidebar is on. Set at startup from getUIState and on every UI-state
// save (serveUIState).
var sidebarAllHosts atomic.Bool

// Agent status tracking. There's no event stream reporting agent
// state, so a single goroutine (statusPoller) scrapes each live agent tab's tmux
// pane on a tick and runs the detect.go heuristics. The result is cached here and
// read by the SSE hub, the sidebar API (/api/tree, /api/agents), and MCP tools.

// statusStore is the process-wide cache of per-agent status, keyed by
// statusKey(host, tabID). Local agents key by plain tab id (so the sidebar, which
// looks up by tab id, is unchanged); remote agents key by "host|tabID". lastWorking
// is kept per agent for Claude's working-hold deglitch (see detectAgentStatus).
type statusStore struct {
	mu sync.RWMutex
	m  map[string]*statusEntry // keyed by statusKey(host, tabID)
}

type statusEntry struct {
	status      AgentStatus
	lastWorking time.Time
	at          time.Time // when this entry was last refreshed (TTL for on-demand reads)
}

var agentStatuses = &statusStore{m: map[string]*statusEntry{}}

// statusKey namespaces a tab's status by host: local agents key by plain tab id,
// remote agents by "host|tabID", so the sidebar's tab-id lookups keep working and
// the grid can address agents across hosts.
func statusKey(host, tabID string) string {
	if isLocalHost(host) {
		return tabID
	}
	return host + "|" + tabID
}

// status returns an agent's last-detected status (StatusUnknown if never polled).
func (s *statusStore) status(host, tabID string) AgentStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e := s.m[statusKey(host, tabID)]; e != nil {
		return e.status
	}
	return StatusUnknown
}

// statusFresh returns an agent's cached status and whether it's fresh (refreshed
// within ttl). Used by on-demand readers (MCP, grid) to avoid an ssh round trip
// when the poller already has a recent value.
func (s *statusStore) statusFresh(host, tabID string, ttl time.Duration) (AgentStatus, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e := s.m[statusKey(host, tabID)]; e != nil {
		return e.status, time.Since(e.at) < ttl
	}
	return StatusUnknown, false
}

// set records a freshly-detected status for an agent (used by on-demand reads so
// later callers hit the cache).
func (s *statusStore) set(host, tabID string, st AgentStatus) {
	s.mu.Lock()
	k := statusKey(host, tabID)
	if s.m[k] == nil {
		s.m[k] = &statusEntry{}
	}
	s.m[k].status = st
	s.m[k].at = time.Now()
	s.mu.Unlock()
}

// snapshot returns a copy of key→status for the SSE payload / API responses.
func (s *statusStore) snapshot() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.m))
	for id, e := range s.m {
		out[id] = string(e.status)
	}
	return out
}

// forget drops an agent's cached status (called when a tab is closed).
func (s *statusStore) forget(host, tabID string) {
	s.mu.Lock()
	delete(s.m, statusKey(host, tabID))
	s.mu.Unlock()
}

// pollOnce scrapes the LOCAL host's live-agent tabs. See pollHost.
func (s *statusStore) pollOnce() bool { return s.pollHost("") }

// pollHost scrapes every tab on host that currently has an agent, updating the
// cache (keyed statusKey(host, tabID)), and returns whether anything changed.
// Local presence is live (/proc, via agentKindsForHost); remote presence comes
// from the DB (an exited remote agent shows until its tab closes — we can't walk
// a remote /proc cheaply). A local tab whose agent exited is dropped from the
// cache; remote tabs are dropped only when their tab closes.
func (s *statusStore) pollHost(host string) bool {
	kinds := agentKindsForHost(host)
	now := time.Now()
	changed := false
	for tabID, kind := range kinds {
		session := tabSession(tabID)
		setSessionHost(session, host)
		screen, err := tmuxCapture(session)
		if err != nil {
			continue
		}
		key := statusKey(host, tabID)
		s.mu.RLock()
		e := s.m[key]
		var prev AgentStatus
		var lw time.Time
		if e != nil {
			prev, lw = e.status, e.lastWorking
		}
		s.mu.RUnlock()
		raw := detectAgentStatus(kind, screen, prev, &lw, now)
		s.mu.Lock()
		if s.m[key] == nil {
			s.m[key] = &statusEntry{}
			changed = true // a new agent appeared
		}
		s.m[key].lastWorking = lw
		s.m[key].at = now
		if s.m[key].status != raw {
			s.m[key].status = raw
			changed = true
		}
		s.mu.Unlock()
	}
	// Drop cache entries for THIS host's tabs that no longer have an agent.
	prefix := ""
	if !isLocalHost(host) {
		prefix = host + "|"
	}
	s.mu.Lock()
	for key := range s.m {
		// Only consider keys belonging to this host: local keys have no "|", remote
		// keys are "host|tabID".
		if prefix == "" {
			if strings.Contains(key, "|") {
				continue // a remote entry
			}
		} else if !strings.HasPrefix(key, prefix) {
			continue
		}
		tabID := strings.TrimPrefix(key, prefix)
		if _, ok := kinds[tabID]; !ok {
			delete(s.m, key)
			changed = true
		}
	}
	s.mu.Unlock()
	return changed
}

// agentKindsForHost returns live tab id → agent kind for a host. Local uses live
// /proc inspection (an exited agent stops counting); remote falls back to the DB
// (the stored agent kind for each still-open agent tab).
func agentKindsForHost(host string) map[string]string {
	if isLocalHost(host) {
		return tabAgentKinds()
	}
	out := map[string]string{}
	recs, _ := listAgents(host)
	for _, rec := range recs {
		if rec.Agent != "" && rec.TabID != "" && agentTabLive(rec) {
			out[rec.TabID] = rec.Agent
		}
	}
	return out
}

// statusPoller scans live agent tabs on a tick and kicks the hub when a status
// changes. It pauses while no browser is connected (no one to push to), so an
// idle lasso doesn't run capture-pane forever. The LOCAL host is polled every
// tick; when a REMOTE host is active it's polled on a slower cadence (each remote
// capture is an ssh round trip), so the host you're looking at stays near-live
// while background hosts are served on demand (agentStatusNow's cache).
func statusPoller(ctx context.Context, h *hub) {
	t := time.NewTicker(*statusEvery)
	defer t.Stop()
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if h.clientCount() == 0 {
				continue
			}
			tick++
			changed := agentStatuses.pollOnce()
			// Remote hosts are polled every ~3rd tick (≈2s at the 750ms default) —
			// each capture is an ssh round trip. In all-hosts mode every usable
			// remote is scraped so the sidebar's cross-host dots stay live;
			// otherwise only the active remote host is.
			if tick%3 == 0 {
				if sidebarAllHosts.Load() {
					for _, t := range usableHostTargets() {
						if isLocalHost(t.host) {
							continue
						}
						if agentStatuses.pollHost(t.host) {
							changed = true
						}
					}
				} else if active := curBackend().Name(); !isLocalHost(active) {
					if agentStatuses.pollHost(active) {
						changed = true
					}
				}
			}
			if changed {
				h.kick()
			}
		}
	}
}

// treeSignature is a deterministic string of the workspace/tab tree (ids,
// titles, kinds, pin/closed state). The hub bumps PanesRev when it changes so the
// sidebar re-renders after a workspace/tab is created, renamed, pinned, or closed.
func treeSignature() string {
	wss, err := listWorkspaces("local")
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for _, w := range wss {
		fmt.Fprintf(&sb, "%s:%s:%t|", w.ID, w.Title, w.Pinned)
		tabs, _ := listTabs(w.ID)
		for _, t := range tabs {
			fmt.Fprintf(&sb, "  %s:%s:%s;", t.ID, t.Title, t.Kind)
		}
	}
	return sb.String()
}
