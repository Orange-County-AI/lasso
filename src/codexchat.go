package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// codex transcripts
// ---------------------------------------------------------------------------

// Codex is the third log shape, and it differs from the other two in ways that
// all matter:
//
//   - herdr names its session by id (kind=id), and the log lives at
//     ~/.codex/sessions/YYYY/MM/DD/rollout-<time>-<id>.jsonl
//   - every record is {timestamp, type, payload}. The conversation is the
//     `response_item` records (message, reasoning, the tool calls and their
//     outputs). Turn state is in `event_msg` (task_started, task_complete,
//     turn_aborted), and everything else is bookkeeping.
//   - the "user" role also carries what Codex injects itself: environment
//     context, AGENTS.md, plugin lists, and the <image> tags around a pasted
//     picture
//   - tools usually run as a JavaScript cell (`exec`) calling tools.exec_command,
//     tools.view_image, ... inside it, so the card's subject has to be read out
//     of the code
//   - images come back inline as base64, which is why the log is read through
//     codexlog.go rather than the shared tail window

func isCodex(harness string) bool {
	return strings.ToLower(strings.TrimSpace(harness)) == "codex"
}

// codexPaths remembers where a session's log was found. Codex never moves a live
// one, and without this every 2s poll of a legacy (non-v7) id would walk the
// whole sessions tree.
var codexPaths sync.Map // host + "\x00" + id -> path

// findCodexTranscript locates a Codex session log by id on the pane's host.
//
// Current Codex ids are UUIDv7, whose first 48 bits are the creation time in
// milliseconds, so the day directory is known without a search. The directory
// is named in the host's LOCAL time, which the id does not carry, so the UTC day
// and its neighbours are tried. An older, random id falls back to a newest-first
// walk.
func findCodexTranscript(b Backend, id string) string {
	key := b.Name() + "\x00" + id
	if v, ok := codexPaths.Load(key); ok {
		if isFile(b, v.(string)) {
			return v.(string)
		}
		codexPaths.Delete(key)
	}
	home, err := b.HomeDir()
	if err != nil || home == "" {
		return ""
	}
	root := filepath.Join(home, ".codex", "sessions")
	suffix := "-" + id + ".jsonl"
	found := ""
	if t, ok := uuidV7Time(id); ok {
		for _, d := range []time.Time{t, t.Add(-24 * time.Hour), t.Add(24 * time.Hour)} {
			if found = findSuffix(b, filepath.Join(root, d.UTC().Format("2006/01/02")), suffix); found != "" {
				break
			}
		}
	} else {
		found = walkCodexSessions(b, root, suffix)
	}
	if found == "" {
		found = findSuffix(b, filepath.Join(home, ".codex", "archived_sessions"), suffix)
	}
	if found != "" {
		codexPaths.Store(key, found)
	}
	return found
}

// uuidV7Time reads the timestamp out of a version-7 UUID.
func uuidV7Time(id string) (time.Time, bool) {
	hexs := strings.ReplaceAll(id, "-", "")
	if len(hexs) != 32 || hexs[12] != '7' {
		return time.Time{}, false
	}
	raw, err := hex.DecodeString(hexs[:12])
	if err != nil {
		return time.Time{}, false
	}
	var ms int64
	for _, c := range raw {
		ms = ms<<8 | int64(c)
	}
	return time.UnixMilli(ms), true
}

func findSuffix(b Backend, dir, suffix string) string {
	ents, err := b.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if !e.Dir && strings.HasPrefix(e.Name, "rollout-") && strings.HasSuffix(e.Name, suffix) {
			return filepath.Join(dir, e.Name)
		}
	}
	return ""
}

// walkCodexSessions searches year/month/day directories newest first. Bounded:
// an old session nobody has open is not worth an unbounded crawl per request.
func walkCodexSessions(b Backend, root, suffix string) string {
	const maxDays = 400
	days := 0
	for _, y := range dirsDesc(b, root) {
		for _, m := range dirsDesc(b, y) {
			for _, d := range dirsDesc(b, m) {
				if p := findSuffix(b, d, suffix); p != "" {
					return p
				}
				if days++; days >= maxDays {
					return ""
				}
			}
		}
	}
	return ""
}

