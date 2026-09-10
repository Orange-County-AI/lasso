package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestTtydRole builds a role with no instances and a short bind timeout, for
// tests that never spawn a real ttyd.
func newTestTtydRole(t *testing.T) *ttydRole {
	t.Helper()
	r := newTtydRole(context.Background(), "ttyd", "/terminal")
	r.waitTimeout = 20 * time.Millisecond
	return r
}

// A resident instance is REUSED, not respawned: that is what makes a second tab
// arriving on a host someone already has open free. If ensure ever reached
// startTtyd it would fail (empty PATH), and retiring the old instance would call
// its cancel.
func TestTtydRoleReusesResidentInstance(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	r := newTestTtydRole(t)
	stopped := false
	r.inst["ticket500"] = &ttydInstance{
		sock:       "/tmp/lasso-ttyd-test-ticket500.sock",
		cancel:     func() { stopped = true },
		lastActive: time.Now().Add(-time.Hour),
	}
	r.bySlug["ticket500"] = "ticket500"
	r.inst["local"] = &ttydInstance{sock: "/tmp/lasso-ttyd-test-local.sock", cancel: func() {}, lastActive: time.Now()}
	r.bySlug["local"] = "local"

	if err := r.ensure("ticket500", "herdr --remote ticket500", nil); err != nil {
		t.Fatalf("ensure a resident host: %v", err)
	}
	if stopped {
		t.Error("the resident instance was stopped — arriving on a warm host must not respawn it")
	}
	if got, want := r.sockForSlug("ticket500"), "/tmp/lasso-ttyd-test-ticket500.sock"; got != want {
		t.Errorf("sockForSlug = %q, want %q", got, want)
	}
	// The whole point of per-host routing: the other host's terminal is still
	// dialable at the same time, for the tab that is on it.
	if got, want := r.sockForSlug("local"), "/tmp/lasso-ttyd-test-local.sock"; got != want {
		t.Errorf("sockForSlug(local) = %q, want %q — a second host's terminal must stay reachable", got, want)
	}
	if len(r.inst) != 2 {
		t.Errorf("instances = %d, want both hosts resident", len(r.inst))
	}
}

// A spawn that fails registers nothing, so the proxy keeps reporting "no ttyd"
// instead of dialing a path nothing is listening on.
func TestTtydRoleFailedSpawnRegistersNothing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no ttyd binary
	r := newTestTtydRole(t)
	if err := r.ensure("local", "sh", nil); err == nil {
		t.Fatal("ensure succeeded without a ttyd binary")
	}
	if len(r.inst) != 0 {
		t.Errorf("instances = %d, want 0 after a failed spawn", len(r.inst))
	}
	if got := r.sockForSlug("local"); got != "" {
		t.Errorf("sockForSlug = %q, want empty", got)
	}
}

// An unknown slug — a stale iframe still pointed at a retired host — resolves to
// nothing rather than to some other host's terminal.
func TestTtydRoleUnknownSlugResolvesToNothing(t *testing.T) {
	r := newTestTtydRole(t)
	r.inst["local"] = &ttydInstance{sock: "/tmp/local.sock", cancel: func() {}, lastActive: time.Now()}
	r.bySlug["local"] = "local"
	if got := r.sockForSlug("norm"); got != "" {
		t.Errorf("sockForSlug(unknown) = %q, want empty", got)
	}
}

// Eviction retires the least-recently-active host and never a WATCHED one.
func TestTtydRoleEvictsLeastRecentlyActive(t *testing.T) {
	setDefaultBackend(&localBackend{})
	t.Cleanup(func() { setDefaultBackend(nil) })
	r := newTestTtydRole(t)
	now := time.Now()
	stopped := map[string]bool{}
	add := func(host string, age time.Duration) {
		r.inst[host] = &ttydInstance{
			sock:       "/tmp/" + host + ".sock",
			cancel:     func() { stopped[host] = true },
			lastActive: now.Add(-age),
		}
		r.bySlug[hostSlug(host)] = host
	}
	// One host over the ceiling. The default host is the oldest on purpose: it
	// must survive regardless, since a tab is always able to be sitting on it.
	add("local", 10*time.Hour)
	add("ocai", 3*time.Hour)
	add("norm", 2*time.Hour)
	add("wistock", 90*time.Minute)
	add("52labs", time.Hour)
	add("visiquate", 30*time.Minute)
	add("ticket500", time.Minute)

	r.evictLocked()

	if len(r.inst) != ttydWarmHosts {
		t.Fatalf("instances = %d, want %d", len(r.inst), ttydWarmHosts)
	}
	if !stopped["ocai"] {
		t.Error("the least-recently-active host was not retired")
	}
	if stopped["local"] {
		t.Error("the DEFAULT host was retired")
	}
	if _, ok := r.inst["ocai"]; ok {
		t.Error("retired host still resident")
	}
	for _, host := range []string{"local", "norm", "wistock", "52labs", "visiquate", "ticket500"} {
		if _, ok := r.inst[host]; !ok {
			t.Errorf("%s was evicted but is inside the warm window", host)
		}
	}
}

