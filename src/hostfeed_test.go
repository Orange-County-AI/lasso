package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// paneHostBackend is a fake herdr for one host: pane.list answers with a single
// focused pane whose cwd names the host, so a frame can be traced back to the
// machine it came from. calls counts pane.list round-trips, which is how the
// per-host cache is observed.
type paneHostBackend struct {
	Backend
	host  string
	calls atomic.Int32
}

func (b *paneHostBackend) Name() string      { return b.host }
func (b *paneHostBackend) HerdrSock() string { return "" } // no event stream; the poll carries everything

func (b *paneHostBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	switch method {
	case "pane.list":
		b.calls.Add(1)
		return json.RawMessage(fmt.Sprintf(
			`{"panes":[{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"t1","cwd":"/work/%s","focused":true}]}`,
			b.host)), nil
	case "workspace.list":
		return json.RawMessage(`{"workspaces":[{"workspace_id":"w1","label":"ws","number":1,"focused":true}]}`), nil
	case "tab.get":
		return json.RawMessage(`{"tab":{"label":"tab"}}`), nil
	}
	return json.RawMessage(`{}`), nil
}

// stubTwoHosts stands up two fake hosts and makes both addressable, with the
// first as the default. Returns them in the order given.
func stubTwoHosts(t *testing.T, a, b string) (*paneHostBackend, *paneHostBackend) {
	t.Helper()
	ba, bb := &paneHostBackend{host: a}, &paneHostBackend{host: b}
	byHost := map[string]Backend{a: ba, b: bb}
	stubCloseBackends(t, byHost)
	// hostAllowed gates on the probe store, so declare the fakes reachable and
	// running there too; otherwise namedHostBackend refuses them before the
	// resolver below is ever consulted.
	stubProbedHosts(t, a, b)
	prevFn := hostBackendFn
	hostBackendFn = func(host string) (Backend, error) {
		if be, ok := byHost[host]; ok {
			return be, nil
		}
		return nil, fmt.Errorf("host %q not available", host)
	}
	prev := defaultBackend()
	setDefaultBackend(ba)
	t.Cleanup(func() { hostBackendFn = prevFn; setDefaultBackend(prev) })
	return ba, bb
}

// stubProbedHosts declares aliases as reachable, running, protocol-compatible
// hosts in the probe store for the duration of a test.
func stubProbedHosts(t *testing.T, aliases ...string) {
	t.Helper()
	_, proto := localProtocol()
	hostStore.mu.Lock()
	prevEntries, prevOrder := hostStore.entries, hostStore.order
	hostStore.entries = map[string]HostInfo{}
	hostStore.order = nil
	for _, a := range aliases {
		hostStore.entries[a] = HostInfo{
			Alias: a, Hostname: a, Reachable: true, Running: true,
			Protocol: proto, Compatible: true, Socket: "/tmp/" + a + ".sock",
		}
		hostStore.order = append(hostStore.order, a)
	}
	hostStore.mu.Unlock()
	t.Cleanup(func() {
		hostStore.mu.Lock()
		hostStore.entries, hostStore.order = prevEntries, prevOrder
		hostStore.mu.Unlock()
	})
}

// The point of the whole change: two feeds, two hosts, each carrying its own
// host's state. A frame from one must never describe the other.
func TestFeedsAreIndependentPerHost(t *testing.T) {
	local, norm := stubTwoHosts(t, "local", "norm")
	h := newHub()

	fl, err := h.feed("local")
	if err != nil {
		t.Fatalf("feed(local): %v", err)
	}
	fn, err := h.feed("norm")
	if err != nil {
		t.Fatalf("feed(norm): %v", err)
	}
	if fl == fn {
		t.Fatal("both hosts share one feed")
	}
	t.Cleanup(func() { h.stopFeedIfIdle(fl); h.stopFeedIfIdle(fn) })

	waitFor(t, func() bool { return fl.snapshot().Cwd == "/work/local" })
	waitFor(t, func() bool { return fn.snapshot().Cwd == "/work/norm" })

	if got := fl.snapshot().Host; got != "local" {
		t.Errorf("local feed Host = %q, want local", got)
	}
	if got := fn.snapshot().Host; got != "norm" {
		t.Errorf("norm feed Host = %q, want norm", got)
	}
	if local.calls.Load() == 0 || norm.calls.Load() == 0 {
		t.Errorf("polls: local=%d norm=%d, want both hosts polled", local.calls.Load(), norm.calls.Load())
	}
}

// The pane.list cache is keyed by host. Sharing one entry would serve titan's
// panes under norm's name the moment two tabs polled close together, and
// invalidating on one host's herdr event would throw away the other's snapshot.
func TestPaneListCacheIsPerHost(t *testing.T) {
	a, b := &paneHostBackend{host: "hostA"}, &paneHostBackend{host: "hostB"}

	ra, err := herdrPaneList(a)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := herdrPaneList(b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ra), "hostB") || strings.Contains(string(rb), "hostA") {
		t.Fatalf("cache crossed hosts: A=%s B=%s", ra, rb)
	}
	// Both are warm: a second read inside the TTL hits neither herdr.
	_, _ = herdrPaneList(a)
	_, _ = herdrPaneList(b)
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Fatalf("calls A=%d B=%d, want 1 each (cached)", a.calls.Load(), b.calls.Load())
	}
	// An event on A drops only A's entry.
	invalidatePaneList("hostA")
	_, _ = herdrPaneList(a)
	_, _ = herdrPaneList(b)
	if got := a.calls.Load(); got != 2 {
		t.Errorf("hostA calls = %d, want 2 after its own invalidation", got)
	}
	if got := b.calls.Load(); got != 1 {
		t.Errorf("hostB calls = %d, want 1 — one host's event must not drop another's cache", got)
	}
}

