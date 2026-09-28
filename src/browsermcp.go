package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// /browser-mcp — the shared browser as an MCP server (see browser.go for the
// browser itself). An agent adds ONE HTTP MCP URL and gets Google's
// chrome-devtools-mcp tool surface against the same Chromium the human watches
// in the Browser tab, with nothing installed on the agent's machine and lasso's
// own auth in front of it.
//
// chrome-devtools-mcp only speaks stdio — it has no HTTP server mode — so lasso
// is a BRIDGE: every MCP session on /browser-mcp gets its own chrome-devtools-mcp
// child, spawned when the session initializes, whose tools are mirrored onto a
// per-session server and whose tools/call answers are handed back untouched
// (a screenshot's image content included). One child per session rather than
// one shared child because chrome-devtools-mcp keeps per-client state — the
// "selected page" its page-scoped tools act on, console/network buffers,
// emulation settings — so two agents behind one child would steer each other's
// page. A child is ~one node process; the cap (-browser-mcp-max) bounds them.
//
// The child connects to lasso's OWN /cdp, never to Chromium's loopback port:
// /cdp is the address that survives relaunches, it launches the browser lazily,
// and its in-flight counter is what keeps the idle stop from firing under an
// agent that is still connected. It authenticates with an internal token (see
// internalCDPToken in cdpproxy.go) because an agent's credential never reaches
// lasso's own child.
//
// Lifetime: a session ends when the client DELETEs it, when it sits idle for
// browserMCPSessionTimeout, when its child dies, or when the shared browser
// stops or relaunches (proxy change, restart, idle stop, crash) — the child's
// CDP connection is dead then, and closing the session makes a spec-compliant
// client re-initialize (the old session id answers 404) onto a fresh child and
// the new browser. Every end kills the child.

// browserMCPSessionTimeout reaps the child of an agent that went away without
// closing its session (a killed process, a laptop lid). The SDK pauses it while
// a POST is being answered, so a long tool call never trips it.
const browserMCPSessionTimeout = 30 * time.Minute

// browserMCPStartTimeout bounds spawning a child through its tools/list. node's
// cold start is a second or two; anything past this is a child that is stuck.
const browserMCPStartTimeout = 45 * time.Second

// browserMCPTerminate is how long the SDK's stdio close waits after closing the
// child's stdin (and again after SIGTERM) before escalating.
const browserMCPTerminate = 2 * time.Second

// browserMCPInstallHint is what every "not installed" answer tells the operator.
// There is deliberately no `npx chrome-devtools-mcp@latest` fallback: fetching an
// unpinned package from the registry at runtime, on the machine holding the
// browser's logged-in profile, is a supply-chain hole with lasso's name on it.
const browserMCPInstallHint = "install it on lasso's machine (`npm i -g chrome-devtools-mcp` or `mise use -g npm:chrome-devtools-mcp`), or set LASSO_BROWSER_MCP to its path"

// browserMCPInstructions is surfaced to the model once per session through
// initialize. It is the etiquette of a browser someone else is looking at.
const browserMCPInstructions = `This is lasso's SHARED browser: a real Chromium on lasso's machine that a human is watching live in lasso's Browser tab, and that other agents may be using too.

- The human's tab shows ONE page, the most recently opened. Open your own page (new_page) rather than navigating a page you did not open, unless the human asked you to work in theirs.
- Close the pages you opened (close_page) when you are done.
- localhost inside this browser means lasso's machine, not yours.
- Accounts logged into this browser are the human's, not yours: reading is fine, but posting, sending, accepting or buying anything needs the human's go-ahead first.`

type browserMCPConfig struct {
	Binary    string // -browser-mcp / LASSO_BROWSER_MCP: a path or PATH name; "off" disables; "" = chrome-devtools-mcp
	ExtraArgs string // LASSO_BROWSER_MCP_ARGS, whitespace-split after lasso's own
	Max       int    // concurrent children; <= 0 means the default
	// ExtraEnv is appended to the child's minimal environment. A test seam (the
	// fake child is this test binary, told what to be by an env var); nothing in
	// production sets it.
	ExtraEnv []string
}

