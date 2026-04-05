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

	// WAL mode for concurrent reads + single writer
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		return nil, fmt.Errorf("sqlite: WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		return nil, fmt.Errorf("sqlite: fk: %w", err)
	}

	s := &SQLiteStore{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("sqlite: migrate: %w", err)
	}
	return s, nil
}

func (s *SQLiteStore) migrate() error {
	_, err := s.db.Exec(`
	CREATE TABLE IF NOT EXISTS sessions (
		id         TEXT PRIMARY KEY,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS messages (
		id         TEXT PRIMARY KEY,
		session_id TEXT NOT NULL REFERENCES sessions(id),
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
		workspace_id TEXT NOT NULL REFERENCES workspaces(id),
		version      INTEGER NOT NULL,
		title        TEXT NOT NULL,
		content      TEXT NOT NULL,
		saved_at     TEXT NOT NULL
	);
	`)
	return err
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

	// Snapshot current state before overwriting
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
		`SELECT id, session_id, type, title, content, created_at, closed_at FROM workspaces ORDER BY created_at`,
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

	// Get latest snapshot
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

	// Restore workspace to snapshot
	if _, err := tx.Exec(`UPDATE workspaces SET title=?, content=? WHERE id=?`, r.Title, r.Content, workspaceID); err != nil {
		return nil, err
	}
	// Remove the used snapshot
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

func (s *SQLiteStore) Close() error { return s.db.Close() }
