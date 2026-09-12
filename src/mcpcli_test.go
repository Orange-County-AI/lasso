package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The whole point of deriving the flags from the wire: `lasso mcp` lists exactly
// what the server registered, so a tool added to mcp_tools.go needs no second
// edit here to be callable.
func TestMCPCLIListsTheServedTools(t *testing.T) {
	srv := httptest.NewServer(withMCPAuth(newMCPHandler(), "", "", false))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := dialMCP(ctx, srv.URL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	tools, err := listMCPTools(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) == 0 {
		t.Fatal("no tools listed")
	}
	for _, want := range []string{"list_hosts", "create_agent", "notify", "whoami"} {
		if findMCPTool(tools, want) == nil {
			t.Errorf("tool %q missing from the listing", want)
		}
	}
	// Sorted, so the listing doesn't follow registration order.
	for i := 1; i < len(tools); i++ {
		if tools[i-1].Name > tools[i].Name {
			t.Fatalf("listing not sorted: %q before %q", tools[i-1].Name, tools[i].Name)
		}
	}
}

// Either spelling resolves: the model-facing docs say list_hosts and every CLI
// listing says list-hosts, so refusing whichever one someone has in front of
// them is a papercut with no upside.
func TestMCPCLIAcceptsBothToolSpellings(t *testing.T) {
	tools := []*mcp.Tool{{Name: "list_hosts"}, {Name: "create_agent"}}
	for _, typed := range []string{"list_hosts", "list-hosts", "LIST-HOSTS", " list-hosts "} {
		if got := findMCPTool(tools, typed); got == nil || got.Name != "list_hosts" {
			t.Errorf("findMCPTool(%q) = %v", typed, got)
		}
	}
	if findMCPTool(tools, "list_host") != nil {
		t.Error("a near miss must not resolve")
	}
}

// An omitted flag has to be ABSENT from the call, not sent as a zero value: the
// tools distinguish the two (an empty host means "search every host I may
// reach", not "the host named empty string").
func TestMCPCLIOmitsUnsetFlags(t *testing.T) {
	tool := &mcp.Tool{Name: "list_agents", InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"host":  map[string]any{"type": "string"},
			"limit": map[string]any{"type": "integer"},
			"focus": map[string]any{"type": "boolean"},
		},
	}}
	full := false
	args, err := parseToolArgs(tool, []string{"-host", "gigachad"}, &full)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 1 || args["host"] != "gigachad" {
		t.Fatalf("args = %#v, want only host", args)
	}
}

// Values are coerced to the schema's type, so a tool that declares an integer
// never receives the string "3" — the server would refuse it.
func TestMCPCLICoercesToTheSchemaType(t *testing.T) {
	tool := &mcp.Tool{Name: "t", InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"count":    map[string]any{"type": "integer"},
			"ratio":    map[string]any{"type": "number"},
			"focus":    map[string]any{"type": "boolean"},
			"agent_id": map[string]any{"type": "string"},
			"extra_args": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
	}}
	full := false
	args, err := parseToolArgs(tool, []string{
		"-count", "3", "-ratio", "0.5", "-focus",
		"--agent-id", "dk3n97h1oxig",
		"-extra-args", "--model", "-extra-args", "opus",
	}, &full)
	if err != nil {
		t.Fatal(err)
	}
	if args["count"] != int64(3) {
		t.Errorf("count = %#v, want int64(3)", args["count"])
	}
	if args["ratio"] != 0.5 {
		t.Errorf("ratio = %#v", args["ratio"])
	}
	// A boolean written bare must mean true, not "present".
	if args["focus"] != true {
		t.Errorf("focus = %#v", args["focus"])
	}
	// Underscore and kebab are the same field, not two.
	if args["agent_id"] != "dk3n97h1oxig" {
		t.Errorf("agent_id = %#v", args["agent_id"])
	}
	list, ok := args["extra_args"].([]any)
	if !ok || len(list) != 2 || list[0] != "--model" || list[1] != "opus" {
		t.Errorf("extra_args = %#v, want one element per occurrence", args["extra_args"])
	}
}

