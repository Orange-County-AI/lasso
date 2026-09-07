package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"testing"
)

// uhpFixtureReply serializes a fake host's domain panes into the observed UHP
// wire contract. Shared by lifecycle tests; it does not assert call sequences.
func uhpFixtureReply(panes []pane, method string, params any) (json.RawMessage, error) {
	p, _ := params.(map[string]any)
	wirePane := func(v pane) pane {
		if v.TerminalID == "" {
			v.TerminalID = "terminal-" + v.PaneID
		}
		if v.WorkspaceID == "" {
			v.WorkspaceID = "workspace-" + v.PaneID
		}
		if v.TabID == "" {
			v.TabID = "tab-" + v.PaneID
		}
		return v
	}
	switch method {
	case "pane.get", "pane.current":
		for _, v := range panes {
			if method == "pane.current" && !v.Focused {
				continue
			}
			if method == "pane.get" && p["pane"] != v.PaneID {
				continue
			}
			v = wirePane(v)
			return json.Marshal(map[string]any{"type": "pane", "pane": v.PaneID, "terminal_id": v.TerminalID, "workspace_id": v.WorkspaceID, "tab_id": v.TabID, "name": v.Label, "cwd": v.Cwd, "focused": v.Focused, "agent": v.Agent, "status": v.AgentStatus, "workspace": strconv.Itoa(v.WorkspaceNumber), "tab": strconv.Itoa(max(1, v.TabNumber))})
		}
		return nil, &luvusError{Code: "not_found", Message: "fixture pane not found"}
	case "session.snapshot":
		var workspaces []any
		for _, v := range panes {
			v = wirePane(v)
			workspaces = append(workspaces, map[string]any{"name": v.WorkspaceLabel, "index": v.WorkspaceNumber + 1, "tabs": []any{map[string]any{"name": v.TabLabel, "index": max(1, v.TabNumber), "panes": []pane{v}}}})
		}
		return json.Marshal(map[string]any{"type": "session_snapshot", "workspaces": workspaces})
	case "workspace.list":
		byID := map[string]pane{}
		for _, v := range panes {
			v = wirePane(v)
			byID[v.WorkspaceID] = v
		}
		ordered := make([]pane, 0, len(byID))
		for _, v := range byID {
			ordered = append(ordered, v)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].WorkspaceNumber < ordered[j].WorkspaceNumber })
		var workspaces []any
		for _, v := range ordered {
			workspaces = append(workspaces, map[string]any{"workspace_id": v.WorkspaceID, "workspace": strconv.Itoa(v.WorkspaceNumber), "display_position": strconv.Itoa(v.WorkspaceNumber), "name": v.WorkspaceLabel, "cwd": v.Cwd, "active": v.Focused})
		}
		return json.Marshal(map[string]any{"type": "workspace_list", "workspaces": workspaces})
	case "workspace.open":
		for _, v := range panes {
			if p["path"] == v.Cwd {
				return json.Marshal(map[string]any{"type": "workspace", "workspace": strconv.Itoa(v.WorkspaceNumber)})
			}
		}
		return nil, fmt.Errorf("fixture workspace path not found: %v", p["path"])
	}
	return json.RawMessage(`{}`), nil
}

func TestRuntimePanesRejectsReplacedTerminal(t *testing.T) {
	b := &replacedTerminalBackend{}
	if _, err := runtimePanes(b); err == nil {
		t.Fatal("mixed snapshots accepted a replacement PTY as the original agent")
	}
}

type replacedTerminalBackend struct{ Backend }

func (*replacedTerminalBackend) LuvusCall(method string, params any) (json.RawMessage, error) {
	v := pane{PaneID: "1", TerminalID: "original", WorkspaceID: "workspace", TabID: "tab"}
	if method == "pane.get" {
		v.TerminalID = "replacement"
	}
	return uhpFixtureReply([]pane{v}, method, params)
}
