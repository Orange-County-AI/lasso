package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// POST /api/create-terminal always creates a fresh PTY. A named workspace uses
// tab.new under the per-host focus lock; the home-directory path uses atomic
// terminal.backend.create so an existing home workspace cannot reuse a pane.
// Commands run only after the new shell settles.
type createTerminalReq struct {
	Command       string `json:"command"`
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	TabName       string `json:"tab_name"`
	// Focus lands the user on the new terminal (default true — the web dialog
	// wants you typing immediately). An API caller creating a terminal for
	// someone else passes false so it does not yank every client's shared focus.
	Focus *bool `json:"focus"`
	// Host names the machine to create the terminal on; empty means the calling
	// tab's own host (and, for a caller that names none, the default one).
	Host string `json:"host"`
}

type createTerminalResp struct {
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id,omitempty"`
	RootPane    string `json:"root_pane"`
	// Creation still succeeded when command delivery failed. Returning the
	// warning separately prevents a client retry from duplicating the terminal.
	CommandError string `json:"command_error,omitempty"`
	TabNameError string `json:"tab_name_error,omitempty"`
}

// terminalWorkspace is /api/workspaces' own row shape, which the picker is
// written against — deliberately not UHP's. workspace.list calls the same four
// things `name`, `workspace` (a JSON string), `tabs` and `active`; translating
// once here keeps the endpoint's contract independent of the runtime's field
// names.
type terminalWorkspace struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
	TabCount    int    `json:"tab_count"`
	Focused     bool   `json:"focused"`
	// NextTabName is the default name for a tab created here: the smallest
	// positive integer no existing tab in this workspace is already named.
	// Luvus leaves new tabs unnamed, so the monotonic "1, 2, 3" a user expects
	// has to be produced by lasso from the names that are actually in use.
	NextTabName string `json:"next_tab_name"`
}

// nextTabName picks the smallest positive integer not among names. Only names
// that are exactly a positive integer count; a tab someone renamed to "logs" or
// "3b" neither reserves nor unlocks a number.
func nextTabName(names []string) string {
	used := make(map[int]bool, len(names))
	for _, name := range names {
		n, err := strconv.Atoi(strings.TrimSpace(name))
		if err == nil && n > 0 {
			used[n] = true
		}
	}
	next := 1
	for used[next] {
		next++
	}
	return strconv.Itoa(next)
}