// A host that drops out of rotation is retired on idle, so a long session pays
// only for the terminals still in use. A watched host is exempt however long its
// terminal sits idle — with several tabs open, "least recently active" stops
// being a proxy for "nobody is looking at it".
func TestTtydRoleRetiresIdleInstances(t *testing.T) {
	setDefaultBackend(&localBackend{})
	t.Cleanup(func() { setDefaultBackend(nil) })
	r := newTestTtydRole(t)
	now := time.Now()
	stopped := map[string]bool{}
	add := func(host string, age time.Duration) {
		r.inst[host] = &ttydInstance{
			sock:       "/tmp/" + host + ".sock",
			cancel:     func() { stopped[host] = true },
			lastActive: now.Add(-age),
		}
		r.bySlug[hostSlug(host)] = host
	}
	add("local", 10*time.Hour) // the default host, idle far past the window
	add("ocai", ttydIdle+time.Minute)
	add("norm", ttydIdle-time.Minute)

	r.retireIdleLocked(now)

	if stopped["local"] {
		t.Error("the DEFAULT host's terminal was retired for being idle")
	}
	if !stopped["ocai"] {
		t.Error("an instance idle past ttydIdle was kept")
	}
	if _, ok := r.inst["norm"]; !ok || stopped["norm"] {
		t.Error("an instance inside ttydIdle was retired")
	}
	if len(r.inst) != 2 {
		t.Errorf("instances = %d, want local + norm", len(r.inst))
	}
}

// Every host gets its own socket path AND its own proxy path segment. Two
// aliases that sanitize to the same string must NOT collide: with per-host
// routing a collision means two hosts sharing one terminal, so an altered alias
// carries a digest of the original.
func TestHostSlugIsInjective(t *testing.T) {
	if got, want := hostSlug("ticket500"), "ticket500"; got != want {
		t.Errorf("hostSlug(%q) = %q, want it unchanged", "ticket500", got)
	}
	a, b := hostSlug("weird/alias:1"), hostSlug("weird_alias_1")
	if a == b {
		t.Fatalf("hostSlug collided on %q and %q: both %q", "weird/alias:1", "weird_alias_1", a)
	}
	for _, s := range []string{a, b} {
		if strings.ContainsAny(s, "/:. ") {
			t.Errorf("hostSlug = %q, want it safe in a URL path and a filename", s)
		}
	}
}

func TestTtydRoleSockPathIsPerHost(t *testing.T) {
	r := newTestTtydRole(t)
	local, remote := r.sockPath("local"), r.sockPath("ticket500")
	if local == remote {
		t.Fatalf("sockPath collided: %q", local)
	}
	for _, p := range []string{local, remote} {
		if !strings.Contains(p, "lasso-ttyd-") {
			t.Errorf("sockPath = %q, want the role's filename stem", p)
		}
	}
	shell := newTtydRole(context.Background(), "shell", "/shell")
	if shell.sockPath("local") == local {
		t.Error("the two roles share a socket path for one host")
	}
}

// A nil role (spawn-ttyd=false) answers the proxy without panicking.
func TestTtydRoleNilSafe(t *testing.T) {
	var r *ttydRole
	if got := r.sockForSlug("local"); got != "" {
		t.Errorf("sockForSlug on a nil role = %q, want empty", got)
	}
	if r.resident("local") {
		t.Error("a nil role reported a resident instance")
	}
	if err := r.ensure("local", "sh", nil); err != nil {
		t.Errorf("ensure on a nil role = %v, want nil", err)
	}
}

