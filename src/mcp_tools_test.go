package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// targetRecords are the host's lasso-created agents.
func targetRecords() []AgentRecord {
	return []AgentRecord{
		{ID: "a1", Title: "clem", RootPane: "w1-1"},
		{ID: "a2", Title: "builder", RootPane: "w2-1"},
	}
}

// targetPanes are the host's live herdr panes: two lasso-owned (w1-1, w2-1) and
// two foreign sessions lasso never created (a bot "Clem (OCAI)" and "Ticket
// 500"), plus a bare shell that carries no agent.
func targetPanes() []hostPane {
	return []hostPane{
		{PaneID: "w1-1", WorkspaceID: "w1", WorkspaceLabel: "clem", Agent: "claude", AgentStatus: "idle", HasAgent: true},
		{PaneID: "w2-1", WorkspaceID: "w2", WorkspaceLabel: "builder", Agent: "codex", AgentStatus: "working", HasAgent: true},
		{PaneID: "w9-1", WorkspaceID: "w9", WorkspaceLabel: "Clem (OCAI)", Agent: "claude", AgentStatus: "working", HasAgent: true},
		{PaneID: "w8-1", WorkspaceID: "w8", WorkspaceLabel: "Ticket 500", Agent: "claude", AgentStatus: "idle", HasAgent: true},
		{PaneID: "w7-1", WorkspaceID: "w7", WorkspaceLabel: "scratch", HasAgent: false},
	}
}

func TestResolveTargetByID(t *testing.T) {
	// Exact lasso id resolves to its record (the pre-existing id-based path).
	got, err := resolveTarget("local", "a1", targetRecords(), targetPanes())
	if err != nil || got.Record == nil || got.Record.ID != "a1" || got.PaneID != "w1-1" {
		t.Fatalf("a1 -> %+v (err %v), want lasso a1 on w1-1", got, err)
	}
	if got.Pane != nil {
		t.Errorf("id match should not carry a foreign pane: %+v", got.Pane)
	}
}

func TestResolveTargetByPaneID(t *testing.T) {
	// A lasso-owned pane id maps back to its record...
	got, err := resolveTarget("local", "w2-1", targetRecords(), targetPanes())
	if err != nil || got.Record == nil || got.Record.ID != "a2" {
		t.Fatalf("w2-1 -> %+v (err %v), want lasso a2", got, err)
	}
	// ...a foreign pane id resolves to the session, with no lasso record.
	got, err = resolveTarget("local", "w9-1", targetRecords(), targetPanes())
	if err != nil || got.Record != nil || got.Pane == nil || got.PaneID != "w9-1" {
		t.Fatalf("w9-1 -> %+v (err %v), want foreign pane w9-1", got, err)
	}
}

func TestResolveTargetByName(t *testing.T) {
	// A lasso agent's title resolves to its record, case-insensitively.
	got, err := resolveTarget("local", "BUILDER", targetRecords(), targetPanes())
	if err != nil || got.Record == nil || got.Record.ID != "a2" {
		t.Fatalf("BUILDER -> %+v (err %v), want lasso a2", got, err)
	}
	// A foreign session's sidebar name resolves to its pane — this is the clem
	// bug: "Clem (OCAI)" is a herdr session lasso did not create.
	got, err = resolveTarget("local", "clem (ocai)", targetRecords(), targetPanes())
	if err != nil || got.Record != nil || got.Pane == nil || got.PaneID != "w9-1" {
		t.Fatalf("clem (ocai) -> %+v (err %v), want foreign pane w9-1", got, err)
	}
}

func TestResolveTargetNameAmbiguous(t *testing.T) {
	// A foreign session sharing a lasso agent's name makes "clem" ambiguous: it
	// must be refused with BOTH candidates listed, never guessed.
	panes := append(targetPanes(), hostPane{PaneID: "w5-1", WorkspaceLabel: "clem", Agent: "claude", AgentStatus: "idle", HasAgent: true})
	_, err := resolveTarget("local", "clem", targetRecords(), panes)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("clem ambiguity: err = %v, want an ambiguous-match error", err)
	}
	if !strings.Contains(err.Error(), "a1@local") || !strings.Contains(err.Error(), "w5-1") {
		t.Fatalf("ambiguity error should list both candidates: %v", err)
	}
}

