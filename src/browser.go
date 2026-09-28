package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The shared browser: one headless Chromium that lasso launches, supervises and
// proxies, which the sidebar's Browser tab (Live mode) and any number of agents
// drive at once over CDP. The human watches a screencast of the same pages an
// agent is clicking through; an agent sees the tab the human just opened.
//
// Go has no websocket library in go.mod and gains none for this. lasso is a
// process supervisor plus a reverse proxy (cdpproxy.go): httputil.ReverseProxy
// already carries websocket upgrades — it is how /terminal/ reaches ttyd — so
// the Browser tab speaks CDP itself over the page's native WebSocket, as one
// more client of the same endpoint the agents use.
//
// It always runs on lasso's OWN machine, whatever host a tab is on, so
// `localhost` inside it means that machine. It launches lazily (the first
// /cdp request, POST /api/browser, or the shared_browser MCP tool) and stops
// after -browser-idle with nothing connected: headless Chromium on a GPU-less
// box software-rasterizes, and a forgotten one is several cores of nothing.

// browserProxySetting is the settings-table key holding the proxy URL.
const browserProxySetting = "browser_proxy"

// browserPortWait bounds how long a launch waits for DevToolsActivePort — the
// file Chromium writes once its debugging socket is bound, which is therefore
// the readiness signal as well as the way to learn a port asked for as 0.
const browserPortWait = 15 * time.Second

// browserStopGrace is how long a SIGTERM gets before the group is SIGKILLed.
const browserStopGrace = 3 * time.Second

// browserPathNames are the PATH names tried, in order, when neither -browser nor
// LASSO_BROWSER names a binary.
var browserPathNames = []string{"chromium", "chromium-browser", "google-chrome-stable", "google-chrome", "chrome"}

// browserMacApps are the macOS app-bundle binaries tried last.
var browserMacApps = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
}

// browserSearch is the space resolveBrowserBinary looks through. Every probe of
// the machine goes through a field so a test can stand a fake filesystem up.
type browserSearch struct {
	Explicit string // -browser / LASSO_BROWSER; "off" disables the feature
	LookPath func(string) (string, error)
	Glob     func(string) ([]string, error)
	Exists   func(string) bool
	Home     string
	GOOS     string
}

func defaultBrowserSearch(explicit string) browserSearch {
	home, _ := os.UserHomeDir()
	return browserSearch{
		Explicit: explicit,
		LookPath: exec.LookPath,
		Glob:     filepath.Glob,
		Exists: func(p string) bool {
			st, err := os.Stat(p)
			return err == nil && !st.IsDir()
		},
		Home: home,
		GOOS: runtime.GOOS,
	}
}

// resolveBrowserBinary finds the Chromium to launch, first hit wins. ok=false
// carries a reason the Settings pane shows as-is, so it names the remedy.
//
// An explicit choice that does not resolve is reported rather than silently
// replaced by whatever else is installed: someone who pointed lasso at a
// particular build wants to hear that it is missing, not to find a different
// browser running under their proxy settings.
func resolveBrowserBinary(s browserSearch) (path, reason string, ok bool) {
	if e := strings.TrimSpace(s.Explicit); e != "" {
		if strings.EqualFold(e, "off") {
			return "", "the shared browser is disabled (LASSO_BROWSER=off / -browser off)", false
		}
		if strings.ContainsRune(e, '/') {
			if s.Exists(e) {
				return e, "", true
			}
			return "", fmt.Sprintf("the configured browser %q does not exist", e), false
		}
		if p, err := s.LookPath(e); err == nil {
			return p, "", true
		}
		return "", fmt.Sprintf("the configured browser %q is not in PATH", e), false
	}
	for _, name := range browserPathNames {
		if p, err := s.LookPath(name); err == nil {
			return p, "", true
		}
	}
	if s.Home != "" {
		if p := newestPlaywrightChromium(s); p != "" {
			return p, "", true
		}
	}
	if s.GOOS == "darwin" {
		for _, p := range browserMacApps {
			if s.Exists(p) {
				return p, "", true
			}
		}
	}
	return "", "no Chromium found — install chromium (or Google Chrome), or set LASSO_BROWSER to its path", false
}

// newestPlaywrightChromium picks the highest-numbered Playwright Chromium build.
// The directories are chromium-<build>, and a lexical sort would put 999 above
// 1243, so the suffix is compared as a number.
func newestPlaywrightChromium(s browserSearch) string {
	matches, _ := s.Glob(filepath.Join(s.Home, ".cache", "ms-playwright", "chromium-*", "chrome-linux*", "chrome"))
	type cand struct {
		path  string
		build int
	}
	var cs []cand
	for _, m := range matches {
		dir := filepath.Base(filepath.Dir(filepath.Dir(m))) // chromium-<build>
		n, err := strconv.Atoi(strings.TrimPrefix(dir, "chromium-"))
		if err != nil || !s.Exists(m) {
			continue
		}
		cs = append(cs, cand{m, n})
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].build > cs[j].build })
	if len(cs) == 0 {
		return ""
	}
	return cs[0].path
}

