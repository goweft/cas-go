package shell

import (
	"context"
	"fmt"
	"time"

	"github.com/goweft/cas/internal/agent"
	"github.com/goweft/cas/internal/intent"
	"github.com/goweft/cas/internal/store"
	"github.com/goweft/cas/internal/workspace"
)

// handleOrchestrate coordinates a multi-workspace task and persists the run log.
func (sh *Shell) handleOrchestrate(ctx context.Context, sess *Session, message string) (*Response, error) {
	active := sh.workspaces.Active()
	if !orchestratable(active) {
		return sh.handleChat(ctx, sess, message)
	}

	// Build WorkspaceInfo for each active workspace
	wsInfos := make([]agent.WorkspaceInfo, len(active))
	for i, ws := range active {
		info := agent.WorkspaceInfo{
			ID:    ws.ID,
			Title: ws.Title,
			Type:  ws.Type,
		}
		if conn, ok := sh.mcpConns[ws.ID]; ok {
			info.ToolSummary = conn.ToolSummary()
		}
		if len(ws.Content) > 200 {
			info.ContentSnip = ws.Content[:200]
		} else {
			info.ContentSnip = ws.Content
		}
		wsInfos[i] = info
	}

	// Create a run ID and a logging executor before starting.
	runID := newID()
	loggingExec := &loggingExecutor{
		inner: sh,
		store: sh.store,
		runID: runID,
	}

	result, err := sh.orchestAgent.Orchestrate(ctx, agent.OrchestratorRequest{
		Instruction: message,
		Workspaces:  wsInfos,
		Executor:    loggingExec,
		Autonomy:    agent.AutonomyRun,
		UserContext: sh.conductor.UserContext(),
		Temperature: 0.3,
	})
	if err != nil {
		return nil, err
	}

	// Persist the run record.
	run := store.OrchestrationRunRow{
		ID:          runID,
		SessionID:   sess.ID,
		Instruction: message,
		Summary:     result.Summary,
		StepCount:   len(result.Plan.Steps),
		CreatedAt:   time.Now().UTC(),
	}
	if err := sh.store.SaveOrchestrationRun(run); err != nil {
		// Non-fatal: log and continue.
		_ = err
	}

	return &Response{ChatReply: result.Summary, Intent: intent.KindOrchestrate}, nil
}

// OrchestrateConfirm runs orchestration with confirm-mode autonomy.
// The confirmFn is called before each step; it blocks until the user approves or skips.
// This is the entry point for the TUI confirm dial — the caller supplies the blocking function.
func (sh *Shell) OrchestrateConfirm(ctx context.Context, sessID, message string, confirmFn ConfirmFunc) (*Response, error) {
	active := sh.workspaces.Active()
	if !orchestratable(active) {
		return sh.handleChat(ctx, &Session{ID: sessID}, message)
	}

	wsInfos := make([]agent.WorkspaceInfo, len(active))
	for i, ws := range active {
		info := agent.WorkspaceInfo{ID: ws.ID, Title: ws.Title, Type: ws.Type}
		if conn, ok := sh.mcpConns[ws.ID]; ok {
			info.ToolSummary = conn.ToolSummary()
		}
		if len(ws.Content) > 200 {
			info.ContentSnip = ws.Content[:200]
		} else {
			info.ContentSnip = ws.Content
		}
		wsInfos[i] = info
	}

	runID := newID()
	loggingExec := &loggingExecutor{inner: sh, store: sh.store, runID: runID}
	exec := &confirmingExecutor{inner: loggingExec, confirm: confirmFn}

	result, err := sh.orchestAgent.Orchestrate(ctx, agent.OrchestratorRequest{
		Instruction: message,
		Workspaces:  wsInfos,
		Executor:    exec,
		Autonomy:    agent.AutonomyConfirm,
		UserContext: sh.conductor.UserContext(),
		Temperature: 0.3,
	})
	if err != nil {
		return nil, err
	}

	run := store.OrchestrationRunRow{
		ID:          runID,
		SessionID:   sessID,
		Instruction: message,
		Summary:     result.Summary,
		StepCount:   len(result.Plan.Steps),
		CreatedAt:   time.Now().UTC(),
	}
	_ = sh.store.SaveOrchestrationRun(run)

	return &Response{ChatReply: result.Summary, Intent: intent.KindOrchestrate}, nil
}

