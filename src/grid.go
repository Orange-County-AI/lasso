package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// gridTermSock is the private unix socket path for one grid terminal's ttyd,
// keyed by lasso's PID + the cell token so concurrent lasso instances never
// collide (mirrors the fixed terminals' PID-keyed sockets).
func gridTermSock(token string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("lasso-gridterm-%d-%s.sock", os.Getpid(), token))
}

// The Grid tab shows every herdr pane across every reachable, protocol-compatible
// host — not just the active one — and embeds each pane as a live terminal. herdr
// has no cross-host enumeration, so lasso aggregates it here: for each host we run
// pane.list / workspace.list / tab.list / agent.list over that host's socket and
// merge the rows, tagging each with its host. Per-pane terminals are individual
// ttyds running `herdr terminal attach <terminal_id>` — against the local herdr
// socket, or a remote host's forwarded socket pair (see gridAttachCmd) — pooled
// and reaped on idle.

// ---------------------------------------------------------------------------
// grid backend pool — herdr RPC to any compatible host without switching active
// ---------------------------------------------------------------------------

// gridPoolEntry is a cached remote backend for herdr RPC, grid terminal
// attaches, and the file ops that target a selected (non-active) host — the
// upload/paste handlers open its SFTP lazily via reqHostBackend. lastUsed
// drives idle reaping so connections (and their SSH control masters) don't
// linger after the Grid tab is closed; lastOK throttles the per-access
// liveness check (see gridHostBackend).
type gridPoolEntry struct {
	backend  *remoteBackend
	lastUsed time.Time
	lastOK   time.Time // last successful liveness verification
}

var gridPool struct {
	mu      sync.Mutex
	entries map[string]*gridPoolEntry // keyed by ssh-config alias
}

// gridBackendIdle must stay comfortably above the cache warmer's interval
// (repocache.go's warmInterval, 2m): the warmer touches every host's backend
// each cycle, so an idle TTL below it meant a full SSH connect + teardown per
// remote host every 2 minutes, forever — constant control-master churn (and
// log spam) with recurring windows where a grid op landed mid-reconnect.
const gridBackendIdle = 5 * time.Minute

// gridHealthEvery throttles the pooled-backend liveness check: a cache hit
// pings the host's forwarded socket at most this often. Between checks a dead
// connection surfaces as op errors; the next checked access (≤ one grid poll
// away) heals it.
const gridHealthEvery = 10 * time.Second

// gridHostBackend returns a Backend for RPC against host without disturbing the
// active backend: "local" → a localBackend on the local socket; any compatible
// remote (INCLUDING whichever host is currently active) → a lazily-created,
// idle-reaped remoteBackend from the grid pool, on its own socket-tagged SSH
// master (…-grid.sock), distinct from the active backend's (…-<host>.sock).
//
// The grid pool is deliberately kept SEPARATE from the active backend even for
// the active host. Reusing the active backend here saved one SSH connection, but
// it chained every grid terminal's lifetime to the active backend's: a host
// switch tears the active backend down and rebuilds it, which yanked the pty out
// from under every grid cell on both the host you left and the one you switched
// to — they went dead ("Press ⏎ to Reconnect") and had to re-attach on each
// click. Streaming grid cells over their own pool master instead makes them
// survive host switches untouched (the two masters coexist by design — different
// socket tags), at the cost of one extra, idle-reaped connection to the active
// remote host while the Grid tab is open.
//
// A cached backend is liveness-checked (throttled by gridHealthEvery) before
// being handed out: its SSH master can die out from under it — network drop,
// remote sshd restart, laptop sleep — and a pool that keeps serving the corpse
// turns one dead connection into every grid op on that host failing until the
// idle reaper happens by. A dead entry is dropped (killing the grid terminals
// that streamed over its master — they're dead anyway, and the frontend
// keepalive re-attaches them) and a fresh connection is dialed in its place.
func gridHostBackend(host string) (Backend, error) {
	if host == "local" {
		return &localBackend{sock: *herdrSock}, nil
	}
	gridPool.mu.Lock()
	if gridPool.entries == nil {
		gridPool.entries = map[string]*gridPoolEntry{}
	}
	if e := gridPool.entries[host]; e != nil {
		e.lastUsed = time.Now()
		fresh := time.Since(e.lastOK) < gridHealthEvery
		b := e.backend
		gridPool.mu.Unlock()
		if fresh {
			return b, nil
		}
		if _, _, err := herdrPing(b.HerdrSock()); err == nil {
			gridPool.mu.Lock()
			if e := gridPool.entries[host]; e != nil && e.backend == b {
				e.lastOK = time.Now()
			}
			gridPool.mu.Unlock()
			return b, nil
		}
		log.Printf("grid:     %s connection unhealthy — reconnecting", host)
		gridPoolDrop(host, b)
	} else {
		gridPool.mu.Unlock()
	}

	// Dial (or wait for a concurrent dial of) a fresh connection. The per-host
	// mutex — never the global pool lock, which must stay cheap for touch/evict/
	// reap — serializes same-host dials: a new backend reuses the SAME PID+tag
	// socket paths, so two dials at once (or a dial racing a teardown) would
	// clobber each other's control master.
	mu := gridDialMu(host)
	mu.Lock()
	defer mu.Unlock()
	gridPool.mu.Lock()
	if e := gridPool.entries[host]; e != nil { // a concurrent dialer beat us
		e.lastUsed = time.Now()
		b := e.backend
		gridPool.mu.Unlock()
		return b, nil
	}
	gridPool.mu.Unlock()

	hi, ok := findHost(host)
	if !ok || !hi.Reachable || !hi.Running || !hi.Compatible {
		return nil, fmt.Errorf("host %s not available", host)
	}
	_, wantProto := localProtocol()
	rb, err := newRemoteBackend(srvCtx, host, hi.Socket, wantProto, "grid")
	if err != nil {
		return nil, err
	}
	gridPool.mu.Lock()
	gridPool.entries[host] = &gridPoolEntry{backend: rb, lastUsed: time.Now(), lastOK: time.Now()}
	gridPool.mu.Unlock()
	startGridReaper()
	return rb, nil
}