// validateBrowserProxy checks a proxy setting against what Chromium's
// --proxy-server actually honours and returns it normalized. "" means none.
//
// It may be a comma-separated FALLBACK list, tried in order —
// "socks5://127.0.0.1:1080,direct://" uses the proxy and connects directly
// when the proxy refuses (a tunnel that is down). That is Chromium's own
// --proxy-server list syntax, so it is passed through as-is; direct:// is only
// accepted last, since nothing after it would ever be tried.
func validateBrowserProxy(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, ",") {
		return validateOneProxy(s)
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if strings.EqualFold(p, "direct://") || strings.EqualFold(p, "direct") {
			if i != len(parts)-1 {
				return "", fmt.Errorf("direct:// must come last in a proxy list: nothing after it is ever tried")
			}
			if i == 0 {
				return "", fmt.Errorf("a proxy list needs a proxy before direct://")
			}
			out = append(out, "direct://")
			continue
		}
		v, err := validateOneProxy(p)
		if err != nil {
			return "", err
		}
		if v == "" {
			return "", fmt.Errorf("empty entry in proxy list %q", s)
		}
		out = append(out, v)
	}
	return strings.Join(out, ","), nil
}

// validateOneProxy checks one proxy URL of a --proxy-server setting.
func validateOneProxy(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Opaque != "" || u.Scheme == "" {
		return "", fmt.Errorf("proxy must be a URL like socks5://host:1080 (schemes: socks5, socks4, http, https), or a fallback list like socks5://host:1080,direct://")
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks4", "http", "https":
	default:
		// socks5h in particular: curl's spelling for "resolve DNS through the
		// proxy", which Chromium does not accept — and does not need, since its
		// socks5 already resolves hostnames on the proxy side.
		return "", fmt.Errorf("unsupported proxy scheme %q: Chromium accepts socks5://, socks4://, http:// or https:// (socks5 already sends DNS through the proxy)", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("proxy credentials are not supported: Chromium cannot authenticate to a SOCKS proxy, and --proxy-server ignores a username/password for HTTP ones")
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("proxy URL has no host")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("proxy port %q is out of range", p)
		}
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("proxy URL must be scheme://host[:port], with no path or query")
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host, nil
}

// browserArgs is Chromium's command line, minus the binary.
//
// --remote-debugging-port=0 lets the kernel pick the port, which is then read
// back from DevToolsActivePort: a fixed port would collide with a second lasso,
// a playwright run, or anything else on the box that thought 9222 was its own.
// --no-sandbox only as root, where Chromium refuses to start without it; as an
// ordinary user the sandbox works and is worth keeping.
//
// --force-device-scale-factor is what makes the Browser tab sharp on a HiDPI
// screen, and nothing later can do it: Page.startScreencast always delivers
// frames at the page's CSS size, ignoring an Emulation device-scale override
// (measured: a page at devicePixelRatio 2 still streamed 474x585 for a 474x585
// viewport), and captureScreenshot, which does honor the override, repaints
// and so re-triggers the screencast. Only a scale set at launch reaches the
// frames. Layout is unchanged (CSS px); raster cost and agent screenshots grow
// by scale squared. "1" (or empty) leaves Chromium at its default.
func browserArgs(profile, proxy string, root bool, scale, extra string) []string {
	args := []string{
		"--headless=new",
		"--remote-debugging-port=0",
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--window-size=1280,800",
		// navigator.webdriver reads true without this, and bot checks
		// (Cloudflare's Turnstile among them) refuse such a browser outright —
		// including for the human logging in by hand in the Browser tab. CDP
		// keeps working; only the page-visible automation flag goes.
		"--disable-blink-features=AutomationControlled",
	}
	if scale != "" && scale != "1" {
		args = append(args, "--force-device-scale-factor="+scale)
	}
	if proxy != "" {
		args = append(args, "--proxy-server="+proxy)
	}
	if root {
		args = append(args, "--no-sandbox")
	}
	args = append(args, strings.Fields(extra)...)
	return append(args, "about:blank")
}

// browserUAs caches browserUserAgent per binary: asking costs a process spawn.
var browserUAs sync.Map // bin -> string

// browserUserAgent is the user agent a regular (headed) Chrome of this
// binary's version sends, "" when the version cannot be read. Headless Chrome
// announces itself as "HeadlessChrome/<v>", which bot checks refuse on sight,
// and --user-agent is the one place to change it for every page and worker at
// once. The version is the binary's own, in the reduced form Chrome itself
// sends (<major>.0.0.0), so the string stays true to what is running.
func browserUserAgent(bin string) string {
	if v, ok := browserUAs.Load(bin); ok {
		return v.(string)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	ua := ""
	if err == nil {
		ua = userAgentFor(string(out), runtime.GOOS)
	}
	browserUAs.Store(bin, ua)
	return ua
}

// userAgentFor builds the UA from `chrome --version` output ("Google Chrome
// 154.0.8037.57", "Chromium 140.0.7339.80 built on Debian") for goos.
func userAgentFor(version, goos string) string {
	major := ""
	for _, f := range strings.Fields(version) {
		if i := strings.IndexByte(f, '.'); i > 0 {
			if _, err := strconv.Atoi(f[:i]); err == nil {
				major = f[:i]
				break
			}
		}
	}
	if major == "" {
		return ""
	}
	platform := "X11; Linux x86_64"
	switch goos {
	case "darwin":
		platform = "Macintosh; Intel Mac OS X 10_15_7"
	case "windows":
		platform = "Windows NT 10.0; Win64; x64"
	}
	return "Mozilla/5.0 (" + platform + ") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + major + ".0.0.0 Safari/537.36"
}

// validBrowserScale accepts a positive number up to 4 and answers "" for
// anything else, logging it: a typo in an env var should cost sharpness, not
// the browser (Chromium silently ignores a malformed scale anyway).
func validBrowserScale(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 || f > 4 {
		log.Printf("browser:  ignoring browser scale %q (want a number in (0, 4])", v)
		return ""
	}
	return v
}

// browserCap is the systemd resource cap a launch is wrapped in.
type browserCap struct {
	CPU string // CPUQuota, e.g. "150%"
	Mem string // MemoryHigh, e.g. "1536M"
}

func (c browserCap) empty() bool { return c.CPU == "" && c.Mem == "" }

// capLimit normalizes one limit from a flag or env: "off" (any case) and ""
// both mean "no limit of this kind".
func capLimit(v string) string {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "off") {
		return ""
	}
	return v
}

// envOrDefault is the env var when it is SET — an empty value included, which
// for a cap means "off" — and def otherwise. The flags default to it, so a
// flag on the command line still wins over the environment.
func envOrDefault(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// envDuration reads a Go duration ("30m", "2h", "0") from the environment,
// falling back to def when unset or unparseable (said once, at startup).
func envDuration(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		log.Printf("browser: ignoring %s=%q (want a duration like 30m): using %s", name, v, def)
		return def
	}
	return d
}

// envInt reads a non-negative integer from the environment (0 meaning "no
// limit" to LASSO_BROWSER_MCP_MAX, its one caller), falling back to def when
// unset or unusable (said once, at startup).
func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		log.Printf("browser: ignoring %s=%q (want a whole number, 0 for no limit): using %d", name, v, def)
		return def
	}
	return n
}

