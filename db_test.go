package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openTestDB points LASSO_DIR at a fresh temp dir and opens the state DB,
// closing it when the test ends. t.Setenv restores LASSO_DIR afterward.
func openTestDB(t *testing.T) {
	t.Helper()
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() {
		if db != nil {
			db.Close()
			db = nil
		}
	})
}

func TestFreshDefaults(t *testing.T) {
	openTestDB(t)
	s, err := getSettings()
	if err != nil {
		t.Fatalf("getSettings: %v", err)
	}
	if s.ReposRoot != "~/projects" {
		t.Errorf("repos_root default = %q, want ~/projects", s.ReposRoot)
	}
	// No preset default agent — the creator falls back to last-used.
	if s.DefaultAgent != "" {
		t.Errorf("default_agent default = %q, want empty", s.DefaultAgent)
	}
}

func TestDefaultAgentEmptyRoundTrip(t *testing.T) {
	openTestDB(t)
	if err := setSetting("default_agent", ""); err != nil {
		t.Fatal(err)
	}
	if s, _ := getSettings(); s.DefaultAgent != "" {
		t.Errorf("default_agent = %q, want empty", s.DefaultAgent)
	}
	if err := setSetting("default_agent", "codex"); err != nil {
		t.Fatal(err)
	}
	if s, _ := getSettings(); s.DefaultAgent != "codex" {
		t.Errorf("default_agent = %q, want codex", s.DefaultAgent)
	}
}

func TestPerHostIsolation(t *testing.T) {
	openTestDB(t)
	if err := setLastRepo("local", "/a"); err != nil {
		t.Fatal(err)
	}
	if err := setLastRepo("minime", "/b"); err != nil {
		t.Fatal(err)
	}
	if hs, _ := getHostState("local"); hs.LastRepo != "/a" {
		t.Errorf("local last_repo = %q, want /a", hs.LastRepo)
	}
	if hs, _ := getHostState("minime"); hs.LastRepo != "/b" {
		t.Errorf("minime last_repo = %q, want /b", hs.LastRepo)
	}
	// A host with no state reads as zero, not another host's value.
	if hs, _ := getHostState("other"); hs.LastRepo != "" {
		t.Errorf("other last_repo = %q, want empty", hs.LastRepo)
	}
}

