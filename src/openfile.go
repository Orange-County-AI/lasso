package main

// open_file — an agent shows its human a file, in lasso's sidebar file viewer.
//
// "I wrote the design doc" is only half an answer when the human then has to go
// and find it. This lets the agent put it on their screen: the tool checks the
// file is really there (on the host it names, through the same backend
// /api/file reads with), then pushes a one-shot `open-file` SSE event to every
// connected tab. The browser decides what to do with it — only a VISIBLE tab
// acts, and an editor holding unsaved edits is never replaced (see
// src/web/src/lib/open-file.ts) — so the server's part is to validate, fan out,
// and report honestly how many tabs it reached.
//
// `lasso open <path>` is the same call from a shell, and the only way a
// RELATIVE path works: the MCP server runs in lasso's process and has no idea
// what the caller's working directory is, so the tool refuses one and the CLI
// resolves it before calling.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const openFileDescription = "Open a file in the human's lasso sidebar file viewer — the Files panel beside their terminal — in every lasso tab they have visible. Use it to SHOW them a file: a doc or plan you just wrote when they ask \"open it for me\", a file you want them to review, the place a bug lives. Pass an absolute path (or one starting with ~/, expanded against the target host's home); a relative path is refused because the server cannot see your working directory — `lasso open <path>` in a shell resolves one for you. `host` defaults to YOUR OWN host (the box lasso runs on when your credential names none), which is where your files are; pass it only for a file on another machine. `line` scrolls the editor to that 1-based line and selects it (a markdown file then opens in its raw editor instead of the rendered preview). A DIRECTORY opens the sidebar's file tree rooted there rather than the viewer. The file must exist: a missing path is an error, so write it first. Pass your $HERDR_PANE_ID as pane_id so the human is told which agent opened it. Check `delivered` in the reply: it is the number of lasso tabs the request reached, and 0 means no lasso tab is open, so the human did NOT see it — do not tell them it is on their screen. A tab that is hidden (a phone in a pocket, a background browser tab) receives it and ignores it, and a viewer holding the human's unsaved edits is not replaced — they get a prompt offering to open it instead."

type openFileIn struct {
	Path   string `json:"path" jsonschema:"Absolute path of the file (or directory) to open, or one starting with ~/ (expanded against the target host's home). Relative paths are refused — the server cannot see your working directory; use \"lasso open <path>\" from a shell for one."`
	Host   string `json:"host,omitempty" jsonschema:"Host the file lives on; omit to target your OWN host — the host your credential was issued for, or the box lasso runs on when it is not host-scoped."`
	Line   int    `json:"line,omitempty" jsonschema:"1-based line to scroll to and select in the editor. Optional; ignored for a directory and for images, PDFs and videos."`
	PaneID string `json:"pane_id,omitempty" jsonschema:"Your own herdr pane id — the value of $HERDR_PANE_ID in your shell. Used only to tell the human which agent opened the file; without it they are told \"an agent\"."`
}

