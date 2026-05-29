// Command ttyd-iframe-demo serves a two-column web UI:
//
//	left  = herdr running inside a ttyd terminal (embedded in an iframe)
//	right = a file viewer that follows herdr's *focused pane* cwd, live
//
// It talks to the herdr server over its newline-delimited JSON unix socket
// (subscribe to focus events + poll pane.list for cwd changes) and pushes
// active-pane updates to the browser over SSE.
//
// Everything binds to loopback by default: the left pane is a writable shell,
// so this is NOT meant to be exposed to a network without deliberate thought.
package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var staticFS embed.FS

var (
	listenAddr  = flag.String("listen", "127.0.0.1:8090", "address for the web server (loopback by default — the terminal is a writable shell)")
	ttydPort    = flag.Int("ttyd-port", 7682, "loopback port ttyd listens on")
	herdrSock   = flag.String("herdr-sock", defaultSock(), "path to the herdr unix socket")
	termCmd     = flag.String("term-cmd", "herdr", "command ttyd runs in the terminal")
	spawnTtyd   = flag.Bool("spawn-ttyd", true, "spawn and supervise ttyd as a child process")
	pollEvery   = flag.Duration("poll", 2*time.Second, "fallback poll interval for cwd changes")
	allowNoAuth = flag.Bool("insecure-no-auth", false, "permit a non-loopback bind without auth (tailnet-only use; never on a public interface)")
	procCwd     = flag.Bool("proc-cwd", true, "for agent panes, recover the real cwd from the agent process via /proc (herdr reports the stale shell cwd)")
	themeName   = flag.String("theme", "auto", "color theme: \"auto\" reads herdr's config.toml, or force one of catppuccin/tokyo-night/dracula/nord/gruvbox/one-dark/solarized/kanagawa/rose-pine/vesper/terminal")
)

// theme is resolved once at startup (mirroring herdr's config) and drives both
// the embedded terminal's palette and the sidebar CSS.
var theme resolvedTheme

func defaultSock() string {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "herdr", "herdr.sock")
}

func main() {
	flag.Parse()

	// Auth credentials come from the environment (UI_AUTH=user:pass), never
	// argv — so they don't leak via `ps`. Safety guard: refuse to bind to a
	// non-loopback address without auth, so this can't accidentally expose a
	// writable shell on a public interface again.
	authUser, authPass, hasAuth := parseAuth(os.Getenv("UI_AUTH"))
	if !isLoopback(*listenAddr) && !hasAuth && !*allowNoAuth {
		log.Fatalf("refusing to listen on non-loopback %q without auth — set UI_AUTH=user:pass, "+
			"or pass -insecure-no-auth to bind bare (only safe on a private interface like tailscale0)", *listenAddr)
	}

	theme = loadHerdrTheme(*themeName)
	if theme.Customized {
		log.Printf("theme:    %q -> %s (+custom overrides)", theme.Name, theme.Resolved)
	} else {
		log.Printf("theme:    %q -> %s", theme.Name, theme.Resolved)
	}
	if err := renderIndex(); err != nil {
		log.Fatalf("render index: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *spawnTtyd {
		if err := startTtyd(ctx); err != nil {
			log.Fatalf("ttyd: %v", err)
		}
	}

	hub := newHub()
	go hub.run(ctx)

	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", *ttydPort))
	proxy := httputil.NewSingleHostReverseProxy(target) // handles WS upgrade natively

	mux := http.NewServeMux()
	mux.Handle("/terminal/", proxy)
	mux.HandleFunc("/api/active", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, hub.snapshot())
	})
	mux.HandleFunc("/api/events", hub.serveSSE)
	mux.HandleFunc("/api/files", serveFiles)
	mux.HandleFunc("/api/file", serveFile)
	mux.HandleFunc("/api/panes", servePanes)
	mux.HandleFunc("/api/focus", serveFocus)
	mux.HandleFunc("/api/rename", serveRename)
	mux.HandleFunc("/api/close", serveClose)
	mux.HandleFunc("/api/diff", serveDiff)
	mux.HandleFunc("/", serveIndex)

	handler := withAuth(mux, authUser, authPass, hasAuth)
	srv := &http.Server{Addr: *listenAddr, Handler: handler}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()

	switch {
	case hasAuth:
		log.Printf("auth:     enabled (basic, user %q)", authUser)
	case !isLoopback(*listenAddr):
		log.Printf("auth:     DISABLED on non-loopback %s (-insecure-no-auth) — relies on the network being private", *listenAddr)
	default:
		log.Printf("auth:     DISABLED (loopback only)")
	}
	log.Printf("UI:       http://%s", *listenAddr)
	log.Printf("terminal: ttyd@127.0.0.1:%d running %q (proxied at /terminal/)", *ttydPort, *termCmd)
	log.Printf("herdr:    %s", *herdrSock)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// ttyd child process
