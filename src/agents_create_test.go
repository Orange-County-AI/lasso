package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixtureTopology is a fake host's MUTABLE pane topology, answering the real
// UHP contract through uhpFixtureReply and reacting to the mutations lasso
// makes the way the server does: workspace.open adds (or focuses) a workspace
// at a path and tab.new adds a pane to the focused workspace.
//
// It has to be mutable because UHP's creation calls answer with positions
// rather than identifiers — workspace.open returns only the workspace's index,
// tab.new only the tab's — so lasso resolves what it just made by reading the
// topology back. A fixture that answered a fixed listing would let a broken
// resolution pass.
type fixtureTopology struct {
	mu    sync.Mutex
	panes []pane
	// missingWorkspace is refused by workspace.focus with not_found, standing
	// in for a workspace closed under a caller holding its id.
	missingWorkspace string
}

func (f *fixtureTopology) snapshot() []pane {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pane(nil), f.panes...)
}

// focusPane must be called with f.mu held.
func (f *fixtureTopology) focusPane(paneID string) {
	for i := range f.panes {
		f.panes[i].Focused = f.panes[i].PaneID == paneID
	}
}

func (f *fixtureTopology) focusedPane() pane {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.panes {
		if p.Focused {
			return p
		}
	}
	return pane{}
}

// reply answers one UHP call against the fixture, mutating it where the real
// server would. Anything it does not model falls through to uhpFixtureReply,
// which serializes the current topology.
func (f *fixtureTopology) reply(method string, params any) (json.RawMessage, error) {
	p, _ := params.(map[string]any)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "workspace.open":
		path, _ := p["path"].(string)
		found := false
		for _, v := range f.panes {
			if v.Cwd == path {
				f.focusPane(v.PaneID)
				found = true
				break
			}
		}
		if !found {
			n := len(f.panes) + 1
			next := pane{
				PaneID:          strconv.Itoa(n),
				Cwd:             path,
				WorkspaceID:     "ws" + strconv.Itoa(n),
				TabID:           "ws" + strconv.Itoa(n) + "-t1",
				WorkspaceNumber: len(f.panes),
				TabNumber:       1,
			}
			f.panes = append(f.panes, next)
			f.focusPane(next.PaneID)
		}
	case "workspace.focus":
		id, _ := p["workspace_id"].(string)
		if id != f.missingWorkspace {
			for _, v := range f.panes {
				if v.WorkspaceID == id {
					f.focusPane(v.PaneID)
					return json.RawMessage(`{"type":"ok"}`), nil
				}
			}
		}
		return nil, &luvusError{Code: "not_found", Message: "workspace id " + id + " not found"}
	case "tab.new":
		var host pane
		for _, v := range f.panes {
			if v.Focused {
				host = v
			}
		}
		if host.WorkspaceID == "" {
			return nil, &luvusError{Code: "no_session", Message: "no workspace is open"}
		}
		tab := 0
		for _, v := range f.panes {
			if v.WorkspaceID == host.WorkspaceID && v.TabNumber > tab {
				tab = v.TabNumber
			}
		}
		next := pane{
			PaneID:          strconv.Itoa(len(f.panes) + 1),
			Cwd:             host.Cwd,
			WorkspaceID:     host.WorkspaceID,
			WorkspaceLabel:  host.WorkspaceLabel,
			WorkspaceNumber: host.WorkspaceNumber,
			TabID:           fmt.Sprintf("%s-t%d", host.WorkspaceID, tab+1),
			TabNumber:       tab + 1,
		}
		f.panes = append(f.panes, next)
		f.focusPane(next.PaneID)
		return json.Marshal(map[string]any{"type": "tab", "tab": strconv.Itoa(next.TabNumber)})
	case "workspace.rename":
		id, _ := p["workspace_id"].(string)
		name, _ := p["name"].(string)
		for i := range f.panes {
			if f.panes[i].WorkspaceID == id {
				f.panes[i].WorkspaceLabel = name
			}
		}
		return json.RawMessage(`{"type":"workspace_rename"}`), nil
	case "tab.rename":
		id, _ := p["tab_id"].(string)
		name, _ := p["name"].(string)
		for i := range f.panes {
			if f.panes[i].TabID == id {
				f.panes[i].TabLabel = name
			}
		}
		return json.RawMessage(`{"type":"ok"}`), nil
	case "workspace.close", "pane.close":
		id, _ := p["workspace_id"].(string)
		paneID, _ := p["pane"].(string)
		kept := f.panes[:0]
		for _, v := range f.panes {
			if v.WorkspaceID != id && v.PaneID != paneID {
				kept = append(kept, v)
			}
		}
		f.panes = kept
		return json.RawMessage(`{"type":"ok"}`), nil
	case "pane.focus":
		id, _ := p["pane"].(string)
		f.focusPane(id)
		return json.RawMessage(`{"type":"ok"}`), nil
	case "pane.run", "pane.send_input":
		return json.RawMessage(`{"type":"ok"}`), nil
	case "pane.read":
		return json.RawMessage(`{"type":"pane_read","text":"$ "}`), nil
	}
	return uhpFixtureReply(append([]pane(nil), f.panes...), method, params)
}

