package store_test

import (
	"testing"
	"time"

	"github.com/goweft/cas/internal/store"
)

func TestMemoryStoreSaveLoadSession(t *testing.T) {
	s := store.NewMemoryStore()
	now := time.Now().UTC()
	if err := s.SaveSession("ses_001", now); err != nil {
		t.Fatal(err)
	}
	rows, err := s.LoadSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "ses_001" {
		t.Errorf("expected 1 session ses_001, got %v", rows)
	}
}

func TestMemoryStoreSaveLoadMessages(t *testing.T) {
	s := store.NewMemoryStore()
	s.SaveSession("ses_001", time.Now().UTC())
	msg := store.MessageRow{
		ID: "m1", SessionID: "ses_001",
		Role: "user", Text: "hello", Timestamp: time.Now().UTC(),
	}
	if err := s.SaveMessage(msg); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.LoadMessages("ses_001")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hello" {
		t.Errorf("unexpected messages: %v", msgs)
	}
}

func TestMemoryStoreWorkspaceLifecycle(t *testing.T) {
	s := store.NewMemoryStore()
	ws := store.WorkspaceRow{
		ID: "ws_001", SessionID: "ses_001",
		Type: "document", Title: "Test Doc",
		Content: "# Test", CreatedAt: time.Now().UTC(),
	}
	if err := s.SaveWorkspace(ws); err != nil {
		t.Fatal(err)
	}

	// Update
	if err := s.UpdateWorkspace("ws_001", "Test Doc", "# Updated"); err != nil {
		t.Fatal(err)
	}

	rows, err := s.LoadWorkspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Content != "# Updated" {
		t.Errorf("expected updated content, got %v", rows)
	}

	// History should have one entry (the pre-update snapshot)
	hist, err := s.LoadHistory("ws_001")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(hist))
	}
}

func TestMemoryStoreUndo(t *testing.T) {
	s := store.NewMemoryStore()
	s.SaveWorkspace(store.WorkspaceRow{
		ID: "ws_001", Type: "document", Title: "Doc", Content: "v1",
	})
	s.UpdateWorkspace("ws_001", "Doc", "v2")
	s.UpdateWorkspace("ws_001", "Doc", "v3")

	restored, err := s.Undo("ws_001")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Content != "v2" {
		t.Errorf("expected undo to restore v2, got %q", restored.Content)
	}
}

func TestMemoryStoreClose(t *testing.T) {
	s := store.NewMemoryStore()
	s.SaveWorkspace(store.WorkspaceRow{ID: "ws_001", Type: "document", Title: "Doc", Content: "x"})
	if err := s.CloseWorkspace("ws_001", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.LoadWorkspaces()
	if rows[0].ClosedAt == nil {
		t.Error("expected ClosedAt to be set")
	}
}
