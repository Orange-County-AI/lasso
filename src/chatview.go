package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Chat view: the focused pane's agent session rendered as a conversation, so a
// phone can read and answer it without a 40-column terminal.
//
// The transcript is the feed, and herdr is what names it: every pane carries
// pane.agent_session, and for omp that is kind="path" — an absolute .jsonl
// lasso can read locally or over SFTP. So this needs no hook, no daemon and no
// per-harness integration: the file the agent already writes is the whole
// thing, and it works for any host lasso can drive.
//
// Nothing here writes. INPUT stays the real TUI — the composer pastes into the
// same pane (see ChatView.tsx) — which is also why a card can honestly say its
// approval is answered in the terminal.
//
// This mirrors the shape Moshi's app renders (see the design notes in
// ChatView.tsx), but the labels are better than Moshi can have: omp stamps a
// written `i` intent on every tool call, so a card reads "Bash · Reading pane
// control help" instead of a truncated command line.

// Caps. The transcript is polled while the view is open, so every one of these
// is a bandwidth decision, not just a layout one.
const (
	// chatReadBytes is the tail window parsed on each request. A session that
	// outgrows it simply shows its most recent history; the item cap below
	// usually binds first for a chatty one.
	chatReadBytes = 512 << 10
	// chatMaxItems is the newest N rows kept after parsing.
	chatMaxItems = 120
	// chatOutputCap bounds one tool result. Long enough for a real command's
	// output, short enough that a `cat` of a large file cannot dominate the
	// payload.
	chatOutputCap = 1400
	// chatDiffHunks/chatDiffLines bound a diff the same way Moshi's renderer
	// does: two hunks of a patch, or a first-N-lines excerpt of a whole file.
	chatDiffHunks = 2
	chatDiffLines = 20
)

// chatItem is one renderable row. Exactly one of Text / Tool / Marker carries
// the content, keyed by Kind.
type chatItem struct {
	Kind string `json:"kind"` // user | agent | tool | marker
	ID   string `json:"id"`
	At   string `json:"at,omitempty"`
	// Text is the prose of a user or agent row.
	Text string `json:"text,omitempty"`
	// Thinking marks an agent row the model wrote to itself: the client folds
	// it behind a disclosure rather than showing it as an answer.
	Thinking bool   `json:"thinking,omitempty"`
	Marker   string `json:"marker,omitempty"` // interrupted | error
	// Count collapses a run of identical markers. A misconfigured provider
	// fails the same way on every turn, and a chat that prints that line two
	// hundred times is a chat nobody can read.
	Count int       `json:"count,omitempty"`
	Tool  *chatTool `json:"tool,omitempty"`
}

// chatTool is a tool call plus the presentation the client renders, already
// truncated to card size. The client never re-derives a subject from the raw
// input: the shapes differ per harness and per tool, and getting that wrong is
// how a card ends up showing a wall of JSON.
type chatTool struct {
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	// Title is the card heading the renderer shows ("Bash", "Edit"). Kept
	// server-side because folding a harness's name for a tool onto one human
	// label is the same table that picks its icon.
	Title string `json:"title"`
	// Family picks the icon and colour (an edit is yellow, a read is blue…),
	// Group is the collapse key: consecutive calls sharing one become a single
	// card. Empty Group means the call always renders on its own.
	Family string `json:"family"`
	Group  string `json:"group,omitempty"`
	// Subject is the one line the collapsed card shows.
	Subject string `json:"subject,omitempty"`
	// Command is the shell line, kept for the expanded body only (Subject
	// prefers the model's own intent where there is one).
	Command string `json:"command,omitempty"`
	State   string `json:"state"` // running | completed | error
	// ResultLine is a short right-aligned completion note ("12 lines",
	// "exit 0").
	ResultLine string `json:"result_line,omitempty"`
	// Output is the tool's text result, capped.
	Output string `json:"output,omitempty"`
	// Diff is a pre-built unified-diff excerpt for an edit or write.
	Diff []chatDiffLine `json:"diff,omitempty"`
	// Error is the failure line shown in place of Output on an errored call.
	Error string `json:"error,omitempty"`
	// Images counts image blocks in the result (a screenshot an agent read).
	Images int `json:"images,omitempty"`
	// DurationMS is the wall time omp recorded for the call.
	DurationMS int `json:"duration_ms,omitempty"`
}

// chatDiffLine is one row of a diff excerpt. There is deliberately no line
// number: omp's older patch format states its range once in the "PUT 57.=58"
// header row, and the old/new-string shape carries no numbering at all, so a
// gutter here could only ever be right for one of the two.
type chatDiffLine struct {
	Kind string `json:"kind"` // add | del | context
	Text string `json:"text"`
}

