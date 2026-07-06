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

func TestListAllAgentsAndUpdatePane(t *testing.T) {
	openTestDB(t)
	mk := func(id, title, work string) AgentRecord {
		return AgentRecord{ID: id, Title: title, Type: "git", Agent: "claude",
			WorkDir: work, WorkspaceID: "ws-" + id, RootPane: "p-" + id, CreatedAt: time.Now()}
	}
	if err := appendAgent("local", mk("a1", "First", "/w/a1")); err != nil {
		t.Fatal(err)
	}
	if err := appendAgent("remote", mk("b1", "Second", "/w/b1")); err != nil {
		t.Fatal(err)
	}

	all, err := listAllAgents()
	if err != nil {
		t.Fatalf("listAllAgents: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("listAllAgents len = %d, want 2", len(all))
	}
	// Append order preserved, and each row carries its host.
	if all[0].Host != "local" || all[0].Agent.ID != "a1" {
		t.Errorf("row 0 = %+v, want host=local id=a1", all[0])
	}
	if all[1].Host != "remote" || all[1].Agent.WorkDir != "/w/b1" {
		t.Errorf("row 1 = %+v, want host=remote work=/w/b1", all[1])
	}

	// Re-pointing an agent at a fresh workspace/pane is scoped by id+host.
	if err := updateAgentPane("a1", "local", "ws-new", "p-new"); err != nil {
		t.Fatalf("updateAgentPane: %v", err)
	}
	rec, err := findAgentRecord("local", "a1")
	if err != nil {
		t.Fatalf("findAgentRecord: %v", err)
	}
	if rec.WorkspaceID != "ws-new" || rec.RootPane != "p-new" {
		t.Errorf("after update: ws=%q pane=%q, want ws-new/p-new", rec.WorkspaceID, rec.RootPane)
	}
	// The same id on another host is untouched.
	if other, _ := findAgentRecord("remote", "b1"); other.RootPane != "p-b1" {
		t.Errorf("remote agent pane = %q, want p-b1 (unchanged)", other.RootPane)
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

// A spawned agent's model + extra CLI args survive the db round-trip, and
// last_models remembers the model per harness (an empty model is a real
// remembered choice — "use the harness default").
func TestAgentModelAndExtraArgsPersist(t *testing.T) {
	openTestDB(t)
	rec := AgentRecord{
		ID: "m1", Title: "t", Type: "scratch", Agent: "claude",
		Model: "opus", ExtraArgs: "--append-system-prompt hi", CreatedAt: time.Now(),
	}
	if err := appendAgent("local", rec); err != nil {
		t.Fatal(err)
	}
	got, err := listAgents("local")
	if err != nil || len(got) != 1 {
		t.Fatalf("listAgents = %+v, %v", got, err)
	}
	if got[0].Model != "opus" || got[0].ExtraArgs != "--append-system-prompt hi" {
		t.Errorf("round-trip lost fields: %+v", got[0])
	}

	if err := setLastModel("local", "claude", "sonnet"); err != nil {
		t.Fatal(err)
	}
	if err := setLastModel("local", "codex", ""); err != nil {
		t.Fatal(err)
	}
	hs, _ := getHostState("local")
	if hs.LastModels["claude"] != "sonnet" {
		t.Errorf("claude model = %q, want sonnet", hs.LastModels["claude"])
	}
	if m, ok := hs.LastModels["codex"]; !ok || m != "" {
		t.Errorf("codex model = %q (present=%v), want remembered empty", m, ok)
	}

	// The config the UI reads carries the compiled-in harness registry and the
	// per-host last-model memory.
	c, err := loadLassoConfig("local")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Harnesses) == 0 || c.Harnesses[0].ID != "claude" {
		t.Errorf("config missing harness registry: %+v", c.Harnesses)
	}
	if c.LastModels["claude"] != "sonnet" {
		t.Errorf("config last_models = %+v", c.LastModels)
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
