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
	if got := paneTranscript(be, base, false); got.Path == "" || got.Harness != "omp" {
		t.Errorf("omp path session = %+v, want the file and its reader", got)
	}

	// herdr keeps agent_session after the agent exits. Nothing is coming, so
	// this is not a wait — the view must not spin an orb over a finished pane.
	exited := base
	exited.Agent = ""
	if got := paneTranscript(be, exited, false); got.Path != "" || got.Starting {
		t.Errorf("exited pane = %+v, want no transcript and no wait", got)
	}

	// Claude reports an ID. Its log is on disk, so the pane is readable —
	// refusing the id is what left these panes empty while their transcripts sat
	// there.
	byID := base
	byID.Agent = "claude"
	byID.AgentSession = &agentSession{Agent: "claude", Kind: "id", Value: claudeID}
	if got := paneTranscript(be, byID, false); got.Path == "" || got.Harness != "claude" {
		t.Errorf("claude id session = %+v, want the log it resolves to", got)
	}

	// An id whose log is not here says so, rather than claiming no session. The
	// agent is running, so its log is expected: this is a wait, not a verdict.
	missing := byID
	missing.AgentSession = &agentSession{Agent: "claude", Kind: "id", Value: "00000000-0000-4000-8000-000000000000"}
	if got := paneTranscript(be, missing, false); got.Path != "" || got.Note == "" || !got.Starting {
		t.Errorf("unresolvable id = %+v, want a note and a wait", got)
	}

	// A harness that reports only an id and that lasso cannot resolve. Nothing
	// is going to arrive for it, so it is a verdict rather than a wait.
	unknown := base
	unknown.AgentSession = &agentSession{Agent: "codex", Kind: "id", Value: "abc"}
	if got := paneTranscript(be, unknown, false); got.Path != "" || got.Note == "" || got.Starting {
		t.Errorf("id-only session = %+v, want a note and no wait", got)
	}

	// A live agent whose session herdr has not reported. For a pane lasso did not
	// create, herdr has no session for it and never will: the plain note, and no
	// wait — the view must not promise a transcript that cannot arrive.
	nosession := base
	nosession.AgentSession = nil
	if got := paneTranscript(be, nosession, false); got.Path != "" || got.Note == "" || got.Starting {
		t.Errorf("session-less pane = %+v, want a note and no wait", got)
	}
	// The same pane mid-create is the case that used to read "no agent session in
	// this pane" about an agent visibly running in front of the reader.
	if got := paneTranscript(be, nosession, true); got.Path != "" || got.Note == "" || !got.Starting {
		t.Errorf("booting pane = %+v, want a note and a wait", got)
	}
	// And the earliest second of that boot, where herdr cannot even see the CLI in
	// the pane yet: no agent AND no session. The wait must win over the liveness
	// check, or the window the reader watches their new agent arrive in — pane
	// created, CLI still starting — reads as an empty pane. This is the note that
	// started all of this: a create's first two seconds.
	empty := base
	empty.Agent = ""
	empty.AgentSession = nil
	if got := paneTranscript(be, empty, true); got.Note == "" || !got.Starting {
		t.Errorf("booting pane with no agent visible yet = %+v, want a wait", got)
	}
	// The boot claim covers the session-less pane and nothing else: if herdr still
	// holds a session for the pane, an exited agent's, the record's boot does not
	// make that session readable and the plain note stands.
	if got := paneTranscript(be, exited, true); got.Starting {
		t.Errorf("booting pane carrying an exited session = %+v, want no wait", got)
	}

	// The value reaches a filesystem read, so anything but an absolute .jsonl
	// is refused rather than joined into a path — and a refusal is a verdict.
	for _, bad := range []string{"../../etc/passwd", "/etc/passwd", "relative.jsonl", ""} {
		p := base
		p.AgentSession = &agentSession{Agent: "omp", Kind: "path", Value: bad}
		if got := paneTranscript(be, p, false); got.Path != "" || got.Starting {
			t.Errorf("value %q = %+v, want refused", bad, got)
		}
	}
}

