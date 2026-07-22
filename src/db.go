package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite" // pure-Go (CGO-free) sqlite driver, registers "sqlite"
)

// Lasso's state — the "New Agent" creator settings, its per-host remembered
// selections, and the log of agents it has spawned — lives in a single SQLite
// database at ~/.lasso/lasso.db. It replaces the earlier config.yaml; an
// existing config.yaml is imported once on first open (see migrateFromYAML).
//
// The database is host-LOCAL: it belongs to the machine lasso runs on, the same
// way the old config.yaml did. But creation routes through curBackend() to the
// active herdr host, so anything that names a repo/branch/path on that host is
// keyed by the active host name (curBackend().Name(): "local" or an ssh alias).
// That keeps a local repo path from being suggested while a remote host is
// selected. Pure user settings (branch prefix, default agent, …) stay global.
//
//	settings    key/value, global, host-agnostic user settings
//	host_state  per-host remembered selections (last repo/agent/type)
//	repo_state  per-host, per-repo settings + memory (copy-files/setup/base)
//	agents      append-only log, each row tagged with the host it ran on
//
// modernc.org/sqlite is pure Go, so the binary stays CGO-free and portable.

// db is the process-wide handle, opened once by openDB in main().
var db *sql.DB

// lassoDBPath is ~/.lasso/lasso.db (honors LASSO_DIR via lassoDir, mainly tests).
func lassoDBPath() string { return filepath.Join(lassoDir(), "lasso.db") }

const dbSchema = `
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS host_state (
  host            TEXT PRIMARY KEY,
  last_repo       TEXT NOT NULL DEFAULT '',
  last_agent      TEXT NOT NULL DEFAULT '',
  last_agent_type TEXT NOT NULL DEFAULT '',
  last_models     TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS repo_state (
  host             TEXT NOT NULL,
  repo_path        TEXT NOT NULL,
  copy_files       TEXT NOT NULL DEFAULT '',
  setup            TEXT NOT NULL DEFAULT '',
  last_base_branch TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (host, repo_path)
);
CREATE TABLE IF NOT EXISTS agents (
  id           TEXT PRIMARY KEY,
  host         TEXT NOT NULL DEFAULT 'local',
  title        TEXT NOT NULL DEFAULT '',
  type         TEXT NOT NULL DEFAULT '',
  repo         TEXT NOT NULL DEFAULT '',
  base_branch  TEXT NOT NULL DEFAULT '',
  branch       TEXT NOT NULL DEFAULT '',
  agent        TEXT NOT NULL DEFAULT '',
  model        TEXT NOT NULL DEFAULT '',
  extra_args   TEXT NOT NULL DEFAULT '',
  description  TEXT NOT NULL DEFAULT '',
  notes        TEXT NOT NULL DEFAULT '',
  attachments  TEXT NOT NULL DEFAULT '[]',
  plan_mode    INTEGER NOT NULL DEFAULT 0,
  work_dir     TEXT NOT NULL DEFAULT '',
  workspace_id TEXT NOT NULL DEFAULT '',
  root_pane    TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL DEFAULT '',
  boot_status  TEXT NOT NULL DEFAULT '',
  boot_error   TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS agent_messages (
  id           TEXT PRIMARY KEY,
  host         TEXT NOT NULL DEFAULT 'local',
  agent_id     TEXT NOT NULL,
  sender_label TEXT NOT NULL DEFAULT '',
  sender_addr  TEXT NOT NULL DEFAULT '',
  body         TEXT NOT NULL,
  status       TEXT NOT NULL DEFAULT 'pending',
  error        TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL DEFAULT '',
  delivered_at TEXT NOT NULL DEFAULT ''
);
`