const browserMCPDefaultMax = 8

// browserMCPBridge serves /browser-mcp.
type browserMCPBridge struct {
	cfg      browserMCPConfig
	lookPath func(string) (string, error)
	handler  *mcp.StreamableHTTPHandler

	// cdpEndpoint is ws://<the address lasso bound>/cdp, set once the listener
	// is up (the route table is built before the bind).
	cdpEndpoint atomic.Value // string

	mu       sync.Mutex
	live     map[*browserMCPSession]struct{}
	starting int // slots reserved by initializes still spawning their child
}

// browserMCP is the process-wide bridge main wires up; nil reads as a feature
// that is not configured.
var browserMCP *browserMCPBridge

func newBrowserMCPBridge(cfg browserMCPConfig) *browserMCPBridge {
	if cfg.Max <= 0 {
		cfg.Max = browserMCPDefaultMax
	}
	b := &browserMCPBridge{cfg: cfg, lookPath: exec.LookPath, live: map[*browserMCPSession]struct{}{}}
	b.cdpEndpoint.Store("")
	// getServer is called for every request that carries no session id — in
	// practice the initialize that opens a session (anything else without an id
	// fails initialization and the SDK closes it). Each gets a server of its
	// own, because a session's tool list IS its child's, and nothing is spawned
	// until the initialize actually runs (browserMCPSession.start).
	b.handler = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		profile, _ := browserMCPProfile(r.URL.Path)
		return b.newSessionServer(profile)
	}, &mcp.StreamableHTTPOptions{
		// Same reason as newMCPHandler: lasso is loopback-bound and reached
		// through a tunnel under a public Host, which the SDK's DNS-rebinding
		// guard would 403. The gate is withBrowserMCPAuth plus Access.
		DisableLocalhostProtection: true,
		SessionTimeout:             browserMCPSessionTimeout,
	})
	return b
}

// setListenAddr records the address the main listener actually bound, which is
// where a child dials /cdp. An unspecified bind (0.0.0.0 / ::, which lasso
// refuses without auth and should never use) is dialed on loopback instead.
func (b *browserMCPBridge) setListenAddr(addr net.Addr) {
	if b == nil || addr == nil {
		return
	}
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	b.cdpEndpoint.Store("ws://" + net.JoinHostPort(host, port) + "/cdp")
}

// resolve finds the chrome-devtools-mcp to run. ok=false carries a reason the
// Settings pane, /api/browser and a refused session all show as-is.
func (b *browserMCPBridge) resolve() (bin, reason string, ok bool) {
	if b == nil {
		return "", "the browser MCP endpoint is not configured on this lasso", false
	}
	e := strings.TrimSpace(b.cfg.Binary)
	if strings.EqualFold(e, "off") {
		return "", "the browser MCP endpoint is disabled (LASSO_BROWSER_MCP=off / -browser-mcp off)", false
	}
	if e == "" {
		e = "chrome-devtools-mcp"
	}
	if strings.ContainsRune(e, '/') {
		if st, err := os.Stat(e); err != nil || st.IsDir() {
			return "", fmt.Sprintf("the configured chrome-devtools-mcp %q does not exist: %s", e, browserMCPInstallHint), false
		}
		return e, "", true
	}
	p, err := b.lookPath(e)
	if err != nil {
		if e == "chrome-devtools-mcp" {
			return "", "chrome-devtools-mcp is not installed on lasso's machine: " + browserMCPInstallHint, false
		}
		return "", fmt.Sprintf("the configured chrome-devtools-mcp %q is not in PATH: %s", e, browserMCPInstallHint), false
	}
	return p, "", true
}

// sessions is the number of live children.
func (b *browserMCPBridge) sessions() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.live)
}

