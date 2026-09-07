package shell

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goweft/cas/internal/agent"
	"github.com/goweft/cas/internal/intent"
	"github.com/goweft/cas/internal/store"
	"github.com/goweft/cas/internal/workspace"
)

// handleOrchestrate coordinates a multi-workspace task in run autonomy and
// persists the run log.
func (sh *Shell) handleOrchestrate(ctx context.Context, sess *Session, message string) (*Response, error) {
	return sh.runOrchestration(ctx, sess, message, nil)
}

// OrchestrateConfirm runs orchestration with confirm-mode autonomy.
// The confirmFn is called before each step; it blocks until the user approves or skips.
// This is the entry point for the TUI confirm dial — the caller supplies the blocking function.
//
// It is a full turn of the message pipeline, not a side door: the session is
// looked up (not fabricated), the user turn is persisted before dispatch, the
// reply is persisted after, and the conductor observes the turn — exactly what
// ProcessMessage does for the same intent. Anything less leaves later chats
// with no record that the orchestration happened.
func (sh *Shell) OrchestrateConfirm(ctx context.Context, sessID, message string, confirmFn ConfirmFunc) (*Response, error) {
	sess, err := sh.GetSession(sessID)
	if err != nil {
		return nil, err
	}

	userMsg := sess.addMessage("user", message)
	if err := sh.store.SaveMessage(toStoreMsg(userMsg)); err != nil {
		return nil, err
	}

	resp, err := sh.runOrchestration(ctx, sess, message, confirmFn)
	if err != nil {
		return nil, err
	}

	shellMsg := sess.addMessage("shell", resp.ChatReply)
	if err := sh.store.SaveMessage(toStoreMsg(shellMsg)); err != nil {
		return nil, err
	}

	wsTitle, wsType := "", ""
	if resp.Workspace != nil {
		wsTitle, wsType = resp.Workspace.Title, resp.Workspace.Type
	}
	sh.conductor.Observe(string(intent.KindOrchestrate), message, wsTitle, wsType)

	return resp, nil
}

// runOrchestration is the single orchestration path behind both autonomy
// modes. A nil confirmFn means run autonomy; otherwise every step is gated
// through confirmFn.
//
// Audit ordering is a contract of this function, not a courtesy:
//   - the run row is inserted with StatusRunning BEFORE any step executes;
//     if that insert fails the orchestration is refused, because a side
//     effect with no audit anchor is unaccountable;
//   - every attempted step is recorded, including the one that failed
//     (with its error); if a step cannot be recorded the plan is aborted;
//   - the run row is finalized to Completed or Failed afterwards. A
//     finalization failure after a completed run is surfaced to the caller
//     rather than swallowed — the side effects happened, and the user should
//     know their record is incomplete.
func (sh *Shell) runOrchestration(ctx context.Context, sess *Session, message string, confirmFn ConfirmFunc) (*Response, error) {
	active := sh.workspaces.Active()
	if !orchestratable(active) {
		return sh.handleChat(ctx, sess, message)
	}

	// Build WorkspaceInfo for each active workspace
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

	// Anchor the audit record before the first side effect.
	run := store.OrchestrationRunRow{
		ID:          newID(),
		SessionID:   sess.ID,
		Instruction: message,
		Status:      store.OrchestrationRunning,
		CreatedAt:   time.Now().UTC(),
	}
	if err := sh.store.SaveOrchestrationRun(run); err != nil {
		return nil, fmt.Errorf("orchestration refused: cannot record run before executing: %w", err)
	}

	loggingExec := &loggingExecutor{inner: sh, store: sh.store, runID: run.ID}
	var exec agent.StepExecutor = loggingExec
	autonomy := agent.AutonomyRun
	if confirmFn != nil {
		exec = &confirmingExecutor{inner: loggingExec, confirm: confirmFn}
		autonomy = agent.AutonomyConfirm
	}

	result, err := sh.orchestAgent.Orchestrate(ctx, agent.OrchestratorRequest{
		Instruction: message,
		Workspaces:  wsInfos,
		Executor:    exec,
		Autonomy:    autonomy,
		UserContext: sh.conductor.UserContext(),
		Temperature: 0.3,
	})
	if err != nil {
		run.Status = store.OrchestrationFailed
		run.Error = err.Error()
		run.StepCount = loggingExec.stepCount
		if uerr := sh.store.UpdateOrchestrationRun(run); uerr != nil {
			return nil, errors.Join(err, fmt.Errorf("orchestration audit record could not be finalized: %w", uerr))
		}
		return nil, err
	}

	run.Status = store.OrchestrationCompleted
	run.Summary = result.Summary
	run.StepCount = loggingExec.stepCount
	if err := sh.store.UpdateOrchestrationRun(run); err != nil {
		return nil, fmt.Errorf("orchestration completed but its audit record could not be finalized: %w", err)
	}

	return &Response{ChatReply: result.Summary, Intent: intent.KindOrchestrate}, nil
}

// loggingExecutor wraps the shell's ExecuteStep and persists each step as it runs.
type loggingExecutor struct {
	inner     agent.StepExecutor
	store     store.Store
	runID     string
	stepCount int
}

// ExecuteStep runs one step and records it — success or failure. The step
// row is written after the step's side effect (it records the outcome), but
// a failure to write it aborts the plan: the next side effect does not run
// without its predecessor on record.
func (e *loggingExecutor) ExecuteStep(ctx context.Context, wsID, instruction, priorContext string) (string, error) {
	e.stepCount++
	output, err := e.inner.ExecuteStep(ctx, wsID, instruction, priorContext)
	step := store.OrchestrationStepRow{
		ID:          newID(),
		RunID:       e.runID,
		StepNumber:  e.stepCount,
		WorkspaceID: wsID,
		Instruction: instruction,
		Output:      output,
	}
	if err != nil {
		step.Output = ""
		step.Error = err.Error()
	}
	if serr := e.store.SaveOrchestrationStep(step); serr != nil {
		if err != nil {
			return "", errors.Join(err, fmt.Errorf("step %d could not be recorded: %w", e.stepCount, serr))
		}
		return "", fmt.Errorf("step %d executed but could not be recorded; aborting plan: %w", e.stepCount, serr)
	}
	if err != nil {
		return "", err
	}
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