func TestMCPCLIRejectsBadValues(t *testing.T) {
	tool := &mcp.Tool{Name: "t", InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"count":  map[string]any{"type": "integer"},
			"source": map[string]any{"type": "string", "enum": []any{"visible", "recent"}},
		},
	}}
	full := false
	if _, err := parseToolArgs(tool, []string{"-count", "many"}, &full); err == nil {
		t.Error("a non-numeric integer should be refused")
	}
	if _, err := parseToolArgs(tool, []string{"-source", "scrollback"}, &full); err == nil {
		t.Error("a value outside the enum should be refused")
	}
	// A positional argument is a mistyped flag, not a silently dropped value.
	if _, err := parseToolArgs(tool, []string{"oops"}, &full); err == nil {
		t.Error("a stray positional should be refused")
	}
}

// Required fields are checked locally only to save a round trip — the server
// validates the same schema, so this must mirror it rather than invent a
// stricter rule.
func TestMCPCLIRequiresWhatTheSchemaRequires(t *testing.T) {
	tool := &mcp.Tool{Name: "notify", InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{"type": "string"},
			"title":   map[string]any{"type": "string"},
		},
		"required": []any{"message"},
	}}
	full := false
	_, err := parseToolArgs(tool, []string{"-title", "hi"}, &full)
	if err == nil || !strings.Contains(err.Error(), "-message") {
		t.Fatalf("err = %v, want it to name the missing flag", err)
	}
	if _, err := parseToolArgs(tool, []string{"-message", "ready"}, &full); err != nil {
		t.Fatalf("with the required flag: %v", err)
	}
}

// -json is a global, and globals have to lead because flag parsing stops at the
// tool name. Accepting it after the tool name too is what removes that papercut.
func TestMCPCLIAcceptsJSONAfterTheToolName(t *testing.T) {
	tool := &mcp.Tool{Name: "list_hosts", InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"refresh": map[string]any{"type": "boolean"}},
	}}
	full := false
	args, err := parseToolArgs(tool, []string{"-refresh", "-json"}, &full)
	if err != nil {
		t.Fatal(err)
	}
	if !full {
		t.Error("-json after the tool name should set the envelope flag")
	}
	if _, leaked := args["json"]; leaked {
		t.Error("-json must not be sent to the tool as an argument")
	}
}

// A tool that declares its own `json` field keeps it: the convenience alias may
// never shadow a real argument.
func TestMCPCLIJSONAliasYieldsToASchemaField(t *testing.T) {
	tool := &mcp.Tool{Name: "t", InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"json": map[string]any{"type": "string"}},
	}}
	full := false
	args, err := parseToolArgs(tool, []string{"-json", "{}"}, &full)
	if err != nil {
		t.Fatal(err)
	}
	if full {
		t.Error("the schema's own json field was treated as the envelope flag")
	}
	if args["json"] != "{}" {
		t.Errorf("json = %#v, want the schema field to receive it", args["json"])
	}
}

// -h prints the schema-derived help and exits cleanly, so the caller can tell it
// from a usage error (which exits 2 with a message).
func TestMCPCLIHelpIsNotAnError(t *testing.T) {
	tool := &mcp.Tool{Name: "t", InputSchema: map[string]any{"type": "object"}}
	full := false
	_, err := parseToolArgs(tool, []string{"-h"}, &full)
	if err != flag.ErrHelp {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
}

// Structured output is the payload when there is one: every lasso tool has a
// typed Out, and the text content beside it is the same data rendered for a
// model, so printing both would double every answer.
func TestMCPCLIRendersStructuredOutput(t *testing.T) {
	res := &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: `{"active":"local"}`}},
		StructuredContent: map[string]any{"active": "local"},
	}
	out, isErr := renderToolResult(res, false, false)
	if isErr || out != `{"active":"local"}` {
		t.Fatalf("out = %q, isErr = %v", out, isErr)
	}
	if got, _ := renderToolResult(res, false, true); !strings.Contains(got, "\n  ") {
		t.Errorf("indented output = %q, want it indented on a terminal", got)
	}
	// -json asks for the envelope, which carries isError and the content blocks.
	if got, _ := renderToolResult(res, true, false); !strings.Contains(got, "structuredContent") {
		t.Errorf("envelope = %q", got)
	}
}

// A tool that refused is not a transport failure: the text goes to stderr and
// the exit code is 1, which is what a script tells them apart by.
func TestMCPCLIReportsAToolError(t *testing.T) {
	res := &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: "host \"nope\" not available"}},
	}
	out, isErr := renderToolResult(res, false, false)
	if !isErr || !strings.Contains(out, "not available") {
		t.Fatalf("out = %q, isErr = %v", out, isErr)
	}
}

