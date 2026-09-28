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
// max <= 0 is no limit, the production default.
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

// childPIDs is every live child's pid, by profile, across all sessions.
func (b *browserMCPBridge) childPIDs() map[string][]int {
	out := map[string][]int{}
	for _, s := range b.snapshot() {
		s.mu.Lock()
		for p, sl := range s.children {
			if sl.child != nil {
				out[p] = append(out[p], sl.child.pid)
			}
		}
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

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitChildren(t *testing.T, b *browserMCPBridge, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b.liveChildren() == n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("live children = %d, want %d", b.liveChildren(), n)
}

// fakeEcho is what the fake's echo tool answers.
type fakeEcho struct {
	Args map[string]any `json:"args"`
	Argv []string       `json:"argv"`
}

// wsEndpoint is the --wsEndpoint the child was started with: which profile's
// /cdp it dials.
func (e fakeEcho) wsEndpoint() string {
	for i, a := range e.Argv {
		if a == "--wsEndpoint" && i+1 < len(e.Argv) {
			return e.Argv[i+1]
		}
	}
	return ""
}

// callEcho calls the fake's echo tool. A tool error comes back as its text in
// refusal, with a zero fakeEcho.
func callEcho(t *testing.T, sess *mcp.ClientSession, args map[string]any) (out fakeEcho, refusal string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: args})
	if err != nil {
		t.Fatalf("echo %v: %v", args, err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError {
		return fakeEcho{}, text
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("echo answer %q: %v", text, err)
	}
	return out, ""
}

func TestBrowserMCPBridgeMirrorsAndForwards(t *testing.T) {
	t.Setenv("UI_AUTH", "u:secret")
	t.Setenv("MCP_OAUTH", "cid:csecret")
	openTestDB(t) // resolving a named profile reads the stored list
	b := testBrowserMCP(t, "ok", 0)
	srv := httptest.NewServer(b)
	defer srv.Close()

	sess, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	init := sess.InitializeResult()
	if init.ServerInfo.Name != "lasso-browser" || !strings.Contains(init.Instructions, "SHARED browser") || !strings.Contains(init.Instructions, "`profile`") {
		t.Errorf("initialize = %+v / %q", init.ServerInfo, init.Instructions)
	}
	if init.Capabilities.Tools == nil {
		t.Errorf("no tools capability advertised")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// tools/list mirrors the child: names, descriptions, schemas, annotations —
	// plus the bridge's own optional `profile`.
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
	for _, name := range []string{"echo", "snap", "env"} {
		s, _ := json.Marshal(byName[name].InputSchema)
		if !strings.Contains(string(s), `"profile":{"description":"Browser profile`) {
			t.Errorf("%s schema lacks profile: %s", name, s)
		}
		if strings.Contains(string(s), `"required":["profile"`) {
			t.Errorf("%s: profile must be optional: %s", name, s)
		}
	}
	if s, _ := json.Marshal(byName["echo"].InputSchema); !strings.Contains(string(s), `"x"`) {
		t.Errorf("echo schema = %s", s)
	}

	// tools/call forwards the arguments as given, minus `profile`, and the
	// child ran with lasso's flags: its own /cdp, the internal token, the
	// screenshot bounds.
	echoed, msg := callEcho(t, sess, map[string]any{"x": 7, "profile": "default"})
	if msg != "" {
		t.Fatal(msg)
	}
	if echoed.Args["x"] != float64(7) || echoed.Args["profile"] != nil || len(echoed.Args) != 1 {
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
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "snap"})
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

	// An unknown tool is the mirror's refusal, not a hang.
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "nope"}); err == nil {
		t.Errorf("unknown tool succeeded")
	}

	// Three calls to the default profile: one child, reused.
	pids := b.childPIDs()
	if len(pids) != 1 || len(pids[defaultBrowserProfile]) != 1 || pids[defaultBrowserProfile][0] <= 1 {
		t.Fatalf("live pids = %v", pids)
	}
	if b.sessions() != 1 || b.liveChildren() != 1 {
		t.Errorf("sessions=%d children=%d", b.sessions(), b.liveChildren())
	}
	// Closing the session (DELETE) kills the child.
	_ = sess.Close()
	waitChildren(t, b, 0)
	waitGone(t, pids[defaultBrowserProfile][0])
}