// openDB opens (creating if absent) ~/.lasso/lasso.db, applies pragmas, creates
// the schema, and imports a legacy config.yaml if present. Call once at startup.
func openDB() error {
	h, err := sql.Open("sqlite", lassoDBPath())
	if err != nil {
		return err
	}
	// One connection serializes all access — simplest correct choice for this
	// low-traffic, write-light store and it sidesteps "database is locked".
	h.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := h.Exec(pragma); err != nil {
			h.Close()
			return fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := h.Exec(dbSchema); err != nil {
		h.Close()
		return fmt.Errorf("create schema: %w", err)
	}
	// Additive column migrations for databases created by an older schema —
	// CREATE TABLE IF NOT EXISTS never alters an existing table. A "duplicate
	// column name" error just means the column already exists; any other error
	// is real. Additive-only keeps the db forward AND backward compatible (an
	// older lasso reading the same db names its columns explicitly).
	for _, alter := range []string{
		`ALTER TABLE host_state ADD COLUMN last_models TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE agents ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN extra_args TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN boot_status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN boot_error TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := h.Exec(alter); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			h.Close()
			return fmt.Errorf("migrate schema: %w", err)
		}
	}
	db = h
	if err := migrateFromYAML(); err != nil {
		return fmt.Errorf("migrate config.yaml: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// settings — global, host-agnostic user settings
// ---------------------------------------------------------------------------

// appSettings is the typed view of the four settings keys the creator uses.
// DefaultAgent may be "" — meaning "no preset default, fall back to last used".
type appSettings struct {
	ReposRoot    string
	BranchPrefix string
	DefaultAgent string
	ScratchSetup string
}

// getSettings reads the settings keys, applying the same default repos_root the
// old applyConfigDefaults did. DefaultAgent is intentionally NOT defaulted.
func getSettings() (appSettings, error) {
	s := appSettings{ReposRoot: "~/projects"}
	rows, err := db.Query("SELECT key, value FROM settings")
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return s, err
		}
		switch k {
		case "repos_root":
			if v != "" {
				s.ReposRoot = v
			}
		case "branch_prefix":
			s.BranchPrefix = v
		case "default_agent":
			s.DefaultAgent = v
		case "scratch_setup":
			s.ScratchSetup = v
		}
	}
	return s, rows.Err()
}

// setSetting upserts one settings key.
func setSetting(key, value string) error {
	_, err := db.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// getSetting reads one settings key; "" (with nil error) when unset.
func getSetting(key string) (string, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err != nil {
		return "", nil // unset (or unreadable) reads as empty
	}
	return v, nil
}

// uiState is the browser's persisted, global (host-agnostic) UI preferences:
// the Grid tab's filters and whether the right sidebar is collapsed. It's kept
// server-side (one JSON blob in settings) rather than in localStorage so the UI
// looks the same across browsers/devices reaching the same lasso.
type uiState struct {
	GridAgentsOnly  bool     `json:"grid_agents_only"`
	GridHiddenHosts []string `json:"grid_hidden_hosts"`
	GridSelected    []string `json:"grid_selected"`
	// GridMode is the Grid tab's visibility mode: "watch" (Multi) shows the
	// panes toggled on in GridWatched, "select" (Single) shows one pane at a
	// time (GridSelectPane). Anything else — including the retired "all" wall —
	// reads as "watch" (normalized in getUIState).
	GridMode string `json:"grid_mode"`
	// GridWatched holds host|pane_id keys of the panes shown in Multi mode.
	GridWatched []string `json:"grid_watched"`
	// GridSelectPane is the host|pane_id shown in Select mode ("" = auto: the
	// first candidate).
	GridSelectPane string `json:"grid_select_pane"`
	// GridRailAgentsOnly filters the Grid tab's pane rail to agent panes.
	GridRailAgentsOnly bool `json:"grid_rail_agents_only"`
	SidebarCollapsed   bool `json:"sidebar_collapsed"`
	// SidebarPct is the right sidebar's open width as a percentage of the panel
	// group. Synced (rather than device-local) because the sidebar's footprint
	// sets the shared herdr pty's width — tabs disagreeing about layout render
	// blank gutters. 0 = never set; the frontend falls back to its default.
	SidebarPct float64 `json:"sidebar_pct"`
	// FilesClickNavigates controls the Files tab's folder-click behavior: when
	// true (the default) clicking a folder re-roots the tree into it; when false
	// it expands the folder in place. Defaulted true in getUIState so a fresh
	// install (or an older stored blob lacking the field) navigates.
	FilesClickNavigates bool `json:"files_click_navigates"`
}

// getUIState reads the persisted UI prefs (zero value — everything on, sidebar
// expanded — when nothing is stored yet, except FilesClickNavigates which
// defaults true).
func getUIState() (uiState, error) {
	us := uiState{
		GridHiddenHosts:     []string{},
		GridSelected:        []string{},
		GridMode:            "watch",
		GridWatched:         []string{},
		FilesClickNavigates: true,
	}
	var v string
	err := db.QueryRow(`SELECT value FROM settings WHERE key='ui_state'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return us, nil
	}
	if err != nil {
		return us, err
	}
	_ = json.Unmarshal([]byte(v), &us)
	if us.GridHiddenHosts == nil {
		us.GridHiddenHosts = []string{}
	}
	if us.GridSelected == nil {
		us.GridSelected = []string{}
	}
	if us.GridWatched == nil {
		us.GridWatched = []string{}
	}
	if us.GridMode != "watch" && us.GridMode != "select" {
		us.GridMode = "watch"
	}
	return us, nil
}

// saveUIState overwrites the persisted UI prefs with us (the client sends the
// whole object, so this is a plain replace).
func saveUIState(us uiState) error {
	b, err := json.Marshal(us)
	if err != nil {
		return err
	}
	return setSetting("ui_state", string(b))
}

// ---------------------------------------------------------------------------
// host_state — per-host remembered selections
// ---------------------------------------------------------------------------

type hostState struct {
	LastRepo      string
	LastAgent     string
	LastAgentType string
	// LastModels maps harness id -> the model chosen last time (may be "",
	// meaning "the harness default"). Stored as one JSON object column so new
	// harnesses need no schema change.
	LastModels map[string]string
}

func getHostState(host string) (hostState, error) {
	var hs hostState
	var models string
	err := db.QueryRow(
		`SELECT last_repo, last_agent, last_agent_type, last_models FROM host_state WHERE host=?`, host).
		Scan(&hs.LastRepo, &hs.LastAgent, &hs.LastAgentType, &models)
	if err == sql.ErrNoRows {
		return hostState{}, nil
	}
	_ = json.Unmarshal([]byte(models), &hs.LastModels)
	return hs, err
}

// upsertHostField sets one host_state column, leaving the others at their
// defaults on insert and untouched on update.
func upsertHostField(host, column, value string) error {
	q := fmt.Sprintf(
		`INSERT INTO host_state(host, %s) VALUES(?, ?)
		 ON CONFLICT(host) DO UPDATE SET %s=excluded.%s`, column, column, column)
	_, err := db.Exec(q, host, value)
	return err
}

func setLastRepo(host, repo string) error     { return upsertHostField(host, "last_repo", repo) }
func setLastAgent(host, agent string) error   { return upsertHostField(host, "last_agent", agent) }
func setLastAgentType(host, typ string) error { return upsertHostField(host, "last_agent_type", typ) }

// setLastModel records the model last used with one harness on a host,
// read-modify-writing the JSON map (safe: SetMaxOpenConns(1) serializes all
// db access, so there is no concurrent-writer window to race).
func setLastModel(host, agent, model string) error {
	hs, err := getHostState(host)
	if err != nil {
		return err
	}
	if hs.LastModels == nil {
		hs.LastModels = map[string]string{}
	}
	hs.LastModels[agent] = model
	b, err := json.Marshal(hs.LastModels)
	if err != nil {
		return err
	}
	return upsertHostField(host, "last_models", string(b))
}

// ---------------------------------------------------------------------------
// repo_state — per-host, per-repo settings + memory
// ---------------------------------------------------------------------------

// getRepoState returns the per-host config for one repo (zero value if none).
func getRepoState(host, repo string) (RepoConfig, error) {
	var rc RepoConfig
	err := db.QueryRow(
		`SELECT copy_files, setup, last_base_branch FROM repo_state WHERE host=? AND repo_path=?`,
		host, repo).Scan(&rc.CopyFiles, &rc.Setup, &rc.LastBaseBranch)
	if err == sql.ErrNoRows {
		return RepoConfig{}, nil
	}
	return rc, err
}

// listRepoState returns every repo's per-host config keyed by absolute path.
func listRepoState(host string) (map[string]*RepoConfig, error) {
	out := map[string]*RepoConfig{}
	rows, err := db.Query(
		`SELECT repo_path, copy_files, setup, last_base_branch FROM repo_state WHERE host=?`, host)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		rc := &RepoConfig{}
		if err := rows.Scan(&path, &rc.CopyFiles, &rc.Setup, &rc.LastBaseBranch); err != nil {
			return out, err
		}
		out[path] = rc
	}
	return out, rows.Err()
}

