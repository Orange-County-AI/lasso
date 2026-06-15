// Command lasso serves a web UI over a local, tmux-backed terminal workspace:
// a React SPA with a sidebar of repos/worktrees/agents, an embedded terminal
// (ttyd attached to tmux sessions), a file viewer, and a git diff view. State
// (workspaces, tabs, agents, settings) lives in a local sqlite db; agent panes
// run in tmux sessions on this machine.
//
// Everything binds to loopback by default: the terminal is a writable shell,
// so this is NOT meant to be exposed to a network without deliberate thought.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// distFS holds the built React + shadcn/ui frontend, embedded into the binary
// so a single executable still serves the whole UI. The `all:` prefix includes
// files whose names begin with "_" or "." The frontend source lives in the
// repo-root web/, but Vite builds it into server/web/dist so this embed (which
// can't reach a sibling dir) resolves. Build it (`bun run build` in web/) before
// `go build` — `mise run build` enforces that order. Favicons live in the build
// (copied from web/public), so there's no separate /static/ route anymore.
//
//go:embed all:web/dist
var distFS embed.FS

var (
	listenAddr    = flag.String("listen", defaultListenAddr, "address for the web server (loopback by default — the terminal is a writable shell)")
	pollEvery     = flag.Duration("poll", 2*time.Second, "fallback poll interval for the SSE refresh")
	statusEvery   = flag.Duration("status-poll", 750*time.Millisecond, "interval the agent-status poller scrapes tmux panes (paused when no browser is connected)")
	allowNoAuth   = flag.Bool("insecure-no-auth", false, "permit a non-loopback bind without auth (tailnet-only use; never on a public interface)")
	devMode       = flag.Bool("dev", false, "dev mode: fall forward to the next free web port if the requested one is busy (so multiple instances coexist). The frontend itself is served by the Vite dev server with hot reload — see `mise run dev`.")
	tailscaleUp   = flag.Bool("tailscale", false, "also expose the loopback server on the tailnet via `tailscale serve` (HTTPS at https://<node>.<tailnet>.ts.net)")
	tailscalePort = flag.Int("tailscale-https-port", 443, "HTTPS port for --tailscale's `tailscale serve` (use another, e.g. 8443, when 443 is already taken on this host)")
)