// browserMCPProfile is the browser profile a /browser-mcp path names:
// /browser-mcp is the default profile's, /browser-mcp/<id> another's.
// ok=false is a path with more than one segment after the prefix.
func browserMCPProfile(p string) (string, bool) {
	rest := strings.Trim(strings.TrimPrefix(p, "/browser-mcp"), "/")
	if rest == "" {
		return defaultBrowserProfile, true
	}
	if strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// browserMCPPathFor is the /browser-mcp address of a profile.
func browserMCPPathFor(profile string) string {
	if profile == "" || profile == defaultBrowserProfile {
		return "/browser-mcp"
	}
	return "/browser-mcp/" + profile
}

// ServeHTTP refuses a NEW session up front when the endpoint cannot work (off,
// or no chrome-devtools-mcp), with the reason in the body, instead of letting
// the client find out from a failed initialize. A request that names a session
// always reaches the SDK, so an existing one can still be closed.
func (b *browserMCPBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if b == nil {
		http.Error(w, "the browser MCP endpoint is not configured on this lasso", http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Mcp-Session-Id") == "" {
		if _, reason, ok := b.resolve(); !ok {
			http.Error(w, reason, http.StatusServiceUnavailable)
			return
		}
		profile, ok := browserMCPProfile(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if profile != defaultBrowserProfile {
			if _, err := browserFor(profile); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
		}
	}
	b.handler.ServeHTTP(w, r)
}

// browserMCPSession is one MCP session and the child behind it.
type browserMCPSession struct {
	b   *browserMCPBridge
	srv *mcp.Server
	// profile is the browser profile this session's child drives; a stop of
	// that profile's browser ends the session, a stop of another's does not.
	profile string

	mu      sync.Mutex
	ss      *mcp.ServerSession
	child   *mcp.ClientSession
	cmd     *exec.Cmd
	pid     int
	started bool
	closed  bool
}

func (b *browserMCPBridge) newSessionServer(profile string) *mcp.Server {
	if profile == "" {
		profile = defaultBrowserProfile
	}
	s := &browserMCPSession{b: b, profile: profile}
	instructions := browserMCPInstructions
	if profile != defaultBrowserProfile {
		instructions += "\n- This session drives the browser PROFILE \"" + profile + "\": its own Chromium, with its own cookies, logins and proxy. Other profiles' pages are not visible here."
	}
	s.srv = mcp.NewServer(&mcp.Implementation{
		Name:    "lasso-browser",
		Title:   "Lasso shared browser (chrome-devtools-mcp)",
		Version: lassoSemver,
	}, &mcp.ServerOptions{
		Instructions: instructions,
		// The tools are registered while initialize is being answered. Declaring
		// the capability up front, with listChanged off, keeps AddTool from
		// queueing a tools/list_changed notification at a session that has not
		// even received its initialize result yet.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: false}},
	})
	s.srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "initialize" {
				ss, _ := req.GetSession().(*mcp.ServerSession)
				if err := s.start(ctx, ss); err != nil {
					// The initialize answers with this error, the SDK closes the
					// half-open session, and nothing is left running.
					log.Printf("browser-mcp: session refused: %v", err)
					return nil, err
				}
			}
			return next(ctx, method, req)
		}
	})
	return s.srv
}