// gridDialMu returns the per-host mutex serializing pool dials/teardowns for
// one alias (see gridHostBackend).
func gridDialMu(host string) *sync.Mutex {
	gridDials.mu.Lock()
	defer gridDials.mu.Unlock()
	if gridDials.byHost == nil {
		gridDials.byHost = map[string]*sync.Mutex{}
	}
	m := gridDials.byHost[host]
	if m == nil {
		m = &sync.Mutex{}
		gridDials.byHost[host] = m
	}
	return m
}

var gridDials struct {
	mu     sync.Mutex
	byHost map[string]*sync.Mutex
}

// gridPoolDrop removes host's pool entry if it still holds b (a concurrent
// healer may have already replaced it), closing b and releasing the grid
// terminals that streamed over its SSH master. The close is synchronous, under
// the host's dial mutex: the redial that typically follows reuses the same
// socket paths, and an async teardown's `ssh -O exit` landing after the new
// master bound them would kill the fresh connection.
func gridPoolDrop(host string, b *remoteBackend) {
	gridPool.mu.Lock()
	e := gridPool.entries[host]
	if e != nil && e.backend == b {
		delete(gridPool.entries, host)
	} else {
		e = nil
	}
	gridPool.mu.Unlock()
	if e != nil {
		releaseGridTermsForHost(host)
		mu := gridDialMu(host)
		mu.Lock()
		_ = b.Close()
		mu.Unlock()
	}
}

// gridPoolHas reports whether a pooled connection to host is currently held.
func gridPoolHas(host string) bool {
	gridPool.mu.Lock()
	defer gridPool.mu.Unlock()
	_, ok := gridPool.entries[host]
	return ok
}

// gridPoolHosts snapshots the aliases with a pooled connection.
func gridPoolHosts() []string {
	gridPool.mu.Lock()
	defer gridPool.mu.Unlock()
	hosts := make([]string, 0, len(gridPool.entries))
	for h := range gridPool.entries {
		hosts = append(hosts, h)
	}
	return hosts
}

// closeBackendsOnExit synchronously closes the active backend and every pooled
// grid backend so their SSH control masters and forwarded sockets are cleaned
// up before the process exits. Teardown is otherwise async (a goroutine per
// backend watching context cancel), and main returning would race it — leaving
// masters to linger until their ControlPersist expires.
func closeBackendsOnExit() {
	gridPool.mu.Lock()
	entries := gridPool.entries
	gridPool.entries = nil
	gridPool.mu.Unlock()
	for _, e := range entries {
		_ = e.backend.Close()
	}
	_ = curBackend().Close()
}