// chatPayload is the /api/chat body.
type chatPayload struct {
	PaneID string `json:"pane_id"`
	Agent  string `json:"agent"`
	// Host is the machine these rows were read from. Carried so a client can
	// address a submission back to THAT host rather than to whichever host its
	// tab has since moved to: pane ids are unique per host only, so a message
	// aimed at "w1:p1" from the wrong host lands in a different agent's pane.
	Host  string `json:"host"`
	Title string `json:"title,omitempty"`
	Model string `json:"model,omitempty"`
	// Cwd is the directory the session is working in, so a relative image an
	// agent wrote into its prose ("![](docs/arch.png)") resolves against the
	// machine and folder it MEANT rather than against lasso's own origin.
	Cwd   string     `json:"cwd,omitempty"`
	Items []chatItem `json:"items"`
	// Tokens is the newest assistant turn's prompt size, the honest half of a
	// context meter: the window size is the model's, and lasso does not guess
	// it.
	Tokens int `json:"tokens,omitempty"`
	// Running is true while the newest turn has produced no stop, so the view
	// can show a working indicator without inferring it from a clock.
	Running bool `json:"running,omitempty"`
	// More is set when the read window cut older rows off.
	More bool `json:"more,omitempty"`
	// Note explains why Items is empty when it is (no transcript yet, an
	// unsupported format) — the view shows it instead of an empty box.
	Note string `json:"note,omitempty"`
}

// ---------------------------------------------------------------------------
// record shapes
// ---------------------------------------------------------------------------

// ompRecord is one line of an omp session transcript. The file is a JSON-lines
// log of mixed record types; only "message" carries conversation, and the
// header records ("session", "title", "model_change") are metadata.
type ompRecord struct {
	Type      string      `json:"type"`
	ID        string      `json:"id"`
	Timestamp string      `json:"timestamp"`
	Message   *ompMessage `json:"message"`
	// Session header fields.
	Title string `json:"title"`
	Model string `json:"model"`
}

type ompMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	StopReason string          `json:"stopReason"`
	ErrMessage string          `json:"errorMessage"`
	Model      string          `json:"model"`
	// The fields below are RawMessage on purpose: omp's own type for them varies
	// with the tool and the version — `details` is a Python-repr string on some
	// tools and a JSON object on others — and a typed field throws the WHOLE
	// record away when the other shape arrives. That is not a theoretical risk:
	// it silently dropped every tool result in a live session, leaving a chat
	// where no card ever finished. Read them through the accessors below.
	IsError json.RawMessage `json:"isError"`
	Details json.RawMessage `json:"details"`
	Usage   json.RawMessage `json:"usage"`
	Context json.RawMessage `json:"contextSnapshot"`
}

// rawTrue reports a JSON boolean true, tolerating the string spelling some
// harnesses use. Anything else (absent, null, a number) is false.
func rawTrue(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "true" || s == `"true"` || s == `"True"`
}

// rawNumber reads a number at key from an object, 0 when the blob is absent,
// of another shape, or lacks the key. Numbers may arrive quoted.
func rawNumber(raw json.RawMessage, key string) int {
	if len(raw) == 0 {
		return 0
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return 0
	}
	v, ok := obj[key]
	if !ok {
		return 0
	}
	return numberValue(v)
}

// numberValue reads a JSON number or a quoted one.
func numberValue(raw json.RawMessage) int {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" {
		return 0
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f)
	}
	return 0
}

// ompBlock is one content block. Only the fields the renderer needs are typed;
// the rest is ignored rather than decoded into a map (this runs per line on a
// file that can be hundreds of kilobytes).
type ompBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ---------------------------------------------------------------------------
// transcript location
// ---------------------------------------------------------------------------

// paneTranscriptPath returns the pane's agent transcript, from herdr's own
// agent_session. kind="path" is the whole contract: herdr resolved the file for
// its harness (that is how it resumes a pane), and the value is the absolute
// path lasso reads. Harnesses that report an id instead — claude, so far — are
// not chat-viewable, and answer "" rather than sending the reader to a guess.
func paneTranscriptPath(p pane) string {
	s := p.AgentSession
	if s == nil || s.Kind != "path" {
		return ""
	}
	// An agent_session outlives the agent (herdr keeps it to resume the pane),
	// so the pane must still be running one — otherwise a plain shell sitting in
	// the directory of an exited agent would keep showing that session.
	if !paneHasLiveAgent(p) {
		return ""
	}
	v := strings.TrimSpace(s.Value)
	if !filepath.IsAbs(v) || !strings.HasSuffix(v, ".jsonl") {
		return ""
	}
	return v
}

// ---------------------------------------------------------------------------
// parsing
// ---------------------------------------------------------------------------

