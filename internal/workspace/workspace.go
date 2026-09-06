// Package workspace manages the lifecycle of CAS workspaces.
// All mutations go through contract enforcement before touching the store.
package workspace

import (
	"fmt"
	"time"

	"github.com/goweft/cas/internal/contract"
	"github.com/goweft/cas/internal/store"
)

// Workspace is a single live workspace instance.
type Workspace struct {
	ID        string
	Type      string // "document" | "code" | "list" | "mcp" | "web"
	Title     string
	Content   string
	CreatedAt time.Time
	ClosedAt  *time.Time
	SessionID string
	// Connected is false for mcp/web workspaces restored without a live session.
	// Always true for document/code/list types.
	Connected bool

	// seq is the manager-local creation sequence. It, not CreatedAt, defines
	// ordering: wall clocks are too coarse on some platforms (Windows) to
	// separate back-to-back creates, and map iteration order is random.
	seq int64
}

func (w *Workspace) IsActive() bool { return w.ClosedAt == nil }

// ErrNotFound is returned when a workspace ID does not exist.
type ErrNotFound struct{ ID string }

func (e *ErrNotFound) Error() string { return fmt.Sprintf("workspace %q not found", e.ID) }

// ErrClosed is returned when an operation targets a closed workspace.
type ErrClosed struct{ ID string }

func (e *ErrClosed) Error() string { return fmt.Sprintf("workspace %q is closed", e.ID) }

// ErrNoHistory is returned when there is no history to undo.
type ErrNoHistory struct{ ID string }

func (e *ErrNoHistory) Error() string {
	return fmt.Sprintf("no history to undo for workspace %q", e.ID)
}

// Manager holds all live workspace state and mediates store access.
type Manager struct {
	store      store.Store
	workspaces map[string]*Workspace
	nextSeq    int64
}

// NewManager returns a Manager backed by the given store.
func NewManager(s store.Store) *Manager {
	return &Manager{
		store:      s,
		workspaces: make(map[string]*Workspace),
	}
}

// Restore loads persisted workspaces from the store into memory.
func (m *Manager) Restore() error {
	rows, err := m.store.LoadWorkspaces()
	if err != nil {
		return err
	}
	for _, row := range rows {
		ws := &Workspace{
			ID:        row.ID,
			Type:      row.Type,
			Title:     row.Title,
			Content:   row.Content,
			CreatedAt: row.CreatedAt,
			ClosedAt:  row.ClosedAt,
			SessionID: row.SessionID,
			// mcp and web workspaces restored from disk have no live session.
			Connected: row.Type != "mcp" && row.Type != "web",
		}
		ws.seq = m.nextSeq
		m.nextSeq++
		m.workspaces[ws.ID] = ws
	}
	return nil
}

// Create makes a new workspace, enforcing the contract before writing to store.
func (m *Manager) Create(id, wsType, title, content, sessionID string) (*Workspace, error) {
	// All freshly created workspaces start connected.
	c := contract.DefaultWorkspaceContract(wsType, len(content))
	if err := c.CheckPreconditions(); err != nil {
		return nil, fmt.Errorf("create blocked by contract: %w", err)
	}

	ws := &Workspace{
		ID:        id,
		Type:      wsType,
		Title:     title,
		Content:   content,
		CreatedAt: time.Now().UTC(),
		SessionID: sessionID,
		Connected: true,
	}

	if err := c.CheckPostconditions(); err != nil {
		return nil, fmt.Errorf("create failed postcondition: %w", err)
	}

	if err := m.store.SaveWorkspace(store.WorkspaceRow{
		ID:        ws.ID,
		SessionID: ws.SessionID,
		Type:      ws.Type,
		Title:     ws.Title,
		Content:   ws.Content,
		CreatedAt: ws.CreatedAt,
	}); err != nil {
		return nil, err
	}

	ws.seq = m.nextSeq
	m.nextSeq++
	m.workspaces[ws.ID] = ws
	return ws, nil
}

// Update modifies the title and/or content of an active workspace.
//
// Ordering matters: the new values are validated against the contract and
// persisted BEFORE they are assigned to the in-memory workspace. A rejected
// or unpersisted update therefore leaves memory identical to the store —
// the workspace never holds content the contract refused or the database
// never saw.
func (m *Manager) Update(id, title, content string) (*Workspace, error) {
	ws, err := m.get(id)
	if err != nil {
		return nil, err
	}
	if !ws.IsActive() {
		return nil, &ErrClosed{ID: id}
	}

	c := contract.DefaultWorkspaceContract(ws.Type, len(content))
	if err := c.CheckPreconditions(); err != nil {
		return nil, fmt.Errorf("update blocked by contract: %w", err)
	}

	newTitle, newContent := ws.Title, ws.Content
	if title != "" {
		newTitle = title
	}
	if content != "" {
		newContent = content
	}

	if err := c.CheckPostconditions(); err != nil {
		return nil, fmt.Errorf("update failed postcondition: %w", err)
	}

	if err := m.store.UpdateWorkspace(id, newTitle, newContent); err != nil {
		return nil, err
	}

	ws.Title = newTitle
	ws.Content = newContent
	return ws, nil
}

// Undo restores the active workspace to its previous version.
// Returns ErrNoHistory if there is nothing to undo, ErrNotFound if the
// workspace does not exist, or ErrClosed if it is no longer active.
func (m *Manager) Undo(id string) (*Workspace, error) {
	ws, err := m.get(id)
	if err != nil {
		return nil, err
	}
	if !ws.IsActive() {
		return nil, &ErrClosed{ID: id}
	}

	restored, err := m.store.Undo(id)
	if err != nil {
		return nil, &ErrNoHistory{ID: id}
	}

	ws.Title = restored.Title
	ws.Content = restored.Content
	return ws, nil
}

// Close marks a workspace as closed.
func (m *Manager) Close(id string) (*Workspace, error) {
	ws, err := m.get(id)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	ws.ClosedAt = &now
	if err := m.store.CloseWorkspace(id, now); err != nil {
		return nil, err
	}
	return ws, nil
}

// Get returns a workspace by ID.
func (m *Manager) Get(id string) (*Workspace, error) { return m.get(id) }

func (m *Manager) get(id string) (*Workspace, error) {
	ws, ok := m.workspaces[id]
	if !ok {
		return nil, &ErrNotFound{ID: id}
	}
	return ws, nil
}

// Active returns all non-closed workspaces in creation order.
func (m *Manager) Active() []*Workspace {
	var out []*Workspace
	for _, ws := range m.workspaces {
		if ws.IsActive() {
			out = append(out, ws)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].seq < out[j-1].seq; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// All returns every workspace including closed ones.
func (m *Manager) All() []*Workspace {
	out := make([]*Workspace, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		out = append(out, ws)
	}
	return out
}
