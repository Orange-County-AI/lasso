package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	// chatPageExtendTries bounds how far a page may grow backwards to keep a
	// tool call with its result. Three doublings is 4 MiB, far past the
	// adjacent records this exists for.
	chatPageExtendTries = 3
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
	// off is where in the transcript this row's record begins. Not on the wire:
	// it is what makes paging exact (the oldest KEPT row is the cursor the next
	// page is fetched before), and a row dropped by the per-read cap must not
	// silently take the rows beneath it out of reach.
	off int64
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
	Cwd string `json:"cwd,omitempty"`
	// Path is the transcript these rows came from. A client accumulating pages
	// uses it to notice the pane has started a DIFFERENT session and start over,
	// rather than splicing two conversations into one list.
	Path string `json:"path,omitempty"`
	// StartOffset is where this window begins in that transcript. Fetch the page
	// above it with ?before=<StartOffset>.
	StartOffset int64      `json:"start_offset"`
	Items       []chatItem `json:"items"`
	// Tokens is the newest assistant turn's prompt size, the honest half of a
	// context meter: the window size is the model's, and lasso does not guess
	// it.
	Tokens int `json:"tokens,omitempty"`
	// Running is true while the agent is still working on the newest turn, so
	// the view can show a working indicator without inferring one from a clock.
	// Two sources, because neither covers the whole turn: herdr's own pane
	// status is authoritative and current, and the transcript's newest turn says
	// whether a call it made is still outstanding.
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

// chatTranscript is where a pane's conversation is read from, and which reader
// understands it.
type chatTranscript struct {
	Path string
	// Harness selects the record reader. Every harness writes its own shape —
	// omp logs `message` records with toolCall blocks, claude logs `user` and
	// `assistant` records with tool_use ones — so the file alone does not say
	// how to read it.
	Harness string
	// Note explains an unreadable session to the person looking at the empty
	// view.
	Note string
}

// paneTranscript resolves the pane's agent transcript from herdr's own
// agent_session — the handle herdr itself resumes the pane with.
//
// Two shapes, and the difference is not cosmetic. kind="path" is a file herdr
// named outright. kind="id" is a session IDENTIFIER, and only the harness that
// minted it can say where its log lives: for claude that is
// ~/.claude/projects/<slug>/<id>.jsonl, which lasso already resolves for the
// file viewer's cwd (findClaudeTranscript). Refusing the id outright is what
// left Claude Code panes unreadable while their transcripts sat on disk.
func paneTranscript(b Backend, p pane) chatTranscript {
	s := p.AgentSession
	if s == nil {
		return chatTranscript{Note: "No agent session in this pane."}
	}
	// An agent_session outlives the agent (herdr keeps it to resume the pane),
	// so the pane must still be running one — otherwise a plain shell sitting in
	// the directory of an exited agent would keep showing that session.
	if !paneHasLiveAgent(p) {
		return chatTranscript{Note: "No agent session in this pane."}
	}
	agent := strings.ToLower(strings.TrimSpace(s.Agent))
	v := strings.TrimSpace(s.Value)
	switch s.Kind {
	case "path":
		if !filepath.IsAbs(v) || !strings.HasSuffix(v, ".jsonl") {
			return chatTranscript{Note: "This agent's transcript is not readable by lasso yet."}
		}
		return chatTranscript{Path: v, Harness: agent}
	case "id":
		if agent == "claude" {
			if id := safeSessionID(v); id != "" {
				if path := findClaudeTranscript(b, id, "", p); path != "" {
					return chatTranscript{Path: path, Harness: agent}
				}
			}
			// The id is real but its log is not on this machine yet — the session
			// has not written one, or it lives on the other side of an ssh hop.
			return chatTranscript{Note: "This session's transcript is not on this host yet."}
		}
		return chatTranscript{Note: "This agent's transcript is not readable by lasso yet."}
	}
	return chatTranscript{Note: "No agent session in this pane."}
}

// ---------------------------------------------------------------------------
// parsing
// ---------------------------------------------------------------------------

