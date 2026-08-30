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
// cannot eat the leading bytes.
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
	command := req.Command
	if strings.TrimSpace(command) == "" {
		command = ""
	}
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
		if err := paneRun(b, paneID, command); err != nil {
			out.CommandError = fmt.Sprintf("submit command: %v", err)
		}
	}
	writeJSON(w, out)
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
