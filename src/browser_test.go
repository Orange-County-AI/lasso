package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestValidateBrowserProxy(t *testing.T) {
	ok := map[string]string{
		"":                        "",
		"  ":                      "",
		"socks5://host:1080":      "socks5://host:1080",
		"SOCKS5://Host:1080/":     "socks5://Host:1080",
		"socks4://10.0.0.1:1080":  "socks4://10.0.0.1:1080",
		"http://proxy.lan:3128":   "http://proxy.lan:3128",
		"https://proxy.lan":       "https://proxy.lan",
		"socks5://[::1]:1080":     "socks5://[::1]:1080",
		" http://proxy.lan:8080 ": "http://proxy.lan:8080",
	}
	for in, want := range ok {
		got, err := validateBrowserProxy(in)
		if err != nil || got != want {
			t.Errorf("validateBrowserProxy(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := map[string]string{
		"socks5h://host:1080":         "unsupported proxy scheme",
		"ftp://host":                  "unsupported proxy scheme",
		"host:1080":                   "",
		"proxy.lan":                   "",
		"socks5://user:pw@host:1080":  "credentials",
		"http://user@host:3128":       "credentials",
		"socks5://:1080":              "no host",
		"http://host:3128/path":       "no path",
		"http://host:3128?x=1":        "no path",
		"socks5://host:99999":         "",
		"http://host:3128#frag":       "no path",
		"socks5:host":                 "",
		"http://host:3128/?":          "no path",
		"socks5://host:1080/wpad.dat": "no path",
	}
	for in, want := range bad {
		if _, err := validateBrowserProxy(in); err == nil {
			t.Errorf("validateBrowserProxy(%q) accepted", in)
		} else if want != "" && !strings.Contains(err.Error(), want) {
			t.Errorf("validateBrowserProxy(%q) = %v; want it to mention %q", in, err, want)
		}
	}
}

func TestBrowserArgs(t *testing.T) {
	got := browserArgs("/d/browser-profile", "", false, "", "")
	want := []string{"--headless=new", "--remote-debugging-port=0", "--user-data-dir=/d/browser-profile",
		"--no-first-run", "--no-default-browser-check", "--window-size=1280,800", "about:blank"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plain args = %q", got)
	}
	if s := browserArgs("/p", "", false, "2", ""); !slices.Contains(s, "--force-device-scale-factor=2") {
		t.Fatalf("scale 2 not passed: %q", s)
	}
	if s := browserArgs("/p", "", false, "1", ""); slices.ContainsFunc(s, func(a string) bool { return strings.HasPrefix(a, "--force-device-scale-factor") }) {
		t.Fatalf("scale 1 must leave the default: %q", s)
	}
	for in, want := range map[string]string{"2": "2", "1.5": "1.5", "": "", "0": "", "-1": "", "abc": "", "9": ""} {
		if got := validBrowserScale(in); got != want {
			t.Errorf("validBrowserScale(%q) = %q, want %q", in, got, want)
		}
	}
	got = browserArgs("/p", "socks5://h:1080", true, "", "  --lang=en-US   --disable-gpu ")
	tail := got[len(got)-5:]
	wantTail := []string{"--proxy-server=socks5://h:1080", "--no-sandbox", "--lang=en-US", "--disable-gpu", "about:blank"}
	if !reflect.DeepEqual(tail, wantTail) {
		t.Fatalf("args tail = %q, want %q", tail, wantTail)
	}
	if got[len(got)-1] != "about:blank" {
		t.Fatal("about:blank must be last so extra args cannot be read as URLs after it")
	}
}

func TestBrowserCommandWrapping(t *testing.T) {
	name, argv := browserCommand("/bin/chrome", []string{"--a"}, browserCap{})
	if name != "/bin/chrome" || !reflect.DeepEqual(argv, []string{"--a"}) {
		t.Fatalf("uncapped = %s %q", name, argv)
	}
	name, argv = browserCommand("/bin/chrome", []string{"--a"}, browserCap{CPU: "150%", Mem: "1536M"})
	want := []string{"--user", "--scope", "--quiet", "--collect", "-p", "CPUQuota=150%", "-p", "MemoryHigh=1536M", "--", "env", "DBUS_SESSION_BUS_ADDRESS=disabled:", "/bin/chrome", "--a"}
	if name != "systemd-run" || !reflect.DeepEqual(argv, want) {
		t.Fatalf("capped = %s %q", name, argv)
	}
	_, argv = browserCommand("/bin/chrome", nil, browserCap{Mem: "1G"})
	if strings.Contains(strings.Join(argv, " "), "CPUQuota") || !strings.Contains(strings.Join(argv, " "), "MemoryHigh=1G") {
		t.Fatalf("mem-only cap = %q", argv)
	}
}

// fakeSearch builds a browserSearch over an in-memory file set and PATH.
func fakeSearch(explicit, goos string, onPath map[string]string, files ...string) browserSearch {
	set := map[string]bool{}
	for _, f := range files {
		set[f] = true
	}
	return browserSearch{
		Explicit: explicit,
		LookPath: func(n string) (string, error) {
			if p, ok := onPath[n]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		Glob: func(pattern string) ([]string, error) {
			var out []string
			for f := range set {
				if ok, _ := path.Match(pattern, f); ok { // * never crosses a /
					out = append(out, f)
				}
			}
			return out, nil
		},
		Exists: func(p string) bool { return set[p] },
		Home:   "/home/u",
		GOOS:   goos,
	}
}

func TestResolveBrowserBinaryOrder(t *testing.T) {
	pw := func(n string) string { return "/home/u/.cache/ms-playwright/chromium-" + n + "/chrome-linux64/chrome" }

	// "off" wins over everything, and says it is a choice rather than a gap.
	if _, reason, ok := resolveBrowserBinary(fakeSearch("off", "linux", map[string]string{"chromium": "/usr/bin/chromium"})); ok || !strings.Contains(reason, "disabled") {
		t.Fatalf("off: ok=%v reason=%q", ok, reason)
	}
	// An explicit path is used as-is; a missing one is reported, not replaced.
	if p, _, ok := resolveBrowserBinary(fakeSearch("/opt/c/chrome", "linux", nil, "/opt/c/chrome")); !ok || p != "/opt/c/chrome" {
		t.Fatalf("explicit path = %q %v", p, ok)
	}
	if _, reason, ok := resolveBrowserBinary(fakeSearch("/opt/missing", "linux", map[string]string{"chromium": "/usr/bin/chromium"})); ok || !strings.Contains(reason, "/opt/missing") {
		t.Fatalf("missing explicit: ok=%v reason=%q", ok, reason)
	}
	// An explicit PATH name goes through LookPath.
	if p, _, ok := resolveBrowserBinary(fakeSearch("brave", "linux", map[string]string{"brave": "/usr/bin/brave"})); !ok || p != "/usr/bin/brave" {
		t.Fatalf("explicit name = %q %v", p, ok)
	}
	// PATH names in order: chromium before google-chrome.
	s := fakeSearch("", "linux", map[string]string{"google-chrome": "/usr/bin/google-chrome", "chromium-browser": "/usr/bin/chromium-browser"}, pw("1243"))
	if p, _, _ := resolveBrowserBinary(s); p != "/usr/bin/chromium-browser" {
		t.Fatalf("PATH order: got %q", p)
	}
	// Then Playwright, newest build by NUMBER (999 sorts above 1243 as text).
	s = fakeSearch("", "linux", nil, pw("999"), pw("1243"), pw("1100"), "/Applications/Chromium.app/Contents/MacOS/Chromium")
	if p, _, _ := resolveBrowserBinary(s); p != pw("1243") {
		t.Fatalf("playwright newest: got %q", p)
	}
	// macOS bundles last, and only on darwin.
	mac := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	if p, _, ok := resolveBrowserBinary(fakeSearch("", "darwin", nil, mac)); !ok || p != mac {
		t.Fatalf("darwin bundle = %q %v", p, ok)
	}
	if _, _, ok := resolveBrowserBinary(fakeSearch("", "linux", nil, mac)); ok {
		t.Fatal("a macOS bundle path must not resolve on linux")
	}
	// Nothing at all: a reason that names the remedy.
	if _, reason, ok := resolveBrowserBinary(fakeSearch("", "linux", nil)); ok || !strings.Contains(reason, "LASSO_BROWSER") {
		t.Fatalf("none: ok=%v reason=%q", ok, reason)
	}
}

func TestParseDevToolsActivePort(t *testing.T) {
	port, path, err := parseDevToolsActivePort([]byte("40123\n/devtools/browser/7f0e-aa\n"))
	if err != nil || port != 40123 || path != "/devtools/browser/7f0e-aa" {
		t.Fatalf("= %d %q %v", port, path, err)
	}
	for _, bad := range []string{"", "40123\n", "x\n/devtools/browser/a", "40123\n/json"} {
		if _, _, err := parseDevToolsActivePort([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestCDPUpstreamPath(t *testing.T) {
	const bp = "/devtools/browser/abc"
	cases := map[string]string{
		"/cdp":                       bp,
		"/cdp/":                      bp,
		"/cdp/devtools/page/P1":      "/devtools/page/P1",
		"/cdp/devtools/browser/abc":  "/devtools/browser/abc",
		"/cdp/json":                  "/json",
		"/cdp/json/list":             "/json/list",
		"/cdp/json/version":          "/json/version",
		"/cdp/json/new":              "/json/new",
		"/cdp/json/close/P1":         "/json/close/P1",
		"/cdp/json/activate/P1":      "/json/activate/P1",
		"/cdp/json/protocol":         "/json/protocol",
		"/cdp/devtools/inspector.js": "/devtools/inspector.js",
	}
	for in, want := range cases {
		if got, ok := cdpUpstreamPath(in, bp); !ok || got != want {
			t.Errorf("cdpUpstreamPath(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"/cdpx", "/cdp/other", "/cdp/jsonx", "/cdp/devtoolsx"} {
		if _, ok := cdpUpstreamPath(in, bp); ok {
			t.Errorf("cdpUpstreamPath(%q) accepted", in)
		}
	}
}

func TestRewriteCDPJSON(t *testing.T) {
	list := `[{"id":"P1","type":"page","url":"https://x/","webSocketDebuggerUrl":"ws://127.0.0.1:40123/devtools/page/P1",
	  "devtoolsFrontendUrl":"/devtools/inspector.html?ws=127.0.0.1:40123/devtools/page/P1"},
	 {"id":"P2","type":"page","devtoolsFrontendUrl":"https://chrome-devtools-frontend.appspot.com/serve_rev/@abc/inspector.html?ws=127.0.0.1:40123/devtools/page/P2"},
	 {"id":"P3","type":"page","devtoolsFrontendUrl":"devtools://weird"}]`
	out, err := rewriteCDPJSON([]byte(list), "wss://lasso.example.com")
	if err != nil {
		t.Fatal(err)
	}
	var ts []map[string]any
	if err := json.Unmarshal(out, &ts); err != nil {
		t.Fatal(err)
	}
	if got := ts[0]["webSocketDebuggerUrl"]; got != "wss://lasso.example.com/cdp/devtools/page/P1" {
		t.Errorf("ws url = %v", got)
	}
	fe, _ := ts[0]["devtoolsFrontendUrl"].(string)
	u, _ := url.Parse(fe)
	if u.Path != "/cdp/devtools/inspector.html" || u.Query().Get("wss") != "lasso.example.com/cdp/devtools/page/P1" || u.Query().Has("ws") {
		t.Errorf("relative frontend url = %q", fe)
	}
	fe2, _ := ts[1]["devtoolsFrontendUrl"].(string)
	u2, _ := url.Parse(fe2)
	if u2.Host != "chrome-devtools-frontend.appspot.com" || u2.Query().Get("wss") != "lasso.example.com/cdp/devtools/page/P2" {
		t.Errorf("absolute frontend url = %q", fe2)
	}
	if _, has := ts[2]["devtoolsFrontendUrl"]; has {
		t.Error("an unrecognized frontend url must be dropped, not passed through")
	}
	if strings.Contains(string(out), "127.0.0.1") {
		t.Errorf("loopback port leaked through: %s", out)
	}

	// /json/version: the browser target maps to the stable bare /cdp.
	ver := `{"Browser":"Chrome/140","webSocketDebuggerUrl":"ws://127.0.0.1:40123/devtools/browser/abc-123"}`
	out, err = rewriteCDPJSON([]byte(ver), "ws://100.64.0.5:8190")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	_ = json.Unmarshal(out, &v)
	if v["webSocketDebuggerUrl"] != "ws://100.64.0.5:8190/cdp" || v["Browser"] != "Chrome/140" {
		t.Errorf("version = %v", v)
	}
}

func TestCDPWSBase(t *testing.T) {
	r := httptest.NewRequest("GET", "http://lasso.lan:8190/cdp/json", nil)
	if got := cdpWSBase(r); got != "ws://lasso.lan:8190" {
		t.Errorf("plain = %q", got)
	}
	// A tailnet address over plain http stays ws:// — unlike externalBaseURL,
	// non-loopback does not imply TLS here.
	r = httptest.NewRequest("GET", "http://100.86.22.100:8190/cdp/json", nil)
	if got := cdpWSBase(r); got != "ws://100.86.22.100:8190" {
		t.Errorf("tailnet = %q", got)
	}
	r = httptest.NewRequest("GET", "http://127.0.0.1:8190/cdp/json", nil)
	r.Host = "lasso.example.com"
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := cdpWSBase(r); got != "wss://lasso.example.com" {
		t.Errorf("forwarded https = %q", got)
	}
	r = httptest.NewRequest("GET", "https://lasso.example.com/cdp/json", nil)
	if got := cdpWSBase(r); got != "wss://lasso.example.com" {
		t.Errorf("tls = %q", got)
	}
}

func TestCDPOriginGuard(t *testing.T) {
	cases := []struct {
		host, origin, xfh string
		want              bool
	}{
		{"lasso.lan:8190", "", "", true},                                           // CLI / agent
		{"lasso.lan:8190", "http://lasso.lan:8190", "", true},                      // lasso's own page
		{"LASSO.lan:8190", "http://lasso.LAN:8190", "", true},                      // case-insensitive
		{"lasso.example.com", "https://lasso.example.com", "", true},               // default port omitted
		{"lasso.example.com:443", "https://lasso.example.com", "", true},           // default port explicit
		{"127.0.0.1:8190", "https://lasso.example.com", "lasso.example.com", true}, // behind a proxy
		{"lasso.lan:8190", "https://evil.example", "", false},
		{"lasso.lan:8190", "http://lasso.lan:9999", "", false}, // another port is another origin
		{"lasso.lan:8190", "null", "", false},
		{"127.0.0.1:8190", "https://evil.example", "lasso.example.com", false},
		{"localhost:8190", "http://localhost.evil.example:8190", "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "http://"+c.host+"/cdp", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.xfh != "" {
			r.Header.Set("X-Forwarded-Host", c.xfh)
		}
		if got := cdpOriginAllowed(r); got != c.want {
			t.Errorf("host=%q origin=%q xfh=%q: allowed=%v want %v", c.host, c.origin, c.xfh, got, c.want)
		}
	}
}

// fakeChromium stands in for Chromium's debugging endpoint: it records what the
// proxy sent it, serves /json, and answers a websocket upgrade by echoing.
type fakeChromium struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []*http.Request
}

func newFakeChromium(t *testing.T) *fakeChromium {
	t.Helper()
	f := &fakeChromium{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Clone(context.Background()))
		f.mu.Unlock()
		self := r.Host
		switch {
		case r.URL.Path == "/json/version":
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			fmt.Fprintf(w, `{"Browser":"HeadlessChrome/140","webSocketDebuggerUrl":"ws://%s/devtools/browser/abc"}`, self)
		case r.URL.Path == "/json/list" || r.URL.Path == "/json":
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			fmt.Fprintf(w, `[{"id":"P1","type":"page","title":"Ex","url":"https://example.com/","webSocketDebuggerUrl":"ws://%s/devtools/page/P1"},
				{"id":"W1","type":"service_worker","url":"https://example.com/sw.js"}]`, self)
		case strings.HasPrefix(r.URL.Path, "/devtools/") && strings.EqualFold(r.Header.Get("Upgrade"), "websocket"):
			hj, _ := w.(http.Hijacker)
			conn, buf, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = buf.Flush()
			_, _ = io.Copy(conn, buf) // echo until the client hangs up
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeChromium) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		return nil
	}
	return f.seen[len(f.seen)-1]
}

// testBrowserManager is a manager whose "running" browser is the fake, so the
// proxy can be exercised without ever launching Chromium.
func testBrowserManager(t *testing.T, f *fakeChromium) *browserManager {
	t.Helper()
	m := &browserManager{
		cfg: browserConfig{Dir: t.TempDir(), Idle: 15 * time.Minute},
		search: func() browserSearch {
			return browserSearch{Explicit: "/fake/chrome", Exists: func(string) bool { return true }}
		},
		proxy: func() string { return "" },
		sem:   make(chan struct{}, 1),
	}
	if f != nil {
		u, _ := url.Parse(f.srv.URL)
		port, _ := strconv.Atoi(u.Port())
		m.proc = &browserProc{bin: "/fake/chrome", port: port, wsPath: "/devtools/browser/abc",
			started: time.Now(), exited: make(chan struct{})}
	}
	return m
}

func TestCDPProxyPassthroughAndRewrite(t *testing.T) {
	f := newFakeChromium(t)
	m := testBrowserManager(t, f)
	lasso := httptest.NewServer(http.HandlerFunc(m.serveCDP))
	defer lasso.Close()
	lu, _ := url.Parse(lasso.URL)

	req, _ := http.NewRequest("GET", lasso.URL+"/cdp/json/version", nil)
	req.Header.Set("Origin", lasso.URL) // lasso's own page
	req.Header.Set("Authorization", "Basic c2VjcmV0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	if want := "ws://" + lu.Host + "/cdp"; v["webSocketDebuggerUrl"] != want {
		t.Errorf("webSocketDebuggerUrl = %v, want %s", v["webSocketDebuggerUrl"], want)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("Content-Length %d for a %d-byte rewritten body", resp.ContentLength, len(body))
	}
	up := f.last()
	fu, _ := url.Parse(f.srv.URL)
	if up.Host != fu.Host {
		t.Errorf("upstream Host = %q, want the loopback IP:port %q", up.Host, fu.Host)
	}
	if up.Header.Get("Origin") != "" || up.Header.Get("Authorization") != "" {
		t.Errorf("Origin/Authorization reached Chromium: %v", up.Header)
	}

	// /json/list keeps the target list and repoints each target through /cdp.
	resp, err = http.Get(lasso.URL + "/cdp/json/list")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "ws://"+lu.Host+"/cdp/devtools/page/P1") || strings.Contains(string(body), fu.Host) {
		t.Errorf("list not rewritten: %s", body)
	}

	// A foreign Origin never reaches Chromium.
	before := len(f.seen)
	req, _ = http.NewRequest("GET", lasso.URL+"/cdp/json/list", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign origin: status %d, want 403", resp.StatusCode)
	}
	if len(f.seen) != before {
		t.Error("a foreign-origin request was forwarded")
	}

	// Unknown subpath: 404, not a passthrough.
	resp, _ = http.Get(lasso.URL + "/cdp/other")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/cdp/other: %d", resp.StatusCode)
	}
}

// The websocket path: bare /cdp upgrades onto the browser target, and bytes
// flow both ways through the hijacked connection.
func TestCDPProxyWebsocketUpgrade(t *testing.T) {
	f := newFakeChromium(t)
	m := testBrowserManager(t, f)
	lasso := httptest.NewServer(http.HandlerFunc(m.serveCDP))
	defer lasso.Close()
	lu, _ := url.Parse(lasso.URL)

	conn, err := net.Dial("tcp", lu.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /cdp HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", lu.Host, lu.Host)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101", resp.StatusCode)
	}
	if n := m.inflight.Load(); n != 1 {
		t.Errorf("inflight = %d with a live websocket, want 1 (the idle stop must see it)", n)
	}
	up := f.last()
	if up.URL.Path != "/devtools/browser/abc" || up.Header.Get("Origin") != "" {
		t.Errorf("upstream path %q origin %q", up.URL.Path, up.Header.Get("Origin"))
	}
	_, _ = conn.Write([]byte("hello-cdp"))
	got := make([]byte, 9)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "hello-cdp" {
		t.Fatalf("echo = %q %v", got, err)
	}
	conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for m.inflight.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := m.inflight.Load(); n != 0 {
		t.Errorf("inflight = %d after the socket closed", n)
	}
}

func TestCDPUnavailable(t *testing.T) {
	m := testBrowserManager(t, nil)
	m.search = func() browserSearch { return fakeSearch("off", "linux", nil) }
	lasso := httptest.NewServer(http.HandlerFunc(m.serveCDP))
	defer lasso.Close()
	resp, err := http.Get(lasso.URL + "/cdp/json/version")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "disabled") {
		t.Errorf("status %d body %q", resp.StatusCode, body)
	}
	st := m.status()
	if st.Available || st.Running || !strings.Contains(st.Reason, "disabled") || st.WSPath != "/cdp" || st.Pages == nil {
		t.Errorf("status = %+v", st)
	}
}

func TestBrowserStatusListsPagesOnly(t *testing.T) {
	f := newFakeChromium(t)
	m := testBrowserManager(t, f)
	st := m.status()
	if !st.Available || !st.Running || st.Binary != "/fake/chrome" || st.IdleMinutes != 15 || st.StartedAt == "" {
		t.Fatalf("status = %+v", st)
	}
	if len(st.Pages) != 1 || st.Pages[0].ID != "P1" || st.Pages[0].Title != "Ex" {
		t.Errorf("pages = %+v, want only the page target", st.Pages)
	}
}

func TestBrowserProxyEndpointRejectsBadInput(t *testing.T) {
	openTestDB(t)
	m := testBrowserManager(t, nil)
	m.proxy = func() string { v, _ := getSetting(browserProxySetting); return v }
	post := func(body string) (int, string) {
		w := httptest.NewRecorder()
		m.serveProxy(w, httptest.NewRequest("POST", "/api/browser/proxy", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	if code, body := post(`{"proxy":"socks5://u:p@h:1080"}`); code != 400 || !strings.Contains(body, "credentials") {
		t.Errorf("creds: %d %q", code, body)
	}
	if code, _ := post(`{"proxy":"socks5://h:1080"}`); code != 200 {
		t.Errorf("valid: %d", code)
	}
	if v, _ := getSetting(browserProxySetting); v != "socks5://h:1080" {
		t.Errorf("stored %q", v)
	}
	if code, body := post(`{"proxy":""}`); code != 200 || !strings.Contains(body, `"proxy":""`) {
		t.Errorf("clear: %d %q", code, body)
	}
}

func TestCDPAuthGate(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	do := func(h http.Handler, origin, user, pass, bearer string) int {
		r := httptest.NewRequest("GET", "http://lasso.lan:8190/cdp/json", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if user != "" {
			r.SetBasicAuth(user, pass)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	// Open: no UI_AUTH, no MCP_OAUTH — same trust model as /mcp.
	prev := oauthCfg
	oauthCfg = oauthConf{}
	t.Cleanup(func() { oauthCfg = prev })
	if c := do(withCDPAuth(ok, "", "", false), "", "", "", ""); c != 299 {
		t.Errorf("open: %d", c)
	}

	// UI_AUTH: basic credentials required, from a CLI and from lasso's page alike.
	h := withCDPAuth(ok, "u", "p", true)
	if c := do(h, "", "", "", ""); c != 401 {
		t.Errorf("UI_AUTH, none: %d", c)
	}
	if c := do(h, "http://lasso.lan:8190", "", "", ""); c != 401 {
		t.Errorf("UI_AUTH, same-origin without creds: %d", c)
	}
	if c := do(h, "", "u", "wrong", ""); c != 401 {
		t.Errorf("UI_AUTH, wrong: %d", c)
	}
	if c := do(h, "", "u", "p", ""); c != 299 {
		t.Errorf("UI_AUTH, right: %d", c)
	}
}

func TestCDPAuthGateOAuth(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	stubSSHHosts(t, "gigachad")
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	do := func(h http.Handler, origin, bearer string) int {
		r := httptest.NewRequest("GET", "http://lasso.lan:8190/cdp/json", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	h := withCDPAuth(ok, "", "", false)
	if c := do(h, "", ""); c != 401 {
		t.Errorf("no token: %d, want 401", c)
	}
	if c := do(h, "", "garbage"); c != 401 {
		t.Errorf("bad token: %d, want 401", c)
	}
	// lasso's own page, with UI_AUTH unset (the rest of the UI is open too).
	if c := do(h, "http://lasso.lan:8190", ""); c != 299 {
		t.Errorf("same-origin page: %d", c)
	}
	// A foreign page gets nothing from that allowance.
	if c := do(h, "https://evil.example", ""); c == 299 {
		t.Errorf("foreign page passed the gate")
	}
	// A credential confined to another host cannot drive lasso's browser.
	if c := do(h, "", hostClientToken(t, "gigachad", scopeSelf)); c != http.StatusForbidden {
		t.Errorf("self-scoped remote token: %d, want 403", c)
	}
	if c := do(h, "", hostClientToken(t, "local", scopeSelf)); c != 299 {
		t.Errorf("lasso-host token: %d", c)
	}

	// With UI_AUTH also set, a page must present it like everywhere else.
	h = withCDPAuth(ok, "u", "p", true)
	if c := do(h, "http://lasso.lan:8190", ""); c != 401 {
		t.Errorf("UI_AUTH+MCP_OAUTH same-origin without creds: %d", c)
	}
}

func TestSharedBrowserTool(t *testing.T) {
	openTestDB(t)
	f := newFakeChromium(t)
	prev := sharedBrowser
	sharedBrowser = testBrowserManager(t, f)
	t.Cleanup(func() { sharedBrowser = prev })

	srv := httptest.NewServer(withRequestBase(newMCPHandler()))
	defer srv.Close()
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	var out sharedBrowserOut
	if msg := callTool(t, sess, "shared_browser", map[string]any{"start": false}, &out); msg != "" {
		t.Fatal(msg)
	}
	su, _ := url.Parse(srv.URL)
	if out.WSEndpoint != "ws://"+su.Host+"/cdp" || out.HTTPEndpoint != "http://"+su.Host+"/cdp" || out.WSPath != "/cdp" {
		t.Errorf("endpoints = %+v", out)
	}
	if !out.Available || !out.Running || len(out.Pages) != 1 {
		t.Errorf("out = %+v", out)
	}

	// Without the base header (a client that spoofs one is overwritten; one that
	// never came through withRequestBase has none), the note says what to do.
	srv2 := httptest.NewServer(newMCPHandler())
	defer srv2.Close()
	sess2, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv2.URL, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess2.Close()
	out = sharedBrowserOut{}
	if msg := callTool(t, sess2, "shared_browser", map[string]any{"start": false}, &out); msg != "" {
		t.Fatal(msg)
	}
	if out.WSEndpoint != "" || !strings.Contains(out.Note, "ws_path") {
		t.Errorf("no-base out = %+v", out)
	}
}

func TestSharedBrowserToolScope(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	stubSSHHosts(t, "gigachad")
	prev := sharedBrowser
	sharedBrowser = testBrowserManager(t, nil)
	t.Cleanup(func() { sharedBrowser = prev })
	sess := mcpTestSession(t, hostClientToken(t, "gigachad", scopeSelf))
	var out sharedBrowserOut
	msg := callTool(t, sess, "shared_browser", map[string]any{"start": false}, &out)
	if !strings.Contains(msg, "lasso's own machine") {
		t.Errorf("self-scoped remote caller: %q", msg)
	}
}

func TestReclaimProfileClearsAStalePidFile(t *testing.T) {
	m := testBrowserManager(t, nil)
	if err := os.MkdirAll(m.profileDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	// A pid file naming a process that does not hold the profile is stale and
	// cleared, never acted on.
	_ = os.WriteFile(m.pidFile(), []byte(fmt.Sprintf("%d\n%d\n", os.Getpid(), os.Getpid())), 0o600)
	if err := m.reclaimProfile(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.pidFile()); !os.IsNotExist(err) {
		t.Error("stale pid file kept")
	}
}

func TestBrowserEnvDropsCredentials(t *testing.T) {
	got := browserEnv([]string{"HOME=/h", "UI_AUTH=u:p", "MCP_OAUTH=a:b", "LASSO_MCP_TOKEN=t", "PATH=/bin"})
	if !reflect.DeepEqual(got, []string{"HOME=/h", "PATH=/bin"}) {
		t.Errorf("env = %q", got)
	}
}

func TestLaunchFailureNamesTheSandboxRemedy(t *testing.T) {
	stderr := "[1:1:0927/024726.611261:FATAL:content/browser/zygote_host/zygote_host_impl_linux.cc:129] No usable sandbox! If you are running on Ubuntu 23.10+ ...\n" +
		"Received signal 6\n#0 0x56bf2cc11bc3 (chrome+0x6a9abc2)\n  r8: 0000000000000016  r9: 0000000000000001\n[end of stack trace]\n"
	err := launchFailure("/x/chrome", stderr)
	if !strings.Contains(err.Error(), "LASSO_BROWSER_ARGS=--no-sandbox") || strings.Contains(err.Error(), "r8:") {
		t.Errorf("sandbox failure = %v", err)
	}
	err = launchFailure("/x/chrome", "[0927/1:ERROR:foo.cc:1] something odd\nReceived signal 11\n#0 0x1\n  r8: 0000000000000016\n")
	if !strings.Contains(err.Error(), "something odd") || strings.Contains(err.Error(), "0x1") {
		t.Errorf("generic failure = %v", err)
	}
}

func TestBrowserCapKnobs(t *testing.T) {
	for in, want := range map[string]string{"": "", "off": "", " OFF ": "", "200%": "200%", " 2G ": "2G"} {
		if got := capLimit(in); got != want {
			t.Errorf("capLimit(%q) = %q, want %q", in, got, want)
		}
	}
	t.Setenv("LASSO_BROWSER_CPU", "300%")
	if got := envOrDefault("LASSO_BROWSER_CPU", "200%"); got != "300%" {
		t.Errorf("set env = %q", got)
	}
	// Set but empty is a deliberate "off", not "use the default".
	t.Setenv("LASSO_BROWSER_MEM", "")
	if got := capLimit(envOrDefault("LASSO_BROWSER_MEM", "2G")); got != "" {
		t.Errorf("empty env = %q", got)
	}
	if got := envOrDefault("LASSO_BROWSER_UNSET_FOR_TEST", "2G"); got != "2G" {
		t.Errorf("unset env = %q", got)
	}
	t.Setenv("LASSO_BROWSER_IDLE", "30m")
	if got := envDuration("LASSO_BROWSER_IDLE", time.Minute); got != 30*time.Minute {
		t.Errorf("idle = %s", got)
	}
	t.Setenv("LASSO_BROWSER_IDLE", "soon")
	if got := envDuration("LASSO_BROWSER_IDLE", time.Minute); got != time.Minute {
		t.Errorf("bad idle = %s", got)
	}
}

func TestBrowserStatusReportsLimitsInForce(t *testing.T) {
	f := newFakeChromium(t)
	m := testBrowserManager(t, f)
	m.cfg.Cap = browserCap{CPU: "200%", Mem: "2G"}
	if st := m.status(); st.CPUQuota != "" || st.MemHigh != "" {
		t.Errorf("an uncapped running browser reported limits: %+v", st)
	}
	m.proc.capped = true
	if st := m.status(); !st.Capped || st.CPUQuota != "200%" || st.MemHigh != "2G" {
		t.Errorf("capped status = %+v", st)
	}
}