func dirsDesc(b Backend, dir string) []string {
	ents, err := b.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.Dir {
			out = append(out, filepath.Join(dir, e.Name))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// ---------------------------------------------------------------------------
// reading a page
// ---------------------------------------------------------------------------

// readCodexPage is serveChat's windowed read for a Codex log: the page ending at
// `end`, widened while it holds results whose call lies above it, with calls it
// leaves running looked up past its end.
func readCodexPage(b Backend, path string, size, end int64) chatParse {
	var parsed chatParse
	for tries := 0; ; tries++ {
		lines := codexLinesBefore(b, path, size, end, chatReadBytes*(tries+1))
		parsed = parseCodexLines(lines)
		// A page the scan bound cut short inside a run of image records has no
		// rows, and a page with no rows would otherwise report the top of the
		// conversation. Its lines still say how far it got.
		if len(parsed.items) == 0 && len(lines) > 0 {
			parsed.startOffset = lines[0].off
		}
		if parsed.pendingResults == 0 || len(lines) == 0 || lines[0].off == 0 || tries >= chatPageExtendTries {
			break
		}
	}
	if len(parsed.running) > 0 && end < size {
		for _, l := range codexLinesFrom(b, path, size, end, chatReadBytes) {
			var rec codexRecord
			if json.Unmarshal(l.data, &rec) != nil || rec.Type != "response_item" {
				continue
			}
			var p codexPayload
			if json.Unmarshal(rec.Payload, &p) != nil || !strings.HasSuffix(p.Type, "_output") {
				continue
			}
			if t, ok := parsed.running[p.CallID]; ok {
				codexFinish(t, p.Output)
				delete(parsed.running, p.CallID)
				if len(parsed.running) == 0 {
					break
				}
			}
		}
	}
	return parsed
}

// ---------------------------------------------------------------------------
// records
// ---------------------------------------------------------------------------

type codexRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexPayload is every payload field the chat reads. RawMessage where Codex's
// own type varies (a tool output is a string on one tool and a block list on
// another), because one mistyped field throws the whole record away.
type codexPayload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Summary   json.RawMessage `json:"summary"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Arguments json.RawMessage `json:"arguments"`
	CallID    string          `json:"call_id"`
	Status    string          `json:"status"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
	Model     string          `json:"model"`
	Info      json.RawMessage `json:"info"`
	Message   json.RawMessage `json:"message"`
}

type codexBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// parseCodexTranscript reads a window of a Codex log given as raw bytes, the
// form the shared dispatch hands every harness.
func parseCodexTranscript(data []byte, base int64) chatParse {
	var lines []jsonlLine
	off := base
	for _, raw := range bytes.Split(data, []byte("\n")) {
		lines = append(lines, jsonlLine{off: off, data: raw})
		off += int64(len(raw)) + 1
	}
	return parseCodexLines(lines)
}

// parseCodexLines turns log lines into chat rows.
func parseCodexLines(lines []jsonlLine) chatParse {
	var out chatParse
	byCall := map[string]*chatTool{}
	pending := map[string]json.RawMessage{}
	turnOpen := false

	// endTurn settles the cards a turn left open. Codex writes no output for a
	// call the human interrupted, and a card left "running" would keep the whole
	// view saying the agent is working.
	endTurn := func(interrupted bool) {
		turnOpen = false
		for _, t := range byCall {
			if t.State != "running" {
				continue
			}
			if interrupted {
				t.State, t.Error = "error", "interrupted"
			} else {
				t.State = "completed"
			}
		}
	}

	for _, l := range lines {
		line := bytes.TrimSpace(l.data)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec codexRecord
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		var p codexPayload
		if json.Unmarshal(rec.Payload, &p) != nil {
			continue
		}
		first := len(out.items)
		id := "x" + strconv.FormatInt(l.off, 10)
		switch rec.Type {
		case "turn_context":
			if p.Model != "" {
				out.model = p.Model
			}
		case "event_msg":
			switch p.Type {
			case "task_started":
				turnOpen = true
			case "task_complete":
				endTurn(false)
			case "turn_aborted":
				endTurn(true)
				appendMarker(&out, chatItem{
					Kind: "marker", ID: rowID(id, 900), At: rec.Timestamp,
					Marker: "interrupted",
				})
			case "error":
				if msg := strings.TrimSpace(rawString(p.Message)); msg != "" {
					appendMarker(&out, chatItem{
						Kind: "marker", ID: rowID(id, 901), At: rec.Timestamp,
						Marker: "error", Text: msg,
					})
				}
			case "token_count":
				var info struct {
					Last struct {
						Input int `json:"input_tokens"`
					} `json:"last_token_usage"`
				}
				if json.Unmarshal(p.Info, &info) == nil && info.Last.Input > 0 {
					out.tokens = info.Last.Input
				}
			}
		case "response_item":
			switch p.Type {
			case "message":
				codexMessage(&out, p, id, rec.Timestamp)
			case "reasoning":
				var sum []codexBlock
				_ = json.Unmarshal(p.Summary, &sum)
				var parts []string
				for _, s := range sum {
					if strings.TrimSpace(s.Text) != "" {
						parts = append(parts, strings.TrimSpace(s.Text))
					}
				}
				if len(parts) > 0 {
					out.items = append(out.items, chatItem{
						Kind: "agent", ID: id, At: rec.Timestamp,
						Text: strings.Join(parts, "\n\n"), Thinking: true,
					})
				}
			case "custom_tool_call", "function_call", "local_shell_call", "web_search_call":
				t := codexTool(p, id)
				if t == nil {
					break
				}
				if t.State == "running" {
					byCall[t.CallID] = t
				}
				out.items = append(out.items, chatItem{Kind: "tool", ID: t.CallID, At: rec.Timestamp, Tool: t})
				if res, ok := pending[t.CallID]; ok {
					delete(pending, t.CallID)
					codexFinish(t, res)
				}
			case "custom_tool_call_output", "function_call_output", "local_shell_call_output":
				if t, ok := byCall[p.CallID]; ok {
					codexFinish(t, p.Output)
				} else if p.CallID != "" {
					pending[p.CallID] = p.Output
				}
			}
		}
		for i := first; i < len(out.items); i++ {
			out.items[i].off = l.off
		}
	}

	if len(out.items) > chatMaxItems {
		keep := len(out.items) - chatMaxItems
		for keep > 0 && out.items[keep].off == out.items[keep-1].off {
			keep--
		}
		out.items = out.items[keep:]
		out.more = true
	}
	out.pendingResults = len(pending)
	out.running = map[string]*chatTool{}
	for key, t := range byCall {
		if t.State == "running" {
			out.running[key] = t
		}
	}
	if len(out.items) > 0 {
		out.startOffset = out.items[0].off
	}
	out.run = turnOpen || len(out.running) > 0
	if len(out.items) == 0 && out.note == "" {
		out.note = "No messages yet."
	}
	return out
}

// codexMessage adds a message record's rows. Only the user and assistant roles
// are conversation; developer and system messages are Codex's own instructions.
func codexMessage(out *chatParse, p codexPayload, id, at string) {
	if p.Role != "user" && p.Role != "assistant" {
		return
	}
	var blocks []json.RawMessage
	if json.Unmarshal(p.Content, &blocks) != nil {
		return
	}
	first, images := len(out.items), 0
	for i, raw := range blocks {
		var b codexBlock
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		switch b.Type {
		case "input_text", "output_text", "text":
			text := strings.TrimSpace(b.Text)
			if text == "" || (p.Role == "user" && codexInjected(text)) {
				continue
			}
			kind := "user"
			if p.Role == "assistant" {
				kind = "agent"
			}
			out.items = append(out.items, chatItem{Kind: kind, ID: rowID(id, i), At: at, Text: text})
		case "input_image":
			images++
		}
	}
	// A turn that was only a pasted picture still happened.
	if p.Role == "user" && images > 0 && len(out.items) == first {
		word := "images"
		if images == 1 {
			word = "image"
		}
		out.items = append(out.items, chatItem{
			Kind: "user", ID: id, At: at,
			Text: fmt.Sprintf("[%d %s attached]", images, word),
		})
	}
}

// codexInjected reports text Codex put in the user's turn itself. It wraps
// nearly all of it in a tag (<environment_context>, <user_instructions>,
// <recommended_plugins>, the <image name=…> and </image> pair around a pasted
// picture); AGENTS.md and its own image-read failures are the exceptions.
func codexInjected(text string) bool {
	if strings.HasPrefix(text, "# AGENTS.md instructions") ||
		strings.HasPrefix(text, "Codex could not read the local image") {
		return true
	}
	return len(text) > 1 && text[0] == '<' && (text[1] == '/' || (text[1] >= 'a' && text[1] <= 'z'))
}

// ---------------------------------------------------------------------------
// tools
// ---------------------------------------------------------------------------

var (
	// codexInnerToolRe finds the tools a JavaScript cell calls.
	codexInnerToolRe = regexp.MustCompile(`\btools\.([A-Za-z0-9_]+)\s*\(`)
	codexWallRe      = regexp.MustCompile(`Wall time:? ([0-9.]+) seconds`)
	codexExitRe      = regexp.MustCompile(`Process exited with code (-?[0-9]+)`)
)

// codexArgRe matches `key: <string literal>` in a cell's code, in any of the
// three quote styles.
func codexArgRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + key + `\s*:\s*("(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'|` + "`[^`]*`" + `)`)
}