func upsertRepoField(host, repo, column, value string) error {
	q := fmt.Sprintf(
		`INSERT INTO repo_state(host, repo_path, %s) VALUES(?, ?, ?)
		 ON CONFLICT(host, repo_path) DO UPDATE SET %s=excluded.%s`, column, column, column)
	_, err := db.Exec(q, host, repo, value)
	return err
}

func setRepoCopyFiles(host, repo, v string) error {
	return upsertRepoField(host, repo, "copy_files", v)
}
func setRepoSetup(host, repo, v string) error { return upsertRepoField(host, repo, "setup", v) }
func setLastBaseBranch(host, repo, v string) error {
	return upsertRepoField(host, repo, "last_base_branch", v)
}

// ---------------------------------------------------------------------------
// agents — append-only log
// ---------------------------------------------------------------------------

// appendAgent records a freshly created agent, tagged with the host it ran on.
func appendAgent(host string, rec AgentRecord) error {
	att, _ := json.Marshal(rec.Attachments)
	if rec.Attachments == nil {
		att = []byte("[]")
	}
	_, err := db.Exec(
		`INSERT INTO agents(id, host, title, type, repo, base_branch, branch, agent, model, extra_args,
			description, notes, attachments, plan_mode, work_dir, workspace_id, root_pane, created_at,
			boot_status, boot_error)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, host, rec.Title, rec.Type, rec.Repo, rec.BaseBranch, rec.Branch, rec.Agent,
		rec.Model, rec.ExtraArgs, rec.Description, rec.Notes, string(att), boolToInt(rec.PlanMode),
		rec.WorkDir, rec.WorkspaceID, rec.RootPane, rec.CreatedAt.Format(time.RFC3339Nano),
		rec.BootStatus, rec.BootError)
	return err
}

// updateAgentCreated flips a write-ahead record (BootCreating) to its created
// state in one statement: the workspace/pane herdr returned plus the next boot
// status. Scoped by id+host since ids are only unique within a host.
func updateAgentCreated(id, host, workspaceID, rootPane, status string) error {
	_, err := db.Exec(
		`UPDATE agents SET workspace_id=?, root_pane=?, boot_status=? WHERE id=? AND host=?`,
		workspaceID, rootPane, status, id, host)
	return err
}

// findInterruptedCreate returns the newest record of a create for host+repo+
// branch that never completed: still at BootCreating (the process died mid-
// create, or the response was lost) or flipped to BootFailed without ever
// getting a workspace (the create RPC itself errored). Records that reached a
// workspace are real agents and never match. Powers the resume-on-retry path in
// createAgent — the New Agent modal resends the same generated branch name, so
// a retry after a mid-create 502 picks the orphan up instead of minting a -2.
func findInterruptedCreate(host, repo, branch string) (AgentRecord, bool) {
	row := db.QueryRow(
		`SELECT id, title, work_dir FROM agents
		 WHERE host=? AND repo=? AND branch=? AND workspace_id='' AND boot_status IN (?, ?)
		 ORDER BY created_at DESC LIMIT 1`,
		host, repo, branch, BootCreating, BootFailed)
	var rec AgentRecord
	if err := row.Scan(&rec.ID, &rec.Title, &rec.WorkDir); err != nil {
		return AgentRecord{}, false
	}
	rec.Host, rec.Repo, rec.Branch = host, repo, branch
	return rec, true
}

// deleteAgentRecord removes one agent row. Used when a retried create adopts an
// interrupted attempt: the new attempt's record supersedes the orphan, and one
// logical agent should not appear twice in history.
func deleteAgentRecord(id, host string) error {
	_, err := db.Exec(`DELETE FROM agents WHERE id=? AND host=?`, id, host)
	return err
}

// sweepInterruptedCreates marks any record still at BootCreating as failed —
// called once at startup, where such a record by definition belongs to a create
// a previous process never finished. The distinctive error keeps it adoptable
// (workspace_id is still empty) and tells the user what happened. (If a second
// lasso instance shares this DB — e.g. a dev run — its in-flight create could
// be swept too; creates take seconds, so the window is negligible.)
func sweepInterruptedCreates() {
	res, err := db.Exec(
		`UPDATE agents SET boot_status=?, boot_error=? WHERE boot_status=?`,
		BootFailed, "create interrupted by a lasso restart — retrying the same create resumes it", BootCreating)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("agents:   marked %d interrupted create(s) failed (adoptable on retry)", n)
	}
}

// updateAgentBootStatus records the outcome of an agent's async boot (see
// bootAgent). Scoped by id+host since ids are only unique within a host. Best
// effort: a persist failure here just leaves the record at its prior status.
func updateAgentBootStatus(id, host, status, bootErr string) error {
	if db == nil {
		return nil // db closed out from under a late boot goroutine (e.g. in tests)
	}
	_, err := db.Exec(
		`UPDATE agents SET boot_status=?, boot_error=? WHERE id=? AND host=?`,
		status, bootErr, id, host)
	return err
}

// listAgents returns the agents created on a host, oldest first (append order).
func listAgents(host string) ([]AgentRecord, error) {
	rows, err := db.Query(
		`SELECT id, title, type, repo, base_branch, branch, agent, model, extra_args, description, notes,
			attachments, plan_mode, work_dir, workspace_id, root_pane, created_at, boot_status, boot_error
		 FROM agents WHERE host=? ORDER BY created_at`, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRecord
	for rows.Next() {
		var rec AgentRecord
		var att, created string
		var plan int
		if err := rows.Scan(&rec.ID, &rec.Title, &rec.Type, &rec.Repo, &rec.BaseBranch,
			&rec.Branch, &rec.Agent, &rec.Model, &rec.ExtraArgs, &rec.Description, &rec.Notes, &att, &plan,
			&rec.WorkDir, &rec.WorkspaceID, &rec.RootPane, &created, &rec.BootStatus, &rec.BootError); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(att), &rec.Attachments)
		rec.PlanMode = plan != 0
		rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		rec.Host = host
		out = append(out, rec)
	}
	return out, rows.Err()
}

// hostAgent pairs an AgentRecord with the host it ran on — used by the cross-host
// history view (the agent log is host-local but tagged per host).
type hostAgent struct {
	Host  string
	Agent AgentRecord
}

// listAllAgents returns every recorded agent across all hosts, oldest first. The
// ⌘K switcher's "show closed" mode joins these against the live panes so an agent
// whose pane was closed can still be found (and reopened) by its work dir/prompt.
func listAllAgents() ([]hostAgent, error) {
	rows, err := db.Query(
		`SELECT id, host, title, type, repo, base_branch, branch, agent, model, extra_args, description, notes,
			attachments, plan_mode, work_dir, workspace_id, root_pane, created_at, boot_status, boot_error
		 FROM agents ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hostAgent
	for rows.Next() {
		var rec AgentRecord
		var host, att, created string
		var plan int
		if err := rows.Scan(&rec.ID, &host, &rec.Title, &rec.Type, &rec.Repo, &rec.BaseBranch,
			&rec.Branch, &rec.Agent, &rec.Model, &rec.ExtraArgs, &rec.Description, &rec.Notes, &att, &plan,
			&rec.WorkDir, &rec.WorkspaceID, &rec.RootPane, &created, &rec.BootStatus, &rec.BootError); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(att), &rec.Attachments)
		rec.PlanMode = plan != 0
		rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		rec.Host = host
		out = append(out, hostAgent{Host: host, Agent: rec})
	}
	return out, rows.Err()
}