// chatParse is the result of reading one transcript.
type chatParse struct {
	items  []chatItem
	title  string
	model  string
	tokens int
	run    bool
	more   bool
	note   string
}

// parseChatTranscript turns a tail of an omp session transcript into chat rows.
// The first line of a tail read is usually a fragment; it simply fails to parse,
// like any other line that isn't a JSON object.
func parseChatTranscript(data []byte) chatParse {
	var out chatParse
	lines := bytes.Split(data, []byte("\n"))

	// Tool results routinely land before the call they answer (the call is
	// written when the model finishes streaming it, the result when the tool
	// returns), so results are held here until their call appears. Without this
	// the transcript reads as a result floating above its own card.
	//
	// Keyed by callKey, not by the block id: a provider-backed call is stored as
	// "<call id>|<response item id>" while its result names only the part before
	// the pipe, so matching on the raw id silently leaves those calls running
	// forever.
	byCall := map[string]*chatTool{}
	pending := map[string]*ompMessage{}

	appendTool := func(t *chatTool) {
		out.items = append(out.items, chatItem{Kind: "tool", ID: t.CallID, Tool: t})
	}

	for _, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec ompRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Type == "session" || rec.Type == "title" || rec.Type == "title_change" {
			if rec.Title != "" {
				out.title = rec.Title
			}
			continue
		}
		m := rec.Message
		if m == nil {
			continue
		}
		id := rec.ID
		switch m.Role {
		case "user":
			for i, b := range decodeBlocks(m.Content) {
				if b.Type != "text" || strings.TrimSpace(b.Text) == "" {
					continue
				}
				out.items = append(out.items, chatItem{
					Kind: "user", ID: rowID(id, i), At: rec.Timestamp,
					Text: strings.TrimSpace(b.Text),
				})
			}
		case "assistant":
			if m.Model != "" {
				out.model = m.Model
			}
			if n := rawNumber(m.Context, "promptTokens"); n > 0 {
				out.tokens = n
			} else if n := rawNumber(m.Usage, "totalTokens"); n > 0 {
				out.tokens = n
			}
			for i, b := range decodeBlocks(m.Content) {
				switch b.Type {
				case "text":
					if strings.TrimSpace(b.Text) == "" {
						continue
					}
					out.items = append(out.items, chatItem{
						Kind: "agent", ID: rowID(id, i), At: rec.Timestamp,
						Text: strings.TrimSpace(b.Text),
					})
				case "thinking":
					if strings.TrimSpace(b.Thinking) == "" {
						continue
					}
					out.items = append(out.items, chatItem{
						Kind: "agent", ID: rowID(id, i), At: rec.Timestamp,
						Text: strings.TrimSpace(b.Thinking), Thinking: true,
					})
				case "toolCall":
					t := ompTool(b)
					byCall[callKey(t.CallID)] = t
					appendTool(t)
					if res, ok := pending[callKey(t.CallID)]; ok {
						delete(pending, callKey(t.CallID))
						applyToolResult(t, res)
					}
				}
			}
			// A turn that ended without finishing is worth a row: the transcript
			// is otherwise silent about it, and a chat that just stops reads as
			// a bug. omp's own silent-abort sentinel is not an interruption.
			switch m.StopReason {
			case "aborted":
				if m.ErrMessage != "__omp.silent_abort__" {
					appendMarker(&out, chatItem{
						Kind: "marker", ID: rowID(id, 900), At: rec.Timestamp,
						Marker: "interrupted",
					})
				}
			case "error":
				if strings.TrimSpace(m.ErrMessage) != "" {
					appendMarker(&out, chatItem{
						Kind: "marker", ID: rowID(id, 901), At: rec.Timestamp,
						Marker: "error", Text: strings.TrimSpace(m.ErrMessage),
					})
				}
			}
			out.run = m.StopReason == "" || m.StopReason == "toolUse"
		case "toolResult":
			if t, ok := byCall[callKey(m.ToolCallID)]; ok {
				applyToolResult(t, m)
				continue
			}
			if m.ToolCallID != "" {
				pending[callKey(m.ToolCallID)] = m
			}
		}
	}

	// Cap by dropping the oldest rows, and say so — the view offers no paging,
	// so a silent truncation would look like a short session.
	if len(out.items) > chatMaxItems {
		out.items = out.items[len(out.items)-chatMaxItems:]
		out.more = true
	}
	for i := range out.items {
		if out.items[i].Tool != nil && out.items[i].Tool.State == "running" {
			out.run = true
		}
	}
	if len(out.items) == 0 && out.note == "" {
		out.note = "No messages yet."
	}
	return out
}

