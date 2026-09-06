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
	ID        string
	SessionID string
	Role      string
	Text      string
	Timestamp time.Time
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

// Orchestration run lifecycle. A run row is written with StatusRunning
// BEFORE any step executes, so a crash or a failing step can never leave
// step rows without a parent. It is finalized to Completed or Failed
// afterwards; a row still in Running after the process is gone is itself
// the audit signal that the run was interrupted.
const (
	OrchestrationRunning   = "running"
	OrchestrationCompleted = "completed"
	OrchestrationFailed    = "failed"
)

// OrchestrationRunRow records an orchestration task through its lifecycle.
type OrchestrationRunRow struct {
	ID          string
	SessionID   string
	Instruction string
	Summary     string
	StepCount   int // steps attempted (including a failed one), not steps planned
	Status      string
	Error       string // populated when Status == OrchestrationFailed
	CreatedAt   time.Time
}

// OrchestrationStepRow records a single step within an orchestration run.
// A failed step is recorded with Error set and Output empty.
type OrchestrationStepRow struct {
	ID          string
	RunID       string
	StepNumber  int
	WorkspaceID string
	Instruction string
	Output      string
	Error       string
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

	// Orchestration runs. SaveOrchestrationRun inserts the row before any
	// step runs; UpdateOrchestrationRun finalizes summary/step_count/status/
	// error on the existing row (by ID).
	SaveOrchestrationRun(run OrchestrationRunRow) error
	UpdateOrchestrationRun(run OrchestrationRunRow) error
	SaveOrchestrationStep(step OrchestrationStepRow) error
	LoadOrchestrationRuns(sessionID string) ([]OrchestrationRunRow, error)
	LoadOrchestrationSteps(runID string) ([]OrchestrationStepRow, error)

	Close() error
}