var codexArgRes = map[string]*regexp.Regexp{
	"cmd":    codexArgRe("cmd"),
	"path":   codexArgRe("path"),
	"prompt": codexArgRe("prompt"),
	"query":  codexArgRe("query"),
}

// codexArg is the first string literal given for key in a cell's code.
func codexArg(code, key string) string {
	m := codexArgRes[key].FindStringSubmatch(code)
	if m == nil {
		return ""
	}
	lit := m[1]
	if lit[0] == '"' {
		if s, err := strconv.Unquote(lit); err == nil {
			return s
		}
	}
	return strings.ReplaceAll(lit[1:len(lit)-1], `\'`, `'`)
}

// codexToolName folds a namespaced tool (image_gen__imagegen) onto the name the
// card tables know.
func codexToolName(name string) string {
	if i := strings.LastIndex(name, "__"); i >= 0 {
		return name[i+2:]
	}
	return name
}

func newCodexTool(callID, name string) *chatTool {
	if name == "" {
		name = "tool"
	}
	return &chatTool{
		CallID: callID,
		Name:   name,
		Title:  toolTitle(name),
		Family: toolFamily(name),
		Group:  toolGroups[strings.ToLower(name)],
		State:  "running",
	}
}

// codexTool builds a card from a call record. nil for a record with nothing to
// show.
func codexTool(p codexPayload, id string) *chatTool {
	callID := p.CallID
	if callID == "" {
		callID = id
	}
	switch p.Type {
	case "custom_tool_call":
		input := rawString(p.Input)
		switch p.Name {
		case "exec":
			return codexExecTool(callID, input)
		case "apply_patch":
			t := newCodexTool(callID, "apply_patch")
			t.Diff, t.Subject = codexPatch(input)
			return t
		}
		t := newCodexTool(callID, codexToolName(p.Name))
		t.Subject = clipLine(input, chatSubjectCap)
		return t
	case "function_call":
		name := codexToolName(p.Name)
		t := newCodexTool(callID, name)
		var args map[string]json.RawMessage
		if json.Unmarshal([]byte(rawString(p.Arguments)), &args) == nil {
			describeTool(t, args)
			if t.Subject == "" {
				var s string
				if json.Unmarshal(args["prompt"], &s) == nil {
					t.Subject = clipLine(s, chatSubjectCap)
				}
			}
		}
		return t
	case "local_shell_call":
		t := newCodexTool(callID, "local_shell")
		var a struct {
			Command []string `json:"command"`
		}
		if json.Unmarshal(p.Action, &a) == nil {
			t.Command = strings.Join(a.Command, " ")
			t.Subject = clipLine(t.Command, chatSubjectCap)
		}
		return t
	case "web_search_call":
		t := newCodexTool(callID, "websearch")
		var a struct {
			Query string `json:"query"`
		}
		if json.Unmarshal(p.Action, &a) == nil {
			t.Subject = clipLine(a.Query, chatSubjectCap)
		}
		// A search is recorded once it is done; nothing follows to finish it.
		t.State = "completed"
		return t
	}
	return nil
}

