// Command lasso serves a two-column web UI:
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

// distFS holds the built React + shadcn/ui frontend (web/dist), embedded into
// the binary so a single executable still serves the whole UI. The `all:`
// prefix includes files whose names begin with "_" or "." Build the frontend
// (`bun run build` in web/) before `go build` — `mise run build` enforces that
// order. Favicons live in the build (copied from web/public), so there's no
// separate /static/ route anymore.
//
//go:embed all:web/dist
var distFS embed.FS

var (
	listenAddr  = flag.String("listen", defaultListenAddr, "address for the web server (loopback by default — the terminal is a writable shell)")
	ttydPort    = flag.Int("ttyd-port", 7682, "loopback port ttyd listens on")
	herdrSock   = flag.String("herdr-sock", defaultSock(), "path to the herdr unix socket")
	termCmd     = flag.String("term-cmd", "herdr", "command ttyd runs in the terminal")
	termNice    = flag.Int("term-nice", 0, "if non-zero, launch the herdr terminal at this nice level (reset-on-fork + nice, needs RLIMIT_NICE); 0 disables")
	termNoSwap  = flag.Bool("term-no-swap", false, "launch the herdr terminal in a transient systemd scope with MemorySwapMax=0 so its pages are never swapped out (mirrors the ccp alias)")
	shellCmd    = flag.String("shell-cmd", "", "command for the out-of-herdr Terminal tab (right column); empty = $SHELL, then bash, then sh")
	spawnTtyd   = flag.Bool("spawn-ttyd", true, "spawn and supervise ttyd as a child process")
	pollEvery   = flag.Duration("poll", 2*time.Second, "fallback poll interval for cwd changes")
	allowNoAuth = flag.Bool("insecure-no-auth", false, "permit a non-loopback bind without auth (tailnet-only use; never on a public interface)")
	devMode     = flag.Bool("dev", false, "dev mode: fall forward to the next free web port if the requested one is busy (so multiple instances coexist). The frontend itself is served by the Vite dev server with hot reload — see `mise run dev`.")
	themeName   = flag.String("theme", "auto", "color theme: \"auto\" follows herdr's config.toml live, or force a herdr theme name — dark: catppuccin/tokyo-night/dracula/nord/gruvbox/one-dark/solarized/kanagawa/rose-pine/vesper/terminal; light: catppuccin-latte/tokyo-night-day/gruvbox-light/one-light/solarized-light/kanagawa-lotus/rose-pine-dawn")
)

// theme is resolved at startup (mirroring herdr's config) and drives both the
// embedded terminal's palette and the sidebar CSS. The hub re-resolves it live
// (see hub.curTheme); this global only seeds the initial page + ttyd spawn.
var theme resolvedTheme

// themePayload is the JSON served at /api/theme: the resolved theme's CSS
// variables (for the sidebar) and xterm.js ITheme (for the live terminal), so
// the browser can repaint both when herdr's theme changes without a reload.
type themePayload struct {
	Name       string          `json:"name"`
	Resolved   string          `json:"resolved"`
	Customized bool            `json:"customized"`
	CSS        string          `json:"css"`   // :root declaration lines
	Xterm      json.RawMessage `json:"xterm"` // xterm.js ITheme object
}

func defaultSock() string {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "herdr", "herdr.sock")
}

