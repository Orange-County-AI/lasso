package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The grid: a live wall of every terminal across every host. It aggregates each
// host's live tabs (from the central DB, gated on a live tmux session) into a flat
// list the React GridTab renders, filters (by host / agents-only), and drives.
//
// Unlike the file/diff handlers (which target the ACTIVE host), the grid spans all
// reachable hosts at once. Per host the only remote call is one `tmux
// list-sessions` for liveness; everything else (workspaces, tabs, agent kind +
// prompt) comes from the central DB, and live status from the poller cache.

// gridPane is one live tab on one host. lasso's terminal granularity is the tab's
// tmux session (lasso_<tab_id>), so a "pane" maps 1:1 to a tab.
type gridPane struct {
	Host           string `json:"host"`       // "local" or ssh alias (the attach key)
	HostLabel      string `json:"host_label"` // display name
	TabID          string `json:"tab_id"`     // also the attach handle (session lasso_<tab_id>)
	WorkspaceID    string `json:"workspace_id"`
	WorkspaceLabel string `json:"workspace_label"`
	TabLabel       string `json:"tab_label"`
	Cwd            string `json:"cwd"`
	Agent          string `json:"agent,omitempty"`        // claude|codex when an agent runs here
	AgentStatus    string `json:"agent_status,omitempty"` // idle|working|blocked|unknown
	HasAgent       bool   `json:"has_agent"`
	Prompt         string `json:"prompt,omitempty"` // the agent's initial prompt (⌘K/grid search)
	Git            bool   `json:"git"`              // pane lives in a git checkout (else no status dot)
	Dirty          int    `json:"dirty,omitempty"`  // working-tree changes (git status lines); 0 = clean
}

type gridPayload struct {
	Panes  []gridPane        `json:"panes"`
	Errors map[string]string `json:"errors,omitempty"` // host → why it couldn't be reached
}

// --- aggregation cache (coalesce overlapping fetches) --------------------------

const gridCacheTTL = 1500 * time.Millisecond

var gridCache struct {
	mu       sync.Mutex
	at       time.Time
	payload  gridPayload
	inflight bool
	done     chan struct{}
}

// serveGrid (GET /api/grid) returns the cross-host pane list, coalescing rapid
// polls behind a short cache so the UI's refetch loop doesn't hammer every host.
func serveGrid(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, gridPanes())
}

func gridPanes() gridPayload {
	gridCache.mu.Lock()
	if !gridCache.at.IsZero() && time.Since(gridCache.at) < gridCacheTTL {
		p := gridCache.payload
		gridCache.mu.Unlock()
		return p
	}
	if gridCache.inflight {
		done := gridCache.done
		gridCache.mu.Unlock()
		<-done // a concurrent fetch is running — wait for it and reuse its result
		gridCache.mu.Lock()
		p := gridCache.payload
		gridCache.mu.Unlock()
		return p
	}
	gridCache.inflight = true
	gridCache.done = make(chan struct{})
	done := gridCache.done
	gridCache.mu.Unlock()

	p := fetchGridPanes()

	gridCache.mu.Lock()
	gridCache.payload = p
	gridCache.at = time.Now()
	gridCache.inflight = false
	gridCache.mu.Unlock()
	close(done)
	return p
}

// invalidateGridCache forces the next gridPanes to re-aggregate (after a grid
// mutation: rename/close).
func invalidateGridCache() {
	gridCache.mu.Lock()
	gridCache.at = time.Time{}
	gridCache.mu.Unlock()
}