// createAgentBackend records the git plumbing lasso now runs itself: the
// worktree is cut with `git worktree add`, not through the runtime, so the path
// it lands at is observable in the git argv rather than in an RPC's params.
type createAgentBackend struct {
	*memBackend
	*fixtureTopology
	gitMu        sync.Mutex
	worktreePath string
	worktreeArgs []string
}

func newCreateAgentBackend() *createAgentBackend {
	return &createAgentBackend{memBackend: newMemBackend(), fixtureTopology: &fixtureTopology{}}
}

func (b *createAgentBackend) LuvusCall(method string, params any) (json.RawMessage, error) {
	return b.reply(method, params)
}

func (b *createAgentBackend) GitOut(dir string, args ...string) (string, error) {
	if len(args) >= 2 && args[0] == "worktree" && args[1] == "add" {
		b.gitMu.Lock()
		b.worktreeArgs = append([]string{dir}, args...)
		for _, a := range args[2:] {
			if strings.HasPrefix(a, "/") {
				b.worktreePath = a
			}
		}
		path := b.worktreePath
		b.gitMu.Unlock()
		// The tree now exists on disk as far as the rest of the flow can tell.
		b.dirs[path] = true
	}
	return "", nil
}

func (b *createAgentBackend) worktree() (string, []string) {
	b.gitMu.Lock()
	defer b.gitMu.Unlock()
	return b.worktreePath, append([]string(nil), b.worktreeArgs...)
}

