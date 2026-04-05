package shell_test

import (
	"context"
	"os"
	"testing"

	"github.com/goweft/cas/internal/intent"
	"github.com/goweft/cas/internal/shell"
	"github.com/goweft/cas/internal/store"
)

func newShell() (*shell.Shell, *store.MemoryStore) {
	s := store.NewMemoryStore()
	sh := shell.NewShell(s)
	return sh, s
}

func TestCreateSession(t *testing.T) {
	sh, _ := newShell()
	sess, err := sh.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == "" {
		t.Error("expected non-empty session ID")
	}
}

func TestGetSession(t *testing.T) {
	sh, _ := newShell()
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
	sh, _ := newShell()
	_, err := sh.GetSession("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestLatestSession(t *testing.T) {
	sh, _ := newShell()
	if sh.LatestSession() != nil {
		t.Error("expected nil when no sessions")
	}
	s1, _ := sh.CreateSession()
	s2, _ := sh.CreateSession()
	latest := sh.LatestSession()
	if latest.ID != s2.ID && latest.ID != s1.ID {
		t.Error("latest session should be one of the created sessions")
	}
}

func TestDetectIntentInShell(t *testing.T) {
	cases := []struct {
		msg  string
		kind intent.Kind
	}{
		{"write a project proposal", intent.KindCreate},
		{"add a section about costs", intent.KindEdit},
		{"close the workspace", intent.KindClose},
		{"hello", intent.KindChat},
		{"edit it directly", intent.KindChat},
	}
	for _, tc := range cases {
		got := intent.Detect(tc.msg)
		if got.Kind != tc.kind {
			t.Errorf("msg=%q: expected %q got %q", tc.msg, tc.kind, got.Kind)
		}
	}
}

func TestRestorePreservesSessionsAndWorkspaces(t *testing.T) {
	s := store.NewMemoryStore()
	sh1 := shell.NewShell(s)
	sess, _ := sh1.CreateSession()

	sh2 := shell.NewShell(s)
	if err := sh2.Restore(); err != nil {
		t.Fatal(err)
	}
	_, err := sh2.GetSession(sess.ID)
	if err != nil {
		t.Errorf("session not restored: %v", err)
	}
}

// TestStreamMessageIntegration is an LLM integration test.
// Run with: CAS_INTEGRATION=1 go test ./internal/shell/...
func TestStreamMessageIntegration(t *testing.T) {
	if os.Getenv("CAS_INTEGRATION") != "1" {
		t.Skip("set CAS_INTEGRATION=1 to run LLM integration tests")
	}
	sh, _ := newShell()
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