// A ttyd's `-t theme=` is frozen at spawn, so the DOCUMENT must carry the
// palette lasso is painting now — otherwise a terminal opened directly at
// /terminal/<slug>/ keeps serving the theme its process started under (a light
// canvas under dark herdr chrome, for hours). Only the document is rewritten:
// an asset or the websocket must reach ttyd untouched, and a URL that already
// names a theme is left alone, or the redirect would loop.
func TestLiveTtydThemeOnDocumentOnly(t *testing.T) {
	prev := srvHub
	srvHub = newHub()
	srvHub.curTheme = resolveThemeByName("nord")
	t.Cleanup(func() { srvHub = prev })

	var reached string
	h := withLiveTtydTheme(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = r.URL.RequestURI()
	}))
	get := func(target string) *httptest.ResponseRecorder {
		reached = ""
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}

	rec := get("/terminal/local/")
	if rec.Code != http.StatusFound {
		t.Fatalf("document should be redirected onto the live theme, got %d", rec.Code)
	}
	if reached != "" {
		t.Errorf("document must not reach ttyd before it carries a theme (got %q)", reached)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location %q: %v", rec.Header().Get("Location"), err)
	}
	if loc.Path != "/terminal/local/" {
		t.Errorf("redirect moved the path: %q", loc.Path)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(loc.Query().Get("theme")), &got); err != nil {
		t.Fatalf("theme option isn't the xterm.js ITheme: %v", err)
	}
	if want := resolveThemeByName("nord").ui.PanelBg; got["background"] != want {
		t.Errorf("document carries background %q, want the live theme's %q", got["background"], want)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("a redirect carrying the palette must not be cached, got %q", cc)
	}

	// The websocket and ttyd's own assets are not documents.
	for _, path := range []string{"/terminal/local/ws", "/terminal/local/token", "/terminal/local/favicon.png"} {
		if rec := get(path); rec.Code != http.StatusOK || reached != path {
			t.Errorf("%s should pass through, got %d (reached %q)", path, rec.Code, reached)
		}
	}

	// Already themed: passed through, so a browser following the redirect
	// cannot be redirected again.
	themed := "/terminal/local/?theme=" + url.QueryEscape(`{"background":"#000000"}`)
	if rec := get(themed); rec.Code != http.StatusOK || reached != themed {
		t.Errorf("a themed document should pass through, got %d (reached %q)", rec.Code, reached)
	}
}

// The document has to boot on the palette the TAB is wearing, transparency
// included: xterm answers herdr's OSC 11 query from whatever it booted with,
// and herdr keeps that answer for the session and every client on it, so a tab
// that boots its terminal on the wrong lightness repaints every pane on that
// herdr — in other browsers too. Re-pinning from the page afterwards is too
// late for that, and gives up entirely on a ttyd slower than 5s.
func TestTtydDocThemeFollowsTabPalette(t *testing.T) {
	prev := srvHub
	srvHub = newHub()
	srvHub.curTheme = resolveThemeByName("nord")
	t.Cleanup(func() { srvHub = prev })

	live := resolveThemeByName("nord")
	bg := func(q string) string {
		var got map[string]string
		v, err := url.ParseQuery(q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		if err := json.Unmarshal([]byte(ttydDocTheme(v)), &got); err != nil {
			t.Fatalf("theme for %q isn't an ITheme: %v", q, err)
		}
		return got["background"]
	}

	if got := bg(""); got != live.ui.PanelBg {
		t.Errorf("no palette named: background %q, want the live theme's %q", got, live.ui.PanelBg)
	}
	// A light tab on a dark fleet: the whole point.
	lotus := resolveThemeByName("kanagawa-lotus")
	if got := bg("palette=kanagawa-lotus"); got != lotus.ui.PanelBg {
		t.Errorf("named palette: background %q, want %q", got, lotus.ui.PanelBg)
	}
	// A stale preference costs the shared look, never the terminal.
	if got := bg("palette=no-such-theme"); got != live.ui.PanelBg {
		t.Errorf("unknown palette: background %q, want the live theme's %q", got, live.ui.PanelBg)
	}
	if got := bg("transparent=1"); got != live.ui.PanelBg+"00" {
		t.Errorf("transparent: background %q, want %q", got, live.ui.PanelBg+"00")
	}
	if got := bg("palette=kanagawa-lotus&transparent=1"); got != lotus.ui.PanelBg+"00" {
		t.Errorf("named + transparent: background %q, want %q", got, lotus.ui.PanelBg+"00")
	}
	// The cursor's own glyph is drawn IN the block, so it stays opaque or the
	// cursor becomes a hole in the wallpaper.
	var full map[string]string
	if err := json.Unmarshal([]byte(ttydDocTheme(url.Values{"transparent": {"1"}})), &full); err != nil {
		t.Fatalf("ITheme: %v", err)
	}
	if full["cursorAccent"] != live.ui.PanelBg {
		t.Errorf("cursorAccent %q went transparent with the canvas", full["cursorAccent"])
	}

	// ttyd merges the whole query into its client options, so the two keys it
	// has never heard of must not survive the redirect.
	var reached string
	h := withLiveTtydTheme(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = r.URL.RequestURI()
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/terminal/local/?palette=kanagawa-lotus&transparent=1", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("document should be redirected, got %d (reached %q)", rec.Code, reached)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location %q: %v", rec.Header().Get("Location"), err)
	}
	if loc.Query().Has("palette") || loc.Query().Has("transparent") {
		t.Errorf("the tab's own hints leaked into ttyd's options: %q", loc.RawQuery)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(loc.Query().Get("theme")), &got); err != nil {
		t.Fatalf("theme option isn't the xterm.js ITheme: %v", err)
	}
	if want := lotus.ui.PanelBg + "00"; got["background"] != want {
		t.Errorf("redirect carries background %q, want %q", got["background"], want)
	}
}
