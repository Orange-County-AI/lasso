package main

// `lasso mcp` — lasso's own MCP tool surface, as shell commands.
//
//	lasso mcp                    list the tools the running server offers
//	lasso mcp <tool> -h          one tool's flags, from its live schema
//	lasso mcp <tool> [flags]     call it
//
// This is the general case of `lasso notify`, and it exists for the same reason:
// an agent with lasso's MCP server configured calls the tools directly, while one
// that only has a terminal — which is most of them, most of the time — types this
// and gets exactly the same behavior, from exactly the same descriptions.
//
// The flags are built from the inputSchema the SERVER advertises, never from a
// table in this file. The binary you type is not necessarily the binary that is
// answering (a `lasso update` since the shell started, LASSO_URL pointing at
// another box), and a hand-maintained copy of the tool list is precisely the
// drift notify was written as an MCP client to avoid. It also means a tool added
// to mcp_tools.go is on the CLI the moment the server restarts, with no second
// place to edit.
//
// Deliberately NOT a replacement for `lasso notify` or `lasso closeme`: notify's
// exit code is a contract (non-zero when nothing was listening) and it reads a
// piped message, and closeme resolves $HERDR_PANE_ID for you. Both are the short
// spelling of a call this can also make the long way.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// errFlagReported marks a parse failure the flag package has ALREADY written to
// stderr (with the tool's usage after it). The caller exits on it without saying
// anything further — a second copy of "flag provided but not defined" under a
// screen of help reads as two different problems.
var errFlagReported = errors.New("flag error already reported")

// mcpCLITimeout bounds the whole round trip: connect, initialize, list, call.
// Above notifyCLITimeout because the tools it now reaches include the slow ones
// on purpose — wait_agent blocks until an agent goes idle, and create_agent
// waits on a worktree and an ssh hop. -timeout overrides it.
const mcpCLITimeout = 120 * time.Second

// mcpCallSlack is what a tool's own timeout is padded by, to cover the round
// trip either side of the wait it describes. A tool that times out internally
// answers — with matched:false — and that answer is the useful one, so the
// transport must always outlast it rather than race it.
const mcpCallSlack = 15 * time.Second

// toolSelfTimeout reads a timeout the CALLER passed to the tool, so the
// transport deadline can cover it. Recognized by name and unit suffix rather
// than a list of tools: a new blocking tool spelled the same way is covered
// without an edit here, and a field this misreads can only ever make the CLI
// wait a little longer than it would have.
func toolSelfTimeout(argv map[string]any) time.Duration {
	var longest time.Duration
	for name, v := range argv {
		if !strings.Contains(name, "timeout") {
			continue
		}
		var n float64
		switch t := v.(type) {
		case int64:
			n = float64(t)
		case float64:
			n = t
		default:
			continue
		}
		unit := time.Millisecond
		switch {
		case strings.HasSuffix(name, "_s"), strings.HasSuffix(name, "_sec"), strings.HasSuffix(name, "_seconds"):
			unit = time.Second
		case strings.HasSuffix(name, "_ms"), strings.HasSuffix(name, "_millis"):
			unit = time.Millisecond
		default:
			// No unit in the name: milliseconds is what every MCP timeout field
			// lasso serves uses, and reading ms as seconds would hang for an hour.
			unit = time.Millisecond
		}
		if d := time.Duration(n) * unit; d > longest {
			longest = d
		}
	}
	return longest
}

