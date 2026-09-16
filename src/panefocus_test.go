package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gatedPaneBackend answers pane.list. The first call blocks until released (so a
// test can act while it is in flight) or sleeps `slow` (so a test can watch a
// snapshot age while the call is still outstanding).
type gatedPaneBackend struct {
	Backend
	host    string
	slow    time.Duration
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (b *gatedPaneBackend) Name() string      { return b.host }
func (b *gatedPaneBackend) HerdrSock() string { return "" }

func (b *gatedPaneBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	if method != "pane.list" {
		return json.RawMessage(`{}`), nil
	}
	n := b.calls.Add(1)
	if n == 1 {
		if b.started != nil {
			close(b.started)
		}
		if b.release != nil {
			<-b.release
		}
		time.Sleep(b.slow)
	}
	return json.RawMessage(fmt.Sprintf(`{"panes":[],"call":%d}`, n)), nil
}

// The TTL has to run from when the snapshot was TAKEN, not from when it landed.
// pane.list costs 0.5-1.5s on a busy session, so stamping the reply made a
// snapshot of the session as it was seconds ago read as fresh — which is how the
// browser's post-create lookup got a pane listing from before its agent existed
// and gave up on navigating to it.
func TestPaneListTTLRunsFromWhenTheSnapshotWasTaken(t *testing.T) {
	be := &gatedPaneBackend{host: "slow-panelist", slow: paneListTTL + 100*time.Millisecond}
	if _, err := herdrPaneList(be); err != nil {
		t.Fatalf("herdrPaneList: %v", err)
	}
	if _, err := herdrPaneList(be); err != nil {
		t.Fatalf("herdrPaneList: %v", err)
	}
	if got := be.calls.Load(); got != 2 {
		t.Fatalf("pane.list calls = %d, want 2 — a snapshot older than the TTL was served as fresh because the TTL was timed from the reply", got)
	}
}

// An invalidation that arrives DURING a pane.list must be recorded immediately.
// Waiting for the call to finish (which taking the entry's lock would mean) puts
// the stamp after the snapshot's, so the listing that could not contain the new
// pane is served as fresh for a full TTL.
func TestPaneListInvalidationDoesNotWaitOnAnInFlightCall(t *testing.T) {
	be := &gatedPaneBackend{
		host:    "inflight-panelist",
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		_, _ = herdrPaneList(be)
	}()
	<-be.started

	invalidated := make(chan struct{})
	go func() {
		invalidatePaneList(be.Name())
		close(invalidated)
	}()
	select {
	case <-invalidated:
	case <-time.After(2 * time.Second):
		close(be.release) // don't wedge the rest of the package
		<-polled
		t.Fatal("invalidatePaneList blocked until the in-flight pane.list returned — its stamp lands after that stale snapshot's, so the snapshot is served as fresh")
	}
	close(be.release)
	<-polled

	if _, err := herdrPaneList(be); err != nil {
		t.Fatalf("herdrPaneList: %v", err)
	}
	if got := be.calls.Load(); got != 2 {
		t.Fatalf("pane.list calls = %d, want 2 — the snapshot predates the invalidation and must not be served", got)
	}
}

// resetPaneListCache drops every host's cached pane.list.
//
// The cache is process-global and keyed by host NAME, and it stores errors as
// well as snapshots. The two tests above dodge that by inventing their own host
// names, but TestCreateAgentInvalidatesPaneList cannot: createAgent invalidates
// by its backend's own name, so the test has to share "local" with every other
// test in the package. A transport failure cached under that name by an earlier
// test was therefore served straight back here, inside the 400ms TTL — which is
// what made it fail roughly one run in four with "dial unix: missing address",
// a dial this test never makes.
func resetPaneListCache() {
	paneListCache.mu.Lock()
	defer paneListCache.mu.Unlock()
	paneListCache.byHost = nil
}

// createAgent must drop its host's pane.list cache: the browser's very next move
// is to look the new pane up so it can focus it.
func TestCreateAgentInvalidatesPaneList(t *testing.T) {
	// Start cold, and leave cold: this test shares the "local" cache slot with
	// the rest of the package in both directions.
	resetPaneListCache()
	t.Cleanup(resetPaneListCache)

	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)

	b := &createAgentBackend{memBackend: newMemBackend()}
	prev := defaultBackend()
	setDefaultBackend(b)
	t.Cleanup(func() { setDefaultBackend(prev) })

	// Warm the cache for this host with a snapshot that predates the create.
	counter := &gatedPaneBackend{host: b.Name()}
	if _, err := herdrPaneList(counter); err != nil {
		t.Fatalf("warm herdrPaneList: %v", err)
	}
	if _, err := createAgent(b, createAgentReq{
		Type: "scratch", Title: "land on me", Prompt: "go", Agent: "claude",
	}); err != nil {
		t.Fatalf("createAgent: %v", err)
	}
	if _, err := herdrPaneList(counter); err != nil {
		t.Fatalf("herdrPaneList: %v", err)
	}
	if got := counter.calls.Load(); got != 2 {
		t.Fatalf("pane.list calls = %d, want 2 — the post-create lookup was served a snapshot taken before the workspace existed", got)
	}
}

// focusBackend records which herdr methods a focus lands on, failing the ones
// named in fail so a pane herdr does not know can be exercised.
type focusBackend struct {
	Backend
	methods []string
	fail    map[string]bool
}

func (b *focusBackend) Name() string      { return "local" }
func (b *focusBackend) HerdrSock() string { return "" }

func (b *focusBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	b.methods = append(b.methods, method)
	if b.fail[method] {
		return nil, fmt.Errorf("%s: pane_not_found", method)
	}
	return json.RawMessage(`{}`), nil
}

func focusPost(t *testing.T, b *focusBackend, body string) *httptest.ResponseRecorder {
	t.Helper()
	prev := defaultBackend()
	setDefaultBackend(b)
	t.Cleanup(func() { setDefaultBackend(prev) })
	rec := httptest.NewRecorder()
	serveFocus(rec, httptest.NewRequest(http.MethodPost, "/api/focus", strings.NewReader(body)))
	return rec
}

// A pane id must reach pane.focus, and nothing else: focusing the workspace and
// tab instead lands on whichever pane a SPLIT tab had active, which is the
// sibling of the one that was asked for.
func TestFocusWithPaneIDUsesPaneFocus(t *testing.T) {
	b := &focusBackend{}
	rec := focusPost(t, b, `{"workspace_id":"ws1","tab_id":"t1","pane_id":"ws1:p2"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(b.methods) != 1 || b.methods[0] != "pane.focus" {
		t.Fatalf("herdr calls = %v, want exactly [pane.focus] — a workspace/tab focus lands on the split tab's other pane", b.methods)
	}
}

// A pane that closed between the listing and the click must still land the user
// somewhere: with a workspace beside it, the focus falls back to it rather than
// answering 502 and leaving the user where they were.
func TestFocusFallsBackToWorkspaceWhenPaneIsGone(t *testing.T) {
	b := &focusBackend{fail: map[string]bool{"pane.focus": true}}
	rec := focusPost(t, b, `{"workspace_id":"ws1","tab_id":"t1","pane_id":"ws1:p9"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	want := []string{"pane.focus", "workspace.focus", "tab.focus"}
	if !slices.Equal(b.methods, want) {
		t.Fatalf("herdr calls = %v, want %v", b.methods, want)
	}
}

// A pane id ALONE is the strict form: the creator retries it while herdr
// materializes the pane, so a failure has to be reported rather than silently
// landing the user on some other pane.
func TestFocusWithPaneIDOnlyReportsFailure(t *testing.T) {
	b := &focusBackend{fail: map[string]bool{"pane.focus": true}}
	rec := focusPost(t, b, `{"pane_id":"ws1:p9"}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 — the creator's retry needs the failure", rec.Code)
	}
	if len(b.methods) != 1 || b.methods[0] != "pane.focus" {
		t.Fatalf("herdr calls = %v, want exactly [pane.focus]", b.methods)
	}
}

// A caller that knows only the workspace — the creator falling back when the new
// pane hasn't surfaced in pane.list yet — must still land on the new agent.
// Refusing the whole request (the old "workspace_id and tab_id required") left
// the user wherever they were, which is the bug the fallback exists to avoid.
func TestFocusWithoutTabIDFocusesTheWorkspace(t *testing.T) {
	b := &focusBackend{}
	rec := focusPost(t, b, `{"workspace_id":"ws1"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(b.methods) != 1 || b.methods[0] != "workspace.focus" {
		t.Fatalf("herdr calls = %v, want exactly [workspace.focus]", b.methods)
	}

	// Naming neither a pane nor a workspace is still a 400: nothing to focus.
	rec = focusPost(t, &focusBackend{}, `{"tab_id":"t1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status without workspace_id or pane_id = %d, want 400", rec.Code)
	}
}