// runServer is the foreground HTTP server — the historical `./lasso` behavior.
// main() (cli.go) dispatches here for a bare invocation or the `serve`
// subcommand; the CLI subcommands (start/stop/restart/update/doctor) never reach
// it. It parses flags from os.Args, so `serve` strips its own arg first.
func runServer() {
	flag.Parse()

	// All filesystem/git operations route through the local backend.
	setBackend(&localBackend{})

	// Open the host-local state DB (~/.lasso/lasso.db), migrating a legacy
	// config.yaml on first run. Fatal if it can't open — the creator depends on it.
	if err := openDB(); err != nil {
		log.Fatalf("open state db: %v", err)
	}
	defer db.Close()

	// Ensure ~/.lasso/settings.json has a "theme" key with sensible defaults
	// before any UI toggle, so an external reader (e.g. a statusline) started
	// ahead of the browser still finds the live appearance. Best-effort; the
	// browser keeps it current via /api/theme.
	if err := seedThemeFile(); err != nil {
		log.Printf("seed theme.json: %v", err)
	}

	// Auth credentials come from the environment (UI_AUTH=user:pass), never
	// argv — so they don't leak via `ps`. Safety guard: refuse to bind to a
	// non-loopback address without auth, so this can't accidentally expose a
	// writable shell on a public interface again.
	authUser, authPass, hasAuth := parseAuth(os.Getenv("UI_AUTH"))
	if !isLoopback(*listenAddr) && !hasAuth && !*allowNoAuth {
		log.Fatalf("refusing to listen on non-loopback %q without auth — set UI_AUTH=user:pass, "+
			"or pass -insecure-no-auth to bind bare (only safe on a private interface like tailscale0)", *listenAddr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := newHub()
	srvHub = hub
	srvCtx = ctx
	go hub.run(ctx)
	if us, err := getUIState(); err == nil {
		sidebarAllHosts.Store(us.SidebarAllHosts) // so the poller scrapes all hosts if it was on
	}
	go statusPoller(ctx, hub) // scrape agent tmux panes; push status changes via SSE

	// tmux is the terminal backend now. Set the server's options before any
	// session detaches (destroy-unattached off, so background sessions survive),
	// reconcile the saved tab tree against live sessions, and keep tab cwds fresh
	// for post-reboot restoration.
	startSessionCloseListener(ctx, hub) // FIFO must exist before the hook fires
	_ = tmuxEnsureServer()
	reconcileTabs()
	// Warm the viewport ttyd now (attached to the park session) so the slow
	// browser xterm⇄ttyd attach handshake overlaps app load instead of stalling
	// the first tab the user opens. Best-effort.
	go func() { _, _ = ensureViewport() }()
	go cwdSaver(ctx)
	go tabExitWatcher(ctx, hub) // backstop for the FIFO listener above

	mux := http.NewServeMux()
	mux.HandleFunc("/api/active", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, hub.snapshot())
	})
	mux.HandleFunc("/api/events", hub.serveSSE)
	mux.HandleFunc("/api/files", serveFiles)
	mux.HandleFunc("/api/tab-cwd", serveTabCwd)
	mux.HandleFunc("/api/file", serveFile)
	mux.HandleFunc("/api/file-delete", serveFileDelete)
	mux.HandleFunc("/api/file-rename", serveFileRename)
	mux.HandleFunc("/api/file-write", serveFileWrite)
	mux.HandleFunc("/api/file-upload", serveFileUpload)
	// the viewport: one persistent ttyd (tmux attach) switched between tab
	// sessions, replacing the old per-tab ttyd pool.
	mux.HandleFunc("/api/tab/term", serveTabTerm)
	mux.HandleFunc("/api/tab/term-touch", serveTabTermTouch)
	mux.HandleFunc("/api/tab/term-ready", serveTabReady)
	mux.HandleFunc("/tab-term/", serveTabTermProxy)
	// sidebar: the workspace/tab tree + agent list + their mutations (tmux-era)
	mux.HandleFunc("/api/tree", serveTree)
	mux.HandleFunc("/api/agents", serveAgentsList)
	mux.HandleFunc("/api/tab/new", serveNewTab)
	mux.HandleFunc("/api/tab/rename", serveTabRename)
	mux.HandleFunc("/api/tab/close", serveTabClose)
	mux.HandleFunc("/api/agent/close", serveAgentClose)
	mux.HandleFunc("/api/workspace/create", serveCreateWorkspace)
	mux.HandleFunc("/api/workspace/rename", serveWorkspaceRenameDB)
	mux.HandleFunc("/api/workspace/close", serveWorkspaceClose)
	mux.HandleFunc("/api/spaces/reorder", serveSpacesReorder)
	mux.HandleFunc("/api/repo/rename", serveRepoRename)
	mux.HandleFunc("/api/repo/open", serveOpenRepo)
	mux.HandleFunc("/api/repo/close", serveRepoClose)
	mux.HandleFunc("/api/create-worktree", serveCreateWorktreeOnly)
	mux.HandleFunc("/api/ui-state", serveUIState)
	mux.HandleFunc("/api/theme", serveTheme)
	mux.HandleFunc("/api/paste-image", servePasteImage)
	mux.HandleFunc("/api/diff", serveDiff)
	mux.HandleFunc("/api/diff-file", serveDiffFile)
	mux.HandleFunc("/api/version", serveVersion)
	mux.HandleFunc("/api/agent-config", serveAgentConfig)
	mux.HandleFunc("/api/repo-config", serveRepoConfig)
	mux.HandleFunc("/api/repos", serveRepos)
	mux.HandleFunc("/api/repo-branches", serveRepoBranches)
	// multi-host: list ssh-config hosts + switch the active one.
	mux.HandleFunc("/api/hosts", serveHosts)
	mux.HandleFunc("/api/host", serveHostSwitch)
	// grid: the cross-host terminal wall + its per-pane ttyd proxy.
	mux.HandleFunc("/api/grid", serveGrid)
	mux.HandleFunc("/api/grid/term", serveGridTerm)
	mux.HandleFunc("/api/grid/term-touch", serveGridTermTouch)
	mux.HandleFunc("/api/grid/term-release", serveGridTermRelease)
	mux.HandleFunc("/api/grid/rename", serveGridRename)
	mux.HandleFunc("/api/grid/close", serveGridClose)
	mux.HandleFunc("/grid-term/", serveGridTermProxy)
	mux.HandleFunc("/api/create-agent", serveCreateAgent)
	mux.HandleFunc("/api/agent-upload", serveAgentUpload)
	mux.HandleFunc("/api/self-update", serveSelfUpdate)
	// MCP server: lets an agent session orchestrate other lasso agents over the
	// Model Context Protocol. Mounted here (before the SPA catch-all) and exempt
	// from UI_AUTH below — see withAuthExcept. The handler serves both /mcp and
	// /mcp/… (the Streamable-HTTP transport's own subpaths).
	mcpHandler := newMCPHandler()
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)
	dist, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		log.Fatalf("dist fs: %v", err)
	}
	// Hashed, content-addressed build assets are immutable → long cache. Every
	// other path falls through to the SPA entry (index.html). In -dev the live
	// frontend is the Vite dev server (HMR) proxied onto these API routes; this
	// embedded copy is what the production binary serves.
	mux.Handle("/assets/", cacheControl(http.FileServer(http.FS(dist))))
	mux.Handle("/", serveDist(dist))
	if *devMode {
		log.Printf("dev:      ON — backend only; run the Vite dev server in web/ for the frontend (mise run dev)")
	}

	// /mcp is intentionally unauthenticated (see CLAUDE.md security note); the rest
	// of the app stays behind UI_AUTH when set.
	handler := withAuthExcept(mux, authUser, authPass, hasAuth, "/mcp")

	// Bind now (not via ListenAndServe) so dev can fall forward to the next free
	// port if the requested one is taken. Outside dev a busy port is fatal — we
	// don't want a prod instance silently landing somewhere unexpected.
	ln, boundAddr, err := listenWithFallback(*listenAddr, *devMode, 50)
	if err != nil {
		log.Fatalf("listen %s: %v", *listenAddr, err)
	}
	if boundAddr != *listenAddr {
		log.Printf("dev:      web port %s busy → using %s", *listenAddr, boundAddr)
		*listenAddr = boundAddr // so the URL log + isLoopback reflect reality
	}

	// Terminals are per-tab ttyds attached to tmux sessions (see tabterm.go),
	// spawned on demand when the browser opens a tab — nothing to spawn at boot.

	// Optionally publish the loopback server on the tailnet via `tailscale serve`
	// (lasso itself stays loopback). Non-fatal: if it can't come up (e.g. the
	// operator bit isn't set) we log loudly and keep serving locally rather than
	// crash-looping under a supervisor.
	var tsStop func()
	if *tailscaleUp {
		_, portStr, _ := net.SplitHostPort(*listenAddr)
		port, _ := strconv.Atoi(portStr)
		if stopFn, tsURL, err := exposeOverTailnet(port, *tailscalePort); err != nil {
			log.Printf("tailnet:  FAILED to expose over tailscale — continuing loopback-only:\n%v", err)
		} else {
			tsStop = stopFn
			defer tsStop()
			log.Printf("tailnet:  %s (via tailscale serve)", tsURL)
		}
	}

	srv := &http.Server{Handler: handler}
	go func() {
		<-ctx.Done()
		if tsStop != nil {
			tsStop() // drop the tailnet route before the listener goes away
		}
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
	log.Printf("terminals: per-tab ttyd → tmux -S %s", lassoTmuxSock())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// listenWithFallback binds addr. If dev is true and the port is already in use,
// it scans forward up to span ports (same host) and binds the first free one,
// returning the listener and the address it actually bound. Outside dev (or for
// any non-EADDRINUSE error) it returns the bind error so the caller can fail.
func listenWithFallback(addr string, dev bool, span int) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil || !dev || !errors.Is(err, syscall.EADDRINUSE) {
		return ln, addr, err
	}
	host, portStr, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		return nil, addr, err
	}
	start, convErr := strconv.Atoi(portStr)
	if convErr != nil {
		return nil, addr, err
	}
	for p := start + 1; p <= start+span; p++ {
		cand := net.JoinHostPort(host, strconv.Itoa(p))
		if l, e := net.Listen("tcp", cand); e == nil {
			return l, cand, nil
		}
	}
	return nil, addr, fmt.Errorf("no free port in %d..%d: %w", start, start+span, err)
}

