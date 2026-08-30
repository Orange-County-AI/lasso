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
// way the old config.yaml did. But creation routes through defaultBackend() to the
// active herdr host, so anything that names a repo/branch/path on that host is
// keyed by the active host name (defaultBackend().Name(): "local" or an ssh alias).
// That keeps a local repo path from being suggested while a remote host is
// selected. Pure user settings (branch prefix, default agent, …) stay global.
//
//	settings    key/value, global, host-agnostic user settings
//	host_state  per-host remembered selections (last repo/agent/type)
//	repo_state  per-host, per-repo settings + memory (copy-files/setup/base)
//	agents      append-only log, each row tagged with the host it ran on
//	oauth_*     MCP OAuth clients/codes/tokens, when MCP_OAUTH is set (oauth.go)
//	mcp_group_* host groups + directed grants, the reach that sits between
//	            "self" and "fleet" (groups.go)
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
  last_agent_type TEXT NOT NULL DEFAULT ''
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
  effort       TEXT NOT NULL DEFAULT '',
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
  boot_error   TEXT NOT NULL DEFAULT '',
  -- closed_at stamps the moment reconciliation (agentreap.go) confirmed the
  -- agent's herdr pane was gone. Non-empty = tombstone: the row stays for the
  -- history/reopen views, but every "which agents are there" query filters it
  -- out, since nothing can be sent to, read from, or closed on it.
  closed_at    TEXT NOT NULL DEFAULT ''
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

// groupsSchema is the host-group model (groups.go, `lasso mcp-group`), appended
// to the schema exec below. Deliberately no foreign keys, even though the
// connection sets PRAGMA foreign_keys=ON: no table in this db uses them (house
// style), and here they would also forbid naming a subgroup before it exists —
// which the CLI allows on purpose, since a dangling reference is inert in
// closure resolution rather than an error, exactly like an ssh alias that was
// removed after a host joined a group.
const groupsSchema = `
CREATE TABLE IF NOT EXISTS mcp_groups (
  name       TEXT PRIMARY KEY,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS mcp_group_members (
  group_name TEXT NOT NULL,
  member     TEXT NOT NULL,   -- host alias, "local", or a child group name
  kind       TEXT NOT NULL,   -- 'host' | 'group'
  PRIMARY KEY (group_name, member, kind)
);
CREATE TABLE IF NOT EXISTS mcp_group_grants (
  from_group TEXT NOT NULL,
  to_group   TEXT NOT NULL,   -- directed: from's hosts may reach to's hosts
  PRIMARY KEY (from_group, to_group)
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
	if _, err := h.Exec(dbSchema + oauthSchema + groupsSchema); err != nil {
		h.Close()
		return fmt.Errorf("create schema: %w", err)
	}
	// Additive column migrations for databases created by an older schema —
	// CREATE TABLE IF NOT EXISTS never alters an existing table. A "duplicate
	// column name" error just means the column already exists; any other error
	// is real. Additive-only keeps the db forward AND backward compatible (an
	// older lasso reading the same db names its columns explicitly).
	for _, alter := range []string{
		`ALTER TABLE agents ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN extra_args TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN effort TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN boot_status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN boot_error TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN closed_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE oauth_clients ADD COLUMN host TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE oauth_clients ADD COLUMN mcp_scope TEXT NOT NULL DEFAULT ''`,
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
	ReposRoot                string
	BranchPrefix             string
	DefaultAgent             string
	DefaultTerminalWorkspace string
	ScratchSetup             string
}

// getSettings reads the settings keys, applying the same default repos_root the
// old applyConfigDefaults did. DefaultAgent is intentionally NOT defaulted.
func getSettings() (appSettings, error) {
	s := appSettings{ReposRoot: "~/projects", DefaultTerminalWorkspace: "~"}
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
		case "default_terminal_workspace":
			if v != "" {
				s.DefaultTerminalWorkspace = v
			}
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
// right-sidebar layout and usage-footer preferences. It's kept server-side (one
// JSON blob in settings) rather than in localStorage so the UI looks the same
// across browsers/devices reaching the same lasso.
type uiState struct {
	SidebarCollapsed bool `json:"sidebar_collapsed"`
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
	// UsageHidden contains provider names omitted from the bottom usage footer.
	// A deny-list keeps new providers visible by default on older installations.
	UsageHidden []string `json:"usage_hidden"`
	// UsageOrder is the preferred provider order in the bottom usage footer.
	// Missing providers are appended client-side so upgrades expose new ones.
	UsageOrder []string `json:"usage_order"`
	// UsageCompact selects the one-line abbreviated footer layout.
	UsageCompact bool `json:"usage_compact"`
}

// getUIState reads the persisted UI prefs (zero value — everything on, sidebar
// expanded — when nothing is stored yet, except FilesClickNavigates which
// defaults true).
func getUIState() (uiState, error) {
	us := uiState{
		FilesClickNavigates: true,
		UsageHidden:         []string{},
		UsageOrder:          []string{},
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
	if us.UsageHidden == nil {
		us.UsageHidden = []string{}
	}
	if us.UsageOrder == nil {
		us.UsageOrder = []string{}
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
}

func getHostState(host string) (hostState, error) {
	var hs hostState
	err := db.QueryRow(
		`SELECT last_repo, last_agent, last_agent_type FROM host_state WHERE host=?`, host).
		Scan(&hs.LastRepo, &hs.LastAgent, &hs.LastAgentType)
	if err == sql.ErrNoRows {
		return hostState{}, nil
	}
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
		`INSERT INTO agents(id, host, title, type, repo, base_branch, branch, agent, model, effort, extra_args,
			description, notes, attachments, plan_mode, work_dir, workspace_id, root_pane, created_at,
			boot_status, boot_error)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, host, rec.Title, rec.Type, rec.Repo, rec.BaseBranch, rec.Branch, rec.Agent,
		rec.Model, rec.Effort, rec.ExtraArgs, rec.Description, rec.Notes, string(att), boolToInt(rec.PlanMode),
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