func reapGridBackends() {
	now := time.Now()
	// Hosts with a live grid terminal keep their backend: remote cells stream
	// over its SSH master, so reaping it would kill them mid-view. (Snapshot
	// under gridTerms.mu first; the two locks are never nested.)
	inUse := map[string]bool{}
	gridTerms.mu.Lock()
	for key := range gridTerms.byKey {
		if host, _, ok := strings.Cut(key, "|"); ok {
			inUse[host] = true
		}
	}
	gridTerms.mu.Unlock()
	type deadEntry struct {
		host    string
		backend *remoteBackend
	}
	var dead []deadEntry
	gridPool.mu.Lock()
	for host, e := range gridPool.entries {
		if inUse[host] {
			e.lastUsed = now // attached cells count as use
			continue
		}
		if now.Sub(e.lastUsed) > gridBackendIdle {
			dead = append(dead, deadEntry{host, e.backend})
			delete(gridPool.entries, host)
		}
	}
	gridPool.mu.Unlock()
	for _, d := range dead {
		// Same dial-mutex discipline as gridPoolDrop: a concurrent redial
		// reuses this host's socket paths and must not race the teardown.
		mu := gridDialMu(d.host)
		mu.Lock()
		_ = d.backend.Close()
		mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// GET /api/grid — every pane across every compatible host
// ---------------------------------------------------------------------------

// gridPane is one pane on one host, enriched with workspace/tab labels and
// whether herdr has detected an agent in it (HasAgent / Agent come from
// agent.list, since pane.list reports only agent_status, not the agent kind).
type gridPane struct {
	Host           string `json:"host"`       // "local" or ssh-config alias (focus/attach key)
	HostLabel      string `json:"host_label"` // display name (hostname for local)
	PaneID         string `json:"pane_id"`
	TerminalID     string `json:"terminal_id"`
	WorkspaceID    string `json:"workspace_id"`
	WorkspaceLabel string `json:"workspace_label"`
	TabID          string `json:"tab_id"`
	TabLabel       string `json:"tab_label"`
	PaneLabel      string `json:"pane_label,omitempty"` // herdr's per-pane title; disambiguates sibling panes in one workspace
	Cwd            string `json:"cwd"`
	Agent          string `json:"agent"`
	AgentStatus    string `json:"agent_status"`
	HasAgent       bool   `json:"has_agent"`
	Focused        bool   `json:"focused"`
	// Prompt is the initial prompt the user gave the agent when creating it
	// (lasso's AgentRecord.Description, not anything herdr knows). It's shipped so
	// the pane switcher can search the full prompt text; the UI need not display it.
	Prompt string `json:"prompt,omitempty"`
	// AgentID + Closed are set only on the rows /api/agent-history adds for past
	// agents (lasso AgentRecords) whose herdr pane is gone. AgentID is the record's
	// id, passed back to /api/agent/reopen to re-create a workspace at its work dir.
	// Live grid panes leave both empty/false.
	AgentID string `json:"agent_id,omitempty"`
	Closed  bool   `json:"closed,omitempty"`
}

type gridPayload struct {
	Panes  []gridPane        `json:"panes"`
	Errors map[string]string `json:"errors,omitempty"` // host → why it couldn't be listed
}

// gridCache coalesces the (potentially multi-second, multi-host) aggregation so
// overlapping polls and concurrent viewers share one fetch. Short TTL — herdr
// state moves, and the frontend polls a couple seconds apart.
var gridCache struct {
	mu   sync.Mutex
	at   time.Time
	data gridPayload
}

const gridCacheTTL = 1500 * time.Millisecond

// invalidateGridCache drops the cached aggregation so the next /api/grid refetches
// — used after a mutation (rename/close) so the change shows up without waiting
// for the TTL.
func invalidateGridCache() {
	gridCache.mu.Lock()
	gridCache.at = time.Time{}
	gridCache.mu.Unlock()
}

// gridHostAllowed reports whether host is one we may drive: local, the active
// host, a host we already hold a pooled connection to, or a discovered
// reachable+compatible remote. Guards every grid mutation so a bogus alias
// can't make us connect out. The pool check keeps a connected host usable
// through a transiently-failed discovery probe (e.g. a saturated sshd): the
// live connection is better evidence than a flapped probe, and without it the
// probe cache's 30s of "unreachable" would reject attaches and drop cells.
func gridHostAllowed(host string) bool {
	if host == "local" || host == curBackend().Name() || gridPoolHas(host) {
		return true
	}
	hi, ok := findHost(host)
	return ok && hi.Reachable && hi.Running && hi.Compatible
}

// ---------------------------------------------------------------------------
// GET/POST /api/ui-state — persisted browser UI prefs (grid filters + sidebar)
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
		if us.GridHiddenHosts == nil {
			us.GridHiddenHosts = []string{}
		}
		if us.GridSelected == nil {
			us.GridSelected = []string{}
		}
		if us.GridWatched == nil {
			us.GridWatched = []string{}
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
// grid mutations — rename / close, routed to the pane's own host
// ---------------------------------------------------------------------------

// serveGridRename renames the workspace a pane belongs to, on that pane's host
// (the grid spans hosts, so it can't go through the active-backend /api/rename).
// The cell's title is the workspace label, mirroring the Agents tab's rename.
func serveGridRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host        string `json:"host"`
		WorkspaceID string `json:"workspace_id"`
		Label       string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.WorkspaceID == "" || strings.TrimSpace(req.Label) == "" {
		http.Error(w, "workspace_id and non-empty label required", http.StatusBadRequest)
		return
	}
	if !gridHostAllowed(req.Host) {
		http.Error(w, "host not available", http.StatusBadRequest)
		return
	}
	b, err := gridHostBackend(req.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if _, err := b.HerdrCall("workspace.rename", map[string]any{"workspace_id": req.WorkspaceID, "label": req.Label}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	invalidateGridCache()
	writeJSON(w, map[string]any{"ok": true})
}

// serveGridClose closes panes, each on its own host. The selection can span
// hosts, so the request is a flat list of {host, pane_id}; failures are reported
// per pane rather than failing the whole batch.
func serveGridClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Panes []struct {
			Host   string `json:"host"`
			PaneID string `json:"pane_id"`
		} `json:"panes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	errs := map[string]string{}
	closed := 0
	for i, p := range req.Panes {
		if i > 0 && !sleepCtx(ctx, closePace) { // breather between panes (see closePane)
			break
		}
		if p.PaneID == "" || !gridHostAllowed(p.Host) {
			errs[p.PaneID] = "host not available"
			continue
		}
		b, err := gridHostBackend(p.Host)
		if err != nil {
			errs[p.PaneID] = err.Error()
			continue
		}
		// If an agent (claude/codex) is running in this pane, kill its process
		// first so it exits cleanly instead of being yanked out from under the
		// closing pane. No-op for panes without an agent — those just close.
		killPaneAgent(b, p.PaneID)
		// Retry with backoff like serveClose: closing a pane makes herdr recompute
		// layout, so a burst of single-shot closes races that and fails transiently.
		closer := func(id string) error {
			_, err := b.HerdrCall("pane.close", map[string]any{"pane_id": id})
			return err
		}
		if err := closePaneWith(ctx, closer, p.PaneID); err != nil {
			errs[p.PaneID] = err.Error()
			continue
		}
		closed++
	}
	invalidateGridCache()
	writeJSON(w, map[string]any{"closed": closed, "errors": errs})
}

// ---------------------------------------------------------------------------
// GET /api/agent-history — every agent lasso ever spawned, as switcher rows
// ---------------------------------------------------------------------------

// serveAgentHistory returns every recorded agent (across hosts) shaped as a
// gridPane so the ⌘K switcher can list past agents alongside live panes. These
// carry AgentID (for reopen) and the agent's work dir as Cwd; the title rides in
// WorkspaceLabel so the switcher's primary label and search both pick it up. The
// frontend decides which are actually closed by diffing host+pane_id against the
// live grid — a record whose pane is still live is just the same agent it already
// shows, so it dedupes those out.
func serveAgentHistory(w http.ResponseWriter, r *http.Request) {
	recs, err := listAllAgents()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	local := localHostname()
	out := make([]gridPane, 0, len(recs))
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
		out = append(out, gridPane{
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
func scanOrphanWorkDirs(b Backend, host, hostLabel string, known map[string]bool) []gridPane {
	var out []gridPane
	add := func(dir string) {
		if known[dir] {
			return
		}
		out = append(out, gridPane{
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
// the new pane is returned (as a gridPane) so the client can focus it.
func serveAgentReopen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host    string `json:"host"`
		AgentID string `json:"agent_id"`
		WorkDir string `json:"work_dir"`
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
	if !gridHostAllowed(req.Host) {
		http.Error(w, "host not available", http.StatusBadRequest)
		return
	}
	b, err := gridHostBackend(req.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Resolve the dir + label either from the agent record (re-pointing it so it
	// reads as live again) or, for an orphan directory with no record, from the
	// requested path (constrained to the lasso worktrees/scratch trees).
	var workDir, label, recID string
	if req.AgentID != "" {
		rec, err := findAgentRecord(req.Host, req.AgentID)
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
		"focus": true,
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
	invalidateGridCache()

	// Return the new pane as a full gridPane (terminal_id, tab_id, …) so the client
	// can focus it through the normal path. Look it up in the host's live panes.
	panes, _ := gridHostPanes(b, req.Host, hostLabelFor(req.Host))
	for _, p := range panes {
		if p.PaneID == pane {
			writeJSON(w, p)
			return
		}
	}
	// Fall back to the minimal identifiers if the fresh pane isn't listed yet.
	writeJSON(w, gridPane{Host: req.Host, HostLabel: hostLabelFor(req.Host), PaneID: pane, WorkspaceID: ws, Cwd: workDir})
}

// hostLabelFor returns the display label for a host: the machine hostname for
// local, else the ssh-config alias (matching fetchGridPanes' labeling).
func hostLabelFor(host string) string {
	if host == "local" {
		return localHostname()
	}
	return host
}

func serveGrid(w http.ResponseWriter, r *http.Request) {
	startGridReaper()
	gridCache.mu.Lock()
	defer gridCache.mu.Unlock()
	if !gridCache.at.IsZero() && time.Since(gridCache.at) < gridCacheTTL {
		writeJSON(w, gridCache.data)
		return
	}
	data := fetchGridPanes(r.Context())
	gridCache.at = time.Now()
	gridCache.data = data
	writeJSON(w, data)
}

// gridTarget is one host to aggregate.
type gridTarget struct {
	host  string // "local" or alias
	label string
}

// gridLastGood remembers each host's most recent successful pane listing so a
// transient failure — a dead SSH master caught mid-heal, a flapped discovery
// probe — degrades to stale panes plus an error instead of blanking the host
// out of the grid (which unmounted its cells and dropped it from the pane
// rail). A host that stays gone ages out after gridLastGoodTTL.
var gridLastGood struct {
	mu     sync.Mutex
	byHost map[string]gridLastGoodEntry
}

type gridLastGoodEntry struct {
	panes []gridPane
	at    time.Time
}

const gridLastGoodTTL = 5 * time.Minute

func gridLastGoodSet(host string, panes []gridPane) {
	gridLastGood.mu.Lock()
	if gridLastGood.byHost == nil {
		gridLastGood.byHost = map[string]gridLastGoodEntry{}
	}
	gridLastGood.byHost[host] = gridLastGoodEntry{panes: panes, at: time.Now()}
	gridLastGood.mu.Unlock()
}

// gridLastGoodFor returns host's remembered panes if still within the TTL,
// dropping an aged-out entry on the way.
func gridLastGoodFor(host string) ([]gridPane, bool) {
	gridLastGood.mu.Lock()
	defer gridLastGood.mu.Unlock()
	e, ok := gridLastGood.byHost[host]
	if !ok {
		return nil, false
	}
	if time.Since(e.at) > gridLastGoodTTL {
		delete(gridLastGood.byHost, host)
		return nil, false
	}
	return e.panes, true
}

// fetchGridPanes queries every compatible host concurrently and merges their
// panes. A host that can't be listed serves its last-known panes (if fresh)
// alongside an Errors entry rather than vanishing — or failing the whole grid.
// Local is always included; remotes come from the (cached) host discovery
// probe, plus any host we still hold a pooled connection to (a live connection
// outranks a transiently-failed probe).
func fetchGridPanes(ctx context.Context) gridPayload {
	targets := []gridTarget{{host: "local", label: localHostname()}}
	seen := map[string]bool{"local": true}
	for _, hi := range discoverHosts(ctx, false) {
		if hi.Reachable && hi.Running && hi.Compatible {
			targets = append(targets, gridTarget{host: hi.Alias, label: hi.Alias})
			seen[hi.Alias] = true
		}
	}
	for _, host := range gridPoolHosts() {
		if !seen[host] {
			targets = append(targets, gridTarget{host: host, label: host})
			seen[host] = true
		}
	}

	type result struct {
		panes []gridPane
		err   error
		host  string
	}
	results := make([]result, len(targets))
	sem := make(chan struct{}, 6) // bound concurrent host queries
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t gridTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i].host = t.host
			b, err := gridHostBackend(t.host)
			if err != nil {
				results[i].err = err
				return
			}
			results[i].panes, results[i].err = gridHostPanes(b, t.host, t.label)
		}(i, t)
	}
	wg.Wait()

	out := gridPayload{Panes: []gridPane{}}
	for _, r := range results {
		if r.err != nil {
			if out.Errors == nil {
				out.Errors = map[string]string{}
			}
			out.Errors[r.host] = firstLine(r.err.Error())
			if panes, ok := gridLastGoodFor(r.host); ok {
				out.Panes = append(out.Panes, panes...)
			}
			continue
		}
		gridLastGoodSet(r.host, r.panes)
		out.Panes = append(out.Panes, r.panes...)
	}
	return out
}

// gridHostPanes lists one host's panes and joins workspace/tab labels + agent
// detection. Mirrors fetchPanes' join, over an arbitrary backend.
func gridHostPanes(b Backend, host, hostLabel string) ([]gridPane, error) {
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
	// agent.list is the only source of the agent *kind* (claude/codex/…) and of
	// which panes are agents at all — pane.list carries agent_status but no kind.
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
	promptByPane := map[string]string{}
	promptByWS := map[string]string{}
	if recs, err := listAgents(host); err == nil {
		for _, rec := range recs {
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

	out := make([]gridPane, 0, len(pl.Panes))
	for _, p := range pl.Panes {
		kind, isAgent := agentKind[p.PaneID]
		prompt := promptByPane[p.PaneID]
		if prompt == "" && isAgent {
			prompt = promptByWS[p.WorkspaceID]
		}
		out = append(out, gridPane{
			Host:           host,
			HostLabel:      hostLabel,
			PaneID:         p.PaneID,
			TerminalID:     p.TerminalID,
			WorkspaceID:    p.WorkspaceID,
			WorkspaceLabel: wss[p.WorkspaceID].label,
			TabID:          p.TabID,
			TabLabel:       tabs[p.TabID].label,
			PaneLabel:      p.Label,
			Cwd:            paneCwd(p),
			Agent:          kind,
			AgentStatus:    p.AgentStatus,
			HasAgent:       isAgent,
			Focused:        p.Focused,
			Prompt:         prompt,
		})
	}
	// Newest first: herdr assigns workspaces/tabs monotonically increasing numbers
	// as they're created (and exposes no timestamps), so a descending sort puts the
	// most-recently-created workspaces — and within them the newest tabs — at the
	// top of the grid. Panes are still grouped by host (callers concatenate per
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
// per-pane terminals — a dynamic ttyd pool
// ---------------------------------------------------------------------------

// gridTermEntry is one ttyd attached to a single herdr pane, proxied under
// /grid-term/<token>/. lastUsed (bumped by both the keepalive POST and proxied
// traffic) drives idle reaping.
type gridTermEntry struct {
	token    string
	sock     string
	base     string // "/grid-term/<token>/"
	proxy    *httputil.ReverseProxy
	cancel   context.CancelFunc
	lastUsed time.Time
}

var gridTerms struct {
	mu      sync.Mutex
	byKey   map[string]*gridTermEntry // host|terminal_id → entry
	byToken map[string]*gridTermEntry // token → entry
}

const (
	// gridTermIdle: how long a grid terminal survives without a keepalive touch
	// or proxied traffic before the reaper kills it. This is only a BACKSTOP for a
	// cell whose explicit release was dropped — the normal teardown paths (per-cell
	// release on unmount, releaseAll when the Grid tab is left) fire promptly. It
	// must sit comfortably above the keepalive interval (KEEPALIVE_MS, 18s) with
	// enough headroom to survive the two things that legitimately stall keepalives
	// on a live, still-visible cell: (1) a backgrounded browser tab, where the
	// browser throttles the keepalive timer (Chrome clamps hidden-tab timers and,
	// after ~5 min hidden, to ~once/minute), and (2) a host-switch storm that
	// briefly saturates the browser's request pipeline. At the old 30s — barely
	// 1.6× the keepalive — either stall reaped every visible cell, and returning to
	// the tab (or the next click) then showed a full "Press ⏎ to Reconnect"
	// reconnect storm. 120s gives ~6× headroom; a truly-abandoned cell still lingers
	// only ~2 min, and its ttyd is cheap.
	gridTermIdle = 120 * time.Second
	gridTermMax  = 24 // backstop; lazy viewport-mounting keeps the real count low
)

// terminalIDRe restricts a terminal id to the characters herdr actually emits so
// it's safe to drop straight into the ttyd argv (no shell metacharacters).
var terminalIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func serveGridTerm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host       string `json:"host"`
		TerminalID string `json:"terminal_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Host == "" || !terminalIDRe.MatchString(req.TerminalID) {
		http.Error(w, "host and a valid terminal_id required", http.StatusBadRequest)
		return
	}
	// Only attach on a host we can actually drive (local, active, pooled, or a
	// discovered compatible remote). Guards against a bogus alias shelling out.
	if !gridHostAllowed(req.Host) {
		http.Error(w, "host not available", http.StatusBadRequest)
		return
	}
	base, err := ensureGridTerm(req.Host, req.TerminalID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"base": base})
}

// serveGridTermRelease tears down a pane's grid terminal. The frontend calls
// this when a cell leaves the grid (the tab is hidden, or the pane is focused in
// the Herdr terminal): herdr sizes a pane's pty to its smallest attached client,
// so a lingering thin grid attach would clamp the pane and keep a full-screen TUI
// rendering narrow in the wide Herdr terminal. Releasing detaches it so the pane
// resizes back up. Best-effort — releasing an unknown terminal is a no-op.
func serveGridTermRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host       string `json:"host"`
		TerminalID string `json:"terminal_id"`
		// Token (optional) scopes the release to the specific attach the caller
		// created. Cell releases are fire-and-forget, so one issued at unmount can
		// land AFTER a quick remount already re-attached the same pane — without
		// the token check it would kill the fresh attach out from under the new
		// cell, whose iframe then 404s until the next keepalive notices.
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	releaseGridTerm(req.Host, req.TerminalID, req.Token)
	writeJSON(w, map[string]any{"ok": true})
}