// start spawns this session's child, mirrors its tools, and registers it. It
// runs inside the initialize request, so every failure is the initialize's.
func (s *browserMCPSession) start(ctx context.Context, ss *mcp.ServerSession) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil // a repeated initialize on a live session: nothing new to spawn
	}
	s.started = true
	s.mu.Unlock()
	if ss == nil {
		return errors.New("browser MCP: no server session to bind the child to")
	}
	b := s.b
	if err := b.reserve(); err != nil {
		return err
	}
	registered := false
	defer func() {
		if !registered {
			b.unreserve()
		}
	}()

	bin, reason, ok := b.resolve()
	if !ok {
		return errors.New(reason)
	}
	endpoint, _ := b.cdpEndpoint.Load().(string)
	if endpoint == "" {
		return errors.New("lasso is not listening yet; retry in a moment")
	}
	if s.profile != defaultBrowserProfile {
		if _, err := browserFor(s.profile); err != nil {
			return err
		}
		// cdpEndpoint is ws://<addr>/cdp; the profile's browser is one level down.
		endpoint += strings.TrimPrefix(cdpPathFor(s.profile), "/cdp")
	}

	cmd := exec.Command(bin, browserMCPArgs(endpoint, internalCDPToken, b.cfg.ExtraArgs)...)
	cmd.Env = append(browserMCPEnv(os.Environ()), b.cfg.ExtraEnv...)
	cmd.Dir = os.TempDir()
	tail := &tailBuffer{max: 8 << 10}
	logw := &browserMCPStderr{tail: tail}
	cmd.Stderr = logw
	// Its own process group, so the stop takes anything it spawned with it, and
	// (linux) Pdeathsig so a kill -9 of lasso does not leave node running.
	cmd.SysProcAttr = browserSysProcAttr()

	cctx, cancel := context.WithTimeout(ctx, browserMCPStartTimeout)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "lasso", Version: lassoSemver}, nil)
	child, err := client.Connect(cctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: browserMCPTerminate}, nil)
	if err != nil {
		reapBrowserMCPChild(cmd)
		return browserMCPStartFailure(bin, cmd, err, tail)
	}
	pid := cmd.Process.Pid
	logw.setPID(pid)

	var tools []*mcp.Tool
	for t, err := range child.Tools(cctx, nil) { // follows nextCursor pagination
		if err != nil {
			_ = child.Close()
			reapBrowserMCPChild(cmd)
			return fmt.Errorf("chrome-devtools-mcp (%s) started but tools/list failed: %v%s", bin, err, browserMCPTail(tail))
		}
		tools = append(tools, t)
	}
	for _, t := range tools {
		if !mcpObjectSchema(t.InputSchema) {
			// Server.AddTool panics on a non-object input schema. A child that
			// ships one has a broken tool, not a broken session: skip it.
			log.Printf("browser-mcp: skipping tool %q: its input schema is not an object", t.Name)
			continue
		}
		if t.OutputSchema != nil && !mcpObjectSchema(t.OutputSchema) {
			t.OutputSchema = nil
		}
		s.srv.AddTool(t, s.forward(t.Name))
	}

	s.mu.Lock()
	s.ss, s.child, s.cmd, s.pid = ss, child, cmd, pid
	s.mu.Unlock()
	b.mu.Lock()
	b.starting--
	b.live[s] = struct{}{}
	n := len(b.live)
	b.mu.Unlock()
	registered = true
	log.Printf("browser-mcp: session started for profile %q, child pid %d (%d tools; %d of %d live)", s.profile, pid, len(tools), n, b.cfg.Max)

	// Either end going away ends both. The session ending (DELETE, idle
	// timeout, a relaunch closing it) kills the child; the child dying (a
	// crash, an OOM kill) closes the session so the client re-initializes onto a
	// fresh one instead of calling tools on a corpse.
	go func() { _ = ss.Wait(); s.close("session ended") }()
	go func() {
		_ = child.Wait()
		if s.close("child exited") {
			_ = ss.Close()
		}
	}()
	return nil
}

// forward is a mirrored tool's handler: the call goes to the child as-is (name
// and raw arguments) and its result comes back as-is — Content (images too),
// StructuredContent, IsError and _meta — so the bridge adds nothing a client
// could tell apart from talking to chrome-devtools-mcp directly.
func (s *browserMCPSession) forward(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s.mu.Lock()
		child := s.child
		s.mu.Unlock()
		if child == nil {
			return nil, errors.New("this browser session has ended; reconnect to start a new one")
		}
		p := &mcp.CallToolParams{Name: name, Meta: req.Params.Meta}
		if len(req.Params.Arguments) > 0 {
			p.Arguments = req.Params.Arguments
		}
		return child.CallTool(ctx, p)
	}
}

