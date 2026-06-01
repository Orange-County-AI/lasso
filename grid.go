package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
// ttyds running `herdr terminal attach <terminal_id>` (or `herdr --remote <host>
// terminal attach …`), pooled and reaped on idle.

// ---------------------------------------------------------------------------
// grid backend pool — herdr RPC to any compatible host without switching active
// ---------------------------------------------------------------------------

// gridPoolEntry is a cached remote backend used only for herdr RPC (no SFTP — it
// stays lazy and never opens). lastUsed drives idle reaping so connections (and
// their SSH control masters) don't linger after the Grid tab is closed.
type gridPoolEntry struct {
	backend  *remoteBackend
	lastUsed time.Time
}

var gridPool struct {
	mu      sync.Mutex
	entries map[string]*gridPoolEntry // keyed by ssh-config alias
}

const gridBackendIdle = 90 * time.Second

// gridHostBackend returns a Backend for RPC against host without disturbing the
// active backend: "local" → a localBackend on the local socket; the active host →
// the live active backend (reuses its already-forwarded socket, no new SSH); any
// other compatible remote → a lazily-created, idle-reaped remoteBackend.
func gridHostBackend(host string) (Backend, error) {
	if host == "local" {
		return &localBackend{sock: *herdrSock}, nil
	}
	if cur := curBackend(); host == cur.Name() {
		return cur, nil
	}
	gridPool.mu.Lock()
	defer gridPool.mu.Unlock()
	if gridPool.entries == nil {
		gridPool.entries = map[string]*gridPoolEntry{}
	}
	if e := gridPool.entries[host]; e != nil {
		e.lastUsed = time.Now()
		return e.backend, nil
	}
	hi, ok := findHost(host)
	if !ok || !hi.Reachable || !hi.Running || !hi.Compatible {
		return nil, fmt.Errorf("host %s not available", host)
	}
	_, wantProto := localProtocol()
	rb, err := newRemoteBackend(srvCtx, host, hi.Socket, wantProto, "grid")
	if err != nil {
		return nil, err
	}
	gridPool.entries[host] = &gridPoolEntry{backend: rb, lastUsed: time.Now()}
	startGridReaper()
	return rb, nil
}

// gridPoolEvict closes and drops any pooled backend for host. Called when the
// active host switches to host (the active backend now serves it) so we don't
// keep a redundant second connection.
func gridPoolEvict(host string) {
	gridPool.mu.Lock()
	e := gridPool.entries[host]
	delete(gridPool.entries, host)
	gridPool.mu.Unlock()
	if e != nil {
		go e.backend.Close()
	}
}

func reapGridBackends() {
	now := time.Now()
	var dead []*remoteBackend
	gridPool.mu.Lock()
	for host, e := range gridPool.entries {
		if now.Sub(e.lastUsed) > gridBackendIdle {
			dead = append(dead, e.backend)
			delete(gridPool.entries, host)
		}
	}
	gridPool.mu.Unlock()
	for _, b := range dead {
		_ = b.Close()
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
	Cwd            string `json:"cwd"`
	Agent          string `json:"agent"`
	AgentStatus    string `json:"agent_status"`
	HasAgent       bool   `json:"has_agent"`
	Focused        bool   `json:"focused"`
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

// fetchGridPanes queries every compatible host concurrently and merges their
// panes. A host that can't be listed lands in Errors rather than failing the
// whole grid. Local is always included; remotes come from the (cached) host
// discovery probe.
func fetchGridPanes(ctx context.Context) gridPayload {
	targets := []gridTarget{{host: "local", label: localHostname()}}
	for _, hi := range discoverHosts(ctx, false) {
		if hi.Reachable && hi.Running && hi.Compatible {
			targets = append(targets, gridTarget{host: hi.Alias, label: hi.Alias})
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
			continue
		}
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

	out := make([]gridPane, 0, len(pl.Panes))
	for _, p := range pl.Panes {
		kind, isAgent := agentKind[p.PaneID]
		out = append(out, gridPane{
			Host:           host,
			HostLabel:      hostLabel,
			PaneID:         p.PaneID,
			TerminalID:     p.TerminalID,
			WorkspaceID:    p.WorkspaceID,
			WorkspaceLabel: wss[p.WorkspaceID].label,
			TabID:          p.TabID,
			TabLabel:       tabs[p.TabID].label,
			Cwd:            paneCwd(p),
			Agent:          kind,
			AgentStatus:    p.AgentStatus,
			HasAgent:       isAgent,
			Focused:        p.Focused,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if wi, wj := wss[out[i].WorkspaceID].number, wss[out[j].WorkspaceID].number; wi != wj {
			return wi < wj
		}
		if ti, tj := tabs[out[i].TabID].number, tabs[out[j].TabID].number; ti != tj {
			return ti < tj
		}
		return out[i].PaneID < out[j].PaneID
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
	gridTermIdle = 30 * time.Second
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
	// Only attach on a host we can actually reach: local, the active host, or a
	// discovered compatible remote. Guards against a bogus alias shelling out.
	if req.Host != "local" && req.Host != curBackend().Name() {
		if hi, ok := findHost(req.Host); !ok || !hi.Reachable || !hi.Running || !hi.Compatible {
			http.Error(w, "host not available", http.StatusBadRequest)
			return
		}
	}
	base, err := ensureGridTerm(req.Host, req.TerminalID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"base": base})
}

// ensureGridTerm returns the proxy base path for host's terminal, spawning a
// dedicated ttyd (running `herdr terminal attach …`) on first use. A repeat call
// just bumps lastUsed — so the frontend re-POSTs as a keepalive.
func ensureGridTerm(host, terminalID string) (string, error) {
	key := host + "|" + terminalID
	gridTerms.mu.Lock()
	defer gridTerms.mu.Unlock()
	if gridTerms.byKey == nil {
		gridTerms.byKey = map[string]*gridTermEntry{}
		gridTerms.byToken = map[string]*gridTermEntry{}
	}
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

	// Build the attach argv. herdr's `terminal attach` always talks to a *local*
	// herdr socket (and `--remote` only works with the default launch command, not
	// subcommands), so a remote pane is attached by SSHing into its host and
	// running herdr there — through a login shell so ~/.local/bin is on PATH and
	// with `-tt` so the attach TUI gets a remote PTY, mirroring how host discovery
	// probes hosts. The remote command must reach ssh as a single argument, which
	// is why this uses the explicit-argv ttyd spawn rather than the whitespace-
	// split one.
	var argv []string
	if host == "local" {
		argv = append(strings.Fields(herdrBinary()), "terminal", "attach", terminalID)
	} else {
		argv = []string{
			"ssh", "-tt",
			"-o", "BatchMode=yes",
			"-o", "ConnectTimeout=8",
			"-o", "StrictHostKeyChecking=accept-new",
			host,
			"${SHELL:-sh} -lc " + shellQuote("herdr terminal attach "+terminalID),
		}
	}

	ctx, cancel := context.WithCancel(srvCtx)
	if err := startTtydArgv(ctx, sock, basePath, argv, nil); err != nil {
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
