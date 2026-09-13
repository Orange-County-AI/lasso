package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// POST /api/create-terminal creates a bare Herdr terminal, either as a new tab
// in an existing workspace or as the root tab of a new workspace. A non-empty
// command is typed into the shell only after it has settled, so shell startup
// cannot eat the leading bytes. It may span several lines — those run as one
// script (see terminalScript), not one command per line.
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

type terminalWorkspace struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
	TabCount    int    `json:"tab_count"`
	Focused     bool   `json:"focused"`
}

// GET /api/workspaces lists the live workspaces on one Herdr host for the new
// terminal picker. IDs target an exact live workspace; labels are display and
// persistence values because Herdr IDs do not survive a new session.
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
	res, err := b.HerdrCall("workspace.list", map[string]any{})
	if err != nil {
		http.Error(w, fmt.Sprintf("workspace.list: %v", err), http.StatusBadGateway)
		return
	}
	var payload struct {
		Workspaces []terminalWorkspace `json:"workspaces"`
	}
	if err := json.Unmarshal(res, &payload); err != nil {
		http.Error(w, fmt.Sprintf("workspace.list response: %v", err), http.StatusBadGateway)
		return
	}
	if payload.Workspaces == nil {
		payload.Workspaces = []terminalWorkspace{}
	}
	writeJSON(w, payload)
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
	command := normalizeTerminalCommand(req.Command)
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
	workspaceID := strings.TrimSpace(req.WorkspaceID)
	workspaceName := strings.TrimSpace(req.WorkspaceName)
	if workspaceName == "" {
		workspaceName = "~"
	}
	tabName := strings.TrimSpace(req.TabName)
	createWorkspace := func() (json.RawMessage, error) {
		return b.HerdrCall("workspace.create", map[string]any{
			"cwd":   expandTildeOn(b, "~"),
			"label": workspaceName,
			"focus": focus,
		})
	}

	var res json.RawMessage
	var method string
	if workspaceID == "" {
		method = "workspace.create"
		res, err = createWorkspace()
	} else {
		method = "tab.create"
		params := map[string]any{
			"workspace_id": workspaceID,
			"focus":        focus,
		}
		if tabName != "" {
			params["label"] = tabName
		}
		res, err = b.HerdrCall(method, params)
		// Workspace cleanup can race the picker. Resolve the persisted label
		// again first (another workspace with that label may still exist), then
		// create it only when no live match remains.
		if err != nil && strings.Contains(err.Error(), "workspace_not_found") {
			workspaceID = terminalWorkspaceIDByLabel(b, workspaceName)
			if workspaceID != "" {
				params := map[string]any{
					"workspace_id": workspaceID,
					"focus":        focus,
				}
				if tabName != "" {
					params["label"] = tabName
				}
				res, err = b.HerdrCall("tab.create", params)
			}
			if workspaceID == "" || (err != nil && strings.Contains(err.Error(), "workspace_not_found")) {
				workspaceID = ""
				method = "workspace.create"
				res, err = createWorkspace()
			}
		}
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("%s: %v", method, err), http.StatusBadGateway)
		return
	}

	createdWorkspace, tabID, paneID := parseTerminalCreateResult(res)
	if workspaceID == "" {
		workspaceID = createdWorkspace
	}
	out := createTerminalResp{
		WorkspaceID: workspaceID,
		TabID:       tabID,

		RootPane: paneID,
	}
	if method == "workspace.create" && tabName != "" && tabID != "" {
		if _, err := b.HerdrCall("tab.rename", map[string]any{
			"tab_id": tabID,
			"label":  tabName,
		}); err != nil {
			out.TabNameError = fmt.Sprintf("name tab: %v", err)
		}
	}
	if command != "" {
		waitPaneReady(b, paneID)
		if err := paneRun(b, paneID, terminalScript(command)); err != nil {
			out.CommandError = fmt.Sprintf("submit command: %v", err)
		}
	}
	writeJSON(w, out)
}

// normalizeTerminalCommand turns the New-terminal dialog's text into the script
// typed at the new shell (see terminalScript for how a multi-line one is
// delivered). Line breaks are kept: they are the script's own.
//
// A CRLF is folded to LF first. The browser sends "\n", but a paste from another
// platform can carry a CR — and at a cooked-mode PTY a CR is an accept-line of
// its own, which inside a heredoc body would both submit early and leave the CR
// in the script. Trailing newlines are dropped so a block ending on a blank line
// does not add an empty command; interior ones are the user's. Blank input
// reduces to "" — the "open a bare interactive shell" the empty field means.
func normalizeTerminalCommand(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimRight(s, "\n")
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}

// terminalScript is what is actually typed at the new shell for a command: a
// multi-line one is wrapped in a heredoc sourced by the shell already in the
// pane, so the whole block is ONE command — parsed as a script before any of it
// runs, so a heredoc, an if/then/fi or a quoted string inside it means what it
// means, and the terminal shows one submission rather than a prompt per line.
//
// Sourcing it rather than handing the body to a child shell keeps the pane's own
// state: a "cd" in the block leaves the terminal there, and the pane's aliases,
// functions and exported vars apply. "." is POSIX — checked on bash, dash, sh
// and macOS zsh — and /dev/stdin is the heredoc's own body. The delimiter is
// quoted so the body reaches the script verbatim, its expansions happening when
// it runs rather than while it is read.
//
// A single-line command is typed as itself. It is already one command, and
// wrapping it would put three lines in the shell's history for one.
func terminalScript(command string) string {
	if !strings.Contains(command, "\n") {
		return command
	}
	delim := scriptDelimiter(command)
	return ". /dev/stdin <<'" + delim + "'\n" + command + "\n" + delim
}

// scriptDelimiter picks a heredoc delimiter the script cannot contain: a line
// matching it ends the body early, and the rest of the script then reaches the
// shell as typed input. Underscores are appended until no line matches.
func scriptDelimiter(script string) string {
	delim := "LASSO_EOF"
	for {
		clash := false
		for _, line := range strings.Split(script, "\n") {
			if line == delim {
				clash = true
				break
			}
		}
		if !clash {
			return delim
		}
		delim += "_"
	}
}

func terminalWorkspaceIDByLabel(b Backend, label string) string {
	res, err := b.HerdrCall("workspace.list", map[string]any{})
	if err != nil {
		return ""
	}
	var payload struct {
		Workspaces []terminalWorkspace `json:"workspaces"`
	}
	if json.Unmarshal(res, &payload) != nil {
		return ""
	}
	for _, workspace := range payload.Workspaces {
		if workspace.Label == label {
			return workspace.WorkspaceID
		}
	}
	return ""
}

func parseTerminalCreateResult(res json.RawMessage) (workspaceID, tabID, rootPane string) {
	var r struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		Tab struct {
			WorkspaceID string `json:"workspace_id"`
			TabID       string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	_ = json.Unmarshal(res, &r)
	workspaceID = r.Workspace.WorkspaceID
	if workspaceID == "" {
		workspaceID = r.Tab.WorkspaceID
	}
	return workspaceID, r.Tab.TabID, r.RootPane.PaneID
}