// chatParse is the result of reading one transcript.
type chatParse struct {
	items []chatItem
	// startOffset is the transcript offset of the OLDEST row returned. The next
	// page is "everything before this", so it stays exact even when this read
	// dropped rows to fit chatMaxItems.
	startOffset int64
	title       string
	model       string
	tokens      int
	run         bool
	more        bool
	note        string
	// pendingResults is how many tool results this window could not pair with
	// their call. Non-zero means the window's start fell between a call and its
	// answer.
	pendingResults int
	// running maps a call this window left unfinished to the card that owns it,
	// so the answer can be looked for PAST the window's end. A call and its
	// result are separate records and the window ends between them whenever the
	// end is a page cursor: reading further back cannot help, because the result
	// is forwards. The map holds the same *chatTool the item does, so applying a
	// result through it updates the row on screen.
	running map[string]*chatTool
}

// parseChatTranscript turns a window of an omp session transcript into chat
// rows. base is where that window starts in the file, so every row can say
// where it came from. The first line of a window is usually a fragment; it
// simply fails to parse, like any other line that isn't a JSON object.
func parseChatTranscript(data []byte, base int64) chatParse {
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

	off := int64(0)
	for _, raw := range lines {
		lineStart := base + off
		off += int64(len(raw)) + 1 // the newline bytes.Split took away
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		// Stamped once for the whole record, after the switch below: every row
		// a record produces starts where that record does, and one place is one
		// chance to forget it rather than five.
		first := len(out.items)
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
		for i := first; i < len(out.items); i++ {
			out.items[i].off = lineStart
		}
	}

	// Cap by dropping the oldest rows — but never inside a RECORD. Rows from one
	// record share its offset, so a cut mid-record would report a cursor naming
	// that record's start, and the next page (everything BEFORE the cursor)
	// would exclude the record entirely: the blocks above the cut would be
	// unreachable, not merely deferred.
	if len(out.items) > chatMaxItems {
		keep := len(out.items) - chatMaxItems
		for keep > 0 && out.items[keep].off == out.items[keep-1].off {
			keep--
		}
		out.items = out.items[keep:]
		out.more = true
	}
	// Leftovers are results whose call was NOT in this window. The next page
	// (or the tail before it) holds that call, and would render its card as
	// still running forever, so serveChat reads a bigger window rather than
	// shipping a page that cannot be completed.
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
// claude code transcripts
// ---------------------------------------------------------------------------

// Claude Code logs a DIFFERENT shape from omp's, in a different place, and the
// differences are all load-bearing:
//
//   - the record type is the role ("user" / "assistant"), and the log also
//     carries bookkeeping records that are not conversation at all (mode,
//     permission-mode, file-history-snapshot, ai-title, queue-operation, ...)
//   - a message's content is a bare STRING on a plain user turn and an array of
//     blocks everywhere else
//   - a tool result is not a message of its own: it arrives as a `tool_result`
//     block on a USER record, keyed by the call's tool_use_id
//   - subagent traffic shares the parent's log, flagged isSidechain
//
// The item model, the cards, paging and everything downstream are shared; only
// the record reader differs.
type claudeRecord struct {
	Type        string         `json:"type"`
	UUID        string         `json:"uuid"`
	Timestamp   string         `json:"timestamp"`
	AITitle     string         `json:"aiTitle"`
	IsSidechain bool           `json:"isSidechain"`
	IsMeta      bool           `json:"isMeta"`
	Message     *claudeMessage `json:"message"`
}

type claudeMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Usage      json.RawMessage `json:"usage"`
}