// Which panes lasso counts as still starting. The distinction decides whether an
// empty chat view shows progress or a verdict, and it rests entirely on lasso's
// own record — herdr reports an agent in the pane either way.
func TestPaneBooting(t *testing.T) {
	const pane = "w1:p1"
	cases := []struct {
		name string
		recs []AgentRecord
		want bool
		why  string
	}{
		{
			name: "boot in flight",
			recs: []AgentRecord{{RootPane: pane, BootStatus: BootBooting, CreatedAt: time.Now()}},
			want: true,
			why:  "the CLI is coming up — the session handle has not been announced yet",
		},
		{
			name: "creating",
			recs: []AgentRecord{{RootPane: pane, BootStatus: BootCreating, CreatedAt: time.Now()}},
			want: true,
			why:  "the pane may not even be materialized yet",
		},
		{
			name: "boot just flipped ready",
			recs: []AgentRecord{{RootPane: pane, BootStatus: BootReady, CreatedAt: time.Now().Add(-10 * time.Second)}},
			want: true,
			why:  "the status flips when the CLI launches and the session handle lands a beat later — the gap the reader is watching",
		},
		{
			name: "boot finished long ago",
			recs: []AgentRecord{{RootPane: pane, BootStatus: BootReady, CreatedAt: time.Now().Add(-time.Hour)}},
			want: false,
			why:  "an agent that has run for an hour without a session is not starting, it is unreadable",
		},
		{
			name: "grace expired without a session",
			recs: []AgentRecord{{RootPane: pane, BootStatus: BootReady, CreatedAt: time.Now().Add(-chatBootGrace - time.Minute)}},
			want: false,
			why:  "a create whose CLI died silently must stop promising a start",
		},
		{
			name: "another pane's record",
			recs: []AgentRecord{{RootPane: "w2:p9", BootStatus: BootBooting, CreatedAt: time.Now()}},
			want: false,
			why:  "one pane's boot says nothing about another's",
		},
		{
			name: "no records at all",
			recs: nil,
			want: false,
			why:  "a foreign session lasso never created has no boot to wait for",
		},
	}
	for _, tc := range cases {
		if got := paneBooting(tc.recs, pane); got != tc.want {
			t.Errorf("%s = %v, want %v (%s)", tc.name, got, tc.want, tc.why)
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
// an empty conversation that looks like a broken view — and because that harness
// is RUNNING, it says so as a wait: the log is expected, so the view shows
// progress instead of a verdict.
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
	if !out.Starting {
		t.Error("a running agent whose log has not landed is a wait, want starting")
	}
}

// The other half of that distinction: a pane with no agent at all is not coming
// up, and a view that spun an orb over it would be promising a transcript that
// can never arrive.
func TestServeChatPlainPaneIsNotStarting(t *testing.T) {
	be := &chatFakeBackend{panes: []string{
		`{"pane_id":"w1:p1","focused":true,"agent_session":{"source":"herdr:omp","agent":"omp","kind":"path","value":"/home/u/.omp/agent/sessions/-proj/2026-09-13T03-43-04-614Z_01a0.jsonl"}}`,
	}}
	useFakeHost(t, be)

	rec := httptest.NewRecorder()
	serveChat(rec, httptest.NewRequest(http.MethodGet, "/api/chat", nil))
	var out chatPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Note == "" {
		t.Error("note is empty for a pane with no agent, want an explanation")
	}
	if out.Starting {
		t.Error("a pane with no agent is over, not starting")
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
	// failRead stands in for a pane whose screen cannot be read at all: callers
	// that scan it for a dialog must say "no", never invent one.
	failRead bool
	writes   []string
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
		if b.failRead {
			return nil, fmt.Errorf("pane is gone")
		}
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

// The ask shapes are copied from real transcripts. omp carries the questions on
// the call's arguments and the picks in the result's `details.results`; claude
// puts the questions in the tool_use input and the picks — keyed by question
// text — in a record-level `toolUseResult`.
const (
	recAskOMP = `{"type":"message","id":"a9","timestamp":"2026-09-13T04:00:00.000Z","message":{"role":"assistant","stopReason":"toolUse","content":[` +
		`{"type":"toolCall","id":"call_ask","name":"ask","arguments":{"i":"Choosing a strategy","questions":[` +
		`{"id":"cap","header":"Capacity","question":"What should the plan do about capacity?","options":[` +
		`{"label":"Reclaim only","description":"Free ~90G now.","preview":"prune -> 25.0G"},` +
		`{"label":"Grow the disk","description":"Needs a support ticket."}],"recommended":1},` +
		`{"id":"lh","header":"Longhorn","question":"Is a second node actually on the roadmap?","multi":true,"options":[` +
		`{"label":"No second node"},{"label":"Soon"}]}]}}]}}`
	recAskOMPSomeReplies = `{"type":"message","id":"r9","timestamp":"2026-09-13T04:02:00.000Z","message":{"role":"toolResult","toolCallId":"call_ask","toolName":"ask","content":[{"type":"text","text":"User answers:\ncap: Grow the disk"}],"details":{"results":[` +
		`{"id":"cap","question":"What should the plan do about capacity?","options":["Reclaim only","Grow the disk"],"multi":false,"selectedOptions":["Grow the disk"]},` +
		`{"id":"lh","question":"Is a second node actually on the roadmap?","options":["No second node","Soon"],"multi":true,"selectedOptions":["Soon"]}]}}}`

	recAskClaude = `{"type":"assistant","uuid":"ca1","timestamp":"2026-09-13T04:00:00.000Z","message":{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_q","name":"AskUserQuestion","input":{"questions":[{"question":"Which way should the port be republished?","header":"neko port","multiSelect":false,"options":[` +
		`{"label":"Reproduce it as-is (Recommended)","description":"An incus proxy device forwards it.","preview":"incus config device add ..."},` +
		`{"label":"Leave it where it is"}]}]}}]}}`
	recAskClaudeResult = `{"type":"user","uuid":"cu1","timestamp":"2026-09-13T04:03:00.000Z","toolUseResult":{"questions":[{"question":"Which way should the port be republished?","header":"neko port","multiSelect":false,"options":[{"label":"Reproduce it as-is (Recommended)"},{"label":"Leave it where it is"}]}],"answers":{"Which way should the port be republished?":"Reproduce it as-is (Recommended)"}},` +
		`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_q","content":"Your questions have been answered: \"Which way should the port be republished?\"=\"Reproduce it as-is (Recommended)\""}]}}`
)

// askOf returns the ask card from a parse, failing the test if there is none.
func askOf(t *testing.T, parsed chatParse) *chatAsk {
	t.Helper()
	for i := range parsed.items {
		if tool := parsed.items[i].Tool; tool != nil && tool.Ask != nil {
			return tool.Ask
		}
	}
	t.Fatal("no ask card in the parse")
	return nil
}

func TestParseChatTranscriptAsk(t *testing.T) {
	// The call alone: the questions and their options are the card's content,
	// and there is nothing to have answered yet.
	ask := askOf(t, parseChatTranscript(chatLog(recAskOMP), 0))
	if len(ask.Questions) != 2 {
		t.Fatalf("questions = %d, want 2", len(ask.Questions))
	}
	first := ask.Questions[0]
	if first.Header != "Capacity" || first.Question != "What should the plan do about capacity?" {
		t.Errorf("first question = %q/%q", first.Header, first.Question)
	}
	if len(first.Options) != 2 {
		t.Fatalf("options = %d, want 2", len(first.Options))
	}
	if first.Options[0].Preview != "prune -> 25.0G" || first.Options[0].Description != "Free ~90G now." {
		t.Errorf("first option = %+v, want its description and preview", first.Options[0])
	}
	if first.Recommended == nil || *first.Recommended != 1 {
		t.Errorf("recommended = %v, want the index omp named", first.Recommended)
	}
	if first.Multi || !ask.Questions[1].Multi {
		t.Error("multi did not survive the parse")
	}
	if len(first.Selected) != 0 {
		t.Errorf("selected = %v on an unanswered ask", first.Selected)
	}

	// With the result: the picks land on the questions they were made for, and
	// the raw "User answers" prose is dropped so the card does not say the same
	// thing twice.
	parsed := parseChatTranscript(chatLog(recAskOMP, recAskOMPSomeReplies), 0)
	ask = askOf(t, parsed)
	for _, q := range ask.Questions {
		want := []string{"Grow the disk"}
		if q.Header == "Longhorn" {
			want = []string{"Soon"}
		}
		if strings.Join(q.Selected, ",") != strings.Join(want, ",") {
			t.Errorf("%s selected = %v, want %v", q.Header, q.Selected, want)
		}
	}
	for i := range parsed.items {
		tool := parsed.items[i].Tool
		if tool == nil || tool.Ask == nil {
			continue
		}
		if tool.Output != "" || tool.ResultLine != "answered" {
			t.Errorf("answered card = output %q / %q, want the picks instead of the prose", tool.Output, tool.ResultLine)
		}
	}
}

func TestParseClaudeTranscriptAsk(t *testing.T) {
	ask := askOf(t, parseClaudeTranscript(chatLog(recAskClaude), 0))
	if len(ask.Questions) != 1 {
		t.Fatalf("questions = %d, want 1", len(ask.Questions))
	}
	q := ask.Questions[0]
	if q.Header != "neko port" || len(q.Options) != 2 {
		t.Fatalf("question = %+v", q)
	}
	// claude names its recommendation in the label rather than in a field, and
	// an index lasso invented here would mark the wrong option.
	if q.Recommended != nil {
		t.Errorf("recommended = %v, want none — claude does not send one", *q.Recommended)
	}
	if q.Options[0].Preview != "incus config device add ..." {
		t.Errorf("preview = %q", q.Options[0].Preview)
	}

	ask = askOf(t, parseClaudeTranscript(chatLog(recAskClaude, recAskClaudeResult), 0))
	if len(ask.Questions[0].Selected) != 1 || ask.Questions[0].Selected[0] != "Reproduce it as-is (Recommended)" {
		t.Errorf("selected = %v, want the answer claude recorded", ask.Questions[0].Selected)
	}
}

// A multi-select answer arrives as a list rather than a string.
func TestClaudeAskAnswersList(t *testing.T) {
	got := claudeAskAnswers(json.RawMessage(`{"answers":{"Pick some":["a","b"]}}`))
	if len(got) != 1 || strings.Join(got[0].Selected, ",") != "a,b" {
		t.Fatalf("answers = %+v, want both labels", got)
	}
}

// A ONE-question ask reports its answer flat rather than under `results`, which
// a live probe of omp 18.1.19 is where this shape came from. Reading only the
// wrapped shape left the answered card showing its raw text instead of the pick.
func TestOmpAskAnswersShapes(t *testing.T) {
	flat := ompAskAnswers(json.RawMessage(
		`{"question":"Which?","options":["a","b"],"multi":false,"selectedOptions":["b"]}`))
	if len(flat) != 1 || flat[0].Question != "Which?" || strings.Join(flat[0].Selected, ",") != "b" {
		t.Errorf("flat details = %+v, want the single entry", flat)
	}
	wrapped := ompAskAnswers(json.RawMessage(
		`{"results":[{"question":"One","selectedOptions":["x"]},{"question":"Two","multi":true,"selectedOptions":["y","z"]}]}`))
	if len(wrapped) != 2 || wrapped[1].Question != "Two" || strings.Join(wrapped[1].Selected, ",") != "y,z" {
		t.Errorf("wrapped details = %+v, want both entries", wrapped)
	}
	// A details blob that is not an object at all (omp writes Python-repr
	// strings for some tools) yields nothing rather than a half-read answer.
	if got := ompAskAnswers(json.RawMessage(`"{'wallTimeMs': 12}"`)); got != nil {
		t.Errorf("non-object details = %+v, want nothing", got)
	}
	if got := ompAskAnswers(json.RawMessage(`{"options":["a"]}`)); got != nil {
		t.Errorf("details with no question = %+v, want nothing", got)
	}
}

// The keystrokes are the whole feature: a human cannot see the dialog lasso is
// typing into, so the sequence has to be right by construction.
func TestChatAskKeystrokes(t *testing.T) {
	up := strings.Repeat("\x1b[A", chatAskClampUps)
	down := func(n int) string { return strings.Repeat("\x1b[B", n) }
	for _, tc := range []struct {
		name    string
		agent   string
		answers []chatAskPick
		want    string
	}{
		{
			// One question submits on its Enter — no review screen to walk past.
			name:    "omp single question, one answer",
			agent:   "omp",
			answers: []chatAskPick{{Selected: []int{0}, Options: 3}},
			want:    up + "\r",
		},
		{
			// The cursor is reset first, so the count is the option's index and
			// not a delta from wherever a human left it.
			name:    "omp: the index is the row distance",
			agent:   "omp",
			answers: []chatAskPick{{Selected: []int{2}, Options: 3}},
			want:    up + down(2) + "\r",
		},
		{
			// A multi question toggles with Space (Enter would only advance), so
			// the picks are toggled on the way down.
			name:    "omp multi picks toggle",
			agent:   "omp",
			answers: []chatAskPick{{Selected: []int{2, 0}, Multi: true, Options: 4}},
			want:    up + " " + down(2) + " " + "\r",
		},
		{
			// Several questions: each Enter advances, the last lands on the
			// review screen, and one more press submits it.
			name:    "omp: several questions submit from the review screen",
			agent:   "omp",
			answers: []chatAskPick{{Selected: []int{1}, Options: 3}, {Selected: []int{0}, Options: 2}},
			want:    up + down(1) + "\r" + up + "\r" + "\r",
		},
		{
			// claude must never be sent an ↑: there it walks back to the
			// previous question, so this one goes straight down from row 1.
			name:    "claude never presses up",
			agent:   "claude",
			answers: []chatAskPick{{Selected: []int{2}, Options: 3}},
			want:    down(2) + "\r",
		},
		{
			// A single-question claude ask submits on the pick, so nothing
			// follows it.
			name:    "claude single question, single answer",
			agent:   "claude",
			answers: []chatAskPick{{Selected: []int{0}, Options: 2}},
			want:    "\r",
		},
		{
			// claude's Enter toggles a multi row rather than advancing, so the
			// question is left by walking past the options and the "Other" row
			// onto Submit — and a single multi question still has a review
			// screen behind it.
			name:    "claude multi walks to Submit",
			agent:   "claude",
			answers: []chatAskPick{{Selected: []int{1}, Multi: true, Options: 3}},
			want:    down(1) + " " + down(3) + "\r" + "\r",
		},
		{
			name:    "claude two questions drive on to the review screen",
			agent:   "claude",
			answers: []chatAskPick{{Selected: []int{1}, Options: 2}, {Selected: []int{0}, Options: 2}},
			want:    down(1) + "\r" + "\r" + "\r",
		},
	} {
		if got := chatAskKeystrokes(tc.agent, tc.answers); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// askScreenHolds is the drift check: the keystrokes only mean what the card said
// they mean while the question the answer was picked for is still the one on
// screen.
func TestAskScreenHolds(t *testing.T) {
	const question = "What should the plan do about capacity beyond that?"
	options := []string{"Reclaim only", "Grow the disk"}
	tall := "  ╭─ Ask ─────────────────────────╮\n" +
		"  │ What should the plan do about\n" +
		"  │ capacity beyond that?\n" +
		"  ❯ ○ Reclaim only\n" +
		"  │ Enter select · ↑/↓ move · Esc cancel"
	// The shape a short pane actually produces: the question is off the top of
	// the viewport and only the dialog's lower half is on screen. Refusing here
	// would refuse the case the feature exists for.
	short := "  │   ○ Reclaim only\n" +
		"  │   ○ Grow the disk\n" +
		"  │   ○ Other (type your own)\n" +
		"  │ Enter select · n note · ↑/↓ move · Esc cancel"
	// The transcript alone — an agent's prose quoting the question back — is not
	// a dialog, and must never be typed into.
	prose := "  And the plan should answer: What should the plan do about capacity\n  beyond that? — I will ask."

	if !askScreenHolds(tall, question, options) {
		t.Error("a wrapped question did not match the dialog on screen")
	}
	if !askScreenHolds(short, question, options) {
		t.Error("options alone did not match — a short pane must still be answerable")
	}
	if askScreenHolds(prose, question, options) {
		t.Error("prose quoting the question passed for a dialog")
	}
	if askScreenHolds(tall, "Is a second node actually on the roadmap?", []string{"No second node"}) {
		t.Error("a different question and its options matched")
	}
	if askScreenHolds(short, "", nil) {
		t.Error("nothing to check against claimed to match")
	}
	// A narrow pane wraps the legend across two lines. A per-line footer test
	// refused a dialog that was plainly up (seen live, at 55 columns).
	wrapped := "  1. [ ] Red\n  2. [ ] Blue\n" +
		"  Enter to select · ↑/↓ to navigate · Esc to\n  cancel"
	if !askScreenHolds(wrapped, "Which colours do you want?", []string{"Red", "Blue"}) {
		t.Error("a wrapped footer was not recognised as a dialog")
	}
}

// claude records a multi-select answer as ONE comma-joined string, not a list —
// a live session wrote "Red, Green" for two ticked boxes, which marked nothing
// on the card until this matched it back against the options.
func TestApplyAskAnswersJoinedMulti(t *testing.T) {
	tool := &chatTool{Ask: &chatAsk{Questions: []chatAskQuestion{{
		Question: "Which colours do you want?",
		Multi:    true,
		Options: []chatAskOption{
			{Label: "Red"}, {Label: "Blue"}, {Label: "Green"},
			// A label that contains a comma must still match whole, first.
			{Label: "Red, White and Blue"},
		},
	}}}, Output: "Your questions have been answered: …", ResultLine: "1 lines"}
	applyAskAnswers(tool, []chatAskAnswer{{Question: "Which colours do you want?", Selected: []string{"Red, Green"}}})
	got := strings.Join(tool.Ask.Questions[0].Selected, "|")
	if got != "Red|Green" {
		t.Errorf("selected = %q, want the two boxes that were ticked", got)
	}
	if tool.Output != "" || tool.ResultLine != "answered" {
		t.Errorf("verdict = %q / %q, want the picks instead of the prose", tool.Output, tool.ResultLine)
	}

	// An answer naming no option stays as it came: a wrong mark is worse than a
	// card that simply does not claim one.
	other := &chatTool{Ask: &chatAsk{Questions: []chatAskQuestion{{
		Question: "Q", Options: []chatAskOption{{Label: "Red"}},
	}}}}
	applyAskAnswers(other, []chatAskAnswer{{Question: "Q", Selected: []string{"Purple, Orange"}}})
	if len(other.Ask.Questions[0].Selected) != 0 {
		t.Errorf("selected = %v, want nothing matched", other.Ask.Questions[0].Selected)
	}
}

func TestServeChatAnswer(t *testing.T) {
	be := &chatSendBackend{paneID: "w1:p1", agent: "omp"}
	question := "What should the plan do about capacity?"
	footer := "\n  Enter select · ↑/↓ move · Esc cancel"
	be.screen = "  ╭─ Ask ───────────╮\n  │ " + question + "\n  ❯ ○ Reclaim only" + footer
	useFakeHost(t, be)

	post := func(body string) (*httptest.ResponseRecorder, map[string]string) {
		rec := httptest.NewRecorder()
		serveChatAnswer(rec, httptest.NewRequest(http.MethodPost, "/api/chat/answer", strings.NewReader(body)))
		var out map[string]string
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
		}
		return rec, out
	}

	rec, out := post(`{"pane_id":"w1:p1","expect":"` + question + `","answers":[{"selected":[1],"multi":false,"options":2}]}`)
	if rec.Code != http.StatusOK || out["outcome"] != "sent" {
		t.Fatalf("status/outcome = %d/%q, want 200/sent", rec.Code, out["outcome"])
	}
	if len(be.writes) != 1 || be.writes[0] != chatAskKeystrokes("omp", []chatAskPick{{Selected: []int{1}, Options: 2}}) {
		t.Errorf("writes = %q, want the answer sequence", be.writes)
	}

	// The harness decides which keys those are, and it is herdr's answer: a
	// claude pane must not be sent omp's ↑-clamp, which there means "previous
	// question".
	be.writes = nil
	be.screen = "  ╭─ Ask ───────────╮\n  │ " + question + "\n  ❯ ○ Reclaim only" + footer
	be.agent = "claude"
	// The host's pane listing is cached for a beat, and this changes what that
	// host reports — the way a pane's harness changing would.
	invalidatePaneList(be.Name())
	rec, out = post(`{"pane_id":"w1:p1","expect":"` + question + `","labels":["Reclaim only"],"answers":[{"selected":[1],"multi":false,"options":2}]}`)
	if rec.Code != http.StatusOK || out["outcome"] != "sent" {
		t.Fatalf("claude status/outcome = %d/%q, want 200/sent", rec.Code, out["outcome"])
	}
	if len(be.writes) != 1 || be.writes[0] != chatAskKeystrokes("claude", []chatAskPick{{Selected: []int{1}, Options: 2}}) {
		t.Errorf("claude writes = %q, want the claude sequence", be.writes)
	}
	be.agent = "omp"
	be.writes = nil

	// A short pane: the question has scrolled off the top, and the options and
	// the footer are the whole screen. That is still the dialog, and still
	// answerable.
	be.screen = "  │   ○ Reclaim only\n  │   ○ Grow the disk\n" +
		"  │   ○ Other (type your own)" + footer
	rec, out = post(`{"pane_id":"w1:p1","expect":"` + question + `","labels":["Reclaim only"],"answers":[{"selected":[1],"multi":false,"options":2}]}`)
	if rec.Code != http.StatusOK || out["outcome"] != "sent" {
		t.Fatalf("short pane status/outcome = %d/%q, want 200/sent", rec.Code, out["outcome"])
	}

	// The dialog has moved on — a different question is on screen — so nothing
	// may be typed: those keystrokes would land on whatever it shows now.
	be.writes = nil
	be.screen = "  ╭─ Ask ───────────╮\n  │ Is a second node planned?" + footer
	rec, out = post(`{"pane_id":"w1:p1","expect":"` + question + `","labels":["Reclaim only"],"answers":[{"selected":[1],"multi":false,"options":2}]}`)
	if rec.Code != http.StatusOK || out["outcome"] != "refused" {
		t.Fatalf("status/outcome = %d/%q, want 200/refused", rec.Code, out["outcome"])
	}
	if len(be.writes) != 0 {
		t.Errorf("writes = %q, want none when the question is gone", be.writes)
	}

	// An unreadable pane is refused for the same reason.
	be.failRead = true
	rec, out = post(`{"pane_id":"w1:p1","expect":"` + question + `","answers":[{"selected":[1],"multi":false}]}`)
	if rec.Code != http.StatusOK || out["outcome"] != "refused" {
		t.Errorf("unreadable pane = %d/%q, want 200/refused", rec.Code, out["outcome"])
	}
	be.failRead = false

	// A negative index is not a wrong answer, it is a panic in the repeat that
	// builds the sequence.
	rec, _ = post(`{"pane_id":"w1:p1","expect":"` + question + `","answers":[{"selected":[-1],"multi":false}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative index status = %d, want 400", rec.Code)
	}

	rec, _ = post(`{"pane_id":"w9:p9","expect":"` + question + `","answers":[{"selected":[0],"multi":false}]}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown pane status = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	serveChatAnswer(rec, httptest.NewRequest(http.MethodGet, "/api/chat/answer", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
}
