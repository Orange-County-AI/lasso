package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// ompPlanBootFake backs the bootAgent-level tests: pane.read returns the trust
// dialog (stable text, so waitPaneReady settles fast and confirmAgentTrust
// fires instead of polling out its 30s window) and every pane.send_text payload
// is captured, so a test can assert on the launch line that actually got typed.
type ompPlanBootFake struct {
	*memBackend
	mu    sync.Mutex
	sends []string
}

func (b *ompPlanBootFake) HerdrCall(method string, params any) (json.RawMessage, error) {
	switch method {
	case "pane.read":
		return json.RawMessage(`{"read":{"text":"trust this folder"}}`), nil
	case "pane.send_text":
		if p, ok := params.(map[string]any); ok {
			if txt, ok := p["text"].(string); ok {
				b.mu.Lock()
				b.sends = append(b.sends, txt)
				b.mu.Unlock()
			}
		}
	}
	return json.RawMessage(`{}`), nil
}

func (b *ompPlanBootFake) GitOut(string, ...string) (string, error) { return "", nil }

func (b *ompPlanBootFake) launchLine(t *testing.T) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.sends) == 0 {
		t.Fatal("bootAgent never sent the launch command")
	}
	return strings.TrimSuffix(strings.TrimPrefix(b.sends[0], "\x15"), "\n")
}

func bootOmpAgent(t *testing.T, id string, planMode bool) (*ompPlanBootFake, AgentRecord) {
	t.Helper()
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)

	b := &ompPlanBootFake{memBackend: newMemBackend()}
	rec := AgentRecord{
		ID:          id,
		Host:        "local",
		Type:        "scratch",
		Agent:       "omp",
		Title:       "Plan it",
		Description: "plan the thing",
		PlanMode:    planMode,
		WorkDir:     "/work",
		RootPane:    "p1",
	}
	bootAgent(b, "local", rec, "")
	return b, rec
}

// End to end through bootAgent: a plan-mode omp agent must get lasso's own
// overlay written onto the target host and pointed at by --config, carrying BOTH
// halves (theme pin + plan keys), and closing the agent must remove it. The
// user's ~/.omp/agent/config.yml is never touched — the overlay is a separate
// file merged over it for that run only.
func TestBootAgentStagesOmpPlanOverlayAndCloseCleansUp(t *testing.T) {
	b, rec := bootOmpAgent(t, "ompplan1", true)

	path := ompConfigPath(b, rec.ID)
	got := b.files[path]
	if !strings.Contains(got, string(ompPlanOverlay)) {
		t.Errorf("staged overlay is missing the plan keys:\n got: %q\nwant contains: %q", got, string(ompPlanOverlay))
	}
	if !strings.Contains(got, "dark: "+ompThemeName) || !strings.Contains(got, "light: "+ompThemeName) {
		t.Errorf("staged overlay is missing the theme pin: %q", got)
	}
	launch := b.launchLine(t)
	if !strings.Contains(launch, "--config '"+path+"'") {
		t.Errorf("launch line must point --config at the staged overlay: %q", launch)
	}
	if len(launch) > maxTypedLaunch {
		t.Errorf("launch line len = %d, want <= %d: %q", len(launch), maxTypedLaunch, launch)
	}

	// Closing removes it. RootPane is cleared so the fake isn't asked to
	// simulate a kill sequence — the cleanup under test runs regardless.
	closed := rec
	closed.RootPane = ""
	if _, err := closeAgentRecord(b, closed, false, false); err != nil {
		t.Fatalf("closeAgentRecord: %v", err)
	}
	if _, ok := b.files[path]; ok {
		t.Errorf("staged overlay must be removed on close: %s", path)
	}
}