func decodeBlocks(raw json.RawMessage) []ompBlock {
	if len(raw) == 0 {
		return nil
	}
	var blocks []ompBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

// callKey is the identity a tool result is matched by. A provider-backed call
// is stored as "<call id>|<response item id>" (a model served through two
// transports names its own response item), while the result that answers it
// carries only the call id — so the part before the pipe is the shared key.
func callKey(id string) string {
	if i := strings.IndexByte(id, '|'); i >= 0 {
		return id[:i]
	}
	return id
}

// rowID names an item after the record that produced it. Record ids are unique
// per session and stable across re-reads, which is what lets the client keep a
// card's expansion state while the transcript grows underneath it (React keys,
// and the same reason the renderer memoises on a tool's revision).
func rowID(recID string, block int) string {
	if recID == "" {
		return "b" + strconv.Itoa(block)
	}
	if block == 0 {
		return recID
	}
	return recID + ":" + strconv.Itoa(block)
}

// appendMarker adds a marker row, folding it into the previous one when they
// are the same kind with the same text. Runs are the norm, not the exception: a
// provider with no API key fails identically on every turn it is tried.
func appendMarker(out *chatParse, it chatItem) {
	if n := len(out.items); n > 0 {
		prev := &out.items[n-1]
		if prev.Kind == "marker" && prev.Marker == it.Marker && prev.Text == it.Text {
			prev.Count++
			return
		}
	}
	it.Count = 1
	out.items = append(out.items, it)
}

// ---------------------------------------------------------------------------
// tool presentation
// ---------------------------------------------------------------------------

// toolFamily maps the tool names lasso sees in practice onto the handful of
// icon/colour families the card renders. Deliberately a name table rather than
// a per-harness switch: a new harness's name for an old idea should land in the
// right family without touching the renderer.
var toolFamilies = map[string]string{
	"bash": "shell", "shell": "shell", "exec_command": "shell",
	"local_shell": "shell", "shell_command": "shell", "terminal": "shell",
	"eval": "eval",
	"read": "read", "read_file": "read", "readfile": "read",
	"write": "write",
	"edit":  "edit", "multiedit": "edit", "apply_patch": "edit", "patch": "edit",
	"grep": "search", "glob": "search", "search": "search", "rg": "search",
	"webfetch": "web", "websearch": "web", "web_fetch": "web", "fetch": "web",
	"todo": "todo", "todowrite": "todo", "todolist": "todo", "update_plan": "todo",
	"task": "task", "agent": "task",
	"view_image": "image", "imagegen": "image", "generate_image": "image",
	"edit_image": "image",
}

// toolGroups is the collapse key for a run of consecutive calls. Two names
// share a key only when reading them as one card loses nothing: a burst of
// Reads, Greps and Globs is one "file" card listing its paths, and a burst of
// Edits is one "edit" card with the combined diff. Bash is its own run because
// every command in it matters; eval is ungrouped because each cell is a
// distinct computation a reader actually wants to see.
var toolGroups = map[string]string{
	"read": "file", "read_file": "file", "readfile": "file",
	"grep": "file", "glob": "file", "search": "file", "rg": "file",
	"write": "file", "edit": "file", "multiedit": "file",
	"apply_patch": "file", "patch": "file",
	"bash": "shell", "shell": "shell", "exec_command": "shell",
	"local_shell": "shell", "shell_command": "shell", "terminal": "shell",
	"webfetch": "web", "websearch": "web", "web_fetch": "web", "fetch": "web",
	"task": "task", "agent": "task",
}

// toolTitles are the card headings. Lower-case variant names are folded in
// because harnesses disagree about capitalisation for the same tool.
var toolTitles = map[string]string{
	"bash": "Bash", "shell": "Shell", "exec_command": "Shell",
	"local_shell": "Shell", "shell_command": "Shell", "terminal": "Shell",
	"eval": "Eval",
	"read": "Read", "read_file": "Read", "readfile": "Read",
	"write": "Write", "edit": "Edit", "multiedit": "Edit",
	"apply_patch": "Patch", "patch": "Patch",
	"grep": "Grep", "glob": "Glob", "search": "Search", "rg": "Grep",
	"webfetch": "Web Fetch", "websearch": "Web Search", "web_fetch": "Web Fetch",
	"fetch": "Web Fetch",
	"todo":  "Tasks", "todowrite": "Tasks", "todolist": "Tasks", "update_plan": "Plan",
	"task": "Task", "agent": "Agent",
	"view_image": "Image", "imagegen": "Image", "generate_image": "Image",
	"edit_image": "Image", "hub": "Hub",
}

func toolFamily(name string) string {
	if f, ok := toolFamilies[strings.ToLower(name)]; ok {
		return f
	}
	return "generic"
}

func toolTitle(name string) string {
	if t, ok := toolTitles[strings.ToLower(name)]; ok {
		return t
	}
	if name == "" {
		return "Tool"
	}
	return name
}

// ompTool builds a tool card from a toolCall block.
func ompTool(b ompBlock) *chatTool {
	name := b.Name
	if name == "" {
		name = "tool"
	}
	t := &chatTool{
		CallID: b.ID,
		Name:   name,
		Title:  toolTitle(name),
		Family: toolFamily(name),
		Group:  toolGroups[strings.ToLower(name)],
		State:  "running",
	}
	if len(b.Arguments) > 0 {
		var args map[string]json.RawMessage
		if err := json.Unmarshal(b.Arguments, &args); err == nil {
			describeTool(t, args)
		}
	}
	return t
}

// describeTool fills Subject/Command/Diff from the call's arguments. Keys are
// read leniently on purpose: omp's own schema is stable but the same idea
// appears under different names across harnesses, and a missing key must
// degrade to a bare title rather than to nothing.
func describeTool(t *chatTool, args map[string]json.RawMessage) {
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := args[k]; ok {
				var s string
				if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	intent := str("i", "intent", "description")
	path := str("path", "file_path", "filePath")

	switch t.Family {
	case "shell":
		t.Command = str("command", "cmd")
		t.Subject = clipLine(intent, chatSubjectCap)
		if t.Subject == "" {
			t.Subject = clipLine(t.Command, chatSubjectCap)
		}
	case "eval":
		lang, title := str("language"), str("title")
		t.Subject = clipLine(intent, chatSubjectCap)
		if t.Subject == "" {
			t.Subject = clipLine(strings.TrimSpace(lang+" "+title), chatSubjectCap)
		}
	case "read", "image":
		t.Subject = clipLine(shortPath(path), chatSubjectCap)
		if t.Subject == "" {
			t.Subject = clipLine(intent, chatSubjectCap)
		}
	case "write":
		t.Subject = clipLine(shortPath(path), chatSubjectCap)
		t.Diff = writeDiff(str("content", "file_text"))
	case "edit":
		// An older omp patch names its file inside the patch, not in an
		// argument, so the header is the fallback for the subject.
		var patchPath string
		t.Diff, patchPath = editDiff(args, str("input"))
		if path == "" {
			path = patchPath
		}
		t.Subject = clipLine(shortPath(path), chatSubjectCap)
	case "search":
		t.Subject = clipLine(str("pattern", "query", "path"), chatSubjectCap)
		if t.Subject == "" {
			t.Subject = clipLine(intent, chatSubjectCap)
		}
	case "web":
		t.Subject = clipLine(str("url", "query"), chatSubjectCap)
	case "todo":
		t.Subject = clipLine(intent, chatSubjectCap)
		if t.Subject == "" {
			t.Subject = clipLine(str("op"), chatSubjectCap)
		}
	case "task":
		t.Subject = clipLine(str("description"), chatSubjectCap)
		if t.Subject == "" {
			t.Subject = clipLine(intent, chatSubjectCap)
		}
	default:
		// Nothing known about this tool: the model's own intent is the best
		// label available, and is always better than a JSON dump.
		t.Subject = clipLine(intent, chatSubjectCap)
	}
}

// chatSubjectCap is the collapsed card's one line. Wide enough for a real
// command, narrow enough that the card never wraps on a phone.
const chatSubjectCap = 88

// ---------------------------------------------------------------------------
// diffs
// ---------------------------------------------------------------------------

var (
	editHeaderRe = regexp.MustCompile(`^\[([^\]#]+)(?:#[^\]]*)?\]\s*$`)
	// omp's patch operations: "PUT 57.=58:", "DELETE 12:". Kept as context rows
	// rather than dropped — the range is what says where a change landed, and a
	// diff without its hunk headers is a list of orphaned lines.
	patchOpRe = regexp.MustCompile(`^[A-Z][A-Z ]{1,12}[0-9.,=<>\- ]*:\s*$`)
)

// editDiff builds a diff excerpt for an edit or patch call, from either shape
// in the wild: an old/new string pair (omp's newer edits), or omp's older
// single-string patch. The latter is a range-replacement format — a
// "[path#anchor]" header, then "PUT <range>:" blocks each followed by the lines
// to write at that range — so its body is additions plus the operation lines,
// with no deletions to show. The path is returned too, because that header is
// the only place an older patch names its file.
func editDiff(args map[string]json.RawMessage, raw string) (out []chatDiffLine, path string) {
	collect := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := args[k]; ok {
				var s string
				if json.Unmarshal(v, &s) == nil {
					return s
				}
			}
		}
		return ""
	}
	old := collect("old_string", "oldString", "old_text")
	nw := collect("new_string", "newString", "new_text")
	if old != "" || nw != "" {
		for _, ln := range splitLines(old, 8) {
			out = append(out, chatDiffLine{Kind: "del", Text: ln})
		}
		for _, ln := range splitLines(nw, 12) {
			out = append(out, chatDiffLine{Kind: "add", Text: ln})
		}
		return capDiff(out), path
	}
	if raw == "" {
		return nil, path
	}
	for i, ln := range strings.Split(raw, "\n") {
		if i == 0 {
			if m := editHeaderRe.FindStringSubmatch(ln); m != nil {
				path = m[1]
			}
			continue
		}
		switch {
		case strings.TrimSpace(ln) == "":
		case patchOpRe.MatchString(ln):
			out = append(out, chatDiffLine{
				Kind: "context",
				Text: strings.TrimSuffix(strings.TrimSpace(ln), ":"),
			})
		case strings.HasPrefix(ln, "+"):
			out = append(out, chatDiffLine{Kind: "add", Text: strings.TrimPrefix(ln, "+")})
		case strings.HasPrefix(ln, "-"):
			out = append(out, chatDiffLine{Kind: "del", Text: strings.TrimPrefix(ln, "-")})
		default:
			out = append(out, chatDiffLine{Kind: "context", Text: ln})
		}
	}
	return capDiff(out), path
}

