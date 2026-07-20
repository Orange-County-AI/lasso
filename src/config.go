package main

import (
	"os"
	"path/filepath"
	"time"
)

// Lasso keeps its state — settings for the "New Agent" creator, per-host
// remembered selections, and a record of every agent it has spawned — in a
// SQLite database at ~/.lasso/lasso.db (see db.go). Working directories for
// created agents live under ~/.lasso/worktrees (git agents) and ~/.lasso/scratch
// (non-git agents); staged attachment uploads land in ~/.lasso/uploads.
//
// The store is host-local: it belongs to the machine lasso runs on, while the
// creation itself routes through curBackend() so it targets whichever herdr host
// is active. Selections that name a repo/branch on that host are therefore keyed
// by the active host name (curBackend().Name()).

// LassoConfig is the shape of the /api/agent-config response, assembled for a
// given host from the database.
type LassoConfig struct {
	// ReposRoot is the directory (or directories, one per line) the repo picker
	// scans one level deep for git repos. Defaults to ~/projects.
	ReposRoot string `json:"repos_root"`
	// BranchPrefix seeds the branch-prefix field in the creator (e.g. "feat/").
	BranchPrefix string `json:"branch_prefix"`
	// DefaultAgent is the AI agent preselected in the creator ("claude"|"codex"|"opencode").
	// It may be empty — "no preset default" — in which case the creator falls
	// back to LastAgent.
	DefaultAgent string `json:"default_agent"`
	// LastRepo is the repo path selected last time on this host, preselected next.
	LastRepo string `json:"last_repo,omitempty"`
	// LastAgent is the AI agent chosen last time on this host (the fallback when
	// DefaultAgent is empty).
	LastAgent string `json:"last_agent,omitempty"`
	// LastModels remembers the model chosen last time on this host, per agent
	// harness (keyed by harness id). Seeds the creator's model field.
	LastModels map[string]string `json:"last_models,omitempty"`
	// Harnesses is the compiled-in agent registry (see harness.go): the launchable
	// agents plus their UI metadata (plan-mode support, model suggestions).
	Harnesses []harnessDef `json:"harnesses"`
	// LastAgentType is the agent type chosen last time ("git"|"scratch"),
	// preselected next time.
	LastAgentType string `json:"last_agent_type,omitempty"`
	// ScratchSetup is the default setup script run before the agent in scratch
	// (non-git) agents.
	ScratchSetup string `json:"scratch_setup,omitempty"`
	// Repos holds per-repo memory + settings for this host, keyed by repo path.
	Repos map[string]*RepoConfig `json:"repos,omitempty"`
	// Agents is the log of agents lasso has created on this host.
	Agents []AgentRecord `json:"agents,omitempty"`
}

// RepoConfig is the remembered, per-repo creator state. The yaml tags are kept
// (like AgentRecord's) so a legacy config.yaml unmarshals during migration.
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
// The yaml tags are retained only so a legacy config.yaml can be unmarshaled
// during the one-time DB migration (see migrateFromYAML).
type AgentRecord struct {
	ID          string    `yaml:"id" json:"id"`
	Title       string    `yaml:"title" json:"title"`
	Type        string    `yaml:"type" json:"type"` // "git" | "scratch"
	Repo        string    `yaml:"repo,omitempty" json:"repo,omitempty"`
	BaseBranch  string    `yaml:"base_branch,omitempty" json:"base_branch,omitempty"`
	Branch      string    `yaml:"branch,omitempty" json:"branch,omitempty"`
	Agent       string    `yaml:"agent" json:"agent"`
	Model       string    `yaml:"model,omitempty" json:"model,omitempty"`
	ExtraArgs   string    `yaml:"extra_args,omitempty" json:"extra_args,omitempty"`
	Description string    `yaml:"description,omitempty" json:"description,omitempty"`
	Notes       string    `yaml:"notes,omitempty" json:"notes,omitempty"`
	Attachments []string  `yaml:"attachments,omitempty" json:"attachments,omitempty"`
	PlanMode    bool      `yaml:"plan_mode" json:"plan_mode"`
	WorkDir     string    `yaml:"work_dir" json:"work_dir"`
	WorkspaceID string    `yaml:"workspace_id,omitempty" json:"workspace_id,omitempty"`
	RootPane    string    `yaml:"root_pane,omitempty" json:"root_pane,omitempty"`
	CreatedAt   time.Time `yaml:"created_at" json:"created_at"`
	// BootStatus tracks the async boot that runs after create returns (see
	// createAgent / bootAgent). create persists the record with BootBooting, then
	// a background goroutine copies files, runs setup, and launches the agent CLI —
	// flipping this to BootReady on success or BootFailed on error. A failed boot
	// is surfaced as the agent's status (see agentInfoFrom) so get_agent/list_agents
	// show it rather than a phantom healthy agent. Empty on legacy records.
	BootStatus string `yaml:"boot_status,omitempty" json:"boot_status,omitempty"`
	// BootError is the reason a boot failed, kept for get_agent/list_agents. Empty
	// unless BootStatus is BootFailed.
	BootError string `yaml:"boot_error,omitempty" json:"boot_error,omitempty"`
}

