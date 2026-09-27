package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// namedFSBackend is a real local filesystem answering under another host's
// name, so a test can stand a remote host up without an ssh master.
type namedFSBackend struct {
	Backend
	name string
}

func (b *namedFSBackend) Name() string { return b.name }

// captureOpenFile replaces the fan-out with a recorder that reports `tabs`
// deliveries, and returns what it was handed.
func captureOpenFile(t *testing.T, tabs int) *[]openFileEvent {
	t.Helper()
	var got []openFileEvent
	prev := openFileBroadcast
	openFileBroadcast = func(ev openFileEvent) int {
		got = append(got, ev)
		return tabs
	}
	t.Cleanup(func() { openFileBroadcast = prev })
	return &got
}

// stubOpenFileBackends routes the tool's file checks to the given backends by
// host, recording which hosts were asked for.
func stubOpenFileBackends(t *testing.T, backends map[string]Backend) *[]string {
	t.Helper()
	var asked []string
	prev := openFileBackend
	openFileBackend = func(host string) (Backend, error) {
		asked = append(asked, host)
		if b, ok := backends[host]; ok {
			return b, nil
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { openFileBackend = prev })
	return &asked
}

func writeTemp(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The server cannot see the caller's working directory, so a relative path
// would be resolved against lasso's own — refused, naming both ways out.
func TestOpenFileRefusesARelativePath(t *testing.T) {
	captureOpenFile(t, 1)
	for _, p := range []string{"notes.md", "./docs/plan.md", "../x"} {
		_, _, err := openFileTool(context.Background(), nil, openFileIn{Path: p})
		if err == nil {
			t.Fatalf("%q: a relative path must be refused", p)
		}
		if !strings.Contains(err.Error(), "absolute") || !strings.Contains(err.Error(), "lasso open") {
			t.Errorf("%q: error %q should say to pass an absolute path or use lasso open", p, err)
		}
	}
	if _, _, err := openFileTool(context.Background(), nil, openFileIn{Path: "  "}); err == nil {
		t.Error("an empty path must be refused")
	}
}

// A path that is not there is an error, and nothing is pushed: an agent that
// forgot to write the file must hear about it, not have the human open a 404.
func TestOpenFileMissingFileIsAnError(t *testing.T) {
	got := captureOpenFile(t, 1)
	swapBackend(t, &localBackend{})
	missing := filepath.Join(t.TempDir(), "nope.md")
	_, _, err := openFileTool(context.Background(), nil, openFileIn{Path: missing})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want a does-not-exist error", err)
	}
	if len(*got) != 0 {
		t.Errorf("pushed %d events for a missing file", len(*got))
	}
}

// ~ expands against the TARGET host's home — here the default backend, whose
// home is a sentinel directory — and the event carries the resolved path.
func TestOpenFileExpandsTildeOnTheTargetHost(t *testing.T) {
	got := captureOpenFile(t, 1)
	home := t.TempDir()
	want := writeTemp(t, home, "plan.md", "# plan\n")
	swapBackend(t, &fakeHomeBackend{Backend: &localBackend{}, home: home})

	_, out, err := openFileTool(context.Background(), nil, openFileIn{Path: "~/plan.md", Line: 12})
	if err != nil {
		t.Fatal(err)
	}
	if out.Path != want || out.Host != "local" || out.Delivered != 1 || out.Dir {
		t.Fatalf("out = %+v, want path %q on local", out, want)
	}
	if len(*got) != 1 {
		t.Fatalf("pushed %d events, want 1", len(*got))
	}
	ev := (*got)[0]
	if ev.Path != want || ev.Host != "local" || ev.Line != 12 || ev.From != "an agent" || ev.Dir {
		t.Errorf("event = %+v", ev)
	}
}

// With no host, an identified caller's file is looked for on ITS OWN host —
// not on the lasso box, where "the doc I just wrote" does not exist.
func TestOpenFileDefaultsToTheCallersHost(t *testing.T) {
	got := captureOpenFile(t, 2)
	stubSSHHosts(t, "gigachad")
	dir := t.TempDir()
	doc := writeTemp(t, dir, "design.md", "x")
	asked := stubOpenFileBackends(t, map[string]Backend{
		"gigachad": &namedFSBackend{Backend: &localBackend{}, name: "gigachad"},
	})

	_, out, err := openFileTool(context.Background(), callerReq("c-gig", "gigachad", scopeSelf), openFileIn{Path: doc})
	if err != nil {
		t.Fatal(err)
	}
	if len(*asked) != 1 || (*asked)[0] != "gigachad" {
		t.Errorf("backends asked = %v, want only the caller's own host", *asked)
	}
	if out.Host != "gigachad" || out.Delivered != 2 || (*got)[0].Host != "gigachad" {
		t.Errorf("out = %+v, event = %+v", out, *got)
	}
}

// An unidentified caller (open /mcp, the CLI with no token) defaults to local.
func TestOpenFileUnidentifiedCallerDefaultsToLocal(t *testing.T) {
	captureOpenFile(t, 1)
	doc := writeTemp(t, t.TempDir(), "a.txt", "x")
	asked := stubOpenFileBackends(t, map[string]Backend{"local": &localBackend{}})
	if _, _, err := openFileTool(context.Background(), nil, openFileIn{Path: doc}); err != nil {
		t.Fatal(err)
	}
	if len(*asked) != 1 || (*asked)[0] != "local" {
		t.Errorf("backends asked = %v, want local", *asked)
	}
}

// A contained caller may not reach into another host's filesystem through the
// human's screen: the scope gate runs before any backend is consulted.
func TestOpenFileRespectsCallerScope(t *testing.T) {
	got := captureOpenFile(t, 1)
	stubSSHHosts(t, "gigachad")
	asked := stubOpenFileBackends(t, map[string]Backend{"local": &localBackend{}})
	doc := writeTemp(t, t.TempDir(), "secret.txt", "x")

	_, _, err := openFileTool(context.Background(), callerReq("c-gig", "gigachad", scopeSelf),
		openFileIn{Path: doc, Host: "local"})
	if err == nil || !strings.Contains(err.Error(), "may not address") {
		t.Fatalf("err = %v, want a scope refusal", err)
	}
	if len(*asked) != 0 || len(*got) != 0 {
		t.Errorf("refused call reached a backend (%v) or pushed (%d)", *asked, len(*got))
	}

	// And a host with no ssh alias at all is not addressable by anyone.
	if _, _, err := openFileTool(context.Background(), nil, openFileIn{Path: doc, Host: "no-such-host-xyz"}); err == nil {
		t.Error("an unaddressable host must be refused")
	}
}

// A directory opens the tree there; a line number means nothing for one.
func TestOpenFileDirectoryOpensTheTree(t *testing.T) {
	got := captureOpenFile(t, 1)
	swapBackend(t, &localBackend{})
	dir := t.TempDir()
	_, out, err := openFileTool(context.Background(), nil, openFileIn{Path: dir + "/", Line: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Dir || out.Path != dir {
		t.Errorf("out = %+v, want dir %q", out, dir)
	}
	if ev := (*got)[0]; !ev.Dir || ev.Line != 0 {
		t.Errorf("event = %+v, want dir with no line", ev)
	}
}

// delivered:0 is never success: with no tab connected the reply says so.
func TestOpenFileNoTabIsNotSuccess(t *testing.T) {
	prev := srvHub
	srvHub = newHub()
	t.Cleanup(func() { srvHub = prev })
	swapBackend(t, &localBackend{})
	doc := writeTemp(t, t.TempDir(), "a.md", "x")

	_, out, err := openFileTool(context.Background(), nil, openFileIn{Path: doc})
	if err != nil {
		t.Fatal(err)
	}
	if out.Delivered != 0 || out.Detail != openFileNobody {
		t.Errorf("out = %+v, want delivered 0 with the nobody detail", out)
	}

	// No hub at all (a CLI path, a test) is the same answer, not a panic.
	srvHub = nil
	if _, out, _ := openFileTool(context.Background(), nil, openFileIn{Path: doc}); out.Delivered != 0 {
		t.Errorf("delivered = %d with no hub", out.Delivered)
	}
}

// The real fan-out: every connected tab gets the event exactly once, under the
// `open-file` name, and delivered counts them.
func TestOpenFileFansOutToEveryTab(t *testing.T) {
	prev := srvHub
	srvHub = newHub()
	t.Cleanup(func() { srvHub = prev })
	a, unsubA := srvHub.subscribeEvents()
	defer unsubA()
	b, unsubB := srvHub.subscribeEvents()
	defer unsubB()
	c, unsubC := srvHub.subscribeEvents()
	unsubC() // a tab that has gone away is not counted
	swapBackend(t, &localBackend{})
	doc := writeTemp(t, t.TempDir(), "review.go", "package x\n")

	_, out, err := openFileTool(context.Background(), nil, openFileIn{Path: doc, Line: 3})
	if err != nil {
		t.Fatal(err)
	}
	if out.Delivered != 2 {
		t.Fatalf("delivered = %d, want 2", out.Delivered)
	}
	for i, ch := range []chan sseEvent{a, b} {
		select {
		case ev := <-ch:
			if ev.name != "open-file" {
				t.Errorf("tab %d: event name %q", i, ev.name)
			}
			raw, _ := json.Marshal(ev.data)
			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["path"] != doc || payload["host"] != "local" || payload["line"] != float64(3) || payload["from"] != "an agent" {
				t.Errorf("tab %d: payload %s", i, raw)
			}
			if _, ok := payload["dir"]; ok {
				t.Errorf("tab %d: a file's payload should omit dir: %s", i, raw)
			}
		default:
			t.Errorf("tab %d got nothing", i)
		}
	}
	select {
	case <-c:
		t.Error("an unsubscribed tab still received the event")
	default:
	}
}

// pane_id names the sender, exactly as whoami would; an unknown pane still
// opens the file and says why it could not be attributed.
func TestOpenFileAttributesToTheCallingAgent(t *testing.T) {
	openTestDB(t)
	if err := appendAgent("local", AgentRecord{ID: "a1", Type: "git",
		Title: "Write the design doc", RootPane: "w1F:p1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stubCloseBackends(t, map[string]Backend{
		"local": newCloseBackend("local", map[string]string{"w1F:p1": "w1F:p1"}),
	})
	stubPeers(t, nil, nil)
	got := captureOpenFile(t, 1)
	stubOpenFileBackends(t, map[string]Backend{"local": &localBackend{}})
	doc := writeTemp(t, t.TempDir(), "design.md", "x")

	_, out, err := openFileTool(context.Background(), nil, openFileIn{Path: doc, PaneID: "w1F:p1"})
	if err != nil {
		t.Fatal(err)
	}
	if (*got)[0].From != "Write the design doc" || out.Detail != "" {
		t.Errorf("from = %q, detail = %q", (*got)[0].From, out.Detail)
	}

	_, out, err = openFileTool(context.Background(), nil, openFileIn{Path: doc, PaneID: "wZ:p9"})
	if err != nil {
		t.Fatal(err)
	}
	if (*got)[1].From != "an agent" || out.Delivered != 1 || !strings.Contains(out.Detail, "could not name you") {
		t.Errorf("from = %q, out = %+v", (*got)[1].From, out)
	}
}

// The CLI resolves a relative path against ITS working directory, leaves ~ for
// the target host, and takes flags on either side of the path.
func TestOpenCLIResolvesRelativePaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HERDR_PANE_ID", "w9:p2")

	in, err := parseOpenArgs([]string{"docs/plan.md", "-line", "40"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "docs/plan.md"); in.Path != want {
		t.Errorf("path = %q, want %q", in.Path, want)
	}
	if in.Line != 40 || in.PaneID != "w9:p2" {
		t.Errorf("in = %+v", in)
	}

	in, err = parseOpenArgs([]string{"-host", "gigachad", "~/notes.md"})
	if err != nil {
		t.Fatal(err)
	}
	if in.Path != "~/notes.md" || in.Host != "gigachad" {
		t.Errorf("in = %+v, want ~ passed through untouched", in)
	}

	if _, err := parseOpenArgs(nil); err == nil {
		t.Error("no path must be a usage error")
	}
	if _, err := parseOpenArgs([]string{"a", "b"}); err == nil {
		t.Error("two paths must be a usage error")
	}
}

// The whole CLI round trip over the real /mcp handler.
func TestCallOpenFileToolOverMCP(t *testing.T) {
	got := captureOpenFile(t, 1)
	swapBackend(t, &localBackend{})
	doc := writeTemp(t, t.TempDir(), "report.md", "x")
	srv := httptest.NewServer(withMCPAuth(newMCPHandler(), "", "", false))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := callOpenFileTool(ctx, srv.URL, http.DefaultClient, openFileIn{Path: doc, Line: 2})
	if err != nil {
		t.Fatalf("callOpenFileTool: %v", err)
	}
	if out.Delivered != 1 || out.Path != doc || len(*got) != 1 || (*got)[0].Line != 2 {
		t.Fatalf("out = %+v, events = %+v", out, *got)
	}

	_, err = callOpenFileTool(ctx, srv.URL, http.DefaultClient, openFileIn{Path: "relative.md"})
	if err == nil || !strings.Contains(err.Error(), "relative") {
		t.Errorf("err = %v, want the tool's relative-path refusal", err)
	}
}