// runServer is the foreground HTTP server — the historical `./lasso` behavior.
// main() (cli.go) dispatches here for a bare invocation or the `serve`
// subcommand; the CLI subcommands (start/stop/restart/update/doctor) never reach
// it. It parses flags from os.Args, so `serve` strips its own arg first.
func runServer() {
	flag.Parse()

	// Start out driving the local herdr daemon. The footer's host switcher swaps
	// this for a remoteBackend (and back) at runtime via /api/host.
	setBackend(&localBackend{sock: *herdrSock})

	// Open the host-local state DB (~/.lasso/lasso.db), migrating a legacy
	// config.yaml on first run. Fatal if it can't open — the creator depends on it.
	if err := openDB(); err != nil {
		log.Fatalf("open state db: %v", err)
	}
	defer db.Close()

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// When we spawn ttyd ourselves, give each instance its own private unix
	// socket (keyed by PID) instead of a shared TCP port — so a prod instance
	// and several dev instances can run at once without ever colliding on a
	// port or, worse, silently proxying onto each other's terminal. Only the
	// external-ttyd path (-spawn-ttyd=false) still uses *ttydPort. We resolve the
	// path here (the proxy needs it) but defer the spawn until after the web port
	// binds, so a startup failure doesn't leak an orphaned ttyd.
	// Two ttyds when we spawn our own: the herdr terminal (/terminal/) and a plain
	// out-of-herdr shell (/shell/, the right-column Terminal tab). Each gets its
	// own private unix socket keyed by PID. The external-ttyd path
	// (-spawn-ttyd=false) only wires the herdr terminal to *ttydPort; the shell
	// terminal is viewer-spawned only, so it's absent in that mode.
	var ttydSock, shellSock string
	if *spawnTtyd {
		ttydSock = filepath.Join(os.TempDir(), fmt.Sprintf("lasso-ttyd-%d.sock", os.Getpid()))
		shellSock = filepath.Join(os.TempDir(), fmt.Sprintf("lasso-shell-%d.sock", os.Getpid()))
	}

	hub := newHub()
	srvHub = hub
	srvCtx = ctx
	go hub.run(ctx)

	// handles WS upgrade natively (the hijacked conn is dialed via Transport too)
	var proxy *httputil.ReverseProxy
	if *spawnTtyd {
		proxy = unixSocketProxy(ttydSock)
	} else {
		target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", *ttydPort))
		proxy = httputil.NewSingleHostReverseProxy(target)
	}

	mux := http.NewServeMux()
	mux.Handle("/terminal/", proxy)
	if *spawnTtyd {
		mux.Handle("/shell/", unixSocketProxy(shellSock))
	}
	mux.HandleFunc("/api/active", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, hub.snapshot())
	})
	mux.HandleFunc("/api/theme", func(w http.ResponseWriter, r *http.Request) {
		rt := hub.themeSnapshot()
		writeJSON(w, themePayload{
			Name:       rt.Name,
			Resolved:   rt.Resolved,
			Customized: rt.Customized,
			CSS:        rt.cssVars(),
			Xterm:      json.RawMessage(rt.xtermJSON()),
		})
	})
	mux.HandleFunc("/api/events", hub.serveSSE)
	mux.HandleFunc("/api/files", serveFiles)
	mux.HandleFunc("/api/file", serveFile)
	mux.HandleFunc("/api/file-delete", serveFileDelete)
	mux.HandleFunc("/api/file-rename", serveFileRename)
	mux.HandleFunc("/api/file-write", serveFileWrite)
	mux.HandleFunc("/api/file-upload", serveFileUpload)
	mux.HandleFunc("/api/panes", servePanes)
	mux.HandleFunc("/api/grid", serveGrid)
	mux.HandleFunc("/api/grid/term", serveGridTerm)
	mux.HandleFunc("/api/grid/term-touch", serveGridTermTouch)
	mux.HandleFunc("/api/grid/term-release", serveGridTermRelease)
	mux.HandleFunc("/api/grid/term-release-all", serveGridTermReleaseAll)
	mux.HandleFunc("/api/grid/rename", serveGridRename)
	mux.HandleFunc("/api/grid/close", serveGridClose)
	mux.HandleFunc("/grid-term/", serveGridTermProxy)
	mux.HandleFunc("/api/ui-state", serveUIState)
	mux.HandleFunc("/api/focus", serveFocus)
	mux.HandleFunc("/api/rename", serveRename)
	mux.HandleFunc("/api/workspace-rename", serveWorkspaceRename)
	mux.HandleFunc("/api/close", serveClose)
	mux.HandleFunc("/api/paste-image", servePasteImage)
	mux.HandleFunc("/api/diff", serveDiff)
	mux.HandleFunc("/api/diff-file", serveDiffFile)
	mux.HandleFunc("/api/version", serveVersion)
	mux.HandleFunc("/api/hosts", serveHosts)
	mux.HandleFunc("/api/host", serveHostSwitch)
	mux.HandleFunc("/api/agent-config", serveAgentConfig)
	mux.HandleFunc("/api/repo-config", serveRepoConfig)
	mux.HandleFunc("/api/repos", serveRepos)
	mux.HandleFunc("/api/repo-branches", serveRepoBranches)
	mux.HandleFunc("/api/create-agent", serveCreateAgent)
	mux.HandleFunc("/api/agent-upload", serveAgentUpload)
	mux.HandleFunc("/api/host-update", serveHostUpdate)
	mux.HandleFunc("/api/host-provision", serveHostProvision)
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

	// Spawn ttyd only after the web port is ours — so a busy-port exit above
	// never leaves an orphaned ttyd behind (its cleanup is tied to ctx, which
	// log.Fatalf bypasses).
	if *spawnTtyd {
		// Each terminal is owned by a manager so it can be respawned with a new
		// command when the active host changes (left: herdr / `herdr --remote`,
		// right: local shell / `ssh <host>`). The first spawn here runs the local
		// host's commands; a host switch later restarts both via the managers.
		terminals.herdr = newTtydManager(ctx, ttydSock, "/terminal")
		terminals.shell = newTtydManager(ctx, shellSock, "/shell")
		if err := terminals.herdr.restart(termPrefix()+curBackend().TermCmd(), curBackend().TermEnv()); err != nil {
			log.Fatalf("ttyd: %v", err)
		}
		// Out-of-herdr shell: env stripped of the HERDR_* session markers so
		// commands like `herdr update` (which refuse to run inside a session) work.
		if err := terminals.shell.restart(curBackend().ShellCmd(), outsideHerdrEnv()); err != nil {
			log.Fatalf("ttyd (shell): %v", err)
		}
	}

	srv := &http.Server{Handler: handler}
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
	if *spawnTtyd {
		log.Printf("terminal: ttyd@%s running %q (proxied at /terminal/)", ttydSock, *termCmd)
		log.Printf("shell:    ttyd@%s running %q (proxied at /shell/)", shellSock, shellCommand())
	} else {
		log.Printf("terminal: ttyd@127.0.0.1:%d (external) running %q (proxied at /terminal/)", *ttydPort, *termCmd)
	}
	log.Printf("herdr:    %s", *herdrSock)
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

