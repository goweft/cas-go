package workspace_test

import (
	"testing"

	"github.com/goweft/cas/internal/store"
	"github.com/goweft/cas/internal/workspace"
)

func newManager() *workspace.Manager {
	return workspace.NewManager(store.NewMemoryStore())
}

func TestCreateWorkspace(t *testing.T) {
	m := newManager()
	ws, err := m.Create("ws1", "document", "Test Doc", "# Test", "ses1")
	if err != nil {
		t.Fatal(err)
	}
	if ws.ID != "ws1" || ws.Type != "document" || ws.Title != "Test Doc" {
		t.Errorf("unexpected workspace: %+v", ws)
	}
	if !ws.IsActive() {
		t.Error("expected workspace to be active")
	}
}

func TestCreateRejectsUnknownType(t *testing.T) {
	m := newManager()
	_, err := m.Create("ws1", "spreadsheet", "Bad", "content", "ses1")
	if err == nil {
		t.Error("expected error for unknown workspace type")
	}
}

func TestUpdateWorkspace(t *testing.T) {
	m := newManager()
	m.Create("ws1", "document", "Original", "# Original", "ses1")
	ws, err := m.Update("ws1", "Updated", "# Updated content")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Title != "Updated" || ws.Content != "# Updated content" {
		t.Errorf("unexpected update result: %+v", ws)
	}
}

func TestUpdateNotFound(t *testing.T) {
	m := newManager()
	_, err := m.Update("nonexistent", "title", "content")
	if err == nil {
		t.Error("expected error for nonexistent workspace")
	}
}

func TestCloseWorkspace(t *testing.T) {
	m := newManager()
	m.Create("ws1", "document", "Doc", "content", "ses1")
	ws, err := m.Close("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if ws.IsActive() {
		t.Error("expected workspace to be closed")
	}
}

func TestActiveFiltersClosedWorkspaces(t *testing.T) {
	m := newManager()
	m.Create("ws1", "document", "Doc1", "c1", "ses1")
	m.Create("ws2", "code", "Doc2", "c2", "ses1")
	m.Close("ws1")

	active := m.Active()
	if len(active) != 1 || active[0].ID != "ws2" {
		t.Errorf("expected 1 active workspace ws2, got %v", active)
	}
}

func TestActiveOrdering(t *testing.T) {
	m := newManager()
	m.Create("ws1", "document", "First", "c1", "ses1")
	m.Create("ws2", "document", "Second", "c2", "ses1")
	m.Create("ws3", "list", "Third", "c3", "ses1")

	active := m.Active()
	if len(active) != 3 {
		t.Fatalf("expected 3, got %d", len(active))
	}
	if active[0].ID != "ws1" || active[2].ID != "ws3" {
		t.Errorf("wrong order: %v", active)
	}
}

func TestRestoreWorkspaces(t *testing.T) {
	s := store.NewMemoryStore()
	m1 := workspace.NewManager(s)
	m1.Create("ws1", "document", "Persisted", "# Hello", "ses1")

	// New manager, same store — simulates restart
	m2 := workspace.NewManager(s)
	if err := m2.Restore(); err != nil {
		t.Fatal(err)
	}
	ws, err := m2.Get("ws1")
	if err != nil {
		t.Fatalf("workspace not restored: %v", err)
	}
	if ws.Title != "Persisted" {
		t.Errorf("unexpected title: %s", ws.Title)
	}
}
