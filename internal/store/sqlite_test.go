package store_test

import (
	"os"
	"testing"
	"time"

	"github.com/goweft/cas/internal/store"
)

func TestSQLiteStoreRoundtrip(t *testing.T) {
	f, err := os.CreateTemp("", "cas-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	s, err := store.NewSQLiteStore(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Session
	if err := s.SaveSession("ses_001", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	sessions, err := s.LoadSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "ses_001" {
		t.Errorf("unexpected sessions: %v", sessions)
	}

	// Message
	if err := s.SaveMessage(store.MessageRow{
		ID: "m1", SessionID: "ses_001",
		Role: "user", Text: "hello", Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.LoadMessages("ses_001")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hello" {
		t.Errorf("unexpected messages: %v", msgs)
	}

	// Workspace lifecycle
	if err := s.SaveWorkspace(store.WorkspaceRow{
		ID: "ws_001", SessionID: "ses_001",
		Type: "document", Title: "Test", Content: "v1",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWorkspace("ws_001", "Test", "v2"); err != nil {
		t.Fatal(err)
	}
	wss, err := s.LoadWorkspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(wss) != 1 || wss[0].Content != "v2" {
		t.Errorf("unexpected workspaces: %v", wss)
	}

	// History
	hist, err := s.LoadHistory("ws_001")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Content != "v1" {
		t.Errorf("unexpected history: %v", hist)
	}

	// Undo
	restored, err := s.Undo("ws_001")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Content != "v1" {
		t.Errorf("undo: expected v1, got %q", restored.Content)
	}

	// Close workspace
	if err := s.CloseWorkspace("ws_001", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	wss, _ = s.LoadWorkspaces()
	if wss[0].ClosedAt == nil {
		t.Error("expected ClosedAt to be set after close")
	}
}

func TestSQLiteStoreImplementsInterface(t *testing.T) {
	f, _ := os.CreateTemp("", "cas-iface-*.db")
	f.Close()
	defer os.Remove(f.Name())

	s, err := store.NewSQLiteStore(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Compile-time check — if SQLiteStore doesn't implement Store, this fails
	var _ store.Store = s
}