type openFileOut struct {
	// Delivered is the number of lasso tabs the request was pushed to. 0 means
	// nobody saw it.
	Delivered int    `json:"delivered"`
	Path      string `json:"path"` // the resolved absolute path that was opened
	Host      string `json:"host"` // the host it was opened on
	Dir       bool   `json:"dir,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// openFileEvent is the `open-file` SSE payload the browser acts on.
type openFileEvent struct {
	Path string `json:"path"`
	Host string `json:"host"`
	Line int    `json:"line,omitempty"`
	Dir  bool   `json:"dir,omitempty"`
	From string `json:"from"`
}

// openFileNobody is the reply's detail when no tab is connected — the one
// outcome an agent must not report as success.
const openFileNobody = "no lasso tab is open, so nobody saw it"

// openFileBackend resolves the host whose filesystem the path is checked on.
// It is namedHostBackend — the resolver /api/file and the viewer itself use —
// so a file this tool accepts is one the browser can then actually load. A var
// so tests can stand a fake fleet in.
var openFileBackend = namedHostBackend

// openFileBroadcast hands the event to every connected tab and reports how many
// took it; a no-op answering 0 before the server is up (tests, CLI paths).
var openFileBroadcast = func(ev openFileEvent) int {
	if srvHub == nil {
		return 0
	}
	return srvHub.broadcast("open-file", ev)
}

func openFileTool(ctx context.Context, req *mcp.CallToolRequest, in openFileIn) (*mcp.CallToolResult, openFileOut, error) {
	p := strings.TrimSpace(in.Path)
	if p == "" {
		return nil, openFileOut{}, fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(p) && p != "~" && !strings.HasPrefix(p, "~/") {
		return nil, openFileOut{}, fmt.Errorf("path %q is relative: pass an absolute path (or one starting with ~/) — the server cannot see your working directory. From a shell, `lasso open %s` resolves it for you", p, p)
	}
	if in.Line < 0 {
		return nil, openFileOut{}, fmt.Errorf("line must be a positive, 1-based line number (got %d)", in.Line)
	}
	cs := callerFrom(req)
	host := cs.hostOr(strings.TrimSpace(in.Host))
	if err := cs.requireHost(host); err != nil {
		return nil, openFileOut{}, err
	}
	be, err := openFileBackend(host)
	if err != nil {
		return nil, openFileOut{}, err
	}
	// Expanded against THAT host's home, like every sidebar endpoint: a remote
	// ~/notes.md is not the lasso box's ~/notes.md.
	p = filepath.Clean(expandTildeOn(be, p))
	if !filepath.IsAbs(p) {
		return nil, openFileOut{}, fmt.Errorf("could not expand %q on host %q", in.Path, host)
	}
	info, err := be.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, openFileOut{}, fmt.Errorf("%s does not exist on host %q — write the file before asking lasso to open it", p, host)
		}
		return nil, openFileOut{}, fmt.Errorf("stat %s on host %q: %w", p, host, err)
	}

	from, detail := "an agent", ""
	if strings.TrimSpace(in.PaneID) != "" {
		// Attribution only: an unresolvable pane still opens the file, and the
		// reason rides back in detail for the agent to fix next time.
		who, err := resolveCallerAgent(ctx, cs, "", in.PaneID)
		switch {
		case err != nil:
			detail = "could not name you: " + err.Error()
		case who.Found:
			if name := firstNonEmpty(who.Agent.SidebarName, who.Agent.Title); name != "" {
				from = name
			}
		default:
			detail = "could not name you: " + who.Detail
		}
	}

	ev := openFileEvent{Path: p, Host: be.Name(), From: from, Dir: info.IsDir()}
	if !ev.Dir {
		ev.Line = in.Line
	}
	out := openFileOut{Path: p, Host: ev.Host, Dir: ev.Dir}
	out.Delivered = openFileBroadcast(ev)
	if out.Delivered == 0 {
		// The honesty rule notify's `sent` follows: an agent must be able to tell
		// "it is on their screen" from "nobody was looking".
		out.Detail = openFileNobody
		return nil, out, nil
	}
	out.Detail = detail
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// lasso open — the same call from a shell
// ---------------------------------------------------------------------------

const openCLITimeout = 30 * time.Second

func printOpenUsage(w *os.File) {
	fmt.Fprint(w, `lasso open — show a file in the human's lasso sidebar file viewer

usage:
  lasso open [flags] <path> [flags]

flags:
  -line <n>       scroll to (and select) this 1-based line
  -host <alias>   host the file lives on (default: your own host — see below)
  -pane <id>      your herdr pane id, to say who opened it (default: $HERDR_PANE_ID)

<path> may be relative: it is resolved against THIS shell's working directory,
on the machine this command runs on, before the call. A ~/ path is passed
through and expanded against the target host's home. A directory opens the
sidebar's file tree there instead of the viewer.

The host defaults to the caller's own host, which for a CLI with no per-host
credential (no LASSO_MCP_TOKEN) is lasso's own box, "local". Running this on a
remote fleet machine without a per-host credential? Pass -host <alias>, or the
path will be looked for on the lasso host instead.

Exits non-zero when no lasso tab is open (nobody saw it), with the reason on
stderr — so an agent can tell "on their screen" from "nobody was looking".

environment:
  LASSO_LISTEN      host:port of the local lasso (default `+defaultListenAddr+`)
  LASSO_URL         full base URL, if lasso is not on plain http loopback
  LASSO_MCP_TOKEN   bearer token, when /mcp is gated by MCP_OAUTH
  UI_AUTH           user:pass, when the server runs behind basic auth
`)
}

func cliOpen(args []string) {
	in, err := parseOpenArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		printOpenUsage(os.Stdout)
		return
	}
	if err != nil {
		if !errors.Is(err, errFlagReported) {
			fmt.Fprintf(os.Stderr, "lasso open: %v\n", err)
			printOpenUsage(os.Stderr)
		}
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), openCLITimeout)
	defer cancel()
	out, err := callOpenFileTool(ctx, mcpEndpoint(), mcpCLIClient(), in)
	if err != nil {
		fatal("open: %v", err)
	}
	if out.Delivered == 0 {
		fmt.Fprintf(os.Stderr, "lasso open: not shown — %s\n", out.Detail)
		os.Exit(1)
	}
	what := "file"
	if out.Dir {
		what = "directory"
	}
	tabs := "tab"
	if out.Delivered != 1 {
		tabs = "tabs"
	}
	line := fmt.Sprintf("opened %s %s on %s in %d lasso %s", what, out.Path, out.Host, out.Delivered, tabs)
	if out.Detail != "" {
		line += " (" + out.Detail + ")"
	}
	fmt.Println(line)
}

// parseOpenArgs parses `lasso open`'s flags — before AND after the path, since
// `lasso open notes.md -line 40` is how people type it and the flag package
// stops at the first positional — and resolves a relative path against the
// working directory. Split from cliOpen so a test can drive it.
func parseOpenArgs(args []string) (openFileIn, error) {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {}
	line := fs.Int("line", 0, "1-based line to scroll to")
	host := fs.String("host", "", "host the file lives on")
	pane := fs.String("pane", os.Getenv("HERDR_PANE_ID"), "your herdr pane id")
	parse := func(a []string) error {
		if err := fs.Parse(a); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return err
			}
			return errFlagReported
		}
		return nil
	}
	if err := parse(args); err != nil {
		return openFileIn{}, err
	}
	var paths []string
	for fs.NArg() > 0 {
		paths = append(paths, fs.Arg(0))
		if err := parse(fs.Args()[1:]); err != nil {
			return openFileIn{}, err
		}
	}
	switch len(paths) {
	case 0:
		return openFileIn{}, errors.New("no path given")
	case 1:
	default:
		return openFileIn{}, fmt.Errorf("one path at a time (got %d)", len(paths))
	}
	p, err := resolveCLIPath(paths[0])
	if err != nil {
		return openFileIn{}, err
	}
	return openFileIn{Path: p, Host: *host, Line: *line, PaneID: *pane}, nil
}

// resolveCLIPath makes a relative path absolute against this process's working
// directory. A ~ path is left for the server, which expands it against the
// TARGET host's home rather than this machine's.
func resolveCLIPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("empty path")
	}
	if p == "~" || strings.HasPrefix(p, "~/") || filepath.IsAbs(p) {
		return p, nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", p, err)
	}
	return abs, nil
}

// callOpenFileTool connects to endpoint, calls open_file once and decodes its
// structured output — callNotifyTool's shape, so a test can drive the real
// /mcp handler over httptest.
func callOpenFileTool(ctx context.Context, endpoint string, hc *http.Client, in openFileIn) (openFileOut, error) {
	sess, err := dialMCP(ctx, endpoint, hc)
	if err != nil {
		return openFileOut{}, err
	}
	defer sess.Close()

	args := map[string]any{"path": in.Path}
	if in.Host != "" {
		args["host"] = in.Host
	}
	if in.Line > 0 {
		args["line"] = in.Line
	}
	if in.PaneID != "" {
		args["pane_id"] = in.PaneID
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "open_file", Arguments: args})
	if err != nil {
		return openFileOut{}, err
	}
	if res.IsError {
		return openFileOut{}, errors.New(toolErrorText(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return openFileOut{}, fmt.Errorf("tool result: %w", err)
	}
	var out openFileOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return openFileOut{}, fmt.Errorf("tool result: %w", err)
	}
	return out, nil
}