// Boot status values for AgentRecord.BootStatus.
const (
	// BootBooting: create returned; the async boot (file copy, setup, CLI launch)
	// is still running. Not surfaced as a status string — the agent's live herdr
	// status takes over as soon as the CLI comes up.
	BootBooting = "booting"
	// BootReady: the async boot finished launching the agent CLI.
	BootReady = "ready"
	// BootFailed: the async boot errored (pane gone, setup/launch RPC failed). The
	// agent never came up; surfaced as status "failed".
	BootFailed = "failed"
)

// legacyConfig mirrors the old ~/.lasso/config.yaml so migrateFromYAML can read
// it. The live LassoConfig dropped its yaml tags, so it can't unmarshal the old
// snake_case keys — this struct carries them for the one-time import.
type legacyConfig struct {
	ReposRoot    string                 `yaml:"repos_root"`
	BranchPrefix string                 `yaml:"branch_prefix"`
	DefaultAgent string                 `yaml:"default_agent"`
	LastRepo     string                 `yaml:"last_repo,omitempty"`
	ScratchSetup string                 `yaml:"scratch_setup,omitempty"`
	Repos        map[string]*RepoConfig `yaml:"repos,omitempty"`
	Agents       []AgentRecord          `yaml:"agents,omitempty"`
}

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

// lassoDirFor is lassoDir for a specific backend's host. The local backend uses
// lassoDir() (honoring LASSO_DIR, creating subdirs locally); a remote backend
// resolves ~/.lasso under *that host's* home and ensures the subdirs exist
// there. Agent worktrees/scratch dirs must live on the host the agent runs on —
// using the local ~/.lasso path on a remote host fails with "permission denied"
// (the local home may not even exist remotely).
func lassoDirFor(b Backend) string {
	// The local backend, or an explicit LASSO_DIR override (tests), resolves the
	// local ~/.lasso. Only a real remote backend resolves against the remote home.
	if _, ok := b.(*localBackend); ok || os.Getenv("LASSO_DIR") != "" {
		return lassoDir()
	}
	home, err := b.HomeDir()
	if err != nil || home == "" {
		return lassoDir() // fall back rather than guessing a remote path
	}
	dir := filepath.Join(home, ".lasso")
	for _, sub := range []string{"", "worktrees", "scratch", "uploads"} {
		_ = b.MkdirAll(filepath.Join(dir, sub), 0o755)
	}
	return dir
}

func lassoWorktreesDirFor(b Backend) string {
	return filepath.Join(lassoDirFor(b), "worktrees")
}
func lassoScratchDirFor(b Backend) string {
	return filepath.Join(lassoDirFor(b), "scratch")
}
func lassoUploadsDirFor(b Backend) string {
	return filepath.Join(lassoDirFor(b), "uploads")
}

// loadLassoConfig assembles the creator config for one host from the database:
// global settings plus that host's remembered selections, per-repo state, and
// agent log.
func loadLassoConfig(host string) (*LassoConfig, error) {
	s, err := getSettings()
	if err != nil {
		return nil, err
	}
	hs, err := getHostState(host)
	if err != nil {
		return nil, err
	}
	repos, err := listRepoState(host)
	if err != nil {
		return nil, err
	}
	agents, err := listAgents(host)
	if err != nil {
		return nil, err
	}
	return &LassoConfig{
		ReposRoot:     s.ReposRoot,
		BranchPrefix:  s.BranchPrefix,
		DefaultAgent:  s.DefaultAgent,
		ScratchSetup:  s.ScratchSetup,
		LastRepo:      hs.LastRepo,
		LastAgent:     hs.LastAgent,
		LastAgentType: hs.LastAgentType,
		LastModels:    hs.LastModels,
		Harnesses:     harnesses,
		Repos:         repos,
		Agents:        agents,
	}, nil
}
