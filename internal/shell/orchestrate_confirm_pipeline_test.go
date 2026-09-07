package shell_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goweft/cas/internal/shell"
	"github.com/goweft/cas/internal/store"
)

// ── Confirm-mode orchestration stays in the message pipeline ─────────
//
// The TUI's confirm dial calls OrchestrateConfirm directly rather than
// ProcessMessage. Before the fix that path fabricated a bare Session{ID},
// so the user turn and the reply never reached the session history, the
// message store, or the conductor — later chats in the same session had no
// record the orchestration ever happened.

func newShellWithProfile(t *testing.T) (*shell.Shell, *store.MemoryStore, string) {
	t.Helper()
	s := store.NewMemoryStore()
	profile := filepath.Join(t.TempDir(), "profile.json")
	return shell.NewShell(s, profile), s, profile
}

func conductorMessageCount(t *testing.T, profilePath string) int {
	t.Helper()
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		return 0
	}
	var p struct {
		MessageCount int `json:"message_count"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("profile.json: %v", err)
	}
	return p.MessageCount
}

// TestOrchestrateConfirmPersistsTurn: a confirm-mode orchestration that
// completes must leave both turns in the session, both in the store, and
// one observation with the conductor — identical to the router path.
// Uses a lone document workspace so runOrchestration takes its chat
// fallback (the plan-echo fake answers "ok"); the persistence contract is
// the same on either branch and this one completes offline.
func TestOrchestrateConfirmPersistsTurn(t *testing.T) {
	fake := &planEchoLLM{wsID: "unused"}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	t.Setenv("CAS_PROVIDER", "ollama")
	t.Setenv("OLLAMA_BASE_URL", srv.URL)

	sh, s, profile := newShellWithProfile(t)
	sess, _ := sh.CreateSession()
	if _, err := sh.Workspaces().Create("doc-1", "document", "Notes", "# Notes", sess.ID); err != nil {
		t.Fatal(err)
	}
	before := conductorMessageCount(t, profile)

	msg := "use the notes to draft a summary"
	resp, err := sh.OrchestrateConfirm(context.Background(), sess.ID, msg, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.ChatReply == "" {
		t.Fatalf("expected a reply, got %+v", resp)
	}

	// Session history: the live session object the router path also uses.
	live, err := sh.GetSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.History) != 2 || live.History[0].Role != "user" || live.History[0].Text != msg || live.History[1].Role != "shell" {
		t.Errorf("session history not updated: %+v", live.History)
	}

	// Store: both turns persisted.
	rows, err := s.LoadMessages(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 persisted messages, got %d", len(rows))
	}
	if rows[0].Role != "user" || rows[0].Text != msg || rows[1].Role != "shell" || rows[1].Text != resp.ChatReply {
		t.Errorf("persisted turns wrong: %+v", rows)
	}

	// Conductor observed exactly one message.
	if got := conductorMessageCount(t, profile); got != before+1 {
		t.Errorf("conductor message_count = %d, want %d", got, before+1)
	}
}

// TestOrchestrateConfirmUnknownSessionRefused: the old path accepted any
// session ID because it never looked the session up. It must now fail the
// same way ProcessMessage does.
func TestOrchestrateConfirmUnknownSessionRefused(t *testing.T) {
	sh, _ := newShell(t)
	_, err := sh.OrchestrateConfirm(context.Background(), "no-such-session", "use it", func(string) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected session-not-found, got: %v", err)
	}
}

// TestOrchestrateConfirmFailedRunStillRecordsUserTurn: parity with the
// router — the user turn is saved before dispatch, so a run that fails at
// a step still leaves the user's message in history and store.
func TestOrchestrateConfirmFailedRunStillRecordsUserTurn(t *testing.T) {
	fake := &planEchoLLM{wsID: "test-mcp-ws-1"}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	t.Setenv("CAS_PROVIDER", "ollama")
	t.Setenv("OLLAMA_BASE_URL", srv.URL)

	sh, s := newShell(t)
	sess, _ := sh.CreateSession()
	if _, err := sh.Workspaces().Create("test-mcp-ws-1", "mcp", "MCP: http://fake", "tools", sess.ID); err != nil {
		t.Fatal(err)
	}

	msg := "use this server to list the available tools"
	_, err := sh.OrchestrateConfirm(context.Background(), sess.ID, msg, func(string) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "no MCP connection") {
		t.Fatalf("expected failure at the connection layer, got: %v", err)
	}
	rows, _ := s.LoadMessages(sess.ID)
	if len(rows) != 1 || rows[0].Role != "user" || rows[0].Text != msg {
		t.Errorf("user turn not persisted before dispatch: %+v", rows)
	}
	// And the audit record from finding 2 is still present on this path.
	runs, _ := s.LoadOrchestrationRuns(sess.ID)
	if len(runs) != 1 || runs[0].Status != store.OrchestrationFailed {
		t.Errorf("failed confirm-mode run not audited: %+v", runs)
	}
}
