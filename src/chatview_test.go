package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// chatLog joins fixture lines the way the file stores them: one JSON record per
// line, newline-terminated. (Named for the log, not the transcript: the latter
// is the production type that says where a session is read from.)
func chatLog(lines ...string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

// The shapes here are copied from real omp transcripts: the tool call is a
// block inside an assistant message, and its result is a separate later record
// keyed by toolCallId.
const (
	recUser   = `{"type":"message","id":"u1","timestamp":"2026-09-13T03:43:10.000Z","message":{"role":"user","content":[{"type":"text","text":"make the tests pass"}]}}`
	recHeader = `{"type":"session","version":3,"id":"s1","cwd":"/w","title":"Fix the tests"}`
)

func TestParseChatTranscriptShapes(t *testing.T) {
	parsed := parseChatTranscript(chatLog(
		recHeader,
		recUser,
		`{"type":"message","id":"a1","timestamp":"2026-09-13T03:43:11.000Z","message":{"role":"assistant","model":"gpt-5","stopReason":"toolUse","contextSnapshot":{"promptTokens":1234},"content":[`+
			`{"type":"thinking","thinking":"**Considering the failing case**"},`+
			`{"type":"text","text":"Running the suite."},`+
			`{"type":"toolCall","id":"call_1","name":"bash","arguments":{"command":"go test ./...","i":"Running the test suite","timeout":300}}]}}`,
		// The result lands before the call in some sessions; it must still reach
		// the same card rather than reading as an orphan.
		`{"type":"message","id":"r1","timestamp":"2026-09-13T03:43:14.000Z","message":{"role":"toolResult","toolCallId":"call_1","toolName":"bash","isError":false,"details":"{'timeoutSeconds': 300, 'wallTimeMs': 117.5}","content":[{"type":"text","text":"ok  \tlasso\t1.2s\nPASS"}]}}`,
		`{"type":"message","id":"e1","timestamp":"2026-09-13T03:43:20.000Z","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"Done."}]}}`,
	), 0)

	if parsed.title != "Fix the tests" {
		t.Errorf("title = %q, want the session title", parsed.title)
	}
	if parsed.model != "gpt-5" || parsed.tokens != 1234 {
		t.Errorf("model/tokens = %q/%d, want gpt-5/1234", parsed.model, parsed.tokens)
	}
	if parsed.run {
		t.Error("run = true after a clean stop, want false")
	}

	var user, thinking, tool *chatItem
	for i := range parsed.items {
		switch {
		case parsed.items[i].Kind == "user":
			user = &parsed.items[i]
		case parsed.items[i].Thinking:
			thinking = &parsed.items[i]
		case parsed.items[i].Tool != nil:
			tool = &parsed.items[i]
		}
	}
	if user == nil || user.Text != "make the tests pass" {
		t.Fatalf("user item = %+v, want the prompt", user)
	}
	if thinking == nil || !strings.Contains(thinking.Text, "Considering") {
		t.Fatalf("thinking item = %+v, want the thinking block", thinking)
	}
	if tool == nil {
		t.Fatal("no tool card for the bash call")
	}
	// The model's own intent is the better label, and the command survives for
	// the expanded body.
	if tool.Tool.Subject != "Running the test suite" {
		t.Errorf("subject = %q, want the call intent", tool.Tool.Subject)
	}
	if tool.Tool.Command != "go test ./..." {
		t.Errorf("command = %q, want the raw command", tool.Tool.Command)
	}
	if tool.Tool.Title != "Bash" {
		t.Errorf("title = %q, want the card heading", tool.Tool.Title)
	}
	if tool.Tool.State != "completed" {
		t.Errorf("state = %q, want completed — the out-of-order result did not bind", tool.Tool.State)
	}
	if !strings.Contains(tool.Tool.Output, "PASS") {
		t.Errorf("output = %q, want the command output", tool.Tool.Output)
	}
	if tool.Tool.ResultLine != "2 lines" {
		t.Errorf("result line = %q, want a line count", tool.Tool.ResultLine)
	}
	if tool.Tool.DurationMS != 117 {
		t.Errorf("duration = %d, want 117ms read out of the details blob", tool.Tool.DurationMS)
	}
}

// A running card is the only thing that should light the working indicator when
// the turn never stopped.
func TestParseChatTranscriptRunning(t *testing.T) {
	parsed := parseChatTranscript(chatLog(
		recUser,
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"sleep 90"}}]}}`,
	), 0)
	if !parsed.run {
		t.Error("run = false with an unsettled tool call, want true")
	}
	if got := parsed.items[len(parsed.items)-1].Tool.State; got != "running" {
		t.Errorf("tool state = %q, want running", got)
	}
}

func TestParseChatTranscriptDiffs(t *testing.T) {
	// A write is excerpted to the head of the file plus a marker saying how much
	// was dropped, so a card can never become the file.
	content := strings.Join([]string{"1", "2", "3", "4", "5"}, "\n")
	write := `{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"c1","name":"write","arguments":{"path":"/home/x/proj/src/main.go","content":` +
		jsonString(content) + `}}]}}`
	parsed := parseChatTranscript(chatLog(write), 0)
	tool := parsed.items[0].Tool
	if tool.Subject != "…/proj/src/main.go" {
		t.Errorf("subject = %q, want the abbreviated path", tool.Subject)
	}
	if tool.Family != "write" {
		t.Errorf("family = %q, want write", tool.Family)
	}
	if got := len(tool.Diff); got != 5 {
		t.Fatalf("diff rows = %d, want one per line", got)
	}
	if tool.Diff[0].Kind != "add" {
		t.Errorf("diff row kind = %q, want add", tool.Diff[0].Kind)
	}

	// omp's older single-string patch: a bracketed header, then "PUT <range>:"
	// blocks each followed by the lines to write at that range.
	edit := `{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"c2","name":"edit","arguments":{"i":"Fixing the header","input":` +
		jsonString("[AGENTS.md#CB11]\nPUT 57.=58:\n+first new line\n+second new line\n") + `}}]}}`
	parsed = parseChatTranscript(chatLog(edit), 0)
	tool = parsed.items[0].Tool
	if tool.Subject != "AGENTS.md" {
		t.Errorf("subject = %q, want the path parsed out of the patch header", tool.Subject)
	}
	if len(tool.Diff) != 3 {
		t.Fatalf("diff = %+v, want the two added lines under their operation", tool.Diff)
	}
	if tool.Diff[0].Kind != "context" || tool.Diff[0].Text != "PUT 57.=58" {
		t.Errorf("first row = %+v, want the operation as context", tool.Diff[0])
	}
	if tool.Diff[1].Kind != "add" || tool.Diff[1].Text != "first new line" {
		t.Errorf("added row = %+v, want the body line without its marker", tool.Diff[1])
	}
}

// A long patch is excerpted, and says that it was.
func TestParseChatTranscriptCapsDiff(t *testing.T) {
	var body []string
	body = append(body, "[a/b.md#1]", "PUT 1.=40:")
	for i := 0; i < 40; i++ {
		body = append(body, "+line "+itoa(i))
	}
	edit := `{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"stop","content":[{"type":"toolCall","id":"c1","name":"edit","arguments":{"input":` +
		jsonString(strings.Join(body, "\n")) + `}}]}}`
	parsed := parseChatTranscript(chatLog(edit), 0)
	diff := parsed.items[0].Tool.Diff
	if len(diff) != chatDiffLines+1 {
		t.Fatalf("diff rows = %d, want %d kept plus the overflow marker", len(diff), chatDiffLines)
	}
	last := diff[len(diff)-1]
	if last.Kind != "context" || !strings.Contains(last.Text, "more lines") {
		t.Errorf("last row = %+v, want an overflow marker", last)
	}
}

// A provider-backed call is stored as "<call id>|<response item id>" but its
// result names only the call id, so matching on the raw id leaves every such
// card stuck at "running" — which is how the unfixed reader rendered a whole
// live session.
func TestParseChatTranscriptPipedCallIDs(t *testing.T) {
	parsed := parseChatTranscript(chatLog(
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"call_01_ET_abc|fc_0bb1cbfe","name":"bash","arguments":{"command":"ls","i":"Listing"}}]}}`,
		`{"type":"message","id":"r1","message":{"role":"toolResult","toolCallId":"call_01_ET_abc","toolName":"bash","content":[{"type":"text","text":"a.go\nb.go"}]}}`,
	), 0)
	tool := parsed.items[len(parsed.items)-1].Tool
	if tool.State != "completed" {
		t.Fatalf("state = %q, want completed — the piped id did not match its result", tool.State)
	}
	if !strings.Contains(tool.Output, "b.go") {
		t.Errorf("output = %q, want the result body", tool.Output)
	}
}