// claudeBlock covers every block claude writes into a content array: text and
// thinking on the assistant side, tool_use for a call, tool_result for an
// answer. One struct because they are mutually exclusive per block.
type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`          // tool_use
	Name      string          `json:"name"`        // tool_use
	Input     json.RawMessage `json:"input"`       // tool_use
	ToolUseID string          `json:"tool_use_id"` // tool_result
	Content   json.RawMessage `json:"content"`     // tool_result: string or blocks
	IsError   json.RawMessage `json:"is_error"`
}

// claudeBlocks normalizes a message's content, which claude writes as a bare
// string on a plain user turn and as an array of blocks everywhere else. A blank
// string is not a row — claude emits empty text and thinking blocks (a thinking
// block can carry a signature with no prose).
func claudeBlocks(raw json.RawMessage) []claudeBlock {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []claudeBlock{{Type: "text", Text: s}}
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	return blocks
}

// claudeResultBody reads a tool_result's content, which is a string for text and
// an array when the tool returned images alongside it.
func claudeResultBody(raw json.RawMessage) (body string, images int) {
	if len(raw) == 0 {
		return "", 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, 0
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", 0
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			images++
		}
	}
	return strings.Join(parts, "\n"), images
}

// parseClaudeTranscript reads a window of a Claude Code session log.
//
// Sidechain records are skipped rather than shown: they are a subagent's own
// conversation, interleaved into the parent's log by timestamp, and rendering
// them inline would put one agent's turns inside another's. The parent's Task
// card still carries the call and its result, which is the part the parent's
// conversation is actually about.
func parseClaudeTranscript(data []byte, base int64) chatParse {
	var out chatParse
	byCall := map[string]*chatTool{}
	type pendingResult struct {
		body    string
		isError bool
		images  int
	}
	pending := map[string]pendingResult{}
	lastStop := ""

	off := int64(0)
	for _, raw := range bytes.Split(data, []byte("\n")) {
		lineStart := base + off
		off += int64(len(raw)) + 1 // the newline bytes.Split took away
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec claudeRecord
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		// Claude titles its own sessions; the header reads better for it.
		if rec.Type == "ai-title" {
			if strings.TrimSpace(rec.AITitle) != "" {
				out.title = strings.TrimSpace(rec.AITitle)
			}
			continue
		}
		if rec.Type != "user" && rec.Type != "assistant" {
			continue
		}
		if rec.IsSidechain || rec.IsMeta || rec.Message == nil {
			continue
		}
		first := len(out.items)
		id := rec.UUID
		role := rec.Message.Role
		if role == "" {
			role = rec.Type
		}
		// Counted so a turn that was ONLY a pasted screenshot is not a gap in the
		// conversation: claude writes the image as a block with no text beside
		// it, and a record that contributes no row at all reads as a message that
		// never happened.
		recordImages := 0
		if role == "assistant" {
			if rec.Message.Model != "" {
				out.model = rec.Message.Model
			}
			// The prompt size is the whole context sent: fresh tokens plus both
			// cache directions, which is what claude itself counts against the
			// window.
			if n := rawNumber(rec.Message.Usage, "input_tokens") +
				rawNumber(rec.Message.Usage, "cache_read_input_tokens") +
				rawNumber(rec.Message.Usage, "cache_creation_input_tokens"); n > 0 {
				out.tokens = n
			}
			if rec.Message.StopReason != "" {
				lastStop = rec.Message.StopReason
			}
		}
		for i, b := range claudeBlocks(rec.Message.Content) {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				kind := "user"
				if role == "assistant" {
					kind = "agent"
				}
				out.items = append(out.items, chatItem{
					Kind: kind, ID: rowID(id, i), At: rec.Timestamp,
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
			case "tool_use":
				t := claudeTool(b)
				byCall[callKey(t.CallID)] = t
				out.items = append(out.items, chatItem{Kind: "tool", ID: t.CallID, Tool: t})
				if res, ok := pending[callKey(t.CallID)]; ok {
					delete(pending, callKey(t.CallID))
					finishTool(t, res.body, res.isError, res.images)
				}
			case "tool_result":
				body, images := claudeResultBody(b.Content)
				key := callKey(b.ToolUseID)
				if t, ok := byCall[key]; ok {
					finishTool(t, body, rawTrue(b.IsError), images)
					continue
				}
				pending[key] = pendingResult{body: body, isError: rawTrue(b.IsError), images: images}
			case "image":
				recordImages++
			}
		}
		// A user turn that carried an image and nothing else still happened, and
		// says so rather than vanishing. Deliberately not attributed to the
		// tool-card path: this is the human's own paste, not a tool's output.
		if role == "user" && recordImages > 0 && len(out.items) == first {
			word := "images"
			if recordImages == 1 {
				word = "image"
			}
			out.items = append(out.items, chatItem{
				Kind: "user", ID: rowID(id, 0), At: rec.Timestamp,
				Text: fmt.Sprintf("[%d %s attached]", recordImages, word),
			})
		}
		for i := first; i < len(out.items); i++ {
			out.items[i].off = lineStart
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
	// A turn that ended by calling a tool is still going; one that ended
	// anywhere else is not.
	out.run = lastStop == "tool_use"
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

// claudeTool builds a card from a tool_use block. The argument names are the
// same ideas omp uses (a shell command, a file path, a pattern), which is what
// lets describeTool serve both.
func claudeTool(b claudeBlock) *chatTool {
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
	if len(b.Input) > 0 {
		var args map[string]json.RawMessage
		if json.Unmarshal(b.Input, &args) == nil {
			describeTool(t, args)
		}
	}
	return t
}

// parseTranscript dispatches on the harness that wrote the log. The harness
// comes from herdr's own agent_session rather than from sniffing the bytes, so a
// log that cannot be parsed is reported as such instead of being read as some
// other harness's format.
func parseTranscript(harness string, data []byte, base int64) chatParse {
	if strings.ToLower(strings.TrimSpace(harness)) == "claude" {
		return parseClaudeTranscript(data, base)
	}
	return parseChatTranscript(data, base)
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

// finishTool folds a finished tool into its card. Shape-neutral on purpose: omp
// and claude report the same facts — a body, an error flag, images, a wall time
// — inside different envelopes, and this is the one place that decides what a
// finished card says.
func finishTool(t *chatTool, body string, isError bool, images int) {
	t.State = "completed"
	if isError {
		t.State = "error"
	}
	if images > 0 {
		t.Images += images
	}
	body = strings.TrimSpace(body)
	if t.State == "error" {
		// An errored call shows the failure, not the whole output: the first
		// line is what a reader needs on a phone.
		t.Error = clipLine(body, 200)
		if t.Error == "" {
			t.Error = "failed"
		}
		return
	}
	t.Output = clipBlock(body, chatOutputCap)
	if n := lineCount(body); n > 0 && t.Output != "" {
		t.ResultLine = strconv.Itoa(n) + " lines"
	}
}

// applyToolResult is the omp envelope around finishTool.
func applyToolResult(t *chatTool, m *ompMessage) {
	var texts []string
	images := 0
	for _, b := range decodeBlocks(m.Content) {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
			images++
		}
	}
	finishTool(t, strings.Join(texts, "\n"), rawTrue(m.IsError), images)
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
	// herdr's own view of the pane is what answers "is it generating right now",
	// and it answers BEFORE the transcript can. Both harnesses write a COMPLETE
	// assistant message, so the first seconds of every turn have no record at
	// all: the file's newest assistant record is still the previous turn's, and
	// a stop-reason read alone reports an idle agent for exactly the stretch a
	// human is staring at the view waiting for an answer. paneAgentPresence also
	// recovers a status herdr left empty, from the pane's own title.
	_, paneStatus := paneAgentPresence(p)

	out := chatPayload{
		PaneID: p.PaneID,
		Agent:  p.Agent,
		Host:   be.Name(),
		Cwd:    paneCwd(p),
		Title:  cleanPaneTitle(p.TerminalTitleStripped),
	}
	tx := paneTranscript(be, p)
	if tx.Path == "" {
		// The note distinguishes the situations that matter to whoever is
		// looking at the empty view: an agent that never ran here, a harness
		// lasso cannot read, and a session whose log is not on this host yet.
		out.Note = tx.Note
		writeChat(w, out)
		return
	}
	path := tx.Path
	info, err := be.Stat(path)
	if err != nil || info.IsDir() {
		out.Note = "The agent's transcript is not readable yet."
		writeChat(w, out)
		return
	}
	// `before` asks for the window ENDING at that transcript offset — the page
	// above the one already on screen. Absent (or out of range) means the tail,
	// which is what a view opens on.
	end := info.Size()
	if v := r.URL.Query().Get("before"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && n < end {
			end = n
		}
	}
	start := end - chatReadBytes
	if start < 0 {
		start = 0
	}
	// A window that starts between a tool call and the result that answers it
	// parses the result with no call to attach it to, and that result is then
	// gone: the call is in the page below, which renders as still running
	// forever. So read wider until the window can be parsed whole. Bounded —
	// a call and its answer are adjacent records, so one or two windows always
	// covers them, and the loop stops at the start of the file regardless.
	var parsed chatParse
	for tries := 0; ; tries++ {
		parsed = parseTranscript(tx.Harness, readChatRange(be, path, start, end), start)
		if parsed.pendingResults == 0 || start == 0 || tries >= chatPageExtendTries {
			break
		}
		start -= chatReadBytes
		if start < 0 {
			start = 0
		}
	}
	// A page whose end is a cursor can split a call from its answer; the window
	// above already holds that answer, but this page owns the row.
	resolveForwardResults(be, path, tx.Harness, end, info.Size(), parsed.running)
	out.Items = parsed.items
	out.Model = parsed.model
	out.Tokens = parsed.tokens
	// Either source is enough to say the agent is working: herdr because it
	// watches the pane, the transcript because a call it recorded is still
	// unanswered. herdr's "idle" deliberately does not override an outstanding
	// call — the result record is written only once the call returns, so there
	// is a window where the file still shows the call and the agent is done with
	// it, and a card that flickers back to finished reads worse than a spare
	// "working" beat.
	out.Running = parsed.run || paneStatus == "working"
	out.More = parsed.more || parsed.startOffset > 0
	out.StartOffset = parsed.startOffset
	// The transcript itself, so a client accumulating pages can tell "more of
	// this session" from "a different session in the same pane" and start over
	// instead of splicing two conversations together.
	out.Path = path
	out.Note = parsed.note
	if parsed.title != "" {
		out.Title = parsed.title
	}
	if out.Agent == "" {
		out.Agent = "agent"
	}
	writeChat(w, out)
}

// writeChat answers a chat request. Every path funnels through here so that
// `items` is always a LIST: a nil slice marshals to null, the no-transcript
// paths return before anything fills it, and a client that treats the field as
// a list then dies on "not iterable" instead of showing the note explaining why
// there is nothing to show.
func writeChat(w http.ResponseWriter, out chatPayload) {
	if out.Items == nil {
		out.Items = []chatItem{}
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

// readChatRange reads [start, end) of a transcript. end comes from a Stat, and
// the file grows while this runs, so the read is capped rather than trusted to
// be exactly the window that was measured.
func readChatRange(b Backend, path string, start, end int64) []byte {
	f, err := b.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return nil
		}
	}
	n := end - start
	if n <= 0 {
		return nil
	}
	// The window is already bounded by construction (chatReadBytes, grown by
	// chatPageExtendTries); this only guards against a caller that isn't. A
	// tighter cap here would silently TRUNCATE a widened window, which is the
	// very loss it was widened to avoid.
	if max := int64(chatReadBytes) * (chatPageExtendTries + 1); n > max {
		n = max
	}
	data, err := io.ReadAll(io.LimitReader(f, n))
	if err != nil {
		return nil
	}
	return data
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

// resolveForwardResults looks for the answers to calls a window left running,
// in the records AFTER that window's end.
//
// This is what makes a page self-contained. A tool's call and its result are
// separate records, and a page ends at a cursor, so the end lands between them
// routinely: the call parses as still running and the row stays that way for
// good, because every later page only ever looks further back. Reading the
// window wider does not help — the answer is forwards.
//
// Bounded by one window, and skipped entirely when the end IS the end of the
// file, which is the case for every live poll.
func resolveForwardResults(b Backend, path, harness string, from, size int64, running map[string]*chatTool) {
	if len(running) == 0 || from >= size {
		return
	}
	for _, raw := range bytes.Split(readChatRange(b, path, from, from+chatReadBytes), []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		res, ok := forwardResult(harness, line)
		if !ok {
			continue
		}
		t, hit := running[res.key]
		if !hit {
			continue
		}
		finishTool(t, res.body, res.isError, res.images)
		t.DurationMS = res.durationMS
		delete(running, res.key)
		if len(running) == 0 {
			return
		}
	}
}

// forwardResult pulls one tool result out of a single log line, in whichever
// shape the harness that wrote it uses. ok is false for anything else — most
// lines are not results.
func forwardResult(harness string, line []byte) (res struct {
	key        string
	body       string
	isError    bool
	images     int
	durationMS int
}, ok bool) {
	if strings.ToLower(strings.TrimSpace(harness)) == "claude" {
		var rec claudeRecord
		if json.Unmarshal(line, &rec) != nil || rec.Type != "user" || rec.IsSidechain || rec.Message == nil {
			return res, false
		}
		for _, b := range claudeBlocks(rec.Message.Content) {
			if b.Type != "tool_result" || b.ToolUseID == "" {
				continue
			}
			body, images := claudeResultBody(b.Content)
			res.key, res.body, res.images = callKey(b.ToolUseID), body, images
			res.isError = rawTrue(b.IsError)
			return res, true
		}
		return res, false
	}
	var rec ompRecord
	if json.Unmarshal(line, &rec) != nil || rec.Message == nil {
		return res, false
	}
	m := rec.Message
	if m.Role != "toolResult" || m.ToolCallID == "" {
		return res, false
	}
	var texts []string
	for _, b := range decodeBlocks(m.Content) {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
			res.images++
		}
	}
	res.key = callKey(m.ToolCallID)
	res.body = strings.Join(texts, "\n")
	res.isError = rawTrue(m.IsError)
	if raw := m.Details; len(raw) > 0 {
		res.durationMS = detailsWallMS(raw)
	}
	return res, true
}