// updateAgentPane re-points a recorded agent at a freshly created workspace/pane,
// so a reopened agent shows as live again (the switcher matches records to panes
// by host+root_pane). Scoped by id+host since ids are only unique within a host.
func updateAgentPane(id, host, workspaceID, rootPane string) error {
	_, err := db.Exec(
		`UPDATE agents SET workspace_id=?, root_pane=? WHERE id=? AND host=?`,
		workspaceID, rootPane, id, host)
	return err
}

// updateAgentTitleByWorkspace re-titles the agent living in a workspace, keeping
// the record's title — the address list_agents and message_agent surface — in
// step with a workspace rename from the UI. Scoped by host since workspace ids
// are only unique per host.
func updateAgentTitleByWorkspace(host, workspaceID, title string) error {
	if db == nil || workspaceID == "" || strings.TrimSpace(title) == "" {
		return nil
	}
	_, err := db.Exec(
		`UPDATE agents SET title=? WHERE host=? AND workspace_id=?`,
		strings.TrimSpace(title), host, workspaceID)
	return err
}

// ---------------------------------------------------------------------------
// agent_messages — the store-and-forward queue behind the message_agent MCP
// tool. Rows are appended by message_agent and drained by the message
// dispatcher (messages.go), which submits into the recipient's pane only when
// herdr reports its agent idle.
// ---------------------------------------------------------------------------