// browserCommand turns a binary + args into what exec runs. With a cap it is a
// transient systemd scope: --scope makes systemd-run register the scope and
// then exec the command IN PLACE, so the child pid lasso holds is Chromium's own
// and the process-group signalling and Pdeathsig still reach it. The cap is
// what keeps a software-rasterizing headless browser from taking a GPU-less
// box's cores; --collect drops the scope even when Chromium exits non-zero.
func browserCommand(bin string, args []string, c browserCap) (string, []string) {
	if c.empty() {
		return bin, args
	}
	argv := []string{"--user", "--scope", "--quiet", "--collect"}
	if c.CPU != "" {
		argv = append(argv, "-p", "CPUQuota="+c.CPU)
	}
	if c.Mem != "" {
		argv = append(argv, "-p", "MemoryHigh="+c.Mem)
	}
	// Chromium's browser process otherwise asks the session bus to move it into
	// a transient scope of its own (app-org.chromium.Chromium-<pid>.scope, meant
	// for the XDG portal's app identification) — an UNCAPPED one, carrying every
	// process it launches afterwards out from under the quota. Measured: the
	// browser process escaped while only the zygote-forked renderers stayed.
	// Headless needs no session bus, so it is given an address that cannot be
	// dialed; `env` execs in place, so the pid lasso holds is still Chromium's.
	// Only inside the scope: systemd-run itself needs the bus to create it.
	argv = append(argv, "--", "env", "DBUS_SESSION_BUS_ADDRESS=disabled:", bin)
	return "systemd-run", append(argv, args...)
}

// canCapBrowser reports whether a systemd user scope is on offer: linux, a
// systemd-run to ask with, and a user manager to answer (XDG_RUNTIME_DIR is how
// systemd-run --user finds it — absent under a bare ssh session or a container).
func canCapBrowser() bool {
	if runtime.GOOS != "linux" || os.Getenv("XDG_RUNTIME_DIR") == "" {
		return false
	}
	_, err := exec.LookPath("systemd-run")
	return err == nil
}