// shellCommand resolves the command for the out-of-herdr Terminal tab:
// -shell-cmd if set, else $SHELL, else bash, else sh.
func shellCommand() string {
	if c := strings.TrimSpace(*shellCmd); c != "" {
		return c
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	if _, err := exec.LookPath("bash"); err == nil {
		return "bash"
	}
	return "sh"
}

// startTtyd spawns one ttyd serving command under basePath on its own private
// unix socket. command is split on whitespace into the child argv. env, if
// non-nil, overrides the child environment (the shell terminal passes
// outsideHerdrEnv); nil inherits the viewer's env.
func startTtyd(ctx context.Context, sock, basePath, command string, env []string) error {
	return startTtydArgv(ctx, sock, basePath, strings.Fields(command), env)
}

// startTtydArgv is startTtyd with the child command given as an explicit argv,
// for commands that can't survive whitespace-splitting — notably the grid's
// remote attach (`ssh -tt <host> "$SHELL -lc 'herdr terminal attach …'"`), whose
// final argument must reach ssh as one token.
func startTtydArgv(ctx context.Context, sock, basePath string, cmdArgv []string, env []string) error {
	// Bind a private unix socket (one per instance) rather than a shared TCP
	// port, so concurrent prod/dev instances can't collide or cross-connect.
	// Clear any stale socket left by a crashed prior run with this PID so ttyd
	// can bind.
	_ = os.Remove(sock)

	// The xterm.js ITheme (background/foreground/cursor + 16 ANSI colors) is
	// derived from herdr's selected theme, so the terminal palette lines up
	// with herdr's chrome and the sidebar. Passed to ttyd via `-t theme=<json>`,
	// which forwards it to xterm.js in the browser. Seed from the hub's *live*
	// theme (the global `theme` is only the startup snapshot) so a terminal
	// spawned after a theme change — e.g. a Grid cell, or a host-switch respawn —
	// starts on the current palette rather than flashing the stale one before the
	// browser re-applies it.
	xtheme := theme.xtermJSON()
	if srvHub != nil {
		xtheme = srvHub.themeSnapshot().xtermJSON()
	}
	args := []string{
		"-i", sock, // private unix socket (ttyd accepts a socket path here)
		"-b", basePath, // base path so assets/ws resolve under the proxy
		"-W",                           // writable
		"-t", "disableLeaveAlert=true", // no confirm dialog inside the iframe
		"-t", "fontSize=13",
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

// ---------------------------------------------------------------------------
// herdr socket client
// ---------------------------------------------------------------------------

// herdrError is a structured error returned by herdr's socket API
// (e.g. {"code":"pane_not_found","message":"pane X not found"}). Callers can
// inspect Code (via errors.As) to react to specific conditions — notably to
// treat an already-gone pane as a no-op rather than a hard failure.
type herdrError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *herdrError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("herdr error %s: %s", e.Code, e.Message)
	}
	return "herdr error: " + e.Code
}

// herdrCall does one request/response round-trip against the active host's herdr
// socket. The dial/encode/decode logic lives in herdrCallSock (backend.go), which
// both backends share; this routes to whichever host is active.
func herdrCall(method string, params any) (json.RawMessage, error) {
	return curBackend().HerdrCall(method, params)
}

type pane struct {
	PaneID        string `json:"pane_id"`
	TerminalID    string `json:"terminal_id"` // herdr terminal handle, for direct `terminal attach`
	WorkspaceID   string `json:"workspace_id"`
	TabID         string `json:"tab_id"`
	Cwd           string `json:"cwd"`            // the shell's launch dir — stale once an agent owns the pane
	ForegroundCwd string `json:"foreground_cwd"` // herdr-resolved cwd of the pane's foreground process; "" when unresolvable
	Focused       bool   `json:"focused"`
	Agent         string `json:"agent"`
	AgentStatus   string `json:"agent_status"`
}

// paneCwd is the best cwd for a pane. For a plain shell, herdr's foreground_cwd
// (the live cwd of whatever process owns the terminal) tracks the user's cd's
// and wins. For an AGENT pane, the shell launch dir is the agent's project root
// and is what the file viewer should follow: the agent's foreground process is
// often a transient subprocess (e.g. a plugin under ~/.claude/plugins/cache)
// whose cwd would otherwise drag the viewer away from the worktree. So agents
// prefer the launch cwd, using foreground_cwd only when herdr reports none.
// (herdr added foreground_cwd in 0.6.5, superseding the viewer's old
// /proc-scraping workaround.)
func paneCwd(p pane) string {
	if p.Agent != "" {
		if p.Cwd != "" {
			return p.Cwd
		}
		return p.ForegroundCwd
	}
	if p.ForegroundCwd != "" {
		return p.ForegroundCwd
	}
	return p.Cwd
}

// paneCwdUsesForeground reports whether paneCwd returned the foreground cwd
// rather than the shell launch cwd (drives Active.CwdSource).
func paneCwdUsesForeground(p pane) bool {
	if p.Agent != "" {
		return p.Cwd == "" && p.ForegroundCwd != ""
	}
	return p.ForegroundCwd != ""
}

// pane.list is by far herdr's most expensive method: as of 0.6.5 it resolves
// every pane's foreground_cwd via the TTY + /proc on each call (~0.5–1.5s for a
// busy session), versus <10ms for workspace.list/tab.list. The viewer hits it
// from both the active-pane refresh loop and the grid endpoint, so a short
// single-flight cache keeps a focus event, the periodic poll, and a grid fetch
// that land close together from each paying the full cost. Event-driven
// refreshes invalidate the cache first (see invalidatePaneList) so focus
// changes never serve a stale snapshot.
var paneListCache struct {
	mu   sync.Mutex
	at   time.Time
	data json.RawMessage
	err  error
}

const paneListTTL = 400 * time.Millisecond

func herdrPaneList() (json.RawMessage, error) {
	paneListCache.mu.Lock()
	defer paneListCache.mu.Unlock()
	if !paneListCache.at.IsZero() && time.Since(paneListCache.at) < paneListTTL {
		return paneListCache.data, paneListCache.err
	}
	// The call is made under the lock on purpose: concurrent callers coalesce
	// onto this one in-flight request rather than firing parallel slow calls.
	data, err := herdrCall("pane.list", map[string]any{})
	paneListCache.at = time.Now()
	paneListCache.data, paneListCache.err = data, err
	return data, err
}

// invalidatePaneList drops the cached pane.list so the next call refetches. The
// hub calls this on every herdr event: an event means pane state changed, so a
// cached snapshot would be stale.
func invalidatePaneList() {
	paneListCache.mu.Lock()
	paneListCache.at = time.Time{}
	paneListCache.mu.Unlock()
}

type workspace struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"` // display order; changes when workspaces are reordered
	Focused     bool   `json:"focused"`
}

