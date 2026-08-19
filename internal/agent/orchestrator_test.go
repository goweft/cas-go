package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/goweft/cas/internal/agent"
)

// mockExecutor satisfies the StepExecutor interface for testing.
type mockExecutor struct {
	outputs map[string]string // wsID → output
	err     error
}

func (m *mockExecutor) ExecuteStep(_ context.Context, wsID, _, _ string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	if out, ok := m.outputs[wsID]; ok {
		return out, nil
	}
	return "mock output for " + wsID, nil
}

var testWorkspaces = []agent.WorkspaceInfo{
	{ID: "ws-1", Title: "Linear Issues", Type: "mcp", ToolSummary: "- list_issues: list open issues"},
	{ID: "ws-2", Title: "GitHub PRs", Type: "mcp", ToolSummary: "- create_pr: create a pull request"},
}

// ── OrchestratorAgent contract tests ─────────────────────────────

func TestOrchestratorContractEmptyInstruction(t *testing.T) {
	a := agent.NewOrchestratorAgent()
	_, err := a.Orchestrate(context.Background(), agent.OrchestratorRequest{
		Instruction: "  ",
		Workspaces:  testWorkspaces,
		Executor:    &mockExecutor{},
		Autonomy:    agent.AutonomySuggest,
	})
	if err == nil {
		t.Fatal("expected contract violation for empty instruction, got nil")
	}
	if !strings.Contains(err.Error(), "instruction_not_empty") {
		t.Errorf("expected instruction_not_empty violation, got: %v", err)
	}
}

func TestOrchestratorContractTooFewWorkspaces(t *testing.T) {
	// The floor is 1 workspace: a lone tool-bearing workspace is a
	// legitimate orchestration target (the post-ingest case). Zero
	// workspaces still violates — nothing to act on.
	a := agent.NewOrchestratorAgent()
	_, err := a.Orchestrate(context.Background(), agent.OrchestratorRequest{
		Instruction: "do something",
		Workspaces:  nil,
		Executor:    &mockExecutor{},
		Autonomy:    agent.AutonomySuggest,
	})
	if err == nil {
		t.Fatal("expected contract violation for 0 workspaces, got nil")
	}
	if !strings.Contains(err.Error(), "minimum_workspaces") {
		t.Errorf("expected minimum_workspaces violation, got: %v", err)
	}
}

func TestOrchestratorContractMissingWorkspaceID(t *testing.T) {
	a := agent.NewOrchestratorAgent()
	_, err := a.Orchestrate(context.Background(), agent.OrchestratorRequest{
		Instruction: "do something",
		Workspaces: []agent.WorkspaceInfo{
			{ID: "ws-1", Title: "One", Type: "mcp"},
			{ID: "", Title: "Two", Type: "web"}, // missing ID
		},
		Executor: &mockExecutor{},
		Autonomy: agent.AutonomySuggest,
	})
	if err == nil {
		t.Fatal("expected contract violation for missing workspace ID, got nil")
	}
	if !strings.Contains(err.Error(), "workspaces_have_ids") {
		t.Errorf("expected workspaces_have_ids violation, got: %v", err)
	}
}

func TestOrchestratorContractNilExecutor(t *testing.T) {
	a := agent.NewOrchestratorAgent()
	_, err := a.Orchestrate(context.Background(), agent.OrchestratorRequest{
		Instruction: "do something",
		Workspaces:  testWorkspaces,
		Executor:    nil,
		Autonomy:    agent.AutonomySuggest,
	})
	if err == nil {
		t.Fatal("expected contract violation for nil executor, got nil")
	}
	if !strings.Contains(err.Error(), "executor_present") {
		t.Errorf("expected executor_present violation, got: %v", err)
	}
}

func TestOrchestratorContractInvalidAutonomy(t *testing.T) {
	a := agent.NewOrchestratorAgent()
	_, err := a.Orchestrate(context.Background(), agent.OrchestratorRequest{
		Instruction: "do something",
		Workspaces:  testWorkspaces,
		Executor:    &mockExecutor{},
		Autonomy:    agent.Autonomy("yolo"),
	})
	if err == nil {
		t.Fatal("expected contract violation for invalid autonomy, got nil")
	}
	if !strings.Contains(err.Error(), "autonomy_valid") {
		t.Errorf("expected autonomy_valid violation, got: %v", err)
	}
}

func TestOrchestratorValidPreconditions(t *testing.T) {
	t.Skip("skipped: requires live LLM endpoint")
}
