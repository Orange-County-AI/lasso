package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type terminalCreateBackend struct {
	*memBackend
	method       string
	params       map[string]any
	renameParams map[string]any
	sent         []string
	tabCreateErr error
	workspaces   string         // workspace.list reply; "" means one "~" workspace
	titled       chan [2]string // background titler requests (tab id, command)
}

func (b *terminalCreateBackend) HomeDir() (string, error) { return "/home/test", nil }

func (b *terminalCreateBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	p, _ := params.(map[string]any)
	switch method {
	case "workspace.create":
		b.method, b.params = method, p
		return json.RawMessage(`{"workspace":{"workspace_id":"ws"},"tab":{"tab_id":"ws:t1","workspace_id":"ws"},"root_pane":{"pane_id":"p1"}}`), nil
	case "tab.create":
		b.method, b.params = method, p
		if b.tabCreateErr != nil {
			return nil, b.tabCreateErr
		}
		return json.RawMessage(`{"tab":{"tab_id":"ws:t2","workspace_id":"ws"},"root_pane":{"pane_id":"p2"}}`), nil
	case "tab.rename":
		b.renameParams = p
		return json.RawMessage(`{}`), nil
	case "workspace.list":
		if b.workspaces != "" {
			return json.RawMessage(b.workspaces), nil
		}
		return json.RawMessage(`{"workspaces":[{"workspace_id":"ws","label":"~","number":1,"tab_count":2,"focused":true}]}`), nil
	case "pane.read":
		return json.RawMessage(`{"read":{"text":"$ "}}`), nil
	case "pane.send_text":
		b.sent = append(b.sent, p["text"].(string))
		return json.RawMessage(`{}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func withTerminalBackend(t *testing.T) *terminalCreateBackend {
	t.Helper()
	b := &terminalCreateBackend{memBackend: newMemBackend()}
	prev := defaultBackend()
	setDefaultBackend(b)
	t.Cleanup(func() { setDefaultBackend(prev) })
	b.titled = make(chan [2]string, 1)
	terminalTitler = func(_ Backend, tabID, command string) { b.titled <- [2]string{tabID, command} }
	t.Cleanup(func() { terminalTitler = autoTitleTerminal })
	return b
}

func TestCreateTerminalFocus(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{name: "defaults true", body: `{}`, want: true},
		{name: "caller opts out", body: `{"focus":false}`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := withTerminalBackend(t)
			req := httptest.NewRequest(http.MethodPost, "/api/create-terminal", strings.NewReader(tc.body))
			res := httptest.NewRecorder()
			serveCreateTerminal(res, req)

			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
			}
			if got, ok := b.params["focus"].(bool); !ok || got != tc.want {
				t.Fatalf("workspace.create focus = %#v, want %v", b.params["focus"], tc.want)
			}
			if got := b.params["label"]; got != "Scratch" {
				t.Fatalf("workspace.create label = %#v, want Scratch", got)
			}
		})
	}
}

// A terminal with no workspace named joins the Scratch workspace scratch agents
// already share, rather than making a second "Scratch" beside it.
func TestCreateTerminalJoinsExistingScratch(t *testing.T) {
	b := withTerminalBackend(t)
	b.workspaces = `{"workspaces":[{"workspace_id":"ws","label":"Scratch","number":1,"tab_count":3}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/create-terminal", strings.NewReader(`{"tab_name":"logs"}`))
	res := httptest.NewRecorder()
	serveCreateTerminal(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if b.method != "tab.create" || b.params["workspace_id"] != "ws" || b.params["label"] != "logs" {
		t.Fatalf("%s %#v, want tab.create into ws labeled logs", b.method, b.params)
	}
	var out createTerminalResp
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.WorkspaceID != "ws" || out.TabID != "ws:t2" || out.RootPane != "p2" {
		t.Fatalf("response = %#v", out)
	}
}

// The picker sends Scratch's id once it exists. That must still root the tab
// at home, not inherit the directory of whichever scratch agent made Scratch.
func TestCreateTerminalScratchByIDStartsAtHome(t *testing.T) {
	b := withTerminalBackend(t)
	b.workspaces = `{"workspaces":[{"workspace_id":"ws","label":"Scratch","number":1,"tab_count":3}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/create-terminal", strings.NewReader(`{"workspace_id":"ws","workspace_name":"Scratch"}`))
	res := httptest.NewRecorder()
	serveCreateTerminal(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if b.method != "tab.create" || b.params["workspace_id"] != "ws" || b.params["cwd"] != "/home/test" {
		t.Fatalf("%s %#v, want tab.create into ws at /home/test", b.method, b.params)
	}
}

func TestCreateTerminalInExistingWorkspaceRunsCommand(t *testing.T) {
	b := withTerminalBackend(t)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/create-terminal",
		strings.NewReader(`{"workspace_id":"ws","tab_name":"2","command":"git status"}`),
	)
	res := httptest.NewRecorder()
	serveCreateTerminal(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if b.method != "tab.create" {
		t.Fatalf("method = %q, want tab.create", b.method)
	}
	if got := b.params["workspace_id"]; got != "ws" {
		t.Fatalf("workspace_id = %#v, want ws", got)
	}
	if got := b.params["label"]; got != "2" {
		t.Fatalf("tab.create label = %#v, want 2", got)
	}
	if len(b.sent) != 1 || b.sent[0] != "\x15git status\n" {
		t.Fatalf("sent = %#v, want command submission", b.sent)
	}
	var out createTerminalResp
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.WorkspaceID != "ws" || out.TabID != "ws:t2" || out.RootPane != "p2" {
		t.Fatalf("response = %#v", out)
	}
}

// A multi-line command is run as ONE script, not a command per line: the block
// is fed to the shell already in the pane as a sourced heredoc, so it is parsed
// whole — a heredoc, an if/then/fi or a quoted string inside it means what it
// means — and the terminal sees one submission. A single-line command is typed
// as itself.
func TestCreateTerminalRunsMultiLineAsOneScript(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "single line stays bare",
			command: "git status",
			want:    "\x15git status\n",
		},
		{
			name:    "two commands become one script",
			command: "echo hello\necho world",
			want:    "\x15. /dev/stdin <<'LASSO_EOF'\necho hello\necho world\nLASSO_EOF\n",
		},
		{
			name:    "crlf paste folds before wrapping",
			command: "echo hello\r\necho world\r\n",
			want:    "\x15. /dev/stdin <<'LASSO_EOF'\necho hello\necho world\nLASSO_EOF\n",
		},
		{
			// A line that IS the delimiter would end the body early, and the
			// rest of the script would reach the shell as typed input.
			name:    "delimiter inside the script",
			command: "echo hello\nLASSO_EOF\necho world",
			want:    "\x15. /dev/stdin <<'LASSO_EOF_'\necho hello\nLASSO_EOF\necho world\nLASSO_EOF_\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := withTerminalBackend(t)
			req := httptest.NewRequest(
				http.MethodPost,
				"/api/create-terminal",
				strings.NewReader(fmt.Sprintf(`{"workspace_id":"ws","command":%q}`, tc.command)),
			)
			res := httptest.NewRecorder()
			serveCreateTerminal(res, req)

			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
			}
			if len(b.sent) != 1 || b.sent[0] != tc.want {
				t.Fatalf("sent = %#v, want %q", b.sent, tc.want)
			}
		})
	}
}

func TestCreateTerminalNamesNewWorkspaceRootTab(t *testing.T) {
	b := withTerminalBackend(t)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/create-terminal",
		strings.NewReader(`{"workspace_name":"project","tab_name":"dev"}`),
	)
	res := httptest.NewRecorder()
	serveCreateTerminal(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if b.method != "workspace.create" {
		t.Fatalf("method = %q, want workspace.create", b.method)
	}
	if got := b.renameParams["tab_id"]; got != "ws:t1" {
		t.Fatalf("tab.rename tab_id = %#v, want ws:t1", got)
	}
	if got := b.renameParams["label"]; got != "dev" {
		t.Fatalf("tab.rename label = %#v, want dev", got)
	}
}

func TestCreateTerminalRecreatesCleanedWorkspace(t *testing.T) {
	b := withTerminalBackend(t)
	b.tabCreateErr = errors.New("herdr error workspace_not_found: workspace gone not found")
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/create-terminal",
		strings.NewReader(`{"workspace_id":"gone","workspace_name":"~"}`),
	)
	res := httptest.NewRecorder()
	serveCreateTerminal(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if b.method != "workspace.create" {
		t.Fatalf("method = %q, want workspace.create", b.method)
	}
	if got := b.params["label"]; got != "~" {
		t.Fatalf("workspace.create label = %#v, want ~", got)
	}
}

func TestListTerminalWorkspaces(t *testing.T) {
	withTerminalBackend(t)
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	res := httptest.NewRecorder()
	serveWorkspaces(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	var out struct {
		Workspaces []terminalWorkspace `json:"workspaces"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Workspaces) != 1 || out.Workspaces[0].Label != "~" || out.Workspaces[0].TabCount != 2 {
		t.Fatalf("workspaces = %#v", out.Workspaces)
	}
}

func TestCreateTerminalNamesTabAfterTrivialCommand(t *testing.T) {
	b := withTerminalBackend(t)
	req := httptest.NewRequest(http.MethodPost, "/api/create-terminal",
		strings.NewReader(`{"workspace_id":"ws","command":"btm"}`))
	res := httptest.NewRecorder()
	serveCreateTerminal(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if got := b.params["label"]; got != "btm" {
		t.Fatalf("tab.create label = %#v, want btm", got)
	}
	select {
	case got := <-b.titled:
		t.Fatalf("trivial command asked the titler: %v", got)
	default:
	}
}

func TestCreateTerminalAsksTitlerForScript(t *testing.T) {
	b := withTerminalBackend(t)
	req := httptest.NewRequest(http.MethodPost, "/api/create-terminal",
		strings.NewReader(`{"workspace_id":"ws","command":"cd ~/projects/lasso && mise run dev"}`))
	res := httptest.NewRecorder()
	serveCreateTerminal(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	// The program's name at once; the titler's answer replaces it later.
	if got := b.params["label"]; got != "cd" {
		t.Fatalf("tab.create label = %#v, want cd", got)
	}
	got := <-b.titled
	if got[0] != "ws:t2" || got[1] != "cd ~/projects/lasso && mise run dev" {
		t.Fatalf("titler asked for %v", got)
	}
}

func TestCreateTerminalExplicitTabNameWins(t *testing.T) {
	b := withTerminalBackend(t)
	req := httptest.NewRequest(http.MethodPost, "/api/create-terminal",
		strings.NewReader(`{"workspace_id":"ws","tab_name":"logs","command":"tail -f /var/log/syslog | grep err"}`))
	res := httptest.NewRecorder()
	serveCreateTerminal(res, req)
	if got := b.params["label"]; got != "logs" {
		t.Fatalf("tab.create label = %#v, want logs", got)
	}
	select {
	case got := <-b.titled:
		t.Fatalf("explicit name asked the titler: %v", got)
	default:
	}
}

func postCreateTerminal(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/create-terminal", strings.NewReader(body))
	res := httptest.NewRecorder()
	serveCreateTerminal(res, req)
	return res
}

// A working directory roots the shell wherever the terminal lands: a new
// workspace, a tab in an existing one, or Scratch. "~" expands against the
// target host's home, not lasso's.
func TestCreateTerminalCwd(t *testing.T) {
	for _, tc := range []struct {
		name, body, workspaces, method string
	}{
		{name: "new workspace", body: `{"workspace_name":"proj","cwd":"~/src/proj"}`, method: "workspace.create"},
		{name: "existing workspace", body: `{"workspace_id":"ws","workspace_name":"~","cwd":"/home/test/src/proj"}`, method: "tab.create"},
		{
			name:       "scratch",
			body:       `{"cwd":"~/src/proj/"}`,
			workspaces: `{"workspaces":[{"workspace_id":"ws","label":"Scratch","number":1,"tab_count":1}]}`,
			method:     "tab.create",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := withTerminalBackend(t)
			b.workspaces = tc.workspaces
			b.mkdirAllAncestors("/home/test/src/proj")
			res := postCreateTerminal(t, tc.body)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
			}
			if b.method != tc.method || b.params["cwd"] != "/home/test/src/proj" {
				t.Fatalf("%s %#v, want %s at /home/test/src/proj", b.method, b.params, tc.method)
			}
		})
	}
}

// Without a cwd a tab in an existing workspace keeps herdr's own default.
func TestCreateTerminalNoCwdInExistingWorkspace(t *testing.T) {
	b := withTerminalBackend(t)
	if res := postCreateTerminal(t, `{"workspace_id":"ws","workspace_name":"~"}`); res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if _, ok := b.params["cwd"]; ok {
		t.Fatalf("tab.create %#v, want no cwd", b.params)
	}
}

// herdr would start a shell somewhere else for a bad cwd, so it is refused
// before anything is created.
func TestCreateTerminalRejectsBadCwd(t *testing.T) {
	for _, cwd := range []string{"relative/dir", "/home/test/missing", "/home/test/notes.txt"} {
		t.Run(cwd, func(t *testing.T) {
			b := withTerminalBackend(t)
			b.addFile("/home/test/notes.txt", "x")
			res := postCreateTerminal(t, fmt.Sprintf(`{"workspace_name":"proj","cwd":%q}`, cwd))
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", res.Code, res.Body.String())
			}
			if b.method != "" {
				t.Fatalf("%s was called for a refused cwd", b.method)
			}
		})
	}
}