// fetchGridPanes aggregates panes from the local host plus every reachable,
// tmux-capable ssh host, concurrently.
func fetchGridPanes() gridPayload {
	targets := usableHostTargets()

	out := gridPayload{Errors: map[string]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for _, t := range targets {
		wg.Add(1)
		go func(t hostTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			panes, err := gridHostPanes(t.host, t.label)
			mu.Lock()
			if err != nil {
				out.Errors[hostOrLocal(t.host)] = err.Error()
			}
			out.Panes = append(out.Panes, panes...)
			mu.Unlock()
		}(t)
	}
	wg.Wait()

	// Newest-first by tab id (base36 of UnixNano → lexicographic ~ chronological
	// for same-length ids; fall back to host for stability).
	sort.SliceStable(out.Panes, func(i, j int) bool {
		if out.Panes[i].TabID != out.Panes[j].TabID {
			return out.Panes[i].TabID > out.Panes[j].TabID
		}
		return out.Panes[i].Host < out.Panes[j].Host
	})
	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	return out
}

// gridHostPanes builds one host's live panes from the central DB, gated on a live
// tmux session. A remote host that's unreachable returns an error (surfaced in the
// payload's Errors) rather than silently contributing nothing.
func gridHostPanes(host, label string) ([]gridPane, error) {
	be, err := hostBackend(host)
	if err != nil {
		return nil, err
	}
	wss, err := listWorkspaces(host)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, s := range tmuxListSessionsOn(host) {
		live[s] = true
	}
	kinds := agentKindsForHost(host)
	statuses := agentStatuses.snapshot()
	agentByID := map[string]AgentRecord{}
	if recs, err := listAgents(host); err == nil {
		for _, rec := range recs {
			agentByID[rec.ID] = rec
		}
	}

	var panes []gridPane
	for _, ws := range wss {
		// One `git status` per git workspace (not per tab), shared across its
		// tabs' cells. Cached behind gridCacheTTL so the UI's poll doesn't re-shell.
		dirty, isGit := workspaceGitStatus(be, ws)
		tabs, _ := listTabs(ws.ID)
		for _, t := range tabs {
			session := tabSession(t.ID)
			if !live[session] {
				continue // session not running (reboot leftover / unreachable)
			}
			setSessionHost(session, host) // so grid attach + status route correctly
			p := gridPane{
				Host: hostOrLocal(host), HostLabel: label, TabID: t.ID,
				WorkspaceID: ws.ID, WorkspaceLabel: ws.Title, TabLabel: t.Title, Cwd: t.Cwd,
				Git: isGit, Dirty: dirty,
			}
			if k := kinds[t.ID]; k != "" {
				p.HasAgent = true
				p.Agent = k
				p.AgentStatus = statuses[statusKey(host, t.ID)]
				if p.AgentStatus == "" {
					p.AgentStatus = string(StatusIdle)
				}
				if rec, ok := agentByID[t.AgentID]; ok {
					p.Prompt = rec.Description
				}
			}
			panes = append(panes, p)
		}
	}
	return panes, nil
}

// workspaceGitStatus reports whether a workspace is a git checkout and, if so,
// how many entries its working tree shows dirty (count of `git status --porcelain`
// lines; 0 = clean). A non-git (scratch) workspace returns isGit=false so the UI
// shows no status dot. A git workspace whose status errors is reported as git +
// clean rather than dropped, so a transient git hiccup doesn't blink the dot off.
func workspaceGitStatus(be Backend, ws Workspace) (dirty int, isGit bool) {
	if ws.Kind != "git" || ws.WorkDir == "" {
		return 0, false
	}
	out, err := be.GitOut(ws.WorkDir, "status", "--porcelain")
	if err != nil {
		return 0, true
	}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return 0, true
	}
	return strings.Count(out, "\n") + 1, true
}

// --- grid mutations ------------------------------------------------------------

// serveGridRename (POST {host, workspace_id, label}) renames a workspace. State is
// central, so this is a plain DB update regardless of host.
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
	if strings.TrimSpace(req.Label) == "" || req.WorkspaceID == "" {
		http.Error(w, "workspace_id and label required", http.StatusBadRequest)
		return
	}
	if err := renameWorkspaceSynced(req.WorkspaceID, strings.TrimSpace(req.Label)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	invalidateGridCache()
	kickHub()
	writeJSON(w, map[string]any{"ok": true})
}

// serveGridClose (POST {panes:[{host, tab_id}]}) closes the given tabs (killing
// their sessions, host-aware), and any workspace left with no live tabs.
func serveGridClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Panes []struct {
			Host  string `json:"host"`
			TabID string `json:"tab_id"`
		} `json:"panes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, p := range req.Panes {
		setSessionHost(tabSession(p.TabID), p.Host)
		t, err := getTab(p.TabID)
		closeOneTab(p.TabID)
		releaseGridTerm(p.Host, p.TabID)
		if err == nil {
			if rest, _ := listTabs(t.WorkspaceID); len(rest) == 0 {
				_ = closeWorkspace(t.WorkspaceID)
			}
		}
	}
	invalidateGridCache()
	kickHub()
	writeJSON(w, map[string]any{"ok": true})
}

// --- per-pane ttyd pool --------------------------------------------------------
//
// Each grid cell embeds a real terminal: a dedicated ttyd that attaches the
// pane's tmux session (locally, or via `ssh -tt` for a remote host), proxied under
// /grid-term/<token>/. Spawned on first view (IntersectionObserver in the UI),
// released when hidden, idle-reaped as a backstop. Because tmux uses
// `window-size latest`, a grid cell's small client DOES clamp the shared
// window while attached — that's fine in grid mode (the main viewport is
// hidden), and on grid exit the UI both releases the cells' clients and kicks
// the viewport's size (kickTerminalSize) so the window snaps back to it.

const gridTermIdle = 60 * time.Second

type gridTermEntry struct {
	token    string
	sock     string
	base     string
	proxy    *httputil.ReverseProxy
	cancel   context.CancelFunc
	lastUsed time.Time
}

var gridTerms struct {
	mu     sync.Mutex
	m      map[string]*gridTermEntry // keyed by "host|tabID"
	reaper bool
}

func gridTermKey(host, tabID string) string { return hostOrLocal(host) + "|" + tabID }

// serveGridTerm (POST {host, tab_id}) ensures a ttyd attached to that pane and
// returns its proxy base path.
func serveGridTerm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host  string `json:"host"`
		TabID string `json:"tab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !tabIDRe.MatchString(req.TabID) {
		http.Error(w, "valid tab_id required", http.StatusBadRequest)
		return
	}
	base, err := ensureGridTerm(req.Host, req.TabID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"base": base})
}

