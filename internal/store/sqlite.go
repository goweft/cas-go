// SQLiteStore is the production Store implementation backed by SQLite (WAL mode).
// Uses modernc.org/sqlite — pure Go, no CGo, preserves single static binary.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore persists sessions, messages, workspaces, and history.
type SQLiteStore struct {
	db *sql.DB
}

// DefaultPath returns ~/.cas/cas.db.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cas", "cas.db")
}

// NewSQLiteStore opens (or creates) the SQLite database at path.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("sqlite: mkdir %s: %w", filepath.Dir(path), err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}

	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		return nil, fmt.Errorf("sqlite: WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return nil, fmt.Errorf("sqlite: fk: %w", err)
	}

	s := &SQLiteStore{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("sqlite: migrate: %w", err)
	}
	return s, nil
}

// migrate applies schema creation and any pending version migrations.
// Uses PRAGMA user_version to track which migrations have run.
func (s *SQLiteStore) migrate() error {
	// ── v0: base schema ──────────────────────────────────────────
	if _, err := s.db.Exec(`
	CREATE TABLE IF NOT EXISTS sessions (
		id         TEXT PRIMARY KEY,
		created_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS messages (
		id         TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		role       TEXT NOT NULL,
		text       TEXT NOT NULL,
		timestamp  TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS workspaces (
		id         TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		type       TEXT NOT NULL,
		title      TEXT NOT NULL,
		content    TEXT NOT NULL,
		created_at TEXT NOT NULL,
		closed_at  TEXT
	);
	CREATE TABLE IF NOT EXISTS workspace_history (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		workspace_id TEXT NOT NULL,
		version      INTEGER NOT NULL,
		title        TEXT NOT NULL,
		content      TEXT NOT NULL,
		saved_at     TEXT NOT NULL
	);
	`); err != nil {
		return err
	}

	// ── v1: fix messages.id from INTEGER to TEXT ──────────────────
	// Existing databases created before this migration have
	// messages.id as INTEGER PRIMARY KEY AUTOINCREMENT.
	// Inserting hex string IDs into that column raises SQLITE_MISMATCH (20).
	var schemaVersion int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&schemaVersion)

	if schemaVersion < 1 {
		if err := s.migrateMessagesIDToText(); err != nil {
			return fmt.Errorf("migration v1: %w", err)
		}
		if _, err := s.db.Exec(`PRAGMA user_version = 1`); err != nil {
			return err
		}
	}

	if schemaVersion < 2 {
		if err := s.migrateAddOrchestration(); err != nil {
			return fmt.Errorf("migration v2: %w", err)
		}
		if _, err := s.db.Exec(`PRAGMA user_version = 2`); err != nil {
			return err
		}
	}

	if schemaVersion < 3 {
		if err := s.migrateAddOrchestrationStatus(); err != nil {
			return fmt.Errorf("migration v3: %w", err)
		}
		if _, err := s.db.Exec(`PRAGMA user_version = 3`); err != nil {
			return err
		}
	}

	return nil
}

// migrateAddOrchestrationStatus (v3) adds run lifecycle columns so a run is
// recorded before its steps execute and failed runs/steps are auditable.
// Pre-v3 runs were only ever written after success, so backfilling
// status='completed' is exact, not a guess.
func (s *SQLiteStore) migrateAddOrchestrationStatus() error {
	stmts := []string{
		`ALTER TABLE orchestration_runs ADD COLUMN status TEXT NOT NULL DEFAULT 'completed'`,
		`ALTER TABLE orchestration_runs ADD COLUMN error TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE orchestration_steps ADD COLUMN error TEXT NOT NULL DEFAULT ''`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// migrateMessagesIDToText recreates the messages table with id TEXT PRIMARY KEY.
// Preserves all existing rows; old integer IDs are converted to strings.
func (s *SQLiteStore) migrateMessagesIDToText() error {
	// Check current column type — if already TEXT, nothing to do
	var colType string
	rows, err := s.db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dfltValue sql.NullString
		var pk int
		rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk)
		if name == "id" {
			colType = typ
		}
	}
	rows.Close()

	if colType == "TEXT" || colType == "" {
		return nil // already correct or table doesn't exist
	}

	// Recreate with TEXT id
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		CREATE TABLE messages_new (
			id         TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			role       TEXT NOT NULL,
			text       TEXT NOT NULL,
			timestamp  TEXT NOT NULL
		)
	`); err != nil {
		return err
	}

	// Copy existing rows — cast integer IDs to text
	if _, err := tx.Exec(`
		INSERT INTO messages_new (id, session_id, role, text, timestamp)
		SELECT CAST(id AS TEXT), session_id, role, text, timestamp FROM messages
	`); err != nil {
		return err
	}

	if _, err := tx.Exec(`DROP TABLE messages`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE messages_new RENAME TO messages`); err != nil {
		return err
	}

	return tx.Commit()
}