// codexExecTool builds a card for a JavaScript cell. The card takes its title
// and icon from the first tool the cell calls, and its subject from that call's
// argument: a cell running `cat BRIEF.md` reads "Shell · cat BRIEF.md", not as
// a line of Promise.allSettled.
func codexExecTool(callID, code string) *chatTool {
	calls := codexInnerToolRe.FindAllStringSubmatch(code, -1)
	inner := "exec"
	if len(calls) > 0 {
		inner = codexToolName(calls[0][1])
	}
	t := newCodexTool(callID, inner)
	t.Name = "exec"
	if len(calls) == 0 {
		t.Title, t.Family, t.Group = "Exec", "eval", ""
	}
	t.Command = clipBlock(strings.TrimSpace(code), chatOutputCap)
	switch t.Family {
	case "shell":
		t.Subject = codexArg(code, "cmd")
	case "read", "image":
		if t.Subject = codexArg(code, "path"); t.Subject != "" {
			t.Subject = shortPath(t.Subject)
		} else {
			t.Subject = codexArg(code, "prompt")
		}
	case "web", "search":
		t.Subject = codexArg(code, "query")
	}
	if t.Subject == "" {
		t.Subject = codexFirstLine(code)
	}
	t.Subject = clipLine(t.Subject, chatSubjectCap)
	if n := len(calls); n > 1 && t.Subject != "" {
		t.Subject = clipLine(fmt.Sprintf("%s (+%d more)", t.Subject, n-1), chatSubjectCap)
	}
	return t
}

