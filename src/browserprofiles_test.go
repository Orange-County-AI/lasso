package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testFleet installs a fleet (and a default manager) for one test, both
// restored afterwards. The fleet's managers never launch anything: a test
// that needs a "running" profile gives it a fake with runProfileOn.
func testFleet(t *testing.T) *browserFleet {
	t.Helper()
	openTestDB(t)
	f := newBrowserFleet(browserConfig{Dir: t.TempDir(), Idle: 15 * time.Minute})
	prevF, prevB := sharedBrowsers, sharedBrowser
	sharedBrowsers = f
	sharedBrowser = testBrowserManager(t, nil)
	sharedBrowser.proxy = func() string { v, _ := getSetting(browserProxySetting); return v }
	t.Cleanup(func() { sharedBrowsers, sharedBrowser = prevF, prevB })
	prevChanged := browserProfilesChanged
	browserProfilesChanged = func() {}
	t.Cleanup(func() { browserProfilesChanged = prevChanged })
	return f
}

// runProfileOn makes a profile's manager look like it has a running browser
// (the fake), without launching one.
func runProfileOn(t *testing.T, f *browserFleet, id string, fc *fakeChromium) *browserManager {
	t.Helper()
	m, err := f.manager(id)
	if err != nil {
		t.Fatal(err)
	}
	m.search = func() browserSearch {
		return browserSearch{Explicit: "/fake/chrome", Exists: func(string) bool { return true }}
	}
	u, _ := url.Parse(fc.srv.URL)
	port, _ := strconv.Atoi(u.Port())
	m.proc = &browserProc{bin: "/fake/chrome", port: port, wsPath: "/devtools/browser/abc",
		started: time.Now(), exited: make(chan struct{})}
	return m
}

func TestSlugProfileID(t *testing.T) {
	for in, want := range map[string]string{
		"Work":                  "work",
		"Work (US exit)":        "work-us-exit",
		"  --Ünïcode--  ":       "n-code",
		"!!!":                   "profile",
		strings.Repeat("a", 40): strings.Repeat("a", 28),
	} {
		if got := slugProfileID(in); got != want {
			t.Errorf("slugProfileID(%q) = %q, want %q", in, got, want)
		}
		if err := validBrowserProfileID(slugProfileID(in)); err != nil {
			t.Errorf("slug of %q does not validate: %v", in, err)
		}
	}
	for _, bad := range []string{"", "-x", "A", "a/b", "..", "a_b", strings.Repeat("a", 33)} {
		if validBrowserProfileID(bad) == nil {
			t.Errorf("id %q validated", bad)
		}
	}
}

func TestBrowserProfileCRUD(t *testing.T) {
	f := testFleet(t)

	p, err := f.create("Work", "", "socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "work" || p.Proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("created %+v", p)
	}
	// A derived id is made unique; an explicit one that clashes is refused, as
	// is a duplicate name in any case.
	if p2, err := f.create("Work!", "", ""); err != nil || p2.ID != "work-2" {
		t.Fatalf("second derived id = %+v, %v", p2, err)
	}
	if _, err := f.create("Other", "work", ""); err == nil {
		t.Error("an explicit duplicate id was accepted")
	}
	if _, err := f.create("WORK", "", ""); err == nil {
		t.Error("a duplicate name was accepted")
	}
	if _, err := f.create("Default", "", ""); err == nil {
		t.Error("the default profile's name was accepted for another")
	}
	if _, err := f.create("x", "default", ""); err == nil {
		t.Error("id default was accepted")
	}
	if _, err := f.create("Bad", "", "socks5h://h:1"); err == nil || !strings.Contains(err.Error(), "socks5h") {
		t.Errorf("bad proxy: %v", err)
	}

	ps := allBrowserProfiles()
	if len(ps) != 3 || ps[0].ID != defaultBrowserProfile || ps[1].ID != "work" || ps[2].ID != "work-2" {
		t.Fatalf("profiles = %+v", ps)
	}
	if id, err := resolveProfile("WORK"); err != nil || id != "work" {
		t.Errorf("resolve by name = %q, %v", id, err)
	}
	if _, err := resolveProfile("nope"); err == nil || !strings.Contains(err.Error(), "work") {
		t.Errorf("unknown profile error should list the profiles: %v", err)
	}

	name, clear := "Job", ""
	if got, err := f.update("work", &name, &clear); err != nil || got.Name != "Job" || got.Proxy != "" {
		t.Fatalf("update = %+v, %v", got, err)
	}
	// The default is renamed and re-proxied through its own settings, which is
	// where an older lasso looks for its proxy.
	dn, dp := "Personal", "http://127.0.0.1:3128"
	if _, err := f.update(defaultBrowserProfile, &dn, &dp); err != nil {
		t.Fatal(err)
	}
	if v, _ := getSetting(browserProxySetting); v != dp || defaultProfileName() != "Personal" {
		t.Errorf("default proxy %q name %q", v, defaultProfileName())
	}

	// Deleting removes its directory — the logins in it.
	m, _ := f.manager("work")
	if err := os.MkdirAll(filepath.Join(m.profileDir(), "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.remove(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.profileDir()); !os.IsNotExist(err) {
		t.Errorf("profile dir survived deletion: %v", err)
	}
	if !m.retired.Load() {
		t.Error("a deleted profile's manager can still launch")
	}
	if _, err := m.startLocked(); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Errorf("start after delete: %v", err)
	}
	if err := f.remove(context.Background(), defaultBrowserProfile); err == nil {
		t.Error("the default profile was deleted")
	}
	if _, err := browserFor("work"); err == nil {
		t.Error("a deleted profile still resolves")
	}
}