func printMCPUsage(w *os.File) {
	fmt.Fprint(w, `lasso mcp — call lasso's MCP tools from a shell

usage:
  lasso mcp                       list the tools this lasso serves
  lasso mcp <tool> -h             show one tool's flags
  lasso mcp <tool> [flags]        call it
  lasso mcp <tool> -json          print the whole MCP result envelope

flags (before the tool name):
  -json             print the full result envelope, not just the structured output
  -timeout <dur>    give up after this long (default `+mcpCLITimeout.String()+`)

Tool names take either spelling: list-hosts or list_hosts. Flags are derived
from the schema the running server advertises, so they follow it automatically.
Output is indented on a terminal and compact when piped, and the exit code is 1
when the tool itself failed.

environment:
  LASSO_LISTEN      host:port of the local lasso (default `+defaultListenAddr+`)
  LASSO_URL         full base URL, if lasso is not on plain http loopback
  LASSO_MCP_TOKEN   bearer token, when /mcp is gated by MCP_OAUTH
  UI_AUTH           user:pass, when the server runs behind basic auth
`)
}

// cliMCP owns the flags and the exit codes. Go's flag package stops at the first
// non-flag argument, which is what splits `lasso mcp -json list-hosts -host x`
// into our flags, the tool name, and the tool's own flags — the same rule
// mcp2cli uses, and the reason -json has to lead. (It is accepted after the tool
// name too; see toolFlagSet.)
func cliMCP(args []string) {
	if wantsHelp(args) {
		printMCPUsage(os.Stdout)
		return
	}
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { printMCPUsage(os.Stderr) }
	full := fs.Bool("json", false, "print the full MCP result envelope")
	timeout := fs.Duration("timeout", mcpCLITimeout, "give up after this long")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}

	// The root carries no deadline — the session has to outlive the setup phase,
	// since a blocking tool legitimately runs longer than getting connected did.
	// Each phase takes its own child deadline instead.
	root, stop := context.WithCancel(context.Background())
	defer stop()

	setup, cancelSetup := context.WithTimeout(root, *timeout)
	defer cancelSetup()
	sess, err := dialMCP(setup, mcpEndpoint(), mcpCLIClient())
	if err != nil {
		fatal("mcp: %v", err)
	}
	defer sess.Close()

	tools, err := listMCPTools(setup, sess)
	if err != nil {
		fatal("mcp: list tools: %v", err)
	}

	rest := fs.Args()
	if len(rest) == 0 {
		printToolList(os.Stdout, tools)
		return
	}
	tool := findMCPTool(tools, rest[0])
	if tool == nil {
		fmt.Fprintf(os.Stderr, "lasso mcp: no tool %q on this server\n\n", rest[0])
		printToolList(os.Stderr, tools)
		os.Exit(2)
	}

	argv, err := parseToolArgs(tool, rest[1:], full)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		if !errors.Is(err, errFlagReported) {
			fmt.Fprintf(os.Stderr, "lasso mcp %s: %v\n", cliToolName(tool.Name), err)
		}
		os.Exit(2)
	}

	// wait_agent blocks for as long as its own timeout_ms says, and create_agent
	// waits on a worktree and an ssh hop. Letting the CLI's transport deadline
	// expire under a tool that was asked to wait longer reports a bare "context
	// deadline exceeded" for a call that was going fine — so the argument raises
	// the deadline when it exceeds it. It only ever extends.
	callTimeout := *timeout
	if want := toolSelfTimeout(argv); want+mcpCallSlack > callTimeout {
		callTimeout = want + mcpCallSlack
	}
	call, cancelCall := context.WithTimeout(root, callTimeout)
	defer cancelCall()
	res, err := sess.CallTool(call, &mcp.CallToolParams{Name: tool.Name, Arguments: argv})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			fatal("mcp: %s: gave up after %s — raise it with `lasso mcp -timeout <dur> %s ...`",
				cliToolName(tool.Name), callTimeout, cliToolName(tool.Name))
		}
		fatal("mcp: %s: %v", cliToolName(tool.Name), err)
	}
	out, isErr := renderToolResult(res, *full, isTerminal(os.Stdout))
	if isErr {
		// The call reached the tool and the tool refused. Said on stderr with a
		// non-zero exit so a script can tell it from a transport failure — which
		// exits 1 too, but through fatal, and never prints a result.
		fmt.Fprintln(os.Stderr, out)
		os.Exit(1)
	}
	fmt.Println(out)
}