// ---------------------------------------------------------------------------
// ttyd child process
// ---------------------------------------------------------------------------

// unixSocketProxy reverse-proxies to one of our ttyds over its private unix
// socket. The host in the URL is a placeholder — the custom DialContext ignores
// it and dials the socket. WS upgrades work because the hijacked conn is dialed
// through the same Transport.
func unixSocketProxy(sock string) *httputil.ReverseProxy {
	p := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "ttyd.sock"})
	p.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	return p
}

// startTtydArgv spawns one ttyd serving the given argv under basePath on its own
// private unix socket. env, if non-nil, overrides the child environment; nil
// inherits the server's env.
func startTtydArgv(ctx context.Context, sock, basePath string, cmdArgv []string, env []string) error {
	// Bind a private unix socket (one per instance) rather than a shared TCP
	// port, so concurrent prod/dev instances can't collide or cross-connect.
	// Clear any stale socket left by a crashed prior run with this PID so ttyd
	// can bind.
	_ = os.Remove(sock)

	// The xterm.js ITheme (background/foreground/cursor + 16 ANSI colors) is the
	// fixed Onyx palette, passed to ttyd via `-t theme=<json>` so the terminal
	// starts already themed rather than flashing ttyd's default light palette
	// before the browser re-applies it (see web/src/lib/theme.ts).
	xtheme := onyxXtermJSON
	args := []string{
		"-i", sock, // private unix socket (ttyd accepts a socket path here)
		"-b", basePath, // base path so assets/ws resolve under the proxy
		"-W",                           // writable
		"-t", "disableLeaveAlert=true", // no confirm dialog inside the iframe
		"-t", "fontSize=14",
		// Keep a solid block cursor even when xterm thinks it's unfocused.
		// We live in an iframe whose focus is handed over programmatically
		// (contentWindow.focus()), which doesn't always flip xterm's internal
		// focus flag — so without this it falls back to the default "outline"
		// inactive cursor, which reads as a hollow box / bare underline (most
		// glaring in TUIs like helix that rely on a block cursor). xterm has
		// dedicated handling that keeps the glyph under an inactive block
		// readable, so this stays legible.
		"-t", "cursorInactiveStyle=block",
		"-t", "theme=" + xtheme,
	}
	args = append(args, cmdArgv...)
	cmd := exec.Command("ttyd", args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.Env = env                                         // nil → inherit
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own process group so we can kill cleanly
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("spawned ttyd (pid %d) %q @ %s", cmd.Process.Pid, strings.Join(cmdArgv, " "), basePath)
	go func() {
		<-ctx.Done()
		// kill the whole process group (ttyd + the shell it spawned)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}()
	go func() { _ = cmd.Wait(); _ = os.Remove(sock) }()
	return nil
}

// srvHub and srvCtx are set in runServer so background subsystems (the viewport
// watcher, the status poller, the sidebar handlers) can reach the SSE hub and the
// server's root context (the lifetime of spawned ttyds).
var (
	srvHub *hub
	srvCtx context.Context
)

// waitSocket polls until the socket file exists (want=true) or is gone
// (want=false), or the timeout elapses. Used after spawning a ttyd so the first
// proxied request doesn't 502 before the socket is bound.
func waitSocket(sock string, want bool, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := os.Stat(sock)
		if (err == nil) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// version + update check (Settings tab)
// ---------------------------------------------------------------------------

// Active is the state pushed to the browser.
type Active struct {
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	PanesRev    int    `json:"panes_rev"` // bumps when the workspace/tab tree changes
	Host        string `json:"host"`      // active host: always "local"
	TermRev     int    `json:"term_rev"`  // bumps so the browser reloads terminal iframes
	UIRev       int    `json:"ui_rev"`    // bumps when /api/ui-state is written, so other clients re-pull the layout
	// AgentStatuses maps each live agent tab id to its status (idle|working|
	// blocked), produced by the status poller. The sidebar's agent pane reads it.
	AgentStatuses map[string]string `json:"agent_statuses"`
}

// versionInfo is the /api/version payload: lasso's own version plus whether a
// newer build/release is available.
type versionInfo struct {
	LassoVersion string `json:"lasso_version"`
	// UpdateState says whether the running build is behind: "available" (a newer
	// commit/release is waiting), "current" (already up to date), or "unknown".
	UpdateState string `json:"update_state,omitempty"`
	// LatestVersion is the newest published GitHub release tag.
	LatestVersion string `json:"latest_version,omitempty"`
	Err           string `json:"err,omitempty"`
}

// serveVersion reports lasso's own version and whether an update is available,
// comparing this build's version to the latest GitHub release. The check is
// non-blocking — the tag is "" until the background fetch lands, then the next
// poll picks it up. Dev/worktree runs skip it (they update by rebuild).
func serveVersion(w http.ResponseWriter, r *http.Request) {
	vi := versionInfo{LassoVersion: lassoVersion()}
	if !*devMode {
		if latest, ok := cachedLatestTag(); ok {
			vi.LatestVersion = latest
			if semverNewer(lassoSemver, latest) {
				vi.UpdateState = "available"
			} else {
				vi.UpdateState = "current"
			}
		}
	}
	writeJSON(w, vi)
}

// ---------------------------------------------------------------------------
// image paste: save a clipboard image to disk so the agent in the focused
// pane (e.g. Claude Code) can read it by path
// ---------------------------------------------------------------------------

// maxPasteImage caps the request body so a runaway/hostile paste can't fill
// the disk. Screenshots are well under this.
const maxPasteImage = 25 << 20 // 25 MiB

// pasteImageExt maps an image content-type to a file extension. Anything not
// listed is rejected — this endpoint only ever writes image files.
var pasteImageExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/jpg":  ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// pasteImageDir is the directory pasted clipboard images are written to. Kept
// under lasso's own ~/.lasso/uploads (alongside staged attachment uploads)
// rather than the OS cache dir, so the images live with the rest of lasso's
// data and aren't swept by cache cleaners.
func pasteImageDir() string {
	return filepath.Join(lassoUploadsDir(), "pasted-images")
}

// servePasteImage accepts a raw image body (Content-Type set to the image MIME
// type), writes it to the target host's PasteImageDir() with a timestamped name,
// and returns the absolute path. The browser then inserts that path at the
// terminal cursor. The image MUST be written on the same host the terminal runs
// on — otherwise the path resolves on the wrong machine — so it routes through
// the ?host= backend (reqHost), not curBackend(): the cross-host grid shows tabs
// from several hosts at once while only one is "active".
func servePasteImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	ext, ok := pasteImageExt[strings.ToLower(strings.TrimSpace(ct))]
	if !ok {
		http.Error(w, "unsupported image content-type "+ct, http.StatusUnsupportedMediaType)
		return
	}
	be, err := hostBackend(reqHost(r))
	if err != nil {
		http.Error(w, "host unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPasteImage))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}
	dir := be.PasteImageDir()
	if err := be.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, "mkdir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	name := "clipboard-" + time.Now().Format("2006-01-02-150405") + ext
	path := filepath.Join(dir, name)
	if err := be.WriteFile(path, body, 0o644); err != nil {
		http.Error(w, "write: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"path": path})
}

// ---------------------------------------------------------------------------
// git diff: working-tree (or branch-vs-base) diff for the active pane's repo
// ---------------------------------------------------------------------------

// diffFile is one changed file in the diff metadata: path, status, and per-file
// line counts (from `git diff --numstat`). The actual line-by-line diff is
// fetched lazily per file from /api/diff-file when the user expands it, so the
// file list is always complete (never byte-capped) and we never ship a multi-MB
// blob just to render collapsed headers.
type diffFile struct {
	Path   string `json:"path"`
	Status string `json:"status"` // added | deleted | modified | renamed | untracked
	Staged bool   `json:"staged"`
	Add    int    `json:"add"` // added lines (numstat); 0 for binary
	Del    int    `json:"del"` // deleted lines (numstat); 0 for binary
}

const (
	maxDiff      = 2 << 20   // 2 MiB cap on the unified-diff payload
	maxUntracked = 256 << 10 // 256 KiB per synthesized untracked-file diff
)

// gitOut runs `git -C dir args...` on the active host and returns stdout. The
// local implementation is gitOutLocal; remoteBackend runs git over SSH.
func gitOut(dir string, args ...string) (string, error) {
	return curBackend().GitOut(dir, args...)
}

// gitOutLocal runs `git -C dir args...` on this machine and returns stdout,
// surfacing git's stderr in the error so the browser can show why a repo
// couldn't be diffed. This is localBackend.GitOut.
func gitOutLocal(dir string, args ...string) (string, error) {
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

// serveDiff returns the git diff for the repo containing ?path=. Modes selected
// by ?mode=:
//   - auto (default): working-tree changes when the tree is dirty, otherwise the
//     branch-vs-base comparison — so the pane always shows something useful.
//   - working: show working-tree changes (unstaged + staged) only — empty when
//     the tree is clean.
//   - branch: diff merge-base(base, HEAD)..HEAD, ignoring the working tree —
//     the whole branch vs the primary branch.
//
// Optional ?ignoreWhitespace, ?includeUntracked, and ?baseBranch (override the
// branch the comparison runs against) toggles. The response always reports the
// working-tree dirty-file count so the UI can flag dirtiness in either mode.
func serveDiff(w http.ResponseWriter, r *http.Request) {
	path := filepath.Clean(r.URL.Query().Get("path"))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	ignoreWS := r.URL.Query().Get("ignoreWhitespace") == "true"
	includeUntracked := r.URL.Query().Get("includeUntracked") == "true"
	mode := r.URL.Query().Get("mode")               // "branch" forces the base-branch comparison
	baseOverride := r.URL.Query().Get("baseBranch") // optional explicit base for the comparison

	_ = includeUntracked // untracked files are always included in the metadata list

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

	// working-tree status is always read so the dirty count is accurate even when
	// showing the branch diff.
	status := parseStatus(mustGit(root, "status", "--short"))
	dirty := len(status)

	var files []diffFile
	baseBranch := ""

	// auto (default): show the working tree when it's dirty, otherwise fall back to
	// the branch-vs-base comparison. ?mode=branch / ?mode=working force one or the
	// other.
	isBranchDiff := mode == "branch" || (mode != "working" && dirty == 0)
	if isBranchDiff {
		files, baseBranch = branchFiles(root, branch, baseOverride, wsArg)
		if baseBranch == "" {
			isBranchDiff = false // no base to compare against → show the working tree
		}
	}
	if !isBranchDiff {
		files = workingFiles(root, status, wsArg)
	}

	writeJSON(w, map[string]any{
		"repo": root, "branch": branch, "files": files,
		"isBranchDiff": isBranchDiff, "baseBranch": baseBranch, "dirty": dirty,
	})
}

// serveDiffFile returns the unified diff for a SINGLE file (?file=, repo-relative)
// — fetched lazily when the user expands that file in the Diff view, so the file
// list itself is never byte-capped. ?mode= pins the comparison to what the list
// is showing (branch vs working); the per-file diff is capped at maxDiff (a
// single genuinely huge file), reported via "truncated".
func serveDiffFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := filepath.Clean(q.Get("path"))
	file := q.Get("file")
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	if file == "" {
		http.Error(w, "file is required", http.StatusBadRequest)
		return
	}
	ignoreWS := q.Get("ignoreWhitespace") == "true"
	mode := q.Get("mode")
	baseOverride := q.Get("baseBranch")

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

	var d string
	if mode == "branch" {
		base := baseOverride
		if base == "" {
			base = defaultBranch(root, branch)
		}
		if base != "" {
			if mb := strings.TrimSpace(mustGit(root, "merge-base", base, "HEAD")); mb != "" {
				d = mustGit(root, wsArg("diff", mb+"..HEAD", "--", file)...)
			}
		}
	} else {
		// working tree vs HEAD (staged + unstaged combined); empty ⇒ untracked.
		d = mustGit(root, wsArg("diff", "HEAD", "--", file)...)
		if d == "" {
			d = untrackedDiff(root, file)
		}
	}

	truncated := false
	if len(d) > maxDiff {
		d = d[:maxDiff]
		truncated = true
	}
	writeJSON(w, map[string]any{"diff": d, "truncated": truncated})
}

// branchVsBase returns the diff of merge-base(base, HEAD)..HEAD, the resolved
// base branch, and the changed-file list. base defaults to the repo's primary
// branch (override wins when non-empty). ok is false when no base branch exists
// (e.g. HEAD already is the primary branch) — baseBranch is still returned so
// the caller can report what it tried to compare against.
// branchFiles lists the files changed on this branch vs its base, with per-file
// counts. Returns ("", nil) base when there's no base branch to compare against.
func branchFiles(root, current, override string, wsArg func(...string) []string) ([]diffFile, string) {
	base := override
	if base == "" {
		base = defaultBranch(root, current)
	}
	if base == "" {
		return nil, ""
	}
	mb := strings.TrimSpace(mustGit(root, "merge-base", base, "HEAD"))
	if mb == "" {
		return nil, base
	}
	return fileList(root, wsArg, mb+"..HEAD"), base
}

// workingFiles lists the working-tree changes (staged + unstaged vs HEAD) with
// counts, then appends untracked files (which `git diff` omits).
func workingFiles(root string, status []diffFile, wsArg func(...string) []string) []diffFile {
	files := fileList(root, wsArg, "HEAD")
	for _, f := range status {
		if f.Status == "untracked" {
			files = append(files, diffFile{Path: f.Path, Status: "untracked", Add: countAddedLines(root, f.Path)})
		}
	}
	return files
}

// fileList builds the changed-file list for a comparison (rangeArgs, e.g. "HEAD"
// or "<merge-base>..HEAD"): paths + per-file +/- from `--numstat`, statuses from
// `--name-status`. --no-renames keeps paths plain so the two outputs align (a
// rename shows as delete+add). Whitespace-only modifications (with -w) collapse
// to 0/0 and are dropped, matching the per-file view that would show nothing.
func fileList(root string, wsArg func(...string) []string, rangeArgs ...string) []diffFile {
	num := wsArg(append([]string{"diff", "--numstat", "--no-renames"}, rangeArgs...)...)
	name := append([]string{"diff", "--name-status", "--no-renames"}, rangeArgs...)
	counts, order := parseNumstat(mustGit(root, num...))
	statuses := parseNameStatusMap(mustGit(root, name...))
	var files []diffFile
	for _, p := range order {
		c := counts[p]
		st := statuses[p]
		if st == "" {
			st = "modified"
		}
		if st == "modified" && c[0] == 0 && c[1] == 0 {
			continue // whitespace-only under -w, or a no-op entry
		}
		files = append(files, diffFile{Path: p, Status: st, Add: c[0], Del: c[1]})
	}
	return files
}

// parseNumstat turns `git diff --numstat` ("<add>\t<del>\t<path>", with "-" for
// binary) into a path→[add,del] map plus the original file order.
func parseNumstat(out string) (map[string][2]int, []string) {
	m := map[string][2]int{}
	var order []string
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) < 3 || parts[2] == "" {
			continue
		}
		p := parts[2]
		if _, seen := m[p]; !seen {
			order = append(order, p)
		}
		m[p] = [2]int{numOrZero(parts[0]), numOrZero(parts[1])}
	}
	return m, order
}

func numOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0 // "-" (binary) or malformed
	}
	return n
}

// countAddedLines returns the line count of a small text file, for an untracked
// file's "+N" count (git omits untracked files from numstat). Mirrors the cap in
// untrackedDiff so we never read a huge or binary file just to count lines.
func countAddedLines(root, rel string) int {
	full := filepath.Join(root, rel)
	info, err := curBackend().Stat(full)
	if err != nil || info.IsDir() || info.Size() > maxUntracked {
		return 0
	}
	data, err := curBackend().ReadFile(full)
	if err != nil || isBinary(data) {
		return 0
	}
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
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

// parseNameStatusMap turns `git diff --name-status --no-renames` into a
// path→status map (A/D → added/deleted, else modified). Used to label the files
// listed by parseNumstat.
func parseNameStatusMap(s string) map[string]string {
	m := map[string]string{}
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
		}
		m[parts[len(parts)-1]] = st
	}
	return m
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
	info, err := curBackend().Stat(full)
	if err != nil || info.IsDir() || info.Size() > maxUntracked {
		return ""
	}
	data, err := curBackend().ReadFile(full)
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

// ---------------------------------------------------------------------------
// SSE hub
// ---------------------------------------------------------------------------

type hub struct {
	mu      sync.RWMutex
	cur     Active
	rev     int    // sidebar tree revision (bumped when lastSig changes)
	lastSig string // last seen tree+agents signature
	termRev int    // terminal-reload revision (retained in the SSE payload)
	uiRev   int    // ui-state revision (bumped when /api/ui-state is written)
	clients map[chan Active]struct{}

	rootCtx context.Context
	trigger chan struct{}
}

func newHub() *hub {
	return &hub{cur: Active{Host: "local"}, clients: map[chan Active]struct{}{}}
}

// kick forces a near-immediate refresh (non-blocking). The status poller and the
// workspace/tab CRUD handlers call it so changes are pushed without waiting for
// the poll tick.
func (h *hub) kick() {
	select {
	case h.trigger <- struct{}{}:
	default:
	}
}

// bumpUI signals that the persisted UI prefs changed (POST /api/ui-state):
// ui_rev bumps in the SSE payload so other connected clients re-fetch the
// state and apply the new layout (last write wins).
func (h *hub) bumpUI() {
	h.mu.Lock()
	h.uiRev++
	h.mu.Unlock()
	h.kick()
}

// clientCount returns how many browsers are currently connected (the status
// poller pauses when this is zero).
func (h *hub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *hub) snapshot() Active { h.mu.RLock(); defer h.mu.RUnlock(); return h.cur }

// bumpTermRev increments the terminal-reload revision and kicks a refresh, so
// every browser reloads its terminal iframes onto the newly active host (see
// serveHostSwitch + the frontend's HOST_CHANGED_EVENT).
func (h *hub) bumpTermRev() {
	h.mu.Lock()
	h.termRev++
	h.mu.Unlock()
	h.kick()
}

func (h *hub) run(ctx context.Context) {
	h.rootCtx = ctx
	h.trigger = make(chan struct{}, 1)
	trigger := h.trigger
	ticker := time.NewTicker(*pollEvery)
	defer ticker.Stop()

	// refresh rebuilds the pushed Active from the workspace/tab tree (bumping
	// PanesRev when it changes) and the agent-status cache, then broadcasts to
	// connected browsers if anything changed. The status poller and the CRUD
	// handlers kick() the hub when state changes; the ticker is a slow safety net.
	refresh := func() {
		statuses := agentStatuses.snapshot()
		// Fold the live-agent SET (cache keys) into the signature so PanesRev
		// bumps — and the sidebar refetches /api/tree+/api/agents — when an agent
		// starts or exits, not just when the DB tree changes. Status *value*
		// changes still ride the AgentStatuses map (dots update without refetch).
		keys := make([]string, 0, len(statuses))
		for k := range statuses {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sig := treeSignature() + "|agents:" + strings.Join(keys, ",")
		h.mu.Lock()
		if sig != h.lastSig {
			h.lastSig = sig
			h.rev++
		}
		a := Active{
			Host:          curBackend().Name(),
			PanesRev:      h.rev,
			TermRev:       h.termRev,
			UIRev:         h.uiRev,
			AgentStatuses: statuses,
		}
		changed := !sameActive(a, h.cur)
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

// sameActive compares two Active values including the AgentStatuses map (Active
// is no longer comparable with == because it carries a map).
func sameActive(a, b Active) bool {
	if a.PanesRev != b.PanesRev || a.TermRev != b.TermRev ||
		a.Host != b.Host || a.UIRev != b.UIRev ||
		len(a.AgentStatuses) != len(b.AgentStatuses) {
		return false
	}
	for k, v := range a.AgentStatuses {
		if b.AgentStatuses[k] != v {
			return false
		}
	}
	return true
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

// expandTilde resolves a leading ~ or ~/… to the current user's home
// directory so the path input accepts the shorthand. Anything else (including
// ~user, which we don't resolve) is returned unchanged.
func expandTilde(p string) string { return expandTildeOn(curBackend(), p) }

// expandTildeOn expands a leading ~ against a specific backend's home dir, so a
// path can be resolved on a host other than the active one (e.g. listing a
// remote host's repos for its Settings).
func expandTildeOn(be Backend, p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := be.HomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

func serveFiles(w http.ResponseWriter, r *http.Request) {
	path := filepath.Clean(expandTilde(r.URL.Query().Get("path")))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	out, err := curBackend().ReadDir(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir // dirs first
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, map[string]any{"path": path, "parent": filepath.Dir(path), "entries": out})
}

// serveTabCwd reports the live working directory of a tab's terminal — its tmux
// session's foreground-process cwd. The file panel polls this so it follows the
// active terminal as the user cd's around, not just the tab's launch dir. Falls
// back to the tab's saved cwd when the session isn't live (pre-attach / after a
// reboot, before the shell is recreated).
func serveTabCwd(w http.ResponseWriter, r *http.Request) {
	tabID := r.URL.Query().Get("tab")
	if tabID == "" {
		http.Error(w, "tab required", http.StatusBadRequest)
		return
	}
	cwd, err := tmuxCurrentPath(tabSession(tabID))
	if err != nil || cwd == "" {
		if t, terr := getTab(tabID); terr == nil {
			cwd = t.Cwd
		}
	}
	writeJSON(w, map[string]string{"cwd": cwd})
}

// serveFileDelete removes a file or directory (directories recursively). It
// mirrors serveFiles' "any absolute path" trust model — lasso already exposes
// the whole filesystem for browsing, so delete carries the same reach.
func serveFileDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path := filepath.Clean(expandTilde(req.Path))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	if err := curBackend().RemoveAll(path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// serveFileRename renames an entry in place: the new name is a bare basename
// (no separators), kept in the same parent directory.
func serveFileRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path := filepath.Clean(expandTilde(req.Path))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	dst := filepath.Join(filepath.Dir(path), name)
	if _, err := curBackend().Lstat(dst); err == nil {
		http.Error(w, "a file with that name already exists", http.StatusConflict)
		return
	}
	if err := curBackend().Rename(path, dst); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "path": dst})
}

// serveFileWrite overwrites an existing file with new content, preserving its
// permission bits. The file must already exist (the editor only saves files it
// opened) — this is not a create-arbitrary-path endpoint.
func serveFileWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path := filepath.Clean(expandTilde(req.Path))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	info, err := curBackend().Stat(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "not a file", http.StatusBadRequest)
		return
	}
	if err := curBackend().WriteFile(path, []byte(req.Content), info.Mode().Perm()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// maxUpload caps the total multipart body so a runaway/hostile upload can't
// fill the disk via the browser.
const maxUpload = 1 << 30 // 1 GiB

// serveFileUpload writes each file from a multipart/form-data POST into the
// directory named by the `dir` field. Only the basename of each uploaded file
// is honored (never a client-supplied path), so an upload can't escape `dir`.
// Like the other file endpoints it trusts any absolute path on the tailnet.
func serveFileUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "parse upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	dir := filepath.Clean(expandTilde(r.FormValue("dir")))
	if !filepath.IsAbs(dir) {
		http.Error(w, "dir must be absolute", http.StatusBadRequest)
		return
	}
	if info, err := curBackend().Stat(dir); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	} else if !info.IsDir() {
		http.Error(w, "not a directory", http.StatusBadRequest)
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		http.Error(w, "no files", http.StatusBadRequest)
		return
	}
	written := make([]string, 0, len(files))
	for _, fh := range files {
		name := filepath.Base(filepath.FromSlash(fh.Filename))
		if name == "" || name == "." || name == ".." {
			http.Error(w, "invalid filename", http.StatusBadRequest)
			return
		}
		if err := saveUpload(fh, filepath.Join(dir, name)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		written = append(written, name)
	}
	writeJSON(w, map[string]any{"ok": true, "files": written})
}

// saveUpload streams one multipart file to dst on the active host, truncating
// any existing file. Routes through the backend so an upload while a remote host
// is active lands on that host (over SFTP), not the machine running lasso.
func saveUpload(fh *multipart.FileHeader, dst string) error {
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := curBackend().Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

const maxPreview = 2 << 20 // 2 MiB

func serveFile(w http.ResponseWriter, r *http.Request) {
	path := filepath.Clean(expandTilde(r.URL.Query().Get("path")))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	info, err := curBackend().Stat(path)
	if err != nil || info.IsDir() {
		http.Error(w, "not a file", http.StatusNotFound)
		return
	}
	f, err := curBackend().Open(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	// ServeContent sets Last-Modified; without an explicit Cache-Control the
	// browser applies heuristic freshness (~10% of the file's age) and serves
	// stale content from its cache after an edit — reopening a just-saved file
	// showed the old version. File content must always be fetched fresh.
	w.Header().Set("Cache-Control", "no-store")
	// `download=1` forces a browser save (Content-Disposition: attachment) and
	// bypasses the preview cap — the viewer's text fetch omits it, so previews
	// still stay bounded.
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(filepath.Base(path)))
		http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
		return
	}
	// The preview cap bounds text fetched into the editor; binary media
	// (images, PDFs) render in-browser regardless of size, so serve them whole.
	if info.Size() > maxPreview && !isPreviewMedia(path) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "[%s is %d bytes — too large to preview (limit %d)]", filepath.Base(path), info.Size(), maxPreview)
		return
	}
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

// isPreviewMedia reports whether path is a binary media type the viewer renders
// directly (images, PDFs) rather than fetching as text — these bypass the text
// preview size cap.
func isPreviewMedia(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp", ".ico", ".avif":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

// cacheControl gives content-addressed build assets (Vite's /assets/*, whose
// names carry a content hash) a long cache lifetime — they only change when the
// binary is rebuilt, and a new build yields new filenames.
func cacheControl(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

// serveDist serves the embedded SPA build: a real file when one exists for the
// request path (favicons at the root, etc.), otherwise index.html — so the
// single-page app loads for any path. index.html itself is served no-store so a
// new build is always picked up; its hashed asset references handle caching.
func serveDist(dist fs.FS) http.HandlerFunc {
	files := http.FileServer(http.FS(dist))
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name != "" && fs.ValidPath(name) {
			if f, err := dist.Open(name); err == nil {
				_ = f.Close()
				files.ServeHTTP(w, r)
				return
			}
		}
		serveSPAIndex(w, dist)
	}
}

func serveSPAIndex(w http.ResponseWriter, dist fs.FS) {
	b, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		http.Error(w, "frontend build missing (run `bun run build` in web/)", http.StatusInternalServerError)
		return
	}
	// No theme injection needed: Onyx tokens are static in the bundled
	// index.css, so the first paint is already correct (no flash).
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
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
			w.Header().Set("WWW-Authenticate", `Basic realm="lasso", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withAuthExcept is withAuth that lets requests under exempt bypass auth. Used to
// keep /mcp open (its consumers — agent sessions — don't carry UI credentials)
// while the rest of the app stays gated. A path equal to exempt or under
// exempt+"/" is matched, so "/mcp" and "/mcp/…" pass but a sibling like
// "/mcp-foo" does not.
func withAuthExcept(next http.Handler, user, pass string, enabled bool, exempt string) http.Handler {
	if !enabled {
		return next
	}
	gated := withAuth(next, user, pass, enabled)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == exempt || strings.HasPrefix(r.URL.Path, exempt+"/") {
			next.ServeHTTP(w, r)
			return
		}
		gated.ServeHTTP(w, r)
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