// A watched host is in use, so its pooled connection and terminals survive
// reaping. This used to be "is it the active host?", which had one answer and
// would have reaped a second tab's host out from under it.
func TestHostInUseCoversWatchedHosts(t *testing.T) {
	stubTwoHosts(t, "local", "norm")
	h := newHub()
	prevHub := srvHub
	srvHub = h
	t.Cleanup(func() { srvHub = prevHub })

	if hostInUse("norm") {
		t.Fatal("an unwatched, non-default host reported in use")
	}
	f, _, unwatch, err := h.watch("norm")
	if err != nil {
		t.Fatalf("watch(norm): %v", err)
	}
	if !hostInUse("norm") {
		t.Error("a watched host was not in use — its connection could be reaped under the tab watching it")
	}
	if !hostInUse("local") {
		t.Error("the default host was not in use")
	}
	unwatch()
	h.stopFeedIfIdle(f)
	if hostInUse("norm") {
		t.Error("a host stayed in use after its last watcher left and its feed stopped")
	}
}

// The DEFAULT host's feed polls for the life of the server: hub.run warms it so
// a fresh tab's first paint is not cold, and the idle timer must not undo that.
func TestDefaultHostFeedIsNeverIdleStopped(t *testing.T) {
	stubTwoHosts(t, "local", "norm")
	h := newHub()
	f, err := h.feed("local")
	if err != nil {
		t.Fatal(err)
	}
	h.scheduleFeedIdle(f)
	h.mu.RLock()
	armed := f.idleTimer != nil
	h.mu.RUnlock()
	if armed {
		t.Error("the default host's feed was armed to stop when idle")
	}
	// Even asked directly, it survives — nothing else in the process guarantees
	// a fresh tab has a warm host to land on.
	h.stopFeedIfIdle(f)
	if got, _ := h.feed("local"); got != f {
		t.Error("the default host's feed was stopped and rebuilt")
	}
}

// A feed outlives its last watcher by feedIdle, and a watcher returning inside
// that window keeps it warm rather than paying a fresh pane.list.
func TestFeedSurvivesAReconnect(t *testing.T) {
	stubTwoHosts(t, "local", "norm")
	h := newHub()
	f1, _, unwatch, err := h.watch("norm")
	if err != nil {
		t.Fatal(err)
	}
	unwatch()
	f2, _, unwatch2, err := h.watch("norm")
	if err != nil {
		t.Fatal(err)
	}
	defer unwatch2()
	if f1 != f2 {
		t.Error("reconnecting within feedIdle rebuilt the feed instead of reusing it")
	}
}

// The SSE stream is addressed by ?host= — an EventSource cannot send a header —
// so two tabs on one origin get two different hosts' frames.
func TestServeSSERoutesByHostParam(t *testing.T) {
	stubTwoHosts(t, "local", "norm")
	h := newHub()
	srv := httptest.NewServer(http.HandlerFunc(h.serveSSE))
	t.Cleanup(srv.Close)

	for _, want := range []string{"local", "norm"} {
		resp, err := http.Get(srv.URL + "?host=" + want)
		if err != nil {
			t.Fatalf("GET ?host=%s: %v", want, err)
		}
		got := readFrameHost(t, resp.Body, want)
		resp.Body.Close()
		if got != want {
			t.Errorf("stream for ?host=%s carried host %q", want, got)
		}
	}
}

// readFrameHost reads "active" frames until one names want, or the deadline
// passes. The priming frame can be the feed's seeded state, so it reads on.
func readFrameHost(t *testing.T, body interface{ Read([]byte) (int, error) }, want string) string {
	t.Helper()
	r := bufio.NewReader(body)
	deadline := time.Now().Add(3 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			return last
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var a Active
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &a) != nil {
			continue
		}
		last = a.Host
		if a.Host == want {
			return a.Host
		}
	}
	return last
}

// requestHost: the query string wins over the header, so a hand-typed or
// deep-linked ?host= is not overridden by the page's fetch wrapper.
func TestRequestHostPrecedence(t *testing.T) {
	cases := []struct {
		name, url, header, want string
	}{
		{"neither", "/api/panes", "", ""},
		{"header only", "/api/panes", "norm", "norm"},
		{"query only", "/api/panes?host=titan", "", "titan"},
		{"query beats header", "/api/panes?host=titan", "norm", "titan"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.url, nil)
			if c.header != "" {
				req.Header.Set(hostHeader, c.header)
			}
			if got := requestHost(req); got != c.want {
				t.Errorf("requestHost = %q, want %q", got, c.want)
			}
		})
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never held")
}
