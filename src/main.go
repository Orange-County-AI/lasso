// Command lasso serves a two-column web UI:
//
//	left  = luvus running inside a ttyd terminal (embedded in an iframe)
//	right = a file viewer that follows the *focused pane's* working directory,
//	        live — the harness's own cwd when an agent owns the pane (see
//	        agentcwd.go), the pane's otherwise
//
// It talks to the luvus server over its newline-delimited JSON unix socket
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
	"mime"
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
	luvusSock   = flag.String("luvus-sock", defaultSock(), "path to the Luvus UHP unix socket")
	termCmd     = flag.String("term-cmd", luvusCLI(), "command ttyd runs in the terminal")
	termNice    = flag.Int("term-nice", 0, "if non-zero, launch the luvus terminal at this nice level (reset-on-fork + nice, needs RLIMIT_NICE); 0 disables")
	termNoSwap  = flag.Bool("term-no-swap", false, "launch the luvus terminal in a transient systemd scope with MemorySwapMax=0 so its pages are never swapped out (mirrors the ccp alias)")
	shellCmd    = flag.String("shell-cmd", "", "command for the out-of-luvus Terminal tab (right column); empty = $SHELL, then bash, then sh")
	spawnTtyd   = flag.Bool("spawn-ttyd", true, "spawn and supervise ttyd as a child process")
	pollEvery   = flag.Duration("poll", 2*time.Second, "fallback poll interval for cwd changes")
	allowNoAuth = flag.Bool("insecure-no-auth", false, "permit a non-loopback bind without auth (tailnet-only use; never on a public interface)")
	// Cloudflare Access gate (accessgate.go). Opt-in per deployment: when set,
	// EVERY route requires a Cf-Access-Authenticated-User-Email header, and a
	// non-loopback bind is permitted without UI_AUTH because the edge identity
	// IS the auth. Only sound behind an edge that strips client-supplied
	// Cf-Access-* headers.
	requireAccessHdr = flag.Bool("require-access-header", envOn("LASSO_REQUIRE_ACCESS_HEADER"),
		"require a Cf-Access-Authenticated-User-Email header on every request (Cloudflare Access in front); env LASSO_REQUIRE_ACCESS_HEADER=1")
	accessEmails = flag.String("access-allowed-emails", os.Getenv("LASSO_ACCESS_ALLOWED_EMAILS"),
		"comma-separated emails allowed through -require-access-header; empty = any Access-authenticated identity")
	// Self-update shells `systemd-run --user` to pull+restart lasso. On a fleet
	// box an agent must not be able to move its own front door — this turns the
	// whole path off (endpoint refuses, UI hides the action).
	disableSelfUpdate = flag.Bool("disable-self-update", envOn("LASSO_DISABLE_SELF_UPDATE"),
		"disable the in-app self-update (git pull + systemctl --user restart); env LASSO_DISABLE_SELF_UPDATE=1")
	devMode = flag.Bool("dev", false, "dev mode: fall forward to the next free web port if the requested one is busy (so multiple instances coexist). The frontend itself is served by the Vite dev server with hot reload — see `mise run dev`.")
)

// theme is resolved at startup (mirroring luvus's config) and drives both the
// embedded terminal's palette and the sidebar CSS. The hub re-resolves it live
// (see hub.curTheme); this global only seeds the initial page + ttyd spawn.
var theme resolvedTheme

// themePayload is the JSON served at /api/theme: the resolved theme's CSS
// variables (for the sidebar) and xterm.js ITheme (for the live terminal), so
// the browser can repaint both when luvus's theme changes without a reload.
type themePayload struct {
	Name       string          `json:"name"`
	Resolved   string          `json:"resolved"`
	Label      string          `json:"label"`
	Appearance string          `json:"appearance"`
	Customized bool            `json:"customized"`
	CSS        string          `json:"css"`   // :root declaration lines
	Xterm      json.RawMessage `json:"xterm"` // xterm.js ITheme object
	// SyncAgentThemes is the server-level toggle for mirroring the theme into
	// agent CLIs' theme files (agentsync.go); flipped via POST /api/theme-set.
	SyncAgentThemes bool `json:"sync_agent_themes"`
	// ThemeSyncOff lists the hosts ("local" or ssh aliases) lasso writes no
	// theme to at all — the per-host opt-out, also flipped via /api/theme-set.
	ThemeSyncOff []string `json:"theme_sync_off"`
}

func defaultSock() string {
	if p := os.Getenv("LUVUS_API_ADDRESS"); p != "" {
		return p
	}
	if p := os.Getenv("LUVUS_SOCKET_PATH"); p != "" {
		return p
	}
	if p := discoverLocalLuvusSocket(); p != "" {
		return p
	}
	root := os.Getenv("LUVUS_HOME")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".luvus")
	}
	return filepath.Join(root, "luvus.sock")
}

