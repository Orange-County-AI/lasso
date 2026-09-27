package main

// `lasso connect` — register lasso's two MCP servers with the agent CLIs
// installed on THIS machine, so onboarding is one command:
//
//	lasso          <base>/mcp          orchestration: create/list/inspect/close agents, notify, shared_browser
//	lasso-browser  <base>/browser-mcp  chrome-devtools-mcp against the shared browser
//
// Two servers rather than one because they are two jobs: an agent that only
// needs to drive a page is not handed the fleet, and one orchestrating agents
// is not handed thirty browser tools it will never call.
//
// Every CLI is registered through its OWN mechanism, found by reading its help
// on a real install rather than assumed:
//
//	claude    `claude mcp add --transport http --scope <s>` — it refuses a name
//	          that exists, so an update is `claude mcp remove` then add.
//	          Headers can only be passed as `-H` on argv.
//	codex     `codex mcp add <name> --url` — it replaces an entry in place. It has
//	          no flag for a static header (only --bearer-token-env-var, which
//	          makes the entry depend on codex's own environment), so headers are
//	          written into that entry's table in config.toml afterwards: off argv.
//	opencode  `opencode mcp add <name> --url --header K=V` — it replaces too, but
//	          has no remove, so -remove edits opencode.json(c) directly.
//	omp       no mcp subcommand at all: ~/.omp/agent/mcp.json is edited.
//	pi        no MCP client, by design — reported as skipped.
//
// "Unchanged" is decided by READING what the CLI stored (its config file, or
// `codex mcp get --json`), never by remembering what lasso last wrote, so a
// hand edit is seen and re-running is always safe.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	connectServerName  = "lasso"
	connectBrowserName = "lasso-browser"

	// connectProbeTimeout bounds the initialize + shared_browser round trip.
	connectProbeTimeout = 15 * time.Second
	// connectCmdTimeout bounds one agent-CLI invocation. They are node/rust
	// binaries that start in well under a second; this is only for a wedge.
	connectCmdTimeout = 60 * time.Second
)

func printConnectUsage(w io.Writer) {
	fmt.Fprint(w, `lasso connect — register lasso's MCP servers with this machine's agent CLIs

usage:
  lasso connect [flags]

Registers two streamable-HTTP MCP servers with every supported agent CLI found
on this machine (claude, codex, opencode, omp; pi has no MCP client):

  lasso           <url>/mcp           create, list, inspect and close agents; notify; shared_browser
  lasso-browser   <url>/browser-mcp   chrome-devtools-mcp against lasso's shared browser

It asks the server first: /mcp must answer (or nothing is registered), and
lasso-browser is registered only when lasso says /browser-mcp can serve.
Re-running is safe — an entry already identical is left alone, a different
one is replaced.

flags:
  -url <url>        the URL THIS machine reaches lasso on (default: $LASSO_URL,
                    else http://$LASSO_LISTEN, else http://`+defaultListenAddr+`).
                    On another machine that is lasso's tunnel or tailnet URL —
                    127.0.0.1 would point its agents at themselves.
  -token <token>    bearer token to register (default: $LASSO_MCP_TOKEN)
  -header 'N: v'    extra header to register and probe with (repeatable)
  -only a,b         only these CLIs (claude, codex, opencode, omp, pi)
  -scope <s>        Claude Code scope: user (default), local, or project
  -browser=false    do not register lasso-browser
  -remove           unregister both servers from every CLI instead
  -dry-run          print what would be run or written; change nothing
  -force            skip the probe (register even if lasso does not answer)

auth:
  With $LASSO_MCP_TOKEN (or -token) the entries carry
  "Authorization: Bearer <token>"; with only $UI_AUTH, basic credentials.
  Behind Cloudflare Access a remote box also needs a service token:
    -header 'CF-Access-Client-Id: <id>.access' -header 'CF-Access-Client-Secret: <secret>'
  Secrets are masked in everything this prints. Claude Code and OpenCode only
  take headers on their command line, so for those two the header values are
  briefly visible in this machine's process list while the command runs; codex
  and omp get theirs written straight into their config files.

environment:
  LASSO_URL, LASSO_LISTEN, LASSO_MCP_TOKEN, UI_AUTH   as above
  CLAUDE_CONFIG_DIR, CODEX_HOME, XDG_CONFIG_HOME      where those CLIs keep config
`)
}

// ---------------------------------------------------------------------------
// headers + URL
// ---------------------------------------------------------------------------

type connectHeader struct{ Name, Value string }

// headerList is the repeatable -header flag.
type headerList []connectHeader

func (h *headerList) String() string { return "" }
func (h *headerList) Set(s string) error {
	hd, err := parseConnectHeader(s)
	if err != nil {
		return err
	}
	*h = append(*h, hd)
	return nil
}

