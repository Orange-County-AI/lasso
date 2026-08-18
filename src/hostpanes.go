package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// hostPoolEntry is a cached remote backend for herdr RPC and file operations
// against a selected non-active host. Its last-used timestamp drives idle
// reaping; lastOK throttles the per-access liveness check (see hostBackend).
type hostPoolEntry struct {
	backend  *remoteBackend
	lastUsed time.Time
	lastOK   time.Time // last successful liveness verification
}

var hostPool struct {
	mu      sync.Mutex
	entries map[string]*hostPoolEntry // keyed by ssh-config alias
}

// hostBackendIdle stays comfortably above the repo cache warmer's interval
// (repocache.go's warmInterval, 2m): the warmer touches every host's backend
// each cycle, so a shorter TTL would reconnect every remote host on every warm
// cycle. Generous idle retention avoids that control-master churn, while a host
// that stays unwarmed and unused is still collected. Dead pooled masters do not
// wait for the reaper: hostBackend liveness-checks entries on access and redials
// them in place.
const hostBackendIdle = 30 * time.Minute

// hostHealthEvery throttles pooled-backend liveness checks. Between checks a
// dead connection can surface as an operation error; the next checked access
// heals it.
const hostHealthEvery = 10 * time.Second

// hostBackend returns the connection to host: "local" uses the local socket;
// a compatible remote uses the pooled, idle-reaped remote backend on its own
// SSH master. The pool serves RPC, file, diff, and agent-creation work against
// any host, the repo cache warmer pre-warms it, and — since a switch adopts the
// entry rather than dialing beside it (see serveHostSwitch) — it also holds the
// ACTIVE host's connection. Exactly one per host.
//
// It used to be deliberately separate from the active backend, because a switch
// tore the active backend down and rebuilt it, which would have killed whatever
// long remote op — worktree.create, an agent boot, an SFTP upload — was riding
// it. A switch no longer tears anything down: connections die only on idle
// reaping (which skips the active host) or a failed liveness check. So the
// second master per host bought nothing and cost a full ssh handshake every
// time the user came back to a host. namedHostBackend still short-circuits the
// active host onto the connection we already hold.
//
// A cached backend is liveness-checked (throttled by hostHealthEvery) before
// being handed out. Its SSH master can die after a network drop, sshd restart,
// or laptop sleep; a dead entry is dropped and a fresh connection dialed in its
// place rather than making all operations for that host wait for idle reaping.
func hostBackend(host string) (Backend, error) {
	if host == "local" {
		return &localBackend{sock: *herdrSock}, nil
	}
	hostPool.mu.Lock()
	if hostPool.entries == nil {
		hostPool.entries = map[string]*hostPoolEntry{}
	}
	if e := hostPool.entries[host]; e != nil {
		e.lastUsed = time.Now()
		fresh := time.Since(e.lastOK) < hostHealthEvery
		b := e.backend
		hostPool.mu.Unlock()
		if fresh {
			return b, nil
		}
		if _, _, err := herdrPing(b.HerdrSock()); err == nil {
			hostPool.mu.Lock()
			if e := hostPool.entries[host]; e != nil && e.backend == b {
				e.lastOK = time.Now()
			}
			hostPool.mu.Unlock()
			return b, nil
		}
		log.Printf("host pool: %s connection unhealthy — reconnecting", host)
		hostPoolDrop(host, b)
	} else {
		hostPool.mu.Unlock()
	}

	// Dial (or wait for a concurrent dial of) a fresh connection. The per-host
	// mutex — never the global pool lock, which must stay cheap for touch/evict/
	// reap — serializes same-host dials: a new backend reuses the SAME PID+host
	// socket paths, so two dials at once (or a dial racing a teardown) would
	// clobber each other's control master.
	mu := hostDialMu(host)
	mu.Lock()
	defer mu.Unlock()
	hostPool.mu.Lock()
	if e := hostPool.entries[host]; e != nil { // a concurrent dialer beat us
		e.lastUsed = time.Now()
		b := e.backend
		hostPool.mu.Unlock()
		return b, nil
	}
	hostPool.mu.Unlock()

	hi, ok := findHost(host)
	if !ok || !hi.Reachable || !hi.Running || !hi.Compatible {
		return nil, fmt.Errorf("host %s not available", host)
	}
	_, wantProto := localProtocol()
	rb, err := newRemoteBackend(srvCtx, host, hi.Socket, wantProto)
	if err != nil {
		return nil, err
	}
	hostPool.mu.Lock()
	hostPool.entries[host] = &hostPoolEntry{backend: rb, lastUsed: time.Now(), lastOK: time.Now()}
	hostPool.mu.Unlock()
	// Redialing the ACTIVE host replaces the connection the active backend and
	// the hub's event subscription are riding (a switch adopts this very entry),
	// so re-point both. Without this, a master that died under the active host —
	// laptop sleep, network drop, sshd restart — left the UI polling a socket
	// that no longer exists until the user switched away and back.
	if curBackend().Name() == host {
		setBackend(rb)
		if srvHub != nil {
			srvHub.startSub()
		}
	}
	startHostPoolReaper()
	return rb, nil
}