// capDiff bounds an excerpt and says so, so a truncated card never looks like a
// complete one.
func capDiff(lines []chatDiffLine) []chatDiffLine {
	if len(lines) <= chatDiffLines {
		return lines
	}
	kept := lines[:chatDiffLines]
	return append(kept, chatDiffLine{
		Kind: "context",
		Text: "… " + strconv.Itoa(len(lines)-chatDiffLines) + " more lines",
	})
}

// writeDiff excerpts a whole-file write: the head of the file, then a marker
// saying how much was left out.
func writeDiff(content string) []chatDiffLine {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	out := make([]chatDiffLine, 0, chatDiffLines+1)
	for _, ln := range capStrings(lines, chatDiffLines) {
		out = append(out, chatDiffLine{Kind: "add", Text: ln})
	}
	if len(lines) > chatDiffLines {
		out = append(out, chatDiffLine{
			Kind: "context",
			Text: "… " + strconv.Itoa(len(lines)-chatDiffLines) + " more lines",
		})
	}
	return out
}

func splitLines(s string, limit int) []string {
	if s == "" {
		return nil
	}
	return capStrings(strings.Split(s, "\n"), limit)
}

func capStrings(in []string, n int) []string {
	if n <= 0 || len(in) <= n {
		return in
	}
	return in[:n]
}

