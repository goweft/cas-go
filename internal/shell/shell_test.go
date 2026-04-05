package shell_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goweft/cas/internal/intent"
	"github.com/goweft/cas/internal/shell"
	"github.com/goweft/cas/internal/store"
)

func newShell(t *testing.T) (*shell.Shell, *store.MemoryStore) {
	t.Helper()
	s := store.NewMemoryStore()
	// Conductor writes to a temp file so tests never touch ~/.cas/profile.json
	conductorPath := filepath.Join(t.TempDir(), "profile.json")
	sh := shell.NewShell(s, conductorPath)
	return sh, s
}

// ── Session management ────────────────────────────────────────────

func TestCreateSession(t *testing.T) {
	sh, _ := newShell(t)
	sess, err := sh.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == "" {
		t.Error("expected non-empty session ID")
	}
}

func TestCreateSessionPersistsToStore(t *testing.T) {
	sh, s := newShell(t)
	sess, err := sh.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.LoadSessions()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.ID == sess.ID {
			found = true
			break
		}
	}
	if !found {
		t.Error("session not found in store after CreateSession")
	}
}

func TestGetSession(t *testing.T) {
	sh, _ := newShell(t)
	sess, _ := sh.CreateSession()
	got, err := sh.GetSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sess.ID {
		t.Errorf("session ID mismatch: %s != %s", got.ID, sess.ID)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	sh, _ := newShell(t)
	_, err := sh.GetSession("nonexistent-id")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestLatestSessionNilWhenNone(t *testing.T) {
	sh, _ := newShell(t)
	if sh.LatestSession() != nil {
		t.Error("expected nil when no sessions exist")
	}
}

func TestLatestSessionReturnsMostRecent(t *testing.T) {
	sh, _ := newShell(t)
	s1, _ := sh.CreateSession()
	s2, _ := sh.CreateSession()
	latest := sh.LatestSession()
	// Latest should be s2 (created after s1)
	if latest.ID != s2.ID && latest.ID != s1.ID {
		t.Error("latest session should be one of the created sessions")
	}
	// Specifically it should not be s1 if s2 is newer
	if s2.CreatedAt.After(s1.CreatedAt) && latest.ID != s2.ID {
		t.Errorf("expected s2 as latest, got %s", latest.ID)
	}
}

// ── Intent routing (no LLM) ───────────────────────────────────────

func TestDetectIntentRouting(t *testing.T) {
	cases := []struct {
		msg  string
		kind intent.Kind
	}{
		{"write a project proposal", intent.KindCreate},
		{"add error handling", intent.KindEdit},
		{"add a validation step", intent.KindEdit},
		{"fix the error messages", intent.KindEdit},
		{"close the workspace", intent.KindClose},
		{"hello", intent.KindChat},
		{"edit it directly", intent.KindChat}, // self-edit → chat
	}
	for _, tc := range cases {
		got := intent.Detect(tc.msg)
		if got.Kind != tc.kind {
			t.Errorf("msg=%q: expected %q got %q", tc.msg, tc.kind, got.Kind)
		}
	}
}

// ── Error paths (no LLM required) ────────────────────────────────

func TestEditWithNoActiveWorkspace(t *testing.T) {
	sh, _ := newShell(t)
	sess, _ := sh.CreateSession()
	// No workspace exists — edit should return graceful reply, not error
	resp, err := sh.ProcessMessage(context.Background(), sess.ID, "rewrite the introduction")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	// Should tell user there's nothing to edit
	if resp.ChatReply == "" {
		t.Error("expected non-empty chat reply")
	}
}

func TestCloseWithNoActiveWorkspace(t *testing.T) {
	sh, _ := newShell(t)
	sess, _ := sh.CreateSession()
	resp, err := sh.ProcessMessage(context.Background(), sess.ID, "close the workspace")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.ChatReply == "" {
		t.Error("expected non-empty chat reply for close with no workspace")
	}
}

func TestProcessMessageUnknownSession(t *testing.T) {
	sh, _ := newShell(t)
	_, err := sh.ProcessMessage(context.Background(), "bad-session-id", "hello")
	if err == nil {
		t.Error("expected error for unknown session ID")
	}
}

// ── Restore ───────────────────────────────────────────────────────

func TestRestorePreservesSessionsAndWorkspaces(t *testing.T) {
	s := store.NewMemoryStore()
	conductorPath := filepath.Join(t.TempDir(), "profile.json")

	sh1 := shell.NewShell(s, conductorPath)
	sess, _ := sh1.CreateSession()

	sh2 := shell.NewShell(s, conductorPath)
	if err := sh2.Restore(); err != nil {
		t.Fatal(err)
	}
	_, err := sh2.GetSession(sess.ID)
	if err != nil {
		t.Errorf("session not restored: %v", err)
	}
}

// ── Conductor integration ─────────────────────────────────────────

func TestConductorSessionCountIncrements(t *testing.T) {
	sh, _ := newShell(t)
	sh.CreateSession()
	sh.CreateSession()
	summary := sh.ProfileSummary()
	count, ok := summary["session_count"].(int)
	if !ok {
		t.Fatalf("session_count not an int: %T", summary["session_count"])
	}
	if count < 2 {
		t.Errorf("expected session_count >= 2, got %d", count)
	}
}

func TestUserContextEmptyInitially(t *testing.T) {
	sh, _ := newShell(t)
	ctx := sh.UserContext()
	if ctx != "" {
		t.Errorf("expected empty UserContext with no observations, got %q", ctx)
	}
}

// ── LLM integration (skipped unless CAS_INTEGRATION=1) ───────────

func TestStreamMessageIntegration(t *testing.T) {
	if os.Getenv("CAS_INTEGRATION") != "1" {
		t.Skip("set CAS_INTEGRATION=1 to run LLM integration tests")
	}
	sh, _ := newShell(t)
	sess, _ := sh.CreateSession()
	ctx := context.Background()

	var tokens []string
	resp, err := sh.StreamMessage(ctx, sess.ID, "hello", func(tok string) {
		tokens = append(tokens, tok)
	})
	if err != nil {
		t.Fatalf("StreamMessage error: %v", err)
	}
	if resp == nil {
		t.Error("expected non-nil response")
	}
	if resp.ChatReply == "" {
		t.Error("expected non-empty ChatReply")
	}
}