// parseDevToolsActivePort reads Chromium's DevToolsActivePort: line 1 is the
// port, line 2 the browser target's websocket path.
func parseDevToolsActivePort(b []byte) (int, string, error) {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 {
		return 0, "", fmt.Errorf("DevToolsActivePort is incomplete")
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || port < 1 || port > 65535 {
		return 0, "", fmt.Errorf("DevToolsActivePort has no valid port")
	}
	path := strings.TrimSpace(lines[1])
	if !strings.HasPrefix(path, "/devtools/browser/") {
		return 0, "", fmt.Errorf("DevToolsActivePort has an unexpected path %q", path)
	}
	return port, path, nil
}

// tailBuffer keeps the last max bytes written to it. Chromium's stderr is
// chatty for the life of the process and almost none of it matters; the tail is
// kept for the one case that does — a launch that dies before it is ready, whose
// last lines are the only explanation there is.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// browserProc is one running Chromium.
type browserProc struct {
	cmd     *exec.Cmd
	bin     string // the Chromium binary (cmd.Path is systemd-run when capped)
	pid     int
	port    int
	wsPath  string // /devtools/browser/<id>
	started time.Time
	capped  bool
	exited  chan struct{}
	stderr  *tailBuffer
	// stopping marks a stop lasso asked for, so the exit watcher does not
	// record it as a crash.
	stopping atomic.Bool
}

func (p *browserProc) target() string { return fmt.Sprintf("127.0.0.1:%d", p.port) }

type browserConfig struct {
	Explicit string        // -browser / LASSO_BROWSER
	Idle     time.Duration // stop after this long with nothing connected; 0 = never
	Cap      browserCap    // empty = no systemd scope
	Dir      string        // lasso's data dir; the profile lives under it
	Scale    string        // --force-device-scale-factor; "" or "1" = default
	// AuthRequired says /cdp demands credentials (UI_AUTH or MCP_OAUTH), which
	// the MCP tool tells an agent before it tries to connect bare.
	AuthRequired bool
}

// browserManager supervises one profile's Chromium. At most one runs per
// profile; every profile is its own process (see browserprofiles.go for why).
type browserManager struct {
	cfg    browserConfig
	search func() browserSearch
	proxy  func() string // the stored proxy setting
	// saveProxy stores a new proxy for this profile. The default profile keeps
	// it in the browser_proxy setting it always had; nil means that.
	saveProxy func(string) error
	// id is the profile this manager runs ("" reads as the default profile)
	// and dir its user-data-dir ("" = the default <lassoDir>/browser-profile).
	id  string
	dir string
	// retired marks a deleted profile: a request still holding the manager
	// must not relaunch a browser into the directory being removed.
	retired atomic.Bool

	// sem serializes launches and stops, and is a channel rather than a mutex
	// so a request waiting on someone else's launch can give up with its ctx.
	sem chan struct{}

	mu       sync.Mutex
	proc     *browserProc
	lastErr  string
	note     string // the last relaunch's report (pages reopened)
	lastUsed time.Time

	inflight atomic.Int64

	// onStop runs whenever a browser process goes away — a stop, an idle stop,
	// a relaunch's stop half, a crash — with the reason. The /browser-mcp bridge
	// hangs off it: every session's child holds a CDP connection to the process
	// that just ended. It may run with sem held, so it must not block on it.
	onStop func(why string)
}

func newBrowserManager(cfg browserConfig) *browserManager {
	return &browserManager{
		cfg:    cfg,
		search: func() browserSearch { return defaultBrowserSearch(cfg.Explicit) },
		proxy: func() string {
			v, _ := getSetting(browserProxySetting)
			return v
		},
		sem: make(chan struct{}, 1),
	}
}

// sharedBrowser is the process-wide manager main wires up; nil reads as a
// feature that is not configured.
var sharedBrowser *browserManager

func (m *browserManager) profileDir() string {
	if m.dir != "" {
		return m.dir
	}
	return filepath.Join(m.cfg.Dir, "browser-profile")
}

// profileID is the profile this manager runs.
func (m *browserManager) profileID() string {
	if m.id == "" {
		return defaultBrowserProfile
	}
	return m.id
}

func (m *browserManager) pidFile() string { return filepath.Join(m.profileDir(), "lasso-browser.pid") }

// begin/end bracket every /cdp request. A proxied websocket holds ServeHTTP for
// the life of the connection, so the in-flight count IS the number of live CDP
// clients plus the requests in the middle of being answered — which is exactly
// what the idle stop must see as zero before it is allowed to act.
func (m *browserManager) begin() { m.inflight.Add(1); m.touch() }
func (m *browserManager) end()   { m.inflight.Add(-1); m.touch() }

func (m *browserManager) touch() {
	m.mu.Lock()
	m.lastUsed = time.Now()
	m.mu.Unlock()
}

func (m *browserManager) current() *browserProc {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proc
}

func (m *browserManager) acquire(ctx context.Context) error {
	select {
	case m.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *browserManager) release() { <-m.sem }

// ensure returns the running browser, launching it if nothing is.
func (m *browserManager) ensure(ctx context.Context) (*browserProc, error) {
	if p := m.current(); p != nil {
		return p, nil
	}
	if err := m.acquire(ctx); err != nil {
		return nil, err
	}
	defer m.release()
	if p := m.current(); p != nil { // someone else's launch landed while we waited
		return p, nil
	}
	return m.startLocked()
}

// startLocked launches Chromium. The caller holds sem.
func (m *browserManager) startLocked() (*browserProc, error) {
	if m.retired.Load() {
		return nil, fmt.Errorf("the browser profile %q was deleted", m.profileID())
	}
	bin, reason, ok := resolveBrowserBinary(m.search())
	if !ok {
		m.setErr(reason)
		return nil, errors.New(reason)
	}
	proxy, err := validateBrowserProxy(m.proxy())
	if err != nil {
		// A stored value that no longer validates (hand-edited db, an older
		// build's looser rules) must not wedge the feature: launch direct and
		// say why, rather than refusing to start at all.
		log.Printf("browser: ignoring stored proxy: %v", err)
		proxy = ""
	}
	if err := os.MkdirAll(m.profileDir(), 0o700); err != nil {
		m.setErr(err.Error())
		return nil, err
	}
	if err := m.reclaimProfile(); err != nil {
		m.setErr(err.Error())
		return nil, err
	}
	extra := os.Getenv("LASSO_BROWSER_ARGS")
	args := browserArgs(m.profileDir(), proxy, os.Geteuid() == 0, m.cfg.Scale, extra)
	if ua := browserUserAgent(bin); ua != "" && !strings.Contains(extra, "--user-agent") {
		// Before the trailing about:blank, which must stay last.
		args = append(args[:len(args)-1:len(args)-1], "--user-agent="+ua, "about:blank")
	}
	capped := !m.cfg.Cap.empty() && canCapBrowser()
	var p *browserProc
	if capped {
		p, err = m.launch(bin, args, m.cfg.Cap)
		if err != nil {
			// The scope is the part most likely to be refused (no user manager
			// behind XDG_RUNTIME_DIR after all, a delegation policy that denies
			// CPUQuota): an uncapped browser beats none.
			log.Printf("browser: capped launch failed (%v) — retrying without the systemd scope", err)
			capped = false
		}
	}
	if !capped {
		p, err = m.launch(bin, args, browserCap{})
	}
	if err != nil {
		m.setErr(err.Error())
		return nil, err
	}
	p.capped = capped
	m.mu.Lock()
	m.proc = p
	m.lastErr = ""
	m.lastUsed = time.Now()
	m.mu.Unlock()
	go m.watch(p)
	how := "uncapped"
	if capped {
		how = "capped"
		if m.cfg.Cap.CPU != "" {
			how += " CPUQuota=" + m.cfg.Cap.CPU
		}
		if m.cfg.Cap.Mem != "" {
			how += " MemoryHigh=" + m.cfg.Cap.Mem
		}
	}
	log.Printf("browser: started %s for profile %q (pid %d, %s) on 127.0.0.1:%d", bin, m.profileID(), p.pid, how, p.port)
	return p, nil
}

func (m *browserManager) setErr(s string) {
	m.mu.Lock()
	m.lastErr = s
	m.mu.Unlock()
}

// launch starts one Chromium and waits for it to publish its debugging port.
func (m *browserManager) launch(bin string, args []string, c browserCap) (*browserProc, error) {
	portFile := filepath.Join(m.profileDir(), "DevToolsActivePort")
	// A leftover from the previous run would be read as this one's port, which
	// belongs to a process that no longer exists (or, worse, to something else).
	_ = os.Remove(portFile)
	// Chromium restores the previous run's tabs from the profile on its own, on
	// top of the about:blank it is launched with — so every launch left one more
	// blank tab behind, and a proxy relaunch (which reopens its pages itself)
	// showed each of them twice. What a launch opens is decided here instead:
	// logins and cookies persist in the profile, the tab set does not. An idle
	// stop therefore closes the tabs, which is what "nobody is using it" means.
	_ = os.RemoveAll(filepath.Join(m.profileDir(), "Default", "Sessions"))

	name, argv := browserCommand(bin, args, c)
	cmd := exec.Command(name, argv...)
	tail := &tailBuffer{max: 16 << 10}
	cmd.Stdout, cmd.Stderr = tail, tail
	cmd.Env = browserEnv(os.Environ())
	cmd.SysProcAttr = browserSysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	p := &browserProc{cmd: cmd, bin: bin, pid: cmd.Process.Pid, started: time.Now(), exited: make(chan struct{}), stderr: tail}
	go func() { _ = cmd.Wait(); close(p.exited) }()
	_ = os.WriteFile(m.pidFile(), fmt.Appendf(nil, "%d\n%d\n", p.pid, os.Getpid()), 0o600)

	deadline := time.NewTimer(browserPortWait)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if b, err := os.ReadFile(portFile); err == nil {
			if port, path, err := parseDevToolsActivePort(b); err == nil {
				p.port, p.wsPath = port, path
				return p, nil
			}
			// A half-written file: Chromium writes it in one go, but a read can
			// still land between its create and its write. Poll again.
		}
		select {
		case <-p.exited:
			_ = os.Remove(m.pidFile())
			return nil, launchFailure(bin, tail.String())
		case <-deadline.C:
			m.kill(p)
			return nil, fmt.Errorf("%s did not publish its debugging port within %s%s", filepath.Base(bin), browserPortWait, tailSuffix(tail))
		case <-tick.C:
		}
	}
}

// launchFailure explains a Chromium that died during startup. What it printed
// ends in a stack trace and a register dump, so the tail of it — the obvious
// thing to show — is hex; the line that says what went wrong is the FATAL one
// above the trace.
func launchFailure(bin, stderr string) error {
	why := summarizeChromeStderr(stderr)
	if strings.Contains(why, "No usable sandbox") {
		// Ubuntu 23.10+ lets only binaries with an AppArmor profile create the
		// unprivileged user namespaces Chromium's sandbox is built on, and
		// Playwright's (or any hand-unpacked) build has none. Falling back to
		// --no-sandbox on our own would quietly run every page an agent opens
		// with lasso's full user privileges — a choice for the operator to
		// make, so it is named here rather than made.
		return fmt.Errorf("%s cannot start its sandbox on this machine (on Ubuntu 23.10+ AppArmor blocks the user namespaces it needs for builds without a profile): use a packaged Chromium/Google Chrome, or accept running it unsandboxed with LASSO_BROWSER_ARGS=--no-sandbox", filepath.Base(bin))
	}
	if why == "" {
		return fmt.Errorf("%s exited before it was ready", filepath.Base(bin))
	}
	return fmt.Errorf("%s exited before it was ready: %s", filepath.Base(bin), why)
}

// summarizeChromeStderr picks the useful part of Chromium's startup output: its
// FATAL line when there is one, otherwise the last few lines that are not the
// crash handler's stack frames and registers.
func summarizeChromeStderr(s string) string {
	var keep []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if i := strings.Index(ln, ":FATAL:"); i >= 0 {
			if j := strings.Index(ln[i:], "] "); j >= 0 {
				return clipLine(ln[i+j+2:], 400)
			}
			return clipLine(ln, 400)
		}
		if ln == "" || strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, "[end of stack trace]") ||
			strings.HasPrefix(ln, "Received signal") || strings.Contains(ln, ": 0000") {
			continue
		}
		keep = append(keep, ln)
	}
	if len(keep) > 3 {
		keep = keep[len(keep)-3:]
	}
	return clipLine(strings.Join(keep, " | "), 600)
}

