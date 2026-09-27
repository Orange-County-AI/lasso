package main

import (
	"bytes"
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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeBrowserMCPEnv, when set, makes the test binary a chrome-devtools-mcp
// stand-in (see TestMain): "ok" serves three tools over stdio, "crash" dies on
// startup the way a broken install does.
const fakeBrowserMCPEnv = "LASSO_TEST_FAKE_BROWSER_MCP"

// fakeJPEG is what the fake's snap tool returns: the bytes must survive the
// child → bridge → client round trip exactly (base64 twice over).
var fakeJPEG = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0xff, 0xd9}

func runFakeBrowserMCP(mode string) int {
	if mode == "crash" {
		fmt.Fprintln(os.Stderr, "Error: boom: fake chrome-devtools-mcp failure")
		return 3
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake-chrome-devtools-mcp", Version: "0"}, nil)
	obj := map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "number"}}}
	srv.AddTool(&mcp.Tool{Name: "echo", Description: "echo the arguments and argv", InputSchema: obj,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			b, _ := json.Marshal(map[string]any{"args": req.Params.Arguments, "argv": os.Args[1:]})
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "snap", Description: "a screenshot", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "took a screenshot"},
				&mcp.ImageContent{MIMEType: "image/jpeg", Data: fakeJPEG},
			}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "env", Description: "the environment", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Join(os.Environ(), "\n")}}}, nil
		})
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil && !errors.Is(err, io.EOF) {
		return 1
	}
	return 0
}

// testBrowserMCP is a bridge whose chrome-devtools-mcp is this test binary.
func testBrowserMCP(t *testing.T, mode string, max int) *browserMCPBridge {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b := newBrowserMCPBridge(browserMCPConfig{Binary: exe, Max: max, ExtraEnv: []string{fakeBrowserMCPEnv + "=" + mode}})
	b.setListenAddr(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1})
	t.Cleanup(func() { b.closeAll("test over") })
	return b
}

func browserMCPConnect(t *testing.T, endpoint string) (*mcp.ClientSession, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "0"}, nil)
	return c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, DisableStandaloneSSE: true}, nil)
}

// livePIDs is the child pid of every live session.
func (b *browserMCPBridge) livePIDs() []int {
	var out []int
	for _, s := range b.snapshot() {
		s.mu.Lock()
		out = append(out, s.pid)
		s.mu.Unlock()
	}
	return out
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child pid %d is still running", pid)
}

func waitSessions(t *testing.T, b *browserMCPBridge, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b.sessions() == n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("sessions = %d, want %d", b.sessions(), n)
}

