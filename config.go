package main

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Lasso keeps its own small amount of state — settings for the "New Agent"
// creator plus a record of every agent it has spawned — in a single YAML file
// at ~/.lasso/config.yaml. YAML (not JSON/sqlite) so the multi-line setup
// scripts (commands run before the agent) read cleanly. Working directories for
// created agents live under ~/.lasso/worktrees (git agents) and ~/.lasso/scratch
// (non-git agents); staged attachment uploads land in ~/.lasso/uploads.
//
// All of this is host-local: the config + agent list belong to the machine
// lasso runs on, while the creation itself routes through curBackend() so it
// targets whichever herdr host is active. (Per-host repos roots are out of
// scope for now.)

// LassoConfig is the on-disk shape of ~/.lasso/config.yaml. Both yaml (disk) and
// json (the /api/agent-config response) tags are set so the field names match in
// both directions.
type LassoConfig struct {
	// ReposRoot is the directory the repo picker scans (one level deep) for git
	// repos. Defaults to ~/projects.
	ReposRoot string `yaml:"repos_root" json:"repos_root"`
	// BranchPrefix seeds the branch-prefix field in the creator (e.g. "feat/").
	BranchPrefix string `yaml:"branch_prefix" json:"branch_prefix"`
	// DefaultAgent is the AI agent preselected in the creator ("claude"|"codex").
	DefaultAgent string `yaml:"default_agent" json:"default_agent"`
	// LastRepo is the repo path selected last time, preselected next time.
	LastRepo string `yaml:"last_repo,omitempty" json:"last_repo,omitempty"`
	// ScratchSetup is the default setup script run before the agent in scratch
	// (non-git) agents.
	ScratchSetup string `yaml:"scratch_setup,omitempty" json:"scratch_setup,omitempty"`
	// Repos holds per-repo memory + settings, keyed by absolute repo path.
	Repos map[string]*RepoConfig `yaml:"repos,omitempty" json:"repos,omitempty"`
	// Agents is the append-only log of agents lasso has created.
	Agents []AgentRecord `yaml:"agents,omitempty" json:"agents,omitempty"`
}

// RepoConfig is the remembered, per-repo creator state.
type RepoConfig struct {
	// LastBaseBranch is the base branch chosen last time for this repo.
	LastBaseBranch string `yaml:"last_base_branch,omitempty" json:"last_base_branch,omitempty"`
	// CopyFiles is a comma/newline-separated list of globs copied from the repo
	// into a new worktree (e.g. ".env,.env.local").
	CopyFiles string `yaml:"copy_files,omitempty" json:"copy_files,omitempty"`
	// Setup is the setup script run in the worktree's shell before the agent.
	Setup string `yaml:"setup,omitempty" json:"setup,omitempty"`
}

// AgentRecord is one agent lasso spawned, kept so the UI can list/relink them.
type AgentRecord struct {
	ID          string    `yaml:"id" json:"id"`
	Title       string    `yaml:"title" json:"title"`
	Type        string    `yaml:"type" json:"type"` // "git" | "scratch"
	Repo        string    `yaml:"repo,omitempty" json:"repo,omitempty"`
	BaseBranch  string    `yaml:"base_branch,omitempty" json:"base_branch,omitempty"`
	Branch      string    `yaml:"branch,omitempty" json:"branch,omitempty"`
	Agent       string    `yaml:"agent" json:"agent"`
	Description string    `yaml:"description,omitempty" json:"description,omitempty"`
	Notes       string    `yaml:"notes,omitempty" json:"notes,omitempty"`
	Attachments []string  `yaml:"attachments,omitempty" json:"attachments,omitempty"`
	PlanMode    bool      `yaml:"plan_mode" json:"plan_mode"`
	WorkDir     string    `yaml:"work_dir" json:"work_dir"`
	WorkspaceID string    `yaml:"workspace_id,omitempty" json:"workspace_id,omitempty"`
	RootPane    string    `yaml:"root_pane,omitempty" json:"root_pane,omitempty"`
	CreatedAt   time.Time `yaml:"created_at" json:"created_at"`
}

// configMu serializes config reads/writes: create-agent does a load → mutate →
// save round-trip and two concurrent creates would otherwise lose one's record.
var configMu sync.Mutex

// lassoDir is ~/.lasso (overridable via LASSO_DIR, mainly for tests). It also
// ensures the directory and its worktrees/scratch/uploads subdirs exist.
func lassoDir() string {
	dir := os.Getenv("LASSO_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".lasso")
	}
	for _, sub := range []string{"", "worktrees", "scratch", "uploads"} {
		_ = os.MkdirAll(filepath.Join(dir, sub), 0o755)
	}
	return dir
}

func lassoWorktreesDir() string { return filepath.Join(lassoDir(), "worktrees") }
func lassoScratchDir() string   { return filepath.Join(lassoDir(), "scratch") }
func lassoUploadsDir() string   { return filepath.Join(lassoDir(), "uploads") }
func lassoConfigPath() string   { return filepath.Join(lassoDir(), "config.yaml") }

// applyConfigDefaults fills in the fields the creator needs a sensible value
// for when the file is missing or partial.
func applyConfigDefaults(c *LassoConfig) {
	if c.ReposRoot == "" {
		c.ReposRoot = "~/projects"
	}
	if c.DefaultAgent == "" {
		c.DefaultAgent = "claude"
	}
	if c.Repos == nil {
		c.Repos = map[string]*RepoConfig{}
	}
}

// loadLassoConfig reads ~/.lasso/config.yaml, applying defaults. A missing file
// yields a fresh default config (not an error).
func loadLassoConfig() (*LassoConfig, error) {
	c := &LassoConfig{}
	data, err := os.ReadFile(lassoConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			applyConfigDefaults(c)
			return c, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, err
	}
	applyConfigDefaults(c)
	return c, nil
}

// saveLassoConfig writes the config atomically (temp file + rename) so a crash
// mid-write can't leave a truncated config.
func saveLassoConfig(c *LassoConfig) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	dir := lassoDir()
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, lassoConfigPath())
}

// repoConf returns (creating if needed) the per-repo config entry for path.
func (c *LassoConfig) repoConf(path string) *RepoConfig {
	if c.Repos == nil {
		c.Repos = map[string]*RepoConfig{}
	}
	rc := c.Repos[path]
	if rc == nil {
		rc = &RepoConfig{}
		c.Repos[path] = rc
	}
	return rc
}
