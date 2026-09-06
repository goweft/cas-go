package workspace_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/goweft/cas/internal/store"
	"github.com/goweft/cas/internal/workspace"
)

// failingUpdateStore is a Store whose UpdateWorkspace always fails, standing
// in for a database error after the contract has already passed.
type failingUpdateStore struct {
	store.Store
}

var errStoreDown = errors.New("store: update failed")

func (f *failingUpdateStore) UpdateWorkspace(id, title, content string) error {
	return errStoreDown
}

// TestUpdateRejectedByPostconditionLeavesMemoryUntouched: an oversized
// update must fail the content_size_within_limit postcondition AND leave
// the in-memory workspace exactly as it was. Before the fix, Update
// assigned the new title/content first and checked the contract second,
// so a rejected update stayed live in memory while the store still held
// the old version.
func TestUpdateRejectedByPostconditionLeavesMemoryUntouched(t *testing.T) {
	m := newManager()
	if _, err := m.Create("ws1", "document", "Original", "# Original", "ses1"); err != nil {
		t.Fatal(err)
	}

	oversized := strings.Repeat("x", 512*1024+1)
	_, err := m.Update("ws1", "Renamed", oversized)
	if err == nil {
		t.Fatal("expected postcondition failure for oversized content")
	}
	if !strings.Contains(err.Error(), "postcondition") {
		t.Fatalf("expected a postcondition error, got: %v", err)
	}

	ws, err := m.Get("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Title != "Original" {
		t.Errorf("rejected update mutated Title: got %q, want %q", ws.Title, "Original")
	}
	if ws.Content != "# Original" {
		t.Errorf("rejected update mutated Content: got %d bytes, want %q", len(ws.Content), "# Original")
	}
}

// TestUpdateStoreFailureLeavesMemoryUntouched: if the contract passes but
// persistence fails, memory must still match the store — the old content.
func TestUpdateStoreFailureLeavesMemoryUntouched(t *testing.T) {
	m := workspace.NewManager(&failingUpdateStore{Store: store.NewMemoryStore()})
	if _, err := m.Create("ws1", "document", "Original", "# Original", "ses1"); err != nil {
		t.Fatal(err)
	}

	_, err := m.Update("ws1", "Renamed", "# Replaced")
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("expected store error, got: %v", err)
	}

	ws, err := m.Get("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Title != "Original" || ws.Content != "# Original" {
		t.Errorf("failed persistence mutated memory: title=%q content=%q", ws.Title, ws.Content)
	}
}

// Positive control: a valid update still lands in both memory and store.
func TestUpdateAcceptedLandsInMemoryAndStore(t *testing.T) {
	s := store.NewMemoryStore()
	m := workspace.NewManager(s)
	if _, err := m.Create("ws1", "document", "Original", "# Original", "ses1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update("ws1", "Renamed", "# Replaced"); err != nil {
		t.Fatal(err)
	}
	ws, _ := m.Get("ws1")
	if ws.Title != "Renamed" || ws.Content != "# Replaced" {
		t.Errorf("accepted update not in memory: %+v", ws)
	}
	rows, err := s.LoadWorkspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Content != "# Replaced" || rows[0].Title != "Renamed" {
		t.Errorf("accepted update not in store: %+v", rows)
	}
}
