package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	mcpclient "github.com/goweft/cas/internal/mcp"
	"github.com/goweft/cas/internal/store"
)

// ── Confirm dial shows the concrete action (review round 3, finding 1) ──
//
// End-to-end through OrchestrateConfirm with an offline MCP connection
// injected into the shell. The LLM fake answers the orchestrator with a
// one-step plan and the MCP agent with a tool call. Before the fix the
// ConfirmFunc saw "[ws] <step instruction>" and the tool call then ran
// under run autonomy; now it sees the tool name and its arguments.

type twoPromptLLM struct {
	mu   sync.Mutex
	wsID string
	seen []string
}

func (f *twoPromptLLM) handler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	system := ""
	if len(body.Messages) > 0 && body.Messages[0].Role == "system" {
		system = body.Messages[0].Content
	}
	f.mu.Lock()
	f.seen = append(f.seen, system)
	f.mu.Unlock()

	reply := "ok"
	switch {
	case strings.Contains(system, "coordinating a multi-step task"):
		reply = fmt.Sprintf(`{"steps":[{"workspace_id":%q,"instruction":"list the open issues"}],"explanation":"one step"}`, f.wsID)
	case strings.Contains(system, "connected to an MCP server"):
		reply = `{"tool":"list_issues","args":{"state":"open","repo":"goweft/cas"},"summary":"list open issues"}`
	}
	w.Header().Set("Content-Type", "application/json")
	enc, _ := json.Marshal(map[string]any{"message": map[string]string{"content": reply}, "done": true})
	w.Write(enc)
}

func newConfirmTestShell(t *testing.T) (*Shell, *Session) {
	t.Helper()
	fake := &twoPromptLLM{wsID: "mcp-1"}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	t.Setenv("CAS_PROVIDER", "ollama")
	t.Setenv("OLLAMA_BASE_URL", srv.URL)

	sh := NewShell(store.NewMemoryStore(), filepath.Join(t.TempDir(), "profile.json"))
	sess, _ := sh.CreateSession()
	if _, err := sh.Workspaces().Create("mcp-1", "mcp", "MCP: http://fake", "tools", sess.ID); err != nil {
		t.Fatal(err)
	}
	// Offline connection: tool list present, no live client. An approved
	// call fails with "no live client" — proof it reached the connection.
	sh.mcpConns["mcp-1"] = &mcpclient.Connection{
		ServerURL: "http://fake",
		Tools:     []mcpclient.Tool{{Name: "list_issues", Description: "list issues"}},
	}
	return sh, sess
}

func TestConfirmDialShowsToolNameAndArguments(t *testing.T) {
	sh, sess := newConfirmTestShell(t)
	var prompts []string
	_, err := sh.OrchestrateConfirm(context.Background(), sess.ID, "use this server to list the open issues", func(d string) bool {
		prompts = append(prompts, d)
		return true
	})
	if len(prompts) != 1 {
		t.Fatalf("expected exactly one confirmation for one tool step, got %d: %v", len(prompts), prompts)
	}
	p := prompts[0]
	for _, want := range []string{"[mcp-1]", "list_issues", `state="open"`, `repo="goweft/cas"`} {
		if !strings.Contains(p, want) {
			t.Errorf("confirm prompt %q lacks %q", p, want)
		}
	}
	if strings.Contains(p, "list the open issues") {
		t.Errorf("confirm prompt is the abstract step instruction, not the action: %q", p)
	}
	// Approved → executed → reached the offline connection.
	if err == nil || !strings.Contains(err.Error(), "no live client") {
		t.Fatalf("approved tool call did not reach the connection: %v", err)
	}
}

func TestConfirmDialDeclineSkipsToolCall(t *testing.T) {
	sh, sess := newConfirmTestShell(t)
	resp, err := sh.OrchestrateConfirm(context.Background(), sess.ID, "use this server to list the open issues", func(string) bool { return false })
	if err != nil {
		t.Fatalf("declining must not fail the run: %v", err)
	}
	if resp == nil {
		t.Fatal("expected a response")
	}
	runs, _ := sh.store.LoadOrchestrationRuns(sess.ID)
	if len(runs) != 1 || runs[0].Status != store.OrchestrationCompleted {
		t.Fatalf("declined run should complete with the step skipped: %+v", runs)
	}
	steps, _ := sh.store.LoadOrchestrationSteps(runs[0].ID)
	if len(steps) != 1 || !strings.HasPrefix(steps[0].Output, "(skipped: call tool list_issues") {
		t.Errorf("declined step not recorded as skipped with the action: %+v", steps)
	}
}
