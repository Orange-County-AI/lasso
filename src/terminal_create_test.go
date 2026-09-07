package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// terminalCreateBackend is a fake host that answers the real UHP contract
// through uhpFixtureReply and mutates its own topology the way the server does:
// workspace.open adds a workspace at the path, tab.new adds a pane to the
// focused workspace. Every call is recorded so a test can assert on the
// sequence lasso actually sends — which is the whole point here, since the two
// creation paths are focus-driven and their correctness IS the ordering.
type terminalCreateBackend struct {
	*memBackend
	panes []pane
	calls []terminalCall
	// missingWorkspace is a workspace_id that workspace.focus refuses with
	// not_found, standing in for a workspace cleaned up under the picker.
	missingWorkspace string
}

type terminalCall struct {
	method string
	params map[string]any
}

func (b *terminalCreateBackend) HomeDir() (string, error) { return "/home/test", nil }

func (b *terminalCreateBackend) call(method string) map[string]any {
	for _, c := range b.calls {
		if c.method == method {
			return c.params
		}
	}
	return nil
}

func (b *terminalCreateBackend) methods() []string {
	out := make([]string, 0, len(b.calls))
	for _, c := range b.calls {
		out = append(out, c.method)
	}
	return out
}

func (b *terminalCreateBackend) focus(paneID string) {
	for i := range b.panes {
		b.panes[i].Focused = b.panes[i].PaneID == paneID
	}
}

func (b *terminalCreateBackend) focused() pane {
	for _, p := range b.panes {
		if p.Focused {
			return p
		}
	}
	return pane{}
}

func (b *terminalCreateBackend) LuvusCall(method string, params any) (json.RawMessage, error) {
	p, _ := params.(map[string]any)
	b.calls = append(b.calls, terminalCall{method, p})
	switch method {
	case "terminal.backend.create":
		path, _ := p["cwd"].(string)
		id := strconv.Itoa(len(b.panes) + 1)
		next := pane{PaneID: id, TerminalID: "terminal-" + id, Cwd: path, WorkspaceID: "ws-" + id, TabID: "tab-" + id, WorkspaceNumber: len(b.panes), TabNumber: 1}
		for _, v := range b.panes {
			if v.Cwd == path {
				next.WorkspaceID, next.WorkspaceLabel, next.WorkspaceNumber = v.WorkspaceID, v.WorkspaceLabel, v.WorkspaceNumber
				next.TabNumber = max(next.TabNumber, v.TabNumber+1)
			}
		}
		b.panes = append(b.panes, next)
		if focus, _ := p["focus"].(bool); focus {
			b.focus(next.PaneID)
		}
		return json.Marshal(map[string]any{"type": "terminal_backend_created", "pane_id": next.PaneID, "terminal_id": next.TerminalID})
	case "workspace.open":
		path, _ := p["path"].(string)
		for _, v := range b.panes {
			if v.Cwd == path {
				b.focus(v.PaneID)
				return uhpFixtureReply(b.panes, method, params)
			}
		}
		next := pane{
			PaneID:          strconv.Itoa(len(b.panes) + 1),
			Cwd:             path,
			WorkspaceID:     "ws-" + strconv.Itoa(len(b.panes)+1),
			TabID:           "tab-" + strconv.Itoa(len(b.panes)+1),
			WorkspaceNumber: len(b.panes),
			TabNumber:       1,
		}
		b.panes = append(b.panes, next)
		b.focus(next.PaneID)
		return uhpFixtureReply(b.panes, method, params)
	case "workspace.focus":
		id, _ := p["workspace_id"].(string)
		if id == b.missingWorkspace {
			return nil, &luvusError{Code: "not_found", Message: "workspace id " + id + " not found"}
		}
		for _, v := range b.panes {
			if v.WorkspaceID == id {
				b.focus(v.PaneID)
				return json.RawMessage(`{"type":"ok"}`), nil
			}
		}
		return nil, &luvusError{Code: "not_found", Message: "workspace id " + id + " not found"}
	case "tab.new":
		host := b.focused()
		if host.WorkspaceID == "" {
			return nil, &luvusError{Code: "no_session", Message: "no workspace"}
		}
		tab := 0
		for _, v := range b.panes {
			if v.WorkspaceID == host.WorkspaceID && v.TabNumber > tab {
				tab = v.TabNumber
			}
		}
		next := pane{
			PaneID:          strconv.Itoa(len(b.panes) + 1),
			Cwd:             host.Cwd,
			WorkspaceID:     host.WorkspaceID,
			WorkspaceLabel:  host.WorkspaceLabel,
			WorkspaceNumber: host.WorkspaceNumber,
			TabID:           fmt.Sprintf("%s-t%d", host.WorkspaceID, tab+1),
			TabNumber:       tab + 1,
		}
		b.panes = append(b.panes, next)
		b.focus(next.PaneID)
		return json.Marshal(map[string]any{"type": "tab", "tab": strconv.Itoa(next.TabNumber)})
	case "workspace.rename":
		id, _ := p["workspace_id"].(string)
		name, _ := p["name"].(string)
		for i := range b.panes {
			if b.panes[i].WorkspaceID == id {
				b.panes[i].WorkspaceLabel = name
			}
		}
		return json.RawMessage(`{"type":"workspace_rename"}`), nil
	case "tab.rename":
		id, _ := p["tab_id"].(string)
		name, _ := p["name"].(string)
		for i := range b.panes {
			if b.panes[i].TabID == id {
				b.panes[i].TabLabel = name
			}
		}
		return json.RawMessage(`{"type":"ok"}`), nil
	case "pane.focus":
		id, _ := p["pane"].(string)
		b.focus(id)
		return json.RawMessage(`{"type":"ok"}`), nil
	case "pane.read":
		return json.Marshal(map[string]any{"type": "pane_read", "text": "$ "})
	case "pane.run":
		return json.RawMessage(`{"type":"ok"}`), nil
	}
	return uhpFixtureReply(b.panes, method, params)
}