func tailSuffix(t *tailBuffer) string {
	if s := summarizeChromeStderr(t.String()); s != "" {
		return ": " + s
	}
	return ""
}

// browserEnv is lasso's environment minus its own credentials. Chromium has no
// use for them, and every child a page can exploit is one more place they
// would otherwise be readable from (/proc/<pid>/environ of any renderer).
func browserEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "UI_AUTH", "MCP_OAUTH", "LASSO_MCP_TOKEN":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// watch notices a Chromium that exits on its own (a crash, an OOM kill, the
// host's browser reaper) and clears it, so the next /cdp request relaunches
// instead of proxying onto a dead port.
func (m *browserManager) watch(p *browserProc) {
	<-p.exited
	m.mu.Lock()
	if m.proc == p {
		m.proc = nil
		if !p.stopping.Load() {
			m.lastErr = "the browser exited unexpectedly" + tailSuffix(p.stderr)
			log.Printf("browser: pid %d exited unexpectedly", p.pid)
		}
	}
	crashed := !p.stopping.Load()
	m.mu.Unlock()
	if crashed && m.onStop != nil {
		m.onStop("it exited unexpectedly")
	}
	if b, err := os.ReadFile(m.pidFile()); err == nil && strings.HasPrefix(string(b), strconv.Itoa(p.pid)+"\n") {
		_ = os.Remove(m.pidFile())
	}
}

