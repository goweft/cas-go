package shell_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goweft/cas/internal/shell"
	"github.com/goweft/cas/internal/store"
)

// ── Orchestration audit (regression: failed runs left no record) ──────
//
// Before the fix, the run row was inserted only after every step succeeded,
// and a failing step returned before its own row was written. A plan whose
// step N failed therefore left steps 1..N-1 orphaned (no parent run; foreign
// keys are off) and step N unrecorded. These tests use the same
// single-MCP-workspace setup as the gate tests: the plan reaches the MCP
// connection layer and fails there, which is exactly the "side effect
// attempted, then failure" shape that must be auditable.

func orchestrateToFailure(t *testing.T) (*store.MemoryStore, string, error) {
	t.Helper()
	fake := &planEchoLLM{wsID: "test-mcp-ws-1"}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	t.Setenv("CAS_PROVIDER", "ollama")
	t.Setenv("OLLAMA_BASE_URL", srv.URL)

	sh, s := newShell(t)
	sess, _ := sh.CreateSession()
	if _, err := sh.Workspaces().Create("test-mcp-ws-1", "mcp", "MCP: http://fake", "tools", sess.ID); err != nil {
		t.Fatal(err)
	}
	_, err := sh.ProcessMessage(context.Background(), sess.ID, "use this server to list the available tools")
	if !fake.sawPlanningPrompt() {
		t.Fatal("orchestrator never planned — setup is wrong, not the audit")
	}
	return s, sess.ID, err
}

// TestOrchestrationFailedRunIsRecorded: a run whose step fails must leave a
// run row in status=failed carrying the error, and the failed step itself
// must be on record with its error.
func TestOrchestrationFailedRunIsRecorded(t *testing.T) {
	s, sessID, err := orchestrateToFailure(t)
	if err == nil || !strings.Contains(err.Error(), "no MCP connection") {
		t.Fatalf("expected the step to fail at the connection layer, got: %v", err)
	}

	runs, _ := s.LoadOrchestrationRuns(sessID)
	if len(runs) != 1 {
		t.Fatalf("expected exactly 1 run row for the failed orchestration, got %d", len(runs))
	}
	run := runs[0]
	if run.Status != store.OrchestrationFailed {
		t.Errorf("run.Status = %q, want %q", run.Status, store.OrchestrationFailed)
	}
	if !strings.Contains(run.Error, "no MCP connection") {
		t.Errorf("run.Error = %q, want the step failure", run.Error)
	}
	if run.StepCount != 1 {
		t.Errorf("run.StepCount = %d, want 1 (the attempted step)", run.StepCount)
	}

	steps, _ := s.LoadOrchestrationSteps(run.ID)
	if len(steps) != 1 {
		t.Fatalf("expected the failed step to be recorded, got %d step rows", len(steps))
	}
	if !strings.Contains(steps[0].Error, "no MCP connection") {
		t.Errorf("step.Error = %q, want the failure", steps[0].Error)
	}
	if steps[0].Output != "" {
		t.Errorf("failed step must not carry output, got %q", steps[0].Output)
	}
}

// TestOrchestrationStepsNeverOrphaned: every step row must reference a run
// row that exists. This is the property the fix guarantees regardless of
// where in the plan a failure lands.
func TestOrchestrationStepsNeverOrphaned(t *testing.T) {
	s, sessID, _ := orchestrateToFailure(t)
	runs, _ := s.LoadOrchestrationRuns(sessID)
	known := map[string]bool{}
	for _, r := range runs {
		known[r.ID] = true
	}
	for _, r := range runs {
		steps, _ := s.LoadOrchestrationSteps(r.ID)
		for _, st := range steps {
			if !known[st.RunID] {
				t.Errorf("step %s references unknown run %s", st.ID, st.RunID)
			}
		}
	}
	if len(runs) == 0 {
		t.Fatal("no run row at all — steps (if any) have nothing to belong to")
	}
}

// runInsertFailsStore refuses to insert a run row, standing in for a store
// that cannot anchor the audit record.
type runInsertFailsStore struct {
	*store.MemoryStore
}

var errNoAudit = errors.New("store: cannot insert run")

func (f *runInsertFailsStore) SaveOrchestrationRun(run store.OrchestrationRunRow) error {
	return errNoAudit
}

// TestOrchestrationRefusedWhenRunCannotBeRecorded: if the run row cannot be
// written, no step may execute — the planner must not even be consulted.
func TestOrchestrationRefusedWhenRunCannotBeRecorded(t *testing.T) {
	fake := &planEchoLLM{wsID: "test-mcp-ws-1"}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	t.Setenv("CAS_PROVIDER", "ollama")
	t.Setenv("OLLAMA_BASE_URL", srv.URL)

	s := &runInsertFailsStore{MemoryStore: store.NewMemoryStore()}
	sh := shell.NewShell(s, filepath.Join(t.TempDir(), "profile.json"))
	sess, _ := sh.CreateSession()
	if _, err := sh.Workspaces().Create("test-mcp-ws-1", "mcp", "MCP: http://fake", "tools", sess.ID); err != nil {
		t.Fatal(err)
	}

	_, err := sh.ProcessMessage(context.Background(), sess.ID, "use this server to list the available tools")
	if !errors.Is(err, errNoAudit) {
		t.Fatalf("expected refusal wrapping the store error, got: %v", err)
	}
	if fake.sawPlanningPrompt() {
		t.Error("planner was consulted despite the run being unrecordable")
	}
}