func TestBrowserMCPBridgeMirrorsAndForwards(t *testing.T) {
	t.Setenv("UI_AUTH", "u:secret")
	t.Setenv("MCP_OAUTH", "cid:csecret")
	b := testBrowserMCP(t, "ok", 4)
	srv := httptest.NewServer(b)
	defer srv.Close()

	sess, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	init := sess.InitializeResult()
	if init.ServerInfo.Name != "lasso-browser" || !strings.Contains(init.Instructions, "SHARED browser") {
		t.Errorf("initialize = %+v / %q", init.ServerInfo, init.Instructions)
	}
	if init.Capabilities.Tools == nil {
		t.Errorf("no tools capability advertised")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// tools/list mirrors the child: names, descriptions, schemas, annotations.
	lt, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range lt.Tools {
		byName[tl.Name] = tl
	}
	if len(byName) != 3 || byName["echo"] == nil || byName["snap"] == nil || byName["env"] == nil {
		t.Fatalf("tools = %v", byName)
	}
	if e := byName["echo"]; e.Description != "echo the arguments and argv" || e.Annotations == nil || !e.Annotations.ReadOnlyHint {
		t.Errorf("echo = %+v", e)
	}
	if s, _ := json.Marshal(byName["echo"].InputSchema); !strings.Contains(string(s), `"x"`) {
		t.Errorf("echo schema = %s", s)
	}

	// tools/call forwards the arguments as given, and the child ran with lasso's
	// flags: its own /cdp, the internal token, the screenshot bounds.
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"x": 7}})
	if err != nil || res.IsError {
		t.Fatalf("echo: %v %+v", err, res)
	}
	var echoed struct {
		Args map[string]any `json:"args"`
		Argv []string       `json:"argv"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &echoed); err != nil {
		t.Fatal(err)
	}
	if echoed.Args["x"] != float64(7) {
		t.Errorf("args = %v", echoed.Args)
	}
	argv := strings.Join(echoed.Argv, " ")
	for _, want := range []string{
		"--wsEndpoint ws://127.0.0.1:1/cdp",
		`--wsHeaders {"X-Lasso-Internal":"` + internalCDPToken + `"}`,
		"--no-usage-statistics", "--no-performance-crux",
		"--screenshotFormat jpeg", "--screenshotMaxWidth 1280", "--screenshotMaxHeight 1280",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q lacks %q", argv, want)
		}
	}

	// Image content comes back byte for byte.
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "snap"})
	if err != nil || len(res.Content) != 2 {
		t.Fatalf("snap: %v %+v", err, res)
	}
	img, ok := res.Content[1].(*mcp.ImageContent)
	if !ok || img.MIMEType != "image/jpeg" || !bytes.Equal(img.Data, fakeJPEG) {
		t.Errorf("image = %#v", res.Content[1])
	}

	// lasso's credentials never reach the child.
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "env"})
	if err != nil {
		t.Fatal(err)
	}
	env := res.Content[0].(*mcp.TextContent).Text
	if strings.Contains(env, "UI_AUTH") || strings.Contains(env, "MCP_OAUTH") || strings.Contains(env, "secret") {
		t.Errorf("child env leaks credentials:\n%s", env)
	}
	if !strings.Contains(env, "CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1") {
		t.Errorf("child env lacks the telemetry opt-out:\n%s", env)
	}

	// An unknown tool is the child's refusal (or the mirror's), not a hang.
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "nope"}); err == nil {
		t.Errorf("unknown tool succeeded")
	}

	// Closing the session (DELETE) kills the child.
	pids := b.livePIDs()
	if len(pids) != 1 || pids[0] <= 1 {
		t.Fatalf("live pids = %v", pids)
	}
	_ = sess.Close()
	waitSessions(t, b, 0)
	waitGone(t, pids[0])
}

func TestBrowserMCPCap(t *testing.T) {
	b := testBrowserMCP(t, "ok", 1)
	srv := httptest.NewServer(b)
	defer srv.Close()
	first, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := browserMCPConnect(t, srv.URL); err == nil || !strings.Contains(err.Error(), "limit of 1") {
		t.Fatalf("second session over the cap: %v", err)
	}
	if b.sessions() != 1 || b.starting != 0 {
		t.Errorf("after refusal: live=%d starting=%d", b.sessions(), b.starting)
	}
	_ = first.Close()
	waitSessions(t, b, 0)
	third, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatalf("a slot freed by a close is reusable: %v", err)
	}
	_ = third.Close()
}

func postInitialize(t *testing.T, h http.Handler) (int, string) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	r := httptest.NewRequest("POST", "http://lasso.lan:8190/browser-mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func TestBrowserMCPUnavailable(t *testing.T) {
	b := newBrowserMCPBridge(browserMCPConfig{Binary: "/nonexistent/chrome-devtools-mcp"})
	if code, body := postInitialize(t, b); code != 503 || !strings.Contains(body, "does not exist") || !strings.Contains(body, "npm i -g chrome-devtools-mcp") {
		t.Errorf("missing path: %d %q", code, body)
	}
	b = newBrowserMCPBridge(browserMCPConfig{Binary: "off"})
	if code, body := postInitialize(t, b); code != 503 || !strings.Contains(body, "disabled") {
		t.Errorf("off: %d %q", code, body)
	}
	b = newBrowserMCPBridge(browserMCPConfig{})
	b.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if code, body := postInitialize(t, b); code != 503 || !strings.Contains(body, "not installed") || !strings.Contains(body, "mise use -g npm:chrome-devtools-mcp") {
		t.Errorf("not on PATH: %d %q", code, body)
	}
	var nilBridge *browserMCPBridge
	if code, _ := postInitialize(t, nilBridge); code != 503 {
		t.Errorf("unconfigured: %d", code)
	}
}

func TestBrowserMCPChildFailsToStart(t *testing.T) {
	b := testBrowserMCP(t, "crash", 4)
	srv := httptest.NewServer(b)
	defer srv.Close()
	_, err := browserMCPConnect(t, srv.URL)
	if err == nil {
		t.Fatal("a child that dies on startup produced a session")
	}
	for _, want := range []string{"exited during startup", "exit status 3", "boom: fake chrome-devtools-mcp failure"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if b.sessions() != 0 || b.starting != 0 {
		t.Errorf("after a failed start: live=%d starting=%d", b.sessions(), b.starting)
	}
}

// A shared browser that stops (idle, restart, relaunch, crash) takes every
// browser-mcp session with it: their CDP connections are dead.
func TestBrowserMCPClosesWhenTheBrowserStops(t *testing.T) {
	f := newFakeChromium(t)
	m := testBrowserManager(t, f)
	b := testBrowserMCP(t, "ok", 4)
	m.onStop = b.browserStopped
	srv := httptest.NewServer(b)
	defer srv.Close()
	sess, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	pids := b.livePIDs()
	if len(pids) != 1 {
		t.Fatalf("pids = %v", pids)
	}
	if err := m.stop(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	waitSessions(t, b, 0)
	waitGone(t, pids[0])
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "snap"}); err == nil {
		t.Errorf("a call on a session closed by the browser stop succeeded")
	}
}

func TestBrowserMCPAuthGate(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	do := func(h http.Handler, origin, user, pass, bearer string) int {
		r := httptest.NewRequest("POST", "http://lasso.lan:8190/browser-mcp", nil)
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
	prev := oauthCfg
	oauthCfg = oauthConf{}
	t.Cleanup(func() { oauthCfg = prev })

	// Open: neither UI_AUTH nor MCP_OAUTH.
	h := withBrowserMCPAuth(ok, "", "", false)
	if c := do(h, "", "", "", ""); c != 299 {
		t.Errorf("open: %d", c)
	}
	if c := do(h, "https://evil.example", "", "", ""); c != 403 {
		t.Errorf("open, foreign Origin: %d", c)
	}
	if c := do(h, "null", "", "", ""); c != 403 {
		t.Errorf("open, null Origin: %d", c)
	}
	// UI_AUTH only: basic, like /cdp (NOT open like /mcp — this fronts /cdp).
	h = withBrowserMCPAuth(ok, "u", "p", true)
	if c := do(h, "", "", "", ""); c != 401 {
		t.Errorf("UI_AUTH, none: %d", c)
	}
	if c := do(h, "", "u", "wrong", ""); c != 401 {
		t.Errorf("UI_AUTH, wrong: %d", c)
	}
	if c := do(h, "", "u", "p", ""); c != 299 {
		t.Errorf("UI_AUTH, right: %d", c)
	}
	if c := do(h, "https://evil.example", "u", "p", ""); c != 403 {
		t.Errorf("UI_AUTH, foreign Origin with creds: %d", c)
	}
}

func TestBrowserMCPAuthGateOAuth(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	stubSSHHosts(t, "gigachad")
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	do := func(h http.Handler, origin, bearer string) int {
		r := httptest.NewRequest("POST", "http://lasso.lan:8190/browser-mcp", nil)
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
	h := withBrowserMCPAuth(ok, "", "", false)
	if c := do(h, "", ""); c != 401 {
		t.Errorf("no token: %d", c)
	}
	if c := do(h, "", "garbage"); c != 401 {
		t.Errorf("bad token: %d", c)
	}
	// No same-origin allowance: no page of lasso's speaks MCP.
	if c := do(h, "http://lasso.lan:8190", ""); c != 401 {
		t.Errorf("same-origin without token: %d", c)
	}
	if c := do(h, "", hostClientToken(t, "gigachad", scopeSelf)); c != http.StatusForbidden {
		t.Errorf("self-scoped remote token: %d, want 403", c)
	}
	local := hostClientToken(t, "local", scopeSelf)
	if c := do(h, "", local); c != 299 {
		t.Errorf("lasso-host token: %d", c)
	}
	if c := do(h, "https://evil.example", local); c != 403 {
		t.Errorf("foreign Origin with a good token: %d", c)
	}
}

func TestInternalCDPToken(t *testing.T) {
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	cdp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	h := withInternalCDP(outer, cdp)
	do := func(path, tok string) int {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8190"+path, nil)
		if tok != "" {
			r.Header.Set(internalCDPHeader, tok)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if len(internalCDPToken) != 64 {
		t.Fatalf("token length %d", len(internalCDPToken))
	}
	for _, p := range []string{"/cdp", "/cdp/", "/cdp/json/list"} {
		if c := do(p, internalCDPToken); c != 299 {
			t.Errorf("right token on %s: %d", p, c)
		}
	}
	if c := do("/cdp", internalCDPToken[:63]+"x"); c != 403 {
		t.Errorf("wrong token: %d", c)
	}
	if c := do("/cdp", ""); c != 403 {
		t.Errorf("no token: %d", c)
	}
	// The token opens /cdp and nothing else.
	for _, p := range []string{"/api/browser", "/browser-mcp", "/mcp", "/cdpx"} {
		if c := do(p, internalCDPToken); c != 403 {
			t.Errorf("right token on %s: %d", p, c)
		}
	}

	// And Chromium never sees it.
	f := newFakeChromium(t)
	m := testBrowserManager(t, f)
	r := httptest.NewRequest("GET", "http://127.0.0.1:8190/cdp/json/version", nil)
	r.Header.Set(internalCDPHeader, internalCDPToken)
	w := httptest.NewRecorder()
	withInternalCDP(outer, http.HandlerFunc(m.serveCDP)).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("proxied: %d %s", w.Code, w.Body)
	}
	if got := f.last().Header.Get(internalCDPHeader); got != "" {
		t.Errorf("internal token forwarded to Chromium")
	}
}

func TestBrowserMCPEnvAllowlist(t *testing.T) {
	got := browserMCPEnv([]string{
		"PATH=/usr/bin", "HOME=/home/u", "UI_AUTH=u:p", "MCP_OAUTH=a:b", "LASSO_MCP_TOKEN=t",
		"MISE_DATA_DIR=/m", "MISE_GITHUB_TOKEN=ghp_x", "AWS_SECRET_ACCESS_KEY=k", "XDG_CONFIG_HOME=/c",
	})
	j := strings.Join(got, " ")
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/u", "MISE_DATA_DIR=/m", "XDG_CONFIG_HOME=/c", "CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1"} {
		if !strings.Contains(j, want) {
			t.Errorf("env lacks %s: %v", want, got)
		}
	}
	for _, bad := range []string{"UI_AUTH", "MCP_OAUTH", "LASSO_MCP_TOKEN", "MISE_GITHUB_TOKEN", "AWS_SECRET"} {
		if strings.Contains(j, bad) {
			t.Errorf("env keeps %s: %v", bad, got)
		}
	}
	args := browserMCPArgs("ws://127.0.0.1:8190/cdp", "tok", "--slim  --viewport 800x600")
	if args[0] != "--wsEndpoint" || args[len(args)-1] != "800x600" || args[len(args)-3] != "--slim" {
		t.Errorf("args = %v", args)
	}
}

func TestBrowserMCPListenAddr(t *testing.T) {
	b := newBrowserMCPBridge(browserMCPConfig{})
	for _, c := range []struct {
		addr net.Addr
		want string
	}{
		{&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8190}, "ws://127.0.0.1:8190/cdp"},
		{&net.TCPAddr{IP: net.IPv4(100, 86, 22, 100), Port: 8190}, "ws://100.86.22.100:8190/cdp"},
		{&net.TCPAddr{IP: net.IPv4zero, Port: 8190}, "ws://127.0.0.1:8190/cdp"},
		{&net.TCPAddr{IP: net.IPv6unspecified, Port: 8190}, "ws://127.0.0.1:8190/cdp"},
		{&net.TCPAddr{IP: net.IPv6loopback, Port: 8190}, "ws://[::1]:8190/cdp"},
	} {
		b.setListenAddr(c.addr)
		if got := b.cdpEndpoint.Load().(string); got != c.want {
			t.Errorf("%v → %q, want %q", c.addr, got, c.want)
		}
	}
}

// The shared_browser tool and /api/browser both report the MCP endpoint.
func TestSharedBrowserReportsMCPEndpoint(t *testing.T) {
	openTestDB(t)
	f := newFakeChromium(t)
	prevB, prevM := sharedBrowser, browserMCP
	sharedBrowser = testBrowserManager(t, f)
	browserMCP = testBrowserMCP(t, "ok", 4)
	t.Cleanup(func() { sharedBrowser, browserMCP = prevB, prevM })

	st := sharedBrowser.status()
	if !st.MCPAvailable || st.MCPBinary == "" || st.MCPReason != "" || st.MCPSessions != 0 {
		t.Errorf("status = %+v", st)
	}

	srv := httptest.NewServer(withRequestBase(newMCPHandler()))
	defer srv.Close()
	sess, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var out sharedBrowserOut
	if msg := callTool(t, sess, "shared_browser", map[string]any{"start": false}, &out); msg != "" {
		t.Fatal(msg)
	}
	su, _ := url.Parse(srv.URL)
	if out.MCPEndpoint != "http://"+su.Host+"/browser-mcp" || !out.MCPAvailable || out.MCPReason != "" {
		t.Errorf("out = %+v", out)
	}

	browserMCP = newBrowserMCPBridge(browserMCPConfig{Binary: "off"})
	out = sharedBrowserOut{}
	if msg := callTool(t, sess, "shared_browser", map[string]any{"start": false}, &out); msg != "" {
		t.Fatal(msg)
	}
	if out.MCPAvailable || !strings.Contains(out.MCPReason, "disabled") {
		t.Errorf("off: %+v", out)
	}
}
