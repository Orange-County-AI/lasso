package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
)

var runtimeMutationLocks sync.Map

// UHP workspace.open/tab.new operate on session focus. Serialize Lasso's
// multi-call operations on each host without blocking unrelated machines.
func runtimeMutationLock(be Backend) func() {
	name := be.Name()
	value, ok := runtimeMutationLocks.Load(name)
	if !ok {
		value, _ = runtimeMutationLocks.LoadOrStore(name, &sync.Mutex{})
	}
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func renameRuntimeTab(be Backend, paneID, name string) (err error) {
	unlock := runtimeMutationLock(be)
	defer unlock()
	data, err := be.LuvusCall("pane.current", map[string]any{})
	if err != nil {
		return err
	}
	var current struct {
		Pane string `json:"pane"`
	}
	if err := json.Unmarshal(data, &current); err != nil {
		return err
	}
	data, err = be.LuvusCall("pane.get", map[string]any{"pane": paneID})
	if err != nil {
		return err
	}
	var target struct {
		TabID string `json:"tab_id"`
	}
	if err := json.Unmarshal(data, &target); err != nil {
		return err
	}
	if target.TabID == "" {
		return fmt.Errorf("Luvus pane has no tab identity")
	}
	if _, err = be.LuvusCall("pane.focus", map[string]any{"pane": paneID}); err != nil {
		return err
	}
	defer func() {
		if current.Pane != "" && current.Pane != paneID {
			if _, restoreErr := be.LuvusCall("pane.focus", map[string]any{"pane": current.Pane}); restoreErr != nil && err == nil {
				err = fmt.Errorf("tab renamed but focus restore failed: %w", restoreErr)
			}
		}
	}()
	_, err = be.LuvusCall("tab.rename", map[string]any{"tab_id": target.TabID, "name": name})
	return err
}

// runtimeWorkspaces projects UHP's stable identities into Lasso's host-local
// workspace model. Display positions are never used as persistent identities.
func runtimeWorkspaces(be Backend) ([]workspace, error) {
	data, err := be.LuvusCall("workspace.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var result struct {
		Workspaces []struct {
			ID       string `json:"workspace_id"`
			Name     string `json:"name"`
			Index    string `json:"workspace"`
			Position string `json:"display_position"`
			Cwd      string `json:"cwd"`
			Active   bool   `json:"active"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	out := make([]workspace, 0, len(result.Workspaces))
	for _, w := range result.Workspaces {
		if w.ID == "" {
			return nil, fmt.Errorf("Luvus workspace lacks stable identity")
		}
		n, err := strconv.Atoi(w.Index)
		if err != nil {
			return nil, fmt.Errorf("Luvus workspace index: %w", err)
		}
		position, err := strconv.Atoi(w.Position)
		if err != nil {
			return nil, fmt.Errorf("Luvus workspace position: %w", err)
		}
		out = append(out, workspace{WorkspaceID: w.ID, Label: w.Name, Number: n, DisplayPosition: position, Focused: w.Active, Cwd: w.Cwd})
	}
	return out, nil
}

// runtimePanes combines an atomic session snapshot (including native agent
// sessions) with pane.get's stable topology. Snapshot positions cannot be joined
// to workspace.list: their index bases differ and reorder can race either read.
// A vanished or replaced PTY invalidates the enumeration instead of exposing a
// partial success that could tombstone a live agent record.
func runtimePanes(be Backend) ([]pane, error) {
	data, err := be.LuvusCall("session.snapshot", map[string]any{})
	if err != nil {
		return nil, err
	}
	var snapshot struct {
		Workspaces []struct {
			Name  string `json:"name"`
			Index int    `json:"index"`
			Tabs  []struct {
				Name  string `json:"name"`
				Index int    `json:"index"`
				Panes []struct {
					pane
					Authority string `json:"agent_authority"`
					Kind      string `json:"kind"`
				} `json:"panes"`
			} `json:"tabs"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, err
	}
	out := make([]pane, 0)
	for _, ws := range snapshot.Workspaces {
		for _, tab := range ws.Tabs {
			for _, entry := range tab.Panes {
				if entry.Kind != "" && entry.Kind != "terminal" {
					continue
				}
				p := entry.pane
				if p.PaneID == "" || p.TerminalID == "" {
					return nil, fmt.Errorf("Luvus snapshot pane lacks runtime identity")
				}
				data, err := be.LuvusCall("pane.get", map[string]any{"pane": p.PaneID})
				if err != nil {
					return nil, err
				}
				var detail struct {
					Pane        string `json:"pane"`
					TerminalID  string `json:"terminal_id"`
					WorkspaceID string `json:"workspace_id"`
					TabID       string `json:"tab_id"`
					Name        string `json:"name"`
					Cwd         string `json:"cwd"`
					Focused     bool   `json:"focused"`
				}
				if err := json.Unmarshal(data, &detail); err != nil {
					return nil, err
				}
				if detail.Pane != p.PaneID || detail.TerminalID != p.TerminalID || detail.WorkspaceID == "" || detail.TabID == "" {
					return nil, fmt.Errorf("Luvus pane identity changed during enumeration")
				}
				p.WorkspaceID, p.TabID = detail.WorkspaceID, detail.TabID
				p.WorkspaceLabel, p.TabLabel = ws.Name, tab.Name
				p.WorkspaceNumber, p.TabNumber = ws.Index-1, tab.Index
				p.Label, p.Cwd, p.Focused = detail.Name, detail.Cwd, detail.Focused
				// Luvus uses the command name (including ordinary shells) when no
				// detector recognizes an agent. That is not an agent identity.
				if entry.Authority == "command_fallback" {
					p.Agent = ""
				}
				out = append(out, p)
			}
		}
	}
	return out, nil
}