// ---------------------------------------------------------------------------

func startTtyd(ctx context.Context) error {
	// The xterm.js ITheme (background/foreground/cursor + 16 ANSI colors) is
	// derived from herdr's selected theme, so the terminal palette lines up
	// with herdr's chrome and the sidebar. Passed to ttyd via `-t theme=<json>`,
	// which forwards it to xterm.js in the browser.
	args := []string{
		"-i", "lo", // loopback only
		"-p", fmt.Sprint(*ttydPort),
		"-b", "/terminal", // base path so assets/ws resolve under the proxy
		"-W",                           // writable
		"-t", "disableLeaveAlert=true", // no confirm dialog inside the iframe
		"-t", "fontSize=13",
		"-t", "theme=" + theme.xtermJSON(),
	}
	args = append(args, strings.Fields(*termCmd)...)
	cmd := exec.Command("ttyd", args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own process group so we can kill cleanly
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("spawned ttyd (pid %d)", cmd.Process.Pid)
	go func() {
		<-ctx.Done()
		// kill the whole process group (ttyd + the shell it spawned)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}()
	go func() { _ = cmd.Wait() }()
	return nil
}

// ---------------------------------------------------------------------------
// herdr socket client
// ---------------------------------------------------------------------------

// herdrCall does one request/response round-trip on a fresh connection.
func herdrCall(method string, params any) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", *herdrSock, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	req := map[string]any{"id": "ui", "method": method, "params": params}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("herdr error: %s", resp.Error)
	}
	return resp.Result, nil
}

type pane struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	Cwd         string `json:"cwd"`
	Focused     bool   `json:"focused"`
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
}

type workspace struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
}

// Active is the state pushed to the browser.
type Active struct {
	PaneID         string `json:"pane_id"`
	Cwd            string `json:"cwd"`
	CwdSource      string `json:"cwd_source"` // "shell" (herdr, reliable) | "process" (/proc, enriched) | "stale" (agent, couldn't resolve)
	WorkspaceID    string `json:"workspace_id"`
	WorkspaceLabel string `json:"workspace_label"`
	TabID          string `json:"tab_id"`
	TabLabel       string `json:"tab_label"`
	Agent          string `json:"agent"`
	AgentStatus    string `json:"agent_status"`
}

func fetchActive() (Active, error) {
	res, err := herdrCall("pane.list", map[string]any{})
	if err != nil {
		return Active{}, err
	}
	var pl struct {
		Panes []pane `json:"panes"`
	}
	if err := json.Unmarshal(res, &pl); err != nil {
		return Active{}, err
	}
	var fp *pane
	for i := range pl.Panes {
		if pl.Panes[i].Focused {
			fp = &pl.Panes[i]
			break
		}
	}
	if fp == nil {
		return Active{}, fmt.Errorf("no focused pane")
	}
	a := Active{
		PaneID: fp.PaneID, Cwd: fp.Cwd, CwdSource: "shell", WorkspaceID: fp.WorkspaceID,
		TabID: fp.TabID, Agent: fp.Agent, AgentStatus: fp.AgentStatus,
	}
	a.TabLabel = tabLabel(fp.TabID)
	// resolve workspace label (best effort)
	if res, err := herdrCall("workspace.list", map[string]any{}); err == nil {
		var wl struct {
			Workspaces []workspace `json:"workspaces"`
		}
		if json.Unmarshal(res, &wl) == nil {
			for _, w := range wl.Workspaces {
				if w.WorkspaceID == a.WorkspaceID {
					a.WorkspaceLabel = w.Label
				}
			}
		}
	}
	// herdr's `cwd` is the shell's launch dir — stale once an agent owns the
	// pane. For agent panes, recover the real cwd from the agent process.
	if fp.Agent != "" {
		a.CwdSource = "stale"
		if *procCwd {
			if real, ok := agentRealCwd(fp.Agent, a.TabLabel); ok {
				a.Cwd, a.CwdSource = real, "process"
			}
		}
	}
	return a, nil
}