func withTerminalBackend(t *testing.T, panes ...pane) *terminalCreateBackend {
	t.Helper()
	b := &terminalCreateBackend{memBackend: newMemBackend(), panes: panes}
	prev := defaultBackend()
	setDefaultBackend(b)
	t.Cleanup(func() { setDefaultBackend(prev) })
	return b
}

// existingWorkspacePane is a workspace already open on the fake host, holding
// one tab with one focused pane.
func existingWorkspacePane() pane {
	return pane{
		PaneID: "1", Cwd: "/work/app", Focused: true,
		WorkspaceID: "ws1", WorkspaceLabel: "~", WorkspaceNumber: 0,
		TabID: "ws1-t1", TabNumber: 1,
	}
}

func postCreateTerminal(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/create-terminal", strings.NewReader(body))
	res := httptest.NewRecorder()
	serveCreateTerminal(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	return res
}

// Focus is not a parameter of any UHP call in the creation sequence —
// workspace.open and tab.new always focus what they make — so focus:false can
// only be honored by putting focus back afterwards. That restore is the whole
// behavior, and its absence is what would yank a watching user's screen.
func TestCreateTerminalRestoresFocusOnlyWhenDeclined(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		wantRestore bool
	}{
		{name: "focus defaults on", body: `{}`, wantRestore: false},
		{name: "caller declines focus", body: `{"focus":false}`, wantRestore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := withTerminalBackend(t, existingWorkspacePane())
			postCreateTerminal(t, tc.body)

			if got := b.focused().PaneID == "1"; got != tc.wantRestore {
				t.Fatalf("focus back on the original pane = %v, want %v (methods: %v)", got, tc.wantRestore, b.methods())
			}
		})
	}
}