func TestResolveTargetNotFoundAndBareShell(t *testing.T) {
	if _, err := resolveTarget("local", "nobody", targetRecords(), targetPanes()); err == nil {
		t.Error("unknown target resolved, want error")
	}
	// A bare shell (no agent) is not addressable by its sidebar name.
	if _, err := resolveTarget("local", "scratch", targetRecords(), targetPanes()); err == nil {
		t.Error("bare-shell name resolved, want error")
	}
	if _, err := resolveTarget("local", "  ", targetRecords(), targetPanes()); err == nil {
		t.Error("blank needle resolved, want error")
	}
}

func TestSidebarNameFallback(t *testing.T) {
	// Workspace label wins; then the pane's own label; then the tab label.
	if got := sidebarName(hostPane{WorkspaceLabel: "ws", PaneLabel: "pn", TabLabel: "tb"}); got != "ws" {
		t.Errorf("sidebarName = %q, want ws", got)
	}
	if got := sidebarName(hostPane{PaneLabel: "pn", TabLabel: "tb"}); got != "pn" {
		t.Errorf("sidebarName = %q, want pn", got)
	}
	if got := sidebarName(hostPane{TabLabel: "tb"}); got != "tb" {
		t.Errorf("sidebarName = %q, want tb", got)
	}
	// Nothing labelled anywhere: the terminal title is the only name left.
	if got := sidebarName(hostPane{TerminalTitle: "Check Norm outline wiki connection"}); got != "Check Norm outline wiki connection" {
		t.Errorf("sidebarName = %q, want the terminal title", got)
	}
	if got := sidebarName(hostPane{}); got != "" {
		t.Errorf("sidebarName = %q, want empty", got)
	}
}

func TestAgentInfoLassoCreatedFlag(t *testing.T) {
	// A lasso record is flagged created; a foreign pane is not, and carries its
	// sidebar name as both the address and the human title.
	if ai := agentInfoFrom("local", AgentRecord{ID: "a1", Title: "clem"}, "idle"); !ai.LassoCreated {
		t.Error("agentInfoFrom should set lasso_created=true")
	}
	ai := agentInfoFromPane("local", hostPane{PaneID: "w9-1", WorkspaceLabel: "Clem (OCAI)", Agent: "claude", AgentStatus: "working"})
	if ai.LassoCreated {
		t.Error("agentInfoFromPane should set lasso_created=false")
	}
	if ai.SidebarName != "Clem (OCAI)" || ai.Title != "Clem (OCAI)" || ai.RootPane != "w9-1" || ai.ID != "" {
		t.Errorf("foreign pane info = %+v, want name/pane populated and no lasso id", ai)
	}
	// With a terminal title the session keeps its sidebar name as the address but
	// reports what it is actually working on as the title.
	ai = agentInfoFromPane("norm", hostPane{
		PaneID: "w3:p1", WorkspaceLabel: "norm", TerminalTitle: "Check Norm outline wiki connection",
		Agent: "claude", AgentStatus: "idle",
	})
	if ai.SidebarName != "norm" || ai.Title != "Check Norm outline wiki connection" {
		t.Errorf("foreign pane info = %q/%q, want sidebar name norm and the terminal title", ai.SidebarName, ai.Title)
	}
}

func TestSendAgentRefusesWhenComposerHasDraft(t *testing.T) {
	openTestDB(t)
	rec := AgentRecord{ID: "a1", Title: "clem", Agent: "claude", RootPane: "w1-1", CreatedAt: time.Now()}
	if err := appendAgent("local", rec); err != nil {
		t.Fatal(err)
	}
	b := &msgPaneBackend{memBackend: newMemBackend(), paneID: "w1-1", agent: "claude", status: "idle", drafted: true}
	prevResolver := resolveBackend
	resolveBackend = func(string) (Backend, error) { return b, nil }
	t.Cleanup(func() { resolveBackend = prevResolver })
	if got := paneComposerState(b, "w1-1", "claude"); got != ComposerDraft {
		t.Fatalf("drafted fake state = %v, want draft", got)
	}
	target, targetBackend, targetErr := resolveAgentTarget("local", "a1")
	if targetErr != nil || targetBackend != b || target.agentKind() != "claude" {
		t.Fatalf("resolved target = %+v / %T / %v, want claude target on fake backend", target, targetBackend, targetErr)
	}

	_, out, err := sendAgentTool(nil, nil, sendAgentIn{AgentID: "a1", Text: "must not send"})
	if err == nil {
		t.Fatal("send_agent accepted a pane with unsent input")
	}
	if out.Sent {
		t.Fatalf("send_agent result = %+v, want sent:false", out)
	}
	if !strings.Contains(err.Error(), "NOT delivered") || !strings.Contains(err.Error(), "message_agent") {
		t.Fatalf("refusal = %q, want actionable queued-delivery guidance", err)
	}
	if len(b.sent) != 0 {
		t.Fatalf("send_agent wrote to a drafted pane: %q", b.sent)
	}
}