// close ends the child and unregisters the session, once. It reports whether
// this call did it (the watchers race each other to it).
func (s *browserMCPSession) close(why string) bool {
	s.mu.Lock()
	if s.closed || s.child == nil {
		s.mu.Unlock()
		return false
	}
	s.closed = true
	child, cmd, pid := s.child, s.cmd, s.pid
	s.mu.Unlock()

	s.b.mu.Lock()
	delete(s.b.live, s)
	s.b.mu.Unlock()

	// Closing the client session closes the child's stdin and waits for it to
	// exit, escalating to SIGTERM and then SIGKILL (browserMCPTerminate each).
	_ = child.Close()
	reapBrowserMCPChild(cmd)
	log.Printf("browser-mcp: session closed (%s), child pid %d stopped", why, pid)
	return true
}

// closeAll ends every live session and waits for their children to be gone.
// Called when the shared browser stops or relaunches (their CDP connections are
// dead) and at shutdown.
func (b *browserMCPBridge) closeAll(why string) {
	b.closeSessions(b.snapshot(), why)
}

func (b *browserMCPBridge) snapshot() []*browserMCPSession {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*browserMCPSession, 0, len(b.live))
	for s := range b.live {
		out = append(out, s)
	}
	return out
}

func (b *browserMCPBridge) closeSessions(sessions []*browserMCPSession, why string) {
	if len(sessions) == 0 {
		return
	}
	log.Printf("browser-mcp: closing %d session(s): %s", len(sessions), why)
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Child first: ServerSession.Close waits for in-flight calls, and a
			// call in flight is waiting on this child, which only answers (with
			// an error) once it is gone.
			s.close(why)
			s.mu.Lock()
			server := s.ss
			s.mu.Unlock()
			if server != nil {
				_ = server.Close() // the old session id now answers 404: the client re-initializes
			}
		}()
	}
	wg.Wait()
}

// browserStopped is the default profile's stop hook (browserManager.onStop).
func (b *browserMCPBridge) browserStopped(why string) {
	b.browserStoppedFor(defaultBrowserProfile, why)
}

// browserStoppedFor closes the sessions driving one profile's browser, which
// just went away. The set is taken NOW, synchronously — a session that
// connects after this moment is talking to the next browser and must survive —
// and closed in the background, since the hook runs with the browser's launch
// lock held. Other profiles' sessions are other processes and are untouched.
func (b *browserMCPBridge) browserStoppedFor(profile, why string) {
	var mine []*browserMCPSession
	for _, s := range b.snapshot() {
		if s.profile == profile {
			mine = append(mine, s)
		}
	}
	if len(mine) > 0 {
		go b.closeSessions(mine, "the shared browser stopped ("+why+")")
	}
}

func (b *browserMCPBridge) reserve() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.live)+b.starting >= b.cfg.Max {
		return fmt.Errorf("lasso's browser MCP is at its limit of %d concurrent sessions (LASSO_BROWSER_MCP_MAX / -browser-mcp-max): close another agent's browser session, or raise the limit", b.cfg.Max)
	}
	b.starting++
	return nil
}

func (b *browserMCPBridge) unreserve() {
	b.mu.Lock()
	b.starting--
	b.mu.Unlock()
}

