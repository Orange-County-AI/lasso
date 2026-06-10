package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
// way the old config.yaml did. Host-scoped rows (repo/branch/path) are keyed by
// host "local"; pure user settings (branch prefix, default agent, …) stay global.
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
  last_agent_type TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS repo_state (
  host             TEXT NOT NULL,
  repo_path        TEXT NOT NULL,
  copy_files       TEXT NOT NULL DEFAULT '',
  setup            TEXT NOT NULL DEFAULT '',
  last_base_branch TEXT NOT NULL DEFAULT '',
  pinned           INTEGER NOT NULL DEFAULT 0,
  display_name     TEXT NOT NULL DEFAULT '',
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
  description  TEXT NOT NULL DEFAULT '',
  notes        TEXT NOT NULL DEFAULT '',
  attachments  TEXT NOT NULL DEFAULT '[]',
  plan_mode    INTEGER NOT NULL DEFAULT 0,
  work_dir     TEXT NOT NULL DEFAULT '',
  workspace_id TEXT NOT NULL DEFAULT '',
  root_pane    TEXT NOT NULL DEFAULT '',
  tab_id       TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL DEFAULT ''
);
-- workspaces: a directory context (git worktree or scratch dir) holding 1..N
-- tabs.
CREATE TABLE IF NOT EXISTS workspaces (
  id         TEXT PRIMARY KEY,
  host       TEXT NOT NULL DEFAULT 'local',
  title      TEXT NOT NULL DEFAULT '',
  repo       TEXT NOT NULL DEFAULT '',     -- '' for scratch
  work_dir   TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL DEFAULT '',     -- 'git' | 'scratch'
  pinned     INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT '',
  closed_at  TEXT NOT NULL DEFAULT ''      -- '' = live (soft close)
);
-- tabs: one terminal each; tab.id is the tmux session suffix (tabSession()).
CREATE TABLE IF NOT EXISTS tabs (
  id           TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL DEFAULT '',
  title        TEXT NOT NULL DEFAULT '',
  cwd          TEXT NOT NULL DEFAULT '',   -- saved cwd to recreate a shell post-reboot
  kind         TEXT NOT NULL DEFAULT 'shell', -- 'shell' | 'agent'
  agent_id     TEXT NOT NULL DEFAULT '',
  ordinal      INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL DEFAULT '',
  closed_at    TEXT NOT NULL DEFAULT ''
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
	db = h
	if err := migrateFromYAML(); err != nil {
		return fmt.Errorf("migrate config.yaml: %w", err)
	}
	// Additive schema migrations (workspaces/tabs tables, repo pin/rename,
	// agents.tab_id) run AFTER the yaml import so yaml-imported agents are
	// backfilled into workspaces/tabs too.
	if err := migrateSchema(); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
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

// getSettingValue reads one raw settings value ("" if absent).
func getSettingValue(key string) string {
	var v string
	_ = db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

// setSetting upserts one settings key.
func setSetting(key, value string) error {
	_, err := db.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// uiState is the browser's persisted, global (host-agnostic) UI preferences:
// the Grid tab's filters and whether the right sidebar is collapsed. It's kept
// server-side (one JSON blob in settings) rather than in localStorage so the UI
// looks the same across browsers/devices reaching the same lasso.
type uiState struct {
	GridAgentsOnly   bool     `json:"grid_agents_only"`
	GridHiddenHosts  []string `json:"grid_hidden_hosts"`
	GridSelected     []string `json:"grid_selected"`
	SidebarCollapsed bool     `json:"sidebar_collapsed"`
}

// getUIState reads the persisted UI prefs (zero value — everything on, sidebar
// expanded — when nothing is stored yet).
func getUIState() (uiState, error) {
	us := uiState{GridHiddenHosts: []string{}, GridSelected: []string{}}
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

// spacesOrderKey is the settings key holding host's manual sidebar ordering.
// Each host gets its own ordering (its spaces are disjoint from other hosts');
// the local host keeps the bare legacy key so existing orderings survive.
func spacesOrderKey(host string) string {
	if isLocalHost(host) {
		return "spaces_order"
	}
	return "spaces_order:" + host
}

// getSpacesOrder reads the user's manual top-level ordering of host's sidebar
// "spaces" list — a JSON array of stable keys ("ws:<id>" for scratch workspaces,
// "repo:<path>" for repos). Empty (nil) when the user hasn't reordered yet, in
// which case serveTree falls back to its default seed order.
func getSpacesOrder(host string) ([]string, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM settings WHERE key=?`, spacesOrderKey(host)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var order []string
	_ = json.Unmarshal([]byte(v), &order)
	return order, nil
}

// setSpacesOrder persists host's manual top-level ordering verbatim (the client
// sends the full current key list on every drag, so this is a plain replace).
func setSpacesOrder(host string, order []string) error {
	b, err := json.Marshal(order)
	if err != nil {
		return err
	}
	return setSetting(spacesOrderKey(host), string(b))
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
		`SELECT last_repo, last_agent, last_agent_type FROM host_state WHERE host=?`, hostOrLocal(host)).
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
	var pinned int
	err := db.QueryRow(
		`SELECT copy_files, setup, last_base_branch, pinned, display_name FROM repo_state WHERE host=? AND repo_path=?`,
		hostOrLocal(host), repo).Scan(&rc.CopyFiles, &rc.Setup, &rc.LastBaseBranch, &pinned, &rc.DisplayName)
	if err == sql.ErrNoRows {
		return RepoConfig{}, nil
	}
	rc.Pinned = pinned != 0
	return rc, err
}

// listRepoState returns every repo's per-host config keyed by absolute path.
func listRepoState(host string) (map[string]*RepoConfig, error) {
	out := map[string]*RepoConfig{}
	rows, err := db.Query(
		`SELECT repo_path, copy_files, setup, last_base_branch, pinned, display_name FROM repo_state WHERE host=?`, hostOrLocal(host))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var pinned int
		rc := &RepoConfig{}
		if err := rows.Scan(&path, &rc.CopyFiles, &rc.Setup, &rc.LastBaseBranch, &pinned, &rc.DisplayName); err != nil {
			return out, err
		}
		rc.Pinned = pinned != 0
		out[path] = rc
	}
	return out, rows.Err()
}

// setRepoDisplayName overrides a repo's sidebar name ("" clears the override).
func setRepoDisplayName(host, repo, name string) error {
	return upsertRepoField(host, repo, "display_name", name)
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
		`INSERT INTO agents(id, host, title, type, repo, base_branch, branch, agent,
			description, notes, attachments, plan_mode, work_dir, workspace_id, root_pane, tab_id, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, host, rec.Title, rec.Type, rec.Repo, rec.BaseBranch, rec.Branch, rec.Agent,
		rec.Description, rec.Notes, string(att), boolToInt(rec.PlanMode), rec.WorkDir,
		rec.WorkspaceID, rec.RootPane, rec.TabID, rec.CreatedAt.Format(time.RFC3339Nano))
	return err
}

// listAgents returns the agents created on a host, oldest first (append order).
func listAgents(host string) ([]AgentRecord, error) {
	rows, err := db.Query(
		`SELECT id, host, title, type, repo, base_branch, branch, agent, description, notes,
			attachments, plan_mode, work_dir, workspace_id, root_pane, tab_id, created_at
		 FROM agents WHERE host=? ORDER BY created_at`, hostOrLocal(host))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRecord
	for rows.Next() {
		var rec AgentRecord
		var att, created string
		var plan int
		if err := rows.Scan(&rec.ID, &rec.Host, &rec.Title, &rec.Type, &rec.Repo, &rec.BaseBranch,
			&rec.Branch, &rec.Agent, &rec.Description, &rec.Notes, &att, &plan,
			&rec.WorkDir, &rec.WorkspaceID, &rec.RootPane, &rec.TabID, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(att), &rec.Attachments)
		rec.PlanMode = plan != 0
		rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, rec)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// agentKind returns an agent's type ("claude"|"codex"), "" if unknown — used by
// the status poller to pick the right detection heuristic for a tab's agent.
func agentKind(agentID string) string {
	var k string
	_ = db.QueryRow(`SELECT agent FROM agents WHERE id=?`, agentID).Scan(&k)
	return k
}

// ---------------------------------------------------------------------------
// workspaces & tabs — the sidebar's tree
// ---------------------------------------------------------------------------

// Workspace is a directory context (a git worktree or a scratch dir) that holds
// one or more tabs. Repos themselves are NOT workspaces — they're derived from
// repos_root at request time (sidebar.go); a workspace is one worktree under a
// repo, or a standalone scratch dir.
type Workspace struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	Title     string    `json:"title"`
	Repo      string    `json:"repo,omitempty"` // "" for scratch
	WorkDir   string    `json:"work_dir"`
	Kind      string    `json:"kind"` // "git" | "scratch"
	Pinned    bool      `json:"pinned"`
	CreatedAt time.Time `json:"created_at"`
	ClosedAt  time.Time `json:"closed_at,omitempty"` // zero = live
}

// Tab is one terminal. Its tmux session is tabSession(Tab.ID). A tab is either a
// plain shell or an agent (claude/codex), linked to its AgentRecord via AgentID.
type Tab struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Title       string    `json:"title"`
	Cwd         string    `json:"cwd"`
	Kind        string    `json:"kind"` // "shell" | "agent"
	AgentID     string    `json:"agent_id,omitempty"`
	Ordinal     int       `json:"ordinal"`
	CreatedAt   time.Time `json:"created_at"`
	ClosedAt    time.Time `json:"closed_at,omitempty"`
}

// tsOrEmpty formats a time as RFC3339Nano, or "" for the zero value (live).
func tsOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

// parseTS parses an RFC3339Nano timestamp; "" → zero time.
func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func insertWorkspace(ws Workspace) error {
	if ws.CreatedAt.IsZero() {
		ws.CreatedAt = time.Now()
	}
	_, err := db.Exec(
		`INSERT INTO workspaces(id, host, title, repo, work_dir, kind, pinned, created_at, closed_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		ws.ID, hostOrLocal(ws.Host), ws.Title, ws.Repo, ws.WorkDir, ws.Kind,
		boolToInt(ws.Pinned), ws.CreatedAt.Format(time.RFC3339Nano), tsOrEmpty(ws.ClosedAt))
	return err
}

func insertTab(t Tab) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	_, err := db.Exec(
		`INSERT INTO tabs(id, workspace_id, title, cwd, kind, agent_id, ordinal, created_at, closed_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		t.ID, t.WorkspaceID, t.Title, t.Cwd, t.Kind, t.AgentID, t.Ordinal,
		t.CreatedAt.Format(time.RFC3339Nano), tsOrEmpty(t.ClosedAt))
	if err == nil && t.Kind != "agent" {
		// A brand-new shell tab: its tmux session (created just before this insert)
		// needs its first prompt primed once the viewport points at it. Agents
		// paint their own TUI, so they're excluded. See primeShellPromptWhenAttached.
		markPrimePending(tabSession(t.ID))
	}
	return err
}

func hostOrLocal(h string) string {
	if h == "" {
		return "local"
	}
	return h
}

func scanWorkspace(rows *sql.Rows) (Workspace, error) {
	var ws Workspace
	var pinned int
	var created, closed string
	err := rows.Scan(&ws.ID, &ws.Host, &ws.Title, &ws.Repo, &ws.WorkDir, &ws.Kind,
		&pinned, &created, &closed)
	ws.Pinned = pinned != 0
	ws.CreatedAt = parseTS(created)
	ws.ClosedAt = parseTS(closed)
	return ws, err
}

const workspaceCols = `id, host, title, repo, work_dir, kind, pinned, created_at, closed_at`

// getWorkspace returns one workspace by id (sql.ErrNoRows if absent).
func getWorkspace(id string) (Workspace, error) {
	rows, err := db.Query(`SELECT `+workspaceCols+` FROM workspaces WHERE id=?`, id)
	if err != nil {
		return Workspace{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Workspace{}, err
		}
		return Workspace{}, sql.ErrNoRows
	}
	return scanWorkspace(rows)
}

// listWorkspaces returns a host's live workspaces (closed_at=”) oldest first.
// host "" means local (rows store 'local').
func listWorkspaces(host string) ([]Workspace, error) {
	rows, err := db.Query(
		`SELECT `+workspaceCols+` FROM workspaces WHERE host=? AND closed_at='' ORDER BY created_at`, hostOrLocal(host))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		ws, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

const tabCols = `id, workspace_id, title, cwd, kind, agent_id, ordinal, created_at, closed_at`

func scanTab(rows *sql.Rows) (Tab, error) {
	var t Tab
	var created, closed string
	err := rows.Scan(&t.ID, &t.WorkspaceID, &t.Title, &t.Cwd, &t.Kind, &t.AgentID,
		&t.Ordinal, &created, &closed)
	t.CreatedAt = parseTS(created)
	t.ClosedAt = parseTS(closed)
	return t, err
}

// getTab returns one tab by id (sql.ErrNoRows if absent).
func getTab(id string) (Tab, error) {
	rows, err := db.Query(`SELECT `+tabCols+` FROM tabs WHERE id=?`, id)
	if err != nil {
		return Tab{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Tab{}, err
		}
		return Tab{}, sql.ErrNoRows
	}
	return scanTab(rows)
}

// listTabs returns a workspace's live tabs in display order (ordinal, then age).
func listTabs(workspaceID string) ([]Tab, error) {
	rows, err := db.Query(
		`SELECT `+tabCols+` FROM tabs WHERE workspace_id=? AND closed_at='' ORDER BY ordinal, created_at`,
		workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tab
	for rows.Next() {
		t, err := scanTab(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// allLiveTabs returns every live tab (any kind) — used by startup reconciliation.
func allLiveTabs() ([]Tab, error) {
	rows, err := db.Query(`SELECT ` + tabCols + ` FROM tabs WHERE closed_at='' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tab
	for rows.Next() {
		t, err := scanTab(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func renameWorkspace(id, title string) error {
	_, err := db.Exec(`UPDATE workspaces SET title=? WHERE id=?`, title, id)
	return err
}

func renameTab(id, title string) error {
	_, err := db.Exec(`UPDATE tabs SET title=? WHERE id=?`, title, id)
	return err
}

func setTabCwd(id, cwd string) error {
	_, err := db.Exec(`UPDATE tabs SET cwd=? WHERE id=?`, cwd, id)
	return err
}

// closeTab soft-closes a tab (the caller kills its tmux session).
func closeTab(id string) error {
	_, err := db.Exec(`UPDATE tabs SET closed_at=? WHERE id=? AND closed_at=''`,
		time.Now().Format(time.RFC3339Nano), id)
	return err
}

// closeWorkspace soft-closes a workspace and all its tabs (caller kills sessions).
func closeWorkspace(id string) error {
	now := time.Now().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE tabs SET closed_at=? WHERE workspace_id=? AND closed_at=''`, now, id); err != nil {
		return err
	}
	_, err := db.Exec(`UPDATE workspaces SET closed_at=? WHERE id=? AND closed_at=''`, now, id)
	return err
}

// nextTabOrdinal returns the next ordinal for a new tab in a workspace.
func nextTabOrdinal(workspaceID string) int {
	var n sql.NullInt64
	_ = db.QueryRow(`SELECT MAX(ordinal) FROM tabs WHERE workspace_id=?`, workspaceID).Scan(&n)
	if n.Valid {
		return int(n.Int64) + 1
	}
	return 0
}

// nextTabName is the default numeric tab title ("1", "2", "3", …).
// It's the next ordinal + 1 (1-based). MAX(ordinal) spans closed tabs too (they
// soft-close, rows stay), so the number is monotonic per workspace: closing a tab
// never lets a later tab reuse its number.
func nextTabName(workspaceID string) string {
	return strconv.Itoa(nextTabOrdinal(workspaceID) + 1)
}

// ---------------------------------------------------------------------------
// schema migrations (PRAGMA user_version)
// ---------------------------------------------------------------------------

// migrateSchema applies versioned, additive migrations. The base dbSchema (run
// in openDB) already creates the current shape for FRESH installs; these steps
// upgrade DBs created before a given column/table existed. Idempotent.
func migrateSchema() error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v < 1 {
		if err := migrateV1(); err != nil {
			return err
		}
		if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
			return err
		}
	}
	if v < 2 {
		if err := migrateV2(); err != nil {
			return err
		}
		if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
			return err
		}
	}
	return nil
}

// migrateV2 was a one-time reconciliation of the V1 backfill against an external
// live-session source. That source is gone (lasso is tmux-backed and owns
// workspace lifecycle entirely via workspaces.closed_at), so this is now a no-op
// retained only to advance the schema version on pre-existing DBs.
func migrateV2() error { return nil }

// migrateV1 adds the tmux-era columns to pre-existing tables and backfills a
// workspace + tab for every existing agent (so old agents appear in the new
// sidebar tree). Columns are added only if missing; the workspaces/tabs tables
// themselves are created by the base dbSchema.
func migrateV1() error {
	for _, c := range []struct{ table, col, def string }{
		{"repo_state", "pinned", "INTEGER NOT NULL DEFAULT 0"},
		{"repo_state", "display_name", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "tab_id", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := addColumnIfMissing(c.table, c.col, c.def); err != nil {
			return err
		}
	}
	return backfillWorkspacesFromAgents()
}

// addColumnIfMissing runs `ALTER TABLE … ADD COLUMN` only when the column isn't
// already present (sqlite has no IF NOT EXISTS for ADD COLUMN).
func addColumnIfMissing(table, col, def string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == col {
			return nil // already there
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, col, def))
	return err
}

// backfillWorkspacesFromAgents synthesizes one workspace + one agent-tab for each
// agent that doesn't yet have a tab, mapping the old 1-agent-per-worktree model
// into the new workspace/tab tree. The agent's row is updated to point at them.
// Their tmux sessions don't exist yet — startup reconciliation recreates fresh
// shells lazily on first attach.
func backfillWorkspacesFromAgents() error {
	rows, err := db.Query(`SELECT id, host, title, type, repo, work_dir, created_at, tab_id FROM agents`)
	if err != nil {
		return err
	}
	type row struct{ id, host, title, typ, repo, workDir, created, tabID string }
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.host, &r.title, &r.typ, &r.repo, &r.workDir, &r.created, &r.tabID); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range todo {
		if r.tabID != "" {
			continue // already migrated
		}
		wsID := "w" + r.id
		created := parseTS(r.created)
		kind := r.typ
		if kind == "" {
			kind = "scratch"
		}
		if err := insertWorkspace(Workspace{
			ID: wsID, Host: hostOrLocal(r.host), Title: r.title, Repo: r.repo,
			WorkDir: r.workDir, Kind: kind, CreatedAt: created,
		}); err != nil {
			return err
		}
		if err := insertTab(Tab{
			ID: r.id, WorkspaceID: wsID, Title: r.title, Cwd: r.workDir,
			Kind: "agent", AgentID: r.id, CreatedAt: created,
		}); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE agents SET workspace_id=?, tab_id=? WHERE id=?`, wsID, r.id, r.id); err != nil {
			return err
		}
	}
	return nil
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