// runServer is the foreground HTTP server — the historical `./lasso` behavior.
// main() (cli.go) dispatches here for a bare invocation or the `serve`
// subcommand; the CLI subcommands (start/stop/restart/update/doctor) never reach
// it. It parses flags from os.Args, so `serve` strips its own arg first.
func runServer() {
	flag.Parse()

	// In dev, tee the standard logger to a stable file and interleave
	// browser-posted events into it (see devlog.go / serveClientLog), so backend
	// and frontend logs form one time-ordered stream for debugging.
	if *devMode {
		setupDevLog()
	}

	// Start out driving the local luvus daemon. The footer's host switcher swaps
	// this for a remoteBackend (and back) at runtime via /api/host.
	setDefaultBackend(&localBackend{sock: *luvusSock})

	// Open the host-local state DB (~/.lasso/lasso.db), migrating a legacy
	// config.yaml on first run. Fatal if it can't open — the creator depends on it.
	if err := openDB(); err != nil {
		log.Fatalf("open state db: %v", err)
	}
	defer db.Close()
	// A record still at BootCreating belongs to a create a previous process died
	// in the middle of — mark it failed (but adoptable, see createAgent's resume
	// path) so it surfaces instead of lingering as a phantom.
	sweepInterruptedCreates()

	// Auth credentials come from the environment (UI_AUTH=user:pass), never
	// argv — so they don't leak via `ps`. Safety guard: refuse to bind to a
	// non-loopback address without auth, so this can't accidentally expose a
	// writable shell on a public interface again.
	authUser, authPass, hasAuth := parseAuth(os.Getenv("UI_AUTH"))
	// Same rule for the MCP endpoint's own OAuth credentials (MCP_OAUTH). Read
	// before the route table is built — withMCPAuth is a no-op when it's unset.
	oauthCfg = loadOAuthConfig()
	logOAuthStatus()
	// The Access header gate counts as auth for the non-loopback refusal below:
	// a request without an edge-vouched identity never reaches a handler.
	gate := newAccessGate(*requireAccessHdr, *accessEmails)
	if !isLoopback(*listenAddr) && !hasAuth && !*allowNoAuth && !gate.require {
		log.Fatalf("refusing to listen on non-loopback %q without auth — set UI_AUTH=user:pass, "+
			"pass -require-access-header when Cloudflare Access fronts this hostname, "+
			"or pass -insecure-no-auth to bind bare (only safe on a private interface like tailscale0)", *listenAddr)
	}

	var themeErr error
	theme, themeErr = loadLuvusTheme()
	if themeErr != nil {
		log.Printf("theme:    Luvus unavailable: %v", themeErr)
		theme = unavailableTheme()
	}
	if theme.Customized {
		log.Printf("theme:    %q -> %s (+custom overrides)", theme.Name, theme.Resolved)
	} else {
		log.Printf("theme:    %q -> %s", theme.Name, theme.Resolved)
	}
	// Mirror into local agents' theme files at boot too (the poll only syncs on
	// a theme CHANGE, so without this a sync-logic upgrade — or files drifted
	// while lasso was down — would wait for the next theme switch to converge).
	if themeErr == nil {
		go syncAgentThemesVia(localFsBackend(), theme)
	}

	// Shutdown is sequenced in two stages: the signal context only *starts* a
	// shutdown, while everything long-lived (remote backends, ttyds, reapers,
	// the hub) hangs off ctx, which is cancelled only AFTER the HTTP server has
	// drained in-flight requests. Deriving both from the signal used to tear
	// down the SSH control masters the instant SIGTERM landed, guaranteeing any
	// in-flight remote call (e.g. the New Agent modal's worktree.create riding a
	// forwarded socket) died mid-request and surfaced as a 502 — the exact race
	// a `lasso update` restart hits when a create is in flight.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancelBackends := context.WithCancel(context.Background())
	defer cancelBackends()

	// When we spawn ttyd ourselves, every terminal gets its own private unix
	// socket (keyed by lasso's PID and the host it serves) instead of a shared
	// TCP port — so a prod instance and several dev instances can run at once
	// without ever colliding on a port or, worse, silently proxying onto each
	// other's terminal. Only the external-ttyd path (-spawn-ttyd=false) still
	// uses *ttydPort. The paths belong to the ttydRoles (switch.go), which own
	// one ttyd per host for each of the two roles: the luvus terminal
	// (/terminal/) and a plain out-of-luvus shell (/shell/, the right-column
	// Terminal tab). The spawn is deferred until after the web port binds, so a
	// startup failure doesn't leak an orphaned ttyd. The external-ttyd path only
	// wires the luvus terminal to *ttydPort; the shell terminal is
	// viewer-spawned only, so it's absent in that mode.

	hub := newHub()
	srvHub = hub
	srvCtx = ctx
	go hub.run(ctx)

	// Drain the agent-to-agent message queue (message_agent MCP tool) for the
	// life of the server, delivering into recipient panes as they go idle.
	go messageDispatchLoop(ctx)

	// Notifications: register the transports, then watch the fleet for agents
	// that block waiting on a human. Both are inert until a device subscribes —
	// the watcher's first act each tick is to ask whether anything is listening,
	// and with nothing registered it never polls a host (see notifywatch.go).
	registerNotifTransport(webPushChannel{})
	go startBlockedWatcher(ctx)

	// handles WS upgrade natively (the hijacked conn is dialed via Transport too)
	var proxy *httputil.ReverseProxy
	if *spawnTtyd {
		proxy = ttydProxy(func() *ttydRole { return terminals.luvus })
	} else {
		target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", *ttydPort))
		proxy = httputil.NewSingleHostReverseProxy(target)
	}

	mux := http.NewServeMux()
	mux.Handle("/terminal/", proxy)
	if *spawnTtyd {
		mux.Handle("/shell/", ttydProxy(func() *ttydRole { return terminals.shell }))
	}
	mux.HandleFunc("/api/active", func(w http.ResponseWriter, r *http.Request) {
		a, err := hub.snapshot(requestHost(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, a)
	})
	mux.HandleFunc("/api/theme", func(w http.ResponseWriter, r *http.Request) {
		rt := hub.themeSnapshot()
		writeJSON(w, themePayload{
			Name:            rt.Name,
			Resolved:        rt.Resolved,
			Customized:      rt.Customized,
			CSS:             rt.cssVars(),
			Xterm:           json.RawMessage(rt.xtermJSON()),
			Label:           rt.Label,
			Appearance:      rt.Appearance,
			SyncAgentThemes: syncAgentThemesEnabled(),
			ThemeSyncOff:    themeSyncOffHosts(),
		})
	})
	mux.HandleFunc("/api/theme-set", serveThemeSet)
	mux.HandleFunc("/api/events", hub.serveSSE)
	mux.HandleFunc("/api/files", serveFiles)
	mux.HandleFunc("/api/file", serveFile)
	mux.HandleFunc("/api/file-delete", serveFileDelete)
	mux.HandleFunc("/api/file-rename", serveFileRename)
	mux.HandleFunc("/api/file-write", serveFileWrite)
	mux.HandleFunc("/api/file-upload", serveFileUpload)
	mux.HandleFunc("/api/panes", servePanes)
	mux.HandleFunc("/api/all-panes", serveAllPanes)
	mux.HandleFunc("/api/ui-state", serveUIState)
	mux.HandleFunc("/api/focus", serveFocus)
	mux.HandleFunc("/api/rename", serveRename)
	mux.HandleFunc("/api/workspace-rename", serveWorkspaceRename)
	mux.HandleFunc("/api/close", serveClose)
	mux.HandleFunc("/api/agent/close", serveAgentClose)
	mux.HandleFunc("/api/agent/reopen", serveAgentReopen)
	mux.HandleFunc("/api/agent-history", serveAgentHistory)
	mux.HandleFunc("/api/paste-file", servePasteFile)
	mux.HandleFunc("/api/diff", serveDiff)
	mux.HandleFunc("/api/diff-file", serveDiffFile)
	mux.HandleFunc("/api/version", serveVersion)
	mux.HandleFunc("/api/usage", serveUsage)
	mux.HandleFunc("/api/usage-bar", serveUsageBar)
	mux.HandleFunc("/api/hosts", serveHosts)
	mux.HandleFunc("/api/host", serveHostAttach)
	mux.HandleFunc("/api/agent-config", serveAgentConfig)
	mux.HandleFunc("/api/repo-config", serveRepoConfig)
	mux.HandleFunc("/api/repos", serveRepos)
	mux.HandleFunc("/api/repo-branches", serveRepoBranches)
	mux.HandleFunc("/api/create-agent", serveCreateAgent)
	mux.HandleFunc("/api/auto-title", serveAutoTitle)
	mux.HandleFunc("/api/create-terminal", serveCreateTerminal)
	mux.HandleFunc("/api/workspaces", serveWorkspaces)
	mux.HandleFunc("/api/agent-upload", serveAgentUpload)
	mux.HandleFunc("/api/host-update", serveHostUpdate)
	mux.HandleFunc("/api/host-provision", serveHostProvision)
	mux.HandleFunc("/api/self-update", serveSelfUpdate)
	// Notifications (notify.go): Web Push to a device that registered itself —
	// on iOS, a lasso added to the home screen. /api/push is the Settings tab's
	// view of it; the watcher (notifywatch.go) is what actually produces them.
	mux.HandleFunc("/api/push", servePushConfig)
	mux.HandleFunc("/api/push/subscribe", servePushSubscribe)
	mux.HandleFunc("/api/push/unsubscribe", servePushUnsubscribe)
	mux.HandleFunc("/api/push/test", servePushTest)
	// MCP server: lets an agent session orchestrate other lasso agents over the
	// Model Context Protocol. Mounted here (before the SPA catch-all) and exempt
	// from UI_AUTH below — see withAuthExcept. The handler serves both /mcp and
	// /mcp/… (the Streamable-HTTP transport's own subpaths).
	//
	// withMCPAuth is a no-op unless MCP_OAUTH is set; when it is, /mcp requires a
	// bearer token from lasso's own OAuth server (oauth.go) or the UI_AUTH
	// credentials.
	mcpHandler := withMCPAuth(newMCPHandler(), authUser, authPass, hasAuth)
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)
	// OAuth 2.1 authorization server for /mcp (oauth.go). The discovery
	// documents, dynamic registration, and the token endpoint must be reachable
	// without UI_AUTH — they're the credential-less half of the handshake — but
	// /oauth/authorize deliberately stays gated, so granting a client access
	// requires a human who can already get into lasso.
	mux.HandleFunc("/.well-known/oauth-protected-resource", serveProtectedResourceMetadata)
	// RFC 9728 §3.1: clients whose resource has a path probe the path-suffixed
	// form (…/oauth-protected-resource/mcp) instead of the bare one.
	mux.HandleFunc("/.well-known/oauth-protected-resource/", serveProtectedResourceMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server", serveAuthServerMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server/", serveAuthServerMetadata)
	mux.HandleFunc("/oauth/register", serveOAuthRegister)
	mux.HandleFunc("/oauth/token", serveOAuthToken)
	mux.HandleFunc("/oauth/authorize", serveOAuthAuthorize)
	dist, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		log.Fatalf("dist fs: %v", err)
	}
	// Hashed, content-addressed build assets are immutable → long cache. Every
	// other path falls through to the SPA entry (index.html). In -dev the live
	// frontend is the Vite dev server (HMR) proxied onto these API routes; this
	// embedded copy is what the production binary serves.
	mux.Handle("/assets/", cacheControl(http.FileServer(http.FS(dist))))
	// The service worker is the one asset whose URL never changes, so it is the
	// one that must never be served stale: a lasso behind Cloudflare would
	// otherwise keep an edge copy of it (js is a cacheable extension and the
	// origin sets no policy) for hours past a self-update. must-revalidate on a
	// 3.7 kB file costs a conditional request; a stale worker costs
	// notifications that quietly stop matching the payload the server sends.
	mux.Handle("/sw.js", noStore(http.FileServer(http.FS(dist))))
	mux.Handle("/", serveDist(dist))
	if *devMode {
		log.Printf("dev:      ON — backend only; run the Vite dev server in web/ for the frontend (mise run dev)")
		// Browser log sink — only mounted in dev (unified log; see devlog.go).
		mux.HandleFunc("/api/log", serveClientLog)
	}

	// /mcp carries its own gate (withMCPAuth above — open by default, OAuth when
	// MCP_OAUTH is set; see CLAUDE.md), and the OAuth discovery/token endpoints
	// are meaningless behind a credential wall. Everything else — including
	// /oauth/authorize, which is where consent is actually granted — stays
	// behind UI_AUTH when set.
	handler := gate.wrap(withAuthExcept(mux, authUser, authPass, hasAuth,
		"/mcp",
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-authorization-server",
		"/oauth/register",
		"/oauth/token",
	))

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
		// Each role owns one ttyd PER HOST (left: luvus / `luvus --remote`,
		// right: local shell / `ssh <host>`), so a host switch points the role at
		// another instance instead of respawning one in place. The first spawn
		// here is the local host's pair; a switch spawns the target's on its
		// first visit and reuses it forever after (see switch.go's ttydRole).
		terminals.luvus = newTtydRole(ctx, "ttyd", "/terminal")
		terminals.shell = newTtydRole(ctx, "shell", "/shell")
		// The default host's pair, spawned eagerly so the first tab's iframes
		// find a bound socket. A tab moving to another host spawns that host's
		// pair through POST /api/host (serveHostAttach), and both stay resident.
		// The shell's env is stripped of the LUVUS_* session markers so commands
		// like `luvus update` (which refuse to run inside a session) work.
		if err := ensureTerminals(defaultBackend()); err != nil {
			log.Fatalf("ttyd: %v", err)
		}
		// Retire terminals for hosts that drop out of rotation (see ttydIdle).
		go terminals.luvus.sweepIdle()
		go terminals.shell.sweepIdle()
	}

	// Probe the ssh-config hosts in the background from startup and keep
	// re-probing on an interval, so the host switcher reads a warm store rather
	// than paying for a cold sweep the first time someone opens it.
	startHostRefresher()

	// Eagerly populate the repo/branch caches for every reachable host so the
	// New Agent dialog opens on warm data instead of blocking on ssh. Started
	// here — after the active backend is up — and refreshed on its own interval.
	startCacheWarmer()

	// Reap only Lasso's own orphaned SSH transports.
	startSSHReaper(ctx)

	srv := &http.Server{Handler: handler}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-sigCtx.Done()
		stop() // restore default signal handling: a second Ctrl-C/SIGTERM force-kills
		// Streaming handlers (SSE) watch `draining` and exit immediately, so
		// Shutdown only waits on real work — not on the drain window per se.
		close(draining)
		log.Printf("shutdown: draining in-flight requests (up to %s)", drainTimeout)
		sh, cancel := context.WithTimeout(context.Background(), drainTimeout)
		_ = srv.Shutdown(sh)
		cancel()
		// Only now tear down what in-flight requests depended on.
		cancelBackends()
		closeBackendsOnExit()
	}()

	gate.logStatus(*listenAddr, hasAuth)
	if *disableSelfUpdate {
		log.Printf("update:   self-update DISABLED (-disable-self-update)")
	}
	switch {
	case hasAuth:
		log.Printf("auth:     enabled (basic, user %q)", authUser)
	case gate.require:
		log.Printf("auth:     basic auth off — the %s gate is the only credential", accessEmailHeader)
	case !isLoopback(*listenAddr):
		log.Printf("auth:     DISABLED on non-loopback %s (-insecure-no-auth) — relies on the network being private", *listenAddr)
	default:
		log.Printf("auth:     DISABLED (loopback only)")
	}
	log.Printf("UI:       http://%s", *listenAddr)
	if *spawnTtyd {
		slug := hostSlug(defaultBackend().Name())
		log.Printf("terminal: ttyd running %q (proxied at /terminal/%s/)", *termCmd, slug)
		log.Printf("shell:    ttyd running %q (proxied at /shell/%s/)", shellCommand(), slug)
	} else {
		log.Printf("terminal: ttyd@127.0.0.1:%d (external) running %q (proxied at /terminal/)", *ttydPort, *termCmd)
	}
	log.Printf("luvus:    %s", *luvusSock)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	// Serve returns the moment Shutdown begins; wait for the drain + backend
	// teardown to finish so we don't exit while a request is still completing.
	<-shutdownDone
}