// Without plan mode the overlay still exists — it carries the theme pin, which
// is the only way a launch beats whatever a running omp last wrote into
// config.yml — but it must carry no plan keys, or a normal agent would come up
// planning.
func TestBootAgentStagesOmpThemeOverlayWithoutPlanMode(t *testing.T) {
	b, rec := bootOmpAgent(t, "ompplain1", false)

	path := ompConfigPath(b, rec.ID)
	got, ok := b.files[path]
	if !ok {
		t.Fatalf("a non-plan omp agent must still stage the theme overlay: %s", path)
	}
	if !strings.Contains(got, "dark: "+ompThemeName) || !strings.Contains(got, "light: "+ompThemeName) {
		t.Errorf("staged overlay is missing the theme pin: %q", got)
	}
	if strings.Contains(got, "plan:") {
		t.Errorf("a non-plan omp agent must not stage the plan keys: %q", got)
	}
	if launch := b.launchLine(t); !strings.Contains(launch, "--config '"+path+"'") {
		t.Errorf("non-plan omp launch line must point --config at the overlay: %q", launch)
	}
}

// ompScreenFake answers pane.list with one omp pane at the given herdr status
// and pane.read with the given screen — the two inputs paneAgentStatus combines
// to decide whether omp is parked on its plan gate.
type ompScreenFake struct {
	*memBackend
	agent  string
	status string
	screen string
}

func (b *ompScreenFake) HerdrCall(method string, params any) (json.RawMessage, error) {
	switch method {
	case "pane.list":
		pl := map[string]any{"panes": []map[string]any{{
			"pane_id": "p1", "agent": b.agent, "agent_status": b.status,
		}}}
		raw, _ := json.Marshal(pl)
		return raw, nil
	case "pane.read":
		raw, _ := json.Marshal(map[string]any{"read": map[string]any{"text": b.screen}})
		return raw, nil
	}
	return json.RawMessage(`{}`), nil
}

// The real screen omp draws when it is parked on its plan gate, as captured
// from a live pane (omp 17.3.4).
const ompPlanReviewScreen = `╭─ Plan Review ───────────┬────────────────────────────────────────╮
│ ▎Context                │  hello.py greeting                     │
├─────────────────────────┴────────────────────────────────────────┤
│ Plan mode - next step                                            │
│  Approve and execute                                            │
│   Approve and compact context                                    │
│   Refine plan                                                    │
├──────────────────────────────────────────────────────────────────┤
│ ↑↓ select · ⏎ confirm · c copy · tab regions · esc cancel        │
╰──────────────────────────────────────────────────────────────────╯`

// herdr cannot see omp's plan gate: its omp integration publishes state from
// tool-approval and `ask` events, and the plan review is a TUI overlay raised
// after the turn ended — so herdr has already said "done". lasso reads the
// screen and reports "blocked", which is what wait_agent status=blocked needs.
func TestPaneAgentStatusReportsOmpPlanReviewAsBlocked(t *testing.T) {
	cases := []struct {
		name   string
		agent  string
		status string
		screen string
		want   string
	}{
		{"parked on the gate (done)", "omp", "done", ompPlanReviewScreen, "blocked"},
		{"parked on the gate (idle)", "omp", "idle", ompPlanReviewScreen, "blocked"},
		// Mid-turn omp is working whatever is on screen; no read should override
		// a status herdr is actively reporting.
		{"working", "omp", "working", ompPlanReviewScreen, "working"},
		// An idle omp with no overlay stays idle.
		{"idle, no overlay", "omp", "idle", "❯ ", "idle"},
		// One marker alone is a phrase an agent could print into its own output;
		// a false "blocked" would make a wait for idle never match.
		{"only the box title", "omp", "done", "Reading the Plan Review section", "done"},
		{"only the picker title", "omp", "done", "wrote 'Plan mode - next step'", "done"},
		// The gate is omp's; another harness showing the same words is not it.
		{"other harness", "claude", "idle", ompPlanReviewScreen, "idle"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &ompScreenFake{memBackend: newMemBackend(), agent: c.agent, status: c.status, screen: c.screen}
			if got := paneAgentStatus(b, "p1"); got != c.want {
				t.Errorf("paneAgentStatus = %q, want %q", got, c.want)
			}
		})
	}
}

