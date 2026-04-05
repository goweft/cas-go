// MemoryStore is an in-memory Store implementation for tests.
// No SQLite, no filesystem, no side effects.
package store

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

type MemoryStore struct {
	mu         sync.Mutex
	sessions   []SessionRow
	messages   []MessageRow
	workspaces map[string]*WorkspaceRow
	history    map[string][]HistoryRow // workspaceID → versions
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		workspaces: make(map[string]*WorkspaceRow),
		history:    make(map[string][]HistoryRow),
	}
}

func (m *MemoryStore) SaveSession(id string, createdAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = append(m.sessions, SessionRow{ID: id, CreatedAt: createdAt})
	return nil
}

func (m *MemoryStore) LoadSessions() ([]SessionRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionRow, len(m.sessions))
	copy(out, m.sessions)
	return out, nil
}

func (m *MemoryStore) SaveMessage(msg MessageRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, msg)
	return nil
}

func (m *MemoryStore) LoadMessages(sessionID string) ([]MessageRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MessageRow
	for _, msg := range m.messages {
		if msg.SessionID == sessionID {
			out = append(out, msg)
		}
	}
	return out, nil
}

func (m *MemoryStore) SaveWorkspace(ws WorkspaceRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := ws
	m.workspaces[ws.ID] = &cp
	return nil
}

func (m *MemoryStore) UpdateWorkspace(id, title, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.workspaces[id]
	if !ok {
		return fmt.Errorf("workspace %s not found", id)
	}
	// Snapshot before update
	versions := m.history[id]
	nextVersion := len(versions) + 1
	m.history[id] = append(versions, HistoryRow{
		WorkspaceID: id,
		Version:     nextVersion,
		Title:       ws.Title,
		Content:     ws.Content,
		SavedAt:     time.Now().UTC(),
	})
	ws.Title = title
	ws.Content = content
	return nil
}

func (m *MemoryStore) CloseWorkspace(id string, closedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.workspaces[id]
	if !ok {
		return fmt.Errorf("workspace %s not found", id)
	}
	ws.ClosedAt = &closedAt
	return nil
}

func (m *MemoryStore) LoadWorkspaces() ([]WorkspaceRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]WorkspaceRow, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		out = append(out, *ws)
	}
	return out, nil
}

func (m *MemoryStore) LoadHistory(workspaceID string) ([]HistoryRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	versions := m.history[workspaceID]
	out := make([]HistoryRow, len(versions))
	copy(out, versions)
	return out, nil
}

func (m *MemoryStore) GetVersion(workspaceID string, version int) (*HistoryRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.history[workspaceID] {
		if h.Version == version {
			cp := h
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("version %d not found for workspace %s", version, workspaceID)
}

func (m *MemoryStore) Undo(workspaceID string) (*HistoryRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	versions := m.history[workspaceID]
	if len(versions) == 0 {
		return nil, fmt.Errorf("no history for workspace %s", workspaceID)
	}
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Version > versions[j].Version
	})
	latest := versions[0]
	m.history[workspaceID] = versions[1:]
	// Restore workspace content
	if ws, ok := m.workspaces[workspaceID]; ok {
		ws.Title = latest.Title
		ws.Content = latest.Content
	}
	return &latest, nil
}

func (m *MemoryStore) ApplyVersion(workspaceID string, version int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.history[workspaceID] {
		if h.Version == version {
			if ws, ok := m.workspaces[workspaceID]; ok {
				ws.Title = h.Title
				ws.Content = h.Content
			}
			return nil
		}
	}
	return fmt.Errorf("version %d not found for workspace %s", version, workspaceID)
}

func (m *MemoryStore) Close() error { return nil }