func TestBrowserProfileHTTP(t *testing.T) {
	f := testFleet(t)
	do := func(method, path, body string) (int, string) {
		w := httptest.NewRecorder()
		f.serveProfiles(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	if code, body := do("POST", "/api/browser/profiles", `{"name":"US","proxy":"socks5://10.0.0.1:1080"}`); code != 200 || !strings.Contains(body, `"ws_path":"/cdp/p/us"`) || !strings.Contains(body, `"mcp_path":"/browser-mcp/us"`) {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, body := do("POST", "/api/browser/profiles", `{"name":"Bad","proxy":"socks5://u:p@h:1"}`); code != 400 || !strings.Contains(body, "credentials") {
		t.Errorf("bad proxy: %d %s", code, body)
	}
	if code, body := do("PATCH", "/api/browser/profiles/us", `{"name":"Stateside"}`); code != 200 || !strings.Contains(body, `"name":"Stateside"`) || !strings.Contains(body, "socks5://10.0.0.1:1080") {
		t.Errorf("rename: %d %s", code, body)
	}
	if code, body := do("PATCH", "/api/browser/profiles/us", `{"proxy":""}`); code != 200 || !strings.Contains(body, `"proxy":""`) {
		t.Errorf("clear proxy: %d %s", code, body)
	}
	if code, _ := do("PATCH", "/api/browser/profiles/ghost", `{"name":"x"}`); code != 404 {
		t.Errorf("patch unknown: %d", code)
	}
	if code, _ := do("DELETE", "/api/browser/profiles/default", ""); code != 400 {
		t.Errorf("delete default: %d", code)
	}
	if code, _ := do("DELETE", "/api/browser/profiles/us", ""); code != 204 {
		t.Errorf("delete: %d", code)
	}

	w := httptest.NewRecorder()
	f.serveStatus(w, httptest.NewRequest("GET", "/api/browser", nil))
	var st browserStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Profiles) != 1 || st.Profiles[0].ID != defaultBrowserProfile || !st.Profiles[0].Default || st.Profiles[0].WSPath != "/cdp" {
		t.Errorf("profiles after delete = %+v", st.Profiles)
	}
}

func TestCDPProfileRouting(t *testing.T) {
	for in, want := range map[string][2]string{
		"/cdp":                     {"/cdp", "default"},
		"/cdp/json/list":           {"/cdp", "default"},
		"/cdp/p/work":              {"/cdp/p/work", "work"},
		"/cdp/p/work/devtools/x/y": {"/cdp/p/work", "work"},
	} {
		prefix, profile, ok := cdpProfilePrefix(in)
		if !ok || prefix != want[0] || profile != want[1] {
			t.Errorf("cdpProfilePrefix(%q) = %q %q %v", in, prefix, profile, ok)
		}
	}
	if _, _, ok := cdpProfilePrefix("/cdp/p/"); ok {
		t.Error("an empty profile id routed")
	}
	bp := "/devtools/browser/abc"
	for in, want := range map[string]string{
		"/cdp/p/w":                    bp,
		"/cdp/p/w/":                   bp,
		"/cdp/p/w/devtools/page/P1":   "/devtools/page/P1",
		"/cdp/p/w/json/list":          "/json/list",
		"/cdp/p/w/json":               "/json",
		"/cdp/p/w/devtools/browser/x": "/devtools/browser/x",
	} {
		if got, ok := cdpUpstreamPathAt("/cdp/p/w", in, bp); !ok || got != want {
			t.Errorf("cdpUpstreamPathAt(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for in, want := range map[string]string{
		"/browser-mcp":       "default",
		"/browser-mcp/":      "default",
		"/browser-mcp/work":  "work",
		"/browser-mcp/work/": "work",
	} {
		if got, ok := browserMCPProfile(in); !ok || got != want {
			t.Errorf("browserMCPProfile(%q) = %q %v", in, got, ok)
		}
	}
	if _, ok := browserMCPProfile("/browser-mcp/a/b"); ok {
		t.Error("a nested /browser-mcp path routed")
	}
}

// A profile's /json answer points every websocket back through ITS prefix, not
// the default profile's /cdp — or a client would silently drive the wrong browser.
func TestCDPProxyServesAProfile(t *testing.T) {
	f := testFleet(t)
	if _, err := f.create("Work", "", ""); err != nil {
		t.Fatal(err)
	}
	fc := newFakeChromium(t)
	runProfileOn(t, f, "work", fc)
	lasso := httptest.NewServer(http.HandlerFunc(serveCDPRouted))
	defer lasso.Close()
	lu, _ := url.Parse(lasso.URL)

	resp, err := http.Get(lasso.URL + "/cdp/p/work/json/list")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	want := "ws://" + lu.Host + "/cdp/p/work/devtools/page/P1"
	if !strings.Contains(string(body), want) {
		t.Errorf("list %s lacks %s", body, want)
	}
	if got := fc.last().URL.Path; got != "/json/list" {
		t.Errorf("upstream path %q", got)
	}

	resp, err = http.Get(lasso.URL + "/cdp/p/ghost/json/list")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown profile: %d", resp.StatusCode)
	}
	// The Origin guard still runs first.
	req, _ := http.NewRequest("GET", lasso.URL+"/cdp/p/work/json/list", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("foreign origin: %d", resp.StatusCode)
	}
}

func TestNormalizeTabURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://example.com/x": "https://example.com/x",
		"example.com":           "https://example.com",
		"localhost:5173/app":    "http://localhost:5173/app",
		"127.0.0.1:8080":        "http://127.0.0.1:8080",
		"about:blank":           "about:blank",
	} {
		if got, err := normalizeTabURL(in); err != nil || got != want {
			t.Errorf("normalizeTabURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "file:///etc/passwd", "javascript:alert(1)", "chrome://settings"} {
		if _, err := normalizeTabURL(bad); err == nil {
			t.Errorf("normalizeTabURL(%q) accepted", bad)
		}
	}
}

// The agent's round trip: create a profile, open a tab in it (which reaches
// the human's lasso tabs as a browser-open event), list it, show it, close it.
func TestBrowserProfileMCPTools(t *testing.T) {
	f := testFleet(t)
	var events []browserOpenEvent
	prevB := browserOpenBroadcast
	browserOpenBroadcast = func(ev browserOpenEvent) int { events = append(events, ev); return 2 }
	t.Cleanup(func() { browserOpenBroadcast = prevB })

	srv := httptest.NewServer(withRequestBase(newMCPHandler()))
	defer srv.Close()
	su, _ := url.Parse(srv.URL)
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	var prof browserProfileOut
	if msg := callTool(t, sess, "create_browser_profile", map[string]any{"name": "Work", "proxy": "socks5://127.0.0.1:9050"}, &prof); msg != "" {
		t.Fatal(msg)
	}
	if prof.ID != "work" || prof.MCPEndpoint != "http://"+su.Host+"/browser-mcp/work" || prof.WSEndpoint != "ws://"+su.Host+"/cdp/p/work" {
		t.Fatalf("created %+v", prof)
	}

	fc := newFakeChromium(t)
	runProfileOn(t, f, "work", fc)

	var tab browserTabOut
	if msg := callTool(t, sess, "open_browser_tab", map[string]any{"url": "example.com", "profile": "Work"}, &tab); msg != "" {
		t.Fatal(msg)
	}
	if tab.TabID != "NEW1" || tab.Profile != "work" || tab.URL != "https://example.com" || tab.Delivered != 2 {
		t.Errorf("open = %+v", tab)
	}
	var put *http.Request
	fc.mu.Lock()
	for _, r := range fc.seen {
		if r.Method == http.MethodPut {
			put = r
		}
	}
	fc.mu.Unlock()
	if put == nil || put.URL.Path != "/json/new" || put.URL.RawQuery != "https:%2F%2Fexample.com" {
		t.Errorf("upstream open = %+v", put)
	}
	if len(events) != 1 || events[0].Profile != "work" || events[0].TabID != "NEW1" || events[0].From != "an agent" {
		t.Errorf("events = %+v", events)
	}

	var quiet browserTabOut
	if msg := callTool(t, sess, "open_browser_tab", map[string]any{"url": "https://a.test", "profile": "work", "show": false}, &quiet); msg != "" {
		t.Fatal(msg)
	}
	if len(events) != 1 || quiet.Delivered != 0 {
		t.Errorf("show:false still broadcast: %+v %+v", events, quiet)
	}

	var tabs listBrowserTabsOut
	if msg := callTool(t, sess, "list_browser_tabs", map[string]any{"profile": "work"}, &tabs); msg != "" {
		t.Fatal(msg)
	}
	if len(tabs.Profiles) != 1 || !tabs.Profiles[0].Running || len(tabs.Profiles[0].Tabs) != 1 || tabs.Profiles[0].Tabs[0].ID != "P1" {
		t.Errorf("tabs = %+v", tabs)
	}

	if msg := callTool(t, sess, "show_browser_tab", map[string]any{"tab_id": "P1", "profile": "work"}, &tab); msg != "" {
		t.Fatal(msg)
	}
	if len(events) != 2 || events[1].TabID != "P1" {
		t.Errorf("show events = %+v", events)
	}
	if msg := callTool(t, sess, "show_browser_tab", map[string]any{"tab_id": "NOPE", "profile": "work"}, &tab); !strings.Contains(msg, "no tab") {
		t.Errorf("show of a missing tab: %q", msg)
	}
	if msg := callTool(t, sess, "close_browser_tab", map[string]any{"tab_id": "../x", "profile": "work"}, &tab); !strings.Contains(msg, "not a tab id") {
		t.Errorf("close of a path-shaped id: %q", msg)
	}

	var closed closeBrowserTabOut
	if msg := callTool(t, sess, "close_browser_tab", map[string]any{"tab_id": "P1", "profile": "work"}, &closed); msg != "" {
		t.Fatal(msg)
	}
	if closed.Closed != "P1" || fc.last().URL.Path != "/json/close/P1" {
		t.Errorf("close = %+v, upstream %s", closed, fc.last().URL.Path)
	}

	var list listBrowserProfilesOut
	if msg := callTool(t, sess, "list_browser_profiles", map[string]any{}, &list); msg != "" {
		t.Fatal(msg)
	}
	if len(list.Profiles) != 2 || list.Profiles[1].Proxy != "socks5://127.0.0.1:9050" || !list.Profiles[1].Running {
		t.Errorf("list = %+v", list)
	}

	var sb sharedBrowserOut
	if msg := callTool(t, sess, "shared_browser", map[string]any{"profile": "work", "start": false}, &sb); msg != "" {
		t.Fatal(msg)
	}
	if sb.Profile != "work" || sb.MCPEndpoint != "http://"+su.Host+"/browser-mcp/work" || sb.WSPath != "/cdp/p/work" || len(sb.Pages) != 1 {
		t.Errorf("shared_browser(work) = %+v", sb)
	}

	// A proxy change on a stopped profile only stores it.
	f.mgrs["work"].proc = nil
	var upd browserProfileOut
	if msg := callTool(t, sess, "update_browser_profile", map[string]any{"profile": "work", "proxy": ""}, &upd); msg != "" {
		t.Fatal(msg)
	}
	if upd.Proxy != "" {
		t.Errorf("update = %+v", upd)
	}

	var del deleteBrowserProfileOut
	if msg := callTool(t, sess, "delete_browser_profile", map[string]any{"profile": "work"}, &del); msg != "" {
		t.Fatal(msg)
	}
	if del.Deleted != "work" {
		t.Errorf("delete = %+v", del)
	}
	if msg := callTool(t, sess, "delete_browser_profile", map[string]any{"profile": "default"}, &del); !strings.Contains(msg, "cannot be deleted") {
		t.Errorf("delete default: %q", msg)
	}
}