// loggingExecutor wraps the shell's ExecuteStep and persists each step as it runs.
type loggingExecutor struct {
	inner     agent.StepExecutor
	store     store.Store
	runID     string
	stepCount int
}

func (e *loggingExecutor) ExecuteStep(ctx context.Context, wsID, instruction, priorContext string) (string, error) {
	e.stepCount++
	output, err := e.inner.ExecuteStep(ctx, wsID, instruction, priorContext)
	if err != nil {
		return "", err
	}
	step := store.OrchestrationStepRow{
		ID:          newID(),
		RunID:       e.runID,
		StepNumber:  e.stepCount,
		WorkspaceID: wsID,
		Instruction: instruction,
		Output:      output,
	}
	// Non-fatal if persistence fails.
	_ = e.store.SaveOrchestrationStep(step)
	return output, nil
}

// ConfirmFunc is called before each step in confirm-autonomy orchestration.
// It should block until the user responds and return true to proceed or false to skip.
// The UI wires this to the FocusConfirm TUI state.
type ConfirmFunc func(description string) bool

// confirmingExecutor wraps a loggingExecutor and pauses before each step for user approval.
type confirmingExecutor struct {
	inner   *loggingExecutor
	confirm ConfirmFunc
}

func (e *confirmingExecutor) ExecuteStep(ctx context.Context, wsID, instruction, priorContext string) (string, error) {
	desc := fmt.Sprintf("[%s] %s", wsID, instruction)
	if len(desc) > 120 {
		desc = desc[:117] + "..."
	}
	if !e.confirm(desc) {
		// User skipped this step — return empty output and continue plan.
		return "(skipped)", nil
	}
	return e.inner.ExecuteStep(ctx, wsID, instruction, priorContext)
}

// ExecuteStep implements agent.StepExecutor.
// Routes a single orchestration step to the appropriate agent based on workspace type.
func (sh *Shell) ExecuteStep(ctx context.Context, wsID, instruction, priorContext string) (string, error) {
	ws, err := sh.workspaces.Get(wsID)
	if err != nil || ws == nil {
		return "", fmt.Errorf("workspace %q not found", wsID)
	}

	// Prepend prior context to instruction if present
	fullInstruction := instruction
	if priorContext != "" {
		fullInstruction = priorContext + "\n\n" + instruction
	}

	switch ws.Type {
	case "mcp":
		result, err := sh.HandleMCPAction(ctx, wsID, fullInstruction, agent.AutonomyRun)
		if err != nil {
			return "", err
		}
		if result.Output != "" {
			return result.Output, nil
		}
		return result.Suggestion, nil
	case "web":
		result, err := sh.HandleWebAction(ctx, wsID, fullInstruction, agent.AutonomyRun)
		if err != nil {
			return "", err
		}
		if result.Answer != "" {
			return result.Answer, nil
		}
		if result.NewPage != nil {
			return result.NewPage.Text, nil
		}
		return "", nil
	default:
		// For document/code/list workspaces: use EditAgent to apply the instruction
		result, err := sh.editAgent.Edit(ctx, agent.EditRequest{
			WSType:         ws.Type,
			Title:          ws.Title,
			CurrentContent: ws.Content,
			EditRequest:    fullInstruction,
			UserContext:    sh.conductor.UserContext(),
			Temperature:    0.3,
		})
		if err != nil {
			return "", err
		}
		// Persist the edit
		_, err = sh.workspaces.Update(wsID, ws.Title, result.Content)
		return result.Content, err
	}
}

// orchestratable reports whether orchestration can act on the active set.
// Zero workspaces has nothing to act on. A single tool-bearing workspace
// (mcp, web) is exactly the advertised "use this server to <task>" case —
// the state a user is in right after their first ingest — and must
// orchestrate rather than silently fall through to the tool-less
// ChatAgent. A lone document or code workspace keeps the chat fallback so
// conversational "use X to Y" phrasings behave as they always have.
func orchestratable(active []*workspace.Workspace) bool {
	switch len(active) {
	case 0:
		return false
	case 1:
		return active[0].Type == "mcp" || active[0].Type == "web"
	default:
		return true
	}
}