// drainTimeout bounds the graceful-shutdown drain. Long enough for the slow
// mutations worth protecting (worktree.create runs a few seconds, more over a
// forwarded socket), short enough to stay under both launchd's default 20s
// ExitTimeOut and systemd's default 90s TimeoutStopSec before they SIGKILL.
const drainTimeout = 15 * time.Second

// draining is closed when shutdown begins. Long-lived streaming handlers (SSE)
// select on it and exit promptly, so srv.Shutdown waits only on genuinely
// in-flight request work rather than idling out the whole drain window.
var draining = make(chan struct{})

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

// ttydDialWait bounds how long a proxied request waits for the active host's
// ttyd socket. A remount arriving while an instance is still binding — or
// during the pointer flip of a host switch — waits instead of 502ing, which is
// what the browser's iframe reload used to race.
const ttydDialWait = 3 * time.Second

// ttydSlugKey carries the host slug from the Director (which sees the request,
// and so the host in its URL) down to DialContext (which sees only a context).
// The two halves of a reverse proxy cannot otherwise talk, and picking the
// instance now depends on the request rather than on a process-wide pointer.
type ttydSlugKey struct{}

// ttydProxy reverse-proxies /<role>/<slug>/… to THAT host's ttyd over its
// private unix socket. The host in the outbound URL is a placeholder — the
// custom DialContext ignores it and dials the socket. WS upgrades work because
// the hijacked conn is dialed through the same Transport.
//
// The slug in the path is what selects the instance, so two tabs on two hosts
// load two different terminals from the same origin at the same time. It used to
// dial whichever socket the role called "active", which is why a second tab
// could not have a terminal of its own.
//
// The socket is resolved per request, not captured: an instance may not be
// spawned yet when the mux is wired, and may be retired and respawned later.
func ttydProxy(role func() *ttydRole) *httputil.ReverseProxy {
	p := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "ttyd.sock"})
	director := p.Director
	p.Director = func(req *http.Request) {
		director(req)
		// /terminal/<slug>/rest → <slug>. ttyd was spawned with -b
		// /terminal/<slug>, so its own asset and websocket URLs carry the slug
		// too and land back on the same instance; the path is passed through
		// untouched.
		slug := ""
		if parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/"), "/", 3); len(parts) >= 2 {
			slug = parts[1]
		}
		// The outbound URL's host must be UNIQUE PER SLUG. http.Transport pools
		// keep-alive connections by that host, and with a single placeholder for
		// every instance the second host's request was served down the first
		// host's already-open connection — its ttyd answered 404, because the
		// path did not match the base it was spawned with. DialContext ignores
		// the address entirely; this exists only to key the pool.
		req.URL.Host = slug + ".ttyd.invalid"
		*req = *req.WithContext(context.WithValue(req.Context(), ttydSlugKey{}, slug))
	}
	p.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			deadline := time.Now().Add(ttydDialWait)
			for {
				// Re-resolved every pass: an instance still binding its socket
				// (a remount racing its own spawn) is found by a later retry
				// instead of 502ing, which is what this wait is for.
				var err error = errNoTtyd
				slug, _ := ctx.Value(ttydSlugKey{}).(string)
				if path := role().sockForSlug(slug); path != "" {
					var c net.Conn
					if c, err = d.DialContext(ctx, "unix", path); err == nil {
						return c, nil
					}
				}
				if time.Now().After(deadline) {
					return nil, err
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(25 * time.Millisecond):
				}
			}
		},
	}
	return p
}

// errNoTtyd is what a dial reports when the requested host has no terminal at
// all: a spawn that failed, a stale iframe still pointed at a retired host, or a
// request that beat the first spawn.
var errNoTtyd = errors.New("no ttyd for that host")

// shellCommand resolves the command for the out-of-luvus Terminal tab:
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
// outsideLuvusEnv); nil inherits the viewer's env.
func startTtyd(ctx context.Context, sock, basePath, command string, env []string) error {
	// Bind a private unix socket (one per instance) rather than a shared TCP
	// port, so concurrent prod/dev instances can't collide or cross-connect.
	// Clear any stale socket left by a crashed prior run with this PID so ttyd
	// can bind.
	_ = os.Remove(sock)

	// The xterm.js ITheme (background/foreground/cursor + 16 ANSI colors) is
	// derived from luvus's selected theme, so the terminal palette lines up
	// with luvus's chrome and the sidebar. Passed to ttyd via `-t theme=<json>`,
	// which forwards it to xterm.js in the browser. Seed from the hub's *live*
	// theme (the global `theme` is only the startup snapshot) so a terminal
	// spawned after a theme change or host-switch respawn starts on the current
	// palette rather than flashing the stale one before the browser reapplies it.
	xtheme := theme.xtermJSON()
	if srvHub != nil {
		xtheme = srvHub.themeSnapshot().xtermJSON()
	}
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
	args = append(args, strings.Fields(command)...)
	cmd := exec.Command("ttyd", args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.Env = env                                         // nil → inherit
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own process group so we can kill cleanly
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("spawned ttyd (pid %d) %q @ %s", cmd.Process.Pid, command, basePath)
	go func() {
		<-ctx.Done()
		// kill the whole process group (ttyd + the shell it spawned)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}()
	go func() { _ = cmd.Wait(); _ = os.Remove(sock) }()
	return nil
}

// ---------------------------------------------------------------------------
// luvus socket client
// ---------------------------------------------------------------------------

// luvusError is a structured error returned by luvus's socket API
// (e.g. {"code":"not_found","message":"no such pane: 42"}). Callers can
// inspect Code (via errors.As) to react to specific conditions — notably to
// treat an already-gone pane as a no-op rather than a hard failure.
type luvusError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *luvusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("luvus error %s: %s", e.Code, e.Message)
	}
	return "luvus error: " + e.Code
}