// ---------------------------------------------------------------------------
// create_agent: MCP input -> launch line
// ---------------------------------------------------------------------------

// launchFake records the command lines herdr is asked to type into a pane, so a
// test can assert on the launch line an agent's boot actually produced.
type launchFake struct {
	*memBackend
	mu sync.Mutex
	// cli is the token that marks the launch line among everything typed into
	// the pane (setup script, trust confirmations). Empty means "claude ", the
	// harness most of these tests use.
	cli      string
	sent     []string
	launched chan struct{} // closed once a line invoking the agent CLI lands
	once     sync.Once
}

func (b *launchFake) cliToken() string {
	if b.cli == "" {
		return "claude "
	}
	return b.cli
}

func (b *launchFake) HerdrCall(method string, params any) (json.RawMessage, error) {
	switch method {
	case "worktree.create", "workspace.create":
		return json.RawMessage(`{"workspace":{"workspace_id":"ws"},"root_pane":{"pane_id":"p1"}}`), nil
	case "pane.read":
		// Stable non-empty text so waitPaneReady settles on the first two polls.
		return json.RawMessage(`{"read":{"text":"$ "}}`), nil
	case "pane.send_text":
		p, _ := params.(map[string]any)
		text, _ := p["text"].(string)
		b.mu.Lock()
		b.sent = append(b.sent, text)
		b.mu.Unlock()
		if strings.Contains(text, b.cliToken()) {
			b.once.Do(func() { close(b.launched) })
		}
	}
	return json.RawMessage(`{}`), nil
}

func (b *launchFake) GitOut(string, ...string) (string, error) { return "", nil }