// omp writes a tool's details as a JSON object for some tools and a Python-repr
// string for others. A typed field threw the entire record away on the shape it
// did not expect, which dropped every tool result in a live session.
func TestParseChatTranscriptDetailsShapes(t *testing.T) {
	cases := []struct {
		name    string
		details string
		wantMS  int
	}{
		{"object", `{"timeoutSeconds":300,"wallTimeMs":117.5}`, 117},
		{"python repr string", `"{'timeoutSeconds': 300, 'wallTimeMs': 42.9}"`, 42},
		{"absent", ``, 0},
		{"unreadable", `"not a dict at all"`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			details := `"details":` + tc.details + `,`
			if tc.details == "" {
				details = ""
			}
			parsed := parseChatTranscript(chatLog(
				`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}]}}`,
				`{"type":"message","id":"r1","message":{"role":"toolResult","toolCallId":"c1",`+details+`"content":[{"type":"text","text":"ok"}]}}`,
			), 0)
			tool := parsed.items[len(parsed.items)-1].Tool
			if tool.State != "completed" {
				t.Fatalf("state = %q, want completed — record dropped over its details shape", tool.State)
			}
			if tool.DurationMS != tc.wantMS {
				t.Errorf("duration = %d, want %d", tool.DurationMS, tc.wantMS)
			}
		})
	}
}

// An errored call reports the failure itself, and marks the card.
func TestParseChatTranscriptErrorResult(t *testing.T) {
	parsed := parseChatTranscript(chatLog(
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"false"}}]}}`,
		`{"type":"message","id":"r1","message":{"role":"toolResult","toolCallId":"c1","isError":true,"content":[{"type":"text","text":"boom\nstack line"}]}}`,
	), 0)
	tool := parsed.items[len(parsed.items)-1].Tool
	if tool.State != "error" || tool.Error != "boom" {
		t.Errorf("state/error = %q/%q, want error with the first output line", tool.State, tool.Error)
	}
}

// The same failure repeated is one row, not two hundred.
func TestParseChatTranscriptCollapsesRepeatedMarkers(t *testing.T) {
	rec := func(id string) string {
		return `{"type":"message","id":"` + id + `","message":{"role":"assistant","stopReason":"error","errorMessage":"No API key for provider: anthropic","content":[]}}`
	}
	parsed := parseChatTranscript(chatLog(recUser, rec("e1"), rec("e2"), rec("e3")), 0)
	var markers []chatItem
	for _, it := range parsed.items {
		if it.Kind == "marker" {
			markers = append(markers, it)
		}
	}
	if len(markers) != 1 {
		t.Fatalf("markers = %d, want the run collapsed to one", len(markers))
	}
	if markers[0].Count != 3 {
		t.Errorf("count = %d, want 3", markers[0].Count)
	}
}

// An interruption is worth a row; omp's own silent-abort sentinel is not — that
// one is how a turn ends when the user simply stopped it.
func TestParseChatTranscriptAbortMarkers(t *testing.T) {
	loud := `{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"aborted","content":[]}}`
	silent := `{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"aborted","errorMessage":"__omp.silent_abort__","content":[]}}`

	parsed := parseChatTranscript(chatLog(loud), 0)
	if n := len(parsed.items); n != 1 || parsed.items[0].Marker != "interrupted" {
		t.Errorf("items = %+v, want one interrupted marker", parsed.items)
	}
	parsed = parseChatTranscript(chatLog(silent), 0)
	if len(parsed.items) != 0 {
		t.Errorf("items = %+v, want nothing for a silent abort", parsed.items)
	}
}

func TestParseChatTranscriptCapsHistory(t *testing.T) {
	lines := []string{recUser}
	for i := 0; i < chatMaxItems+20; i++ {
		lines = append(lines, `{"type":"message","id":"m`+itoa(i)+`","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"line `+itoa(i)+`"}]}}`)
	}
	parsed := parseChatTranscript(chatLog(lines...), 0)
	if len(parsed.items) != chatMaxItems {
		t.Errorf("items = %d, want the cap of %d", len(parsed.items), chatMaxItems)
	}
	if !parsed.more {
		t.Error("more = false after dropping history, want true")
	}
	// The newest rows are the ones kept.
	last := parsed.items[len(parsed.items)-1]
	if last.Text != "line 139" {
		t.Errorf("newest item = %q, want the last line", last.Text)
	}
}