// ---------------------------------------------------------------------------
// results
// ---------------------------------------------------------------------------

var wallTimeRe = regexp.MustCompile(`wallTimeMs['"]?\s*:\s*([0-9.]+)`)

// applyToolResult folds a result into its card.
func applyToolResult(t *chatTool, m *ompMessage) {
	t.State = "completed"
	if rawTrue(m.IsError) {
		t.State = "error"
	}
	var texts []string
	for _, b := range decodeBlocks(m.Content) {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
			t.Images++
		}
	}
	body := strings.TrimSpace(strings.Join(texts, "\n"))
	if t.State == "error" {
		// An errored call shows the failure, not the whole output: the first
		// line is what a reader needs on a phone.
		t.Error = clipLine(body, 200)
		if t.Error == "" {
			t.Error = "failed"
		}
	} else {
		t.Output = clipBlock(body, chatOutputCap)
	}
	if n := lineCount(body); n > 0 && t.Output != "" {
		t.ResultLine = strconv.Itoa(n) + " lines"
	}
	if raw := m.Details; len(raw) > 0 {
		t.DurationMS = detailsWallMS(raw)
	}
}

// detailsWallMS reads the tool's wall time out of its details blob, which is
// either a JSON object ({"wallTimeMs": 117.3}) or a Python-repr string
// ("{'timeoutSeconds': 300, 'wallTimeMs': 117.3}"). Neither shape is
// guaranteed, and a missing duration is not worth a wrong one.
func detailsWallMS(raw json.RawMessage) int {
	if n := rawNumber(raw, "wallTimeMs"); n > 0 {
		return n
	}
	if mm := wallTimeRe.FindStringSubmatch(string(raw)); mm != nil {
		return numberValue(json.RawMessage(mm[1]))
	}
	return 0
}

// ---------------------------------------------------------------------------
// endpoint
// ---------------------------------------------------------------------------