// Active is the state pushed to the browser.
type Active struct {
	PaneID         string `json:"pane_id"`
	Cwd            string `json:"cwd"`
	CwdSource      string `json:"cwd_source"` // "foreground" (herdr's resolved foreground-process cwd) | "shell" (herdr's shell launch cwd, used when foreground is unresolvable)
	WorkspaceID    string `json:"workspace_id"`
	WorkspaceLabel string `json:"workspace_label"`
	TabID          string `json:"tab_id"`
	TabLabel       string `json:"tab_label"`
	Agent          string `json:"agent"`
	AgentStatus    string `json:"agent_status"`
	PanesRev       int    `json:"panes_rev"` // bumps when the pane-grid layout (workspace order/membership) changes
	ThemeRev       int    `json:"theme_rev"` // bumps when herdr's resolved theme changes (config.toml edited)
	HerdrUp        bool   `json:"herdr_up"`  // false when herdr's socket is unreachable; the rest of the struct is then last-known (stale)
	Host           string `json:"host"`      // active host: "local" or an ssh-config alias
	TermRev        int    `json:"term_rev"`  // bumps on host switch so the browser reloads the terminal iframes
}

// fetchActive returns the focused-pane state plus a layout signature. The
// signature captures workspace order + pane membership (see layoutSignature), so
// the caller can detect when the pane grid needs to re-render — e.g. after a
// workspace is reordered in herdr — independently of focus changes.
func fetchActive() (Active, string, error) {
	res, err := herdrPaneList()
	if err != nil {
		return Active{}, "", err
	}
	var pl struct {
		Panes []pane `json:"panes"`
	}
	if err := json.Unmarshal(res, &pl); err != nil {
		return Active{}, "", err
	}

	// workspace.list does double duty: label the focused workspace and feed the
	// layout signature (so a reorder/rename of a workspace is detected).
	var wl struct {
		Workspaces []workspace `json:"workspaces"`
	}
	if res, err := herdrCall("workspace.list", map[string]any{}); err == nil {
		_ = json.Unmarshal(res, &wl)
	}
	sig := layoutSignature(pl.Panes, wl.Workspaces)

	var fp *pane
	for i := range pl.Panes {
		if pl.Panes[i].Focused {
			fp = &pl.Panes[i]
			break
		}
	}
	if fp == nil {
		// herdr is reachable but has no focused pane — e.g. a freshly started
		// server with no session/workspaces yet (common right after a host
		// switch to a host whose herdr was just (re)started). That's "up but
		// empty", NOT down: returning an error here would make the hub mark
		// HerdrUp=false and never flip the active-host display. Return a valid
		// empty Active (with the layout signature) so the success path runs,
		// marks herdr up, and reflects the active host with an empty pane/cwd.
		return Active{}, sig, nil
	}
	a := Active{
		PaneID: fp.PaneID, Cwd: paneCwd(*fp), CwdSource: "shell", WorkspaceID: fp.WorkspaceID,
		TabID: fp.TabID, Agent: fp.Agent, AgentStatus: fp.AgentStatus,
	}
	if paneCwdUsesForeground(*fp) {
		a.CwdSource = "foreground"
	}
	a.TabLabel = tabLabel(fp.TabID)
	for _, w := range wl.Workspaces {
		if w.WorkspaceID == a.WorkspaceID {
			a.WorkspaceLabel = w.Label
		}
	}
	return a, sig, nil
}