// dialMCP opens a one-shot session against endpoint. Shared with notifycli.go so
// the transport options and the "is the server running?" hint live in one place.
func dialMCP(ctx context.Context, endpoint string, hc *http.Client) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "lasso-cli", Version: lassoSemver}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: hc,
		// Nothing here consumes server-initiated messages, and a one-shot command
		// must not hold a second connection open for them.
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("reach lasso's MCP endpoint at %s: %w (is the server running? set LASSO_LISTEN for a non-default port)", endpoint, err)
	}
	return sess, nil
}

// listMCPTools drains the paginated tools/list into one slice, sorted by name so
// the listing is stable across calls rather than following registration order.
func listMCPTools(ctx context.Context, sess *mcp.ClientSession) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

// ---------------------------------------------------------------------------
// tool names
// ---------------------------------------------------------------------------

// cliToolName is how a tool is spelled on the command line: kebab-case, which is
// what every other CLI over an MCP server renders and what a hand types. The
// wire name (snake_case, what the model sees) is what is actually sent.
// wantsHelp reports whether help was asked for outright. Only the first token
// counts: a value further along may legitimately be the string "-h" (a flag
// taking free text), and printing usage instead of calling the tool would be a
// silent wrong answer.
func wantsHelp(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "-h", "--help", "help":
		return true
	}
	return false
}

func cliToolName(wire string) string { return strings.ReplaceAll(wire, "_", "-") }

// findMCPTool resolves a typed name against the served tools, accepting either
// spelling. Both are normalized rather than one being canonical: the model-facing
// docs say list_hosts, every CLI listing says list-hosts, and refusing whichever
// one someone happens to have in front of them is a papercut with no upside.
func findMCPTool(tools []*mcp.Tool, name string) *mcp.Tool {
	want := normalizeToolName(name)
	for _, t := range tools {
		if normalizeToolName(t.Name) == want {
			return t
		}
	}
	return nil
}

func normalizeToolName(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
}

// ---------------------------------------------------------------------------
// the served schema -> flags
// ---------------------------------------------------------------------------

// toolSchema is the slice of a tool's JSON Schema this CLI needs. It is decoded
// from a re-marshal of Tool.InputSchema rather than type-asserted: the SDK
// documents that field as `any`, holding a map[string]any on the client side
// today, and a decode survives it becoming a typed schema or json.RawMessage.
type toolSchema struct {
	Properties map[string]schemaProp `json:"properties"`
	Required   []string              `json:"required"`
}

type schemaProp struct {
	Type        any         `json:"type"`
	Description string      `json:"description"`
	Enum        []any       `json:"enum"`
	Default     any         `json:"default"`
	Items       *schemaProp `json:"items"`
}

// kind collapses the two legal spellings of `type` — a string, or an array of
// them for a nullable field — into the one this CLI switches on. An unknown or
// absent type is treated as a string, which is what a bare JSON value coming off
// a command line is anyway.
func (p schemaProp) kind() string {
	switch t := p.Type.(type) {
	case string:
		return t
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok && s != "null" {
				return s
			}
		}
	}
	return "string"
}