func TestLastAgentAndType(t *testing.T) {
	openTestDB(t)
	if err := setLastAgent("local", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := setLastAgentType("local", "scratch"); err != nil {
		t.Fatal(err)
	}
	hs, _ := getHostState("local")
	if hs.LastAgent != "codex" || hs.LastAgentType != "scratch" {
		t.Errorf("got agent=%q type=%q, want codex/scratch", hs.LastAgent, hs.LastAgentType)
	}
	// Updating one field leaves the others intact (per-column upsert).
	if err := setLastRepo("local", "/repo"); err != nil {
		t.Fatal(err)
	}
	hs, _ = getHostState("local")
	if hs.LastAgent != "codex" || hs.LastAgentType != "scratch" || hs.LastRepo != "/repo" {
		t.Errorf("after setLastRepo: %+v", hs)
	}
}

func TestLoadLassoConfigPerHost(t *testing.T) {
	openTestDB(t)
	_ = setSetting("branch_prefix", "feat/")
	_ = setLastRepo("local", "/repo")
	_ = setRepoCopyFiles("local", "/repo", ".env")
	_ = setLastBaseBranch("local", "/repo", "main")
	_ = appendAgent("local", AgentRecord{ID: "1", Title: "t", Type: "git", CreatedAt: time.Now()})

	c, err := loadLassoConfig("local")
	if err != nil {
		t.Fatalf("loadLassoConfig: %v", err)
	}
	if c.BranchPrefix != "feat/" || c.LastRepo != "/repo" {
		t.Errorf("config = %+v", c)
	}
	if rc := c.Repos["/repo"]; rc == nil || rc.CopyFiles != ".env" || rc.LastBaseBranch != "main" {
		t.Errorf("repo state = %+v", c.Repos["/repo"])
	}
	if len(c.Agents) != 1 || c.Agents[0].ID != "1" {
		t.Errorf("agents = %+v", c.Agents)
	}
	// Another host shares global settings but not the per-host memory/log.
	other, _ := loadLassoConfig("other")
	if other.BranchPrefix != "feat/" {
		t.Errorf("other branch_prefix = %q, want feat/", other.BranchPrefix)
	}
	if other.LastRepo != "" || len(other.Agents) != 0 || len(other.Repos) != 0 {
		t.Errorf("other host leaked state: %+v", other)
	}
}

func TestWorkspaceTabCRUD(t *testing.T) {
	openTestDB(t)
	ws := Workspace{ID: "w1", Host: "local", Title: "feature x", Repo: "/r", WorkDir: "/wt", Kind: "git"}
	if err := insertWorkspace(ws); err != nil {
		t.Fatalf("insertWorkspace: %v", err)
	}
	if err := insertTab(Tab{ID: "t1", WorkspaceID: "w1", Title: "agent", Cwd: "/wt", Kind: "agent", AgentID: "a1"}); err != nil {
		t.Fatalf("insertTab: %v", err)
	}
	if err := insertTab(Tab{ID: "t2", WorkspaceID: "w1", Title: "shell", Cwd: "/wt", Kind: "shell", Ordinal: 1}); err != nil {
		t.Fatalf("insertTab: %v", err)
	}

	got, err := getWorkspace("w1")
	if err != nil || got.Title != "feature x" || got.Kind != "git" {
		t.Fatalf("getWorkspace = %+v err=%v", got, err)
	}
	wss, _ := listWorkspaces("local")
	if len(wss) != 1 {
		t.Fatalf("listWorkspaces = %d, want 1", len(wss))
	}
	tabs, _ := listTabs("w1")
	if len(tabs) != 2 || tabs[0].ID != "t1" || tabs[1].ID != "t2" {
		t.Fatalf("listTabs ordering wrong: %+v", tabs)
	}
	if tabs[0].Kind != "agent" {
		t.Fatalf("t1 kind = %q, want agent", tabs[0].Kind)
	}
	if n := nextTabOrdinal("w1"); n != 2 {
		t.Errorf("nextTabOrdinal = %d, want 2", n)
	}

	// rename + pin + cwd
	_ = renameWorkspace("w1", "renamed")
	_ = setWorkspacePinned("w1", true)
	_ = renameTab("t1", "Bob")
	_ = setTabCwd("t1", "/wt/sub")
	got, _ = getWorkspace("w1")
	if got.Title != "renamed" || !got.Pinned {
		t.Errorf("after rename/pin: %+v", got)
	}
	tb, _ := getTab("t1")
	if tb.Title != "Bob" || tb.Cwd != "/wt/sub" {
		t.Errorf("after tab rename/cwd: %+v", tb)
	}

	// close tab → drops from live lists
	_ = closeTab("t2")
	if tabs, _ := listTabs("w1"); len(tabs) != 1 {
		t.Errorf("after closeTab, live tabs = %d, want 1", len(tabs))
	}
	// close workspace → closes it and remaining tabs
	_ = closeWorkspace("w1")
	if wss, _ := listWorkspaces("local"); len(wss) != 0 {
		t.Errorf("after closeWorkspace, live workspaces = %d, want 0", len(wss))
	}
	if tabs, _ := listTabs("w1"); len(tabs) != 0 {
		t.Errorf("after closeWorkspace, live tabs = %d, want 0", len(tabs))
	}
}

func TestRepoPinAndDisplayName(t *testing.T) {
	openTestDB(t)
	if err := pinRepo("local", "/r", true); err != nil {
		t.Fatal(err)
	}
	if err := setRepoDisplayName("local", "/r", "My Repo"); err != nil {
		t.Fatal(err)
	}
	rc, _ := getRepoState("local", "/r")
	if !rc.Pinned || rc.DisplayName != "My Repo" {
		t.Errorf("repo state = %+v, want pinned + display name", rc)
	}
	// Round-trips through listRepoState too.
	all, _ := listRepoState("local")
	if all["/r"] == nil || !all["/r"].Pinned || all["/r"].DisplayName != "My Repo" {
		t.Errorf("listRepoState = %+v", all["/r"])
	}
}

// TestBackfillFromLegacyAgents verifies a legacy agents row (no workspace/tab)
// gets a synthesized workspace + agent-tab on migrateSchema, and the agent row is
// wired to them.
func TestBackfillFromLegacyAgents(t *testing.T) {
	openTestDB(t)
	if err := appendAgent("local", AgentRecord{
		ID: "ag1", Title: "Legacy", Type: "git", Repo: "/r", WorkDir: "/wt", Agent: "claude", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-migration DB by clearing the agent's tab link and resetting
	// user_version, then re-running the migration.
	if _, err := db.Exec(`UPDATE agents SET tab_id='', workspace_id=''`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	if err := migrateSchema(); err != nil {
		t.Fatalf("migrateSchema: %v", err)
	}
	ws, err := getWorkspace("wag1")
	if err != nil || ws.Repo != "/r" || ws.Kind != "git" {
		t.Fatalf("backfilled workspace = %+v err=%v", ws, err)
	}
	tab, err := getTab("ag1")
	if err != nil || tab.Kind != "agent" || tab.AgentID != "ag1" || tab.WorkspaceID != "wag1" {
		t.Fatalf("backfilled tab = %+v err=%v", tab, err)
	}
	ags, _ := listAgents("local")
	if len(ags) != 1 || ags[0].TabID != "ag1" || ags[0].WorkspaceID != "wag1" {
		t.Fatalf("agent not wired to tab/workspace: %+v", ags)
	}
	// Idempotent: a second run doesn't duplicate.
	if err := migrateSchema(); err != nil {
		t.Fatal(err)
	}
	if tabs, _ := listTabs("wag1"); len(tabs) != 1 {
		t.Errorf("re-run created duplicate tabs: %d", len(tabs))
	}
}

func TestMigrateFromYAML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LASSO_DIR", dir)
	yamlContent := `repos_root: ~/code
branch_prefix: feat/
default_agent: codex
last_repo: /home/x/proj
repos:
  /home/x/proj:
    last_base_branch: dev
    copy_files: .env
    setup: bun install
agents:
  - id: a1
    title: First
    type: git
    agent: claude
    created_at: 2024-01-01T00:00:00Z
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() {
		if db != nil {
			db.Close()
			db = nil
		}
	})

	if s, _ := getSettings(); s.ReposRoot != "~/code" || s.BranchPrefix != "feat/" || s.DefaultAgent != "codex" {
		t.Errorf("settings not migrated: %+v", s)
	}
	if hs, _ := getHostState("local"); hs.LastRepo != "/home/x/proj" {
		t.Errorf("last_repo = %q, want /home/x/proj", hs.LastRepo)
	}
	rc, _ := getRepoState("local", "/home/x/proj")
	if rc.LastBaseBranch != "dev" || rc.CopyFiles != ".env" || rc.Setup != "bun install" {
		t.Errorf("repo state not migrated: %+v", rc)
	}
	agents, _ := listAgents("local")
	if len(agents) != 1 || agents[0].Title != "First" {
		t.Errorf("agents not migrated: %+v", agents)
	}
	// config.yaml is renamed to .imported so it isn't re-imported.
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(err) {
		t.Errorf("config.yaml still present, want renamed")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml.imported")); err != nil {
		t.Errorf("config.yaml.imported missing: %v", err)
	}
}