// The grid feeds lasso's own pane rail and switcher, so it has to reach the
// same verdict the MCP tools do — an agent the rail shows idle while wait_agent
// calls it blocked is the contradiction the whole check exists to remove. The
// grid runs on a poll, so it only screen-reads panes whose record says lasso
// launched them in omp's plan mode.
func TestGridHostPanesReportsOmpPlanGate(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)

	// Two omp agents on the host, both resting on a screen showing the overlay.
	// Only the plan-mode one may be reported blocked: the other never asked to
	// plan, so lasso has no gate to claim on its behalf.
	planned := AgentRecord{ID: "g1", Host: "local", Agent: "omp", PlanMode: true, RootPane: "p1", Type: "scratch"}
	plain := AgentRecord{ID: "g2", Host: "local", Agent: "omp", PlanMode: false, RootPane: "p2", Type: "scratch"}
	for _, rec := range []AgentRecord{planned, plain} {
		if err := appendAgent("local", rec); err != nil {
			t.Fatalf("appendAgent %s: %v", rec.ID, err)
		}
	}

	b := &gridGateFake{memBackend: newMemBackend(), screen: ompPlanReviewScreen}
	panes, err := gridHostPanes(b, "local", "local")
	if err != nil {
		t.Fatalf("gridHostPanes: %v", err)
	}
	got := map[string]string{}
	for _, p := range panes {
		got[p.PaneID] = p.AgentStatus
	}
	if got["p1"] != "blocked" {
		t.Errorf("plan-mode omp pane status = %q, want blocked", got["p1"])
	}
	if got["p2"] != "idle" {
		t.Errorf("non-plan omp pane status = %q, want idle (lasso claims no gate for it)", got["p2"])
	}
	if b.reads["p2"] {
		t.Error("the grid must not screen-read a pane whose record never asked for plan mode")
	}
}

// gridGateFake answers the enumeration gridHostPanes makes: two idle omp panes,
// both showing the same screen, and it records which panes were read so a test
// can prove the poll didn't read the one it had no reason to.
type gridGateFake struct {
	*memBackend
	screen string
	reads  map[string]bool
}

func (b *gridGateFake) HerdrCall(method string, params any) (json.RawMessage, error) {
	switch method {
	case "pane.list":
		raw, _ := json.Marshal(map[string]any{"panes": []map[string]any{
			{"pane_id": "p1", "agent": "omp", "agent_status": "idle", "workspace_id": "w1", "tab_id": "t1"},
			{"pane_id": "p2", "agent": "omp", "agent_status": "idle", "workspace_id": "w1", "tab_id": "t1"},
		}})
		return raw, nil
	case "pane.read":
		if p, ok := params.(map[string]any); ok {
			if id, ok := p["pane_id"].(string); ok {
				if b.reads == nil {
					b.reads = map[string]bool{}
				}
				b.reads[id] = true
			}
		}
		raw, _ := json.Marshal(map[string]any{"read": map[string]any{"text": b.screen}})
		return raw, nil
	}
	return json.RawMessage(`{}`), nil
}

// list_agents applies the gate check with whatever backend it got, and it got
// none when the host's herdr enumeration failed. That must leave the status
// alone, not panic and not invent a gate nobody could look for.
func TestOmpGateStatusWithoutBackend(t *testing.T) {
	if got := ompGateStatus(nil, "p1", "omp", "idle"); got != "idle" {
		t.Errorf("ompGateStatus(nil backend) = %q, want the status unchanged", got)
	}
	if got := ompGateStatus(nil, "", "omp", ""); got != "" {
		t.Errorf("ompGateStatus for an unknown pane = %q, want \"\"", got)
	}
}

// A pane that isn't there has no status at all — the gate check must not turn
// a missing pane into a blocked agent.
func TestPaneAgentStatusMissingPane(t *testing.T) {
	b := &ompScreenFake{memBackend: newMemBackend(), agent: "omp", status: "done", screen: ompPlanReviewScreen}
	if got := paneAgentStatus(b, "nosuch"); got != "" {
		t.Errorf("paneAgentStatus for a missing pane = %q, want \"\"", got)
	}
}