// serveChat serves the focused pane's agent session as chat rows.
//
// Read-only, and resolved against THIS tab's host (reqHostBackend) — the pane
// the herdr terminal beside it is showing. It deliberately does not follow
// cwd_host: that is where the focused pane's *files* live, while the chat is
// about the pane herdr itself has, which is always the tab's host.
func serveChat(w http.ResponseWriter, r *http.Request) {
	be, err := reqHostBackend(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	panes, err := panesRaw(be)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	want := r.URL.Query().Get("pane")
	idx := -1
	for i := range panes {
		if want != "" {
			if panes[i].PaneID == want {
				idx = i
				break
			}
		} else if panes[i].Focused {
			idx = i
			break
		}
	}
	if idx < 0 {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	p := panes[idx]

	out := chatPayload{
		PaneID: p.PaneID,
		Agent:  p.Agent,
		Host:   be.Name(),
		Cwd:    paneCwd(p),
		Title:  cleanPaneTitle(p.TerminalTitleStripped),
	}
	path := paneTranscriptPath(p)
	if path == "" {
		// Two different situations, and the difference matters to whoever is
		// looking at the empty view: an agent that has never run here, and one
		// whose harness lasso cannot read yet.
		if p.AgentSession != nil && p.AgentSession.Kind == "id" {
			out.Note = "This agent's transcript is not readable by lasso yet."
		} else {
			out.Note = "No agent session in this pane."
		}
		writeJSON(w, out)
		return
	}
	info, err := be.Stat(path)
	if err != nil || info.IsDir() {
		out.Note = "The agent's transcript is not readable yet."
		writeJSON(w, out)
		return
	}
	data, windowed := readChatTail(be, path, info.Size())
	parsed := parseChatTranscript(data)
	out.Items = parsed.items
	out.Model = parsed.model
	out.Tokens = parsed.tokens
	out.Running = parsed.run
	out.More = windowed || parsed.more
	out.Note = parsed.note
	if parsed.title != "" {
		out.Title = parsed.title
	}
	if out.Agent == "" {
		out.Agent = "agent"
	}
	writeJSON(w, out)
}

// panesRaw lists a host's panes with everything herdr reported. fetchPanes
// joins in the workspace and tab labels the sidebar needs and drops
// agent_session doing it, which is exactly the field the chat is built on.
func panesRaw(be Backend) ([]pane, error) {
	res, err := herdrPaneList(be)
	if err != nil {
		return nil, err
	}
	var pl struct {
		Panes []pane `json:"panes"`
	}
	if err := json.Unmarshal(res, &pl); err != nil {
		return nil, err
	}
	return pl.Panes, nil
}

// readChatTail reads the newest window of a transcript. windowed reports that
// the file is larger than the window, so the reader knows it is looking at a
// session's tail rather than its whole history.
func readChatTail(b Backend, path string, size int64) (data []byte, windowed bool) {
	f, err := b.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if off := size - chatReadBytes; off > 0 {
		windowed = true
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, false
		}
	}
	// The file grows while it is read; cap it so a busy session cannot turn
	// this into an unbounded slurp.
	data, err = io.ReadAll(io.LimitReader(f, 2*chatReadBytes))
	if err != nil {
		return nil, false
	}
	return data, windowed
}

// ---------------------------------------------------------------------------
// sending
// ---------------------------------------------------------------------------

// chatSendResult is what the composer is told about a submission. It is
// three-valued on purpose: a composer that clears its draft on "uncertain"
// loses a message, and one that retries on "uncertain" can duplicate a turn
// that did land. Neither is a call this server can make for the human.
type chatSendResult string

const (
	// chatSent: the paste was observed in the harness's own composer and that
	// composer was then observed EMPTY — a submitted turn, as close to proof as
	// reading a TUI gets.
	chatSent = "confirmed"
	// chatRefused: nothing was written, because lasso positively declined — the
	// pane holds a human's unsent draft. Only ever set from a check that read
	// the pane BEFORE writing to it; a failed write cannot claim this, see below.
	chatRefused = "refused"
	// chatUncertain: bytes may have been written but delivery is unproven.
	// Includes every send-RPC error, because herdrCallSock reports read
	// timeouts and decode failures AFTER conn.Write has already put the request
	// on the socket — an error is not evidence the pane never saw it. MUST NOT
	// be retried automatically.
	chatUncertain = "uncertain"
)

// How long each phase gets. Vars rather than consts so a test can shrink them:
// the real values are seconds and the failure paths are the interesting ones.
var (
	// How long the paste is given to appear in the composer before Enter is
	// pressed anyway.
	chatPasteWait = 3 * time.Second
	// How long Enter is retried while waiting for the composer to clear. A busy
	// TUI can apply the paste after the first Enter.
	chatEnterWait = 5 * time.Second
	// The gap between composer reads inside each phase.
	chatPollWait = 150 * time.Millisecond
)

// chatSubmit types text into an addressed pane and reports how far it got.
//
// Addressed by pane id, NOT by herdr's focus: the chat displays one pane's
// transcript and must not be able to deliver into whatever pane a TUI happened
// to be showing, which a focus-following keystroke cannot promise.
//
// Deliberately not paneSubmit (agents_create.go) even though the gestures are
// the same. That one answers "did the bytes get handed off", discards both RPC
// errors and returns true when its deadline expires — correct for a durable
// message queue, which re-delivers anyway, and wrong here, where the answer
// decides whether a human's draft is cleared or a turn is silently lost. The
// two share the composer reader; only the reporting differs.
func chatSubmit(b Backend, paneID, agentKind, text string) (chatSendResult, string) {
	if composerGuardEnabled() && paneComposerState(b, paneID, agentKind) == ComposerDraft {
		return chatRefused, "that pane has unsent input — send the draft or clear it first"
	}
	if _, err := b.HerdrCall("pane.send_text", map[string]any{
		"pane_id": paneID,
		"text":    text,
	}); err != nil {
		// Deliberately NOT "refused": the request is written to the socket
		// before the reply is read, so a timeout or an unreadable answer can
		// hide a paste that landed. Reporting this as safely-unsent would tell
		// a human to retry a turn that may already be running.
		return chatUncertain, "the pane stopped answering mid-send: " + err.Error()
	}
	// Phase one: watch the message appear. Without this, a composer that was
	// empty all along (a paste that never rendered) is indistinguishable from a
	// submitted turn, and every send would report success.
	landed := false
	for commit := time.Now().Add(chatPasteWait); time.Now().Before(commit); {
		time.Sleep(chatPollWait)
		if paneComposerState(b, paneID, agentKind) == ComposerDraft {
			landed = true
			break
		}
	}
	if !landed {
		return chatUncertain, "the message was written but never appeared in the pane's input box"
	}
	// Phase two: press Enter until the composer reads empty.
	for deadline := time.Now().Add(chatEnterWait); time.Now().Before(deadline); {
		if _, err := b.HerdrCall("pane.send_text", map[string]any{
			"pane_id": paneID,
			"text":    "\r",
		}); err != nil {
			return chatUncertain, "the pane went away mid-submit"
		}
		time.Sleep(2 * chatPollWait)
		if paneInputEmpty(b, paneID, agentKind) {
			return chatSent, ""
		}
	}
	return chatUncertain, "the message is in the input box but Enter was not confirmed"
}

// serveChatSend types a chat message into the pane the transcript belongs to.
// The pane is addressed explicitly and validated against this host's own pane
// list, so the harness geometry comes from herdr rather than from the caller.
func serveChatSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	be, err := reqHostBackend(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var req struct {
		PaneID string `json:"pane_id"`
		Text   string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.PaneID) == "" || strings.TrimSpace(req.Text) == "" {
		http.Error(w, "pane_id and text are required", http.StatusBadRequest)
		return
	}
	panes, err := panesRaw(be)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	kind := ""
	found := false
	for _, p := range panes {
		if p.PaneID == req.PaneID {
			// herdr's own answer, never the caller's claim.
			kind, _ = paneAgentPresence(p)
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	outcome, detail := chatSubmit(be, req.PaneID, kind, req.Text)
	writeJSON(w, map[string]any{"outcome": outcome, "detail": detail})
}

// ---------------------------------------------------------------------------
// text helpers
// ---------------------------------------------------------------------------

// clipLine keeps the first line of s, ellipsised at n. Cards are one line by
// construction — a subject that wraps makes the list unscannable.
func clipLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// clipBlock bounds a multi-line body, keeping its shape (a truncated diff or
// command output still has to line up) and saying that it was cut.
func clipBlock(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…"
}

func lineCount(s string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// cleanPaneTitle drops the agent's own decoration from a pane title. herdr's
// "stripped" variant removes the glyphs its manifest knows about and leaves the
// rest, so omp's title arrives as "π ⠴ Implement chat transcript backend" — and
// the braille frame changes every few hundred milliseconds, which is not
// something a header can show (it would both flicker and rewrite the payload on
// every poll).
//
// A leading token survives only if it carries at least two letters or digits,
// which is what tells a word from decoration: the glyph run is one or two
// single-character tokens ("π", "✳", a spinner frame), while a real first word
// is a word. Titles that merely start short ("Go 1.22 upgrade") keep theirs.
func cleanPaneTitle(s string) string {
	fields := strings.Fields(s)
	for len(fields) > 0 {
		alnum := 0
		for _, r := range fields[0] {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				alnum++
			}
		}
		if alnum >= 2 {
			break
		}
		fields = fields[1:]
	}
	return strings.Join(fields, " ")
}

// shortPath keeps the last three segments of a path, so a card says which file
// without spending its width on a home directory.
func shortPath(p string) string {
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	if len(parts) <= 3 {
		return p
	}
	return "…/" + strings.Join(parts[len(parts)-3:], "/")
}