// initialize and tools/list spawn nothing: the tool list comes from a cache,
// learned by ONE short probe per lasso process (per binary), not per session.
func TestBrowserMCPLazySpawn(t *testing.T) {
	b := testBrowserMCP(t, "ok", 0)
	srv := httptest.NewServer(b)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var sessions []*mcp.ClientSession
	for i := 0; i < 5; i++ {
		sess, err := browserMCPConnect(t, srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		lt, err := sess.ListTools(ctx, nil)
		if err != nil || len(lt.Tools) != 3 {
			t.Fatalf("session %d tools: %v %v", i, err, lt)
		}
		sessions = append(sessions, sess)
	}
	if n := b.probes.Load(); n != 1 {
		t.Errorf("probes = %d, want exactly 1 for 5 sessions", n)
	}
	if b.liveChildren() != 0 || b.sessions() != 0 || len(b.childPIDs()) != 0 {
		t.Errorf("after initialize + tools/list: children=%d sessions-with-children=%d", b.liveChildren(), b.sessions())
	}
	if len(b.snapshot()) != 5 {
		t.Errorf("registered sessions = %d", len(b.snapshot()))
	}

	// The first tool call is what spawns.
	if _, msg := callEcho(t, sessions[2], nil); msg != "" {
		t.Fatal(msg)
	}
	if b.liveChildren() != 1 || b.sessions() != 1 {
		t.Errorf("after one call: children=%d sessions=%d", b.liveChildren(), b.sessions())
	}

	// A binary that changed under lasso (an upgrade) is probed again.
	b.tools.Store(&browserMCPToolCache{key: "some older binary", tools: nil})
	sess, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if lt, err := sess.ListTools(ctx, nil); err != nil || len(lt.Tools) != 3 {
		t.Fatalf("after re-probe: %v %v", err, lt)
	}
	if n := b.probes.Load(); n != 2 {
		t.Errorf("probes after a binary change = %d, want 2", n)
	}
}

// Calls route to the profile they name, each profile getting its own child in
// the session, and an unknown profile is an error naming the real ones.
func TestBrowserMCPRoutesByProfile(t *testing.T) {
	f := testFleet(t)
	if _, err := f.create("Work", "", ""); err != nil {
		t.Fatal(err)
	}
	b := testBrowserMCP(t, "ok", 0)
	srv := httptest.NewServer(b)
	defer srv.Close()
	sess, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if ins := sess.InitializeResult().Instructions; !strings.Contains(ins, `work ("Work")`) {
		t.Errorf("instructions do not list the profiles: %q", ins)
	}

	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{nil, "ws://127.0.0.1:1/cdp"},
		{map[string]any{"profile": "work"}, "ws://127.0.0.1:1/cdp/p/work"},
		{map[string]any{"profile": "WORK"}, "ws://127.0.0.1:1/cdp/p/work"}, // the display name, any case
		{map[string]any{"profile": ""}, "ws://127.0.0.1:1/cdp"},
		{map[string]any{"profile": "default", "x": 1}, "ws://127.0.0.1:1/cdp"},
	} {
		got, msg := callEcho(t, sess, c.args)
		if msg != "" {
			t.Fatalf("%v: %s", c.args, msg)
		}
		if got.wsEndpoint() != c.want {
			t.Errorf("%v dialed %q, want %q", c.args, got.wsEndpoint(), c.want)
		}
		if _, has := got.Args["profile"]; has {
			t.Errorf("%v: profile reached the child: %v", c.args, got.Args)
		}
	}
	pids := b.childPIDs()
	if len(pids[defaultBrowserProfile]) != 1 || len(pids["work"]) != 1 || b.liveChildren() != 2 {
		t.Errorf("children = %v (%d)", pids, b.liveChildren())
	}

	// Unknown: a tool error listing the profiles, nothing spawned, and no
	// fallback to another profile.
	_, msg := callEcho(t, sess, map[string]any{"profile": "personal", "x": 1})
	if !strings.Contains(msg, `no browser profile "personal"`) || !strings.Contains(msg, `work ("Work")`) || !strings.Contains(msg, "default") {
		t.Errorf("unknown profile: %q", msg)
	}
	if _, msg := callEcho(t, sess, map[string]any{"profile": 3}); !strings.Contains(msg, "must be a string") {
		t.Errorf("non-string profile: %q", msg)
	}
	if b.liveChildren() != 2 {
		t.Errorf("a refused call spawned: %d children", b.liveChildren())
	}
}

// Profiles resolve at call time: one created after the session started works
// without reconnecting, and one deleted since is refused.
func TestBrowserMCPProfileCreatedAfterSessionStart(t *testing.T) {
	f := testFleet(t)
	b := testBrowserMCP(t, "ok", 0)
	srv := httptest.NewServer(b)
	defer srv.Close()
	sess, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, msg := callEcho(t, sess, map[string]any{"profile": "late"}); !strings.Contains(msg, `no browser profile "late"`) {
		t.Fatalf("before it exists: %q", msg)
	}
	if _, err := f.create("Late", "late", ""); err != nil {
		t.Fatal(err)
	}
	got, msg := callEcho(t, sess, map[string]any{"profile": "Late"})
	if msg != "" || got.wsEndpoint() != "ws://127.0.0.1:1/cdp/p/late" {
		t.Fatalf("after create: %q %q", msg, got.wsEndpoint())
	}
	if err := f.remove(context.Background(), "late"); err != nil {
		t.Fatal(err)
	}
	if _, msg := callEcho(t, sess, map[string]any{"profile": "late"}); !strings.Contains(msg, `no browser profile "late"`) {
		t.Errorf("after delete: %q", msg)
	}
}