// tabLabel fetches a tab's display label (best effort, "" on failure).
func tabLabel(tabID string) string {
	res, err := herdrCall("tab.get", map[string]any{"tab_id": tabID})
	if err != nil {
		return ""
	}
	var r struct {
		Tab struct {
			Label string `json:"label"`
		} `json:"tab"`
	}
	if json.Unmarshal(res, &r) != nil {
		return ""
	}
	return r.Tab.Label
}

// agentRealCwd recovers an agent pane's true working directory: it matches the
// herdr tab label against the command line of a running agent process (whose
// first non-flag arg is the task title) and returns that process's
// /proc/<pid>/cwd. Returns ok=false if there's no unambiguous match, so the
// caller can fall back to herdr's (stale) value rather than show a wrong dir.
func agentRealCwd(agent, label string) (string, bool) {
	label = strings.TrimRight(strings.TrimSpace(label), "….") // drop truncation ellipsis/dots
	label = strings.TrimSpace(label)
	if agent == "" || len(label) < 4 { // too-short labels invite false matches
		return "", false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return "", false
	}
	cwds := map[string]struct{}{}
	for _, e := range entries {
		name := e.Name()
		if name[0] < '0' || name[0] > '9' {
			continue
		}
		raw, err := os.ReadFile("/proc/" + name + "/cmdline")
		if err != nil || len(raw) == 0 {
			continue
		}
		argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if !strings.Contains(argv[0], agent) { // e.g. ".../bin/claude"
			continue
		}
		var title string
		for _, arg := range argv[1:] {
			if !strings.HasPrefix(arg, "-") {
				title = arg
				break
			}
		}
		if title == "" || !strings.HasPrefix(title, label) {
			continue
		}
		cwd, err := os.Readlink("/proc/" + name + "/cwd")
		if err != nil {
			continue
		}
		if fi, err := os.Stat(cwd); err == nil && fi.IsDir() {
			cwds[cwd] = struct{}{}
		}
	}
	if len(cwds) == 1 { // unambiguous
		for c := range cwds {
			return c, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// pane grid: list every pane + focus one
// ---------------------------------------------------------------------------

// paneView is a herdr pane enriched with its workspace/tab labels (and ordering
// numbers, used server-side for sorting) for display in the right column's grid.
type paneView struct {
	PaneID         string `json:"pane_id"`
	WorkspaceID    string `json:"workspace_id"`
	WorkspaceLabel string `json:"workspace_label"`
	TabID          string `json:"tab_id"`
	TabLabel       string `json:"tab_label"`
	Cwd            string `json:"cwd"`
	Agent          string `json:"agent"`
	AgentStatus    string `json:"agent_status"`
	Focused        bool   `json:"focused"`
}

// fetchPanes lists every pane and joins in workspace/tab labels, returning them
// grouped by workspace (then tab) order — the order herdr itself shows.
func fetchPanes() ([]paneView, error) {
	res, err := herdrCall("pane.list", map[string]any{})
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
	if r, err := herdrCall("tab.list", map[string]any{}); err == nil {
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
	if r, err := herdrCall("workspace.list", map[string]any{}); err == nil {
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

	out := make([]paneView, 0, len(pl.Panes))
	for _, p := range pl.Panes {
		out = append(out, paneView{
			PaneID:         p.PaneID,
			WorkspaceID:    p.WorkspaceID,
			WorkspaceLabel: wss[p.WorkspaceID].label,
			TabID:          p.TabID,
			TabLabel:       tabs[p.TabID].label,
			Cwd:            p.Cwd,
			Agent:          p.Agent,
			AgentStatus:    p.AgentStatus,
			Focused:        p.Focused,
		})
	}
	sort.Slice(out, func(i, j int) bool {
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

func servePanes(w http.ResponseWriter, r *http.Request) {
	panes, err := fetchPanes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"panes": panes})
}

// serveFocus focuses a pane. herdr exposes no pane.focus, so focusing a pane
// means focusing its workspace and then its tab (panes live one-per-tab in the
// common case; for split tabs this focuses the tab the pane belongs to).
func serveFocus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		TabID       string `json:"tab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.WorkspaceID == "" || req.TabID == "" {
		http.Error(w, "workspace_id and tab_id required", http.StatusBadRequest)
		return
	}
	if _, err := herdrCall("workspace.focus", map[string]any{"workspace_id": req.WorkspaceID}); err != nil {
		http.Error(w, "workspace.focus: "+err.Error(), http.StatusBadGateway)
		return
	}
	if _, err := herdrCall("tab.focus", map[string]any{"tab_id": req.TabID}); err != nil {
		http.Error(w, "tab.focus: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// serveRename renames the tab a pane lives in. The grid labels each card with
// its tab label, so renaming the *tab* is what visibly relabels the card
// (pane.rename sets a pane name that herdr never surfaces in pane.list).
func serveRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TabID string `json:"tab_id"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.TabID == "" || strings.TrimSpace(req.Label) == "" {
		http.Error(w, "tab_id and non-empty label required", http.StatusBadRequest)
		return
	}
	if _, err := herdrCall("tab.rename", map[string]any{"tab_id": req.TabID, "label": req.Label}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// serveClose closes one or more panes (pane.close per id). Closing the last
// pane in a tab closes the tab too. Returns which ids closed and any errors,
// so a partial failure in a bulk close is still reported.
func serveClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		PaneIDs []string `json:"pane_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if len(req.PaneIDs) == 0 {
		http.Error(w, "pane_ids required", http.StatusBadRequest)
		return
	}
	closed := make([]string, 0, len(req.PaneIDs))
	errs := map[string]string{}
	for _, id := range req.PaneIDs {
		if _, err := herdrCall("pane.close", map[string]any{"pane_id": id}); err != nil {
			errs[id] = err.Error()
		} else {
			closed = append(closed, id)
		}
	}
	writeJSON(w, map[string]any{"closed": closed, "errors": errs})
}

// ---------------------------------------------------------------------------
// git diff: working-tree (or branch-vs-base) diff for the active pane's repo
// ---------------------------------------------------------------------------

// diffFile is one changed file in `git status`/`git diff --name-status`.
type diffFile struct {
	Path   string `json:"path"`
	Status string `json:"status"` // added | deleted | modified | renamed | untracked
	Staged bool   `json:"staged"`
}

const (
	maxDiff      = 2 << 20   // 2 MiB cap on the unified-diff payload
	maxUntracked = 256 << 10 // 256 KiB per synthesized untracked-file diff
)

// gitOut runs `git -C dir args...` and returns stdout, surfacing git's stderr
// in the error so the browser can show why a repo couldn't be diffed.
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
				return "", fmt.Errorf("%s", msg)
			}
		}
		return "", err
	}
	return string(out), nil
}

// serveDiff returns the git diff for the repo containing ?path=. It mirrors the
// way Fulcrum builds its diff view: show working-tree changes (unstaged +
// staged), and if the tree is clean fall back to the branch diff against the
// merge-base with the default branch (so a finished feature branch still shows
// its work). Optional ?ignoreWhitespace and ?includeUntracked toggles.
func serveDiff(w http.ResponseWriter, r *http.Request) {
	path := filepath.Clean(r.URL.Query().Get("path"))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	ignoreWS := r.URL.Query().Get("ignoreWhitespace") == "true"
	includeUntracked := r.URL.Query().Get("includeUntracked") == "true"

	root, err := gitOut(path, "rev-parse", "--show-toplevel")
	if err != nil {
		http.Error(w, "not a git repo: "+err.Error(), http.StatusBadGateway)
		return
	}
	root = strings.TrimSpace(root)
	branch := strings.TrimSpace(mustGit(root, "rev-parse", "--abbrev-ref", "HEAD"))

	wsArg := func(base ...string) []string {
		if ignoreWS {
			return append(base, "-w")
		}
		return base
	}

	staged := mustGit(root, wsArg("diff", "--cached")...)
	unstaged := mustGit(root, wsArg("diff")...)
	files := parseStatus(mustGit(root, "status", "--short"))

	combined := staged + unstaged
	if includeUntracked {
		for _, f := range files {
			if f.Status == "untracked" {
				combined += untrackedDiff(root, f.Path)
			}
		}
	}

	isBranchDiff := false
	baseBranch := ""
	if strings.TrimSpace(combined) == "" {
		if baseBranch = defaultBranch(root, branch); baseBranch != "" {
			if mb := strings.TrimSpace(mustGit(root, "merge-base", baseBranch, "HEAD")); mb != "" {
				if bd := mustGit(root, append(wsArg("diff"), mb+"..HEAD")...); strings.TrimSpace(bd) != "" {
					combined = bd
					isBranchDiff = true
					files = parseNameStatus(mustGit(root, "diff", "--name-status", mb+"..HEAD"))
				}
			}
		}
	}

	truncated := false
	if len(combined) > maxDiff {
		combined = combined[:maxDiff]
		truncated = true
	}

	writeJSON(w, map[string]any{
		"repo": root, "branch": branch, "diff": combined, "files": files,
		"isBranchDiff": isBranchDiff, "baseBranch": baseBranch, "truncated": truncated,
	})
}

// mustGit runs a git command, returning "" on error (the diff endpoint treats
// a missing sub-result as empty rather than failing the whole request).
func mustGit(dir string, args ...string) string {
	out, _ := gitOut(dir, args...)
	return out
}

// parseStatus turns `git status --short` porcelain into file entries.
func parseStatus(s string) []diffFile {
	var out []diffFile
	for _, line := range strings.Split(s, "\n") {
		if len(line) < 4 {
			continue
		}
		x, y := line[0], line[1]
		p := strings.TrimSpace(line[3:])
		if i := strings.Index(p, " -> "); i >= 0 { // rename: "old -> new"
			p = p[i+4:]
		}
		st := "modified"
		switch {
		case x == '?' && y == '?':
			st = "untracked"
		case x == 'A' || y == 'A':
			st = "added"
		case x == 'D' || y == 'D':
			st = "deleted"
		case x == 'R':
			st = "renamed"
		}
		out = append(out, diffFile{Path: p, Status: st, Staged: x != ' ' && x != '?'})
	}
	return out
}

// parseNameStatus turns `git diff --name-status` into file entries (used for the
// branch-vs-base fallback, where there's no working-tree status to read).
func parseNameStatus(s string) []diffFile {
	var out []diffFile
	for _, line := range strings.Split(s, "\n") {
		parts := strings.Split(strings.TrimSpace(line), "\t")
		if len(parts) < 2 || parts[0] == "" {
			continue
		}
		st := "modified"
		switch parts[0][0] {
		case 'A':
			st = "added"
		case 'D':
			st = "deleted"
		case 'R':
			st = "renamed"
		}
		out = append(out, diffFile{Path: parts[len(parts)-1], Status: st})
	}
	return out
}

// defaultBranch resolves the repo's base branch for a branch-vs-base diff:
// origin/HEAD if set, else main/master — never the current branch (that would
// diff a branch against itself).
func defaultBranch(root, current string) string {
	if ref, err := gitOut(root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		ref = strings.TrimSpace(ref) // e.g. "origin/main"
		if i := strings.LastIndex(ref, "/"); i >= 0 {
			ref = ref[i+1:]
		}
		if ref != "" && ref != current {
			return ref
		}
	}
	for _, b := range []string{"main", "master"} {
		if b == current {
			continue
		}
		if _, err := gitOut(root, "rev-parse", "--verify", "--quiet", b); err == nil {
			return b
		}
	}
	return ""
}

// untrackedDiff synthesizes an "all added" unified diff for an untracked file
// (git diff omits untracked files), so the Diff view can preview new files too.
func untrackedDiff(root, rel string) string {
	full := filepath.Join(root, rel)
	info, err := os.Stat(full)
	if err != nil || info.IsDir() || info.Size() > maxUntracked {
		return ""
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return ""
	}
	header := fmt.Sprintf("diff --git a/%s b/%s\nnew file\n", rel, rel)
	if isBinary(data) {
		return header + "Binary file (untracked)\n"
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("--- /dev/null\n")
	fmt.Fprintf(&b, "+++ b/%s\n", rel)
	fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", len(lines))
	for _, ln := range lines {
		b.WriteString("+" + ln + "\n")
	}
	return b.String()
}

func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

// subscribeFocus opens a long-lived connection subscribed to focus events and
// signals `trigger` whenever one arrives. Reconnects on failure.
func subscribeFocus(ctx context.Context, trigger chan<- struct{}) {
	for ctx.Err() == nil {
		conn, err := net.Dial("unix", *herdrSock)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		sub := `{"id":"ui-sub","method":"events.subscribe","params":{"subscriptions":` +
			`[{"type":"pane.focused"},{"type":"tab.focused"},{"type":"workspace.focused"}]}}` + "\n"
		if _, err := conn.Write([]byte(sub)); err != nil {
			conn.Close()
			continue
		}
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			select {
			case trigger <- struct{}{}:
			default:
			}
		}
		conn.Close()
		time.Sleep(time.Second)
	}
}

// ---------------------------------------------------------------------------
// SSE hub
// ---------------------------------------------------------------------------

type hub struct {
	mu      sync.RWMutex
	cur     Active
	clients map[chan Active]struct{}
}

func newHub() *hub { return &hub{clients: map[chan Active]struct{}{}} }

func (h *hub) snapshot() Active { h.mu.RLock(); defer h.mu.RUnlock(); return h.cur }

func (h *hub) run(ctx context.Context) {
	trigger := make(chan struct{}, 1)
	go subscribeFocus(ctx, trigger)
	ticker := time.NewTicker(*pollEvery)
	defer ticker.Stop()

	refresh := func() {
		a, err := fetchActive()
		if err != nil {
			return
		}
		h.mu.Lock()
		changed := a != h.cur
		h.cur = a
		clients := make([]chan Active, 0, len(h.clients))
		for c := range h.clients {
			clients = append(clients, c)
		}
		h.mu.Unlock()
		if changed {
			for _, c := range clients {
				select {
				case c <- a:
				default:
				}
			}
		}
	}

	refresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
			refresh()
		case <-ticker.C:
			refresh()
		}
	}
}

func (h *hub) serveSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan Active, 4)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	cur := h.cur
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.clients, ch); h.mu.Unlock() }()

	send := func(a Active) {
		b, _ := json.Marshal(a)
		fmt.Fprintf(w, "event: active\ndata: %s\n\n", b)
		fl.Flush()
	}
	send(cur) // prime with current state

	keep := time.NewTicker(25 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case a := <-ch:
			send(a)
		case <-keep.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// file APIs
// ---------------------------------------------------------------------------

type fileEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size,omitempty"`
}

func serveFiles(w http.ResponseWriter, r *http.Request) {
	path := filepath.Clean(r.URL.Query().Get("path"))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	ents, err := os.ReadDir(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	out := make([]fileEntry, 0, len(ents))
	for _, e := range ents {
		fe := fileEntry{Name: e.Name(), Dir: e.IsDir()}
		if !e.IsDir() {
			if info, err := e.Info(); err == nil {
				fe.Size = info.Size()
			}
		}
		out = append(out, fe)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir // dirs first
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, map[string]any{"path": path, "parent": filepath.Dir(path), "entries": out})
}

const maxPreview = 2 << 20 // 2 MiB

func serveFile(w http.ResponseWriter, r *http.Request) {
	path := filepath.Clean(r.URL.Query().Get("path"))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		http.Error(w, "not a file", http.StatusNotFound)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if info.Size() > maxPreview {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "[%s is %d bytes — too large to preview (limit %d)]", filepath.Base(path), info.Size(), maxPreview)
		return
	}
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

// indexHTML is index.html with the active theme's CSS variables injected,
// rendered once at startup.
var indexHTML []byte

// renderIndex injects a <style> block (overriding the static :root fallback)
// carrying the resolved theme's CSS variables, in place of the <!--THEME-->
// marker (or appended before </head> if the marker is absent).
func renderIndex() error {
	b, err := staticFS.ReadFile("index.html")
	if err != nil {
		return err
	}
	style := "<style id=\"herdr-theme\">/* resolved from herdr theme: " + theme.Resolved +
		" */\n  :root {\n" + theme.cssVars() + "  }</style>"
	s := string(b)
	if strings.Contains(s, "<!--THEME-->") {
		s = strings.Replace(s, "<!--THEME-->", style, 1)
	} else {
		s = strings.Replace(s, "</head>", style+"\n</head>", 1)
	}
	indexHTML = []byte(s)
	return nil
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// auth
// ---------------------------------------------------------------------------

func parseAuth(s string) (user, pass string, ok bool) {
	if s == "" {
		return "", "", false
	}
	u, p, found := strings.Cut(s, ":")
	if !found || u == "" {
		return "", "", false
	}
	return u, p, true
}

// withAuth gates every request behind HTTP basic auth when enabled. The browser
// caches the credentials per-origin, so a single login covers the page, the
// proxied terminal (incl. its WebSocket), SSE, and the file APIs.
func withAuth(next http.Handler, user, pass string, enabled bool) http.Handler {
	if !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="herdr", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