// Only a pane that is actually running a harness whose transcript herdr named
// outright is chat-viewable.
func TestPaneTranscript(t *testing.T) {
	home := t.TempDir()
	// claude keeps its logs under the home it ran with, keyed by the directory
	// the session was started in.
	proj := filepath.Join(home, ".claude", "projects", claudeProjectSlug("/home/u/proj"))
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	const claudeID = "24a7c912-71da-4a1f-9d3b-000000000000"
	if err := os.WriteFile(filepath.Join(proj, claudeID+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	be := &chatFakeBackend{home: home}

	base := pane{
		PaneID: "w1:p1",
		Agent:  "omp",
		Cwd:    "/home/u/proj",
		AgentSession: &agentSession{
			Source: "herdr:omp", Agent: "omp", Kind: "path",
			Value: "/home/u/.omp/agent/sessions/-proj/2026-09-13T03-43-04-614Z_01a0.jsonl",
		},
	}
	if got := paneTranscript(be, base); got.Path == "" || got.Harness != "omp" {
		t.Errorf("omp path session = %+v, want the file and its reader", got)
	}

	// herdr keeps agent_session after the agent exits.
	exited := base
	exited.Agent = ""
	if got := paneTranscript(be, exited); got.Path != "" {
		t.Errorf("exited pane = %+v, want no transcript", got)
	}

	// Claude reports an ID. Its log is on disk, so the pane is readable —
	// refusing the id is what left these panes empty while their transcripts sat
	// there.
	byID := base
	byID.Agent = "claude"
	byID.AgentSession = &agentSession{Agent: "claude", Kind: "id", Value: claudeID}
	if got := paneTranscript(be, byID); got.Path == "" || got.Harness != "claude" {
		t.Errorf("claude id session = %+v, want the log it resolves to", got)
	}

	// An id whose log is not here says so, rather than claiming no session.
	missing := byID
	missing.AgentSession = &agentSession{Agent: "claude", Kind: "id", Value: "00000000-0000-4000-8000-000000000000"}
	if got := paneTranscript(be, missing); got.Path != "" || got.Note == "" {
		t.Errorf("unresolvable id = %+v, want a note", got)
	}

	// A harness that reports only an id and that lasso cannot resolve.
	unknown := base
	unknown.AgentSession = &agentSession{Agent: "codex", Kind: "id", Value: "abc"}
	if got := paneTranscript(be, unknown); got.Path != "" || got.Note == "" {
		t.Errorf("id-only session = %+v, want a note", got)
	}

	nosession := base
	nosession.AgentSession = nil
	if got := paneTranscript(be, nosession); got.Path != "" || got.Note == "" {
		t.Errorf("session-less pane = %+v, want a note", got)
	}

	// The value reaches a filesystem read, so anything but an absolute .jsonl
	// is refused rather than joined into a path.
	for _, bad := range []string{"../../etc/passwd", "/etc/passwd", "relative.jsonl", ""} {
		p := base
		p.AgentSession = &agentSession{Agent: "omp", Kind: "path", Value: bad}
		if got := paneTranscript(be, p); got.Path != "" {
			t.Errorf("value %q = %+v, want refused", bad, got)
		}
	}
}

// jsonString renders a Go string as a JSON string literal for a fixture.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for i > 0 {
		d = append([]byte{byte('0' + i%10)}, d...)
		i /= 10
	}
	return string(d)
}

// useFakeHost installs a fake host as the default backend AND clears the
// pane-list cache for its name. That cache is keyed by HOST and lives 400ms, so
// two fakes both claiming "local" hand the second test the first one's pane
// list — which points into a temp directory that has already been removed, and
// reads as "the transcript is not readable yet" in a test that never touched a
// transcript. panefocus_test.go guards the same hazard the same way.
func useFakeHost(t *testing.T, be Backend) {
	t.Helper()
	prev := defaultBackend()
	setDefaultBackend(be)
	invalidatePaneList(be.Name())
	t.Cleanup(func() {
		setDefaultBackend(prev)
		invalidatePaneList(be.Name())
	})
}

// fakeHostSeq numbers the chat fakes' private pane.list cache keys.
var fakeHostSeq atomic.Int64

// privateHostName is why these fakes are not called "local". That cache is keyed
// by host NAME, and the rest of this suite has several backends claiming
// "local" — including a goroutine a hostfeed test leaks past its own end. A fake
// sharing that key gets served the other backend's result: seen as a 502 here
// carrying "dial unix: missing address", intermittently and only in a full run.
// Nothing on these paths reads the name; only the cache key does.
func privateHostName(p *string, prefix string) string {
	if *p == "" {
		*p = fmt.Sprintf("%s-%d", prefix, fakeHostSeq.Add(1))
	}
	return *p
}

// chatFakeBackend stands up the herdr surface serveChat reads: pane.list
// carrying an omp agent_session, and a real filesystem for the transcript, so
// the Stat/Open path the handler uses is exercised rather than stubbed.
type chatFakeBackend struct {
	Backend
	// name is a per-instance cache key, assigned by useFakeHost.
	name  string
	panes []string // pane.list bodies, pre-encoded
	// home is what a harness that keeps its logs under $HOME resolves against
	// (claude's ~/.claude/projects). Empty for tests that never ask.
	home string
}

func (b *chatFakeBackend) Name() string { return privateHostName(&b.name, "chatfake") }

func (b *chatFakeBackend) HomeDir() (string, error) { return b.home, nil }

// ReadDir backs the fallback scan of ~/.claude/projects for an id whose project
// slug cannot be guessed from the pane's cwd.
func (b *chatFakeBackend) ReadDir(p string) ([]fileEntry, error) {
	ents, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]fileEntry, 0, len(ents))
	for _, e := range ents {
		out = append(out, fileEntry{Name: e.Name(), Dir: e.IsDir()})
	}
	return out, nil
}

