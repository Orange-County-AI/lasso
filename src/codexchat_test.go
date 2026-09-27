package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetectCodexComposer(t *testing.T) {
	for _, tt := range []struct {
		name string
		want ComposerState
	}{
		{"codex-empty", ComposerEmpty},
		{"codex-draft", ComposerDraft},
		{"codex-history-only", ComposerUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			screen, err := os.ReadFile(filepath.Join("testdata", "screens", tt.name+".ansi"))
			if err != nil {
				t.Fatal(err)
			}
			if got := detectCodexComposer(string(screen)); got != tt.want {
				t.Fatalf("detectCodexComposer(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func elide(s string) string {
	var e elider
	e.write([]byte(s))
	return string(e.take())
}

func TestEliderCutsLongStringsToValidJSON(t *testing.T) {
	image := "data:image/png;base64," + strings.Repeat("A", 3*logStringCap)
	long := strings.Repeat("é\\n\\u00e9\\\"", logStringCap)
	in := `{"a":"short","img":"` + image + `","long":"` + long + `","n":[1,"x"]}`
	out := elide(in)
	var got struct {
		A    string `json:"a"`
		Img  string `json:"img"`
		Long string `json:"long"`
		N    []any  `json:"n"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("elided line is not JSON: %v\n%.300s", err, out)
	}
	if got.A != "short" || len(got.N) != 2 {
		t.Fatalf("short values changed: %+v", got)
	}
	if got.Img != "data:" {
		t.Fatalf("data URL kept %d bytes, want just the scheme", len(got.Img))
	}
	if len(got.Long) == 0 || len(got.Long) > logStringCap {
		t.Fatalf("long string kept %d bytes, want a prefix of at most %d", len(got.Long), logStringCap)
	}
	if strings.ContainsRune(got.Long, '�') {
		t.Fatal("prefix was cut inside a UTF-8 sequence")
	}
	if small := `{"a":"b\"c","d":[]}`; elide(small) != small {
		t.Fatalf("a short line must pass through untouched, got %s", elide(small))
	}
}

func codexLine(typ string, payload any) string {
	b, _ := json.Marshal(map[string]any{"timestamp": "2026-09-24T19:13:39.783Z", "type": typ, "payload": payload})
	return string(b)
}

func TestCodexLinesCrossGiantRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-x.jsonl")
	giant := codexLine("response_item", map[string]any{
		"type": "custom_tool_call_output", "call_id": "c1",
		"output": []any{
			map[string]any{"type": "input_text", "text": "Script completed\nWall time 2.5 seconds\nOutput:\n"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + strings.Repeat("Q", 3*chatReadBytes)},
		},
	})
	lines := []string{
		codexLine("response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "make the banner"}}}),
		codexLine("response_item", map[string]any{"type": "custom_tool_call", "call_id": "c1", "name": "exec", "input": `const r = await tools.view_image({path:"/tmp/a/banner.png"}); text(r)`}),
		giant,
		codexLine("response_item", map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Done."}}}),
		codexLine("event_msg", map[string]any{"type": "task_complete"}),
	}
	data := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &localBackend{}
	size := int64(len(data))

	// The tail: a plain window would land inside the image and show nothing.
	page := readLogPage(b, path, "codex", size, size)
	var kinds []string
	for _, it := range page.items {
		kinds = append(kinds, it.Kind)
	}
	if strings.Join(kinds, ",") != "user,tool,agent" {
		t.Fatalf("rows = %v, want user,tool,agent", kinds)
	}
	tool := page.items[1].Tool
	if tool.State != "completed" || tool.Images != 1 || tool.DurationMS != 2500 {
		t.Fatalf("tool = %+v, want completed with 1 image in 2500ms", tool)
	}
	if tool.Title != "Image" || tool.Subject != shortPath("/tmp/a/banner.png") {
		t.Fatalf("card reads %q · %q, want Image · the short path", tool.Title, tool.Subject)
	}
	if page.run {
		t.Fatal("a completed turn must not read as running")
	}

	// A page ending at the assistant row pages back across the giant line.
	before := page.items[2].off
	older := readLogPage(b, path, "codex", size, before)
	if len(older.items) != 2 || older.items[0].Text != "make the banner" {
		t.Fatalf("page above the giant line = %+v", older.items)
	}
	// Its call's result lies past the page's end, and is found there.
	if older.items[1].Tool.State != "completed" {
		t.Fatalf("forward result not applied: %+v", older.items[1].Tool)
	}
}

func TestParseCodexTranscript(t *testing.T) {
	data := strings.Join([]string{
		codexLine("turn_context", map[string]any{"model": "gpt-6-sol"}),
		codexLine("response_item", map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "<permissions instructions>"}}}),
		codexLine("response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<environment_context>\n  <cwd>/x</cwd>\n</environment_context>"}}}),
		codexLine("response_item", map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "<image name=[Image #1] path=\"a.png\">"},
			map[string]any{"type": "input_image", "image_url": "data:"},
			map[string]any{"type": "input_text", "text": "</image>"},
		}}),
		codexLine("event_msg", map[string]any{"type": "task_started"}),
		codexLine("response_item", map[string]any{"type": "custom_tool_call", "call_id": "c1", "name": "exec", "input": "// @exec: {\"yield_time_ms\": 1}\nconst r = await Promise.allSettled([tools.exec_command({cmd:\"cat BRIEF.md\"}), tools.exec_command({cmd:'ls'})]);"}),
		codexLine("response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "c1", "output": []any{
			map[string]any{"type": "input_text", "text": "Script failed\nWall time 0.1 seconds\nOutput:\n"},
			map[string]any{"type": "input_text", "text": "Script error:\nno such file"},
		}}),
		codexLine("response_item", map[string]any{"type": "function_call", "call_id": "c2", "name": "exec_command", "arguments": `{"cmd":"git status"}`}),
		codexLine("response_item", map[string]any{"type": "function_call_output", "call_id": "c2", "output": "Chunk ID: ab\nWall time: 0.0000 seconds\nProcess exited with code 0\nOutput:\nclean\n"}),
		codexLine("response_item", map[string]any{"type": "custom_tool_call", "call_id": "c3", "name": "exec", "input": "await tools.exec_command({cmd:\"sleep 99\"})"}),
		codexLine("event_msg", map[string]any{"type": "token_count", "info": map[string]any{"last_token_usage": map[string]any{"input_tokens": 19179}}}),
		codexLine("event_msg", map[string]any{"type": "turn_aborted", "reason": "interrupted"}),
	}, "\n") + "\n"
	got := parseTranscript("codex", []byte(data), 0)
	if got.model != "gpt-6-sol" || got.tokens != 19179 {
		t.Fatalf("model/tokens = %q/%d", got.model, got.tokens)
	}
	if len(got.items) != 5 {
		t.Fatalf("items = %d, want 5 (image turn, three cards, interrupted): %+v", len(got.items), got.items)
	}
	if got.items[0].Kind != "user" || got.items[0].Text != "[1 image attached]" {
		t.Fatalf("injected context leaked or pasted image lost: %+v", got.items[0])
	}
	c1 := got.items[1].Tool
	if c1.Title != "Shell" || c1.Subject != "cat BRIEF.md (+1 more)" || c1.State != "error" || c1.Error != "no such file" {
		t.Fatalf("exec card = %+v", c1)
	}
	c2 := got.items[2].Tool
	if c2.Subject != "git status" || c2.State != "completed" || c2.Output != "clean" {
		t.Fatalf("function_call card = %+v", c2)
	}
	if c3 := got.items[3].Tool; c3.State != "error" || c3.Error != "interrupted" {
		t.Fatalf("an interrupted call must not stay running: %+v", c3)
	}
	if got.items[4].Marker != "interrupted" || got.run {
		t.Fatalf("turn end = %+v, run=%v", got.items[4], got.run)
	}
}

func TestUUIDV7Time(t *testing.T) {
	ts, ok := uuidV7Time("01a0d4d6-1c62-7fc3-a6b3-196ecbb56557")
	if !ok || ts.UTC().Format("2006/01/02 15:04") != "2026/09/24 19:13" {
		t.Fatalf("uuidV7Time = %v %v", ts.UTC(), ok)
	}
	if _, ok := uuidV7Time("4f1c2a9e-1c62-4fc3-a6b3-196ecbb56557"); ok {
		t.Fatal("a v4 id carries no time")
	}
}

func TestFindCodexTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	id := "01a0d4d6-1c62-7fc3-a6b3-196ecbb56557"
	dir := filepath.Join(home, ".codex", "sessions", "2026", "09", "24")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "rollout-2026-09-24T12-13-16-"+id+".jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := findCodexTranscript(&localBackend{}, id); got != want {
		t.Fatalf("findCodexTranscript = %q, want %q", got, want)
	}
}

// codexProcBackend is the local filesystem with a canned pane.process_info, so
// the pane's own codex process can be staged.
type codexProcBackend struct {
	localBackend
	procs map[string]string // pane id -> process_info JSON
}

func (b *codexProcBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	id, _ := params.(map[string]any)["pane_id"].(string)
	if method != "pane.process_info" || b.procs[id] == "" {
		return nil, fmt.Errorf("unexpected herdr call %s", method)
	}
	return json.RawMessage(`{"process_info":` + b.procs[id] + `}`), nil
}

func codexProcJSON(cwd string, argv ...string) string {
	a, _ := json.Marshal(argv)
	return fmt.Sprintf(`{"foreground_process_group_id":7,"foreground_processes":[{"pid":7,"name":"codex","cwd":%q,"argv":%s}]}`, cwd, a)
}

func writeCodexRollout(t *testing.T, dir, id, cwd, originator string, mod time.Time) string {
	t.Helper()
	path := filepath.Join(dir, "rollout-2026-09-27T18-54-03-"+id+".jsonl")
	meta := fmt.Sprintf(`{"timestamp":"x","type":"session_meta","payload":{"id":%q,"cwd":%q,"originator":%q,"thread_source":"user","base_instructions":"..."}}`+"\n", id, cwd, originator)
	if err := os.WriteFile(path, []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

// The case that broke the chat view: herdr reported NO session for a fresh
// codex pane, and named that pane's session on another codex pane that was
// running `codex resume` of an older session in a different directory.
func TestCodexPaneTranscriptOverridesHerdr(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	now := time.Now()
	today := filepath.Join(home, ".codex", "sessions", now.UTC().Format("2006/01/02"))
	old := filepath.Join(home, ".codex", "sessions", "2026", "09", "24")
	for _, d := range []string{today, old} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const (
		freshID  = "01a0e437-95bb-7c43-bcce-6a2e0687fb7b"
		resumeID = "01a0d4d6-1c62-7fc3-a6b3-196ecbb56557"
		execID   = "01a0e438-0000-7000-8000-000000000000"
	)
	fresh := writeCodexRollout(t, today, freshID, "/work/ea-og", "codex-tui", now.Add(-time.Minute))
	resumed := writeCodexRollout(t, old, resumeID, "/work/jessica", "codex-tui", now.Add(-time.Hour))
	// A newer `codex exec` in the same directory must not steal the pane.
	writeCodexRollout(t, today, execID, "/work/ea-og", "codex_exec", now)

	be := &codexProcBackend{procs: map[string]string{
		"w:fresh":   codexProcJSON("/work/ea-og", "codex"),
		"w:resumed": codexProcJSON("/work/jessica", "codex", "resume", resumeID),
	}}

	if got := codexPaneTranscript(be, pane{PaneID: "w:fresh", Agent: "codex"}, ""); got != fresh {
		t.Errorf("session-less codex pane = %q, want %q", got, fresh)
	}
	if got := codexPaneTranscript(be, pane{PaneID: "w:resumed", Agent: "codex"}, freshID); got != resumed {
		t.Errorf("misattributed codex pane = %q, want its resumed session %q", got, resumed)
	}
	// herdr's id is believed when it was recorded where the pane's codex runs.
	if got := codexPaneTranscript(be, pane{PaneID: "w:fresh", Agent: "codex"}, freshID); got != fresh {
		t.Errorf("matching herdr id = %q, want %q", got, fresh)
	}
	// Without process info (older herdr), herdr's id is used as before.
	if got := codexPaneTranscript(be, pane{PaneID: "w:gone", Agent: "codex"}, resumeID); got != resumed {
		t.Errorf("no process info = %q, want herdr's %q", got, resumed)
	}
}

func TestCodexResumeID(t *testing.T) {
	for argv, want := range map[string]string{
		"codex":                     "",
		"codex resume abc-1":        "abc-1",
		"codex resume --yolo abc-2": "abc-2",
		"codex resume":              "",
	} {
		if got := codexResumeID(strings.Fields(argv)); got != want {
			t.Errorf("codexResumeID(%q) = %q, want %q", argv, got, want)
		}
	}
}