func TestCreateTerminalOpensAndNamesANewWorkspace(t *testing.T) {
	b := withTerminalBackend(t)
	res := postCreateTerminal(t, `{"workspace_name":"project","tab_name":"dev","command":"git status"}`)

	var out createTerminalResp
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.WorkspaceID == "" || out.TabID == "" || out.RootPane == "" {
		t.Fatalf("response = %#v, want the created workspace/tab/pane identified", out)
	}
	if out.CommandError != "" || out.TabNameError != "" {
		t.Errorf("response carried errors: %#v", out)
	}
	panes, err := runtimePanes(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range panes {
		if p.PaneID == out.RootPane && (p.Cwd != "/home/test" || p.WorkspaceLabel != "project" || p.TabLabel != "dev") {
			t.Fatalf("new terminal has wrong directory or labels: %+v", p)
		}
	}
}

// tab.new acts on whichever workspace is ACTIVE and answers with nothing but a
// 1-based position, so the target workspace must be focused first and the new
// tab's stable id resolved from the topology afterwards.
func TestCreateTerminalAddsATabToAnExistingWorkspace(t *testing.T) {
	b := withTerminalBackend(t, existingWorkspacePane())
	res := postCreateTerminal(t, `{"workspace_id":"ws1","tab_name":"2","command":"git status"}`)

	if got := b.call("workspace.focus")["workspace_id"]; got != "ws1" {
		t.Errorf("workspace.focus workspace_id = %#v, want ws1", got)
	}
	if b.call("workspace.open") != nil {
		t.Errorf("opened a workspace instead of adding a tab: %v", b.methods())
	}
	var out createTerminalResp
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.WorkspaceID != "ws1" {
		t.Errorf("workspace_id = %q, want ws1", out.WorkspaceID)
	}
	if out.TabID != "ws1-t2" || out.RootPane != "2" {
		t.Fatalf("response = %#v, want the second tab's own id and pane", out)
	}
	if got := b.call("tab.rename")["tab_id"]; got != "ws1-t2" {
		t.Errorf("tab.rename tab_id = %#v, want the new tab", got)
	}
}

// A workspace can be cleaned up between the picker listing it and the create
// landing. The persisted label is the recovery key: re-resolve it, and only
// open a fresh workspace when no live match remains.
func TestCreateTerminalRecoversFromACleanedWorkspace(t *testing.T) {
	t.Run("label still matches a live workspace", func(t *testing.T) {
		live := existingWorkspacePane()
		b := withTerminalBackend(t, live)
		b.missingWorkspace = "stale"
		res := postCreateTerminal(t, `{"workspace_id":"stale","workspace_name":"~"}`)

		if b.call("workspace.open") != nil {
			t.Errorf("opened a new workspace despite a live match for the label: %v", b.methods())
		}
		var out createTerminalResp
		if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.WorkspaceID != "ws1" {
			t.Errorf("workspace_id = %q, want the workspace the label resolved to", out.WorkspaceID)
		}
	})

	t.Run("nothing matches the label", func(t *testing.T) {
		b := withTerminalBackend(t)
		b.missingWorkspace = "stale"
		res := postCreateTerminal(t, `{"workspace_id":"stale","workspace_name":"~"}`)
		var out createTerminalResp
		if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		panes, err := runtimePanes(b)
		if err != nil {
			t.Fatal(err)
		}
		if len(panes) != 1 || panes[0].PaneID != out.RootPane || panes[0].Cwd != "/home/test" {
			t.Fatalf("stale workspace did not recover to a fresh home terminal: %+v", panes)
		}
	})
}

func TestRepeatedTerminalCreationNeverReusesAnExistingPane(t *testing.T) {
	existing := existingWorkspacePane()
	existing.Cwd = "/home/test"
	existing.WorkspaceLabel = "Keep this workspace"
	b := withTerminalBackend(t, existing)
	var first, second createTerminalResp
	if err := json.Unmarshal(postCreateTerminal(t, `{"workspace_name":"New terminal","focus":false}`).Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(postCreateTerminal(t, `{"workspace_name":"Another terminal","focus":false}`).Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if first.RootPane == existing.PaneID || second.RootPane == first.RootPane || second.RootPane == existing.PaneID {
		t.Fatalf("creation reused a terminal: original=%s first=%s second=%s", existing.PaneID, first.RootPane, second.RootPane)
	}
	if b.focused().PaneID != existing.PaneID || b.panes[0].WorkspaceLabel != existing.WorkspaceLabel {
		t.Fatal("creating a terminal changed the existing workspace or its focus")
	}
}

// pane.run submits a multi-line command one line at a time, so the tail would
// execute as separate broken commands. That is the one delivery hazard left
// after the raw-PTY write went away, and it must be refused rather than sent.
func TestCreateTerminalRefusesAMultiLineCommand(t *testing.T) {
	withTerminalBackend(t)
	req := httptest.NewRequest(http.MethodPost, "/api/create-terminal",
		strings.NewReader(`{"command":"echo one\necho two"}`))
	res := httptest.NewRecorder()
	serveCreateTerminal(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", res.Code, res.Body.String())
	}
}

// /api/workspaces has its own contract, which the picker is written against.
// UHP names the same four things differently (name/workspace/tabs/active), so
// the translation is the endpoint's job.
func TestListTerminalWorkspacesTranslatesUHPFieldNames(t *testing.T) {
	withTerminalBackend(t, pane{
		PaneID: "1", Cwd: "/work/app", Focused: true,
		WorkspaceID: "ws1", WorkspaceLabel: "app", WorkspaceNumber: 3,
		TabID: "ws1-t1", TabNumber: 1,
	})
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
	if len(out.Workspaces) != 1 {
		t.Fatalf("workspaces = %#v, want one", out.Workspaces)
	}
	got := out.Workspaces[0]
	if got.WorkspaceID != "ws1" || got.Label != "app" || got.Number != 3 || !got.Focused {
		t.Errorf("workspace = %#v, want ws1/app/3/focused", got)
	}
}

// workspace.rename refuses a name over 40 characters outright, and agent titles
// are written for a sidebar row rather than that budget — so a long one must be
// trimmed on a word boundary before it is sent, not rejected and not truncated
// mid-token.
func TestWorkspaceNameFitsTheRuntimeCap(t *testing.T) {
	long := "Migrate the whole lasso integration from luvus onto the Luvus harness protocol"
	got := workspaceName(long)
	if len([]rune(got)) > workspaceNameMaxLen {
		t.Errorf("workspaceName(%q) = %q (%d runes), want at most %d", long, got, len([]rune(got)), workspaceNameMaxLen)
	}
	if strings.HasSuffix(got, " ") || !strings.HasPrefix(long, got) {
		t.Errorf("workspaceName = %q, want a whole-word prefix of the title", got)
	}
	if !strings.HasSuffix(got, "integration") {
		t.Errorf("workspaceName = %q, want the cut at the last word boundary that fits", got)
	}
	if got := workspaceName("  short  "); got != "short" {
		t.Errorf("workspaceName trimmed = %q, want short", got)
	}
	if got := workspaceName("   "); got != "" {
		t.Errorf("workspaceName of blank = %q, want \"\" so no rename is attempted", got)
	}
}