// luvusCall does one request/response round-trip against the DEFAULT host's
// luvus socket. The dial/encode/decode logic lives in luvusCallSock (backend.go),
// which both backends share.
//
// Request-path code must not use this: a handler runs against the host its
// caller named (reqBackend), and this one answers for the boot host whoever is
// asking. It survives for background work that legitimately has no caller.
func luvusCall(method string, params any) (json.RawMessage, error) {
	return defaultBackend().LuvusCall(method, params)
}

// pane is Lasso's host-local projection of UHP topology and agent state.
type pane struct {
	PaneID          string `json:"pane_id"`
	TerminalID      string `json:"terminal_id"`
	WorkspaceID     string `json:"workspace_id"`
	TabID           string `json:"tab_id"`
	WorkspaceLabel  string `json:"workspace_label"`
	TabLabel        string `json:"tab_label"`
	WorkspaceNumber int    `json:"workspace_number"`
	TabNumber       int    `json:"tab_number"`
	Label           string `json:"label"`
	Cwd             string `json:"cwd"`
	Focused         bool   `json:"focused"`
	Agent           string `json:"agent"`
	AgentStatus     string `json:"agent_status"`
	AgentSession    string `json:"agent_session"`
}

func paneCwd(p pane) string { return p.Cwd }

// Coalesce full-session projections per host. Each host has its own lock:
// independent tabs must neither exchange panes nor queue behind another host.
// UHP lifecycle events invalidate the entry before the next refresh.
type paneListCacheEntry struct {
	mu   sync.Mutex
	at   time.Time
	data json.RawMessage
	err  error
}

var paneListCache struct {
	mu     sync.Mutex
	byHost map[string]*paneListCacheEntry
}

const paneListTTL = 400 * time.Millisecond

// paneCacheFor returns host's cache slot, creating it on first use.
func paneCacheFor(host string) *paneListCacheEntry {
	paneListCache.mu.Lock()
	defer paneListCache.mu.Unlock()
	if paneListCache.byHost == nil {
		paneListCache.byHost = map[string]*paneListCacheEntry{}
	}
	e := paneListCache.byHost[host]
	if e == nil {
		e = &paneListCacheEntry{}
		paneListCache.byHost[host] = e
	}
	return e
}

func luvusPaneList(be Backend) (json.RawMessage, error) {
	e := paneCacheFor(be.Name())
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.at.IsZero() && time.Since(e.at) < paneListTTL {
		return e.data, e.err
	}
	// The call is made under the lock on purpose: concurrent callers coalesce
	// onto this one in-flight request rather than firing parallel slow calls.
	panes, err := runtimePanes(be)
	var data json.RawMessage
	if err == nil {
		data, err = json.Marshal(struct {
			Panes []pane `json:"panes"`
		}{panes})
	}
	e.at = time.Now()
	e.data, e.err = data, err
	return data, err
}

// invalidatePaneList drops host's cached pane.list so the next call refetches.
// Each host's feed calls it on every luvus event from THAT host: an event means
// that host's pane state changed, so its cached snapshot would be stale — and no
// other host's is affected.
func invalidatePaneList(host string) {
	paneListCache.mu.Lock()
	e := paneListCache.byHost[host]
	paneListCache.mu.Unlock()
	if e == nil {
		return
	}
	e.mu.Lock()
	e.at = time.Time{}
	e.mu.Unlock()
}

type workspace struct {
	WorkspaceID     string `json:"workspace_id"`
	Label           string `json:"label"`
	Number          int    `json:"number"`           // UHP's zero-based API index
	DisplayPosition int    `json:"display_position"` // sidebar order (pinning can differ)
	Focused         bool   `json:"focused"`
	Cwd             string `json:"cwd"`
}

// Active is the state pushed to the browser.
type Active struct {
	PaneID         string `json:"pane_id"`
	Cwd            string `json:"cwd"`
	CwdSource      string `json:"cwd_source"` // "harness" or Luvus's reported "shell" cwd
	WorkspaceID    string `json:"workspace_id"`
	WorkspaceLabel string `json:"workspace_label"`
	TabID          string `json:"tab_id"`
	TabLabel       string `json:"tab_label"`
	Agent          string `json:"agent"`
	AgentStatus    string `json:"agent_status"`
	PanesRev       int    `json:"panes_rev"`    // bumps when workspace order or pane membership changes
	ThemeRev       int    `json:"theme_rev"`    // bumps when Luvus's actual palette changes
	LuvusUp        bool   `json:"luvus_up"`     // false when luvus's socket is unreachable; the rest of the struct is then last-known (stale)
	Host           string `json:"host"`         // the host THIS stream is for: "local" or an ssh-config alias
	HostSlug       string `json:"host_slug"`    // Host's URL path segment, so the browser can address /terminal/<slug>/ without re-deriving it
	CwdHost        string `json:"cwd_host"`     // host Cwd lives on — can differ from Host when the focused pane is an ssh window onto another host's luvus; the sidebar browses Cwd on this host
	UIStateRev     int    `json:"ui_state_rev"` // bumps when the persisted UI prefs change, so every open tab refetches and converges
}

// fetchActive returns the focused-pane state plus a layout signature. The
// signature captures workspace order + pane membership (see layoutSignature), so
// the caller can detect when the pane list needs to re-render — e.g. after a
// workspace is reordered in luvus — independently of focus changes.
func fetchActive(be Backend) (Active, string, error) {
	res, err := luvusPaneList(be)
	if err != nil {
		return Active{}, "", err
	}
	var pl struct {
		Panes []pane `json:"panes"`
	}
	if err := json.Unmarshal(res, &pl); err != nil {
		return Active{}, "", err
	}

	wss, err := runtimeWorkspaces(be)
	if err != nil {
		return Active{}, "", err
	}
	wl := struct{ Workspaces []workspace }{wss}
	sig := layoutSignature(pl.Panes, wl.Workspaces)

	var fp *pane
	for i := range pl.Panes {
		if pl.Panes[i].Focused {
			fp = &pl.Panes[i]
			break
		}
	}
	if fp == nil {
		// luvus is reachable but has no focused pane — e.g. a freshly started
		// server with no session/workspaces yet (common right after a host
		// switch to a host whose luvus was just (re)started). That's "up but
		// empty", NOT down: returning an error here would make the hub mark
		// LuvusUp=false and never flip the active-host display. Return a valid
		// empty Active (with the layout signature) so the success path runs,
		// marks luvus up, and reflects the active host with an empty pane/cwd.
		return Active{}, sig, nil
	}
	agent, status := paneAgentPresence(*fp)
	a := Active{
		PaneID: fp.PaneID, WorkspaceID: fp.WorkspaceID,
		TabID: fp.TabID, Agent: agent, AgentStatus: status,
	}
	a.Cwd, a.CwdSource, a.CwdHost = activeCwd(be, *fp)
	a.TabLabel = fp.TabLabel
	for _, w := range wl.Workspaces {
		if w.WorkspaceID == a.WorkspaceID {
			a.WorkspaceLabel = w.Label
		}
	}
	return a, sig, nil
}