// agentCols is the column list every agent query selects, in scanAgentRow's
// order. Named explicitly (never SELECT *) so an older lasso reading a newer
// db's table keeps working.
const agentCols = `id, host, title, type, repo, base_branch, branch, agent, model, effort, extra_args,
	description, notes, attachments, plan_mode, work_dir, workspace_id, root_pane, created_at,
	boot_status, boot_error, closed_at`

// scanAgentRow reads one agentCols row into an AgentRecord (Host included).
func scanAgentRow(rows *sql.Rows) (AgentRecord, error) {
	var rec AgentRecord
	var att, created string
	var plan int
	if err := rows.Scan(&rec.ID, &rec.Host, &rec.Title, &rec.Type, &rec.Repo, &rec.BaseBranch,
		&rec.Branch, &rec.Agent, &rec.Model, &rec.Effort, &rec.ExtraArgs, &rec.Description, &rec.Notes,
		&att, &plan, &rec.WorkDir, &rec.WorkspaceID, &rec.RootPane, &created,
		&rec.BootStatus, &rec.BootError, &rec.ClosedAt); err != nil {
		return AgentRecord{}, err
	}
	_ = json.Unmarshal([]byte(att), &rec.Attachments)
	rec.PlanMode = plan != 0
	rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return rec, nil
}

