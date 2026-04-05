// Package store defines the SessionStore interface and shared types.
// Concrete implementations: SQLiteStore (production), MemoryStore (tests).
package store

import "time"

// Session row as returned from the store.
type SessionRow struct {
	ID        string
	CreatedAt time.Time
}

// MessageRow as returned from the store.
type MessageRow struct {
	ID          string
	SessionID   string
	Role        string
	Text        string
	Timestamp   time.Time
}

// WorkspaceRow as returned from the store.
type WorkspaceRow struct {
	ID        string
	SessionID string
	Type      string
	Title     string
	Content   string
	CreatedAt time.Time
	ClosedAt  *time.Time
}

// HistoryRow is a versioned snapshot of workspace content.
type HistoryRow struct {
	WorkspaceID string
	Version     int
	Title       string
	Content     string
	SavedAt     time.Time
}

// Store is the persistence interface for CAS.
// Implemented by SQLiteStore and MemoryStore.
type Store interface {
	// Sessions
	SaveSession(id string, createdAt time.Time) error
	LoadSessions() ([]SessionRow, error)

	// Messages
	SaveMessage(msg MessageRow) error
	LoadMessages(sessionID string) ([]MessageRow, error)

	// Workspaces
	SaveWorkspace(ws WorkspaceRow) error
	UpdateWorkspace(id, title, content string) error
	CloseWorkspace(id string, closedAt time.Time) error
	LoadWorkspaces() ([]WorkspaceRow, error)

	// History / undo
	LoadHistory(workspaceID string) ([]HistoryRow, error)
	GetVersion(workspaceID string, version int) (*HistoryRow, error)
	Undo(workspaceID string) (*HistoryRow, error)
	ApplyVersion(workspaceID string, version int) error

	Close() error
}
