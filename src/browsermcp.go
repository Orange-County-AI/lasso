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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// /browser-mcp — the shared browser as an MCP server (see browser.go for the
// browser itself). An agent adds ONE HTTP MCP URL and gets Google's
// chrome-devtools-mcp tool surface against every profile of the Chromium the
// human watches in the Browser tab, with nothing installed on the agent's
// machine and lasso's own auth in front of it.
//
// chrome-devtools-mcp only speaks stdio — it has no HTTP server mode — so lasso
// is a BRIDGE. Every tool it mirrors gains an optional `profile` argument; the
// bridge strips it and forwards the call to this session's child for that
// profile, spawning the child on the first call that needs it. So one MCP
// session holds up to one chrome-devtools-mcp child per profile it has used,
// and a session that never calls a browser tool holds none. Children are per
// session (not shared) because chrome-devtools-mcp keeps per-client state — the
// "selected page", console/network buffers, emulation — and per profile
// because a profile is its own Chromium, which one child can only dial one of.
//
// initialize and tools/list are answered from a cached copy of
// chrome-devtools-mcp's tool list, learned once per lasso process (a short
// probe child the first time a session needs it, refreshed by every real spawn
// and re-probed when the binary changes), which is what lets a session start
// without spawning anything.
//
// /browser-mcp/<id> still exists for configs written before this: the same
// session pinned to one profile, whose tools take no `profile`.
//
// A child connects to lasso's OWN /cdp (/cdp/p/<id> for a profile), never to
// Chromium's loopback port: that address survives relaunches, launches the
// browser lazily, and its in-flight counter is what keeps the idle stop from
// firing under an agent that is still connected. It authenticates with an
// internal token (see internalCDPToken in cdpproxy.go) because an agent's
// credential never reaches lasso's own child.
//
// Lifetime: a profile's browser stopping (stop, relaunch, idle stop, crash)
// closes that profile's child in every session and nothing else — its CDP
// connection is dead, and the next call to that profile spawns a fresh one. A
// child that dies on its own is dropped the same way. The SESSION ends when the
// client DELETEs it, after browserMCPSessionTimeout idle, or at shutdown, and
// that kills every child it holds.

// browserMCPSessionTimeout reaps the children of an agent that went away
// without closing its session (a killed process, a laptop lid). The SDK pauses
// it while a POST is being answered, so a long tool call never trips it.
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

// browserMCPProfileParam is the argument the bridge adds to every mirrored
// tool of a /browser-mcp session, and strips before the call reaches a child.
const browserMCPProfileParam = "profile"

type browserMCPConfig struct {
	Binary    string // -browser-mcp / LASSO_BROWSER_MCP: a path or PATH name; "off" disables; "" = chrome-devtools-mcp
	ExtraArgs string // LASSO_BROWSER_MCP_ARGS, whitespace-split after lasso's own
	// Max is the most chrome-devtools-mcp children alive at once across every
	// session and profile (-browser-mcp-max / LASSO_BROWSER_MCP_MAX); <= 0 is no
	// limit, the default. The tool-list probe is not counted: it is what lets a
	// session start without a child, and it lives for a second.
	Max int
	// ExtraEnv is appended to the child's minimal environment. A test seam (the
	// fake child is this test binary, told what to be by an env var); nothing in
	// production sets it.
	ExtraEnv []string
}

// browserMCPBridge serves /browser-mcp.
type browserMCPBridge struct {
	cfg      browserMCPConfig
	lookPath func(string) (string, error)
	handler  *mcp.StreamableHTTPHandler

	// cdpEndpoint is ws://<the address lasso bound>/cdp, set once the listener
	// is up (the route table is built before the bind).
	cdpEndpoint atomic.Value // string

	mu       sync.Mutex
	live     map[*browserMCPSession]struct{} // initialized sessions, with or without children
	children int                             // counted children alive, across every session
	starting int                             // slots reserved by spawns still in flight

	// tools is chrome-devtools-mcp's tool list, keyed by the binary it came
	// from; probeMu makes a burst of first initializes share one probe.
	tools   atomic.Pointer[browserMCPToolCache]
	probeMu sync.Mutex
	probes  atomic.Int64 // probe children spawned, for tests
}

// browserMCPToolCache is one binary's tool list, already filtered to what
// Server.AddTool accepts. Sessions register copies; nothing mutates it.
type browserMCPToolCache struct {
	key   string
	tools []*mcp.Tool
}

