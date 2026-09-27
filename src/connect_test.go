package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeConnectEnv is a machine with every CLI installed, a throwaway home, and a
// command runner that records instead of running.
type fakeRun struct {
	calls [][]string
	reply func(args []string) ([]byte, error)
}

func fakeConnectEnv(t *testing.T) (*connectEnv, *fakeRun) {
	t.Helper()
	fr := &fakeRun{}
	home := t.TempDir()
	e := &connectEnv{
		home:     home,
		cwd:      filepath.Join(home, "proj"),
		scope:    "user",
		lookPath: func(string) (string, error) { return "/usr/bin/x", nil },
		run: func(dir, name string, args ...string) ([]byte, error) {
			call := append([]string{name}, args...)
			fr.calls = append(fr.calls, call)
			if fr.reply != nil {
				return fr.reply(call)
			}
			return nil, nil
		},
	}
	return e, fr
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestConnectBase(t *testing.T) {
	t.Setenv("LASSO_URL", "")
	t.Setenv("LASSO_LISTEN", "")
	cases := map[string]string{
		"":                                   "http://" + defaultListenAddr,
		"https://lasso.example.com/":         "https://lasso.example.com",
		"https://lasso.example.com/mcp":      "https://lasso.example.com",
		"http://100.1.2.3:8190/browser-mcp/": "http://100.1.2.3:8190",
		"http://h:1/sub/path":                "http://h:1/sub/path",
	}
	for in, want := range cases {
		got, err := connectBase(in)
		if err != nil || got != want {
			t.Errorf("connectBase(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"lasso.example.com", "ftp://x", "http://"} {
		if _, err := connectBase(bad); err == nil {
			t.Errorf("connectBase(%q) accepted", bad)
		}
	}
	// The same resolution notify and mcp use when -url is absent.
	t.Setenv("LASSO_LISTEN", "127.0.0.1:9999")
	if got, _ := connectBase(""); got != "http://127.0.0.1:9999" {
		t.Errorf("LASSO_LISTEN: %q", got)
	}
	t.Setenv("LASSO_URL", "https://tunnel.example/")
	if got, _ := connectBase(""); got != "https://tunnel.example" {
		t.Errorf("LASSO_URL: %q", got)
	}
}

func TestConnectHeaders(t *testing.T) {
	// A bearer token wins over UI_AUTH, as withMCPAuth accepts either.
	h := connectHeaders("tok", "u:p", nil)
	if len(h) != 1 || h[0].Value != "Bearer tok" {
		t.Errorf("token: %+v", h)
	}
	h = connectHeaders("", "u:p", nil)
	if len(h) != 1 || h[0].Value != "Basic dTpw" {
		t.Errorf("basic: %+v", h)
	}
	if h := connectHeaders("", "", nil); len(h) != 0 {
		t.Errorf("none: %+v", h)
	}
	cf1, _ := parseConnectHeader("CF-Access-Client-Id: id.access")
	cf2, _ := parseConnectHeader("CF-Access-Client-Secret:  s3cr3t ")
	over, _ := parseConnectHeader("authorization: Bearer other")
	h = connectHeaders("tok", "", []connectHeader{cf1, cf2, over})
	if len(h) != 3 || h[0].Value != "Bearer other" || h[1].Name != "CF-Access-Client-Id" || h[2].Value != "s3cr3t" {
		t.Errorf("extras: %+v", h)
	}
	for _, bad := range []string{"NoColon", ": v", "Bad Name: v", "X:"} {
		if _, err := parseConnectHeader(bad); err == nil {
			t.Errorf("parseConnectHeader(%q) accepted", bad)
		}
	}
}

func TestMaskSecretNeverShowsTheSecret(t *testing.T) {
	for _, v := range []string{"Bearer abcdefghijklmnop", "Basic dXNlcjpwYXNzd29yZA==", "short", "Bearer tiny", "0123456789abcdef"} {
		m := maskSecret(v)
		_, secret, _ := strings.Cut(v, " ")
		if secret == "" {
			secret = v
		}
		if strings.Contains(m, secret) {
			t.Errorf("maskSecret(%q) = %q leaks it", v, m)
		}
	}
	if m := maskSecret("Bearer abcdefghijklmnop"); !strings.HasPrefix(m, "Bearer abcd") {
		t.Errorf("scheme and prefix dropped: %q", m)
	}
}

func TestConnectTargetsCoverEveryHarness(t *testing.T) {
	for _, h := range harnesses {
		if _, ok := connectTargets[h.ID]; !ok {
			t.Errorf("harness %q has no connect target", h.ID)
		}
	}
	ids, err := connectTargetIDs(" codex , CLAUDE")
	if err != nil || strings.Join(ids, ",") != "claude,codex" {
		t.Errorf("-only: %v %v", ids, err)
	}
	if _, err := connectTargetIDs("claude,nope"); err == nil {
		t.Error("unknown -only accepted")
	}
}

func TestCodexSetHTTPHeaders(t *testing.T) {
	doc := `model = "o3"

[mcp_servers.other]
url = "https://o"
http_headers = { "Keep" = "me" }

[mcp_servers.lasso]
url = "http://h/mcp"
http_headers = { "Authorization" = "Bearer old" }

[mcp_servers.lasso.http_headers]
Authorization = "Bearer older"

[profiles.x]
model = "y"
`
	h := []connectHeader{{"Authorization", `Bearer n"ew\`}, {"CF-Access-Client-Id", "id"}}
	out, err := codexSetHTTPHeaders([]byte(doc), "lasso", h)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	want := `model = "o3"

[mcp_servers.other]
url = "https://o"
http_headers = { "Keep" = "me" }

[mcp_servers.lasso]
http_headers = { "Authorization" = "Bearer n\"ew\\", "CF-Access-Client-Id" = "id" }
url = "http://h/mcp"

[profiles.x]
model = "y"
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// Idempotent, and clearing removes them.
	again, _ := codexSetHTTPHeaders(out, "lasso", h)
	if string(again) != got {
		t.Errorf("not idempotent:\n%s", again)
	}
	cleared, _ := codexSetHTTPHeaders(out, "lasso", nil)
	if strings.Contains(string(cleared), "Bearer") || !strings.Contains(string(cleared), `"Keep" = "me"`) {
		t.Errorf("clear:\n%s", cleared)
	}
	if _, err := codexSetHTTPHeaders([]byte(doc), "missing", h); err == nil {
		t.Error("missing table accepted")
	}
}

func TestCodexEntryReadsGetJSON(t *testing.T) {
	out := []byte("WARNING: noise\n" + `{"name":"lasso","transport":{"type":"streamable_http","url":"http://h/mcp","bearer_token_env_var":null,"http_headers":{"Authorization":"Bearer t"},"env_http_headers":null}}`)
	e, err := codexEntry(out)
	if err != nil {
		t.Fatal(err)
	}
	if !e.matches("http://h/mcp", []connectHeader{{"Authorization", "Bearer t"}}) {
		t.Errorf("entry %+v should match", e)
	}
	if e.matches("http://h/mcp", nil) || e.matches("http://h/x", []connectHeader{{"Authorization", "Bearer t"}}) {
		t.Error("matched a different registration")
	}
	env := []byte(`{"transport":{"type":"streamable_http","url":"http://h/mcp","bearer_token_env_var":"LASSO_MCP_TOKEN"}}`)
	if e, _ := codexEntry(env); e.matches("http://h/mcp", nil) {
		t.Error("an env-var indirection read as lasso's own entry")
	}
}

func TestCodexConnectFlow(t *testing.T) {
	e, fr := fakeConnectEnv(t)
	cfg := filepath.Join(e.home, ".codex", "config.toml")
	writeFixture(t, cfg, "model = \"o3\"\n")
	stored := map[string]bool{}
	fr.reply = func(a []string) ([]byte, error) {
		switch a[2] {
		case "get":
			if !stored[a[3]] {
				return []byte("No MCP server named"), errors.New("exit status 1")
			}
			return []byte(`{"transport":{"type":"streamable_http","url":"http://h/mcp","http_headers":{"Authorization":"Bearer t"}}}`), nil
		case "add":
			// What codex itself does: replace the table, dropping headers.
			stored[a[3]] = true
			writeFixture(t, cfg, "model = \"o3\"\n\n[mcp_servers."+a[3]+"]\nurl = \""+a[5]+"\"\n")
		}
		return nil, nil
	}
	h := []connectHeader{{"Authorization", "Bearer t"}}
	r := connectApply(e, codexConnect, "lasso", "http://h/mcp", h)
	if r.status != "added" {
		t.Fatalf("status %q", r.status)
	}
	// The secret is written to the file, never onto codex's argv.
	for _, c := range fr.calls {
		if strings.Contains(strings.Join(c, " "), "Bearer") {
			t.Errorf("secret on argv: %v", c)
		}
	}
	if !strings.Contains(readFile(t, cfg), `http_headers = { "Authorization" = "Bearer t" }`) {
		t.Errorf("config:\n%s", readFile(t, cfg))
	}
	if r := connectApply(e, codexConnect, "lasso", "http://h/mcp", h); r.status != "unchanged" {
		t.Errorf("second run: %q", r.status)
	}
}

func TestClaudeConnectFlow(t *testing.T) {
	e, fr := fakeConnectEnv(t)
	writeFixture(t, filepath.Join(e.home, ".claude.json"), `{"mcpServers":{"lasso":{"type":"http","url":"http://old/mcp"}},
		"projects":{"`+e.cwd+`":{"mcpServers":{"lasso":{"type":"http","url":"http://h/mcp","headers":{"Authorization":"Bearer t"}}}}}}`)
	h := []connectHeader{{"Authorization", "Bearer t"}}

	// user scope holds a different URL: remove, then add with the header.
	r := connectApply(e, claudeConnect, "lasso", "http://h/mcp", h)
	if r.status != "updated" || len(fr.calls) != 2 {
		t.Fatalf("status %q calls %v", r.status, fr.calls)
	}
	if got := strings.Join(fr.calls[0], " "); got != "claude mcp remove --scope user lasso" {
		t.Errorf("remove: %s", got)
	}
	if got := strings.Join(fr.calls[1], " "); got != "claude mcp add --transport http --scope user lasso http://h/mcp --header Authorization: Bearer t" {
		t.Errorf("add: %s", got)
	}
	if strings.Contains(strings.Join(r.steps, " "), "Bearer t") {
		t.Errorf("shown steps leak the secret: %v", r.steps)
	}

	// local scope reads this directory's entry, which already matches.
	e.scope = "local"
	fr.calls = nil
	if r := connectApply(e, claudeConnect, "lasso", "http://h/mcp", h); r.status != "unchanged" || len(fr.calls) != 0 {
		t.Errorf("local: %q %v", r.status, fr.calls)
	}

	// dry-run runs nothing.
	e.scope, e.dryRun = "project", true
	r = connectApply(e, claudeConnect, "lasso", "http://h/mcp", h)
	if r.status != "would be added" || len(fr.calls) != 0 || len(r.steps) != 1 {
		t.Errorf("dry-run: %q %v %v", r.status, fr.calls, r.steps)
	}
}

const ompFixture = `{
  "$schema": "x",
  "mcpServers": {
    "zeta": {"type": "http", "url": "https://z/mcp", "timeout": 30000},
    "lasso": {"type": "http", "url": "http://old/mcp", "timeout": 5, "headers": {"X": "y"}},
    "alpha": {"command": "a"}
  },
  "disabledServers": ["q"]
}
`

func TestOmpConnectFlow(t *testing.T) {
	e, _ := fakeConnectEnv(t)
	path := ompMCPPath(e)
	writeFixture(t, path, ompFixture)
	h := []connectHeader{{"Authorization", "Bearer t"}}

	// dry-run writes nothing.
	e.dryRun = true
	if r := connectApply(e, ompConnect, "lasso", "http://h/mcp", h); r.status != "would be updated" {
		t.Errorf("dry-run: %q", r.status)
	}
	if readFile(t, path) != ompFixture {
		t.Fatal("dry-run changed the file")
	}
	e.dryRun = false

	if r := connectApply(e, ompConnect, "lasso", "http://h/mcp", h); r.status != "updated" {
		t.Fatalf("update: %q", r.status)
	}
	if r := connectApply(e, ompConnect, "lasso-browser", "http://h/browser-mcp", h); r.status != "added" {
		t.Fatalf("add: %q", r.status)
	}
	got := readFile(t, path)
	// Key order kept, the human's own keys on the entry kept, other servers
	// and top-level keys untouched.
	order := []string{`"$schema"`, `"zeta"`, `"lasso"`, `"timeout": 5`, `"alpha"`, `"lasso-browser"`, `"disabledServers"`}
	last := -1
	for _, k := range order {
		i := strings.Index(got, k)
		if i < 0 || i < last {
			t.Fatalf("%s missing or out of order in:\n%s", k, got)
		}
		last = i
	}
	if strings.Contains(got, `"X"`) || !strings.Contains(got, `"Authorization": "Bearer t"`) {
		t.Errorf("headers not replaced:\n%s", got)
	}
	if readFile(t, path+".bak") != ompFixture {
		t.Error(".bak is not the file as it was before this run")
	}
	if st, _ := os.Stat(path); st.Mode().Perm()&0o077 != 0 {
		t.Errorf("mode %v readable by others", st.Mode().Perm())
	}

	// Idempotent.
	if r := connectApply(e, ompConnect, "lasso", "http://h/mcp", h); r.status != "unchanged" {
		t.Errorf("rerun: %q", r.status)
	}

	// -remove takes out exactly lasso's two entries.
	for _, n := range []string{"lasso", "lasso-browser"} {
		if r := connectRemove(e, ompConnect, n); r.status != "removed" {
			t.Errorf("remove %s: %q", n, r.status)
		}
	}
	if r := connectRemove(e, ompConnect, "lasso"); r.status != "not registered" {
		t.Errorf("second remove: %q", r.status)
	}
	var v struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.MCPServers) != 2 || v.MCPServers["zeta"] == nil || v.MCPServers["alpha"] == nil {
		t.Errorf("after remove: %s", readFile(t, path))
	}
}

func TestOmpConnectCreatesTheFile(t *testing.T) {
	e, _ := fakeConnectEnv(t)
	if r := connectApply(e, ompConnect, "lasso", "http://h/mcp", nil); r.status != "added" {
		t.Fatalf("%q", r.status)
	}
	got := readFile(t, ompMCPPath(e))
	if !strings.Contains(got, ompMCPSchema) || strings.Contains(got, "headers") {
		t.Errorf("new file:\n%s", got)
	}
}

func TestOpencodeConnectFlow(t *testing.T) {
	e, fr := fakeConnectEnv(t)
	e.xdgConfig = filepath.Join(e.home, "xdg")
	jsonc := filepath.Join(e.xdgConfig, "opencode", "opencode.jsonc")
	plain := filepath.Join(e.xdgConfig, "opencode", "opencode.json")
	writeFixture(t, jsonc, `{
  // mine
  "mcp": {
    "lasso": {"type": "remote", "url": "http://h/mcp", "headers": {"Authorization": "Bearer t"},},
  },
}`)
	writeFixture(t, plain, `{"theme":"x","mcp":{"lasso-browser":{"type":"remote","url":"http://h/browser-mcp"},"keep":{"type":"local"}}}`)
	h := []connectHeader{{"Authorization", "Bearer t"}}

	// Readable through its comments: already registered.
	if r := connectApply(e, opencodeConnect, "lasso", "http://h/mcp", h); r.status != "unchanged" {
		t.Errorf("jsonc read: %q", r.status)
	}
	// A different header set: re-added through opencode's own command, which
	// replaces the entry.
	r := connectApply(e, opencodeConnect, "lasso-browser", "http://h/browser-mcp", h)
	if r.status != "updated" || len(fr.calls) != 1 {
		t.Fatalf("%q %v", r.status, fr.calls)
	}
	if got := strings.Join(fr.calls[0], " "); got != "opencode mcp add lasso-browser --url http://h/browser-mcp --header Authorization=Bearer t" {
		t.Errorf("add: %s", got)
	}

	// Removal edits the file (there is no `opencode mcp remove`), and refuses
	// to rewrite one with comments in it rather than drop them.
	if r := connectRemove(e, opencodeConnect, "lasso-browser"); r.status != "removed" {
		t.Errorf("remove json: %q", r.status)
	}
	if got := readFile(t, plain); strings.Contains(got, "lasso-browser") || !strings.Contains(got, `"keep"`) || !strings.Contains(got, `"theme"`) {
		t.Errorf("after remove:\n%s", got)
	}
	if r := connectRemove(e, opencodeConnect, "lasso"); !r.failed() || !strings.Contains(r.status, "comments") {
		t.Errorf("remove jsonc: %q", r.status)
	}
	if !strings.Contains(readFile(t, jsonc), "// mine") {
		t.Error("jsonc was rewritten")
	}
}

func TestStripJSONC(t *testing.T) {
	in := `{"a": "x // not a comment", /* b */ "b": [1, 2,], // c
"c": "\"/*"}`
	var v map[string]any
	if err := json.Unmarshal(stripJSONC([]byte(in)), &v); err != nil {
		t.Fatalf("%v: %s", err, stripJSONC([]byte(in)))
	}
	if v["a"] != "x // not a comment" || v["c"] != `"/*` {
		t.Errorf("%v", v)
	}
}

// The probe is a real MCP round trip against the real handler, sending the
// headers it would register.
func TestProbeLasso(t *testing.T) {
	prevB, prevM := sharedBrowser, browserMCP
	t.Cleanup(func() { sharedBrowser, browserMCP = prevB, prevM })
	sharedBrowser = nil

	var saw string
	mcpH := withMCPAuth(newMCPHandler(), "", "", false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("CF-Access-Client-Id"); v != "" {
			saw = v
		}
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		mcpH.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := probeLasso(ctx, srv.URL, []connectHeader{{"CF-Access-Client-Id", "id.access"}})
	if err != nil {
		t.Fatal(err)
	}
	if saw != "id.access" {
		t.Error("probe did not send the registered headers")
	}
	if p.browserOK || !strings.Contains(p.browserReason, "not configured") {
		t.Errorf("probe = %+v", p)
	}

	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if _, err := probeLasso(ctx, dead.URL, nil); err == nil {
		t.Error("an unreachable lasso probed fine")
	}
}