// launchLine returns the recorded line that invokes the agent CLI.
func (b *launchFake) launchLine() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.sent {
		if strings.Contains(s, b.cliToken()) {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// TestMCPCreateAgentEffortReachesLaunchCommand walks the whole MCP create path
// for the field that silently went missing — the tool's input struct, the
// mapping onto createAgentReq, normalizeEffort's harness check, and the command
// herdr is asked to type — and proves effort:"xhigh" comes out as
// `--effort xhigh` on the launch line. The parity tests in create_params_test.go
// prove no field can go undeclared; this proves this one actually arrives.
//
// Only host resolution is skipped (createAgentTool's resolveBackend hands
// createAgent a real herdr connection; the fake stands in for it).
func TestMCPCreateAgentEffortReachesLaunchCommand(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)

	b := &launchFake{memBackend: newMemBackend(), launched: make(chan struct{})}
	prev := curBackend()
	setBackend(b)
	t.Cleanup(func() { setBackend(prev) })

	in := createAgentIn{
		Type:   "scratch",
		Title:  "effort probe",
		Prompt: "say hi",
		Agent:  "claude",
		Effort: "xhigh",
	}
	if _, err := createAgent(b, in.toCreateReq()); err != nil {
		t.Fatalf("createAgent: %v", err)
	}
	select {
	case <-b.launched:
	case <-time.After(20 * time.Second):
		t.Fatalf("agent CLI was never launched; herdr saw %q", b.sent)
	}
	// The value rides the line shell-quoted, so build the expectation with the
	// same quoter rather than hard-coding today's quoting.
	want := "--effort " + shellQuote("xhigh")
	if line := b.launchLine(); !strings.Contains(line, want) {
		t.Errorf("launch line does not carry the requested effort:\n  %s\nwant it to contain %s", line, want)
	}
}

// An effort level the chosen harness doesn't list must be DROPPED, not passed
// through: an unknown --effort makes the CLI exit at launch, which would fail
// the whole boot (see normalizeEffort). "max" is a claude level; codex has no
// such level, so a codex agent asking for it launches with no effort flag.
func TestMCPCreateAgentDropsEffortTheHarnessDoesNotKnow(t *testing.T) {
	if got := normalizeEffort("codex", "max"); got != "" {
		t.Errorf("normalizeEffort(codex, max) = %q, want \"\" — an unknown level must be dropped, not forwarded", got)
	}
	if got := normalizeEffort("claude", "xhigh"); got != "xhigh" {
		t.Errorf("normalizeEffort(claude, xhigh) = %q, want xhigh", got)
	}
}

// Plan mode was withheld from MCP from the day after the tool shipped, on the
// presumption that a spawned agent would strand itself at an approval gate
// nobody watches. It doesn't — the agent answers normally and only blocks when
// it wants to execute — so the parameter is exposed. It still has to actually
// reach the launch line.
func TestMCPCreateAgentPlanModeReachesLaunchCommand(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)

	b := &launchFake{memBackend: newMemBackend(), launched: make(chan struct{})}
	prev := curBackend()
	setBackend(b)
	t.Cleanup(func() { setBackend(prev) })

	in := createAgentIn{Type: "scratch", Title: "planner", Prompt: "plan it", Agent: "claude", PlanMode: true}
	rec, err := createAgent(b, in.toCreateReq())
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}
	if !rec.PlanMode {
		t.Error("plan_mode:true did not reach the persisted record")
	}
	select {
	case <-b.launched:
	case <-time.After(20 * time.Second):
		t.Fatalf("agent CLI was never launched; herdr saw %q", b.sent)
	}
	line := b.launchLine()
	if !strings.Contains(line, "--permission-mode plan") {
		t.Errorf("launch line is not in plan mode:\n  %s", line)
	}
	// The allow-variant belongs on every line — it makes bypass available
	// without selecting it, so plan mode survives (see claudeCommand). The
	// forcing variant would silently override plan mode; match it with its
	// leading space so the allow flag isn't mistaken for it.
	if !strings.Contains(line, "--allow-dangerously-skip-permissions") {
		t.Errorf("launch line missing the allow-bypass flag:\n  %s", line)
	}
	if strings.Contains(line, " --dangerously-skip-permissions") {
		t.Errorf("plan launch forces bypass mode, which overrides plan:\n  %s", line)
	}
}

// The same for omp, whose plan mode is not a flag at all: an MCP plan request
// has to survive normalizePlanMode, get lasso's overlay staged onto the host,
// and come out the far end as a --config on the typed launch line. This is the
// whole path — anything short of it records planning that never happened.
func TestMCPCreateAgentPlanModeReachesOmpLaunchCommand(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)

	b := &launchFake{memBackend: newMemBackend(), cli: "omp ", launched: make(chan struct{})}
	prev := curBackend()
	setBackend(b)
	t.Cleanup(func() { setBackend(prev) })

	in := createAgentIn{Type: "scratch", Title: "omp planner", Prompt: "plan it", Agent: "omp", PlanMode: true}
	rec, err := createAgent(b, in.toCreateReq())
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}
	if !rec.PlanMode {
		t.Fatal("plan_mode:true did not reach the persisted record for omp")
	}
	select {
	case <-b.launched:
	case <-time.After(20 * time.Second):
		t.Fatalf("agent CLI was never launched; herdr saw %q", b.sent)
	}
	line := b.launchLine()
	overlay := ompConfigPath(b, rec.ID)
	if !strings.Contains(line, "--config '"+overlay+"'") {
		t.Errorf("omp launch line is not in plan mode:\n  %s", line)
	}
	if got := b.files[overlay]; !strings.Contains(got, string(ompPlanOverlay)) {
		t.Errorf("plan overlay was not staged onto the host: %q", got)
	}
	// The overlay is lasso's own file under the lasso dir. Writing the user's
	// own omp config instead would outlive the run and change every later omp
	// session on the box.
	if strings.Contains(overlay, ".omp") {
		t.Errorf("plan overlay must not live in the user's omp config dir: %s", overlay)
	}
}