// Text-only results still print: a tool with no typed Out is not a tool with no
// answer.
func TestMCPCLIFallsBackToTextContent(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}
	if out, _ := renderToolResult(res, false, false); out != "done" {
		t.Fatalf("out = %q", out)
	}
}

// End to end against the real handler: the flags reach the tool and its typed
// output comes back. whoami with no pane resolves to found:false, which needs no
// herdr and is the honest answer rather than an error.
func TestMCPCLICallsAToolEndToEnd(t *testing.T) {
	openTestDB(t)
	srv := httptest.NewServer(withMCPAuth(newMCPHandler(), "", "", false))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := dialMCP(ctx, srv.URL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	tools, err := listMCPTools(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	tool := findMCPTool(tools, "whoami")
	if tool == nil {
		t.Fatal("whoami not served")
	}
	full := false
	args, err := parseToolArgs(tool, nil, &full)
	if err != nil {
		t.Fatal(err)
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	out, isErr := renderToolResult(res, false, false)
	if isErr {
		t.Fatalf("whoami errored: %s", out)
	}
	if !strings.Contains(out, `"found":false`) {
		t.Fatalf("out = %q, want found:false for a caller with no pane", out)
	}
}

// Only a LEADING help token counts. A value further along may legitimately be
// the string "-h" (a flag taking free text), and printing usage instead of
// calling the tool would be a silent wrong answer.
func TestMCPCLIHelpOnlyCountsFirst(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}} {
		if !wantsHelp(args) {
			t.Errorf("wantsHelp(%v) = false", args)
		}
	}
	for _, args := range [][]string{nil, {"-host", "local"}, {"-message", "-h"}, {"-message", "help"}} {
		if wantsHelp(args) {
			t.Errorf("wantsHelp(%v) = true", args)
		}
	}
}

// A parse failure the flag package already wrote to stderr comes back as the
// sentinel, so the caller does not print a second copy of it under a screen of
// usage — two copies read as two different problems.
func TestMCPCLIDoesNotDoubleReportParseErrors(t *testing.T) {
	tool := &mcp.Tool{Name: "t", InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"host": map[string]any{"type": "string"}},
	}}
	full := false
	_, err := parseToolArgs(tool, []string{"-nope", "x"}, &full)
	if !errors.Is(err, errFlagReported) {
		t.Fatalf("err = %v, want the already-reported sentinel", err)
	}
	// Our own validation errors are NOT the sentinel — nothing has printed them.
	_, err = parseToolArgs(tool, []string{"stray"}, &full)
	if err == nil || errors.Is(err, errFlagReported) {
		t.Fatalf("err = %v, want a message the caller still has to print", err)
	}
}

// A blocking tool's own timeout has to outrank the CLI's transport deadline:
// wait_agent asked to wait three minutes, cut off at two, reports a bare
// "context deadline exceeded" for a call that was going fine.
func TestMCPCLIExtendsTheDeadlineForABlockingTool(t *testing.T) {
	cases := []struct {
		name string
		argv map[string]any
		want time.Duration
	}{
		{"ms", map[string]any{"timeout_ms": int64(180000)}, 180 * time.Second},
		{"seconds", map[string]any{"timeout_seconds": int64(90)}, 90 * time.Second},
		{"float", map[string]any{"timeout_ms": float64(5000)}, 5 * time.Second},
		// No unit in the name: ms is what every timeout field lasso serves uses,
		// and reading ms as seconds would hang for an hour.
		{"bare", map[string]any{"timeout": int64(2000)}, 2 * time.Second},
		{"longest wins", map[string]any{"timeout_ms": int64(1000), "idle_timeout_ms": int64(9000)}, 9 * time.Second},
		{"none", map[string]any{"host": "local"}, 0},
		// A non-numeric field that happens to be named for a timeout is ignored
		// rather than guessed at.
		{"not a number", map[string]any{"timeout_ms": "soon"}, 0},
	}
	for _, tc := range cases {
		if got := toolSelfTimeout(tc.argv); got != tc.want {
			t.Errorf("%s: toolSelfTimeout = %v, want %v", tc.name, got, tc.want)
		}
	}
}