// parseConnectHeader reads "Name: value". The name is checked against the HTTP
// token grammar, since a malformed one would be accepted here and rejected by
// every CLI in a different way.
func parseConnectHeader(s string) (connectHeader, error) {
	name, value, ok := strings.Cut(s, ":")
	name, value = strings.TrimSpace(name), strings.TrimSpace(value)
	if !ok || name == "" || value == "" {
		return connectHeader{}, fmt.Errorf("header %q: want 'Name: value'", s)
	}
	for _, r := range name {
		if !(r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return connectHeader{}, fmt.Errorf("header %q: %q is not a valid header name", s, name)
		}
	}
	return connectHeader{Name: name, Value: value}, nil
}

// connectHeaders is the header set every entry is registered with: the auth
// the environment offers (a bearer token wins over UI_AUTH, as withMCPAuth
// accepts either), then the -header extras in order. An extra naming a header
// already in the set replaces it, so -header 'Authorization: …' is an override.
func connectHeaders(token, uiAuth string, extra []connectHeader) []connectHeader {
	var out []connectHeader
	if token = strings.TrimSpace(token); token != "" {
		out = append(out, connectHeader{"Authorization", "Bearer " + token})
	} else if u, p, ok := parseAuth(uiAuth); ok {
		out = append(out, connectHeader{"Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+p))})
	}
	for _, x := range extra {
		replaced := false
		for i := range out {
			if strings.EqualFold(out[i].Name, x.Name) {
				out[i] = x
				replaced = true
			}
		}
		if !replaced {
			out = append(out, x)
		}
	}
	return out
}

func headerMap(h []connectHeader) map[string]string {
	m := map[string]string{}
	for _, x := range h {
		m[x.Name] = x.Value
	}
	return m
}

// maskSecret keeps an auth scheme and the first few characters, which is
// enough to tell two tokens apart in a transcript and not enough to use one.
func maskSecret(v string) string {
	scheme := ""
	if i := strings.IndexByte(v, ' '); i > 0 && i <= 10 {
		scheme, v = v[:i+1], v[i+1:]
	}
	if len(v) <= 8 {
		return scheme + "****"
	}
	return scheme + v[:4] + "…"
}

// connectBase resolves the base URL the entries point at: -url, else the same
// LASSO_URL/LASSO_LISTEN resolution notify and mcp use. A pasted /mcp or
// /browser-mcp suffix is dropped, since both servers hang off the base.
func connectBase(flagURL string) (string, error) {
	b := strings.TrimSpace(flagURL)
	if b == "" {
		b = lassoBaseURL()
	}
	u, err := url.Parse(b)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q is not an http(s) URL", b)
	}
	u.RawQuery, u.Fragment = "", ""
	s := strings.TrimRight(u.String(), "/")
	for _, suf := range []string{"/browser-mcp", "/mcp"} {
		s = strings.TrimSuffix(s, suf)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// probe
// ---------------------------------------------------------------------------

type headerTransport struct {
	h    []connectHeader
	base http.RoundTripper
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for _, h := range t.h {
		r.Header.Set(h.Name, h.Value)
	}
	return t.base.RoundTrip(r)
}

type connectProbe struct {
	browserOK     bool
	browserReason string
}

// probeLasso proves /mcp answers with these headers — the same ones the CLIs
// will send — and asks shared_browser (start:false, so probing never launches
// Chromium) whether /browser-mcp can serve. An error means /mcp itself is out
// of reach; a browser that cannot be asked about is just an unavailable one.
func probeLasso(ctx context.Context, base string, h []connectHeader) (connectProbe, error) {
	hc := &http.Client{Transport: headerTransport{h: h, base: http.DefaultTransport}}
	sess, err := dialMCP(ctx, base+"/mcp", hc)
	if err != nil {
		return connectProbe{}, err
	}
	defer sess.Close()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "shared_browser", Arguments: map[string]any{"start": false}})
	if err != nil {
		return connectProbe{browserReason: "could not ask lasso about its browser: " + err.Error()}, nil
	}
	if res.IsError {
		return connectProbe{browserReason: toolErrorText(res)}, nil
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out sharedBrowserOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return connectProbe{browserReason: "unreadable shared_browser answer: " + err.Error()}, nil
	}
	p := connectProbe{browserOK: out.MCPAvailable, browserReason: out.MCPReason}
	if !p.browserOK && p.browserReason == "" {
		p.browserReason = out.Note
		if p.browserReason == "" {
			p.browserReason = "lasso reports /browser-mcp as unavailable"
		}
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// targets
// ---------------------------------------------------------------------------

// connectEnv is everything a target reads from the machine, gathered once so a
// test can point it at fixture files and a fake command runner.
type connectEnv struct {
	home, cwd       string
	xdgConfig       string // $XDG_CONFIG_HOME, "" when unset
	codexHome       string // $CODEX_HOME, "" when unset
	claudeConfigDir string // $CLAUDE_CONFIG_DIR, "" when unset
	scope           string // Claude Code: user | local | project
	dryRun          bool
	lookPath        func(string) (string, error)
	run             func(dir, name string, args ...string) ([]byte, error)
}

func newConnectEnv(scope string, dryRun bool) (*connectEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	cwd, _ := os.Getwd()
	return &connectEnv{
		home: home, cwd: cwd,
		xdgConfig:       os.Getenv("XDG_CONFIG_HOME"),
		codexHome:       os.Getenv("CODEX_HOME"),
		claudeConfigDir: os.Getenv("CLAUDE_CONFIG_DIR"),
		scope:           scope,
		dryRun:          dryRun,
		lookPath:        exec.LookPath,
		run:             runConnectCmd,
	}, nil
}

// runConnectCmd runs an agent CLI with stdin on /dev/null (opencode's add turns
// into an interactive prompt when it thinks it can ask), returning its combined
// output for the error message.
func runConnectCmd(dir, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// mcpEntry is what a CLI has stored under one server name, reduced to the part
// lasso owns. ok=false when the stored entry is not a plain HTTP one (another
// transport, an env-var header indirection): it is then always rewritten.
type mcpEntry struct {
	url     string
	headers map[string]string
	ok      bool
}

func (e *mcpEntry) matches(url string, h []connectHeader) bool {
	if e == nil || !e.ok || e.url != url {
		return false
	}
	want := headerMap(h)
	if len(e.headers) != len(want) {
		return false
	}
	for k, v := range want {
		if e.headers[k] != v {
			return false
		}
	}
	return true
}

// connectStep is one thing done to a CLI: a command or a file write. show is
// what -dry-run prints, with every header value masked.
type connectStep struct {
	show string
	do   func() error
}

type connectTarget struct {
	// detect answers "" when the CLI is here and speaks HTTP MCP, else why not.
	detect func(e *connectEnv) string
	// read finds the entry currently stored under name (nil when absent).
	read func(e *connectEnv, name string) (*mcpEntry, error)
	// write registers (or replaces) name → url with these headers.
	write func(e *connectEnv, name, url string, h []connectHeader, exists bool) []connectStep
	// unregister removes name.
	unregister func(e *connectEnv, name string) []connectStep
}

// connectTargets is keyed by harness id, so a harness added to harness.go shows
// up here as "no registration" until someone teaches connect about it.
var connectTargets = map[string]connectTarget{
	"claude":   claudeConnect,
	"codex":    codexConnect,
	"opencode": opencodeConnect,
	"omp":      ompConnect,
	"pi": {detect: func(e *connectEnv) string {
		if _, err := e.lookPath("pi"); err != nil {
			return "not installed"
		}
		return "pi has no MCP client (by design); its agents can use `lasso mcp` from the shell"
	}},
}

func connectTargetIDs(only string) ([]string, error) {
	var all []string
	for _, h := range harnesses {
		all = append(all, h.ID)
	}
	if strings.TrimSpace(only) == "" {
		return all, nil
	}
	want := map[string]bool{}
	for _, id := range strings.Split(only, ",") {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			continue
		}
		known := false
		for _, a := range all {
			known = known || a == id
		}
		if !known {
			return nil, fmt.Errorf("-only: unknown agent CLI %q (known: %s)", id, strings.Join(all, ", "))
		}
		want[id] = true
	}
	var out []string
	for _, a := range all {
		if want[a] {
			out = append(out, a)
		}
	}
	return out, nil
}

type connectResult struct {
	target, server, status string
	steps                  []string
}

func (r connectResult) failed() bool { return strings.HasPrefix(r.status, "failed") }

func connectApply(e *connectEnv, t connectTarget, name, url string, h []connectHeader) connectResult {
	r := connectResult{server: name}
	cur, err := t.read(e, name)
	if err != nil {
		r.status = "failed: " + err.Error()
		return r
	}
	if cur.matches(url, h) {
		r.status = "unchanged"
		return r
	}
	r.status = "added"
	if cur != nil {
		r.status = "updated"
	}
	return runSteps(e, r, t.write(e, name, url, h, cur != nil))
}

func connectRemove(e *connectEnv, t connectTarget, name string) connectResult {
	r := connectResult{server: name}
	cur, err := t.read(e, name)
	if err != nil {
		r.status = "failed: " + err.Error()
		return r
	}
	if cur == nil {
		r.status = "not registered"
		return r
	}
	r.status = "removed"
	return runSteps(e, r, t.unregister(e, name))
}

func runSteps(e *connectEnv, r connectResult, steps []connectStep) connectResult {
	for _, s := range steps {
		r.steps = append(r.steps, s.show)
	}
	if e.dryRun {
		r.status = "would be " + r.status
		return r
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			r.status = "failed: " + err.Error()
			return r
		}
	}
	return r
}

// execStep runs a CLI. display is argv with secrets masked; args is the real one.
func execStep(e *connectEnv, dir string, args, display []string) connectStep {
	return connectStep{
		show: "$ " + shellJoin(display),
		do: func() error {
			out, err := e.run(dir, args[0], args[1:]...)
			if err != nil {
				msg := strings.TrimSpace(lastLines(string(out), 3))
				if msg == "" {
					return fmt.Errorf("%s: %v", args[0], err)
				}
				return fmt.Errorf("%s: %v: %s", args[0], err, msg)
			}
			return nil
		},
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a != "" && strings.IndexFunc(a, func(r rune) bool {
			return !(r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == '=' || r == '@' || r == ',' ||
				(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
		}) < 0 {
			out[i] = a
			continue
		}
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

// entryFromJSON reads {url, headers} out of a decoded server object, requiring
// the transport field to say what lasso writes (claude/omp "http", opencode
// "remote").
func entryFromJSON(raw json.RawMessage, wantType string) *mcpEntry {
	var v struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return &mcpEntry{}
	}
	return &mcpEntry{url: v.URL, headers: v.Headers, ok: v.Type == wantType}
}

// ---------------------------------------------------------------------------
// Claude Code
// ---------------------------------------------------------------------------

var claudeConnect = connectTarget{
	detect: func(e *connectEnv) string {
		if _, err := e.lookPath("claude"); err != nil {
			return "not installed"
		}
		return ""
	},
	read: func(e *connectEnv, name string) (*mcpEntry, error) {
		path, servers, err := claudeServers(e)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		raw, ok := servers[name]
		if !ok {
			return nil, nil
		}
		return entryFromJSON(raw, "http"), nil
	},
	write: func(e *connectEnv, name, url string, h []connectHeader, exists bool) []connectStep {
		var steps []connectStep
		if exists {
			// `claude mcp add` refuses a name that exists in the scope.
			rm := []string{"claude", "mcp", "remove", "--scope", e.scope, name}
			steps = append(steps, execStep(e, e.cwd, rm, rm))
		}
		args := []string{"claude", "mcp", "add", "--transport", "http", "--scope", e.scope, name, url}
		display := append([]string(nil), args...)
		for _, x := range h {
			args = append(args, "--header", x.Name+": "+x.Value)
			display = append(display, "--header", x.Name+": "+maskSecret(x.Value))
		}
		return append(steps, execStep(e, e.cwd, args, display))
	},
	unregister: func(e *connectEnv, name string) []connectStep {
		rm := []string{"claude", "mcp", "remove", "--scope", e.scope, name}
		return []connectStep{execStep(e, e.cwd, rm, rm)}
	},
}

// claudeServers is the mcpServers map of the scope being registered in: user
// is ~/.claude.json's top level, local is that file's entry for this directory,
// project is ./.mcp.json. Read only — every write goes through `claude mcp`.
func claudeServers(e *connectEnv) (string, map[string]json.RawMessage, error) {
	path := filepath.Join(e.home, ".claude.json")
	if e.claudeConfigDir != "" {
		path = filepath.Join(e.claudeConfigDir, ".claude.json")
	}
	if e.scope == "project" {
		path = filepath.Join(e.cwd, ".mcp.json")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, nil, nil
	}
	if err != nil {
		return path, nil, err
	}
	var root struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return path, nil, err
	}
	if e.scope == "local" {
		return path, root.Projects[e.cwd].MCPServers, nil
	}
	return path, root.MCPServers, nil
}

// ---------------------------------------------------------------------------
// Codex
// ---------------------------------------------------------------------------

var codexConnect = connectTarget{
	detect: func(e *connectEnv) string {
		if _, err := e.lookPath("codex"); err != nil {
			return "not installed"
		}
		return ""
	},
	read: func(e *connectEnv, name string) (*mcpEntry, error) {
		// codex's config is TOML and go.mod has no TOML parser, so codex is
		// asked. A name it does not know is a non-zero exit.
		out, err := e.run(e.home, "codex", "mcp", "get", name, "--json")
		if err != nil {
			return nil, nil
		}
		return codexEntry(out)
	},
	write: func(e *connectEnv, name, url string, h []connectHeader, _ bool) []connectStep {
		// `codex mcp add` replaces an existing entry of the same name, and in
		// doing so drops any http_headers it had, so the headers are written
		// after it every time rather than only when they changed.
		add := []string{"codex", "mcp", "add", name, "--url", url}
		steps := []connectStep{execStep(e, e.home, add, add)}
		if len(h) == 0 {
			return steps
		}
		path := codexConfigPath(e)
		var shown []string
		for _, x := range h {
			shown = append(shown, fmt.Sprintf("%q = %q", x.Name, maskSecret(x.Value)))
		}
		return append(steps, connectStep{
			show: fmt.Sprintf("write %s: [mcp_servers.%s] http_headers = { %s }", path, name, strings.Join(shown, ", ")),
			do: func() error {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				out, err := codexSetHTTPHeaders(data, name, h)
				if err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
				return writeFileAtomic(path, out, 0o600)
			},
		})
	},
	unregister: func(e *connectEnv, name string) []connectStep {
		rm := []string{"codex", "mcp", "remove", name}
		return []connectStep{execStep(e, e.home, rm, rm)}
	},
}

func codexConfigPath(e *connectEnv) string {
	if e.codexHome != "" {
		return filepath.Join(e.codexHome, "config.toml")
	}
	return filepath.Join(e.home, ".codex", "config.toml")
}

// codexEntry decodes `codex mcp get --json`. An entry reaching its headers
// through the environment (bearer_token_env_var, env_http_headers) is not one
// lasso wrote, so it never reads as unchanged.
func codexEntry(out []byte) (*mcpEntry, error) {
	// codex prints warnings to stderr ahead of the JSON (combined here), so
	// decode from the first brace.
	if i := bytes.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	var v struct {
		Transport struct {
			Type           string            `json:"type"`
			URL            string            `json:"url"`
			BearerEnv      *string           `json:"bearer_token_env_var"`
			HTTPHeaders    map[string]string `json:"http_headers"`
			EnvHTTPHeaders map[string]string `json:"env_http_headers"`
		} `json:"transport"`
	}
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&v); err != nil {
		return nil, fmt.Errorf("codex mcp get: %w", err)
	}
	t := v.Transport
	return &mcpEntry{
		url:     t.URL,
		headers: t.HTTPHeaders,
		ok:      t.Type == "streamable_http" && t.BearerEnv == nil && len(t.EnvHTTPHeaders) == 0,
	}, nil
}

// codexSetHTTPHeaders rewrites the http_headers of [mcp_servers.<name>] in a
// codex config.toml, leaving every other line byte for byte. It is a scoped
// line edit, not a TOML round trip: go.mod carries no TOML library, and codex
// has just written this table itself (`codex mcp add`), so its shape is known —
// a bare-key header line, one key per line. Any http_headers already there, as
// a key or as a [mcp_servers.<name>.http_headers] sub-table, is replaced.
func codexSetHTTPHeaders(doc []byte, name string, h []connectHeader) ([]byte, error) {
	lines := strings.SplitAfter(string(doc), "\n")
	header := "[mcp_servers." + name + "]"
	quoted := `[mcp_servers."` + name + `"]`
	sub := "[mcp_servers." + name + ".http_headers]"
	start := -1
	for i, l := range lines {
		if t := strings.TrimSpace(l); t == header || t == quoted {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("no %s table (codex mcp add did not write one)", header)
	}
	var out []string
	out = append(out, lines[:start+1]...)
	if len(h) > 0 {
		var kv []string
		for _, x := range h {
			kv = append(kv, tomlQuote(x.Name)+" = "+tomlQuote(x.Value))
		}
		out = append(out, "http_headers = { "+strings.Join(kv, ", ")+" }\n")
	}
	// Walk the rest of the file tracking which table each line is in: drop an
	// http_headers key from ours and the whole of an http_headers sub-table.
	section := "ours"
	for _, l := range lines[start+1:] {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			section = "other"
			if t == sub || t == `[mcp_servers."`+name+`".http_headers]` {
				section = "sub"
			}
		}
		if section == "sub" || (section == "ours" && isTOMLKey(t, "http_headers")) {
			continue
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "")), nil
}

func isTOMLKey(line, key string) bool {
	rest, ok := strings.CutPrefix(line, key)
	if !ok {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(rest), "=")
}

// tomlQuote renders s as a TOML basic string.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ---------------------------------------------------------------------------
// OpenCode
// ---------------------------------------------------------------------------

var opencodeConnect = connectTarget{
	detect: func(e *connectEnv) string {
		if _, err := e.lookPath("opencode"); err != nil {
			return "not installed"
		}
		return ""
	},
	read: func(e *connectEnv, name string) (*mcpEntry, error) {
		for _, path := range opencodeConfigFiles(e) {
			root, _, err := readJSONCObject(path)
			if err != nil {
				return nil, err
			}
			if root == nil {
				continue
			}
			if raw, ok := nestedKey(root, "mcp", name); ok {
				return entryFromJSON(raw, "remote"), nil
			}
		}
		return nil, nil
	},
	write: func(e *connectEnv, name, url string, h []connectHeader, _ bool) []connectStep {
		// `opencode mcp add` replaces an entry of the same name. Run from home
		// so it writes the global config, never a project's.
		args := []string{"opencode", "mcp", "add", name, "--url", url}
		display := append([]string(nil), args...)
		for _, x := range h {
			args = append(args, "--header", x.Name+"="+x.Value)
			display = append(display, "--header", x.Name+"="+maskSecret(x.Value))
		}
		return []connectStep{execStep(e, e.home, args, display)}
	},
	unregister: func(e *connectEnv, name string) []connectStep {
		// opencode has no `mcp remove`: edit whichever config file holds it.
		var steps []connectStep
		for _, path := range opencodeConfigFiles(e) {
			root, jsonc, err := readJSONCObject(path)
			if err != nil || root == nil {
				continue
			}
			if _, ok := nestedKey(root, "mcp", name); !ok {
				continue
			}
			path, jsonc := path, jsonc
			steps = append(steps, connectStep{
				show: fmt.Sprintf("write %s: delete mcp.%s", path, name),
				do: func() error {
					if jsonc {
						return fmt.Errorf("%s has comments, which a rewrite would drop; delete mcp.%s from it by hand", path, name)
					}
					return editJSONFile(path, func(root *jsonObject) error {
						return deleteNested(root, "mcp", name)
					})
				},
			})
		}
		return steps
	},
}

// opencodeConfigFiles are the global config files opencode reads, in the
// order it prefers them. XDG_CONFIG_HOME is honored here (unlike the theme
// mirror's opencodeConfigDir, which addresses remote homes) because opencode
// itself honors it on this machine.
func opencodeConfigFiles(e *connectEnv) []string {
	dir := opencodeConfigDir(e.home)
	if e.xdgConfig != "" {
		dir = filepath.Join(e.xdgConfig, "opencode")
	}
	return []string{
		filepath.Join(dir, "opencode.jsonc"),
		filepath.Join(dir, "opencode.json"),
		filepath.Join(dir, "config.json"),
	}
}

// ---------------------------------------------------------------------------
// omp
// ---------------------------------------------------------------------------

const ompMCPSchema = "https://raw.githubusercontent.com/can1357/oh-my-pi/main/packages/coding-agent/src/config/mcp-schema.json"

var ompConnect = connectTarget{
	detect: func(e *connectEnv) string {
		if _, err := e.lookPath("omp"); err == nil {
			return ""
		}
		if _, err := os.Stat(ompAgentDir(e.home)); err == nil {
			return ""
		}
		return "not installed"
	},
	read: func(e *connectEnv, name string) (*mcpEntry, error) {
		root, err := readJSONObjectFile(ompMCPPath(e))
		if err != nil || root == nil {
			return nil, err
		}
		if raw, ok := nestedKey(root, "mcpServers", name); ok {
			return entryFromJSON(raw, "http"), nil
		}
		return nil, nil
	},
	write: func(e *connectEnv, name, url string, h []connectHeader, _ bool) []connectStep {
		path := ompMCPPath(e)
		shown := map[string]string{}
		for _, x := range h {
			shown[x.Name] = maskSecret(x.Value)
		}
		desc := fmt.Sprintf(`{"type":"http","url":%q`, url)
		if len(h) > 0 {
			b, _ := json.Marshal(shown)
			desc += `,"headers":` + string(b)
		}
		return []connectStep{{
			show: fmt.Sprintf("write %s: mcpServers.%s = %s}", path, name, desc),
			do: func() error {
				return editJSONFile(path, func(root *jsonObject) error {
					return setOmpServer(root, name, url, h)
				})
			},
		}}
	},
	unregister: func(e *connectEnv, name string) []connectStep {
		path := ompMCPPath(e)
		return []connectStep{{
			show: fmt.Sprintf("write %s: delete mcpServers.%s", path, name),
			do: func() error {
				return editJSONFile(path, func(root *jsonObject) error {
					return deleteNested(root, "mcpServers", name)
				})
			},
		}}
	},
}

func ompMCPPath(e *connectEnv) string { return filepath.Join(ompAgentDir(e.home), "mcp.json") }

// setOmpServer writes lasso's keys into mcpServers.<name>, keeping any other key
// the human put on that entry (a timeout, enabled:false) and the order of
// everything else in the file.
func setOmpServer(root *jsonObject, name, url string, h []connectHeader) error {
	if _, ok := root.get("$schema"); !ok && len(root.keys) == 0 {
		root.set("$schema", mustJSON(ompMCPSchema))
	}
	servers, err := childObject(root, "mcpServers")
	if err != nil {
		return err
	}
	entry, err := childObject(servers, name)
	if err != nil {
		return err
	}
	entry.set("type", mustJSON("http"))
	entry.set("url", mustJSON(url))
	if len(h) > 0 {
		entry.set("headers", mustJSON(headerObject(h)))
	} else {
		entry.del("headers")
	}
	if err := servers.setObject(name, entry); err != nil {
		return err
	}
	return root.setObject("mcpServers", servers)
}

// headerObject keeps the headers in registration order in the written file.
func headerObject(h []connectHeader) *jsonObject {
	o := newJSONObject()
	for _, x := range h {
		o.set(x.Name, mustJSON(x.Value))
	}
	return o
}

// ---------------------------------------------------------------------------
// order-preserving JSON editing
// ---------------------------------------------------------------------------

// jsonObject is a JSON object that remembers its key order, so rewriting a
// human's config to change one entry leaves the rest of the file where they
// put it. Values stay raw; only the objects on the path being edited are
// decoded.
type jsonObject struct {
	keys []string
	vals map[string]json.RawMessage
}

func newJSONObject() *jsonObject { return &jsonObject{vals: map[string]json.RawMessage{}} }

func decodeJSONObject(data []byte) (*jsonObject, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	o := newJSONObject()
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(k, v)
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON object")
	}
	return o, nil
}

func (o *jsonObject) get(k string) (json.RawMessage, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *jsonObject) set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *jsonObject) setObject(k string, c *jsonObject) error {
	b, err := c.MarshalJSON()
	if err != nil {
		return err
	}
	o.set(k, b)
	return nil
}

func (o *jsonObject) del(k string) bool {
	if _, ok := o.vals[k]; !ok {
		return false
	}
	delete(o.vals, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
	return true
}

func (o *jsonObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (o *jsonObject) encode() ([]byte, error) {
	raw, _ := o.MarshalJSON()
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// childObject decodes o[k] as an object, or a new empty one when absent.
func childObject(o *jsonObject, k string) (*jsonObject, error) {
	raw, ok := o.get(k)
	if !ok || string(bytes.TrimSpace(raw)) == "null" {
		return newJSONObject(), nil
	}
	c, err := decodeJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", k, err)
	}
	return c, nil
}

func nestedKey(root *jsonObject, parent, name string) (json.RawMessage, bool) {
	raw, ok := root.get(parent)
	if !ok {
		return nil, false
	}
	c, err := decodeJSONObject(raw)
	if err != nil {
		return nil, false
	}
	return c.get(name)
}

func deleteNested(root *jsonObject, parent, name string) error {
	c, err := childObject(root, parent)
	if err != nil {
		return err
	}
	if !c.del(name) {
		return nil
	}
	return root.setObject(parent, c)
}

// readJSONObjectFile is nil (no error) for a missing file.
func readJSONObjectFile(path string) (*jsonObject, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return newJSONObject(), nil
	}
	o, err := decodeJSONObject(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return o, nil
}

// readJSONCObject reads a file that may be JSONC. jsonc reports that it only
// parsed with comments or trailing commas stripped — readable, but not a file
// this may rewrite.
func readJSONCObject(path string) (o *jsonObject, jsonc bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if o, err := decodeJSONObject(data); err == nil {
		return o, false, nil
	}
	o, err = decodeJSONObject(stripJSONC(data))
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	return o, true, nil
}

// stripJSONC drops // and /* */ comments and trailing commas outside strings.
func stripJSONC(b []byte) []byte {
	var out []byte
	inStr, esc := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			out = append(out, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				i++
			}
			if i < len(b) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			i += 2
			for i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/') {
				i++
			}
			i++
		case c == '}' || c == ']':
			j := len(out) - 1
			for j >= 0 && (out[j] == ' ' || out[j] == '\t' || out[j] == '\n' || out[j] == '\r') {
				j--
			}
			if j >= 0 && out[j] == ',' {
				out = append(out[:j], out[j+1:]...)
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// editJSONFile applies edit to a JSON object file (created when missing) and
// writes it back atomically, keeping the previous version as <file>.bak.
func editJSONFile(path string, edit func(*jsonObject) error) error {
	root, err := readJSONObjectFile(path)
	if err != nil {
		return err
	}
	if root == nil {
		root = newJSONObject()
	}
	if err := edit(root); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	out, err := root.encode()
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out, 0o600)
}

// backedUp records the files this run has already backed up, so the .bak is
// the file as it was BEFORE lasso connect ran — not as it was between the two
// servers' edits of it.
var backedUp = map[string]bool{}

// writeFileAtomic replaces path with data via a temp file and a rename in the
// same directory, so a crash leaves the old file or the new one and never half
// of each. The previous contents are kept as <path>.bak. A symlinked config
// (a dotfiles repo) is written through, not replaced by a plain file. An
// existing file keeps its mode minus any group/other bits — these files carry
// auth headers — and perm applies to a new one.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	mode := perm
	old, err := os.ReadFile(path)
	switch {
	case err == nil:
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm() &^ 0o077
		}
		if bytes.Equal(old, data) {
			return nil
		}
		if !backedUp[path] {
			if err := os.WriteFile(path+".bak", old, mode); err != nil {
				return fmt.Errorf("back up %s: %w", path, err)
			}
			backedUp[path] = true
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ---------------------------------------------------------------------------
// the command
// ---------------------------------------------------------------------------

func cliConnect(args []string) {
	if wantsHelp(args) {
		printConnectUsage(os.Stdout)
		return
	}
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { printConnectUsage(os.Stderr) }
	urlFlag := fs.String("url", "", "the URL this machine reaches lasso on")
	token := fs.String("token", os.Getenv("LASSO_MCP_TOKEN"), "bearer token")
	var extra headerList
	fs.Var(&extra, "header", "extra header 'Name: value' (repeatable)")
	only := fs.String("only", "", "comma-separated agent CLIs")
	scope := fs.String("scope", "user", "Claude Code scope: user, local or project")
	browser := fs.Bool("browser", true, "register lasso-browser too")
	remove := fs.Bool("remove", false, "unregister both servers")
	dryRun := fs.Bool("dry-run", false, "print what would change; change nothing")
	force := fs.Bool("force", false, "skip the probe")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "lasso connect: unexpected argument %q\n", fs.Arg(0))
		os.Exit(2)
	}
	switch *scope {
	case "user", "local", "project":
	default:
		fmt.Fprintf(os.Stderr, "lasso connect: -scope %q: want user, local or project\n", *scope)
		os.Exit(2)
	}
	base, err := connectBase(*urlFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lasso connect: %v\n", err)
		os.Exit(2)
	}
	ids, err := connectTargetIDs(*only)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lasso connect: %v\n", err)
		os.Exit(2)
	}
	env, err := newConnectEnv(*scope, *dryRun)
	if err != nil {
		fatal("connect: %v", err)
	}
	headers := connectHeaders(*token, os.Getenv("UI_AUTH"), extra)

	servers := []struct{ name, url, skip string }{
		{connectServerName, base + "/mcp", ""},
		{connectBrowserName, base + "/browser-mcp", ""},
	}
	var browserWhy string
	if !*remove {
		if !*browser {
			servers[1].skip = "-browser=false"
		}
		if !*force {
			ctx, cancel := context.WithTimeout(context.Background(), connectProbeTimeout)
			p, err := probeLasso(ctx, base, headers)
			cancel()
			if err != nil {
				fmt.Fprintf(os.Stderr, "lasso connect: %v\n", err)
				fmt.Fprintln(os.Stderr, connectProbeHint(base, headers))
				fmt.Fprintln(os.Stderr, "Nothing was registered. (-force registers without asking.)")
				os.Exit(1)
			}
			if !p.browserOK && *browser {
				browserWhy = p.browserReason
				servers[1].skip = "lasso's /browser-mcp is unavailable (see below)"
			}
		}
	}

	mode := ""
	if *dryRun {
		mode = " (dry run: nothing is changed)"
	}
	auth := "no auth header"
	if len(headers) > 0 {
		var hs []string
		for _, h := range headers {
			hs = append(hs, h.Name+": "+maskSecret(h.Value))
		}
		auth = strings.Join(hs, ", ")
	}
	if *remove {
		fmt.Printf("lasso connect: removing %s and %s%s\n", connectServerName, connectBrowserName, mode)
	} else {
		fmt.Printf("lasso connect → %s  [%s]%s\n", base, auth, mode)
	}

	failed := false
	for _, id := range ids {
		t, known := connectTargets[id]
		why := ""
		switch {
		case !known:
			why = "lasso connect does not know how to register MCP servers with " + id
		default:
			why = t.detect(env)
		}
		for _, s := range servers {
			var r connectResult
			switch {
			case why != "":
				r = connectResult{server: s.name, status: "skipped: " + why}
			case s.skip != "" && !*remove:
				r = connectResult{server: s.name, status: "skipped: " + s.skip}
			case *remove:
				r = connectRemove(env, t, s.name)
			default:
				r = connectApply(env, t, s.name, s.url, headers)
			}
			r.target = id
			failed = failed || r.failed()
			fmt.Printf("  %-9s %-14s %s\n", r.target, r.server, r.status)
			if *dryRun {
				for _, st := range r.steps {
					fmt.Printf("      %s\n", st)
				}
			}
		}
	}
	if browserWhy != "" {
		fmt.Printf("\nlasso-browser was not registered: %s\n", browserWhy)
		fmt.Println("On lasso's machine, install Chrome or Chromium and chrome-devtools-mcp")
		fmt.Println("(npm i -g chrome-devtools-mcp), then run lasso connect again.")
		fmt.Println("README → Shared browser has the details.")
	}
	if failed {
		os.Exit(1)
	}
}

// connectProbeHint says the likeliest reason /mcp did not answer, which
// depends on where lasso was looked for.
func connectProbeHint(base string, h []connectHeader) string {
	u, _ := url.Parse(base)
	host := ""
	if u != nil {
		host = u.Hostname()
	}
	names := map[string]bool{}
	for _, x := range h {
		names[strings.ToLower(x.Name)] = true
	}
	switch {
	case u != nil && u.Scheme == "https" && !names["cf-access-client-id"]:
		return "Behind Cloudflare Access? Pass a service token: -header 'CF-Access-Client-Id: …' -header 'CF-Access-Client-Secret: …'"
	case host == "127.0.0.1" || host == "localhost" || host == "::1":
		return "Is lasso running here (lasso status)? From another machine, pass -url with the URL this machine reaches lasso on."
	default:
		return "Check that " + base + " is the URL this machine reaches lasso on."
	}
}