func decodeToolSchema(in any) (toolSchema, error) {
	var s toolSchema
	if in == nil {
		return s, nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return s, fmt.Errorf("input schema: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("input schema: %w", err)
	}
	return s, nil
}

// argFlag is one schema-typed argument. It coerces on Set so a bad value is
// reported against the flag that carried it, and it records whether it was set
// at all — an omitted argument must be ABSENT from the call, not sent as a zero
// value, since the tools distinguish the two (an empty `host` means "search
// every host I may reach", not "the host named empty string").
type argFlag struct {
	prop schemaProp
	set  bool
	val  any
	list []any
}

func (a *argFlag) String() string { return "" }

// IsBoolFlag lets a boolean be written as `-focus` rather than `-focus=true`,
// which is what anyone typing it expects. `-focus=false` still works.
func (a *argFlag) IsBoolFlag() bool { return a.prop.kind() == "boolean" }

func (a *argFlag) Set(s string) error {
	v, err := coerceArg(a.prop, s)
	if err != nil {
		return err
	}
	a.set = true
	if a.prop.kind() == "array" {
		// Repeatable: each occurrence is one element, so a value containing a
		// comma or a bracket is passed through untouched rather than guessed at.
		a.list = append(a.list, v)
		a.val = a.list
		return nil
	}
	a.val = v
	return nil
}

func coerceArg(p schemaProp, s string) (any, error) {
	kind := p.kind()
	if kind == "array" && p.Items != nil {
		kind = p.Items.kind()
	}
	var v any
	switch kind {
	case "boolean":
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, fmt.Errorf("%q is not a boolean (true/false)", s)
		}
		v = b
	case "integer":
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a whole number", s)
		}
		v = n
	case "number":
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", s)
		}
		v = f
	case "object":
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return nil, fmt.Errorf("expects a JSON object: %w", err)
		}
		v = m
	default:
		v = s
	}
	if len(p.Enum) > 0 {
		if !enumAllows(p.Enum, v) {
			return nil, fmt.Errorf("%q is not one of %s", s, enumList(p.Enum))
		}
	}
	return v, nil
}

func enumAllows(enum []any, v any) bool {
	want := fmt.Sprint(v)
	for _, e := range enum {
		if fmt.Sprint(e) == want {
			return true
		}
	}
	return false
}

func enumList(enum []any) string {
	parts := make([]string, 0, len(enum))
	for _, e := range enum {
		parts = append(parts, fmt.Sprint(e))
	}
	return strings.Join(parts, ", ")
}