// GET /api/workspaces lists the live workspaces on one host for the new
// terminal picker. IDs target an exact live workspace; labels are display and
// persistence values because workspace ids do not survive a new session.
func serveWorkspaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	b, err := reqBackend(r, r.URL.Query().Get("host"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	res, err := b.LuvusCall("workspace.list", map[string]any{})
	if err != nil {
		http.Error(w, fmt.Sprintf("workspace.list: %v", err), http.StatusBadGateway)
		return
	}
	var payload struct {
		Workspaces []struct {
			WorkspaceID string      `json:"workspace_id"`
			Name        string      `json:"name"`
			Workspace   json.Number `json:"workspace"`
			Tabs        int         `json:"tabs"`
			Active      bool        `json:"active"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(res, &payload); err != nil {
		http.Error(w, fmt.Sprintf("workspace.list response: %v", err), http.StatusBadGateway)
		return
	}
	// One snapshot answers the tab names of every workspace; tab.list only
	// covers the focused one.
	tabNames := map[string]map[string]string{} // workspace id -> tab id -> name
	if panes, err := runtimePanes(b); err == nil {
		for _, p := range panes {
			if tabNames[p.WorkspaceID] == nil {
				tabNames[p.WorkspaceID] = map[string]string{}
			}
			tabNames[p.WorkspaceID][p.TabID] = p.TabLabel
		}
	}
	out := make([]terminalWorkspace, 0, len(payload.Workspaces))
	for _, ws := range payload.Workspaces {
		n, _ := ws.Workspace.Int64()
		names := make([]string, 0, len(tabNames[ws.WorkspaceID]))
		for _, name := range tabNames[ws.WorkspaceID] {
			names = append(names, name)
		}
		out = append(out, terminalWorkspace{
			WorkspaceID: ws.WorkspaceID,
			Label:       ws.Name,
			Number:      int(n),
			TabCount:    ws.Tabs,
			Focused:     ws.Active,
			NextTabName: nextTabName(names),
		})
	}
	writeJSON(w, map[string]any{"workspaces": out})
}

func serveCreateTerminal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req createTerminalReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	command := req.Command
	if strings.TrimSpace(command) == "" {
		command = ""
	}
	// A multi-line command is still refused: pane.run submits a multi-line
	// command one line at a time, so the tail would execute as separate broken
	// commands rather than as the thing the user asked for.
	if strings.ContainsAny(command, "\r\n") {
		http.Error(w, "command must be a single line", http.StatusBadRequest)
		return
	}
	if len(command) > maxTypedLaunch {
		http.Error(w, fmt.Sprintf("command is too long (maximum %d bytes)", maxTypedLaunch), http.StatusBadRequest)
		return
	}

	b, err := reqBackend(r, req.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	focus := req.Focus == nil || *req.Focus
	workspaceName := strings.TrimSpace(req.WorkspaceName)
	if workspaceName == "" {
		workspaceName = "~"
	}
	created, err := createTerminal(b, terminalPlan{
		workspaceID:   strings.TrimSpace(req.WorkspaceID),
		workspaceName: workspaceName,
		tabName:       strings.TrimSpace(req.TabName),
		focus:         focus,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out := createTerminalResp{
		WorkspaceID:  created.workspaceID,
		TabID:        created.tabID,
		RootPane:     created.paneID,
		TabNameError: created.tabNameErr,
	}
	// Outside the mutation lock: this polls the pane's screen for seconds, and
	// nothing else in the sequence depends on focus any more.
	if command != "" && created.paneID != "" {
		waitPaneReady(b, created.paneID)
		if err := paneRun(b, created.paneID, command); err != nil {
			out.CommandError = fmt.Sprintf("submit command: %v", err)
		}
	}
	writeJSON(w, out)
}

// terminalPlan is what the caller asked for, resolved of its defaults.
type terminalPlan struct {
	workspaceID   string // "" creates a new workspace rooted at the home dir
	workspaceName string // label for a new workspace, and the fallback lookup key for an existing one
	tabName       string // "" leaves the tab unnamed
	focus         bool
}

// createdTerminal is what actually came up.
type createdTerminal struct {
	workspaceID string
	tabID       string
	paneID      string
	// tabNameErr records a failed rename. Naming is cosmetic and the terminal
	// exists either way, so it is reported beside the result rather than as the
	// call's error — a client retrying on it would create a second terminal.
	tabNameErr string
}

func createTerminal(b Backend, plan terminalPlan) (createdTerminal, error) {
	unlock := runtimeMutationLock(b)
	defer unlock()

	var prevFocus string
	if !plan.focus {
		prevFocus = focusedPaneID(b)
	}
	defer func() {
		if prevFocus != "" {
			_, _ = b.LuvusCall("pane.focus", map[string]any{"pane": prevFocus})
		}
	}()

	var out createdTerminal
	var err error
	if plan.workspaceID == "" {
		out, err = newTerminalWorkspace(b, plan)
	} else {
		out, err = newTerminalTab(b, plan.workspaceID, plan)
		// Workspace cleanup can race the picker. Resolve the persisted label
		// again first (another workspace with that label may still exist), then
		// open a fresh workspace only when no live match remains.
		if isNotFound(err) {
			if id := terminalWorkspaceIDByLabel(b, plan.workspaceName); id != "" {
				out, err = newTerminalTab(b, id, plan)
			}
			if isNotFound(err) {
				out, err = newTerminalWorkspace(b, plan)
			}
		}
	}
	if err != nil {
		return createdTerminal{}, err
	}
	return out, nil
}

// UHP workspace.open may reuse both a workspace and its existing pane. Atomic
// terminal creation guarantees a new PTY even when that directory is already
// open. Luvus owns directory grouping; an existing workspace keeps its name.
func newTerminalWorkspace(b Backend, plan terminalPlan) (createdTerminal, error) {
	home := expandTildeOn(b, "~")
	before, err := runtimeWorkspaces(b)
	if err != nil {
		return createdTerminal{}, err
	}
	res, err := b.LuvusCall("terminal.backend.create", map[string]any{
		"cwd":       home,
		"placement": map[string]any{"kind": "workspace"},
		"focus":     true,
		"label":     workspaceName(plan.workspaceName),
	})
	if err != nil {
		return createdTerminal{}, fmt.Errorf("terminal.backend.create: %w", err)
	}
	var made struct {
		PaneID     string `json:"pane_id"`
		TerminalID string `json:"terminal_id"`
	}
	if err := json.Unmarshal(res, &made); err != nil || made.PaneID == "" || made.TerminalID == "" {
		return createdTerminal{}, fmt.Errorf("terminal.backend.create returned no terminal identity: %s", res)
	}
	res, err = b.LuvusCall("pane.get", map[string]any{"pane": made.PaneID})
	if err != nil {
		return createdTerminal{}, err
	}
	var target struct {
		WorkspaceID string `json:"workspace_id"`
		TabID       string `json:"tab_id"`
		TerminalID  string `json:"terminal_id"`
	}
	if err := json.Unmarshal(res, &target); err != nil || target.TerminalID != made.TerminalID || target.WorkspaceID == "" || target.TabID == "" {
		return createdTerminal{}, fmt.Errorf("created terminal identity changed before it could be resolved")
	}
	existed := false
	for _, ws := range before {
		if ws.WorkspaceID == target.WorkspaceID {
			existed = true
			break
		}
	}
	if !existed {
		if name := workspaceName(plan.workspaceName); name != "" {
			if _, err := b.LuvusCall("workspace.rename", map[string]any{"workspace_id": target.WorkspaceID, "name": name}); err != nil {
				return createdTerminal{workspaceID: target.WorkspaceID, tabID: target.TabID, paneID: made.PaneID, tabNameErr: err.Error()}, nil
			}
		}
	}
	out := createdTerminal{workspaceID: target.WorkspaceID, tabID: target.TabID, paneID: made.PaneID}
	out.tabNameErr = renameTerminalTab(b, out.tabID, plan.tabName)
	return out, nil
}

// newTerminalTab adds a tab to an existing workspace. tab.new only ever acts on
// the ACTIVE workspace, so the workspace is focused first; the reply's 1-based
// position is then matched against the topology to find the tab's stable id and
// its pane.
func newTerminalTab(b Backend, workspaceID string, plan terminalPlan) (createdTerminal, error) {
	if _, err := b.LuvusCall("workspace.focus", map[string]any{"workspace_id": workspaceID}); err != nil {
		return createdTerminal{}, fmt.Errorf("workspace.focus: %w", err)
	}
	res, err := b.LuvusCall("tab.new", map[string]any{})
	if err != nil {
		return createdTerminal{}, fmt.Errorf("tab.new: %w", err)
	}
	var r struct {
		Tab json.Number `json:"tab"`
	}
	if json.Unmarshal(res, &r) != nil || r.Tab == "" {
		return createdTerminal{}, fmt.Errorf("tab.new answered without a tab position: %s", res)
	}
	pos, err := r.Tab.Int64()
	if err != nil {
		return createdTerminal{}, fmt.Errorf("tab.new answered with tab %q: %w", r.Tab, err)
	}
	out := createdTerminal{workspaceID: workspaceID}
	panes, err := runtimePanes(b)
	if err != nil {
		return createdTerminal{}, fmt.Errorf("resolve the new tab's pane: %w", err)
	}
	for _, p := range panes {
		if p.WorkspaceID != workspaceID || p.TabNumber != int(pos) {
			continue
		}
		out.tabID = p.TabID
		if out.paneID == "" || p.Focused {
			out.paneID = p.PaneID
		}
	}
	if out.paneID == "" {
		return createdTerminal{}, fmt.Errorf("tab.new reported tab %d in workspace %s, which holds no pane", pos, workspaceID)
	}
	out.tabNameErr = renameTerminalTab(b, out.tabID, plan.tabName)
	return out, nil
}

// renameTerminalTab names a tab, returning a message when it could not be done.
// tab.rename accepts a tab_id and routes correctly within the FOCUSED workspace
// — which is the one this handler just created the tab in — and caps the name at
// workspaceNameMaxLen.
func renameTerminalTab(b Backend, tabID, name string) string {
	name = workspaceName(name)
	if name == "" || tabID == "" {
		return ""
	}
	if _, err := b.LuvusCall("tab.rename", map[string]any{
		"tab_id": tabID,
		"name":   name,
	}); err != nil {
		return fmt.Sprintf("name tab: %v", err)
	}
	return ""
}

// isNotFound reports whether err is the runtime's "that object is gone"
// refusal, which is the one failure the picker recovers from by re-resolving.
func isNotFound(err error) bool {
	var he *luvusError
	return errors.As(err, &he) && he.Code == "not_found"
}

func terminalWorkspaceIDByLabel(b Backend, label string) string {
	wss, err := runtimeWorkspaces(b)
	if err != nil {
		return ""
	}
	for _, ws := range wss {
		if ws.Label == label {
			return ws.WorkspaceID
		}
	}
	return ""
}