func TestCreateGitAgentUsesUniqueBranchLeafForWorktreeDir(t *testing.T) {
	lasso := t.TempDir()
	t.Setenv("LASSO_DIR", lasso)
	// serveCreateAgent persists the host's remembered selections + agent log, so
	// it needs the state DB open.
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)

	b := newCreateAgentBackend()
	existing := filepath.Join(lasso, "worktrees", "app", "fix-login-a1b2")
	b.dirs[existing] = true

	prev := defaultBackend()
	setDefaultBackend(b)
	t.Cleanup(func() { setDefaultBackend(prev) })

	reqBody := `{
		"type": "git",
		"title": "Fix login",
		"repo": "/repo/app",
		"base_branch": "main",
		"branch_prefix": "feature",
		"branch_name": "fix-login-a1b2",
		"agent": "codex",
		"plan_mode": false
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/create-agent", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()

	serveCreateAgent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("serveCreateAgent status = %d, body = %s", rec.Code, rec.Body.String())
	}
	want := filepath.Join(lasso, "worktrees", "app", "fix-login-a1b2-2")
	got, argv := b.worktree()
	if got != want {
		t.Fatalf("worktree path = %q, want %q (git argv %v)", got, want, argv)
	}
	// The worktree is cut with git, off the requested base, on a new branch —
	// UHP's worktree.create can express none of those three.
	wantArgv := []string{"/repo/app", "worktree", "add", "-b", "feature/fix-login-a1b2", want, "main"}
	if !slices.Equal(argv, wantArgv) {
		t.Errorf("git argv = %v, want %v", argv, wantArgv)
	}

	var agent AgentRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &agent); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if agent.WorkDir != want {
		t.Errorf("response work_dir = %q, want %q", agent.WorkDir, want)
	}
}

// The prompt is optional: creating an agent with nothing typed at all is a
// legitimate "give me a worktree with an agent sitting in it" — the CLI comes
// up idle. The create still needs a NAME (workspace label, branch, work dir),
// so it falls back to untitledAgent instead of rejecting the request, which is
// what it used to do (400 "prompt required").
func TestCreateAgentWithNoPromptIsUntitledRatherThanRejected(t *testing.T) {
	lasso := t.TempDir()
	t.Setenv("LASSO_DIR", lasso)
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)

	b := newCreateAgentBackend()
	prev := defaultBackend()
	setDefaultBackend(b)
	t.Cleanup(func() { setDefaultBackend(prev) })

	rec, err := createAgent(b, createAgentReq{
		Type: "git", Repo: "/repo/app", BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("createAgent with no prompt: %v", err)
	}
	if rec.Title != untitledAgent {
		t.Errorf("title = %q, want %q", rec.Title, untitledAgent)
	}
	// The placeholder title names the agent; it must not become a prompt body.
	if rec.Description != "" {
		t.Errorf("description = %q, want empty — no prompt was given", rec.Description)
	}
	if rec.Branch != "untitled-agent" {
		t.Errorf("branch = %q, want %q", rec.Branch, "untitled-agent")
	}
	want := filepath.Join(lasso, "worktrees", "app", "untitled-agent")
	if rec.WorkDir != want {
		t.Errorf("work_dir = %q, want %q", rec.WorkDir, want)
	}
}

// bootFake is a backend whose agent-boot RPCs are controllable: the workspace
// open resolves to a real root pane through the shared topology fixture, but
// the pane reads that launchAgentInPane waits on block until the test releases
// them, and pane.run (the agent-launch submission) fails. That lets the test
// prove createAgent returns before the boot runs, and that a boot failure is
// recorded on the persisted agent instead of being lost.
type bootFake struct {
	*memBackend
	*fixtureTopology
	release    chan struct{} // closed by the test to let the blocked boot proceed
	readSeen   chan struct{} // closed once the boot's first pane.read lands (boot started)
	readOnce   sync.Once
	releaseOne sync.Once
	runErr     error // returned from pane.run to fail the launch
}

func (b *bootFake) LuvusCall(method string, params any) (json.RawMessage, error) {
	switch method {
	case "pane.read":
		b.readOnce.Do(func() { close(b.readSeen) })
		<-b.release // hold the boot here until the test lets it continue
		// Stable text so waitPaneReady settles quickly once released.
		return json.RawMessage(`{"type":"pane_read","text":"$ "}`), nil
	case "pane.run":
		return nil, b.runErr // the agent-launch submission fails → boot fails
	}
	return b.reply(method, params)
}

func (b *bootFake) GitOut(string, ...string) (string, error) { return "", nil }

func (b *bootFake) releaseBoot() { b.releaseOne.Do(func() { close(b.release) }) }

// createAgent must return as soon as the durable facts exist (id, workspace, root
// pane, persisted record) — WITHOUT waiting for the slow boot (file copy, setup,
// CLI launch, pane readiness). And when that async boot fails, the failure must be
// recorded on the agent so a later get_agent/list_agents shows "failed" rather than
// a phantom healthy agent.
func TestCreateAgentReturnsBeforeBootAndRecordsBootFailure(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)

	b := &bootFake{
		memBackend:      newMemBackend(),
		fixtureTopology: &fixtureTopology{},
		release:         make(chan struct{}),
		readSeen:        make(chan struct{}),
		runErr:          errors.New("pane gone"),
	}
	// Always let the (possibly still-blocked) boot goroutine finish, so it can't
	// leak or write to a closing db. Registered after the db-close cleanup so it
	// runs first (LIFO).
	t.Cleanup(b.releaseBoot)

	prev := defaultBackend()
	setDefaultBackend(b)
	t.Cleanup(func() { setDefaultBackend(prev) })

	start := time.Now()
	rec, err := createAgent(b, createAgentReq{Type: "scratch", Title: "Boot test", Prompt: "boot test"})
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}
	// The boot is still blocked (release not yet closed), so a fast return here
	// proves create didn't wait on it.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("createAgent blocked on boot: took %v", elapsed)
	}
	// Durable facts the caller needs must be populated: the workspace and pane
	// resolved out of the topology after workspace.open (whose reply names only
	// a position).
	if rec.ID == "" || rec.WorkspaceID != "ws1" || rec.RootPane != "1" || rec.WorkDir == "" {
		t.Fatalf("returned record missing durable facts: %+v", rec)
	}
	if rec.BootStatus != BootBooting {
		t.Errorf("returned BootStatus = %q, want %q", rec.BootStatus, BootBooting)
	}

	// Wait until the boot goroutine has actually started (and is now blocked in the
	// pane-readiness wait). The persisted record must still read "booting" — proof
	// the response landed while the boot was mid-flight, not after it.
	select {
	case <-b.readSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("boot goroutine never started")
	}
	if got, err := findAgentRecord("local", rec.ID); err != nil {
		t.Fatalf("findAgentRecord: %v", err)
	} else if got.BootStatus != BootBooting {
		t.Errorf("persisted BootStatus while booting = %q, want %q", got.BootStatus, BootBooting)
	}

	// Let the boot proceed; its pane.run fails, so it must record BootFailed.
	b.releaseBoot()
	deadline := time.Now().Add(5 * time.Second)
	var final AgentRecord
	for time.Now().Before(deadline) {
		final, err = findAgentRecord("local", rec.ID)
		if err != nil {
			t.Fatalf("findAgentRecord: %v", err)
		}
		if final.BootStatus == BootFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if final.BootStatus != BootFailed {
		t.Fatalf("boot failure not recorded: BootStatus = %q, want %q", final.BootStatus, BootFailed)
	}
	if !strings.Contains(final.BootError, "pane gone") {
		t.Errorf("BootError = %q, want it to mention the launch failure", final.BootError)
	}
	// A failed boot must surface as the agent's status, so get_agent/list_agents
	// don't report a phantom healthy agent.
	if info := agentInfoFrom("local", final, ""); info.Status != "failed" {
		t.Errorf("agentInfoFrom status = %q, want \"failed\"", info.Status)
	}
}

// resumeFake simulates the host state an interrupted create leaves behind: the
// branch exists in git, the worktree dir is on disk, and the runtime already
// has a workspace rooted there. It records which methods were called so the
// test can prove the retry adopted the orphan instead of re-creating.
//
// The adopt path is workspace.open on the existing dir, which the runtime
// answers by FOCUSING the workspace already rooted there rather than opening a
// second one — so the fixture is seeded with that workspace and the retry must
// come back with its ids.
type resumeFake struct {
	*memBackend
	*fixtureTopology
	branch string // the git branch that "exists"
	mu     sync.Mutex
	calls  []string
	// gitWorktreeAdd records whether the retry tried to cut a new worktree,
	// which is exactly what adopting must avoid.
	gitWorktreeAdd bool
}

func (b *resumeFake) LuvusCall(method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	b.calls = append(b.calls, method)
	b.mu.Unlock()
	return b.reply(method, params)
}

func (b *resumeFake) GitOut(_ string, args ...string) (string, error) {
	// `git branch --list <name>`: only the interrupted attempt's branch exists.
	if len(args) >= 2 && args[0] == "branch" && args[len(args)-1] == b.branch {
		return "  " + b.branch + "\n", nil
	}
	if len(args) >= 2 && args[0] == "worktree" && args[1] == "add" {
		b.mu.Lock()
		b.gitWorktreeAdd = true
		b.mu.Unlock()
	}
	return "", nil
}

func (b *resumeFake) cutAWorktree() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gitWorktreeAdd
}

func (b *resumeFake) called(method string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.calls {
		if c == method {
			return true
		}
	}
	return false
}

// A retried create after a mid-flight failure (lasso restarted, response lost)
// must resume the interrupted attempt — same branch, same worktree dir, the
// workspace it already has — rather than minting a -2 branch beside an orphan.
// This is the regression behind the 502-then-retry incident: the first
// attempt's worktree completed host-side, but its record was never saved, and
// the retry duplicated the whole tree.
func TestCreateAgentResumesInterruptedCreate(t *testing.T) {
	lasso := t.TempDir()
	t.Setenv("LASSO_DIR", lasso)
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)

	branch := "feature/fix-login-a1b2"
	workDir := filepath.Join(lasso, "worktrees", "app", "fix-login-a1b2")
	b := &resumeFake{
		memBackend: newMemBackend(),
		branch:     branch,
		// The workspace the interrupted attempt already opened at the worktree.
		fixtureTopology: &fixtureTopology{panes: []pane{{
			PaneID: "7", Cwd: workDir, WorkspaceID: "wsX", TabID: "wsX-t1", TabNumber: 1,
		}}},
	}
	b.dirs[workDir] = true // the interrupted attempt's worktree is on disk

	// The interrupted attempt's write-ahead record: no workspace, still at
	// BootCreating (a sweep to BootFailed matches the same way — workspace_id
	// being empty is what marks it interrupted).
	if err := appendAgent("local", AgentRecord{
		ID: "old1", Type: "git", Title: "Fix login", Repo: "/repo/app",
		Branch: branch, WorkDir: workDir, BootStatus: BootCreating,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("appendAgent: %v", err)
	}

	prev := defaultBackend()
	setDefaultBackend(b)
	t.Cleanup(func() { setDefaultBackend(prev) })

	rec, err := createAgent(b, createAgentReq{
		Type: "git", Title: "Fix login", Prompt: "Fix login",
		Repo: "/repo/app", BaseBranch: "main",
		BranchPrefix: "feature", BranchName: "fix-login-a1b2",
	})
	if err != nil {
		t.Fatalf("createAgent: %v", err)
	}

	if rec.Branch != branch {
		t.Errorf("branch = %q, want %q (retry must not mint a -2 suffix)", rec.Branch, branch)
	}
	if rec.WorkDir != workDir {
		t.Errorf("work_dir = %q, want the interrupted attempt's %q", rec.WorkDir, workDir)
	}
	if rec.WorkspaceID != "wsX" || rec.RootPane != "7" {
		t.Errorf("adoption: got workspace %q pane %q, want the live wsX/7", rec.WorkspaceID, rec.RootPane)
	}
	if b.cutAWorktree() {
		t.Error("git worktree add ran — the resume must reattach, not re-create")
	}
	// The orphan record is superseded by this attempt's, not left as a duplicate.
	if _, err := findAgentRecord("local", "old1"); err == nil {
		t.Error("interrupted record still present — resume should delete it")
	}
	recs, err := listAgents("local")
	if err != nil {
		t.Fatalf("listAgents: %v", err)
	}
	n := 0
	for _, r := range recs {
		if r.Branch == branch {
			n++
		}
	}
	if n != 1 {
		t.Errorf("records for %s = %d, want exactly 1", branch, n)
	}
}

func TestWorktreeDirSlug(t *testing.T) {
	cases := []struct {
		branch   string
		fallback string
		want     string
	}{
		{branch: "feature/add-dark-mode-a1b2", fallback: "add-dark-mode", want: "add-dark-mode-a1b2"},
		{branch: "fix/#42", fallback: "agent", want: "42"},
		{branch: "////", fallback: "agent", want: "agent"},
	}
	for _, c := range cases {
		if got := worktreeDirSlug(c.branch, c.fallback); got != c.want {
			t.Errorf("worktreeDirSlug(%q, %q) = %q, want %q", c.branch, c.fallback, got, c.want)
		}
	}
}

// Claude's trust dialog defaults to "No, exit" whenever the folder pre-approves
// tool permissions, so the caret's position — not a fixed default — decides
// whether Enter trusts the folder or quits the agent.
func TestTrustPromptYesHighlighted(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		yes   bool
		found bool
	}{
		{
			name: "claude yes default",
			text: `Do you trust the files in this folder?

  /home/dev/projects/lasso