func ensureGridTerm(host, tabID string) (string, error) {
	key := gridTermKey(host, tabID)
	gridTerms.mu.Lock()
	defer gridTerms.mu.Unlock()
	if gridTerms.m == nil {
		gridTerms.m = map[string]*gridTermEntry{}
	}
	if e := gridTerms.m[key]; e != nil {
		if _, err := os.Stat(e.sock); err == nil {
			e.lastUsed = time.Now()
			return e.base, nil
		}
		if e.cancel != nil {
			e.cancel()
		}
		delete(gridTerms.m, key)
	}

	session := tabSession(tabID)
	setSessionHost(session, host)
	be, err := hostBackend(host)
	if err != nil {
		return "", err
	}

	var tok [9]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(tok[:])
	sock := filepath.Join(os.TempDir(), "lasso-grid-"+token+".sock")
	basePath := "/grid-term/" + token

	ctx, cancel := context.WithCancel(srvCtx)
	if err := startTtydArgv(ctx, sock, basePath, be.TmuxAttachArgv(session), nil); err != nil {
		cancel()
		return "", err
	}
	waitSocket(sock, true, 3*time.Second)

	gridTerms.m[key] = &gridTermEntry{
		token: token, sock: sock, base: basePath + "/",
		proxy: unixSocketProxy(sock), cancel: cancel, lastUsed: time.Now(),
	}
	if !gridTerms.reaper {
		gridTerms.reaper = true
		go gridTermReaper()
	}
	return gridTerms.m[key].base, nil
}

// releaseGridTerm tears down a pane's ttyd (on close).
func releaseGridTerm(host, tabID string) {
	key := gridTermKey(host, tabID)
	gridTerms.mu.Lock()
	e := gridTerms.m[key]
	delete(gridTerms.m, key)
	gridTerms.mu.Unlock()
	if e != nil && e.cancel != nil {
		e.cancel()
	}
}

// serveGridTermTouch (POST {host, tab_id}) keeps a cell's ttyd alive while it's
// on screen, so the reaper doesn't reclaim a visible terminal.
func serveGridTermTouch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host  string `json:"host"`
		TabID string `json:"tab_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	key := gridTermKey(req.Host, req.TabID)
	gridTerms.mu.Lock()
	alive := false
	if e := gridTerms.m[key]; e != nil {
		e.lastUsed = time.Now()
		alive = true
	}
	gridTerms.mu.Unlock()
	writeJSON(w, map[string]any{"alive": alive})
}

// serveGridTermRelease (POST {host, tab_id}) tears down a cell's ttyd when it
// scrolls out of view / the grid tab is left.
func serveGridTermRelease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host  string `json:"host"`
		TabID string `json:"tab_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	releaseGridTerm(req.Host, req.TabID)
	writeJSON(w, map[string]any{"ok": true})
}

func gridTermReaper() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-srvCtx.Done():
			return
		case <-t.C:
		}
		var dead []*gridTermEntry
		gridTerms.mu.Lock()
		for key, e := range gridTerms.m {
			if time.Since(e.lastUsed) > gridTermIdle {
				dead = append(dead, e)
				delete(gridTerms.m, key)
			}
		}
		gridTerms.mu.Unlock()
		for _, e := range dead {
			if e.cancel != nil {
				e.cancel()
			}
		}
	}
}

// serveGridTermProxy routes /grid-term/<token>/… to the matching pane ttyd.
func serveGridTermProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/grid-term/")
	token := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		token = rest[:i]
	}
	var proxy *httputil.ReverseProxy
	gridTerms.mu.Lock()
	for _, e := range gridTerms.m {
		if e.token == token {
			proxy = e.proxy
			break
		}
	}
	gridTerms.mu.Unlock()
	if proxy == nil {
		http.NotFound(w, r)
		return
	}
	proxy.ServeHTTP(w, r)
}