// browserMCPArgs is the child's command line. lasso's own flags come first so
// LASSO_BROWSER_MCP_ARGS can add to them (a later duplicate wins in yargs).
//
//   - --wsEndpoint is lasso's own /cdp (see the file comment), --wsHeaders the
//     internal token that lets it through. The token rides argv, which any
//     process on lasso's machine can read from /proc/<pid>/cmdline:
//     chrome-devtools-mcp takes websocket headers from nowhere else (no env
//     var, no file). Measured, the window is short — it sets process.title,
//     after which its cmdline reads just "chrome-devtools-mcp" — but that is
//     its behavior, not a guarantee, so treat the token as readable by other
//     users of lasso's machine, who could equally reach a loopback lasso's
//     open /cdp. It is never logged and dies with lasso.
//   - usage statistics and CrUX off: the URLs an agent visits are the human's
//     business, not a telemetry endpoint's.
//   - screenshots as JPEG q80 bounded to 1280px: the browser renders at
//     -browser-scale (2 by default), so an unbounded PNG screenshot is ~4x the
//     pixels — and image tokens scale with pixels — for no gain to a model.
func browserMCPArgs(endpoint, token, extra string) []string {
	hdr, _ := json.Marshal(map[string]string{internalCDPHeader: token})
	args := []string{
		"--wsEndpoint", endpoint,
		"--wsHeaders", string(hdr),
		"--no-usage-statistics",
		"--no-performance-crux",
		"--screenshotFormat", "jpeg",
		"--screenshotQuality", "80",
		"--screenshotMaxWidth", "1280",
		"--screenshotMaxHeight", "1280",
	}
	return append(args, strings.Fields(extra)...)
}

// browserMCPEnv is the child's environment: an allowlist, not lasso's minus a
// denylist. The child needs to find node (PATH, and mise's shims need HOME and
// its own dirs) and nothing else; lasso's environment carries UI_AUTH,
// MCP_OAUTH, LASSO_MCP_TOKEN and whatever the operator's shell exported, none
// of which a process driving arbitrary web pages should hold. MISE_* is limited
// to the directory settings, since MISE_GITHUB_TOKEN is a MISE_ variable too.
func browserMCPEnv(env []string) []string {
	// Belt and braces: its own opt-out, in case a future version reads only the
	// environment. CI=1 would do it too, but also changes other behavior.
	return append(minimalChildEnv(env), "CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1")
}

// minimalChildEnv is the allowlist every child lasso runs on the host starts
// from — chrome-devtools-mcp above, and a trusted plugin's MCP server
// (pluginsandbox.go), which gets its manifest env and secrets on top.
func minimalChildEnv(env []string) []string {
	keep := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
		"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TZ": true, "TMPDIR": true,
		"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_CACHE_HOME": true, "XDG_STATE_HOME": true,
		"MISE_DATA_DIR": true, "MISE_CONFIG_DIR": true, "MISE_CACHE_DIR": true, "MISE_STATE_DIR": true,
	}
	out := []string{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if keep[k] {
			out = append(out, kv)
		}
	}
	return out
}

// reapBrowserMCPChild makes sure nothing of a child is left: SIGKILL to its
// process group (anything it spawned), and a Wait for the leader when the SDK's
// close did not get to reap it — Client.Connect returns some failures (an
// unsupported protocol version) without closing its transport, and that would
// otherwise be a zombie.
func reapBrowserMCPChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if pid := cmd.Process.Pid; pid > 1 { // never kill(-0)/kill(-1): see browserManager.kill
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	if cmd.ProcessState == nil {
		go func() { _ = cmd.Wait() }()
	}
}

// browserMCPStartFailure names why a child did not come up: the binary could
// not be executed, or it exited (with its status and the tail of its stderr,
// which is where node prints the actual problem), or it did not answer.
func browserMCPStartFailure(bin string, cmd *exec.Cmd, err error, tail *tailBuffer) error {
	var why string
	switch {
	case cmd.Process == nil:
		why = fmt.Sprintf("could not be started: %v", err)
	case cmd.ProcessState != nil:
		why = fmt.Sprintf("exited during startup (%s): %v", cmd.ProcessState, err)
	default:
		why = fmt.Sprintf("did not complete the MCP handshake: %v", err)
	}
	return fmt.Errorf("chrome-devtools-mcp (%s) %s%s", bin, why, browserMCPTail(tail))
}

func browserMCPTail(t *tailBuffer) string {
	s := t.String()
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return "; stderr: " + clipLine(strings.Join(lines, " | "), 800)
}

