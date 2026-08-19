package shell_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// ── Run workspace ─────────────────────────────────────────────────

func TestRunNoActiveWorkspace(t *testing.T) {
	sh, _ := newShell(t)
	sess, _ := sh.CreateSession()
	resp, err := sh.ProcessMessage(context.Background(), sess.ID, "run it")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Intent != intent.KindRun {
		t.Errorf("expected KindRun, got %q", resp.Intent)
	}
	if resp.ChatReply == "" {
		t.Error("expected non-empty reply explaining no workspace")
	}
}

func TestRunNonCodeWorkspace(t *testing.T) {
	sh, _ := newShell(t)
	sess, _ := sh.CreateSession()
	// Create a document workspace (not code)
	_, err := sh.Workspaces().Create("ws1", "document", "My Doc", "# Hello", sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := sh.ProcessMessage(context.Background(), sess.ID, "run it")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Intent != intent.KindRun {
		t.Errorf("expected KindRun, got %q", resp.Intent)
	}
	if resp.ChatReply == "" || resp.Workspace == nil {
		t.Error("expected reply explaining cannot run document workspace")
	}
}

func TestRunEmptyCodeWorkspace(t *testing.T) {
	sh, _ := newShell(t)
	sess, _ := sh.CreateSession()
	_, err := sh.Workspaces().Create("ws1", "code", "Empty Script", "", sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := sh.ProcessMessage(context.Background(), sess.ID, "run it")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Intent != intent.KindRun {
		t.Errorf("expected KindRun, got %q", resp.Intent)
	}
	if resp.ChatReply == "" {
		t.Error("expected reply explaining workspace is empty")
	}
}

func TestRunBashWorkspace(t *testing.T) {
	sh, _ := newShell(t)
	sess, _ := sh.CreateSession()
	code := "#!/bin/bash\necho hello from cas"
	_, err := sh.Workspaces().Create("ws1", "code", "Hello Script", code, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := sh.ProcessMessage(context.Background(), sess.ID, "run it")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Intent != intent.KindRun {
		t.Errorf("expected KindRun, got %q", resp.Intent)
	}
	if resp.Workspace == nil {
		t.Fatal("expected workspace in response")
	}
	if resp.Workspace.ID != "ws1" {
		t.Errorf("expected workspace ws1, got %q", resp.Workspace.ID)
	}
	// Check that output contains the echo'd text
	if !contains(resp.ChatReply, "hello from cas") {
		t.Errorf("expected 'hello from cas' in output, got %q", resp.ChatReply)
	}
}

func TestRunExitNonZero(t *testing.T) {
	sh, _ := newShell(t)
	sess, _ := sh.CreateSession()
	code := "#!/bin/bash\necho fail >&2\nexit 1"
	_, err := sh.Workspaces().Create("ws1", "code", "Fail Script", code, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := sh.ProcessMessage(context.Background(), sess.ID, "run it")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.ChatReply, "exit 1") {
		t.Errorf("expected 'exit 1' in output, got %q", resp.ChatReply)
	}
	if !contains(resp.ChatReply, "fail") {
		t.Errorf("expected 'fail' in stderr output, got %q", resp.ChatReply)
	}
}

func TestRunUndetectableLanguage(t *testing.T) {
	sh, _ := newShell(t)
	sess, _ := sh.CreateSession()
	code := "some random text that is not code"
	_, err := sh.Workspaces().Create("ws1", "code", "Unknown", code, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := sh.ProcessMessage(context.Background(), sess.ID, "run it")
	if err != nil {
		t.Fatal(err)
	}
	// Should not error, but should explain the failure
	if !contains(resp.ChatReply, "detect language") {
		t.Errorf("expected language detection error in reply, got %q", resp.ChatReply)
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && containsLower(s, substr)
}

func containsLower(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ── Plugin commands ───────────────────────────────────────────────

func TestPluginCommandExecutes(t *testing.T) {
	pluginDir := filepath.Join(t.TempDir(), "plugins")
	os.MkdirAll(pluginDir, 0755)
	os.WriteFile(filepath.Join(pluginDir, "test.lua"), []byte(`
cas.command("ping", "Test plugin", function()
	cas.reply("pong from lua")
end)
`), 0644)

	s := store.NewMemoryStore()
	conductorPath := filepath.Join(t.TempDir(), "profile.json")

	// Create shell with custom plugin dir
	sh := shell.NewShellWithPlugins(s, conductorPath, pluginDir)
	sess, _ := sh.CreateSession()

	resp, err := sh.ProcessMessage(context.Background(), sess.ID, "ping")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Intent != intent.KindPlugin {
		t.Errorf("expected KindPlugin, got %q", resp.Intent)
	}
	if resp.ChatReply != "pong from lua" {
		t.Errorf("expected 'pong from lua', got %q", resp.ChatReply)
	}
}

func TestPluginAccessesWorkspaces(t *testing.T) {
	pluginDir := filepath.Join(t.TempDir(), "plugins")
	os.MkdirAll(pluginDir, 0755)
	os.WriteFile(filepath.Join(pluginDir, "count.lua"), []byte(`
cas.command("wcount", "Count workspaces", function()
	local ws = cas.workspaces()
	cas.reply("count:" .. #ws)
end)
`), 0644)

	s := store.NewMemoryStore()
	sh := shell.NewShellWithPlugins(s, filepath.Join(t.TempDir(), "p.json"), pluginDir)
	sess, _ := sh.CreateSession()

	// Create a workspace
	sh.Workspaces().Create("ws1", "document", "Doc", "content", sess.ID)

	resp, err := sh.ProcessMessage(context.Background(), sess.ID, "wcount")
	if err != nil {
		t.Fatal(err)
	}
	if resp.ChatReply != "count:1" {
		t.Errorf("expected 'count:1', got %q", resp.ChatReply)
	}
}

func TestPluginNonMatchFallsThrough(t *testing.T) {
	// Non-matching messages should not trigger plugins
	pluginDir := filepath.Join(t.TempDir(), "plugins")
	os.MkdirAll(pluginDir, 0755)
	os.WriteFile(filepath.Join(pluginDir, "test.lua"), []byte(`
cas.command("ping", "P", function() cas.reply("pong") end)
`), 0644)

	s := store.NewMemoryStore()
	sh := shell.NewShellWithPlugins(s, filepath.Join(t.TempDir(), "p.json"), pluginDir)

	// "hello" should not match the plugin
	plugins := sh.Plugins()
	_, ok := plugins.Match("hello")
	if ok {
		t.Error("'hello' should not match any plugin command")
	}

	// "ping" should match
	_, ok = plugins.Match("ping")
	if !ok {
		t.Error("'ping' should match the plugin command")
	}
}

// ── Combine workspace ─────────────────────────────────────────────

func TestCombineNeedsTwoWorkspaces(t *testing.T) {
	sh, _ := newShell(t)
	sess, _ := sh.CreateSession()

	// Zero workspaces
	resp, err := sh.ProcessMessage(context.Background(), sess.ID, "combine everything")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Intent != intent.KindCombine {
		t.Errorf("expected KindCombine, got %q", resp.Intent)
	}
	if !strings.Contains(resp.ChatReply, "at least 2") {
		t.Errorf("expected 'at least 2' message, got %q", resp.ChatReply)
	}

	// One workspace
	sh.Workspaces().Create("ws1", "document", "Proposal", "content", sess.ID)
	resp, err = sh.ProcessMessage(context.Background(), sess.ID, "combine all workspaces")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.ChatReply, "at least 2") {
		t.Errorf("expected 'at least 2' message with 1 workspace, got %q", resp.ChatReply)
	}
}

// ── Long-session chat (regression: history contract + duplication) ─

// fakeOllamaChat stands in for the Ollama /api/chat endpoint and records
// every request body it receives, so tests can assert on exactly what the
// shell sends to the model.
type fakeOllamaChat struct {
	mu       sync.Mutex
	requests [][]struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
}

func (f *fakeOllamaChat) handler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, body.Messages)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"message":{"content":"ok"},"done":true}`)
}

// TestChatSurvivesLongSession drives 25 chat turns through the router.
// Before the chatHistory fix, turn 11 onward failed the ChatAgent's
// history_not_excessive precondition (session history is persisted and
// unbounded), and every request carried the current message twice — once
// at the tail of history, once as the user message.
func TestChatSurvivesLongSession(t *testing.T) {
	fake := &fakeOllamaChat{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	t.Setenv("CAS_PROVIDER", "ollama")
	t.Setenv("OLLAMA_BASE_URL", srv.URL)

	sh, _ := newShell(t)
	sess, err := sh.CreateSession()
	if err != nil {
		t.Fatal(err)
	}

	const turns = 25
	for i := 1; i <= turns; i++ {
		msg := fmt.Sprintf("chat turn %d", i)
		resp, err := sh.ProcessMessage(context.Background(), sess.ID, msg)
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		if resp.ChatReply != "ok" {
			t.Fatalf("turn %d: expected reply \"ok\", got %q", i, resp.ChatReply)
		}
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != turns {
		t.Fatalf("expected %d LLM requests, got %d", turns, len(fake.requests))
	}

	last := fake.requests[len(fake.requests)-1]
	final := fmt.Sprintf("chat turn %d", turns)

	// The current message appears exactly once, as the final user turn.
	count := 0
	for _, m := range last {
		if m.Content == final {
			count++
		}
	}
	if count != 1 {
		t.Errorf("current message appears %d times in the request, want 1", count)
	}
	if last[len(last)-1].Content != final {
		t.Errorf("final request message is %q, want %q", last[len(last)-1].Content, final)
	}

	// system + at most 6 history turns + current message.
	if len(last) > 8 {
		t.Errorf("request carries %d messages, want <= 8", len(last))
	}

	// Real context still flows: the immediately preceding turn is present.
	prev := fmt.Sprintf("chat turn %d", turns-1)
	found := false
	for _, m := range last {
		if m.Content == prev {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("previous turn %q missing from chat context", prev)
	}
}

// ── Orchestrate gate (regression: single tool workspace fell to chat) ─

// planEchoLLM serves /api/chat. Requests containing the orchestrator's
// planning-prompt marker get a one-step plan for wsID; everything else
// gets a plain chat reply. It records which system prompts it saw.
type planEchoLLM struct {
	mu      sync.Mutex
	wsID    string
	systems []string
}

func (f *planEchoLLM) handler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	system := ""
	if len(body.Messages) > 0 && body.Messages[0].Role == "system" {
		system = body.Messages[0].Content
	}
	f.mu.Lock()
	f.systems = append(f.systems, system)
	f.mu.Unlock()

	reply := "ok"
	if strings.Contains(system, "coordinating a multi-step task") {
		reply = fmt.Sprintf(`{"steps":[{"workspace_id":%q,"instruction":"list tools"}],"explanation":"single tool step"}`, f.wsID)
	}
	w.Header().Set("Content-Type", "application/json")
	enc, _ := json.Marshal(map[string]any{
		"message": map[string]string{"content": reply},
		"done":    true,
	})
	w.Write(enc)
}

func (f *planEchoLLM) sawPlanningPrompt() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.systems {
		if strings.Contains(s, "coordinating a multi-step task") {
			return true
		}
	}
	return false
}

// TestOrchestrateSingleMCPWorkspaceReachesOrchestrator: with exactly one
// mcp workspace — the state right after a first ingest — the advertised
// "use this server to <task>" must route to the orchestrator, not fall
// through to chat. The run then fails at the MCP connection layer
// (the test workspace has no live connection; completing the call needs
// one — that boundary is issue #3's territory), and reaching that layer
// is itself the proof the gate passed.
func TestOrchestrateSingleMCPWorkspaceReachesOrchestrator(t *testing.T) {
	fake := &planEchoLLM{wsID: "test-mcp-ws-1"}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	t.Setenv("CAS_PROVIDER", "ollama")
	t.Setenv("OLLAMA_BASE_URL", srv.URL)

	sh, _ := newShell(t)
	sess, _ := sh.CreateSession()
	if _, err := sh.Workspaces().Create("test-mcp-ws-1", "mcp", "MCP: http://fake", "tools", sess.ID); err != nil {
		t.Fatal(err)
	}

	_, err := sh.ProcessMessage(context.Background(), sess.ID, "use this server to list the available tools")
	if !fake.sawPlanningPrompt() {
		t.Fatal("orchestrator planning prompt never requested — message fell through to chat")
	}
	if err == nil || !strings.Contains(err.Error(), "no MCP connection") {
		t.Fatalf("expected the run to reach the MCP connection layer, got: %v", err)
	}
}

// TestOrchestrateSingleDocWorkspaceStillFallsToChat: a lone document
// workspace keeps the old behavior — conversational "use X to Y"
// phrasings go to chat, not the orchestrator.
func TestOrchestrateSingleDocWorkspaceStillFallsToChat(t *testing.T) {
	fake := &planEchoLLM{wsID: "unused"}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	t.Setenv("CAS_PROVIDER", "ollama")
	t.Setenv("OLLAMA_BASE_URL", srv.URL)

	sh, _ := newShell(t)
	sess, _ := sh.CreateSession()
	if _, err := sh.Workspaces().Create("test-doc-ws-1", "document", "Notes", "# Notes", sess.ID); err != nil {
		t.Fatal(err)
	}

	resp, err := sh.ProcessMessage(context.Background(), sess.ID, "use shorter sentences to improve the flow")
	if err != nil {
		t.Fatal(err)
	}
	if fake.sawPlanningPrompt() {
		t.Error("lone document workspace must not reach the orchestrator")
	}
	if resp.Intent != intent.KindChat {
		t.Errorf("expected KindChat fallback, got %q", resp.Intent)
	}
}