// browserMCP is the process-wide bridge main wires up; nil reads as a feature
// that is not configured.
var browserMCP *browserMCPBridge

func newBrowserMCPBridge(cfg browserMCPConfig) *browserMCPBridge {
	b := &browserMCPBridge{cfg: cfg, lookPath: exec.LookPath, live: map[*browserMCPSession]struct{}{}}
	b.cdpEndpoint.Store("")
	// getServer is called for every request that carries no session id — in
	// practice the initialize that opens a session (anything else without an id
	// fails initialization and the SDK closes it). Each gets a server of its
	// own, since a session's children are its own.
	b.handler = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		// /browser-mcp/<id> pins the session to that profile (ServeHTTP has
		// already refused a malformed or unknown one); bare /browser-mcp routes.
		return b.newSessionServer(strings.Trim(strings.TrimPrefix(r.URL.Path, "/browser-mcp"), "/"))
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

// sessions is the number of sessions holding at least one child: the agents
// actually using the browser, not every client that merely loaded the server.
func (b *browserMCPBridge) sessions() int {
	n := 0
	for _, s := range b.snapshot() {
		s.mu.Lock()
		for _, sl := range s.children {
			if sl.child != nil {
				n++
				break
			}
		}
		s.mu.Unlock()
	}
	return n
}

// liveChildren is the number of counted children alive.
func (b *browserMCPBridge) liveChildren() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.children
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

// browserMCPPathFor is the /browser-mcp address that drives a profile. It is
// the one URL for every profile now — its tools take `profile` — so an agent
// told about a profile is never steered into adding a second MCP server.
// /browser-mcp/<id> still answers, for configs that already name it.
func browserMCPPathFor(string) string {
	return "/browser-mcp"
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

// ---------------------------------------------------------------------------
// the tool list: learned once, answered from memory
// ---------------------------------------------------------------------------

// browserMCPBinKey identifies the binary a tool list came from: its resolved
// path (a mise install dir names the version), size and mtime, so an upgrade
// in place reads as a different binary and is probed again. A shim whose
// target moves without the shim changing is caught by the refresh every real
// spawn does instead (learnTools).
func browserMCPBinKey(bin string) string {
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		real = bin
	}
	st, err := os.Stat(real)
	if err != nil {
		return real
	}
	return fmt.Sprintf("%s|%d|%d", real, st.Size(), st.ModTime().UnixNano())
}

// toolList is chrome-devtools-mcp's tool list, from the cache when it was
// learned from this binary, else from a probe child that lists its tools and
// exits. chrome-devtools-mcp dials the browser only when a tool runs, so the
// probe starts no Chromium (measured on 1.10.1 with an unreachable
// --wsEndpoint: tools/list answers all 30 tools).
func (b *browserMCPBridge) toolList(ctx context.Context) ([]*mcp.Tool, error) {
	bin, reason, ok := b.resolve()
	if !ok {
		return nil, errors.New(reason)
	}
	key := browserMCPBinKey(bin)
	if c := b.tools.Load(); c != nil && c.key == key {
		return c.tools, nil
	}
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	if c := b.tools.Load(); c != nil && c.key == key { // a concurrent initialize probed first
		return c.tools, nil
	}
	b.probes.Add(1)
	ch, tools, err := b.spawn(ctx, bin, defaultBrowserProfile)
	if err != nil {
		return nil, err
	}
	ch.stop("tool-list probe done")
	return b.learnTools(key, tools), nil
}

// learnTools stores a tool list (filtered to what AddTool accepts) as the
// cache for a binary and returns it. A real spawn calls it too, so an upgrade
// the key could not see is picked up by the next session to start. Sessions
// already open keep the list they started with.
func (b *browserMCPBridge) learnTools(key string, tools []*mcp.Tool) []*mcp.Tool {
	var ok []*mcp.Tool
	for _, t := range tools {
		if !mcpObjectSchema(t.InputSchema) {
			// Server.AddTool panics on a non-object input schema. A child that
			// ships one has a broken tool, not a broken session: skip it.
			log.Printf("browser-mcp: skipping tool %q: its input schema is not an object", t.Name)
			continue
		}
		c := *t
		if c.OutputSchema != nil && !mcpObjectSchema(c.OutputSchema) {
			c.OutputSchema = nil
		}
		ok = append(ok, &c)
	}
	prev := b.tools.Load()
	if prev != nil && sameTools(prev.tools, ok) {
		if prev.key != key {
			b.tools.Store(&browserMCPToolCache{key: key, tools: prev.tools})
		}
		return prev.tools
	}
	if prev != nil {
		log.Printf("browser-mcp: chrome-devtools-mcp's tool list changed (%d → %d tools); new sessions get the new one", len(prev.tools), len(ok))
	}
	b.tools.Store(&browserMCPToolCache{key: key, tools: ok})
	return ok
}

func sameTools(a, b []*mcp.Tool) bool {
	if len(a) != len(b) {
		return false
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// sessionTool is a session's copy of a cached tool: the schema cloned (AddTool
// and the SDK get a value nobody else holds), and for an unpinned session the
// `profile` argument added. own reports a tool that already has a `profile`
// argument of its own, which is then passed through untouched and never routed.
func sessionTool(t *mcp.Tool, profileDesc string) (tool *mcp.Tool, own bool) {
	c := *t
	var schema map[string]any
	raw, _ := json.Marshal(t.InputSchema)
	_ = json.Unmarshal(raw, &schema)
	props, _ := schema["properties"].(map[string]any)
	if _, has := props[browserMCPProfileParam]; has {
		own = true
	}
	if profileDesc != "" && !own {
		if props == nil {
			props = map[string]any{}
			schema["properties"] = props
		}
		props[browserMCPProfileParam] = map[string]any{"type": "string", "description": profileDesc}
		if pid, ok := props["pageId"].(map[string]any); ok {
			d, _ := pid["description"].(string)
			pid["description"] = strings.TrimSpace(d + " A pageId belongs to ONE profile's browser: pass the same `profile` as the list_pages/new_page call it came from.")
		}
	}
	c.InputSchema = schema
	return &c, own
}

// ---------------------------------------------------------------------------
// sessions and their children
// ---------------------------------------------------------------------------

// browserMCPChild is one chrome-devtools-mcp process, dialing one profile.
type browserMCPChild struct {
	b       *browserMCPBridge
	profile string
	client  *mcp.ClientSession
	cmd     *exec.Cmd
	pid     int
	counted bool // holds one of the bridge's children slots
	once    sync.Once
}

// stop ends the child, once: the SDK's stdio close (stdin, SIGTERM, SIGKILL,
// browserMCPTerminate apart), then a SIGKILL to its process group and a reap.
func (c *browserMCPChild) stop(why string) {
	c.once.Do(func() {
		_ = c.client.Close()
		reapBrowserMCPChild(c.cmd)
		if c.counted {
			c.b.mu.Lock()
			c.b.children--
			c.b.mu.Unlock()
		}
		log.Printf("browser-mcp: child pid %d for profile %q stopped (%s)", c.pid, c.profile, why)
	})
}

// browserMCPSlot is a session's child for one profile: spawning until ready
// closes, then either child or err.
type browserMCPSlot struct {
	ready chan struct{}
	child *browserMCPChild
	err   error
}

// browserMCPSession is one MCP session and the children behind it.
type browserMCPSession struct {
	b   *browserMCPBridge
	srv *mcp.Server
	// pinned is the profile of a /browser-mcp/<id> session; "" is the bare
	// /browser-mcp, whose calls name their profile.
	pinned string

	mu       sync.Mutex
	ss       *mcp.ServerSession
	started  bool
	closed   bool
	children map[string]*browserMCPSlot // by profile id
}

func (b *browserMCPBridge) newSessionServer(pinned string) *mcp.Server {
	s := &browserMCPSession{b: b, pinned: pinned, children: map[string]*browserMCPSlot{}}
	s.srv = mcp.NewServer(&mcp.Implementation{
		Name:    "lasso-browser",
		Title:   "Lasso shared browser (chrome-devtools-mcp)",
		Version: lassoSemver,
	}, &mcp.ServerOptions{
		Instructions: s.instructions(),
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

// profilesLine lists the profiles as they are now, for text a model reads.
// It is a snapshot — calls resolve profiles when they run — and empty when
// lasso has no database (tests that never touch profiles).
func profilesLine() string {
	if db == nil {
		return ""
	}
	var parts []string
	for _, p := range allBrowserProfiles() {
		parts = append(parts, fmt.Sprintf("%s (%q)", p.ID, p.Name))
	}
	return strings.Join(parts, ", ")
}

func (s *browserMCPSession) instructions() string {
	if s.pinned != "" {
		return browserMCPInstructions + "\n- This session drives the browser PROFILE \"" + s.pinned + "\": its own Chromium, with its own cookies, logins and proxy. Other profiles' pages are not visible here. The bare /browser-mcp URL drives every profile from one MCP server (each tool takes `profile`)."
	}
	line := "\n- Every tool takes an optional `profile`: a browser profile's id or display name, omitted = the default profile. Each profile is its own Chromium with its own cookies, logins, proxy and PAGES, so a pageId from list_pages/new_page means something only in the profile it came from: pass the same `profile` on every call about that page. Profiles created later work here without reconnecting; lasso's list_browser_profiles tool has the current list."
	if ps := profilesLine(); ps != "" {
		line += " Profiles when this session started: " + ps + "."
	}
	return browserMCPInstructions + line
}

func (s *browserMCPSession) profileParamDesc() string {
	d := "Browser profile to run this in: its id or display name (lasso's list_browser_profiles shows them). Omit for the default profile. Each profile is a separate Chromium with its own cookies, logins, proxy and pages; a pageId from one profile means nothing in another."
	if ps := profilesLine(); ps != "" {
		d += " At session start: " + ps + "."
	}
	return d
}

// start registers the session's tools from the cached list (probing once if
// there is none) and the session itself. It spawns no child: that waits for a
// tool call. It runs inside the initialize request, so a failure — no
// chrome-devtools-mcp, or a probe that could not start — is the initialize's.
func (s *browserMCPSession) start(ctx context.Context, ss *mcp.ServerSession) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil // a repeated initialize on a live session: nothing to redo
	}
	s.started = true
	s.mu.Unlock()
	if ss == nil {
		return errors.New("browser MCP: no server session to bind to")
	}
	b := s.b
	if s.pinned != "" {
		if _, err := browserFor(s.pinned); err != nil {
			return err
		}
	}
	cctx, cancel := context.WithTimeout(ctx, browserMCPStartTimeout)
	defer cancel()
	tools, err := b.toolList(cctx)
	if err != nil {
		return err
	}
	desc := ""
	if s.pinned == "" {
		desc = s.profileParamDesc()
	}
	for _, t := range tools {
		st, own := sessionTool(t, desc)
		if own {
			log.Printf("browser-mcp: tool %q has a %q argument of its own; it is passed through and runs in the session's default profile", t.Name, browserMCPProfileParam)
		}
		s.srv.AddTool(st, s.forward(t.Name, own))
	}

	s.mu.Lock()
	s.ss = ss
	s.mu.Unlock()
	b.mu.Lock()
	b.live[s] = struct{}{}
	n := len(b.live)
	b.mu.Unlock()
	where := "every profile"
	if s.pinned != "" {
		where = fmt.Sprintf("profile %q", s.pinned)
	}
	log.Printf("browser-mcp: session opened for %s (%d tools, no child yet; %d sessions)", where, len(tools), n)
	// The session ending (DELETE, idle timeout, shutdown) kills its children.
	go func() { _ = ss.Wait(); s.end("session ended") }()
	return nil
}

// forward is a mirrored tool's handler: it picks the profile, strips the
// `profile` argument, and hands the call to that profile's child as-is (name
// and raw arguments), returning its result as-is — Content (images too),
// StructuredContent, IsError and _meta — so the bridge adds nothing a client
// could tell apart from talking to chrome-devtools-mcp directly.
func (s *browserMCPSession) forward(name string, own bool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		profile, args, refusal := s.route(req.Params.Arguments, own)
		if refusal != "" {
			return browserMCPToolError(refusal), nil
		}
		child, err := s.child(ctx, profile)
		if err != nil {
			return browserMCPToolError(err.Error()), nil
		}
		p := &mcp.CallToolParams{Name: name, Meta: req.Params.Meta}
		if len(args) > 0 {
			p.Arguments = args
		}
		return child.client.CallTool(ctx, p)
	}
}

func browserMCPToolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// route decides which profile a call runs in and strips `profile` from its
// arguments. Profiles are resolved NOW, not at initialize, so one created
// after the session started works and one deleted since is refused. An
// unknown profile is a refusal naming the current ones — never a quiet
// fallback to another profile, which would act in the wrong browser with the
// wrong logins. A pinned session accepts only its own profile.
func (s *browserMCPSession) route(raw json.RawMessage, own bool) (profile string, args json.RawMessage, refusal string) {
	profile = s.pinned
	if profile == "" {
		profile = defaultBrowserProfile
	}
	if own || len(raw) == 0 {
		return profile, raw, ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return profile, raw, "" // not an object: the child's to refuse
	}
	v, has := m[browserMCPProfileParam]
	if !has {
		return profile, raw, ""
	}
	delete(m, browserMCPProfileParam)
	args, _ = json.Marshal(m)
	var name string
	if string(v) != "null" {
		if err := json.Unmarshal(v, &name); err != nil {
			return "", nil, "`profile` must be a string: a browser profile's id or display name"
		}
	}
	if strings.TrimSpace(name) == "" {
		return profile, args, ""
	}
	id, err := resolveProfile(name)
	if err != nil {
		return "", nil, err.Error()
	}
	if s.pinned != "" && id != s.pinned {
		return "", nil, fmt.Sprintf("this session is pinned to browser profile %q (it was opened at /browser-mcp/%s); omit `profile`, or connect to /browser-mcp to drive every profile", s.pinned, s.pinned)
	}
	return id, args, ""
}

// child is this session's child for a profile, spawning it on first use.
// Concurrent calls for one profile share one spawn. The spawn runs detached
// from the caller, so a call that gives up still leaves the child ready for
// the next; a failed spawn is forgotten, so the next call tries again.
func (s *browserMCPSession) child(ctx context.Context, profile string) (*browserMCPChild, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("this browser session has ended; reconnect to start a new one")
	}
	sl := s.children[profile]
	if sl == nil {
		sl = &browserMCPSlot{ready: make(chan struct{})}
		s.children[profile] = sl
		go s.spawnInto(sl, profile)
	}
	s.mu.Unlock()
	select {
	case <-sl.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if sl.err != nil {
		return nil, sl.err
	}
	return sl.child, nil
}

func (s *browserMCPSession) spawnInto(sl *browserMCPSlot, profile string) {
	b := s.b
	fail := func(err error) {
		s.mu.Lock()
		if s.children[profile] == sl {
			delete(s.children, profile)
		}
		sl.err = err
		s.mu.Unlock()
		close(sl.ready)
	}
	if profile != defaultBrowserProfile {
		// The default profile always exists; another may have been deleted
		// between the call's resolve and here.
		if _, err := browserFor(profile); err != nil {
			fail(err)
			return
		}
	}
	bin, reason, ok := b.resolve()
	if !ok {
		fail(errors.New(reason))
		return
	}
	if err := b.reserve(); err != nil {
		fail(err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), browserMCPStartTimeout)
	defer cancel()
	ch, tools, err := b.spawn(ctx, bin, profile)
	if err != nil {
		b.unreserve()
		fail(err)
		return
	}
	b.mu.Lock()
	b.starting--
	b.children++
	n := b.children
	b.mu.Unlock()
	ch.counted = true
	b.learnTools(browserMCPBinKey(bin), tools)

	s.mu.Lock()
	if s.closed || s.children[profile] != sl {
		s.mu.Unlock()
		ch.stop("session ended while it was starting")
		fail(errors.New("this browser session has ended; reconnect to start a new one"))
		return
	}
	sl.child = ch
	s.mu.Unlock()
	close(sl.ready)
	limit := "no limit"
	if b.cfg.Max > 0 {
		limit = fmt.Sprintf("limit %d", b.cfg.Max)
	}
	log.Printf("browser-mcp: child pid %d started for profile %q (%d live, %s)", ch.pid, profile, n, limit)

	// A child that dies on its own (a crash, an OOM kill) is dropped; the next
	// call to its profile spawns a fresh one. The session is untouched.
	go func() {
		_ = ch.client.Wait()
		s.drop(profile, sl, "child exited")
	}()
}

// drop forgets a profile's child (if it is still that slot's) and stops it.
func (s *browserMCPSession) drop(profile string, sl *browserMCPSlot, why string) {
	s.mu.Lock()
	if s.children[profile] == sl {
		delete(s.children, profile)
	}
	s.mu.Unlock()
	if sl.child != nil {
		sl.child.stop(why)
	}
}

// end closes the session's side of things once: every child stopped, the
// session unregistered. A spawn still in flight sees closed and stops its own.
func (s *browserMCPSession) end(why string) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.closed = true
	var kids []*browserMCPChild
	for _, sl := range s.children {
		if sl.child != nil {
			kids = append(kids, sl.child)
		}
	}
	s.children = map[string]*browserMCPSlot{}
	s.mu.Unlock()

	s.b.mu.Lock()
	delete(s.b.live, s)
	s.b.mu.Unlock()
	stopChildren(kids, why)
	return true
}

func stopChildren(kids []*browserMCPChild, why string) {
	var wg sync.WaitGroup
	for _, c := range kids {
		wg.Add(1)
		go func() { defer wg.Done(); c.stop(why) }()
	}
	wg.Wait()
}

// spawn starts a chrome-devtools-mcp dialing a profile's /cdp and lists its
// tools. The caller owns the child (and, for a counted one, the slot).
func (b *browserMCPBridge) spawn(ctx context.Context, bin, profile string) (*browserMCPChild, []*mcp.Tool, error) {
	endpoint, _ := b.cdpEndpoint.Load().(string)
	if endpoint == "" {
		return nil, nil, errors.New("lasso is not listening yet; retry in a moment")
	}
	if profile != defaultBrowserProfile {
		// cdpEndpoint is ws://<addr>/cdp; the profile's browser is one level down.
		endpoint += strings.TrimPrefix(cdpPathFor(profile), "/cdp")
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

	client := mcp.NewClient(&mcp.Implementation{Name: "lasso", Version: lassoSemver}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: browserMCPTerminate}, nil)
	if err != nil {
		reapBrowserMCPChild(cmd)
		return nil, nil, browserMCPStartFailure(bin, cmd, err, tail)
	}
	pid := cmd.Process.Pid
	logw.setPID(pid)
	ch := &browserMCPChild{b: b, profile: profile, client: cs, cmd: cmd, pid: pid}

	var tools []*mcp.Tool
	for t, err := range cs.Tools(ctx, nil) { // follows nextCursor pagination
		if err != nil {
			ch.stop("tools/list failed")
			return nil, nil, fmt.Errorf("chrome-devtools-mcp (%s) started but tools/list failed: %v%s", bin, err, browserMCPTail(tail))
		}
		tools = append(tools, t)
	}
	return ch, tools, nil
}

// closeAll ends every session — its children stopped, then the server session
// closed so the client sees it gone — and waits. Called at shutdown.
func (b *browserMCPBridge) closeAll(why string) {
	sessions := b.snapshot()
	if len(sessions) == 0 {
		return
	}
	log.Printf("browser-mcp: closing %d session(s): %s", len(sessions), why)
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Children first: ServerSession.Close waits for in-flight calls, and
			// a call in flight is waiting on a child, which only answers (with an
			// error) once it is gone.
			s.end(why)
			s.mu.Lock()
			server := s.ss
			s.mu.Unlock()
			if server != nil {
				_ = server.Close()
			}
		}()
	}
	wg.Wait()
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

// browserStopped is the default profile's stop hook (browserManager.onStop).
func (b *browserMCPBridge) browserStopped(why string) {
	b.browserStoppedFor(defaultBrowserProfile, why)
}

// browserStoppedFor stops every session's child for one profile, whose browser
// just went away: their CDP connections belong to the process that ended. The
// sessions stay, and so do their children for other profiles; the next call
// to this profile spawns a child that dials the next browser. The set is taken
// NOW, synchronously — a child that connects after this moment is on the next
// browser and must survive — and stopped in the background, since the hook
// runs with the browser's launch lock held. A spawn still in flight is left
// alone: chrome-devtools-mcp dials CDP only when a tool runs, which is after
// this stop, so it reaches the next browser.
func (b *browserMCPBridge) browserStoppedFor(profile, why string) {
	var kids []*browserMCPChild
	for _, s := range b.snapshot() {
		s.mu.Lock()
		if sl := s.children[profile]; sl != nil && sl.child != nil {
			delete(s.children, profile)
			kids = append(kids, sl.child)
		}
		s.mu.Unlock()
	}
	if len(kids) > 0 {
		log.Printf("browser-mcp: profile %q's browser stopped (%s): stopping %d child(ren)", profile, why, len(kids))
		go stopChildren(kids, "profile "+profile+"'s browser stopped ("+why+")")
	}
}

// reserve takes a child slot under the opt-in limit. With no limit it still
// counts, so the log and status can say how many are alive.
func (b *browserMCPBridge) reserve() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cfg.Max > 0 && b.children+b.starting >= b.cfg.Max {
		return fmt.Errorf("lasso's browser MCP is at its limit of %d chrome-devtools-mcp processes (LASSO_BROWSER_MCP_MAX / -browser-mcp-max; 0 = no limit): an agent's session holds one per profile it has used until the session ends — close an idle agent's browser session, or raise or remove the limit", b.cfg.Max)
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