// layoutSignature is a deterministic string of workspace order (number + id +
// label) and pane-to-workspace/tab membership. It deliberately omits focus and
// cwd, so it changes for workspace and pane layout changes, but not a focus
// move.
func layoutSignature(panes []pane, wss []workspace) string {
	ws := append([]workspace(nil), wss...)
	sort.Slice(ws, func(i, j int) bool { return ws[i].Number < ws[j].Number })
	var sb strings.Builder
	for _, w := range ws {
		fmt.Fprintf(&sb, "%d:%d:%s:%s;", w.DisplayPosition, w.Number, w.WorkspaceID, w.Label)
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

// ---------------------------------------------------------------------------
// pane list: list every pane + focus one
// ---------------------------------------------------------------------------

// paneView is a luvus pane enriched with workspace/tab labels and ordering
// numbers for API consumers that need a labeled, sorted active-host pane list.
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
// grouped by workspace (then tab) order — the order luvus itself shows.
func fetchPanes(be Backend) ([]paneView, error) {
	res, err := luvusPaneList(be)
	if err != nil {
		return nil, err
	}
	var pl struct {
		Panes []pane `json:"panes"`
	}
	if err := json.Unmarshal(res, &pl); err != nil {
		return nil, err
	}

	type meta struct{ number int }
	tabs := map[string]meta{}
	wss := map[string]meta{}
	for _, p := range pl.Panes {
		wss[p.WorkspaceID] = meta{p.WorkspaceNumber}
		tabs[p.TabID] = meta{p.TabNumber}
	}

	out := make([]paneView, 0, len(pl.Panes))
	for _, p := range pl.Panes {
		agent, status := paneAgentPresence(p)
		out = append(out, paneView{
			PaneID:         p.PaneID,
			WorkspaceID:    p.WorkspaceID,
			WorkspaceLabel: p.WorkspaceLabel,
			TabID:          p.TabID,
			TabLabel:       p.TabLabel,
			Cwd:            paneCwd(p),
			Agent:          agent,
			AgentStatus:    status,
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
	be, err := reqBackend(r, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	panes, err := fetchPanes(be)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"panes": panes})
}

// serveFocus targets the exact pane, including a split in an inactive workspace.
func serveFocus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		PaneID string `json:"pane_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.PaneID == "" {
		http.Error(w, "pane_id required", http.StatusBadRequest)
		return
	}
	be, err := reqBackend(r, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	unlock := runtimeMutationLock(be)
	defer unlock()
	if _, err := be.LuvusCall("pane.focus", map[string]any{"pane": req.PaneID}); err != nil {
		http.Error(w, "pane.focus: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// serveRename renames a pane's tab without leaving the viewing focus changed.
func serveRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		PaneID string `json:"pane_id"`
		Label  string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.PaneID == "" || strings.TrimSpace(req.Label) == "" || len([]rune(req.Label)) > 40 {
		http.Error(w, "pane_id and a label of 1–40 characters required", http.StatusBadRequest)
		return
	}
	be, err := reqBackend(r, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := renameRuntimeTab(be, req.PaneID, req.Label); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// serveWorkspaceRename relabels a workspace (workspace.rename), updating the
// workspace label returned by pane-list APIs and agent records.
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
	be, err := reqBackend(r, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if _, err := be.LuvusCall("workspace.rename", map[string]any{"workspace_id": req.WorkspaceID, "name": req.Label}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Keep the agent record's title — the address list_agents and message_agent
	// surface over MCP — in step with what the pane listings now show.
	_ = updateAgentTitleByWorkspace(be.Name(), req.WorkspaceID, req.Label)
	writeJSON(w, map[string]any{"ok": true})
}

// Bulk-close resilience knobs. Closing a pane makes luvus recompute layout /
// shift focus / maybe close the tab, so a burst of pane.close calls can race
// that reconfiguration and fail transiently — hence retries plus a little
// pacing so luvus settles between calls. Tuned to stay snappy for a handful of
// panes while clearing the flakiness that used to need a manual retry.
const closeAttempts = 4 // total tries per pane

// vars (not consts) so tests can shrink the waits.
var (
	closeBackoffBase = 40 * time.Millisecond  // 1st retry wait; doubles each time
	closeBackoffMax  = 400 * time.Millisecond // cap per-retry wait
	closePace        = 25 * time.Millisecond  // breather between distinct panes
)

// paneCloser performs a single pane.close round-trip against be. A package var
// so tests can substitute a fake luvus without a live socket.
var paneCloser = func(be Backend, id string) error {
	_, err := be.LuvusCall("pane.close", map[string]any{"pane": id})
	return err
}

// closePane closes one pane on be (see closePaneWith).
func closePane(ctx context.Context, be Backend, id string) error {
	return closePaneWith(ctx, func(id string) error { return paneCloser(be, id) }, id)
}

// closePaneWith closes one pane via closer, absorbing the two flaky cases: a
// transient luvus error (retried with exponential backoff) and a pane that's
// already gone — e.g. cascade-closed when its tab's last sibling was closed —
// which is treated as success since the goal (pane gone) is met. invalid_request
// is our own bug, so it fails fast without burning retries. Honors ctx so a
// client that walks away (closed tab / navigation) doesn't keep us hammering
// luvus. closer is host-specific, so the same retry behavior applies wherever
// a caller obtained the backend.
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
		var he *luvusError
		if errors.As(err, &he) {
			switch he.Code {
			case "not_found":
				return nil // already gone — idempotent success
			case "invalid_request":
				return err // malformed on our side; retrying won't help
			}
		}
		last = err // transient (dial/timeout/luvus busy): back off and retry
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
// (see closePane) so a bulk close is resilient to luvus's reconfiguration
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
	be, err := reqBackend(r, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	ctx := r.Context()
	closed := make([]string, 0, len(req.PaneIDs))
	errs := map[string]string{}
	for i, id := range req.PaneIDs {
		if i > 0 && !sleepCtx(ctx, closePace) { // backpressure between panes
			break
		}
		if err := closePane(ctx, be, id); err != nil {
			errs[id] = err.Error()
		} else {
			closed = append(closed, id)
		}
	}
	writeJSON(w, map[string]any{"closed": closed, "errors": errs})
}

// serveThemeSet changes downstream sync preferences only. Luvus owns selection.
func serveThemeSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name string `json:"name"`
		// SyncAgentThemes toggles mirroring the theme into agent CLIs' own
		// theme files (see agentsync.go); nil leaves the setting unchanged.
		// Sent alone (no name) it only flips the setting.
		SyncAgentThemes *bool `json:"sync_agent_themes"`
		// ThemeSyncHost + ThemeSync flip ONE host's theme sync ("local" or an
		// ssh alias): false stops every theme write lasso makes to that host,
		// true resumes them. Sent alone (no name) they only update that host.
		ThemeSyncHost string `json:"theme_sync_host"`
		ThemeSync     *bool  `json:"theme_sync"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Name != "" {
		http.Error(w, "select the theme in Luvus; Lasso follows its active palette", http.StatusBadRequest)
		return
	}
	if req.SyncAgentThemes != nil {
		if err := setSetting(syncAgentThemesKey, strconv.FormatBool(*req.SyncAgentThemes)); err != nil {
			http.Error(w, "save setting: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if req.ThemeSync != nil {
		// An empty host would silently mean "local"; a caller flipping a host's
		// sync has to name it, and it has to be one lasso can address at all.
		if req.ThemeSyncHost == "" || !hostAddressable(req.ThemeSyncHost) {
			http.Error(w, fmt.Sprintf("unknown host %q", req.ThemeSyncHost), http.StatusBadRequest)
			return
		}
		if err := setThemeSyncFor(req.ThemeSyncHost, *req.ThemeSync); err != nil {
			http.Error(w, "save setting: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if *req.ThemeSync {
			// Switching sync back on converges that host now rather than at its
			// next theme or host switch. Off the request path: reaching a remote
			// costs an ssh round trip, and an unreachable one just logs.
			go convergeThemeSyncFor(req.ThemeSyncHost)
		}
	}
	writeJSON(w, map[string]any{
		"ok":                true,
		"sync_agent_themes": syncAgentThemesEnabled(),
		"theme_sync_off":    themeSyncOffHosts(),
	})
}

// ---------------------------------------------------------------------------
// luvus self-update (Settings tab)
// ---------------------------------------------------------------------------

// luvusBinary is the luvus executable to invoke for out-of-session commands
// (version, update) — the first field of -term-cmd (what ttyd runs in the
// terminal), defaulting to "luvus".
func luvusBinary() string {
	if f := strings.Fields(*termCmd); len(f) > 0 {
		return f[0]
	}
	return "luvus"
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

// termPrefix builds a command prefix that launches the luvus terminal so the
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

// outsideLuvusEnv returns the current environment minus the markers luvus uses
// to detect it's running *inside* a session (LUVUS_ENV is set to "1" in every
// pane; LUVUS_PANE_ID / LUVUS_SESSION identify the pane/session). The viewer's
// out-of-luvus shell terminal runs with this env so commands that refuse to run
// inside a session — notably `luvus update` — work there, even when the viewer
// itself was launched from a luvus pane and inherited the markers.
func outsideLuvusEnv() []string {
	drop := map[string]bool{
		"LUVUS_ENV": true, "LUVUS_PANE_ID": true, "LUVUS_SESSION": true,
		"LUVUS_SOCKET_PATH": true, "LUVUS_API_ADDRESS": true, "LUVUS_BIN_PATH": true,
		"LUVUS_WORKSPACE_ID": true, "LUVUS_TAB_ID": true, "LUVUS_HOME": true,
	}
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

// Lasso targets public UHP major 1. Capability discovery additionally verifies
// the methods it requires; the private TUI protocol is not an API contract.
const lassoLuvusProtocol = 1

// versionInfo is the /api/version payload: the luvus socket protocol this lasso
// build targets, the protocol the installed luvus daemon reports over its socket,
// that daemon's version string (display only), and whether the two protocols match.
// Err carries why the luvus protocol couldn't be read (daemon down, socket gone) so
// the tab can say so rather than falsely claim a mismatch.
type versionInfo struct {
	LassoProtocol int    `json:"lasso_protocol"`
	LassoVersion  string `json:"lasso_version"`
	LuvusProtocol int    `json:"luvus_protocol"`
	LuvusVersion  string `json:"luvus_version,omitempty"`
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
	// release-binary install (not the systemd-supervised checkout, which tracks
	// main by commit instead). When it's newer than this build the Settings tab
	// shows an "update available" hint pointing at `lasso update`.
	LatestVersion string `json:"latest_version,omitempty"`
	Err           string `json:"err,omitempty"`
}

// serveVersion reports whether the installed luvus speaks the same socket protocol
// this lasso build targets. It pings the local luvus socket fresh on every request
// — so the tab's refresh button re-checks a daemon that has since restarted —
// rather than reusing the once-cached localProtocol().
func serveVersion(w http.ResponseWriter, r *http.Request) {
	vi := versionInfo{
		LassoProtocol: lassoLuvusProtocol,
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
	if v, p, err := luvusPinger(); err != nil {
		vi.Err = err.Error()
	} else {
		vi.LuvusVersion = v
		vi.LuvusProtocol = p
		vi.Compatible = p == lassoLuvusProtocol
	}
	writeJSON(w, vi)
}

// luvusPinger reports the installed (local) luvus daemon's version and protocol.
// A seam over luvusPing(*luvusSock) so serveVersion is unit-testable without a
// live daemon. It deliberately pings the local socket, not the active backend's,
// so the Settings tab reflects the local lasso↔luvus install even when a remote
// host is selected.
var luvusPinger = func() (string, int, error) { return luvusPing(*luvusSock) }

// ---------------------------------------------------------------------------
// file drop: save a file the browser hands over — a pasted screenshot, a photo
// or a document picked on a phone — so the agent in the focused pane can read
// it by path
// ---------------------------------------------------------------------------

// clipboardExt names the extension for a body that arrived with no filename: a
// clipboard paste is bytes and a MIME type, nothing more. The common image
// types are pinned because mime.ExtensionsByType sorts its answers and would
// name a screenshot ".jpe"; anything else falls back to that lookup, and a type
// the stdlib doesn't know simply gets no extension.
var clipboardExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/jpg":  ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// pasteFileDir is the directory dropped files are written to. Kept under
// lasso's own ~/.lasso/uploads (alongside staged attachment uploads) rather
// than the OS cache dir, so they live with the rest of lasso's data and aren't
// swept by cache cleaners.
func pasteFileDir() string {
	return filepath.Join(lassoUploadsDir(), "dropped-files")
}

// pasteFileName is the basename to write. A file picked on a device carries its
// own name, which is what makes the path readable in a prompt — but it is
// client-supplied, so only its base survives (no directory may be steered from
// the query string) and a timestamp prefix keeps two picks of the same photo
// from overwriting each other.
func pasteFileName(raw, contentType string) string {
	stamp := time.Now().Format("2006-01-02-150405")
	name := filepath.Base(filepath.FromSlash(strings.TrimSpace(raw)))
	switch name {
	case "", ".", "..", string(filepath.Separator):
		ct := strings.ToLower(strings.TrimSpace(contentType))
		ext, ok := clipboardExt[ct]
		if !ok {
			if exts, _ := mime.ExtensionsByType(ct); len(exts) > 0 {
				ext = exts[0]
			}
		}
		return "clipboard-" + stamp + ext
	}
	return stamp + "-" + name
}

// servePasteFile accepts one file as the raw request body (Content-Type is its
// MIME type; ?name= its filename when the browser has one) and answers with the
// absolute path it was written to. The browser inserts that path — at the
// terminal's cursor, or into the mobile input buffer being composed — so the
// agent reads the file from the host it runs on.
func servePasteFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// Target the selected host (?host=, default active) so the file lands where
	// the agent will run and the path we hand back resolves on that host.
	be, err := reqHostBackend(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	dir := be.PasteFileDir()
	if err := be.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, "mkdir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	path := filepath.Join(dir, pasteFileName(r.URL.Query().Get("name"), ct))
	// Streamed rather than read into memory: a picker on a phone will hand over
	// a video as happily as a screenshot. Capped at the same maxUpload as the
	// multipart file endpoint — one ceiling for "bytes the browser sent us".
	out, err := be.Create(path)
	if err != nil {
		http.Error(w, "create: "+err.Error(), http.StatusInternalServerError)
		return
	}
	written, copyErr := io.Copy(out, http.MaxBytesReader(w, r.Body, maxUpload))
	closeErr := out.Close()
	switch {
	case copyErr != nil:
		_ = be.RemoveAll(path)
		http.Error(w, "write: "+copyErr.Error(), http.StatusBadRequest)
		return
	case closeErr != nil:
		_ = be.RemoveAll(path)
		http.Error(w, "write: "+closeErr.Error(), http.StatusInternalServerError)
		return
	case written == 0:
		// An empty file is never what the user meant to attach, and leaving it
		// would put a dead path in their prompt.
		_ = be.RemoveAll(path)
		http.Error(w, "empty body", http.StatusBadRequest)
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

// gitErrNoRepo reports whether a gitOut failure means "there is no repo here" —
// the directory is a plain folder, or it's gone — rather than "the backend could
// not run git at all". Only the former is a normal answer for a pane parked in a
// plain directory; an ssh transport failure must stay a real error so the UI can
// say so instead of silently reporting "no changes".
func gitErrNoRepo(err error) bool {
	s := strings.ToLower(err.Error())
	// `git -C /plain/dir rev-parse` → "fatal: not a git repository (or any of the
	// parent directories): .git"
	// `git -C /gone     rev-parse` → "fatal: cannot change to '/gone': No such file or directory"
	//
	// Deliberately not matching a bare "no such file or directory": ssh emits that
	// for a dead control socket, and swallowing it would turn a broken remote host
	// into a fake "clean, no repo".
	return strings.Contains(s, "not a git repository") ||
		strings.Contains(s, "cannot change to")
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
	// The diff runs on the host the request names (?host=, default active) so the
	// sidebar can diff the FOCUSED pane's host even while the terminal is attached
	// to another one.
	be, err := namedHostBackend(r.URL.Query().Get("host"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
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

	root, err := be.GitOut(path, "rev-parse", "--show-toplevel")
	if err != nil {
		if gitErrNoRepo(err) {
			// A pane sitting in a plain directory is not an error condition. Answer
			// the shape the client expects with isRepo:false so it renders an empty
			// state instead of retrying a 502 every 2.5s forever.
			writeJSON(w, map[string]any{
				"repo": "", "branch": "", "files": []diffFile{},
				"isRepo": false, "isBranchDiff": false, "baseBranch": "", "dirty": 0,
			})
			return
		}
		http.Error(w, "git: "+err.Error(), http.StatusBadGateway)
		return
	}
	root = strings.TrimSpace(root)
	branch := strings.TrimSpace(mustGit(be, root, "rev-parse", "--abbrev-ref", "HEAD"))

	wsArg := func(base ...string) []string {
		if ignoreWS {
			return append(base, "-w")
		}
		return base
	}

	// working-tree status is always read so the dirty count is accurate even when
	// showing the branch diff.
	status := parseStatus(mustGit(be, root, "status", "--short"))
	dirty := len(status)

	var files []diffFile
	baseBranch := ""

	// auto (default): show the working tree when it's dirty, otherwise fall back to
	// the branch-vs-base comparison. ?mode=branch / ?mode=working force one or the
	// other.
	isBranchDiff := mode == "branch" || (mode != "working" && dirty == 0)
	if isBranchDiff {
		files, baseBranch = branchFiles(be, root, branch, baseOverride, wsArg)
		if baseBranch == "" {
			isBranchDiff = false // no base to compare against → show the working tree
		}
	}
	if !isBranchDiff {
		files = workingFiles(be, root, status, wsArg)
	}

	writeJSON(w, map[string]any{
		"repo": root, "branch": branch, "files": files, "isRepo": true,
		"isBranchDiff": isBranchDiff, "baseBranch": baseBranch, "dirty": dirty,
	})
}

// serveDiffFile returns the unified diff for a SINGLE file (?file=, repo-relative)
// — fetched lazily when the user expands that file in the Diff view, so the file
// list itself is never byte-capped. ?mode= pins the comparison to what the list
// is showing (branch vs working); the per-file diff is capped at maxDiff (a
// single genuinely huge file), reported via "truncated".
func serveDiffFile(w http.ResponseWriter, r *http.Request) {
	// Same host routing as serveDiff: the per-file diff must come from the host
	// the file list was built on, or the paths wouldn't line up.
	q := r.URL.Query()
	be, err := namedHostBackend(q.Get("host"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
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

	root, err := be.GitOut(path, "rev-parse", "--show-toplevel")
	if err != nil {
		// Only reachable in a race (the worktree removed while a file row is
		// expanded) — the file list is empty for a non-repo — but it must not 502
		// there either.
		if gitErrNoRepo(err) {
			writeJSON(w, map[string]any{"diff": "", "truncated": false})
			return
		}
		http.Error(w, "git: "+err.Error(), http.StatusBadGateway)
		return
	}
	root = strings.TrimSpace(root)
	branch := strings.TrimSpace(mustGit(be, root, "rev-parse", "--abbrev-ref", "HEAD"))
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
			base = defaultBranch(be, root, branch)
		}
		if base != "" {
			if mb := strings.TrimSpace(mustGit(be, root, "merge-base", base, "HEAD")); mb != "" {
				d = mustGit(be, root, wsArg("diff", mb+"..HEAD", "--", file)...)
			}
		}
	} else {
		// working tree vs HEAD (staged + unstaged combined); empty ⇒ untracked.
		d = mustGit(be, root, wsArg("diff", "HEAD", "--", file)...)
		if d == "" {
			d = untrackedDiff(be, root, file)
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
func branchFiles(be Backend, root, current, override string, wsArg func(...string) []string) ([]diffFile, string) {
	base := override
	if base == "" {
		base = defaultBranch(be, root, current)
	}
	if base == "" {
		return nil, ""
	}
	mb := strings.TrimSpace(mustGit(be, root, "merge-base", base, "HEAD"))
	if mb == "" {
		return nil, base
	}
	return fileList(be, root, wsArg, mb+"..HEAD"), base
}

// workingFiles lists the working-tree changes (staged + unstaged vs HEAD) with
// counts, then appends untracked files (which `git diff` omits).
func workingFiles(be Backend, root string, status []diffFile, wsArg func(...string) []string) []diffFile {
	files := fileList(be, root, wsArg, "HEAD")
	for _, f := range status {
		if f.Status == "untracked" {
			files = append(files, diffFile{Path: f.Path, Status: "untracked", Add: countAddedLines(be, root, f.Path)})
		}
	}
	return files
}

// fileList builds the changed-file list for a comparison (rangeArgs, e.g. "HEAD"
// or "<merge-base>..HEAD"): paths + per-file +/- from `--numstat`, statuses from
// `--name-status`. --no-renames keeps paths plain so the two outputs align (a
// rename shows as delete+add). Whitespace-only modifications (with -w) collapse
// to 0/0 and are dropped, matching the per-file view that would show nothing.
func fileList(be Backend, root string, wsArg func(...string) []string, rangeArgs ...string) []diffFile {
	num := wsArg(append([]string{"diff", "--numstat", "--no-renames"}, rangeArgs...)...)
	name := append([]string{"diff", "--name-status", "--no-renames"}, rangeArgs...)
	counts, order := parseNumstat(mustGit(be, root, num...))
	statuses := parseNameStatusMap(mustGit(be, root, name...))
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
func countAddedLines(be Backend, root, rel string) int {
	full := filepath.Join(root, rel)
	info, err := be.Stat(full)
	if err != nil || info.IsDir() || info.Size() > maxUntracked {
		return 0
	}
	data, err := be.ReadFile(full)
	if err != nil || isBinary(data) {
		return 0
	}
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// mustGit runs a git command on be, returning "" on error (the diff endpoint
// treats a missing sub-result as empty rather than failing the whole request).
func mustGit(be Backend, dir string, args ...string) string {
	out, _ := be.GitOut(dir, args...)
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
func defaultBranch(be Backend, root, current string) string {
	if ref, err := be.GitOut(root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
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
		if _, err := be.GitOut(root, "rev-parse", "--verify", "--quiet", b); err == nil {
			return b
		}
	}
	return ""
}

// untrackedDiff synthesizes an "all added" unified diff for an untracked file
// (git diff omits untracked files), so the Diff view can preview new files too.
func untrackedDiff(be Backend, root, rel string) string {
	full := filepath.Join(root, rel)
	info, err := be.Stat(full)
	if err != nil || info.IsDir() || info.Size() > maxUntracked {
		return ""
	}
	data, err := be.ReadFile(full)
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

// subscribeEvents opens a long-lived connection subscribed to ONE host's luvus
// events and signals `trigger` whenever one arrives (that host's feed then
// re-fetches state). Reconnects on failure. Beyond the *.focused events that
// drive the active-pane view, it listens to the workspace/tab/pane lifecycle
// events — notably workspace.updated, which fires when workspaces are reordered
// — so the pane list's order and membership stay live.
//
// The backend is read through a function, not captured: the host pool can redial
// a dead master under us, and each reconnect must pick up the socket the pool
// currently holds rather than the one this goroutine started on.
func subscribeEvents(ctx context.Context, be func() Backend, trigger chan<- struct{}) {
	for ctx.Err() == nil {
		sock := be().LuvusSock()
		if sock == "" {
			// A backend with no socket to subscribe to (a files-only connection,
			// a fake in a test). The feed's poll still runs; there is just no
			// event stream to shorten its latency.
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			if !sleepCtx(ctx, time.Second) {
				return
			}
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
		sub := "{\"id\":\"ui-sub\",\"method\":\"events.subscribe\",\"params\":{}}\n"
		if _, err := conn.Write([]byte(sub)); err != nil {
			close(stop)
			conn.Close()
			continue
		}
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			var frame struct {
				Event string `json:"event"`
			}
			if json.Unmarshal(sc.Bytes(), &frame) != nil || frame.Event == "terminal.output_ready" {
				continue // output streaming must not force full topology reads
			}
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

// notice is a one-shot, user-facing message pushed to every open tab over the
// SSE stream, where the browser raises it as a toast. It exists for work that
// outlives the request that started it — auto-titling runs long after
// /api/create-agent has answered 200, so a failure there has no response left
// to ride home on and would otherwise only ever reach lasso's log.
type notice struct {
	Level  string `json:"level"` // "error" | "info" | "success"
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// notifyUI raises a notice on every connected tab. A no-op before the server is
// up (CLI subcommands, tests), so background code can call it unconditionally.
func notifyUI(n notice) {
	if srvHub != nil {
		srvHub.notify(n)
	}
}

// hub owns what is GLOBAL to the server — the resolved theme, the UI-prefs
// revision, and the one-shot notice fan-out — plus the registry of per-host
// feeds (hostfeed.go) that own everything host-scoped.
//
// The split is the point: theme and UI prefs are properties of this lasso, so
// every tab sees the same ones whatever host it is on, while panes, focus, cwd
// and luvus liveness belong to a host and reach only the tabs watching it.
type hub struct {
	rootCtx context.Context

	mu         sync.RWMutex
	themeRev   int // theme revision (bumped when the resolved theme changes)
	uiStateRev int // UI-prefs revision (bumped on every /api/ui-state save)
	curTheme   resolvedTheme
	feeds      map[string]*hostFeed
	// noticeClients is every connected tab, subscribed to one-shot notices. Kept
	// as its own channel per client rather than folded into Active because a
	// notice is an EVENT, not state: Active is snapshot-replaced on every poll
	// and re-sent on connect, which would replay (or silently drop) a toast
	// instead of delivering it exactly once. Global, not per feed: a notice is
	// about lasso, not about a host.
	noticeClients map[chan notice]struct{}
}

// newHub seeds the hub's theme with the one resolved at startup, so the first
// poll only bumps themeRev if config.toml has actually changed since boot.
func newHub() *hub {
	return &hub{
		// Replaced by run(). Seeded so a hub built outside main — a test, a CLI
		// path — can start feeds without a nil parent context.
		rootCtx:       context.Background(),
		curTheme:      theme,
		feeds:         map[string]*hostFeed{},
		noticeClients: map[chan notice]struct{}{},
	}
}

// revs reads the two global revision counters feeds stamp into every frame.
func (h *hub) revs() (themeRev, uiStateRev int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.themeRev, h.uiStateRev
}

// notify fans a notice out to every connected tab, whatever host it is on.
// Non-blocking per client (a stalled reader drops the toast rather than wedging
// the caller), matching how state frames are pushed.
func (h *hub) notify(n notice) {
	h.mu.RLock()
	clients := make([]chan notice, 0, len(h.noticeClients))
	for c := range h.noticeClients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()
	for _, c := range clients {
		select {
		case c <- n:
		default:
		}
	}
}

// kick forces a near-immediate refresh of host's feed, used after a mutation so
// its result is pushed without waiting for the poll tick. An empty host kicks
// every running feed — the right shape for a change that could have touched any
// of them (a theme write, whose repaint every tab wants now).
func (h *hub) kick(host string) {
	if host == "" {
		h.eachFeed((*hostFeed).kick)
		return
	}
	h.mu.RLock()
	f := h.feeds[host]
	h.mu.RUnlock()
	if f != nil {
		f.kick()
	}
}

// bumpUIStateRev broadcasts a UI-prefs revision bump to every SSE client
// immediately (no luvus refetch — the prefs live in lasso's own db). Tabs
// refetch /api/ui-state when the rev moves, so starring a pane or collapsing
// the sidebar in one tab converges every other open tab within a beat,
// including tabs sitting on a different host.
func (h *hub) bumpUIStateRev() {
	h.mu.Lock()
	h.uiStateRev++
	h.mu.Unlock()
	h.eachFeed((*hostFeed).pushCurrent)
}

// snapshot is host's current state, starting that host's feed if nothing is
// watching it yet. The first frame it returns may be the seeded empty one, which
// the SSE stream replaces a beat later.
func (h *hub) snapshot(host string) (Active, error) {
	f, err := h.feed(host)
	if err != nil {
		return Active{}, err
	}
	return f.snapshot(), nil
}

func (h *hub) themeSnapshot() resolvedTheme { h.mu.RLock(); defer h.mu.RUnlock(); return h.curTheme }

// run reads the default Luvus theme once per poll, independently of host feeds.
// Palette changes fan out to browsers and opted-in agent theme files only.
func (h *hub) run(ctx context.Context) {
	h.rootCtx = ctx
	// Keep the default host's feed warm from boot: it is what a fresh tab lands
	// on, and paying a cold pane.list on first paint is the one case where the
	// on-demand feed would be felt.
	if _, err := h.feed(""); err != nil {
		log.Printf("feed:     default host: %v", err)
	}
	ticker := time.NewTicker(*pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.refreshTheme()
		}
	}
}

// refreshTheme publishes changes to the actual palette, not unrelated UHP state.
func (h *hub) refreshTheme() {
	rt, err := loadLuvusTheme() // outside the lock: it does I/O
	if err != nil {
		return // an unavailable source must never overwrite the last good theme
	}
	h.mu.Lock()
	if rt == h.curTheme {
		h.mu.Unlock()
		return
	}
	h.curTheme = rt
	h.themeRev++
	h.mu.Unlock()
	if rt.Customized {
		log.Printf("theme:    reloaded %q -> %s (+custom overrides)", rt.Name, rt.Resolved)
	} else {
		log.Printf("theme:    reloaded %q -> %s", rt.Name, rt.Resolved)
	}
	// Downstream writes are off the poll's critical path.
	go syncThemeEverywhere(rt)
	h.eachFeed((*hostFeed).pushCurrent)
}

// serveSSE streams one tab's state. The tab names its host (?host=, since an
// EventSource cannot set a header), and the stream carries THAT host's frames
// plus the global notices — so two tabs on two machines each get their own
// machine's panes over their own subscription.
func (h *hub) serveSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", http.StatusInternalServerError)
		return
	}
	f, ch, unwatch, err := h.watch(requestHost(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer unwatch()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	nch := make(chan notice, 8)
	h.mu.Lock()
	h.noticeClients[nch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.noticeClients, nch)
		h.mu.Unlock()
	}()

	send := func(a Active) {
		b, _ := json.Marshal(a)
		fmt.Fprintf(w, "event: active\ndata: %s\n\n", b)
		fl.Flush()
	}
	sendNotice := func(n notice) {
		b, _ := json.Marshal(n)
		fmt.Fprintf(w, "event: notice\ndata: %s\n\n", b)
		fl.Flush()
	}
	send(f.snapshot()) // prime with current state

	keep := time.NewTicker(25 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-draining:
			// Exit at shutdown so srv.Shutdown isn't held open by idle streams;
			// the browser's EventSource auto-reconnects to the restarted server.
			return
		case a := <-ch:
			send(a)
		case n := <-nch:
			sendNotice(n)
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

// expandTildeOn expands a leading ~ or ~/… against a specific backend's home
// directory so the path input accepts the shorthand on whichever host the
// request targets — active or named (e.g. listing a remote host's repos for its
// Settings). Anything else (including ~user, which we don't resolve) is
// returned unchanged.
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
	// Resolve the host FIRST so the tilde below expands against that host's
	// home (a remote ~ must not become the local home) and a bogus alias is
	// refused before anything touches a filesystem.
	be, err := namedHostBackend(r.URL.Query().Get("host"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	path := filepath.Clean(expandTildeOn(be, r.URL.Query().Get("path")))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	out, err := be.ReadDir(path)
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
		Host string `json:"host"` // host to delete on (default active)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	be, err := namedHostBackend(req.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	path := filepath.Clean(expandTildeOn(be, req.Path))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	if err := be.RemoveAll(path); err != nil {
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
		Host string `json:"host"` // host to rename on (default active)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	be, err := namedHostBackend(req.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	path := filepath.Clean(expandTildeOn(be, req.Path))
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
	if _, err := be.Lstat(dst); err == nil {
		http.Error(w, "a file with that name already exists", http.StatusConflict)
		return
	}
	if err := be.Rename(path, dst); err != nil {
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
		Host    string `json:"host"` // host to write on (default active)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	be, err := namedHostBackend(req.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	path := filepath.Clean(expandTildeOn(be, req.Path))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	info, err := be.Stat(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "not a file", http.StatusBadRequest)
		return
	}
	if err := be.WriteFile(path, []byte(req.Content), info.Mode().Perm()); err != nil {
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
	// The form's `host` field (default active) picks the filesystem `dir` names —
	// resolved before the tilde expands, for the same reason as serveFiles.
	be, err := namedHostBackend(r.FormValue("host"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	dir := filepath.Clean(expandTildeOn(be, r.FormValue("dir")))
	if !filepath.IsAbs(dir) {
		http.Error(w, "dir must be absolute", http.StatusBadRequest)
		return
	}
	if info, err := be.Stat(dir); err != nil {
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
		if err := saveUpload(be, fh, filepath.Join(dir, name)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		written = append(written, name)
	}
	writeJSON(w, map[string]any{"ok": true, "files": written})
}

// saveUpload streams one multipart file to dst on be, truncating any existing
// file. The backend arrives as a parameter so an upload lands on the host the
// request selected (over SFTP when that's a remote), not whichever host happens
// to be active.
func saveUpload(be Backend, fh *multipart.FileHeader, dst string) error {
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := be.Create(dst)
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
	// Read from the host the request targets (?host=, default active) so a preview
	// of a file that lives on another host — e.g. a screenshot just pasted onto the
	// host an agent will run on — resolves there instead of 404ing against the
	// active backend. Resolved BEFORE the tilde expands so ~ names that host's
	// home, not the active one's.
	be, err := reqHostBackend(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	path := filepath.Clean(expandTildeOn(be, r.URL.Query().Get("path")))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	info, err := be.Stat(path)
	if err != nil || info.IsDir() {
		http.Error(w, "not a file", http.StatusNotFound)
		return
	}
	f, err := be.Open(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	// Force revalidation on every fetch. http.ServeContent only sets
	// Last-Modified; with no Cache-Control the browser (and any proxy in front,
	// e.g. the Cloudflare tunnel exposing lasso.knowsuchagency.ai) applies
	// heuristic freshness and keeps serving a stale copy after the file is
	// rewritten on disk — the reported "old content when I reopen the file". With
	// no-cache the cached copy is still stored but must be revalidated first, so
	// ServeContent's If-Modified-Since handling answers 304 when unchanged (cheap,
	// no body — matters for remote SFTP reads) and 200 with fresh bytes once it
	// changes. The viewer's poll and binary-preview signature checks then see the
	// real on-disk state.
	w.Header().Set("Cache-Control", "no-cache")
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
// directly (images, PDFs, videos) rather than fetching as text — these bypass
// the text preview size cap. Videos rely on http.ServeContent's Range support
// for in-browser seeking/streaming.
func isPreviewMedia(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp", ".ico", ".avif",
		".mp4", ".webm", ".ogv", ".mov", ".m4v", ".mkv":
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

// noStore marks a response uncacheable by any intermediary. Used for /sw.js —
// see the route for why that one file needs it.
func noStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
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
	// The pre-paint script restores chrome appearance. The client then reads
	// Luvus's active palette for terminals and the optional matching chrome.
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
			w.Header().Set("WWW-Authenticate", `Basic realm="luvus", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withAuthExcept is withAuth that lets requests under any of the exempt prefixes
// bypass auth. Used to keep /mcp open (its consumers — agent sessions — don't
// carry UI credentials) and to expose the OAuth discovery/token endpoints, which
// are the credential-less half of a handshake, while the rest of the app stays
// gated. A path equal to a prefix or under prefix+"/" is matched, so "/mcp" and
// "/mcp/…" pass but a sibling like "/mcp-foo" does not.
func withAuthExcept(next http.Handler, user, pass string, enabled bool, exempt ...string) http.Handler {
	if !enabled {
		return next
	}
	gated := withAuth(next, user, pass, enabled)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, e := range exempt {
			if r.URL.Path == e || strings.HasPrefix(r.URL.Path, e+"/") {
				next.ServeHTTP(w, r)
				return
			}
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