// hostDialMu returns the per-host mutex serializing pool dials and teardowns.
func hostDialMu(host string) *sync.Mutex {
	hostDials.mu.Lock()
	defer hostDials.mu.Unlock()
	if hostDials.byHost == nil {
		hostDials.byHost = map[string]*sync.Mutex{}
	}
	m := hostDials.byHost[host]
	if m == nil {
		m = &sync.Mutex{}
		hostDials.byHost[host] = m
	}
	return m
}

var hostDials struct {
	mu     sync.Mutex
	byHost map[string]*sync.Mutex
}

// hostPoolDrop removes host's pool entry if it still holds b (a concurrent
// healer may have already replaced it). The close is synchronous, under the
// host's dial mutex: the redial that typically follows reuses the same socket
// paths, and an async teardown's `ssh -O exit` landing after the new master
// bound them would kill the fresh connection.
func hostPoolDrop(host string, b *remoteBackend) {
	hostPool.mu.Lock()
	e := hostPool.entries[host]
	if e != nil && e.backend == b {
		delete(hostPool.entries, host)
	} else {
		e = nil
	}
	hostPool.mu.Unlock()
	if e != nil {
		mu := hostDialMu(host)
		mu.Lock()
		_ = b.Close()
		mu.Unlock()
	}
}

// hostPoolHas reports whether a pooled connection to host is currently held.
func hostPoolHas(host string) bool {
	hostPool.mu.Lock()
	defer hostPool.mu.Unlock()
	_, ok := hostPool.entries[host]
	return ok
}

// hostPoolHosts snapshots the aliases with a pooled connection.
func hostPoolHosts() []string {
	hostPool.mu.Lock()
	defer hostPool.mu.Unlock()
	hosts := make([]string, 0, len(hostPool.entries))
	for h := range hostPool.entries {
		hosts = append(hosts, h)
	}
	return hosts
}

// closeBackendsOnExit synchronously closes the active backend and every pooled
// backend so their SSH control masters and forwarded sockets are cleaned up
// before the process exits. Teardown is otherwise asynchronous, and main
// returning would race it — leaving masters until their ControlPersist expires.
func closeBackendsOnExit() {
	hostPool.mu.Lock()
	entries := hostPool.entries
	hostPool.entries = nil
	hostPool.mu.Unlock()
	for _, e := range entries {
		_ = e.backend.Close()
	}
	_ = curBackend().Close()
}