// kill stops one process group: SIGTERM so Chromium flushes its profile, then
// SIGKILL for whatever is still there after the grace.
func (m *browserManager) kill(p *browserProc) {
	p.stopping.Store(true)
	if p.pid <= 1 {
		// kill(-0) is OUR process group and kill(-1) is every process we may
		// signal. Neither is ever a browser.
		return
	}
	_ = syscall.Kill(-p.pid, syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(browserStopGrace):
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		select {
		case <-p.exited:
		case <-time.After(2 * time.Second):
		}
	}
}

// stop stops the browser if it runs.
func (m *browserManager) stop(ctx context.Context, why string) error {
	if err := m.acquire(ctx); err != nil {
		return err
	}
	defer m.release()
	m.stopLocked(why)
	return nil
}

func (m *browserManager) stopLocked(why string) {
	m.mu.Lock()
	p := m.proc
	m.proc = nil
	m.mu.Unlock()
	if p == nil {
		return
	}
	log.Printf("browser: stopping profile %q, pid %d (%s)", m.profileID(), p.pid, why)
	m.kill(p)
	_ = os.Remove(m.pidFile())
	if m.onStop != nil {
		m.onStop(why)
	}
}

// reclaimProfile deals with a Chromium the pid file says is holding the
// profile. Chromium's own SingletonLock would otherwise make the new launch
// hand its URL to the old process and exit — reading as a launch that died.
//
// Two cases, told apart by the lasso pid recorded beside Chromium's. A browser
// whose lasso is gone is an orphan (non-linux has no Pdeathsig, and a SIGKILL
// can race it) and is stopped. One whose lasso is still alive belongs to a
// second instance sharing this data dir — a dev build next to prod — and
// killing it would pull a live browser out from under that instance's agents,
// so the launch is refused instead, naming the fix.
func (m *browserManager) reclaimProfile() error {
	b, err := os.ReadFile(m.pidFile())
	if err != nil {
		return nil
	}
	lines := strings.Fields(string(b))
	if len(lines) == 0 {
		return nil
	}
	pid, err := strconv.Atoi(lines[0])
	if err != nil || pid <= 1 || !pidAlive(pid) || !strings.Contains(pidCmdline(pid), m.profileDir()) {
		_ = os.Remove(m.pidFile())
		return nil
	}
	if len(lines) > 1 {
		if owner, err := strconv.Atoi(lines[1]); err == nil && owner != os.Getpid() && owner > 1 &&
			pidAlive(owner) && strings.Contains(pidCmdline(owner), "lasso") {
			return fmt.Errorf("the browser profile %s is in use by another lasso (pid %d); give a second instance its own LASSO_DIR", m.profileDir(), owner)
		}
	}
	log.Printf("browser: stopping a stale Chromium (pid %d) left holding %s", pid, m.profileDir())
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(browserStopGrace)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if pidAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		time.Sleep(200 * time.Millisecond)
	}
	_ = os.Remove(m.pidFile())
	return nil
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// pidCmdline is a process's command line as one string, "" when unreadable.
// /proc where there is one; `ps` on a Mac, which has none.
func pidCmdline(pid int) string {
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		return strings.ReplaceAll(string(b), "\x00", " ")
	} else if _, statErr := os.Stat("/proc/self"); statErr == nil {
		return ""
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// run is the idle reaper, and the shutdown path: it stops the browser when ctx
// ends (lasso exiting), which is the same ctx ttyd's children hang off.
func (m *browserManager) run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case <-t.C:
			m.reapIdle()
		}
	}
}

// shutdown stops the browser without waiting on a launch in flight longer than
// the launch itself can take.
func (m *browserManager) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), browserPortWait+browserStopGrace)
	defer cancel()
	_ = m.stop(ctx, "lasso shutting down")
}

func (m *browserManager) reapIdle() {
	if m.cfg.Idle <= 0 || m.inflight.Load() > 0 {
		return
	}
	m.mu.Lock()
	idle := m.proc != nil && time.Since(m.lastUsed) >= m.cfg.Idle
	m.mu.Unlock()
	if !idle {
		return
	}
	// Re-checked under sem: a client that connected between the look and the
	// lock must win over a stop decided a moment ago.
	if m.acquire(context.Background()) != nil {
		return
	}
	defer m.release()
	m.mu.Lock()
	still := m.proc != nil && time.Since(m.lastUsed) >= m.cfg.Idle
	m.mu.Unlock()
	if still && m.inflight.Load() == 0 {
		m.stopLocked(fmt.Sprintf("idle for %s", m.cfg.Idle))
	}
}