// ── Sessions ──────────────────────────────────────────────────────

func (s *SQLiteStore) SaveSession(id string, createdAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO sessions (id, created_at) VALUES (?, ?)`,
		id, createdAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (s *SQLiteStore) LoadSessions() ([]SessionRow, error) {
	rows, err := s.db.Query(`SELECT id, created_at FROM sessions ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		var createdAt string
		if err := rows.Scan(&r.ID, &createdAt); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── Messages ──────────────────────────────────────────────────────

func (s *SQLiteStore) SaveMessage(msg MessageRow) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO messages (id, session_id, role, text, timestamp) VALUES (?, ?, ?, ?, ?)`,
		msg.ID, msg.SessionID, msg.Role, msg.Text,
		msg.Timestamp.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (s *SQLiteStore) LoadMessages(sessionID string) ([]MessageRow, error) {
	rows, err := s.db.Query(
		`SELECT id, session_id, role, text, timestamp FROM messages WHERE session_id=? ORDER BY timestamp`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		var r MessageRow
		var ts string
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Role, &r.Text, &ts); err != nil {
			return nil, err
		}
		r.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── Workspaces ────────────────────────────────────────────────────

func (s *SQLiteStore) SaveWorkspace(ws WorkspaceRow) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO workspaces (id, session_id, type, title, content, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		ws.ID, ws.SessionID, ws.Type, ws.Title, ws.Content,
		ws.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (s *SQLiteStore) UpdateWorkspace(id, title, content string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var curTitle, curContent string
	err = tx.QueryRow(`SELECT title, content FROM workspaces WHERE id=?`, id).
		Scan(&curTitle, &curContent)
	if err != nil {
		return err
	}

	var nextVersion int
	tx.QueryRow(`SELECT COALESCE(MAX(version),0)+1 FROM workspace_history WHERE workspace_id=?`, id).
		Scan(&nextVersion)

	_, err = tx.Exec(
		`INSERT INTO workspace_history (workspace_id, version, title, content, saved_at) VALUES (?, ?, ?, ?, ?)`,
		id, nextVersion, curTitle, curContent, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return err
	}

	_, err = tx.Exec(`UPDATE workspaces SET title=?, content=? WHERE id=?`, title, content, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) CloseWorkspace(id string, closedAt time.Time) error {
	_, err := s.db.Exec(
		`UPDATE workspaces SET closed_at=? WHERE id=?`,
		closedAt.UTC().Format(time.RFC3339Nano), id,
	)
	return err
}

func (s *SQLiteStore) LoadWorkspaces() ([]WorkspaceRow, error) {
	rows, err := s.db.Query(
		`SELECT id, session_id, type, title, content, created_at, closed_at FROM workspaces ORDER BY created_at, rowid`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WorkspaceRow
	for rows.Next() {
		var r WorkspaceRow
		var createdAt string
		var closedAt sql.NullString
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Type, &r.Title, &r.Content, &createdAt, &closedAt); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		if closedAt.Valid {
			t, _ := time.Parse(time.RFC3339Nano, closedAt.String)
			r.ClosedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── History / Undo ────────────────────────────────────────────────

func (s *SQLiteStore) LoadHistory(workspaceID string) ([]HistoryRow, error) {
	rows, err := s.db.Query(
		`SELECT workspace_id, version, title, content, saved_at FROM workspace_history WHERE workspace_id=? ORDER BY version DESC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HistoryRow
	for rows.Next() {
		var r HistoryRow
		var savedAt string
		if err := rows.Scan(&r.WorkspaceID, &r.Version, &r.Title, &r.Content, &savedAt); err != nil {
			return nil, err
		}
		r.SavedAt, _ = time.Parse(time.RFC3339Nano, savedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetVersion(workspaceID string, version int) (*HistoryRow, error) {
	var r HistoryRow
	var savedAt string
	err := s.db.QueryRow(
		`SELECT workspace_id, version, title, content, saved_at FROM workspace_history WHERE workspace_id=? AND version=?`,
		workspaceID, version,
	).Scan(&r.WorkspaceID, &r.Version, &r.Title, &r.Content, &savedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("version %d not found for workspace %s", version, workspaceID)
	}
	if err != nil {
		return nil, err
	}
	r.SavedAt, _ = time.Parse(time.RFC3339Nano, savedAt)
	return &r, nil
}

func (s *SQLiteStore) Undo(workspaceID string) (*HistoryRow, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var r HistoryRow
	var savedAt string
	var rowID int64
	err = tx.QueryRow(
		`SELECT id, workspace_id, version, title, content, saved_at FROM workspace_history WHERE workspace_id=? ORDER BY version DESC LIMIT 1`,
		workspaceID,
	).Scan(&rowID, &r.WorkspaceID, &r.Version, &r.Title, &r.Content, &savedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no history for workspace %s", workspaceID)
	}
	if err != nil {
		return nil, err
	}
	r.SavedAt, _ = time.Parse(time.RFC3339Nano, savedAt)

	if _, err := tx.Exec(`UPDATE workspaces SET title=?, content=? WHERE id=?`, r.Title, r.Content, workspaceID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM workspace_history WHERE id=?`, rowID); err != nil {
		return nil, err
	}
	return &r, tx.Commit()
}

func (s *SQLiteStore) ApplyVersion(workspaceID string, version int) error {
	r, err := s.GetVersion(workspaceID, version)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE workspaces SET title=?, content=? WHERE id=?`, r.Title, r.Content, workspaceID)
	return err
}

// migrateAddOrchestration creates the orchestration_runs and orchestration_steps tables.
func (s *SQLiteStore) migrateAddOrchestration() error {
	_, err := s.db.Exec(`
	CREATE TABLE IF NOT EXISTS orchestration_runs (
		id          TEXT PRIMARY KEY,
		session_id  TEXT NOT NULL,
		instruction TEXT NOT NULL,
		summary     TEXT NOT NULL DEFAULT '',
		step_count  INTEGER NOT NULL DEFAULT 0,
		created_at  TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS orchestration_steps (
		id           TEXT PRIMARY KEY,
		run_id       TEXT NOT NULL REFERENCES orchestration_runs(id),
		step_number  INTEGER NOT NULL,
		workspace_id TEXT NOT NULL,
		instruction  TEXT NOT NULL,
		output       TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_orch_runs_session ON orchestration_runs(session_id);
	CREATE INDEX IF NOT EXISTS idx_orch_steps_run ON orchestration_steps(run_id);
	`)
	return err
}

func (s *SQLiteStore) SaveOrchestrationRun(run OrchestrationRunRow) error {
	status := run.Status
	if status == "" {
		status = OrchestrationCompleted
	}
	_, err := s.db.Exec(
		`INSERT INTO orchestration_runs (id, session_id, instruction, summary, step_count, status, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.SessionID, run.Instruction, run.Summary, run.StepCount, status, run.Error,
		run.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	)
	return err
}

func (s *SQLiteStore) UpdateOrchestrationRun(run OrchestrationRunRow) error {
	res, err := s.db.Exec(
		`UPDATE orchestration_runs SET summary = ?, step_count = ?, status = ?, error = ? WHERE id = ?`,
		run.Summary, run.StepCount, run.Status, run.Error, run.ID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("orchestration run %s not found", run.ID)
	}
	return nil
}

func (s *SQLiteStore) SaveOrchestrationStep(step OrchestrationStepRow) error {
	_, err := s.db.Exec(
		`INSERT INTO orchestration_steps (id, run_id, step_number, workspace_id, instruction, output, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		step.ID, step.RunID, step.StepNumber, step.WorkspaceID, step.Instruction, step.Output, step.Error,
	)
	return err
}

func (s *SQLiteStore) LoadOrchestrationRuns(sessionID string) ([]OrchestrationRunRow, error) {
	rows, err := s.db.Query(
		`SELECT id, session_id, instruction, summary, step_count, status, error, created_at
		 FROM orchestration_runs WHERE session_id = ? ORDER BY created_at ASC`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrchestrationRunRow
	for rows.Next() {
		var r OrchestrationRunRow
		var createdAt string
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Instruction, &r.Summary, &r.StepCount, &r.Status, &r.Error, &createdAt); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse("2006-01-02T15:04:05Z", createdAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) LoadOrchestrationSteps(runID string) ([]OrchestrationStepRow, error) {
	rows, err := s.db.Query(
		`SELECT id, run_id, step_number, workspace_id, instruction, output, error
		 FROM orchestration_steps WHERE run_id = ? ORDER BY step_number ASC`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrchestrationStepRow
	for rows.Next() {
		var r OrchestrationStepRow
		if err := rows.Scan(&r.ID, &r.RunID, &r.StepNumber, &r.WorkspaceID, &r.Instruction, &r.Output, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) Close() error { return s.db.Close() }