// layoutSignature is a deterministic string of the workspace order (number +
// id + label) and pane→workspace/tab membership. It deliberately omits focus and
// cwd, so it changes when workspaces are reordered/renamed or panes are
// added/removed/moved — but NOT on a mere focus change (the grid's focus
// highlight is updated separately, without a full reload).
func layoutSignature(panes []pane, wss []workspace) string {
	ws := append([]workspace(nil), wss...)
	sort.Slice(ws, func(i, j int) bool { return ws[i].Number < ws[j].Number })
	var sb strings.Builder
	for _, w := range ws {
		fmt.Fprintf(&sb, "%d:%s:%s;", w.Number, w.WorkspaceID, w.Label)
	}
	sb.WriteByte('|')
	keys := make([]string, 0, len(panes))
	for _, p := range panes {
		keys = append(keys, p.PaneID+":"+p.WorkspaceID+":"+p.TabID)
	}
	sort.Strings(keys)
	sb.WriteString(strings.Join(keys, ";"))
	return sb.String()
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
	res, err := herdrPaneList()
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
			Cwd:            paneCwd(p),
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

// serveWorkspaceRename relabels a workspace (workspace.rename). The Agents grid
// titles each card with its workspace label, so this is what visibly renames it.
func serveWorkspaceRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		Label       string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.WorkspaceID == "" || strings.TrimSpace(req.Label) == "" {
		http.Error(w, "workspace_id and non-empty label required", http.StatusBadRequest)
		return
	}
	if _, err := herdrCall("workspace.rename", map[string]any{"workspace_id": req.WorkspaceID, "label": req.Label}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// Bulk-close resilience knobs. Closing a pane makes herdr recompute layout /
// shift focus / maybe close the tab, so a burst of pane.close calls can race
// that reconfiguration and fail transiently — hence retries plus a little
// pacing so herdr settles between calls. Tuned to stay snappy for a handful of
// panes while clearing the flakiness that used to need a manual retry.
const closeAttempts = 4 // total tries per pane

// vars (not consts) so tests can shrink the waits.
var (
	closeBackoffBase = 40 * time.Millisecond  // 1st retry wait; doubles each time
	closeBackoffMax  = 400 * time.Millisecond // cap per-retry wait
	closePace        = 25 * time.Millisecond  // breather between distinct panes
)

// paneCloser performs a single pane.close round-trip. A package var so tests can
// substitute a fake herdr without a live socket.
var paneCloser = func(id string) error {
	_, err := herdrCall("pane.close", map[string]any{"pane_id": id})
	return err
}

// closePane closes one pane on the active backend (see closePaneWith).
func closePane(ctx context.Context, id string) error {
	return closePaneWith(ctx, paneCloser, id)
}

// closePaneWith closes one pane via closer, absorbing the two flaky cases: a
// transient herdr error (retried with exponential backoff) and a pane that's
// already gone — e.g. cascade-closed when its tab's last sibling was closed —
// which is treated as success since the goal (pane gone) is met. invalid_request
// is our own bug, so it fails fast without burning retries. Honors ctx so a
// client that walks away (closed tab / navigation) doesn't keep us hammering
// herdr. closer is host-specific (the active backend for serveClose, a grid-pool
// backend for serveGridClose) so the same resilience applies on every host.
func closePaneWith(ctx context.Context, closer func(string) error, id string) error {
	var last error
	for attempt := 0; attempt < closeAttempts; attempt++ {
		if attempt > 0 {
			wait := closeBackoffBase << (attempt - 1)
			if wait > closeBackoffMax {
				wait = closeBackoffMax
			}
			if !sleepCtx(ctx, wait) {
				return ctx.Err()
			}
		}
		err := closer(id)
		if err == nil {
			return nil
		}
		var he *herdrError
		if errors.As(err, &he) {
			switch he.Code {
			case "pane_not_found":
				return nil // already gone — idempotent success
			case "invalid_request":
				return err // malformed on our side; retrying won't help
			}
		}
		last = err // transient (dial/timeout/herdr busy): back off and retry
	}
	return last
}

// sleepCtx waits for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// serveClose closes one or more panes (pane.close per id). Closing the last
// pane in a tab closes the tab too. Calls are serialized with retries + pacing
// (see closePane) so a bulk close is resilient to herdr's reconfiguration
// races; any pane that still can't be closed is reported per-id.
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
	ctx := r.Context()
	closed := make([]string, 0, len(req.PaneIDs))
	errs := map[string]string{}
	for i, id := range req.PaneIDs {
		if i > 0 && !sleepCtx(ctx, closePace) { // backpressure between panes
			break
		}
		if err := closePane(ctx, id); err != nil {
			errs[id] = err.Error()
		} else {
			closed = append(closed, id)
		}
	}
	writeJSON(w, map[string]any{"closed": closed, "errors": errs})
}

// ---------------------------------------------------------------------------
// herdr self-update (Settings tab)
// ---------------------------------------------------------------------------

// herdrBinary is the herdr executable to invoke for out-of-session commands
// (version, update) — the first field of -term-cmd (what ttyd runs in the
// terminal), defaulting to "herdr".
func herdrBinary() string {
	if f := strings.Fields(*termCmd); len(f) > 0 {
		return f[0]
	}
	return "herdr"
}

// rlimitNice is RLIMIT_NICE on Linux; Go's syscall package doesn't export it.
const rlimitNice = 13

// canLowerNiceTo reports whether this process may set its nice value to n.
// RLIMIT_NICE caps the most-favorable nice level at (20 - rlim_cur), so we need
// rlim_cur >= 20-n. Compared in the uint64 domain so RLIM_INFINITY reads as
// "allowed" rather than overflowing to a negative int.
func canLowerNiceTo(n int) bool {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(rlimitNice, &rl); err != nil {
		return false
	}
	return uint64(20-n) <= rl.Cur
}

// termPrefix builds a command prefix that launches the herdr terminal so the
// interactive client stays responsive under load. Two independent, opt-in parts:
//
//   - -term-no-swap: run the client in a transient systemd scope with
//     MemorySwapMax=0 so its pages are never paged out (mirrors the `ccp` alias
//     for agents). Outermost, so the scheduling tweaks below apply inside it.
//   - -term-nice N: reset-on-fork (so a server the client might autospawn does
//     not pass the boost down to agent panes) plus nice -N, the latter added
//     only when RLIMIT_NICE permits it (else skipped, so we never emit a
//     "cannot set niceness" warning).
//
// Each part degrades to a no-op if its helper binary is missing. ttyd splits the
// term command on whitespace, so the prefix is space-joined with a trailing space.
func termPrefix() string {
	var p strings.Builder
	if *termNoSwap {
		if _, err := exec.LookPath("systemd-run"); err == nil {
			p.WriteString("systemd-run --user --scope --quiet -p MemorySwapMax=0 ")
		}
	}
	if *termNice != 0 {
		if _, err := exec.LookPath("chrt"); err == nil {
			p.WriteString("chrt --other --reset-on-fork 0 ")
			if canLowerNiceTo(*termNice) {
				if _, err := exec.LookPath("nice"); err == nil {
					fmt.Fprintf(&p, "nice -n %d ", *termNice)
				}
			}
		}
	}
	return p.String()
}

// outsideHerdrEnv returns the current environment minus the markers herdr uses
// to detect it's running *inside* a session (HERDR_ENV is set to "1" in every
// pane; HERDR_PANE_ID / HERDR_SESSION identify the pane/session). The viewer's
// out-of-herdr shell terminal runs with this env so commands that refuse to run
// inside a session — notably `herdr update` — work there, even when the viewer
// itself was launched from a herdr pane and inherited the markers.
func outsideHerdrEnv() []string {
	drop := map[string]bool{"HERDR_ENV": true, "HERDR_PANE_ID": true, "HERDR_SESSION": true}
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if k, _, ok := strings.Cut(kv, "="); ok && drop[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// lassoHerdrProtocol is the herdr wire-protocol version this lasso build targets:
// the protocol of the *released* herdr lasso is developed and tested against
// (herdr 0.6.5 → protocol 11), NOT whatever the herdr source tree is mid-bumping
// to. Bump it in lockstep when lasso adopts a newer herdr release. We target one
// protocol exactly — no backwards compatibility — so a mismatch in either
// direction reads as incompatible. The Settings tab compares it against the
// protocol the installed herdr daemon actually speaks (from a socket ping) so a
// drifted install — where terminals/RPC silently break — is visible.
//
// Pinned back to 11 (herdr 0.6.5): 0.6.6 has a performance regression, so the
// fleet is rolled back to 0.6.5 for the time being.
const lassoHerdrProtocol = 11

// versionInfo is the /api/version payload: the herdr socket protocol this lasso
// build targets, the protocol the installed herdr daemon reports over its socket,
// that daemon's version string (display only), and whether the two protocols match.
// Err carries why the herdr protocol couldn't be read (daemon down, socket gone) so
// the tab can say so rather than falsely claim a mismatch.
type versionInfo struct {
	LassoProtocol int    `json:"lasso_protocol"`
	LassoVersion  string `json:"lasso_version"`
	HerdrProtocol int    `json:"herdr_protocol"`
	HerdrVersion  string `json:"herdr_version,omitempty"`
	Compatible    bool   `json:"compatible"`
	Updatable     bool   `json:"updatable"`
	// UpdateState (only meaningful when Updatable) says whether the running build
	// is behind main: "available" (a newer commit is waiting to be built),
	// "current" (already on main's tip), or "unknown" (can't tell — the UI then
	// shows the update button anyway so the action never vanishes). CommitsBehind
	// counts how far behind main, when known.
	UpdateState   string `json:"update_state,omitempty"`
	CommitsBehind int    `json:"commits_behind,omitempty"`
	// LatestVersion is the newest published GitHub release tag, set only for a
	// release-binary install (not the supervised/pitchfork checkout, which tracks
	// main by commit instead). When it's newer than this build the Settings tab
	// shows an "update available" hint pointing at `lasso update`.
	LatestVersion string `json:"latest_version,omitempty"`
	Err           string `json:"err,omitempty"`
}

// serveVersion reports whether the installed herdr speaks the same socket protocol
// this lasso build targets. It pings the local herdr socket fresh on every request
// — so the tab's refresh button re-checks a daemon that has since restarted —
// rather than reusing the once-cached localProtocol().
func serveVersion(w http.ResponseWriter, r *http.Request) {
	vi := versionInfo{
		LassoProtocol: lassoHerdrProtocol,
		LassoVersion:  lassoVersion(),
		Updatable:     selfUpdateAvailable(),
	}
	if vi.Updatable {
		// Supervised checkout: compare the build commit to main's tip (local git).
		vi.UpdateState, vi.CommitsBehind = selfUpdateStatus()
	} else if !*devMode {
		// Release binary: compare this build's version to the latest GitHub release
		// (non-blocking — the tag is "" until the background fetch lands, then the
		// next poll picks it up). Dev/worktree runs skip this; they update by rebuild.
		if latest, ok := cachedLatestTag(); ok {
			vi.LatestVersion = latest
			if semverNewer(lassoSemver, latest) {
				vi.UpdateState = "available"
			} else {
				vi.UpdateState = "current"
			}
		}
	}
	if v, p, err := herdrPinger(); err != nil {
		vi.Err = err.Error()
	} else {
		vi.HerdrVersion = v
		vi.HerdrProtocol = p
		vi.Compatible = p == lassoHerdrProtocol
	}
	writeJSON(w, vi)
}

// herdrPinger reports the installed (local) herdr daemon's version and protocol.
// A seam over herdrPing(*herdrSock) so serveVersion is unit-testable without a
// live daemon. It deliberately pings the local socket, not the active backend's,
// so the Settings tab reflects the local lasso↔herdr install even when a remote
// host is selected.
var herdrPinger = func() (string, int, error) { return herdrPing(*herdrSock) }

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
// type), writes it to pasteImageDir() with a timestamped name, and returns the
// absolute path. The browser then inserts that path at the terminal cursor.
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
	// Target the selected host (?host=, default active) so the pasted image lands
	// where the agent will run and the path inserted into the description
	// resolves on that host.
	be, err := reqHostBackend(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
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

// subscribeEvents opens a long-lived connection subscribed to herdr events and
// signals `trigger` whenever one arrives (the hub then re-fetches state).
// Reconnects on failure. Beyond the *.focused events that drive the active-pane
// view, it listens to the workspace/tab/pane lifecycle events — notably
// workspace.updated, which fires when workspaces are reordered — so the pane
// grid's order and membership stay live.
func subscribeEvents(ctx context.Context, trigger chan<- struct{}) {
	for ctx.Err() == nil {
		conn, err := net.Dial("unix", curBackend().HerdrSock())
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		// Close the conn when ctx is cancelled (host switch / shutdown) so the
		// blocking Scan below unblocks and this goroutine exits promptly instead
		// of lingering on the now-stale socket. A second deferred Close on the
		// happy path is harmless (Close is idempotent).
		stop := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				conn.Close()
			case <-stop:
			}
		}()
		sub := `{"id":"ui-sub","method":"events.subscribe","params":{"subscriptions":[` +
			`{"type":"workspace.created"},{"type":"workspace.updated"},{"type":"workspace.renamed"},` +
			`{"type":"workspace.closed"},{"type":"workspace.focused"},` +
			`{"type":"tab.created"},{"type":"tab.closed"},{"type":"tab.renamed"},{"type":"tab.focused"},` +
			`{"type":"pane.created"},{"type":"pane.closed"},{"type":"pane.exited"},{"type":"pane.focused"}` +
			`]}}` + "\n"
		if _, err := conn.Write([]byte(sub)); err != nil {
			close(stop)
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
		close(stop)
		conn.Close()
		if ctx.Err() != nil {
			return
		}
		time.Sleep(time.Second)
	}
}

// ---------------------------------------------------------------------------
// SSE hub
// ---------------------------------------------------------------------------

type hub struct {
	mu       sync.RWMutex
	cur      Active
	rev      int    // pane-grid layout revision (bumped when lastSig changes)
	lastSig  string // last seen layout signature
	themeRev int    // theme revision (bumped when the resolved theme changes)
	termRev  int    // host-switch revision (bumped so the browser reloads terminals)
	curTheme resolvedTheme
	clients  map[chan Active]struct{}

	// Event subscription, restarted against the new socket on a host switch.
	rootCtx   context.Context
	trigger   chan struct{}
	subMu     sync.Mutex
	subCancel context.CancelFunc
}

// newHub seeds the hub's theme with the one resolved at startup, so the first
// poll only bumps themeRev if config.toml has actually changed since boot.
func newHub() *hub {
	// Seed HerdrUp=true so a browser connecting before the first poll doesn't
	// briefly flash the "herdr disconnected" state.
	return &hub{cur: Active{HerdrUp: true}, curTheme: theme, clients: map[chan Active]struct{}{}}
}

// startSub (re)starts the herdr event subscription under a fresh child of the
// hub's root context, cancelling any prior one. Called once at boot and again on
// every host switch so the stream attaches to the new host's (forwarded) socket.
func (h *hub) startSub() {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	if h.subCancel != nil {
		h.subCancel()
	}
	sctx, cancel := context.WithCancel(h.rootCtx)
	h.subCancel = cancel
	go subscribeEvents(sctx, h.trigger)
}

// kick forces a near-immediate refresh (non-blocking), used after a host switch
// so the new host's state is pushed without waiting for the poll tick.
func (h *hub) kick() {
	select {
	case h.trigger <- struct{}{}:
	default:
	}
}

// bumpTermRev increments the terminal-reload counter so the next SSE frame tells
// the browser to reload the terminal iframes (their ttyd sessions were respawned
// against the new host).
func (h *hub) bumpTermRev() {
	h.mu.Lock()
	h.termRev++
	h.mu.Unlock()
}

func (h *hub) snapshot() Active             { h.mu.RLock(); defer h.mu.RUnlock(); return h.cur }
func (h *hub) themeSnapshot() resolvedTheme { h.mu.RLock(); defer h.mu.RUnlock(); return h.curTheme }

func (h *hub) run(ctx context.Context) {
	h.rootCtx = ctx
	h.trigger = make(chan struct{}, 1)
	trigger := h.trigger
	h.startSub()
	ticker := time.NewTicker(*pollEvery)
	defer ticker.Stop()

	refresh := func() {
		a, sig, err := fetchActive()
		if err != nil {
			// herdr's socket is unreachable (closed in the terminal). Keep the
			// last-known state but mark it stale, and notify clients once on the
			// up->down transition so the sidebar can show a disconnected cue.
			h.mu.Lock()
			var down Active
			var clients []chan Active
			if h.cur.HerdrUp {
				h.cur.HerdrUp = false
				down = h.cur
				for c := range h.clients {
					clients = append(clients, c)
				}
			}
			h.mu.Unlock()
			for _, c := range clients {
				select {
				case c <- down:
				default:
				}
			}
			return
		}
		a.HerdrUp = true
		// Re-resolve herdr's theme from config.toml every tick (cheap file read +
		// parse) so an edit to [theme].name is picked up live. Done outside the
		// lock to avoid holding it during I/O.
		rt := loadHerdrTheme(*themeName)
		h.mu.Lock()
		if sig != h.lastSig {
			h.lastSig = sig
			h.rev++
		}
		if rt != h.curTheme {
			h.curTheme = rt
			h.themeRev++
			if rt.Customized {
				log.Printf("theme:    reloaded %q -> %s (+custom overrides)", rt.Name, rt.Resolved)
			} else {
				log.Printf("theme:    reloaded %q -> %s", rt.Name, rt.Resolved)
			}
		}
		a.PanesRev = h.rev
		a.ThemeRev = h.themeRev
		a.TermRev = h.termRev
		a.Host = curBackend().Name()
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

	// Coalesce bursts of herdr events: rapid focus changes (cycling tabs, moving
	// panes) each emit an event, and refresh() is dominated by the ~1s pane.list.
	// A short debounce collapses a burst into a single refresh once it settles —
	// capturing the final state — instead of running one slow refresh per event.
	refresh()
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
			invalidatePaneList() // an event means pane state changed; refetch, don't serve the cache
			if debounce == nil {
				debounce = time.After(eventDebounce)
			}
		case <-debounce:
			debounce = nil
			refresh()
		case <-ticker.C:
			refresh()
		}
	}
}

// eventDebounce is the quiet window the hub waits after the first herdr event
// before refreshing, so a burst of events yields one refresh of the settled
// state. Kept well under human perception so a single focus change still feels
// immediate.
const eventDebounce = 120 * time.Millisecond

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
	// Inject the server-resolved herdr theme into <head> so the first paint
	// matches config.toml instead of flashing index.css's fallback palette
	// before the SPA boots and fetches /api/theme. index.html is served
	// no-store (below), so each reload re-injects the current resolved theme.
	// A missing </head> (shouldn't happen with the Vite build) leaves the HTML
	// untouched — graceful fallback.
	if srvHub != nil {
		style := `<style id="lasso-theme-boot">` + srvHub.themeSnapshot().cssVarsRoot() + `</style>`
		b = []byte(strings.Replace(string(b), "</head>", style+"</head>", 1))
	}
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
			w.Header().Set("WWW-Authenticate", `Basic realm="herdr", charset="UTF-8"`)
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
