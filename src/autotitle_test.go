package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The titlers are coding-agent CLIs answering in prose habits, not an API with
// a schema: the instruction asks for a bare title and they mostly comply, but a
// lead-in, a pair of quotes or a markdown wrapper is still an answer. Throwing
// those turns away would fall through to the next CLI (or to a toast) over
// formatting we can strip.
func TestCleanTitle(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"bare", "Fix the login redirect\n", "Fix the login redirect"},
		{"quoted", `"Fix the login redirect"`, "Fix the login redirect"},
		{"smart quotes", "“Fix the login redirect”", "Fix the login redirect"},
		{"markdown", "**Fix the login redirect**", "Fix the login redirect"},
		{"backticks", "`Fix the login redirect`", "Fix the login redirect"},
		{"lead-in line", "Here's a short title:\n\nFix the login redirect\n", "Fix the login redirect"},
		{"title label", "Title: Fix the login redirect", "Fix the login redirect"},
		{"trailing period", "Fix the login redirect.", "Fix the login redirect"},
		{"trailing prose", "Fix the login redirect\n\nThis names the task.", "Fix the login redirect"},
		{"inner colon kept", "Login: fix the redirect", "Login: fix the redirect"},
		{"collapsed whitespace", "Fix   the  login\tredirect", "Fix the login redirect"},
		{"empty", "\n\n", ""},
		{"only a lead-in", "Here is the title:\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cleanTitle(c.raw); got != c.want {
				t.Errorf("cleanTitle(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// A title is a display name in a narrow sidebar column — past autoTitleMaxLen
// it's clipped anyway, which is the very problem auto-titling exists to fix. A
// model that ignores the word limit must not reintroduce it.
func TestCleanTitleCapsLengthAtAWordBoundary(t *testing.T) {
	const long = "Rename agent titles and panes based on the content of the original prompt when creating an agent"
	got := cleanTitle(long)
	if len(got) > autoTitleMaxLen {
		t.Errorf("title %q is %d chars, want <= %d", got, len(got), autoTitleMaxLen)
	}
	// Cut on a word boundary: the kept text is a whole-word prefix of the
	// original, not "…the origi".
	if !strings.HasPrefix(long, got) || !strings.HasPrefix(long[len(got):], " ") {
		t.Errorf("title = %q, want a whole-word prefix of %q", got, long)
	}
}

// The fallback chain is the feature's whole resilience story: claude, then
// codex, then opencode. A CLI that isn't installed (or isn't logged in) must
// hand off rather than end the attempt.
func TestGenerateAgentTitleFallsThroughToTheNextCLI(t *testing.T) {
	var tried []string
	titlerRunner = func(tl titler, _ string) (string, error) {
		tried = append(tried, tl.id)
		switch tl.id {
		case "claude":
			return "", errors.New("not installed")
		case "codex":
			return "\n\n", nil // ran, but said nothing usable
		default:
			return "Fix the login redirect\n", nil
		}
	}
	t.Cleanup(func() { titlerRunner = runTitler })

	got, err := generateAgentTitle("the login page redirects in a loop after SSO")
	if err != nil {
		t.Fatalf("generateAgentTitle: %v", err)
	}
	if got != "Fix the login redirect" {
		t.Errorf("title = %q, want the third CLI's answer", got)
	}
	if want := []string{"claude", "codex", "opencode"}; strings.Join(tried, ",") != strings.Join(want, ",") {
		t.Errorf("tried %v, want %v (in order)", tried, want)
	}
}

// When every CLI fails the error is what reaches the user's toast, so it has to
// say which part of the chain is missing — "claude isn't installed" and "claude
// isn't logged in" call for very different fixes.
func TestGenerateAgentTitleReportsEveryFailure(t *testing.T) {
	titlerRunner = func(tl titler, _ string) (string, error) {
		return "", errors.New("not installed")
	}
	t.Cleanup(func() { titlerRunner = runTitler })

	_, err := generateAgentTitle("do a thing")
	if err == nil {
		t.Fatal("generateAgentTitle succeeded with every CLI failing")
	}
	for _, tl := range titlers {
		if !strings.Contains(err.Error(), tl.id) {
			t.Errorf("error %q does not name %s", err, tl.id)
		}
	}
}

// The titlers are coding agents pointed at a prompt that tells one to do work.
// Without the guard rails in the instruction a capable CLI starts the task
// instead of naming it — and the prompt has to actually be in there.
func TestTitleInstructionCarriesThePromptAndItsGuardRails(t *testing.T) {
	in := titleInstruction("wire up the settings toggle")
	if !strings.Contains(in, "wire up the settings toggle") {
		t.Error("instruction does not carry the prompt")
	}
	for _, want := range []string{"Do NOT attempt the task", "title alone"} {
		if !strings.Contains(in, want) {
			t.Errorf("instruction is missing its %q guard rail:\n%s", want, in)
		}
	}
	long := strings.Repeat("x", titlePromptLimit*2)
	if got := len(titleInstruction(long)) - len(titleInstruction("")); got > titlePromptLimit {
		t.Errorf("prompt contributed %d chars, want it capped at %d", got, titlePromptLimit)
	}
}

// autoTitleAgent's job is a rename that reaches BOTH places an agent's name
// lives: the herdr workspace (what the agents sidebar shows) and the lasso
// record (the address list_agents/message_agent hand out). Renaming one and not
// the other is how an agent ends up unreachable by the name on screen.
//
// The herdr TAB is not one of those places — it's the user's own organization
// of the terminal, and retitling it puts a generated sentence across the top of
// their screen.
type renameFake struct {
	*memBackend
	renamed map[string]string
	calls   []string
}

func (b *renameFake) HerdrCall(method string, params any) (json.RawMessage, error) {
	b.calls = append(b.calls, method)
	p, _ := params.(map[string]any)
	if method == "workspace.rename" {
		b.renamed[p["workspace_id"].(string)] = p["label"].(string)
	}
	return json.RawMessage(`{"panes":[{"pane_id":"p1","tab_id":"t1"}]}`), nil
}

func TestAutoTitleAgentRenamesWorkspaceAndRecordButNotTheTab(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)
	titlerRunner = func(titler, string) (string, error) { return "Fix the SSO loop\n", nil }
	t.Cleanup(func() { titlerRunner = runTitler })

	rec := AgentRecord{
		ID: "a1", Host: "local", Title: "the login page redirects in a loop after SSO and",
		Type: "scratch", Agent: "claude", Description: "the login page redirects in a loop after SSO and nobody can sign in",
		WorkspaceID: "ws1", RootPane: "p1",
	}
	if err := appendAgent("local", rec); err != nil {
		t.Fatalf("appendAgent: %v", err)
	}

	b := &renameFake{memBackend: newMemBackend(), renamed: map[string]string{}}
	autoTitleAgent(b, "local", rec)

	if got := b.renamed["ws1"]; got != "Fix the SSO loop" {
		t.Errorf("workspace label = %q, want the generated title", got)
	}
	for _, m := range b.calls {
		if m == "tab.rename" {
			t.Error("auto-titling renamed the herdr tab — the tab is the user's, not the agent's")
		}
	}
	got, err := findAgentRecord("local", "a1")
	if err != nil {
		t.Fatalf("findAgentRecord: %v", err)
	}
	if got.Title != "Fix the SSO loop" {
		t.Errorf("record title = %q, want the generated title — list_agents would hand out a name the sidebar no longer shows", got.Title)
	}
}

// The toggle has to default ON with nothing stored — the setting is written
// only when someone opens Settings and flips it, so every existing install
// reads an unset key and must still get the feature.
func TestAutoTitleToggleDefaultsOnAndRoundTrips(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)

	get := func() bool {
		rec := httptest.NewRecorder()
		serveAutoTitle(rec, httptest.NewRequest(http.MethodGet, "/api/auto-title", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.Enabled
	}

	if !get() || !autoTitleEnabled() {
		t.Error("auto-titling is off with the setting unset, want on by default")
	}
	rec := httptest.NewRecorder()
	serveAutoTitle(rec, httptest.NewRequest(http.MethodPost, "/api/auto-title",
		strings.NewReader(`{"enabled":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if get() || autoTitleEnabled() {
		t.Error("auto-titling still on after being switched off")
	}
}

// createAgent has to actually reach auto-titling — the whole feature hangs off
// one conditional at the end of a long function, and a create whose title came
// from the prompt is the case the UI always produces.
func TestCreateAgentAutoTitlesAPromptDerivedTitle(t *testing.T) {
	called := newTitlerSpy(t, "Generated title")

	b := &createAgentBackend{memBackend: newMemBackend()}
	prev := defaultBackend()
	setDefaultBackend(b)
	t.Cleanup(func() { setDefaultBackend(prev) })

	if _, err := createAgent(b, createAgentReq{
		Type: "scratch", Prompt: "the login page redirects in a loop after SSO", NoFocus: true,
	}); err != nil {
		t.Fatalf("createAgent: %v", err)
	}
	select {
	case <-called:
	case <-time.After(10 * time.Second):
		t.Fatal("no titler CLI ran for an agent titled from its prompt")
	}
}

// An explicit title is the caller's choice, not a default to improve on: the
// MCP tool exposes one precisely for a machine-written prompt whose first line
// is useless, so overwriting it would discard the better name.
func TestCreateAgentLeavesAnExplicitTitleAlone(t *testing.T) {
	called := newTitlerSpy(t, "Generated title")

	b := &createAgentBackend{memBackend: newMemBackend()}
	prev := defaultBackend()
	setDefaultBackend(b)
	t.Cleanup(func() { setDefaultBackend(prev) })

	rec, err := createAgent(b, createAgentReq{
		Type: "scratch", Title: "Chosen name", Prompt: "a long machine-written prompt", NoFocus: true,
	})
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}
	if rec.Title != "Chosen name" {
		t.Errorf("title = %q, want the caller's", rec.Title)
	}
	select {
	case <-called:
		t.Error("a titler CLI ran for an agent whose title the caller chose")
	case <-time.After(250 * time.Millisecond):
	}
}

// newTitlerSpy opens a scratch state db and swaps in a titler that reports it
// ran on the returned channel — the shared setup of the two createAgent gate
// tests, which differ only in whether that channel should ever fire.
func newTitlerSpy(t *testing.T, title string) <-chan struct{} {
	t.Helper()
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)
	called := make(chan struct{}, len(titlers))
	titlerRunner = func(titler, string) (string, error) {
		called <- struct{}{}
		return title, nil
	}
	t.Cleanup(func() { titlerRunner = runTitler })
	return called
}

// A background failure's only route to the user is the SSE stream's "notice"
// frame — get the event name or the shape wrong and the toast silently never
// fires, which is indistinguishable from the failure never happening.
func TestHubNoticeReachesTheEventStream(t *testing.T) {
	// The stream is per host now, so it needs a host to resolve to. Its poll
	// against a herdr that isn't there just marks the feed down; the notice
	// fan-out under test is global and unaffected.
	setDefaultBackend(&localBackend{})
	t.Cleanup(func() { setDefaultBackend(nil) })
	h := newHub()
	srv := httptest.NewServer(http.HandlerFunc(h.serveSSE))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	// Wait for the priming "active" frame — proof the client is registered, so
	// the notice can't race the subscription.
	r := bufio.NewReader(resp.Body)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read priming frame: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			break
		}
	}

	h.notify(notice{Level: "error", Title: "Couldn't auto-title", Detail: "claude: not installed"})

	// The host feed pushes its own "active" frames on the same stream, so skip
	// past any that arrive before the notice rather than reading the first frame
	// and asserting on it.
	var event, data string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read notice frame: %v", err)
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			data = ""
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		}
		if event == "notice" && data != "" {
			break
		}
	}
	var got notice
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatalf("decode notice: %v (%s)", err, data)
	}
	if got.Level != "error" || got.Title != "Couldn't auto-title" || got.Detail != "claude: not installed" {
		t.Errorf("notice = %+v, want the one sent", got)
	}
}