// One profile's browser stopping closes only that profile's child in each
// session; the session and its other children carry on, and the next call to
// the stopped profile spawns a fresh child.
func TestBrowserMCPProfileStopClosesOnlyThatChild(t *testing.T) {
	f := testFleet(t)
	if _, err := f.create("Work", "work", ""); err != nil {
		t.Fatal(err)
	}
	b := testBrowserMCP(t, "ok", 0)
	f.onStop = b.browserStoppedFor
	sharedBrowser.onStop = b.browserStopped
	work := runProfileOn(t, f, "work", newFakeChromium(t))
	srv := httptest.NewServer(b)
	defer srv.Close()

	a, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	other, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for _, s := range []*mcp.ClientSession{a, other} {
		for _, p := range []string{"default", "work"} {
			if _, msg := callEcho(t, s, map[string]any{"profile": p}); msg != "" {
				t.Fatal(msg)
			}
		}
	}
	before := b.childPIDs()
	if len(before["work"]) != 2 || len(before[defaultBrowserProfile]) != 2 {
		t.Fatalf("children = %v", before)
	}

	if err := work.stop(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	for _, pid := range before["work"] {
		waitGone(t, pid)
	}
	after := b.childPIDs()
	if len(after["work"]) != 0 || len(after[defaultBrowserProfile]) != 2 {
		t.Errorf("after the work stop: %v", after)
	}
	for _, pid := range before[defaultBrowserProfile] {
		if !alive(pid) {
			t.Errorf("default child %d died with the work profile's browser", pid)
		}
	}
	if len(b.snapshot()) != 2 {
		t.Errorf("sessions = %d, want both still open", len(b.snapshot()))
	}

	// Both sessions keep working: the default child is the same process, and
	// work gets a new one.
	got, msg := callEcho(t, a, nil)
	if msg != "" || got.wsEndpoint() != "ws://127.0.0.1:1/cdp" {
		t.Fatalf("default after the stop: %q %q", msg, got.wsEndpoint())
	}
	if got, msg := callEcho(t, a, map[string]any{"profile": "work"}); msg != "" || got.wsEndpoint() != "ws://127.0.0.1:1/cdp/p/work" {
		t.Fatalf("work after the stop: %q %q", msg, got.wsEndpoint())
	}
	now := b.childPIDs()
	if len(now["work"]) != 1 || now["work"][0] == before["work"][0] || now["work"][0] == before["work"][1] {
		t.Errorf("work children after respawn = %v (before %v)", now["work"], before["work"])
	}
}

// A child that dies on its own is dropped; the session survives and the next
// call to that profile respawns.
func TestBrowserMCPChildDeathRespawns(t *testing.T) {
	b := testBrowserMCP(t, "ok", 0)
	srv := httptest.NewServer(b)
	defer srv.Close()
	sess, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, msg := callEcho(t, sess, nil); msg != "" {
		t.Fatal(msg)
	}
	pid := b.childPIDs()[defaultBrowserProfile][0]
	_ = syscall.Kill(pid, syscall.SIGKILL)
	waitChildren(t, b, 0)
	if _, msg := callEcho(t, sess, nil); msg != "" {
		t.Fatalf("after the child died: %s", msg)
	}
	if p := b.childPIDs()[defaultBrowserProfile]; len(p) != 1 || p[0] == pid {
		t.Errorf("respawned = %v (old %d)", p, pid)
	}
}

// No limit by default: many sessions and profiles each get their child.
func TestBrowserMCPNoCapByDefault(t *testing.T) {
	if b := newBrowserMCPBridge(browserMCPConfig{}); b.cfg.Max != 0 {
		t.Errorf("default Max = %d, want 0 (no limit)", b.cfg.Max)
	}
	b := testBrowserMCP(t, "ok", 0)
	srv := httptest.NewServer(b)
	defer srv.Close()
	for i := 0; i < 10; i++ {
		sess, err := browserMCPConnect(t, srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		if _, msg := callEcho(t, sess, nil); msg != "" {
			t.Fatalf("session %d: %s", i, msg)
		}
	}
	if b.liveChildren() != 10 {
		t.Errorf("children = %d, want 10", b.liveChildren())
	}
}

// The opt-in limit counts children, not sessions: a session over it still
// opens, and its first call is refused naming the knob.
func TestBrowserMCPOptInCap(t *testing.T) {
	testFleet(t)
	if _, err := sharedBrowsers.create("Work", "work", ""); err != nil {
		t.Fatal(err)
	}
	b := testBrowserMCP(t, "ok", 1)
	srv := httptest.NewServer(b)
	defer srv.Close()
	first, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, msg := callEcho(t, first, nil); msg != "" {
		t.Fatal(msg)
	}
	second, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatalf("initialize spawns nothing, so it is never over the cap: %v", err)
	}
	defer second.Close()
	if _, msg := callEcho(t, second, nil); !strings.Contains(msg, "limit of 1") || !strings.Contains(msg, "LASSO_BROWSER_MCP_MAX") {
		t.Errorf("second session's call over the cap: %q", msg)
	}
	if _, msg := callEcho(t, first, map[string]any{"profile": "work"}); !strings.Contains(msg, "limit of 1") {
		t.Errorf("a second profile in the same session over the cap: %q", msg)
	}
	b.mu.Lock()
	starting := b.starting
	b.mu.Unlock()
	if b.liveChildren() != 1 || starting != 0 {
		t.Errorf("after refusals: children=%d starting=%d", b.liveChildren(), starting)
	}
	_ = first.Close()
	waitChildren(t, b, 0)
	if _, msg := callEcho(t, second, nil); msg != "" {
		t.Fatalf("a slot freed by a close is reusable: %s", msg)
	}
}

// /browser-mcp/<id> is pinned to its profile as before: its tools take no
// `profile`, calls dial that profile, and naming another is refused.
func TestBrowserMCPPinnedPath(t *testing.T) {
	f := testFleet(t)
	if _, err := f.create("Work", "work", ""); err != nil {
		t.Fatal(err)
	}
	b := testBrowserMCP(t, "ok", 0)
	mux := http.NewServeMux()
	mux.Handle("/browser-mcp", b)
	mux.Handle("/browser-mcp/", b)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := browserMCPConnect(t, srv.URL+"/browser-mcp/nope"); err == nil {
		t.Errorf("an unknown pinned profile opened a session")
	}
	sess, err := browserMCPConnect(t, srv.URL+"/browser-mcp/work")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if ins := sess.InitializeResult().Instructions; !strings.Contains(ins, `PROFILE "work"`) {
		t.Errorf("instructions = %q", ins)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	lt, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range lt.Tools {
		if s, _ := json.Marshal(tl.InputSchema); strings.Contains(string(s), `"profile"`) {
			t.Errorf("pinned %s advertises profile: %s", tl.Name, s)
		}
	}
	if b.liveChildren() != 0 {
		t.Errorf("a pinned initialize spawned")
	}
	got, msg := callEcho(t, sess, map[string]any{"x": 1})
	if msg != "" || got.wsEndpoint() != "ws://127.0.0.1:1/cdp/p/work" || got.Args["x"] != float64(1) {
		t.Errorf("pinned call: %q %q %v", msg, got.wsEndpoint(), got.Args)
	}
	if got, msg := callEcho(t, sess, map[string]any{"profile": "Work"}); msg != "" || got.Args["profile"] != nil {
		t.Errorf("matching profile: %q %v", msg, got.Args)
	}
	if _, msg := callEcho(t, sess, map[string]any{"profile": "default"}); !strings.Contains(msg, `pinned to browser profile "work"`) {
		t.Errorf("mismatched profile: %q", msg)
	}
	if b.childPIDs()[defaultBrowserProfile] != nil {
		t.Errorf("a pinned session spawned a default child")
	}
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
	if len(b.snapshot()) != 0 || b.liveChildren() != 0 || b.starting != 0 {
		t.Errorf("after a failed start: sessions=%d children=%d starting=%d", len(b.snapshot()), b.liveChildren(), b.starting)
	}
}

// With the tool list already learned, a child that fails to start is the tool
// call's error (isError, with the reason), not a broken session.
func TestBrowserMCPSpawnFailureIsAToolError(t *testing.T) {
	b := testBrowserMCP(t, "ok", 0)
	srv := httptest.NewServer(b)
	defer srv.Close()
	sess, err := browserMCPConnect(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	b.cfg.ExtraEnv = []string{fakeBrowserMCPEnv + "=crash"}
	_, msg := callEcho(t, sess, nil)
	if !strings.Contains(msg, "exited during startup") || !strings.Contains(msg, "boom") {
		t.Errorf("spawn failure: %q", msg)
	}
	b.cfg.ExtraEnv = []string{fakeBrowserMCPEnv + "=ok"}
	if _, msg := callEcho(t, sess, nil); msg != "" {
		t.Errorf("a failed spawn is not remembered; the next call retries: %s", msg)
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