// ---------------------------------------------------------------------------
// talking to the running browser's HTTP endpoints
// ---------------------------------------------------------------------------

// browserPage is one page target, as the status and the MCP tool report it.
type browserPage struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

var browserHTTP = &http.Client{Timeout: 5 * time.Second}

func devtoolsDo(p *browserProc, method, path string, out any) error {
	req, err := http.NewRequest(method, "http://"+p.target()+path, nil)
	if err != nil {
		return err
	}
	resp, err := browserHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// browserPages lists the page targets (not workers, iframes or extensions).
func browserPages(p *browserProc) ([]browserPage, error) {
	var ts []struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := devtoolsDo(p, http.MethodGet, "/json/list", &ts); err != nil {
		return nil, err
	}
	pages := []browserPage{}
	for _, t := range ts {
		if t.Type == "page" {
			pages = append(pages, browserPage{ID: t.ID, URL: t.URL, Title: t.Title})
		}
	}
	return pages, nil
}

// relaunch restarts the browser (under a new proxy) and reopens the pages it
// had. CDP targets do not survive a process, so the best that can be kept is
// the URLs; an agent mid-session reconnects to /cdp — which is stable across the
// relaunch by design — and finds its pages back, if not its page ids.
func (m *browserManager) relaunch(ctx context.Context, why string) error {
	if err := m.acquire(ctx); err != nil {
		return err
	}
	defer m.release()
	old := m.current()
	if old == nil {
		return nil
	}
	var urls []string
	if pages, err := browserPages(old); err == nil {
		for _, pg := range pages {
			if pg.URL != "" && pg.URL != "about:blank" {
				urls = append(urls, pg.URL)
			}
		}
	}
	m.stopLocked(why)
	p, err := m.startLocked()
	if err != nil {
		return err
	}
	// The launch's own tab(s), by id, taken BEFORE reopening: matching on
	// about:blank afterwards would also catch a reopened tab that has not
	// committed its navigation yet, and close the page being restored.
	var launchTabs []string
	if len(urls) > 0 {
		if pages, err := browserPages(p); err == nil {
			for _, pg := range pages {
				launchTabs = append(launchTabs, pg.ID)
			}
		}
	}
	reopened := 0
	for _, u := range urls {
		// PUT: GET on /json/new has been refused as an unsafe verb since
		// Chromium 111. The query is the URL itself, unescaped by Chromium.
		if err := devtoolsDo(p, http.MethodPut, "/json/new?"+url.PathEscape(u), nil); err != nil {
			log.Printf("browser: reopen %s after relaunch: %v", u, err)
			continue
		}
		reopened++
	}
	if reopened > 0 {
		// The launch's own about:blank would otherwise sit first in every tab
		// strip as a page nobody opened.
		for _, id := range launchTabs {
			_ = devtoolsDo(p, http.MethodGet, "/json/close/"+id, nil)
		}
	}
	note := fmt.Sprintf("relaunched (%s); reopened %d of %d page(s)", why, reopened, len(urls))
	m.mu.Lock()
	m.note = note
	m.mu.Unlock()
	log.Printf("browser: %s", note)
	return nil
}

// ---------------------------------------------------------------------------
// HTTP API: /api/browser, /api/browser/proxy
// ---------------------------------------------------------------------------

type browserStatus struct {
	Available   bool   `json:"available"`
	Running     bool   `json:"running"`
	Binary      string `json:"binary"`
	Reason      string `json:"reason"`
	Proxy       string `json:"proxy"`
	IdleMinutes int    `json:"idle_minutes"`
	StartedAt   string `json:"started_at"`
	Capped      bool   `json:"capped"`
	// CPUQuota / MemoryHigh in force: the running browser's when it is capped,
	// otherwise what the next launch will get ("" = that limit is off, or no
	// systemd user scope is available here so nothing will be capped).
	CPUQuota string        `json:"cpu_quota"`
	MemHigh  string        `json:"mem_high"`
	Pages    []browserPage `json:"pages"`
	WSPath   string        `json:"ws_path"`
	// Note reports the last relaunch (a proxy change reopens the pages it had).
	Note string `json:"note,omitempty"`
	// The /browser-mcp bridge (browsermcp.go): whether it can serve a session
	// (chrome-devtools-mcp found and not switched off), which binary, why not,
	// and how many sessions — one child each — are live right now.
	MCPAvailable bool   `json:"mcp_available"`
	MCPBinary    string `json:"mcp_binary"`
	MCPReason    string `json:"mcp_reason"`
	MCPSessions  int    `json:"mcp_sessions"`
	// Profiles is every browser profile, the default first (browserprofiles.go).
	// The fields above describe the default profile, as they always have.
	Profiles []browserProfileStatus `json:"profiles"`
}

// browserProfileStatus is one profile's browser, as /api/browser and the
// profile MCP tools report it.
type browserProfileStatus struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Proxy     string        `json:"proxy"`
	Default   bool          `json:"default"`
	Running   bool          `json:"running"`
	StartedAt string        `json:"started_at"`
	Capped    bool          `json:"capped"`
	Reason    string        `json:"reason"` // the last launch's failure, while stopped
	Note      string        `json:"note,omitempty"`
	Pages     []browserPage `json:"pages"`
	WSPath    string        `json:"ws_path"`  // /cdp, or /cdp/p/<id>
	MCPPath   string        `json:"mcp_path"` // /browser-mcp for every profile (its tools take profile)
}