// parseToolArgs builds a FlagSet from the tool's served schema, parses args
// against it, and returns only the arguments actually given. Required fields are
// checked here purely to save a round trip — the server validates the same
// schema, so this mirrors its answer rather than inventing a stricter one.
func parseToolArgs(tool *mcp.Tool, args []string, full *bool) (map[string]any, error) {
	schema, err := decodeToolSchema(tool.InputSchema)
	if err != nil {
		return nil, err
	}
	fs := flag.NewFlagSet(cliToolName(tool.Name), flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { printToolHelp(os.Stderr, tool, schema) }
	// Asked for deliberately, help is the output, so it goes to stdout and exits
	// clean. Left to the flag package it would land on stderr beside the errors,
	// where `lasso mcp read-agent -h | less` gets nothing.
	if wantsHelp(args) {
		printToolHelp(os.Stdout, tool, schema)
		return nil, flag.ErrHelp
	}

	flags := make(map[string]*argFlag, len(schema.Properties))
	for name, prop := range schema.Properties {
		a := &argFlag{prop: prop}
		flags[name] = a
		// Registered under both spellings against the same value, for the same
		// reason the tool name is: the schema says agent_id, a CLI habit says
		// --agent-id, and neither is worth being wrong about.
		fs.Var(a, name, prop.Description)
		if kebab := cliToolName(name); kebab != name {
			fs.Var(a, kebab, prop.Description)
		}
	}
	// -json is a global, and globals have to lead because flag parsing stops at
	// the tool name. Accepting it here as well removes that papercut, except on a
	// tool whose own schema claims the name.
	if _, taken := schema.Properties["json"]; !taken {
		fs.BoolVar(full, "json", *full, "print the full MCP result envelope")
	}
	if err := fs.Parse(args); err != nil {
		return nil, errFlagReported
	}
	if extra := fs.Args(); len(extra) > 0 {
		return nil, fmt.Errorf("unexpected argument %q — every value is passed as a flag (try -h)", extra[0])
	}

	out := make(map[string]any, len(flags))
	for name, a := range flags {
		if a.set {
			out[name] = a.val
		}
	}
	var missing []string
	for _, req := range schema.Required {
		if _, ok := out[req]; !ok {
			missing = append(missing, "-"+cliToolName(req))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing required %s (try -h)", strings.Join(missing, ", "))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// output
// ---------------------------------------------------------------------------

// listWidth is where a tool's one-line summary is cut in the listing. The
// descriptions are written for a model and run to paragraphs, so the listing
// truncates rather than trying to find a first sentence — they are full of
// "e.g." and there is no honest way to split on a period.
const listWidth = 92

func printToolList(w *os.File, tools []*mcp.Tool) {
	width := 0
	for _, t := range tools {
		if n := len(cliToolName(t.Name)); n > width {
			width = n
		}
	}
	fmt.Fprintf(w, "%d tools at %s:\n\n", len(tools), mcpEndpoint())
	for _, t := range tools {
		fmt.Fprintf(w, "  %-*s  %s\n", width, cliToolName(t.Name), truncate(oneLine(t.Description), listWidth))
	}
	fmt.Fprint(w, "\nlasso mcp <tool> -h   flags for one tool\n")
}

func printToolHelp(w *os.File, tool *mcp.Tool, schema toolSchema) {
	fmt.Fprintf(w, "lasso mcp %s\n\n", cliToolName(tool.Name))
	for _, line := range wrap(oneLine(tool.Description), 78) {
		fmt.Fprintf(w, "  %s\n", line)
	}
	required := make(map[string]bool, len(schema.Required))
	for _, r := range schema.Required {
		required[r] = true
	}
	names := make([]string, 0, len(schema.Properties))
	for n := range schema.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 0 {
		fmt.Fprint(w, "\nflags:\n")
	}
	for _, n := range names {
		p := schema.Properties[n]
		label := "-" + cliToolName(n) + " <" + flagKindLabel(p) + ">"
		if required[n] {
			label += " (required)"
		}
		fmt.Fprintf(w, "  %s\n", label)
		for _, line := range wrap(oneLine(p.Description), 74) {
			fmt.Fprintf(w, "      %s\n", line)
		}
	}
	fmt.Fprint(w, "\nOmitted flags are left out of the call entirely, so the tool's own default\napplies. Repeat a list flag once per element.\n")
}

func flagKindLabel(p schemaProp) string {
	if len(p.Enum) > 0 {
		return enumList(p.Enum)
	}
	switch p.kind() {
	case "array":
		if p.Items != nil {
			return p.Items.kind() + ", repeatable"
		}
		return "repeatable"
	case "boolean":
		return "bool"
	default:
		return p.kind()
	}
}

// renderToolResult turns a result into the text to print and says whether the
// tool itself failed. Structured output is the payload when there is one — every
// lasso tool has a typed Out, so the text content beside it is the same data
// rendered for a model to read, and printing both would double every answer.
func renderToolResult(res *mcp.CallToolResult, full, indent bool) (string, bool) {
	if res.IsError {
		return toolErrorText(res), true
	}
	if full {
		return marshal(res, indent), false
	}
	if res.StructuredContent != nil {
		return marshal(res.StructuredContent, indent), false
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(tc.Text)
		}
	}
	return b.String(), false
}

// marshal indents on a terminal and stays compact when piped, so a human reads
// it and `| jq` gets one line per call without a flag either way.
func marshal(v any, indent bool) string {
	var (
		raw []byte
		err error
	)
	if indent {
		raw, err = json.MarshalIndent(v, "", "  ")
	} else {
		raw, err = json.Marshal(v)
	}
	if err != nil {
		return fmt.Sprintf("(unprintable result: %v)", err)
	}
	return string(raw)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndex(s[:n-1], " ")
	if cut < n/2 {
		cut = n - 1
	}
	return s[:cut] + "…"
}

func wrap(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	lines := []string{words[0]}
	for _, w := range words[1:] {
		last := len(lines) - 1
		if len(lines[last])+1+len(w) <= width {
			lines[last] += " " + w
			continue
		}
		lines = append(lines, w)
	}
	return lines
}