// enqueueAgentMessage appends one pending message to the queue.
func enqueueAgentMessage(m AgentMessage) error {
	_, err := db.Exec(
		`INSERT INTO agent_messages(id, host, agent_id, sender_label, sender_addr, body, status, created_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		m.ID, m.Host, m.AgentID, m.SenderLabel, m.SenderAddr, m.Body, msgPending,
		m.CreatedAt.Format(time.RFC3339Nano))
	return err
}

// listPendingMessages returns every undelivered message across all hosts,
// oldest first — the dispatcher's work list. A nil db (tests, shutdown) reads
// as an empty queue.
func listPendingMessages() ([]AgentMessage, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.Query(
		`SELECT id, host, agent_id, sender_label, sender_addr, body, created_at
		 FROM agent_messages WHERE status=? ORDER BY created_at`, msgPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentMessage
	for rows.Next() {
		var m AgentMessage
		var created string
		if err := rows.Scan(&m.ID, &m.Host, &m.AgentID, &m.SenderLabel, &m.SenderAddr,
			&m.Body, &created); err != nil {
			return nil, err
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// markMessageDelivered flips one message to delivered, stamping when.
func markMessageDelivered(id string) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(
		`UPDATE agent_messages SET status=?, delivered_at=? WHERE id=?`,
		msgDelivered, time.Now().Format(time.RFC3339Nano), id)
	return err
}

// markMessageFailed flips one message to failed with the reason (the recipient
// died before delivery). Failed messages are never retried — a pane that later
// hosts a different agent must not receive them.
func markMessageFailed(id, detail string) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(
		`UPDATE agent_messages SET status=?, error=? WHERE id=?`,
		msgFailed, detail, id)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// one-time migration from the legacy config.yaml
// ---------------------------------------------------------------------------

// migrateFromYAML imports an existing ~/.lasso/config.yaml into the (empty) DB
// once, then renames it to config.yaml.imported so it's neither re-imported nor
// lost. A missing file, or a non-empty settings table, is a no-op. All legacy
// data is host-local, so it lands under host "local".
func migrateFromYAML() error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settings`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil // already configured — don't re-import
	}
	path := lassoConfigPath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil // fresh install, nothing to migrate
	}
	if err != nil {
		return err
	}
	var c legacyConfig
	if err := yaml.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("parse legacy config: %w", err)
	}

	if c.ReposRoot != "" {
		if err := setSetting("repos_root", c.ReposRoot); err != nil {
			return err
		}
	}
	if err := setSetting("branch_prefix", c.BranchPrefix); err != nil {
		return err
	}
	if err := setSetting("default_agent", c.DefaultAgent); err != nil {
		return err
	}
	if err := setSetting("scratch_setup", c.ScratchSetup); err != nil {
		return err
	}
	if c.LastRepo != "" {
		if err := setLastRepo("local", c.LastRepo); err != nil {
			return err
		}
	}
	for path, rc := range c.Repos {
		if rc == nil {
			continue
		}
		if rc.CopyFiles != "" {
			if err := setRepoCopyFiles("local", path, rc.CopyFiles); err != nil {
				return err
			}
		}
		if rc.Setup != "" {
			if err := setRepoSetup("local", path, rc.Setup); err != nil {
				return err
			}
		}
		if rc.LastBaseBranch != "" {
			if err := setLastBaseBranch("local", path, rc.LastBaseBranch); err != nil {
				return err
			}
		}
	}
	for _, rec := range c.Agents {
		if err := appendAgent("local", rec); err != nil {
			return err
		}
	}
	// Settings now non-empty (default_agent always written), so re-runs no-op
	// even before the rename; the rename keeps a human-readable backup.
	return os.Rename(path, path+".imported")
}