// codexFirstLine is a cell's first line of actual code: its `// @exec:` pragma
// and comments say nothing about what it does.
func codexFirstLine(code string) string {
	for _, ln := range strings.Split(code, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" && !strings.HasPrefix(ln, "//") {
			return ln
		}
	}
	return ""
}

// codexPatch excerpts an apply_patch envelope ("*** Begin Patch", "*** Update
// File: <path>", hunks) and names the first file it touches.
func codexPatch(patch string) ([]chatDiffLine, string) {
	var out []chatDiffLine
	path := ""
	for _, ln := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(ln, "*** "):
			for _, p := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: "} {
				if rest, ok := strings.CutPrefix(ln, p); ok && path == "" {
					path = strings.TrimSpace(rest)
				}
			}
		case strings.HasPrefix(ln, "+"):
			out = append(out, chatDiffLine{Kind: "add", Text: ln[1:]})
		case strings.HasPrefix(ln, "-"):
			out = append(out, chatDiffLine{Kind: "del", Text: ln[1:]})
		case strings.HasPrefix(ln, "@@"), strings.TrimSpace(ln) == "":
		default:
			out = append(out, chatDiffLine{Kind: "context", Text: strings.TrimPrefix(ln, " ")})
		}
	}
	return capDiff(out), clipLine(shortPath(path), chatSubjectCap)
}

// codexFinish folds a tool's output into its card. Codex prefixes the output
// with a header of its own ("Script completed\nWall time 0.8 seconds\nOutput:",
// or for a plain command "Chunk ID: …\nProcess exited with code 1\n…Output:"),
// which is where the duration and the failure live; the body is what follows.
func codexFinish(t *chatTool, raw json.RawMessage) {
	body, images := codexOutput(raw)
	isErr, dur := false, 0
	// An older Codex wrapped a command's output in a JSON envelope.
	if trimmed := strings.TrimSpace(body); strings.HasPrefix(trimmed, "{") {
		var env struct {
			Output   *string `json:"output"`
			Metadata struct {
				ExitCode *int    `json:"exit_code"`
				Duration float64 `json:"duration_seconds"`
			} `json:"metadata"`
		}
		if json.Unmarshal([]byte(trimmed), &env) == nil && env.Output != nil {
			body = *env.Output
			isErr = env.Metadata.ExitCode != nil && *env.Metadata.ExitCode != 0
			dur = int(env.Metadata.Duration * 1000)
		}
	}
	head := body
	if len(head) > 600 {
		head = head[:600]
	}
	if strings.HasPrefix(head, "Script failed") {
		isErr = true
	}
	if m := codexWallRe.FindStringSubmatch(head); m != nil {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			dur = int(f * 1000)
		}
	}
	if m := codexExitRe.FindStringSubmatch(head); m != nil && m[1] != "0" {
		isErr = true
	}
	if i := strings.Index(head, "Output:\n"); i >= 0 && (codexExitRe.MatchString(head[:i]) || codexWallRe.MatchString(head[:i])) {
		body = body[i+len("Output:\n"):]
	}
	body = strings.TrimPrefix(strings.TrimSpace(body), "Script error:\n")
	finishTool(t, body, isErr, images)
	if dur > 0 {
		t.DurationMS = dur
	}
}

// codexOutput reads a tool output, which is a string or a list of text and image
// blocks.
func codexOutput(raw json.RawMessage) (string, int) {
	if s := rawString(raw); s != "" {
		return s, 0
	}
	var blocks []codexBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", 0
	}
	var parts []string
	images := 0
	for _, b := range blocks {
		switch b.Type {
		case "input_text", "output_text", "text":
			parts = append(parts, b.Text)
		case "input_image", "image":
			images++
		}
	}
	return strings.Join(parts, "\n"), images
}