// listAgents returns the LIVE agents recorded on a host, oldest first (append
// order) — records reconciliation has tombstoned (closed_at set: their herdr
// pane is gone) are excluded, because every caller of this is asking "which
// agents are there", and an agent whose pane herdr no longer has can be neither
// messaged, read, nor closed. The history/reopen views take
// listAllAgentsIncludingClosed / findAgentRecordAny instead.
func listAgents(host string) ([]AgentRecord, error) {
	rows, err := db.Query(
		`SELECT `+agentCols+` FROM agents WHERE host=? AND closed_at='' ORDER BY created_at`, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRecord
	for rows.Next() {
		rec, err := scanAgentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// findAgentRecordAny looks up one agent by id on a host INCLUDING tombstoned
// records. Only the reopen path wants this: reopening a closed agent is exactly
// the operation that revives its record (updateAgentPane clears closed_at), so
// resolving it through the live-only listAgents would make the history view's
// primary action impossible.
func findAgentRecordAny(host, id string) (AgentRecord, error) {
	rows, err := db.Query(`SELECT `+agentCols+` FROM agents WHERE host=? AND id=?`, host, id)
	if err != nil {
		return AgentRecord{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return AgentRecord{}, err
		}
		return AgentRecord{}, fmt.Errorf("no agent %q on host %q", id, host)
	}
	return scanAgentRow(rows)
}

// markAgentClosed tombstones one record: reconciliation confirmed herdr no
// longer has its pane. Scoped by id+host — ids are only unique within a host, so
// an unscoped update would let one host's herdr state condemn another's records.
// The row itself is kept: it is the agent's history, and reopening it from the
// switcher clears the stamp again (updateAgentPane).
func markAgentClosed(id, host string) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(
		`UPDATE agents SET closed_at=? WHERE id=? AND host=? AND closed_at=''`,
		time.Now().UTC().Format(time.RFC3339Nano), id, host)
	return err
}

// hostAgent pairs an AgentRecord with the host it ran on — used by the cross-host
// history view (the agent log is host-local but tagged per host).
type hostAgent struct {
	Host  string
	Agent AgentRecord
}

// listAllAgents returns every LIVE recorded agent across all hosts, oldest
// first — the cross-host counterpart of listAgents, with the same tombstone
// filter and for the same reason: its callers (message_agent recipient
// resolution, cross-host pane matching, closeme) all resolve a target to act on.
func listAllAgents() ([]hostAgent, error) {
	return queryHostAgents(`SELECT ` + agentCols + ` FROM agents WHERE closed_at='' ORDER BY created_at`)
}

// listAllAgentsIncludingClosed returns every recorded agent across all hosts,
// tombstones included, oldest first. The ⌘K switcher's "show closed" mode joins
// these against the live panes so an agent whose pane was closed can still be
// found (and reopened) by its work dir/prompt — which is the whole point of
// keeping the row rather than deleting it.
func listAllAgentsIncludingClosed() ([]hostAgent, error) {
	return queryHostAgents(`SELECT ` + agentCols + ` FROM agents ORDER BY created_at`)
}

func queryHostAgents(q string) ([]hostAgent, error) {
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hostAgent
	for rows.Next() {
		rec, err := scanAgentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, hostAgent{Host: rec.Host, Agent: rec})
	}
	return out, rows.Err()
}

// updateAgentPane re-points a recorded agent at a freshly created workspace/pane,
// so a reopened agent shows as live again (the switcher matches records to panes
// by host+root_pane). Scoped by id+host since ids are only unique within a host.
// It also clears any closed_at stamp: the record now names a pane herdr has, so
// the tombstone reconciliation set is no longer true and the agent belongs back
// in list_agents.
func updateAgentPane(id, host, workspaceID, rootPane string) error {
	_, err := db.Exec(
		`UPDATE agents SET workspace_id=?, root_pane=?, closed_at='' WHERE id=? AND host=?`,
		workspaceID, rootPane, id, host)
	return err
}

// updateAgentTitle re-titles one agent by id, keeping the record's title — the
// address list_agents and message_agent surface — in step with the workspace
// label auto-titling just changed. Scoped by id+host since ids are only unique
// within a host.
func updateAgentTitle(id, host, title string) error {
	if db == nil || strings.TrimSpace(title) == "" {
		return nil
	}
	_, err := db.Exec(
		`UPDATE agents SET title=? WHERE id=? AND host=?`,
		strings.TrimSpace(title), id, host)
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