// mcpObjectSchema reports whether a schema (as the client decoded it — a map,
// or anything that marshals to one) is a JSON object schema.
func mcpObjectSchema(s any) bool {
	if s == nil {
		return false
	}
	b, err := json.Marshal(s)
	if err != nil {
		return false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	return m["type"] == "object"
}

// browserMCPStderr sends a child's stderr to lasso's log, a line at a time and
// at a bounded rate — chrome-devtools-mcp prints a disclaimer banner on every
// start and can be chatty on errors, eight of them at once more so — while
// keeping the tail for a startup failure's explanation.
type browserMCPStderr struct {
	tail *tailBuffer
	// label prefixes each logged line ("browser-mcp" when empty). Plugin
	// servers (pluginmcp.go) reuse this writer under their own name.
	label string

	mu      sync.Mutex
	pid     int
	partial []byte
	window  time.Time
	lines   int
	dropped int
}

const browserMCPLogPerMinute = 30

func (w *browserMCPStderr) name() string {
	if w.label != "" {
		return w.label
	}
	return "browser-mcp"
}

func (w *browserMCPStderr) setPID(pid int) {
	w.mu.Lock()
	w.pid = pid
	w.mu.Unlock()
}

func (w *browserMCPStderr) Write(p []byte) (int, error) {
	_, _ = w.tail.Write(p)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		i := strings.IndexByte(string(w.partial), '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(w.partial[:i]))
		w.partial = w.partial[i+1:]
		if line == "" {
			continue
		}
		now := time.Now()
		if now.Sub(w.window) >= time.Minute {
			if w.dropped > 0 {
				log.Printf("%s[%d]: (%d more stderr line(s) not logged)", w.name(), w.pid, w.dropped)
			}
			w.window, w.lines, w.dropped = now, 0, 0
		}
		if w.lines >= browserMCPLogPerMinute {
			w.dropped++
			continue
		}
		w.lines++
		who := "starting" // the pid is only known once Connect has returned
		if w.pid > 0 {
			who = fmt.Sprint(w.pid)
		}
		log.Printf("%s[%s]: %s", w.name(), who, clipLine(line, 400))
	}
	if len(w.partial) > 4096 { // a line that never ends is not worth buffering
		w.partial = w.partial[:0]
	}
	return len(p), nil
}

// withBrowserMCPAuth is /browser-mcp's gate. It is a front door to /cdp — the
// child it spawns gets through /cdp on lasso's internal token — so it must be
// at least as strict as withCDPAuth, and it follows /mcp for the token rules:
//
//   - the Origin guard first: no Origin (a CLI or agent's MCP client) or lasso's
//     own origin only, so no web page a user visits can open a session and
//     drive the browser through a loopback lasso.
//   - MCP_OAUTH set: what /mcp accepts (a lasso bearer token, or the UI_AUTH
//     basic credentials), and a token's scope must reach lasso's own machine,
//     where the browser runs (cdpScopeCheck — 403 otherwise).
//   - only UI_AUTH set: its basic credentials. Unlike /mcp, which is open in
//     this configuration, because /cdp is not.
//   - neither: open, /mcp's and /cdp's trust model.
//
// There is no same-origin-page allowance like /cdp's: no page of lasso's speaks
// MCP. Cloudflare Access (gate.wrap) still fronts all of it.
func withBrowserMCPAuth(next http.Handler, user, pass string, hasAuth bool) http.Handler {
	var gated http.Handler
	switch {
	case oauthCfg.Enabled:
		gated = withMCPAuth(cdpScopeCheck(next), user, pass, hasAuth)
	case hasAuth:
		gated = withAuth(next, user, pass, true)
	default:
		gated = next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cdpOriginAllowed(r) {
			http.Error(w, "cross-origin request to /browser-mcp refused", http.StatusForbidden)
			return
		}
		gated.ServeHTTP(w, r)
	})
}