// releaseGridTerm kills the ttyd attached to one pane (if any), which detaches it
// from herdr so the pane is no longer held to this terminal's width. A non-empty
// token releases only that specific attach: when the key's current entry is a
// newer one (the pane was re-attached after this release was issued), it's left
// alone.
func releaseGridTerm(host, terminalID, token string) {
	key := host + "|" + terminalID
	gridTerms.mu.Lock()
	e := gridTerms.byKey[key]
	if e != nil && token != "" && e.token != token {
		e = nil // a newer attach owns this pane now — don't kill it
	}
	if e != nil {
		delete(gridTerms.byKey, key)
		delete(gridTerms.byToken, e.token)
	}
	gridTerms.mu.Unlock()
	if e != nil {
		e.cancel() // SIGTERMs the ttyd process group; its socket is unlinked on exit
	}
}

// serveGridTermTouch bumps a live grid terminal's idle timer WITHOUT ever spawning
// one. A visible cell already has a live attach, so its keepalive only needs to
// touch — and a touch (unlike ensureGridTerm) can't resurrect an attach the cell
// just released. That's the whole point: it closes the race where an in-flight
// keepalive lands after gridTermRelease and re-spawns a thin attach that then clamps
// the focused pane's width in the wide Herdr terminal. Returns {alive} so a caller
// can tell the entry was reaped (and re-attach via /api/grid/term if still visible).
func serveGridTermTouch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host       string `json:"host"`
		TerminalID string `json:"terminal_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Host == "" || !terminalIDRe.MatchString(req.TerminalID) {
		http.Error(w, "host and a valid terminal_id required", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"alive": touchGridTerm(req.Host, req.TerminalID)})
}

// touchGridTerm bumps lastUsed for a live grid terminal and reports whether it
// existed. It never creates one, so a keepalive can't undo a release.
func touchGridTerm(host, terminalID string) bool {
	key := host + "|" + terminalID
	gridTerms.mu.Lock()
	defer gridTerms.mu.Unlock()
	if e := gridTerms.byKey[key]; e != nil {
		e.lastUsed = time.Now()
		return true
	}
	return false
}

// serveGridTermReleaseAll tears down every live grid terminal at once. The frontend
// calls it when the Grid view is left, as an authoritative backstop: even if a
// per-cell release was dropped (best-effort, fire-and-forget) or raced a keepalive,
// no thin grid attach survives to clamp a pane while it's viewed full-size in Herdr.
// Note: grid ttyds are a server-wide pool, so this also drops cells in any other
// browser viewing the Grid — consistent with the global-keyed per-cell release.
func serveGridTermReleaseAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	releaseAllGridTerms()
	writeJSON(w, map[string]any{"ok": true})
}

// releaseGridTermsForHost kills every grid ttyd attached to one host's panes.
// Remote cells stream over the host backend's SSH master, so they can't outlive
// it: callers tearing down or replacing a host's connection (host switch, pool
// evict) release its cells first, and the frontend keepalive (alive=false)
// re-attaches visible ones over the replacement connection.
func releaseGridTermsForHost(host string) {
	prefix := host + "|"
	var dead []*gridTermEntry
	gridTerms.mu.Lock()
	for key, e := range gridTerms.byKey {
		if strings.HasPrefix(key, prefix) {
			delete(gridTerms.byKey, key)
			delete(gridTerms.byToken, e.token)
			dead = append(dead, e)
		}
	}
	gridTerms.mu.Unlock()
	for _, e := range dead {
		e.cancel() // SIGTERMs the ttyd process group; its socket is unlinked on exit
	}
}

// releaseAllGridTerms kills every grid ttyd, detaching all of them from herdr so no
// pane is held to a grid cell's width.
func releaseAllGridTerms() {
	gridTerms.mu.Lock()
	dead := make([]*gridTermEntry, 0, len(gridTerms.byKey))
	for _, e := range gridTerms.byKey {
		dead = append(dead, e)
	}
	gridTerms.byKey = map[string]*gridTermEntry{}
	gridTerms.byToken = map[string]*gridTermEntry{}
	gridTerms.mu.Unlock()
	for _, e := range dead {
		e.cancel() // SIGTERMs the ttyd process group; its socket is unlinked on exit
	}
}

// gridAttachCmd builds the argv and env for the ttyd that attaches one grid
// cell. herdr's `terminal attach` always talks to a *local* socket pair (and
// `--remote` only works with the default launch command, not subcommands), so
// a remote pane used to be attached by SSHing into its host and running herdr
// there — but that gave every visible cell a full SSH connection of its own,
// and (together with the control masters `herdr --remote` leaks, see
// reapOrphanHerdrSSH) enough of them saturated the remote sshd into resetting
// new handshakes. Instead the *local* herdr binary now attaches through the
// host backend's already-forwarded socket pair: HERDR_SOCKET_PATH points at
// the forwarded RPC socket and HERDR_CLIENT_SOCKET_PATH at the forwarded
// streaming socket, so the pty bytes multiplex over the one SSH control master
// lasso already holds for the host — no new SSH connection, ever. A nil env
// means "inherit lasso's environment" (the local case, unchanged).
func gridAttachCmd(host, terminalID string) (argv, env []string, err error) {
	argv = append(strings.Fields(herdrBinary()), "terminal", "attach", terminalID)
	if host == "local" {
		return argv, nil, nil
	}
	b, err := gridHostBackend(host)
	if err != nil {
		return nil, nil, err
	}
	rb, ok := b.(*remoteBackend)
	if !ok {
		return nil, nil, fmt.Errorf("host %s has no remote connection", host)
	}
	return argv, gridAttachEnv(outsideHerdrEnv(), rb.localSock, rb.localClientSock), nil
}

// gridAttachEnv overlays the herdr socket-path variables onto base, dropping
// any existing values (a nested lasso inherits HERDR_SOCKET_PATH pointing at
// the local server — the attach must see only the forwarded pair).
func gridAttachEnv(base []string, sock, clientSock string) []string {
	env := make([]string, 0, len(base)+2)
	for _, kv := range base {
		if strings.HasPrefix(kv, "HERDR_SOCKET_PATH=") || strings.HasPrefix(kv, "HERDR_CLIENT_SOCKET_PATH=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HERDR_SOCKET_PATH="+sock,
		"HERDR_CLIENT_SOCKET_PATH="+clientSock,
	)
}

// ensureGridTerm returns the proxy base path for host's terminal, spawning a
// dedicated ttyd (running `herdr terminal attach …`) on first use. A repeat call
// just bumps lastUsed — so the frontend re-POSTs as a keepalive.
func ensureGridTerm(host, terminalID string) (string, error) {
	key := host + "|" + terminalID
	gridTerms.mu.Lock()
	if e := gridTerms.byKey[key]; e != nil {
		e.lastUsed = time.Now()
		gridTerms.mu.Unlock()
		return e.base, nil
	}
	gridTerms.mu.Unlock()

	// Resolve the attach command outside the lock: a remote host may need a
	// pool backend dialed (SSH), which must not stall touch/proxy/release.
	argv, env, err := gridAttachCmd(host, terminalID)
	if err != nil {
		return "", err
	}

	gridTerms.mu.Lock()
	defer gridTerms.mu.Unlock()
	if gridTerms.byKey == nil {
		gridTerms.byKey = map[string]*gridTermEntry{}
		gridTerms.byToken = map[string]*gridTermEntry{}
	}
	// Re-check: a concurrent request may have attached while we resolved.
	if e := gridTerms.byKey[key]; e != nil {
		e.lastUsed = time.Now()
		return e.base, nil
	}
	if len(gridTerms.byKey) >= gridTermMax {
		reapGridTermsLocked() // make room for genuinely-active cells
		if len(gridTerms.byKey) >= gridTermMax {
			return "", fmt.Errorf("too many live grid terminals (max %d)", gridTermMax)
		}
	}

	var tok [9]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(tok[:])
	sock := gridTermSock(token)
	basePath := "/grid-term/" + token

	ctx, cancel := context.WithCancel(srvCtx)
	if err := startTtydArgv(ctx, sock, basePath, argv, env); err != nil {
		cancel()
		return "", err
	}
	waitSocket(sock, true, 3*time.Second)

	e := &gridTermEntry{
		token:    token,
		sock:     sock,
		base:     basePath + "/",
		proxy:    unixSocketProxy(sock),
		cancel:   cancel,
		lastUsed: time.Now(),
	}
	gridTerms.byKey[key] = e
	gridTerms.byToken[token] = e
	startGridReaper()
	return e.base, nil
}

// serveGridTermProxy routes /grid-term/<token>/… to that pane's ttyd (HTTP +
// WebSocket, via the same unix-socket reverse proxy the fixed terminals use).
func serveGridTermProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/grid-term/")
	token := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		token = rest[:i]
	}
	gridTerms.mu.Lock()
	e := gridTerms.byToken[token]
	if e != nil {
		e.lastUsed = time.Now() // proxied traffic keeps an actively-viewed cell alive
	}
	gridTerms.mu.Unlock()
	if e == nil {
		http.NotFound(w, r)
		return
	}
	e.proxy.ServeHTTP(w, r)
}

func reapGridTerms() {
	gridTerms.mu.Lock()
	reapGridTermsLocked()
	gridTerms.mu.Unlock()
}

func reapGridTermsLocked() {
	now := time.Now()
	for key, e := range gridTerms.byKey {
		if now.Sub(e.lastUsed) > gridTermIdle {
			e.cancel() // SIGTERMs the ttyd process group; its socket is unlinked on exit
			delete(gridTerms.byKey, key)
			delete(gridTerms.byToken, e.token)
		}
	}
}

// ---------------------------------------------------------------------------
// shared reaper
// ---------------------------------------------------------------------------

var gridReaperOnce sync.Once

// startGridReaper launches (once) a goroutine that idle-reaps both pools so a
// closed Grid tab doesn't leave ttyds or SSH masters running.
func startGridReaper() {
	gridReaperOnce.Do(func() {
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-srvCtx.Done():
					return
				case <-t.C:
					reapGridTerms()
					reapGridBackends()
				}
			}
		}()
	})
}