// reapHostBackends drops pool entries no one has touched for hostBackendIdle.
// The ACTIVE host is never a candidate: its entry IS the active backend, whose
// only "use" between switches is the hub's event stream, which reads the
// forwarded socket directly rather than through hostBackend.
func reapHostBackends() {
	now := time.Now()
	type deadEntry struct {
		host    string
		backend *remoteBackend
	}
	var dead []deadEntry
	activeHost := curBackend().Name()
	hostPool.mu.Lock()
	for host, e := range hostPool.entries {
		if host != activeHost && now.Sub(e.lastUsed) > hostBackendIdle {
			dead = append(dead, deadEntry{host, e.backend})
			delete(hostPool.entries, host)
		}
	}
	hostPool.mu.Unlock()
	for _, d := range dead {
		// A concurrent redial reuses this host's socket paths and must not race
		// the teardown.
		mu := hostDialMu(d.host)
		mu.Lock()
		_ = d.backend.Close()
		mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// GET /api/all-panes — every pane across every compatible host
// ---------------------------------------------------------------------------

// hostPane is one pane on one host, enriched with workspace/tab labels and
// whether herdr has detected an agent in it (HasAgent / Agent come from
// agent.list, since pane.list reports only agent_status, not the agent kind).
type hostPane struct {
	Host           string `json:"host"`       // "local" or ssh-config alias (focus/attach key)
	HostLabel      string `json:"host_label"` // display name (hostname for local)
	PaneID         string `json:"pane_id"`
	WorkspaceID    string `json:"workspace_id"`
	WorkspaceLabel string `json:"workspace_label"`
	TabID          string `json:"tab_id"`
	TabLabel       string `json:"tab_label"`
	PaneLabel      string `json:"pane_label,omitempty"` // herdr's per-pane title; disambiguates sibling panes in one workspace
	// TerminalTitle is the pane's OSC title with the agent's state glyphs
	// stripped — for an agent pane, what it is currently working on ("Check Norm
	// outline wiki connection"). It names a session whose workspace was never
	// labelled, and is the only name a foreign session has in that case.
	TerminalTitle string `json:"terminal_title,omitempty"`
	Cwd           string `json:"cwd"`
	Agent         string `json:"agent"`
	AgentStatus   string `json:"agent_status"`
	HasAgent      bool   `json:"has_agent"`
	Focused       bool   `json:"focused"`
	// Prompt is the initial prompt the user gave the agent when creating it
	// (lasso's AgentRecord.Description, not anything herdr knows). It's shipped so
	// the pane switcher can search the full prompt text; the UI need not display it.
	Prompt string `json:"prompt,omitempty"`
	// AgentID + Closed are set only on the rows /api/agent-history adds for past
	// agents (lasso AgentRecords) whose herdr pane is gone. AgentID is the record's
	// id, passed back to /api/agent/reopen to re-create a workspace at its work dir.
	// Live panes leave both empty/false.
	AgentID string `json:"agent_id,omitempty"`
	Closed  bool   `json:"closed,omitempty"`
	// The Mirror* fields are set when this pane is a herdr-mirror stream of
	// another machine's pane rather than a pane on Host (see mirror.go). Such a
	// row IS a real local pane — it focuses and renders like any other — but
	// everything it shows lives on MirrorHost, so the UI must attribute it
	// there and must not offer it affordances that only mean something locally.
	// MirrorHost is herdr-mirror's host key, MirrorLabel the workspace's label as
	// it reads on the remote (no "<host>: " prefix), and MirrorWorkspace /
	// MirrorPane the remote herdr's own ids.
	MirrorHost      string `json:"mirror_host,omitempty"`
	MirrorLabel     string `json:"mirror_label,omitempty"`
	MirrorWorkspace string `json:"mirror_workspace,omitempty"`
	MirrorPane      string `json:"mirror_pane,omitempty"`
}

type panesPayload struct {
	Panes  []hostPane        `json:"panes"`
	Errors map[string]string `json:"errors,omitempty"` // host → why it couldn't be listed
}

// panesCache coalesces the potentially multi-second, multi-host aggregation so
// overlapping polls and concurrent viewers share one fetch. Herdr state moves,
// so the frontend refreshes it every few seconds.
//
// inflight is non-nil while a refresh is running and closes when it lands. The
// refresh runs with mu released: holding it across aggregation serialized every
// /api/all-panes caller behind the slowest host. A refresh that outlived the TTL
// then made each waiter start another one, leaving the ⌘K palette wedged until
// restart.
var panesCache struct {
	mu       sync.Mutex
	at       time.Time
	data     panesPayload
	inflight chan struct{}
}

const panesCacheTTL = 1500 * time.Millisecond

// paneHostTimeout bounds one host's contribution to the aggregation. Generous
// against the slow-but-alive case — a cold redial is a handshake plus a socket
// readiness wait — but finite, so a host that has stopped answering can only
// cost this much per poll instead of the whole endpoint.
const paneHostTimeout = 20 * time.Second

// invalidatePanesCache drops the cached aggregation after a pane-changing
// operation so the next /api/all-panes request refetches without waiting for TTL.
func invalidatePanesCache() {
	panesCache.mu.Lock()
	panesCache.at = time.Time{}
	panesCache.mu.Unlock()
}

// hostAllowed reports whether host is one lasso may drive: local, the active
// host, a host with an existing pooled connection, or a discovered reachable,
// compatible remote. The pool check keeps a connected host usable through a
// transiently failed discovery probe; its live connection is better evidence
// than a flapped probe.
func hostAllowed(host string) bool {
	if host == "local" || host == curBackend().Name() || hostPoolHas(host) {
		return true
	}
	hi, ok := findHost(host)
	return ok && hi.Reachable && hi.Running && hi.Compatible
}

// ---------------------------------------------------------------------------
// GET/POST /api/ui-state — persisted browser UI preferences
// ---------------------------------------------------------------------------

// uiStateMu serializes /api/ui-state read-modify-writes so two tabs patching
// different fields at the same instant can't drop each other's write.
var uiStateMu sync.Mutex

func serveUIState(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		us, err := getUIState()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, us)
	case http.MethodPost:
		uiStateMu.Lock()
		defer uiStateMu.Unlock()
		// Patch semantics: start from the stored state and decode the request
		// over it — only fields present in the body change, so a tab holding a
		// stale copy can't clobber fields it didn't touch (each client sends
		// just its patch).
		us, err := getUIState()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&us); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if us.UsageHidden == nil {
			us.UsageHidden = []string{}
		}
		if us.UsageOrder == nil {
			us.UsageOrder = []string{}
		}
		if err := saveUIState(us); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Nudge every open tab (including the writer) to refetch and converge.
		if srvHub != nil {
			srvHub.bumpUIStateRev()
		}
		writeJSON(w, us)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// GET /api/agent-history — every agent lasso ever spawned, as switcher rows
// ---------------------------------------------------------------------------

// serveAgentHistory returns every recorded agent (across hosts) shaped as a
// hostPane so the ⌘K switcher can list past agents alongside live panes. These
// carry AgentID (for reopen) and the agent's work dir as Cwd; the title rides in
// WorkspaceLabel so the switcher's primary label and search both pick it up. The
// frontend decides which are actually closed by diffing host+pane_id against the
// live pane listing — a record whose pane is still live is just the same agent
// it already shows, so it dedupes those out.
func serveAgentHistory(w http.ResponseWriter, r *http.Request) {
	recs, err := listAllAgentsIncludingClosed()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	local := localHostname()
	out := make([]hostPane, 0, len(recs))
	known := map[string]map[string]bool{} // host -> recorded work dirs (for orphan dedup)
	// listAllAgents returns oldest-first; walk it in reverse so the switcher lists
	// the most recently created agents at the top — the ones you're most likely
	// looking for and least likely to remember a search term for.
	for i := len(recs) - 1; i >= 0; i-- {
		ha := recs[i]
		label := ha.Host
		if ha.Host == "local" {
			label = local
		}
		out = append(out, hostPane{
			Host:           ha.Host,
			HostLabel:      label,
			PaneID:         ha.Agent.RootPane,
			WorkspaceID:    ha.Agent.WorkspaceID,
			WorkspaceLabel: ha.Agent.Title,
			Cwd:            ha.Agent.WorkDir,
			Agent:          ha.Agent.Agent,
			HasAgent:       ha.Agent.Agent != "",
			Prompt:         ha.Agent.Description,
			AgentID:        ha.Agent.ID,
		})
		if ha.Agent.WorkDir != "" {
			if known[ha.Host] == nil {
				known[ha.Host] = map[string]bool{}
			}
			known[ha.Host][ha.Agent.WorkDir] = true
		}
	}
	// Fold in orphan directories on the local host — sessions whose worktree/scratch
	// dir is still on disk but have no agent record (created before agent tracking,
	// or whose record was never written). Without this they're unreachable from the
	// switcher; with it they're findable by directory name and reopenable by path.
	// Remote-host agents still surface via their DB records above; only local orphan
	// dirs are scanned (the common case, and it avoids per-toggle SFTP round-trips).
	lb := &localBackend{sock: *herdrSock}
	out = append(out, scanOrphanWorkDirs(lb, "local", local, known["local"])...)
	writeJSON(w, map[string]any{"agents": out})
}

// scanOrphanWorkDirs lists directories under a host's lasso scratch/ and
// worktrees/<repo>/ trees that aren't in known (the recorded work dirs), shaped as
// switcher rows. Scratch dirs sit one level under scratch/; worktree dirs sit two
// levels under worktrees/ (worktrees/<repo>/<dir>). The full path rides in Cwd so
// the switcher matches against it; the humanized basename is the display label.
// They carry no AgentID — reopen lands by raw path. A tree that can't be read just
// yields no rows.
func scanOrphanWorkDirs(b Backend, host, hostLabel string, known map[string]bool) []hostPane {
	var out []hostPane
	add := func(dir string) {
		if known[dir] {
			return
		}
		out = append(out, hostPane{
			Host:           host,
			HostLabel:      hostLabel,
			WorkspaceLabel: humanizeSlug(filepath.Base(dir)),
			Cwd:            dir,
		})
	}
	scratch := lassoScratchDirFor(b)
	if ents, err := b.ReadDir(scratch); err == nil {
		for _, e := range ents {
			if e.Dir {
				add(filepath.Join(scratch, e.Name))
			}
		}
	}
	wt := lassoWorktreesDirFor(b)
	if repos, err := b.ReadDir(wt); err == nil {
		for _, repo := range repos {
			if !repo.Dir {
				continue
			}
			repoDir := filepath.Join(wt, repo.Name)
			if ents, err := b.ReadDir(repoDir); err == nil {
				for _, e := range ents {
					if e.Dir {
						add(filepath.Join(repoDir, e.Name))
					}
				}
			}
		}
	}
	return out
}

// humanizeSlug turns a directory slug ("ksa-boilerplate-engagement-odoo-sign-1i5t")
// into a readable label by swapping dashes for spaces. The raw path still rides in
// Cwd for search, so this is purely cosmetic.
func humanizeSlug(s string) string { return strings.ReplaceAll(s, "-", " ") }

// withinLassoWorkTrees reports whether dir sits strictly under the host's lasso
// worktrees/ or scratch/ trees — the only paths reopen-by-raw-path is allowed to
// open (an orphan dir with no agent record), so the endpoint can't be coaxed into
// opening an arbitrary directory.
func withinLassoWorkTrees(b Backend, dir string) bool {
	clean := filepath.Clean(dir)
	for _, root := range []string{lassoWorktreesDirFor(b), lassoScratchDirFor(b)} {
		root = filepath.Clean(root)
		if clean != root && strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// POST /api/agent/reopen — re-create a workspace at a past agent's work dir
// ---------------------------------------------------------------------------

// serveAgentReopen re-opens the workspace for a previously-spawned agent whose
// herdr pane was closed: it creates a fresh herdr workspace rooted at the stored
// work dir (the worktree/scratch dir still on disk) and focuses it. It does NOT
// relaunch the agent — per the design, reopening just lands you back in the
// directory; the user starts claude (e.g. `claude --continue`) themselves. The
// record is re-pointed at the new workspace/pane so it shows as live again, and
// the new pane is returned (as a hostPane) so the client can focus it.
func serveAgentReopen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host    string `json:"host"`
		AgentID string `json:"agent_id"`
		WorkDir string `json:"work_dir"`
		// Focus lands the user on the reopened workspace (default true — the
		// ⌘K switcher reopens to jump there). An API caller reopening in the
		// background passes false so it doesn't move every client's shared
		// herdr focus.
		Focus *bool `json:"focus"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Host == "" {
		req.Host = "local"
	}
	if req.AgentID == "" && req.WorkDir == "" {
		http.Error(w, "agent_id or work_dir required", http.StatusBadRequest)
		return
	}
	if !hostAllowed(req.Host) {
		http.Error(w, "host not available", http.StatusBadRequest)
		return
	}
	b, err := hostBackend(req.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Resolve the dir + label either from the agent record (re-pointing it so it
	// reads as live again) or, for an orphan directory with no record, from the
	// requested path (constrained to the lasso worktrees/scratch trees).
	var workDir, label, recID string
	if req.AgentID != "" {
		// Tombstones included: reopening an agent whose pane herdr no longer has is
		// precisely what this endpoint is for, and it revives the record (see
		// updateAgentPane below).
		rec, err := findAgentRecordAny(req.Host, req.AgentID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if rec.WorkDir == "" {
			http.Error(w, "agent has no work dir to reopen", http.StatusBadRequest)
			return
		}
		workDir, label, recID = rec.WorkDir, rec.Title, rec.ID
	} else {
		if !withinLassoWorkTrees(b, req.WorkDir) {
			http.Error(w, "work dir is outside the lasso worktrees/scratch dirs", http.StatusBadRequest)
			return
		}
		workDir = filepath.Clean(req.WorkDir)
		label = humanizeSlug(filepath.Base(workDir))
	}
	if _, statErr := b.Stat(workDir); statErr != nil {
		http.Error(w, fmt.Sprintf("work dir %s is gone: %v", workDir, statErr), http.StatusGone)
		return
	}
	res, err := b.HerdrCall("workspace.create", map[string]any{
		"cwd":   workDir,
		"label": label,
		"focus": req.Focus == nil || *req.Focus,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("workspace.create: %v", err), http.StatusBadGateway)
		return
	}
	ws, pane := parseCreateResult(res)
	// Re-point the record at the new workspace/pane so it reads as live again.
	// Orphan dirs have no record to update.
	if recID != "" {
		_ = updateAgentPane(recID, req.Host, ws, pane)
	}
	invalidatePanesCache()

	// Return the new pane as a full hostPane (terminal_id, tab_id, …) so the client
	// can focus it through the normal path. Look it up in the host's live panes.
	panes, _ := enumerateHostPanes(b, req.Host, hostLabelFor(req.Host))
	for _, p := range panes {
		if p.PaneID == pane {
			writeJSON(w, p)
			return
		}
	}
	// Fall back to the minimal identifiers if the fresh pane isn't listed yet.
	writeJSON(w, hostPane{Host: req.Host, HostLabel: hostLabelFor(req.Host), PaneID: pane, WorkspaceID: ws, Cwd: workDir})
}

// hostLabelFor returns the display label for a host: the machine hostname for
// local, else the ssh-config alias (matching fetchAllPanes' labeling).
func hostLabelFor(host string) string {
	if host == "local" {
		return localHostname()
	}
	return host
}

func serveAllPanes(w http.ResponseWriter, r *http.Request) {
	startHostPoolReaper()
	writeJSON(w, panesSnapshot(r.Context()))
}

// panesSnapshot serves the cached aggregation, refreshing it at most once at a
// time. One caller becomes the refresher and the rest wait on its result (or on
// their own request being cancelled) — nobody holds panesCache.mu across the
// fetch, so a slow host costs latency instead of wedging the endpoint.
//
// The refresh runs under the server context, not the triggering request's: it is
// shared work, and the client that happened to kick it off navigating away must
// not cancel it out from under everyone waiting.
func panesSnapshot(ctx context.Context) panesPayload {
	panesCache.mu.Lock()
	if !panesCache.at.IsZero() && time.Since(panesCache.at) < panesCacheTTL {
		data := panesCache.data
		panesCache.mu.Unlock()
		return data
	}
	if wait := panesCache.inflight; wait != nil {
		panesCache.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
		}
		panesCache.mu.Lock()
		data := panesCache.data
		panesCache.mu.Unlock()
		return panesEmptyIfNil(data)
	}
	wait := make(chan struct{})
	panesCache.inflight = wait
	panesCache.mu.Unlock()

	data := panesFetch(sweepCtx())

	panesCache.mu.Lock()
	panesCache.at, panesCache.data, panesCache.inflight = time.Now(), data, nil
	panesCache.mu.Unlock()
	close(wait)
	return data
}

// panesFetch is the aggregation panesSnapshot refreshes through — a variable so a
// test can stand a slow or counting fetch in for it.
var panesFetch = fetchAllPanes

// panesEmptyIfNil keeps the payload's panes an empty array rather than JSON null
// for a caller that gave up (or asked) before any fetch had ever landed.
func panesEmptyIfNil(p panesPayload) panesPayload {
	if p.Panes == nil {
		p.Panes = []hostPane{}
	}
	return p
}

// paneTarget is one host to aggregate.
type paneTarget struct {
	host  string // "local" or alias
	label string
}

// lastGoodPanes remembers each host's most recent successful listing. A
// transient failure degrades to stale panes plus an error instead of removing a
// flapping host from the ⌘K palette and MCP pane enumeration. A host that stays
// gone ages out after lastGoodPanesTTL.
var lastGoodPanes struct {
	mu     sync.Mutex
	byHost map[string]lastGoodPanesEntry
}

type lastGoodPanesEntry struct {
	panes []hostPane
	at    time.Time
}

const lastGoodPanesTTL = 5 * time.Minute

func lastGoodPanesSet(host string, panes []hostPane) {
	lastGoodPanes.mu.Lock()
	if lastGoodPanes.byHost == nil {
		lastGoodPanes.byHost = map[string]lastGoodPanesEntry{}
	}
	lastGoodPanes.byHost[host] = lastGoodPanesEntry{panes: panes, at: time.Now()}
	lastGoodPanes.mu.Unlock()
}

// lastGoodPanesFor returns host's remembered panes if still within the TTL,
// dropping an aged-out entry on the way.
func lastGoodPanesFor(host string) ([]hostPane, bool) {
	lastGoodPanes.mu.Lock()
	defer lastGoodPanes.mu.Unlock()
	e, ok := lastGoodPanes.byHost[host]
	if !ok {
		return nil, false
	}
	if time.Since(e.at) > lastGoodPanesTTL {
		delete(lastGoodPanes.byHost, host)
		return nil, false
	}
	return e.panes, true
}

// paneErrGrace suppresses a host error until it has failed continuously this
// long. A one-poll timeout over a flaky SSH link often self-heals; serving
// last-good panes without flashing an error keeps the ⌘K palette and MCP pane
// enumeration useful. A persistent failure still surfaces.
const paneErrGrace = 30 * time.Second

var paneFirstFail = struct {
	mu sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

// paneErrSurfaced records a failed host poll and reports whether the failure
// has persisted past the grace window (and so should be shown to the user).
func paneErrSurfaced(host string, now time.Time) bool {
	paneFirstFail.mu.Lock()
	defer paneFirstFail.mu.Unlock()
	first, ok := paneFirstFail.at[host]
	if !ok {
		paneFirstFail.at[host] = now
		return false
	}
	return now.Sub(first) >= paneErrGrace
}

// paneErrClear forgets a host's failure streak after a successful poll.
func paneErrClear(host string) {
	paneFirstFail.mu.Lock()
	defer paneFirstFail.mu.Unlock()
	delete(paneFirstFail.at, host)
}

// paneErrText condenses a host-poll error for /api/all-panes. Transport-level
// failures all mean the host cannot be reached now, so they render as a short
// "unreachable" note instead of a raw dial/read chain. Protocol drift and herdr
// refusals retain their messages.
func paneErrText(err error) string {
	s := firstLine(err.Error())
	lower := strings.ToLower(s)
	for _, m := range []string{
		"i/o timeout", "connection refused", "connection reset",
		"broken pipe", "no such file or directory",
		"no route to host", "network is unreachable", "eof",
	} {
		if strings.Contains(lower, m) {
			return "unreachable (" + m + ")"
		}
	}
	return s
}

// fetchAllPanes queries every compatible host concurrently and merges their
// panes. A host that cannot be listed serves fresh last-known panes alongside an
// error rather than vanishing from the switcher.
// Local is always included; remotes come from the (cached) host discovery
// probe, plus any host we still hold a pooled connection to (a live connection
// outranks a transiently-failed probe).
func fetchAllPanes(ctx context.Context) panesPayload {
	targets := []paneTarget{{host: "local", label: localHostname()}}
	seen := map[string]bool{"local": true}
	for _, hi := range discoverHosts(ctx, false) {
		if hi.Reachable && hi.Running && hi.Compatible {
			targets = append(targets, paneTarget{host: hi.Alias, label: hi.Alias})
			seen[hi.Alias] = true
		}
	}
	for _, host := range hostPoolHosts() {
		if !seen[host] {
			targets = append(targets, paneTarget{host: host, label: host})
			seen[host] = true
		}
	}

	type result struct {
		panes []hostPane
		err   error
		host  string
	}
	results := make([]result, len(targets))
	sem := make(chan struct{}, 6) // bound concurrent host queries
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t paneTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i].host = t.host
			// Every host gets its own deadline, because this is a fan-in: without
			// one, the aggregation is only ever as responsive as its worst host,
			// and a host that never answers means an answer for nobody. Past the
			// deadline the host degrades to its last-good panes plus an error —
			// the same treatment a failing host already gets — and its listing is
			// abandoned to finish (or not) on its own. The send is buffered so
			// that goroutine can't block on a receiver that has moved on.
			ch := make(chan result, 1)
			go func() {
				b, err := hostBackend(t.host)
				if err != nil {
					ch <- result{err: err}
					return
				}
				panes, err := enumerateHostPanes(b, t.host, t.label)
				ch <- result{panes: panes, err: err}
			}()
			select {
			case r := <-ch:
				results[i].panes, results[i].err = r.panes, r.err
			case <-time.After(paneHostTimeout):
				results[i].err = fmt.Errorf("timed out after %v", paneHostTimeout)
			}
		}(i, t)
	}
	wg.Wait()

	out := panesPayload{Panes: []hostPane{}}
	for _, r := range results {
		if r.err != nil {
			if paneErrSurfaced(r.host, time.Now()) {
				if out.Errors == nil {
					out.Errors = map[string]string{}
				}
				out.Errors[r.host] = paneErrText(r.err)
			}
			if panes, ok := lastGoodPanesFor(r.host); ok {
				out.Panes = append(out.Panes, panes...)
			}
			continue
		}
		paneErrClear(r.host)
		lastGoodPanesSet(r.host, r.panes)
		// This branch is the only place lasso holds a fresh, complete, per-host
		// pane enumeration on a schedule, which is exactly what reconciling agent
		// records against herdr needs — and the r.err split above is already the
		// "did herdr actually answer" distinction reconciliation must not get
		// wrong. Deliberately not in the failure branch: last-good panes are a
		// display fallback, not evidence about what is running now.
		reconcileHostAgents(r.host, r.panes)
		out.Panes = append(out.Panes, r.panes...)
	}
	return out
}

// enumerateHostPanes lists one host's panes and joins workspace/tab labels +
// agent detection. Mirrors fetchPanes' join, over an arbitrary backend.
func enumerateHostPanes(b Backend, host, hostLabel string) ([]hostPane, error) {
	res, err := b.HerdrCall("pane.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var pl struct {
		Panes []pane `json:"panes"`
	}
	if err := json.Unmarshal(res, &pl); err != nil {
		return nil, err
	}

	type meta struct {
		label  string
		number int
	}
	tabs := map[string]meta{}
	if r, err := b.HerdrCall("tab.list", map[string]any{}); err == nil {
		var tl struct {
			Tabs []struct {
				TabID  string `json:"tab_id"`
				Label  string `json:"label"`
				Number int    `json:"number"`
			} `json:"tabs"`
		}
		if json.Unmarshal(r, &tl) == nil {
			for _, t := range tl.Tabs {
				tabs[t.TabID] = meta{t.Label, t.Number}
			}
		}
	}
	wss := map[string]meta{}
	if r, err := b.HerdrCall("workspace.list", map[string]any{}); err == nil {
		var wl struct {
			Workspaces []struct {
				WorkspaceID string `json:"workspace_id"`
				Label       string `json:"label"`
				Number      int    `json:"number"`
			} `json:"workspaces"`
		}
		if json.Unmarshal(r, &wl) == nil {
			for _, w := range wl.Workspaces {
				wss[w.WorkspaceID] = meta{w.Label, w.Number}
			}
		}
	}
	// agent.list enumerates the panes herdr has identified an agent in, with the
	// agent *kind* (claude/codex/…). It is not the only source — pane.list has
	// carried the kind since herdr 0.7, and paneAgentPresence recovers panes
	// whose agent herdr never identified at all — but where it does answer it is
	// the most direct one, so it is folded in first.
	agentKind := map[string]string{}
	if r, err := b.HerdrCall("agent.list", map[string]any{}); err == nil {
		var al struct {
			Agents []struct {
				PaneID string `json:"pane_id"`
				Agent  string `json:"agent"`
			} `json:"agents"`
		}
		if json.Unmarshal(r, &al) == nil {
			for _, a := range al.Agents {
				agentKind[a.PaneID] = a.Agent
			}
		}
	}

	// Agent initial prompts live in lasso's own records (AgentRecord.Description),
	// not in herdr — join them in by root pane (and by workspace as a fallback for
	// the agent's pane) so the pane switcher can search the full prompt text.
	//
	// The same pass collects the panes whose agent lasso launched in omp's plan
	// mode. herdr cannot see omp's plan gate (ompplan.go), so those panes — and
	// ONLY those — get a screen read below to find out whether they are parked on
	// it. Narrowed to the records rather than to every omp pane on the host
	// because this runs on the aggregation's poll: a plan-mode omp agent is the
	// only pane that can be at the gate lasso promised, and there are usually none.
	promptByPane := map[string]string{}
	promptByWS := map[string]string{}
	planGate := map[string]bool{}
	if recs, err := listAgents(host); err == nil {
		for _, rec := range recs {
			if rec.PlanMode && rec.RootPane != "" && harnessByID(rec.Agent).stagesConfigOverlay {
				planGate[rec.RootPane] = true
			}
			if rec.Description == "" {
				continue
			}
			if rec.RootPane != "" {
				promptByPane[rec.RootPane] = rec.Description
			}
			if rec.WorkspaceID != "" {
				promptByWS[rec.WorkspaceID] = rec.Description
			}
		}
	}
	// Which of these panes are herdr-mirror streams of another machine's panes,
	// and whose. Free for a host running no mirrors (see hostMirrors).
	mirrors := hostMirrors(b, pl.Panes)

	out := make([]hostPane, 0, len(pl.Panes))
	for _, p := range pl.Panes {
		kind, isAgent := agentKind[p.PaneID]
		status := p.AgentStatus
		if !isAgent {
			// herdr's agent.list left this pane out. That is authoritative for a
			// bare shell — and wrong for a pane whose agent it failed to identify,
			// which paneAgentPresence recovers from the pane's own session and
			// title (see panestatus.go).
			kind, status = paneAgentPresence(p)
			isAgent = kind != ""
		} else if status == "" || status == "unknown" {
			// Identified, but herdr has no state for it — read the title itself.
			if s, _ := titleAgentStatus(kind, p.TerminalTitle); s != "" {
				status = s
			}
		}
		if planGate[p.PaneID] {
			status = ompGateStatus(b, p.PaneID, kind, status)
		}
		prompt := promptByPane[p.PaneID]
		if prompt == "" && isAgent {
			prompt = promptByWS[p.WorkspaceID]
		}
		mr, _ := mirrors.lookup(p.WorkspaceID, p.PaneID)
		out = append(out, hostPane{
			Host:           host,
			HostLabel:      hostLabel,
			PaneID:         p.PaneID,
			WorkspaceID:    p.WorkspaceID,
			WorkspaceLabel: wss[p.WorkspaceID].label,
			TabID:          p.TabID,
			TabLabel:       tabs[p.TabID].label,
			PaneLabel:      p.Label,
			TerminalTitle:  p.TerminalTitleStripped,
			Cwd:            paneCwd(p),
			Agent:          kind,
			AgentStatus:    status,
			HasAgent:       isAgent,
			Focused:        p.Focused,
			Prompt:         prompt,

			MirrorHost:      mr.Host,
			MirrorLabel:     mr.Label,
			MirrorWorkspace: mr.Workspace,
			MirrorPane:      mr.Pane,
		})
	}
	// Newest first: herdr assigns workspaces/tabs monotonically increasing numbers
	// as they're created (and exposes no timestamps), so a descending sort puts the
	// most-recently-created workspaces — and within them the newest tabs — at the
	// top of the listing. Panes are still grouped by host (callers concatenate per
	// host); this orders within a host.
	sort.SliceStable(out, func(i, j int) bool {
		if wi, wj := wss[out[i].WorkspaceID].number, wss[out[j].WorkspaceID].number; wi != wj {
			return wi > wj
		}
		if ti, tj := tabs[out[i].TabID].number, tabs[out[j].TabID].number; ti != tj {
			return ti > tj
		}
		return out[i].PaneID > out[j].PaneID
	})
	return out, nil
}

// ---------------------------------------------------------------------------
// shared host-pool reaper
// ---------------------------------------------------------------------------

var hostReaperOnce sync.Once

// startHostPoolReaper launches the idle reaper once for the independent SSH
// backend pool.
func startHostPoolReaper() {
	hostReaperOnce.Do(func() {
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-srvCtx.Done():
					return
				case <-t.C:
					reapHostBackends()
				}
			}
		}()
	})
}