❯ 1. Yes, proceed
  2. No, exit`,
			yes:   true,
			found: true,
		},
		{
			name: "claude no default with pre-approval warning",
			text: `Do you trust the files in this folder?

  /home/dev/projects/lasso

  This folder pre-approves 12 tool permission(s) via
  .claude/settings.local.json. Only trust it if you wrote them.

❯ No, exit
  Yes, I trust this folder`,
			yes:   false,
			found: true,
		},
		{
			name: "boxed option lines",
			text: "╭──────────────────────────────╮\n" +
				"│  ❯ No, exit                  │\n" +
				"│    Yes, I trust this folder  │\n" +
				"╰──────────────────────────────╯",
			yes:   false,
			found: true,
		},
		{
			name:  "ascii caret fallback",
			text:  "  > Yes, I trust this folder\n    No, exit",
			yes:   true,
			found: true,
		},
		{
			name:  "codex dialog",
			text:  "Do you trust the contents of this directory?\n\n❯ Yes, allow Codex to work here\n  No, exit",
			yes:   true,
			found: true,
		},
		{
			name:  "no caret at all",
			text:  "Do you trust the files in this folder?\n\n  1. Yes, proceed\n  2. No, exit",
			found: false,
		},
		{
			name:  "composer prompt is not a selection",
			text:  "trust this folder\n\n> ",
			found: false,
		},
		{
			name:  "empty screen",
			text:  "",
			found: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yes, found := trustPromptYesHighlighted(tc.text)
			if found != tc.found || yes != tc.yes {
				t.Fatalf("got yes=%v found=%v, want yes=%v found=%v", yes, found, tc.yes, tc.found)
			}
		})
	}
}