// profileStatus is this manager's slice of the status. It lists the pages of a
// running browser, which is one loopback round trip.
func (m *browserManager) profileStatus() browserProfileStatus {
	id := m.profileID()
	st := browserProfileStatus{
		ID: id, Default: id == defaultBrowserProfile, Pages: []browserPage{},
		WSPath: cdpPathFor(id), MCPPath: browserMCPPathFor(id),
	}
	st.Proxy = m.proxy()
	m.mu.Lock()
	p, lastErr, note := m.proc, m.lastErr, m.note
	m.mu.Unlock()
	st.Note = note
	if p == nil {
		st.Reason = lastErr
		return st
	}
	st.Running = true
	st.StartedAt = p.started.UTC().Format(time.RFC3339)
	st.Capped = p.capped
	if pages, err := browserPages(p); err == nil {
		st.Pages = pages
	}
	return st
}

func (m *browserManager) status() browserStatus {
	st := browserStatus{Pages: []browserPage{}, WSPath: "/cdp", Profiles: []browserProfileStatus{}}
	st.MCPBinary, st.MCPReason, st.MCPAvailable = browserMCP.resolve()
	st.MCPSessions = browserMCP.sessions()
	if m == nil {
		st.Reason = "the shared browser is not configured"
		return st
	}
	bin, reason, ok := resolveBrowserBinary(m.search())
	st.Available, st.Binary = ok, bin
	st.Proxy = m.proxy()
	// Rounded up, so a sub-minute idle (LASSO_BROWSER_IDLE=40s) does not read as
	// 0 — which is what "never" looks like.
	st.IdleMinutes = int((m.cfg.Idle + time.Minute - 1) / time.Minute)
	m.mu.Lock()
	p, lastErr, note := m.proc, m.lastErr, m.note
	m.mu.Unlock()
	st.Note = note
	switch {
	case !ok:
		st.Reason = reason
	case p == nil:
		st.Reason = lastErr
	}
	if p == nil && !m.cfg.Cap.empty() && canCapBrowser() {
		st.CPUQuota, st.MemHigh = m.cfg.Cap.CPU, m.cfg.Cap.Mem
	}
	if p != nil {
		st.Running = true
		if p.capped {
			st.CPUQuota, st.MemHigh = m.cfg.Cap.CPU, m.cfg.Cap.Mem
		}
		st.StartedAt = p.started.UTC().Format(time.RFC3339)
		st.Capped = p.capped
		st.Binary = p.bin // what is running, even if PATH has changed since
		if pages, err := browserPages(p); err == nil {
			st.Pages = pages
		}
	}
	return st
}

func (m *browserManager) serveStatus(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, m.status())
	case http.MethodPost:
		var body struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if m == nil {
			http.Error(w, "the shared browser is not configured", http.StatusServiceUnavailable)
			return
		}
		var err error
		switch body.Action {
		case "start":
			_, err = m.ensure(r.Context())
		case "stop":
			err = m.stop(r.Context(), "stopped from the API")
		case "restart":
			if err = m.stop(r.Context(), "restart requested"); err == nil {
				_, err = m.ensure(r.Context())
			}
		default:
			http.Error(w, `action must be "start", "stop" or "restart"`, http.StatusBadRequest)
			return
		}
		st := m.status()
		if err != nil {
			if st.Reason == "" {
				st.Reason = err.Error()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(st)
			return
		}
		writeJSON(w, st)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (m *browserManager) serveProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if m == nil {
		http.Error(w, "the shared browser is not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Proxy string `json:"proxy"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	m.writeProxyResult(w, m.applyProxy(r.Context(), body.Proxy), m.status)
}

// errBadProxy marks an applyProxy failure that is the caller's input, which
// the HTTP handlers answer with 400 and the validator's sentence.
type errBadProxy struct{ error }

// applyProxy validates, stores and (when the browser runs) applies a proxy.
// --proxy-server is read once, at launch, so a running browser only takes a
// new proxy by being relaunched, which reopens the pages it had.
func (m *browserManager) applyProxy(ctx context.Context, raw string) error {
	proxy, err := validateBrowserProxy(raw)
	if err != nil {
		return errBadProxy{err}
	}
	prev := m.proxy()
	save := m.saveProxy
	if save == nil {
		save = func(v string) error { return setSetting(browserProxySetting, v) }
	}
	if err := save(proxy); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	if proxy != prev && m.current() != nil {
		return m.relaunch(ctx, "proxy changed")
	}
	return nil
}

// writeProxyResult answers a proxy change: 400 for a proxy that did not
// validate, 500 for one that could not be stored, 502 (with the status, whose
// reason says why) for one stored but not applied, and the status otherwise.
func (m *browserManager) writeProxyResult(w http.ResponseWriter, err error, status func() browserStatus) {
	var bad errBadProxy
	switch {
	case err == nil:
		writeJSON(w, status())
	case errors.As(err, &bad):
		http.Error(w, bad.Error(), http.StatusBadRequest)
	case strings.HasPrefix(err.Error(), "save: "):
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		st := status()
		if st.Reason == "" {
			st.Reason = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(st)
	}
}