func (b *chatFakeBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	if method != "pane.list" {
		return nil, fmt.Errorf("unexpected herdr method %q", method)
	}
	return json.RawMessage(`{"panes":[` + strings.Join(b.panes, ",") + `]}`), nil
}

func (b *chatFakeBackend) Stat(p string) (fs.FileInfo, error) { return os.Stat(p) }
func (b *chatFakeBackend) Open(p string) (io.ReadSeekCloser, error) {
	return os.Open(p)
}

// chatPane renders one pane.list entry with an omp path session.
func chatPane(id, path string, focused bool) string {
	return chatPaneStatus(id, path, focused, "idle")
}

// chatPaneStatus is chatPane with herdr's own agent_status spelled out, which is
// what the working indicator reads when the transcript cannot say.
func chatPaneStatus(id, path string, focused bool, status string) string {
	return fmt.Sprintf(
		`{"pane_id":%q,"focused":%t,"agent":"omp","agent_status":%q,"terminal_title_stripped":"Fix the tests",`+
			`"agent_session":{"source":"herdr:omp","agent":"omp","kind":"path","value":%q}}`,
		id, focused, status, path)
}

func TestServeChat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-09-13T03-43-04-614Z_abc.jsonl")
	body := chatLog(recHeader, recUser,
		`{"type":"message","id":"a1","message":{"role":"assistant","model":"gpt-5","stopReason":"stop","content":[{"type":"text","text":"All green."}]}}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	be := &chatFakeBackend{panes: []string{
		chatPane("w1:p1", path, false),
		chatPane("w1:p2", filepath.Join(dir, "missing.jsonl"), true),
	}}
	useFakeHost(t, be)

	get := func(query string) (*httptest.ResponseRecorder, chatPayload) {
		rec := httptest.NewRecorder()
		serveChat(rec, httptest.NewRequest(http.MethodGet, "/api/chat"+query, nil))
		var out chatPayload
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
		}
		return rec, out
	}

	// An explicit pane reads that pane's transcript.
	rec, out := get("?pane=w1:p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if out.PaneID != "w1:p1" || out.Agent != "omp" {
		t.Errorf("pane/agent = %q/%q, want w1:p1/omp", out.PaneID, out.Agent)
	}
	// The host rides with the rows: the composer addresses a submission back to
	// the machine they came from, and pane ids are unique per host only.
	if out.Host != be.Name() {
		t.Errorf("host = %q, want the resolved backend's name (%q)", out.Host, be.Name())
	}
	if out.Title != "Fix the tests" {
		t.Errorf("title = %q, want the session's own title", out.Title)
	}
	if out.Model != "gpt-5" {
		t.Errorf("model = %q, want the model off the assistant record", out.Model)
	}
	if len(out.Items) != 2 || out.Items[0].Kind != "user" || out.Items[1].Text != "All green." {
		t.Errorf("items = %+v, want the prompt and the reply", out.Items)
	}
	if out.Running {
		t.Error("running = true after a clean stop")
	}

	// With no pane named it follows the focused one, and a transcript that is
	// not there yet is a note, not an error — the panel is open on a pane whose
	// agent has not written anything.
	rec, out = get("")
	if rec.Code != http.StatusOK {
		t.Fatalf("focused status = %d, want 200", rec.Code)
	}
	if out.PaneID != "w1:p2" || out.Note == "" || len(out.Items) != 0 {
		t.Errorf("focused = %+v, want the focus pane with a note and no items", out)
	}

	rec, _ = get("?pane=nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown pane status = %d, want 404", rec.Code)
	}
}

// A pane running a harness lasso cannot read yet says so, rather than showing
// an empty conversation that looks like a broken view.
func TestServeChatUnreadableSession(t *testing.T) {
	be := &chatFakeBackend{panes: []string{
		`{"pane_id":"w1:p1","focused":true,"agent":"claude","agent_status":"idle",` +
			`"agent_session":{"source":"herdr:claude","agent":"claude","kind":"id","value":"24a7c912"}}`,
	}}
	useFakeHost(t, be)

	rec := httptest.NewRecorder()
	serveChat(rec, httptest.NewRequest(http.MethodGet, "/api/chat", nil))
	var out chatPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Note == "" {
		t.Error("note is empty for an id-only session, want an explanation")
	}
}

// A pane title carries the agent's live status glyphs, one of which is an
// animation frame — a header built from the raw title flickers and rewrites
// itself on every poll.
func TestCleanPaneTitle(t *testing.T) {
	cases := map[string]string{
		"π ⠴ Implement chat transcript backend": "Implement chat transcript backend",
		"✳ Check Norm outline wiki connection":  "Check Norm outline wiki connection",
		"Go 1.22 upgrade":                       "Go 1.22 upgrade",
		"dev@norm: ~/projects/norm":             "dev@norm: ~/projects/norm",
		"⠋ ⠙":                                   "",
		"":                                      "",
	}
	for in, want := range cases {
		if got := cleanPaneTitle(in); got != want {
			t.Errorf("cleanPaneTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// chatSendBackend emulates a pane's composer the way msgPaneBackend does: the
// pasted text is drawn into the harness's composer box, and Enter clears it.
// The knobs reproduce the two failures that matter — a pane that refuses the
// write, and one that swallows it without ever drawing it.
type chatSendBackend struct {
	Backend
	name   string // private pane.list cache key; see privateHostName
	paneID string
	agent  string
	screen string // the composer line as currently drawn
	// failSend stands in for a pane that never saw the bytes: the RPC fails.
	failSend bool
	// applyThenFail models the case the sender must not confuse with the one
	// above — herdrCallSock writes the request BEFORE reading the reply, so a
	// timeout or an unreadable answer can arrive after the pane has already
	// been typed into. The screen changes and THEN the call errors.
	applyThenFail bool
	// ignorePaste accepts the bytes and never draws them — the case where a
	// "sent" report would be a lie.
	ignorePaste bool
	writes      []string
}

func (b *chatSendBackend) Name() string { return privateHostName(&b.name, "chatsend") }

// ompComposer draws a real omp composer footer, which is what detectComposer
// parses: "╰─ <text> ─╯". An EMPTY composer is the bare footer — a wrapped body
// row ("│ … │") above it is what the detector reads as content, so it must not
// be drawn when there is none.
func (b *chatSendBackend) ompComposer(text string) string {
	if text == "" {
		return "some transcript\n╰─  ─╯"
	}
	return "some transcript\n╰─ " + text + " ─╯"
}

func (b *chatSendBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	p, _ := params.(map[string]any)
	switch method {
	case "pane.list":
		return json.RawMessage(fmt.Sprintf(
			`{"panes":[{"pane_id":%q,"focused":true,"agent":%q,"agent_status":"idle"}]}`,
			b.paneID, b.agent)), nil
	case "pane.read":
		return json.RawMessage(fmt.Sprintf(`{"read":{"text":%q}}`, b.screen)), nil
	case "pane.send_text":
		text, _ := p["text"].(string)
		if b.failSend {
			return nil, fmt.Errorf("pane is gone")
		}
		b.writes = append(b.writes, text)
		if text == "\r" {
			b.screen = b.ompComposer("")
		} else if !b.ignorePaste {
			b.screen = b.ompComposer(text)
		}
		if b.applyThenFail {
			return nil, fmt.Errorf("i/o timeout reading reply")
		}
		return json.RawMessage(`{}`), nil
	}
	return nil, fmt.Errorf("unexpected herdr method %q", method)
}

func withFastSubmit(t *testing.T) {
	t.Helper()
	paste, enter, poll := chatPasteWait, chatEnterWait, chatPollWait
	chatPasteWait, chatEnterWait, chatPollWait = 300*time.Millisecond, 400*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { chatPasteWait, chatEnterWait, chatPollWait = paste, enter, poll })
}

// A pasted message that appears in the composer and then clears it is the only
// thing this can call delivered — and it must be addressable without touching
// herdr's focus.
func TestChatSubmitConfirmed(t *testing.T) {
	withFastSubmit(t)
	b := &chatSendBackend{paneID: "w1:p1", agent: "omp"}
	b.screen = b.ompComposer("")
	if outcome, detail := chatSubmit(b, "w1:p1", "omp", "hello agent"); outcome != chatSent {
		t.Fatalf("outcome = %q (%s), want confirmed", outcome, detail)
	}
	if len(b.writes) < 2 || b.writes[0] != "hello agent" {
		t.Errorf("writes = %q, want the message then Enter", b.writes)
	}
}

// A pane holding a human's unsent input is refused before any byte is written.
func TestChatSubmitRefusesOverDraft(t *testing.T) {
	withFastSubmit(t)
	b := &chatSendBackend{paneID: "w1:p1", agent: "omp"}
	b.screen = b.ompComposer("half a thought the human is still typing")
	outcome, _ := chatSubmit(b, "w1:p1", "omp", "hello agent")
	if outcome != chatRefused {
		t.Fatalf("outcome = %q, want refused", outcome)
	}
	if len(b.writes) != 0 {
		t.Errorf("writes = %q, want none — a draft must not be clobbered", b.writes)
	}
}

// A send RPC that fails is NOT proof the pane never saw the bytes: the request
// goes out before the reply is read, so a timeout can hide a paste that landed.
// The only safe report is "may have been sent".
func TestChatSubmitTreatsSendErrorsAsUncertain(t *testing.T) {
	withFastSubmit(t)
	b := &chatSendBackend{paneID: "w1:p1", agent: "omp", failSend: true}
	b.screen = b.ompComposer("")
	outcome, detail := chatSubmit(b, "w1:p1", "omp", "hello agent")
	if outcome != chatUncertain {
		t.Fatalf("outcome = %q, want uncertain — a failed RPC is not a safe-to-retry answer", outcome)
	}
	if detail == "" {
		t.Error("uncertain outcome carries no detail")
	}
}

// The case that makes the rule above load-bearing: the pane APPLIES the paste
// and the call still errors (herdrCallSock's read timeout / decode failure).
// The message is really in the composer, so a "refused / nothing was written"
// answer would be a lie a human could act on by resending.
func TestChatSubmitNeverClaimsUnsentWhenPasteAppliedThenFailed(t *testing.T) {
	withFastSubmit(t)
	b := &chatSendBackend{paneID: "w1:p1", agent: "omp", applyThenFail: true}
	b.screen = b.ompComposer("")
	outcome, _ := chatSubmit(b, "w1:p1", "omp", "hello agent")
	if outcome != chatUncertain {
		t.Fatalf("outcome = %q, want uncertain", outcome)
	}
	// The delivery that the error hid is really there — this is what a wrong
	// "refused" would have caused to be sent twice.
	if !strings.Contains(b.screen, "hello agent") {
		t.Fatalf("fixture is not exercising the case: %q", b.screen)
	}
}

// Bytes accepted but never drawn are neither delivered nor safe to resend: the
// message may be sitting in the pane, and a retry would duplicate the turn.
// Crucially, Enter is NOT pressed in this case — an unconfirmed paste must not
// be followed by a stream of returns into someone's agent.
func TestChatSubmitUncertainWhenPasteNeverLands(t *testing.T) {
	withFastSubmit(t)
	b := &chatSendBackend{paneID: "w1:p1", agent: "omp", ignorePaste: true}
	b.screen = b.ompComposer("")
	outcome, detail := chatSubmit(b, "w1:p1", "omp", "hello agent")
	if outcome != chatUncertain {
		t.Fatalf("outcome = %q, want uncertain", outcome)
	}
	if detail == "" {
		t.Error("uncertain outcome carries no detail")
	}
	for _, wr := range b.writes {
		if wr == "\r" {
			t.Fatalf("writes = %q, want no Enter after a paste that never landed", b.writes)
		}
	}
}

// The endpoint takes the harness from herdr's own pane metadata, never from the
// caller, and refuses a pane this host does not have.
func TestServeChatSendPaneResolution(t *testing.T) {
	be := &chatSendBackend{paneID: "w1:p1", agent: "omp"}
	be.screen = be.ompComposer("")
	useFakeHost(t, be)
	withFastSubmit(t)

	post := func(body string) (*httptest.ResponseRecorder, map[string]string) {
		rec := httptest.NewRecorder()
		serveChatSend(rec, httptest.NewRequest(http.MethodPost, "/api/chat/send", strings.NewReader(body)))
		var out map[string]string
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
		}
		return rec, out
	}

	rec, out := post(`{"pane_id":"w1:p1","text":"hello"}`)
	if rec.Code != http.StatusOK || out["outcome"] != chatSent {
		t.Fatalf("status/outcome = %d/%q, want 200/confirmed", rec.Code, out["outcome"])
	}

	rec, _ = post(`{"pane_id":"w9:p9","text":"hello"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown pane status = %d, want 404", rec.Code)
	}

	rec, _ = post(`{"pane_id":"w1:p1","text":"   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("blank text status = %d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	serveChatSend(rec, httptest.NewRequest(http.MethodGet, "/api/chat/send", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
}

// Paging is the one thing here that can silently lose history, and it has two
// ways to: a cursor that names the window's start rather than the oldest row it
// actually returned skips everything the per-read cap dropped, and a wrong
// boundary repeats or drops a row. So: walk the transcript back to the
// beginning and assert the pages TILE it — every message exactly once, in
// order, ending at the newest.
func TestServeChatPaging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")
	var sb strings.Builder
	// Comfortably past chatReadBytes, so the first page is a real window rather
	// than the whole file.
	const n = 3000
	for i := 0; i < n; i++ {
		sb.WriteString(fmt.Sprintf(
			`{"type":"message","id":"m%06d","timestamp":"2026-09-13T03:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"message %06d %s"}]}}`,
			i, i, strings.Repeat("padding ", 12)))
		sb.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Size() < chatReadBytes {
		t.Fatalf("fixture is only %d bytes; it has to exceed the %d-byte window", fi.Size(), chatReadBytes)
	}

	be := &chatFakeBackend{panes: []string{chatPane("w1:p1", path, true)}}
	useFakeHost(t, be)

	get := func(query string) chatPayload {
		rec := httptest.NewRecorder()
		serveChat(rec, httptest.NewRequest(http.MethodGet, "/api/chat"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d for %q: %s", rec.Code, query, strings.TrimSpace(rec.Body.String()))
		}
		var out chatPayload
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	page := get("?pane=w1:p1")
	if !page.More || page.StartOffset <= 0 {
		t.Fatalf("tail page: more=%v start_offset=%d, want a windowed page", page.More, page.StartOffset)
	}

	// Walk back to the start of the transcript.
	seen := []int{}
	record := func(items []chatItem) {
		for _, it := range items {
			var idx int
			if _, err := fmt.Sscanf(it.Text, "message %06d", &idx); err == nil {
				seen = append(seen, idx)
			}
		}
	}
	record(page.Items)
	offset, pages := page.StartOffset, 1
	for offset > 0 {
		p := get(fmt.Sprintf("?pane=w1:p1&before=%d", offset))
		if len(p.Items) == 0 {
			t.Fatalf("page at before=%d came back empty", offset)
		}
		if p.StartOffset >= offset {
			t.Fatalf("page at before=%d reported start_offset=%d, want strictly earlier", offset, p.StartOffset)
		}
		record(p.Items)
		offset = p.StartOffset
		pages++
		if pages > 100 {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != n {
		t.Fatalf("pages covered %d messages over %d pages, want all %d", len(seen), pages, n)
	}
	// Each page arrives oldest-first, and each is older than the one before it,
	// so the union has to be exactly every message once — no repeat at a page
	// boundary, no hole where the per-read cap dropped rows.
	sort.Ints(seen)
	for i, idx := range seen {
		if idx != i {
			t.Fatalf("message %d of the union = %06d (repeat or gap)", i, idx)
		}
	}
	// And the page a view opens on ends at the newest message.
	if last := page.Items[len(page.Items)-1].Text; !strings.HasPrefix(last, "message 002999") {
		t.Errorf("tail page ends at %q, want the newest message", last)
	}

	// And the oldest page says there is nothing before it.
	first := get(fmt.Sprintf("?pane=w1:p1&before=%d", 1))
	if first.StartOffset != 0 {
		t.Errorf("first page start_offset = %d, want 0", first.StartOffset)
	}
}

// The fixture that catches what single-block records hide: an assistant turn is
// SEVERAL rows (thinking, text, a tool call) sharing one record, so a page
// boundary can fall inside a turn rather than between two of them — and the
// call's result is a separate record again.
//
// Two losses are possible there and both are silent. A cap that cuts mid-record
// reports a cursor naming that record's start, so the next page skips the record
// entirely and the rows above the cut can never be asked for again. And a window
// that starts between a call and its result parses the result with nothing to
// attach it to, leaving the card that owns the call running for good.
func TestServeChatPagingAcrossRecordBoundaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.jsonl")
	var sb strings.Builder
	const n = 2200
	for i := 0; i < n; i++ {
		sb.WriteString(fmt.Sprintf(
			`{"type":"message","id":"a%06d","timestamp":"2026-09-13T03:00:00.000Z","message":{"role":"assistant","model":"m","stopReason":"toolUse","content":[`+
				`{"type":"thinking","thinking":"consider %06d %s"},`+
				`{"type":"text","text":"say %06d"},`+
				`{"type":"toolCall","id":"c%06d","name":"bash","arguments":{"command":"echo %06d","i":"intent %06d"}}]}}`,
			i, i, strings.Repeat("pad ", 20), i, i, i, i))
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintf(
			`{"type":"message","id":"r%06d","timestamp":"2026-09-13T03:00:01.000Z","message":{"role":"toolResult","toolCallId":"c%06d","toolName":"bash","content":[{"type":"text","text":"out %06d"}]}}`,
			i, i, i))
		sb.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	be := &chatFakeBackend{panes: []string{chatPane("w1:p1", path, true)}}
	useFakeHost(t, be)

	get := func(query string) chatPayload {
		rec := httptest.NewRecorder()
		serveChat(rec, httptest.NewRequest(http.MethodGet, "/api/chat"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d for %q: %s", rec.Code, query, strings.TrimSpace(rec.Body.String()))
		}
		var out chatPayload
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// id -> whether some page rendered it finished. A call with a result must
	// reach "completed" on at least one page; "only ever running" is the bug.
	seen := map[string]bool{}
	completed := map[string]bool{}
	total := 0
	page := get("?pane=w1:p1")
	offset, pages := page.StartOffset, 1
	absorb := func(items []chatItem) {
		for _, it := range items {
			total++
			if _, dup := seen[it.ID]; dup {
				t.Fatalf("row %s appeared on two pages", it.ID)
			}
			seen[it.ID] = true
			if it.Tool != nil && it.Tool.State != "running" {
				completed[it.ID] = true
			}
		}
	}
	absorb(page.Items)
	for offset > 0 {
		p := get(fmt.Sprintf("?pane=w1:p1&before=%d", offset))
		if len(p.Items) == 0 {
			t.Fatalf("page at before=%d came back empty", offset)
		}
		absorb(p.Items)
		offset = p.StartOffset
		pages++
		if pages > 200 {
			t.Fatal("paging did not terminate")
		}
	}

	// Every row of every turn is reachable: 3 rows per turn, no gaps, no repeats.
	if total != n*3 {
		t.Errorf("pages carried %d rows over %d pages, want %d", total, pages, n*3)
	}
	for i := 0; i < n; i++ {
		for _, id := range []string{fmt.Sprintf("a%06d", i), fmt.Sprintf("a%06d:1", i), fmt.Sprintf("c%06d", i)} {
			if !seen[id] {
				t.Fatalf("row %s is on no page at all", id)
			}
		}
	}
	// And no call is left running on the only page that shows it.
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c%06d", i)
		if !completed[id] {
			t.Fatalf("tool call %s is still running on every page that has it", id)
		}
	}
}

// The regression that defeats both other mechanisms. One call and its result at
// the TOP of the transcript, then enough later rows that the item cap pushes the
// call off the tail page: the tail parses the pair correctly and then CAPS THE
// CALL OUT, its result emits no row of its own, and the page below — which ends
// at the tail's cursor — holds the call with the answer on the far side of that
// cursor. Reading the tail's window wider cannot help (the call is inside it and
// still capped); refusing state regressions cannot help (no completed row
// survives to refuse anything). Only looking FORWARDS from the page's end can,
// which is why the call is asserted through a fetch of the tail alone.
func TestServeChatPageEndSplitsCallFromResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "split.jsonl")
	var sb strings.Builder
	sb.WriteString(`{"type":"message","id":"a1","timestamp":"2026-09-13T03:00:00.000Z","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"slow","i":"the split call"}}]}}`)
	sb.WriteString("\n")
	// chatMaxItems later rows, so the cap has to drop the call...
	for i := 0; i < chatMaxItems; i++ {
		sb.WriteString(fmt.Sprintf(
			`{"type":"message","id":"z%06d","timestamp":"2026-09-13T03:01:00.000Z","message":{"role":"user","content":[{"type":"text","text":"later %06d %s"}]}}`,
			i, i, strings.Repeat("pad ", 16)))
		sb.WriteString("\n")
	}
	// ...and the answer arriving AFTER them, which is what a backgrounded tool
	// does (omp's bash takes an `async` flag, and its result lands whenever it
	// finishes). That is the part that makes this unfixable by reading wider:
	// the page owning the call now ends before the result exists.
	sb.WriteString(`{"type":"message","id":"r1","timestamp":"2026-09-13T03:02:00.000Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"split call output"}]}}`)
	sb.WriteString("\n")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	be := &chatFakeBackend{panes: []string{chatPane("w1:p1", path, true)}}
	useFakeHost(t, be)

	get := func(query string) chatPayload {
		rec := httptest.NewRecorder()
		serveChat(rec, httptest.NewRequest(http.MethodGet, "/api/chat"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d for %q: %s", rec.Code, query, strings.TrimSpace(rec.Body.String()))
		}
		var out chatPayload
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// The tail alone: the call is past the cap here, so this page must not claim
	// it at all — and must not claim it running either.
	tail := get("?pane=w1:p1")
	for _, it := range tail.Items {
		if it.ID == "c1" && it.Tool != nil && it.Tool.State == "running" {
			t.Fatalf("the tail returned the split call as running")
		}
	}

	// The page that OWNS the call ends where the answer begins; it has to come
	// back finished.
	prev := get(fmt.Sprintf("?pane=w1:p1&before=%d", tail.StartOffset))
	found := false
	for _, it := range prev.Items {
		if it.ID != "c1" {
			continue
		}
		found = true
		if it.Tool == nil || it.Tool.State != "completed" {
			t.Fatalf("the page owning the split call returned it as %+v", it.Tool)
		}
		if !strings.Contains(it.Tool.Output, "split call output") {
			t.Errorf("output = %q, want the result that lives past the page end", it.Tool.Output)
		}
	}
	if !found {
		t.Fatal("no page carried the split call")
	}
}

// Claude Code's log, in the shapes a real one uses: the record type IS the role,
// content is a bare string on a plain user turn, a tool result is a block on a
// USER record rather than a message of its own, and subagent traffic shares the
// same file flagged isSidechain.
func TestParseClaudeTranscript(t *testing.T) {
	data := chatLog(
		`{"type":"mode","mode":"normal"}`,
		`{"type":"ai-title","aiTitle":"Lasso MCP with uvx mcp2cli testing","sessionId":"s1"}`,
		`{"type":"system","subtype":"stop_hook_summary","uuid":"sys1"}`,
		`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-09-11T16:49:39.653Z","isSidechain":false,"message":{"role":"user","content":"are we running the lasso mcp locally?"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-09-11T16:49:44.373Z","isSidechain":false,"message":{"role":"assistant","model":"claude-sonnet-4-5","stop_reason":"tool_use","usage":{"input_tokens":2,"cache_read_input_tokens":100,"cache_creation_input_tokens":50},"content":[`+
			`{"type":"thinking","thinking":""},`+
			`{"type":"thinking","thinking":"**Considering the local server**"},`+
			`{"type":"text","text":"Checking the service."},`+
			`{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"systemctl --user is-active lasso.service"}}]}}`,
		`{"type":"user","uuid":"r1","parentUuid":"a1","timestamp":"2026-09-11T16:49:45.036Z","isSidechain":false,"message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"active\n---\nLISTEN 0 4096 127.0.0.1:8090","is_error":false}]}}`,
		// A subagent's own conversation, interleaved into the parent's log.
		`{"type":"user","uuid":"sc1","parentUuid":"a1","timestamp":"2026-09-11T16:49:46.000Z","isSidechain":true,"message":{"role":"user","content":"sidechain traffic that is not this conversation"}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"r1","timestamp":"2026-09-11T16:49:50.000Z","isSidechain":false,"message":{"role":"assistant","model":"claude-sonnet-4-5","stop_reason":"end_turn","content":[{"type":"text","text":"Yes — it is running."}]}}`,
		`{"type":"user","uuid":"m1","isMeta":true,"message":{"role":"user","content":"meta noise"}}`,
	)

	got := parseTranscript("claude", data, 0)
	if got.title != "Lasso MCP with uvx mcp2cli testing" {
		t.Errorf("title = %q, want the session's own ai-title", got.title)
	}
	if got.model != "claude-sonnet-4-5" {
		t.Errorf("model = %q", got.model)
	}
	// Fresh tokens plus both cache directions: what claude counts against the
	// window.
	if got.tokens != 152 {
		t.Errorf("tokens = %d, want 2+100+50", got.tokens)
	}
	if got.run {
		t.Error("run = true after an end_turn with no call outstanding")
	}
	// Nothing was capped, so there is no earlier ROW to fetch. startOffset
	// still points at the first row's record (not 0: claude writes bookkeeping
	// records above it that produce no rows), which is what makes the page
	// above this one empty rather than wrong.
	if got.more {
		t.Error("more = true without any rows dropped")
	}
	if got.startOffset != int64(strings.Index(string(data), `{"type":"user","uuid":"u1"`)) {
		t.Errorf("startOffset = %d, want the offset of the first row's record", got.startOffset)
	}

	var kinds []string
	var tool *chatTool
	for _, it := range got.items {
		kinds = append(kinds, it.Kind)
		if it.Tool != nil {
			tool = it.Tool
		}
		if strings.Contains(it.Text, "sidechain traffic") {
			t.Error("a sidechain record was rendered as part of the parent conversation")
		}
		if strings.Contains(it.Text, "meta noise") {
			t.Error("an isMeta record was rendered")
		}
	}
	want := []string{"user", "agent", "agent", "tool", "agent"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("rows = %v, want %v", kinds, want)
	}
	// The blank thinking block is not a row; the one with prose is.
	if !got.items[1].Thinking || !strings.Contains(got.items[1].Text, "Considering") {
		t.Errorf("row 1 = %+v, want the thinking block that had prose", got.items[1])
	}
	if tool == nil {
		t.Fatal("no tool card for the Bash call")
	}
	if tool.Title != "Bash" || tool.Family != "shell" {
		t.Errorf("tool = %s/%s, want Bash/shell", tool.Title, tool.Family)
	}
	// The call is answered by a block on a LATER, differently-typed record.
	if tool.State != "completed" {
		t.Fatalf("state = %q — the tool_result block did not reach its call", tool.State)
	}
	if !strings.Contains(tool.Output, "127.0.0.1:8090") {
		t.Errorf("output = %q", tool.Output)
	}
}

// A turn that ended by calling a tool is still going.
func TestParseClaudeTranscriptRunning(t *testing.T) {
	got := parseTranscript("claude", chatLog(
		`{"type":"assistant","uuid":"a1","timestamp":"T","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_9","name":"Read","input":{"file_path":"/tmp/x"}}]}}`,
	), 0)
	if !got.run {
		t.Error("run = false with a tool call outstanding")
	}
	if len(got.items) != 1 || got.items[0].Tool == nil || got.items[0].Tool.State != "running" {
		t.Fatalf("items = %+v, want one running tool card", got.items)
	}
}

// A tool_result whose call is in a LATER window still has to be able to answer
// it: that is the page-end split, in claude's shape.
func TestParseClaudeTranscriptResultBeforeCall(t *testing.T) {
	got := parseTranscript("claude", chatLog(
		`{"type":"user","uuid":"r1","timestamp":"T","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_5","content":"the answer"}]}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"T","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_5","name":"Bash","input":{"command":"ls"}}]}}`,
	), 0)
	if got.pendingResults != 0 {
		t.Errorf("pendingResults = %d, want the buffered result to have been applied", got.pendingResults)
	}
	var tool *chatTool
	for _, it := range got.items {
		if it.Tool != nil {
			tool = it.Tool
		}
	}
	if tool == nil || tool.State != "completed" || !strings.Contains(tool.Output, "the answer") {
		t.Fatalf("tool = %+v, want the earlier result applied", tool)
	}
}

// A user turn that was only a pasted screenshot carries no text, so without this
// it contributed no row and the conversation read as if the message — and the
// reply to it — came from nowhere.
func TestParseClaudeTranscriptImageOnlyTurn(t *testing.T) {
	got := parseTranscript("claude", chatLog(
		`{"type":"user","uuid":"u1","timestamp":"T","isSidechain":false,"message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}}`,
	), 0)
	if len(got.items) != 1 {
		t.Fatalf("rows = %d, want the image turn to still be a row: %+v", len(got.items), got.items)
	}
	if got.items[0].Kind != "user" || !strings.Contains(got.items[0].Text, "image") {
		t.Errorf("row = %+v, want a user row saying an image was attached", got.items[0])
	}
}

// A turn's first seconds have no record at all: both harnesses write a COMPLETE
// assistant message, so while the agent generates its reply the file's newest
// assistant record is still the PREVIOUS turn's, which ended cleanly. A
// stop-reason read therefore says "not running" for exactly the stretch a human
// sits looking at the view waiting for an answer — the one moment the indicator
// matters most. herdr's own pane status covers it.
func TestServeChatRunningFromHerdrStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settled.jsonl")
	body := chatLog(recHeader, recUser,
		`{"type":"message","id":"a1","message":{"role":"assistant","model":"gpt-5","stopReason":"stop","content":[{"type":"text","text":"Done."}]}}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	// The transcript on its own says the agent is idle; herdr says it is working.
	for _, tc := range []struct {
		status string
		want   bool
	}{
		{"working", true},
		{"idle", false},
		// A human's turn to answer, not the agent's: not "generating".
		{"blocked", false},
	} {
		be := &chatFakeBackend{panes: []string{chatPaneStatus("w1:p1", path, true, tc.status)}}
		useFakeHost(t, be)
		rec := httptest.NewRecorder()
		serveChat(rec, httptest.NewRequest(http.MethodGet, "/api/chat", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d for %s: %s", rec.Code, tc.status, strings.TrimSpace(rec.Body.String()))
		}
		var out chatPayload
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Running != tc.want {
			t.Errorf("herdr status %q -> running=%v, want %v", tc.status, out.Running, tc.want)
		}
	}
}
