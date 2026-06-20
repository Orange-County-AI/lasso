package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// POST /api/create-terminal — create a bare herdr workspace running just a
// shell (no agent) and focus it, so the user can start typing commands right
// away. This mirrors the scratch path of createAgent but deliberately skips the
// agent launch and the AgentRecord bookkeeping: a plain terminal isn't an agent,
// so it carries no prompt, branch, or persisted record — it's just a named
// window onto a shell. The pane opens in the user's home directory (a scratch
// agent gets a dedicated dir because it's scoped to one task; a bare terminal is
// general-purpose, so home is the better default and needs no dir created).
type createTerminalReq struct {
	Label string `json:"label"` // workspace name shown on the herdr tab; defaults to "terminal"
}

type createTerminalResp struct {
	WorkspaceID string `json:"workspace_id"`
	RootPane    string `json:"root_pane"`
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
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "terminal"
	}
	b := curBackend()
	res, err := b.HerdrCall("workspace.create", map[string]any{
		"cwd":   expandTildeOn(b, "~"),
		"label": label,
		"focus": true, // land the user on the new terminal immediately
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("workspace.create: %v", err), http.StatusBadGateway)
		return
	}
	ws, pane := parseCreateResult(res)
	writeJSON(w, createTerminalResp{WorkspaceID: ws, RootPane: pane})
}
