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

	// A nil confirmFn runs every step; otherwise steps are gated — tool
	// steps on the concrete action (see executeStep), edit steps on the
	// step description.
	loggingExec := &loggingExecutor{
		inner: &stepExecutor{sh: sh, confirm: confirmFn},
		store: sh.store,
		runID: run.ID,
	}
	autonomy := agent.AutonomyRun
	if confirmFn != nil {
		autonomy = agent.AutonomyConfirm
	}

	result, err := sh.orchestAgent.Orchestrate(ctx, agent.OrchestratorRequest{
		Instruction: message,
		Workspaces:  wsInfos,
		Executor:    loggingExec,
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

// ConfirmFunc is called in confirm-autonomy orchestration before each side
// effect. It receives a human-readable description of exactly what is about
// to happen and should block until the user responds, returning true to
// proceed or false to skip. The UI wires this to the FocusConfirm TUI state.
type ConfirmFunc func(description string) bool

// stepExecutor routes orchestration steps to the shell with an optional
// confirm gate. It exists so the gate travels with the step into the agent
// that performs the side effect, instead of stopping at the step boundary.
type stepExecutor struct {
	sh      *Shell
	confirm ConfirmFunc // nil = run autonomy
}

func (e *stepExecutor) ExecuteStep(ctx context.Context, wsID, instruction, priorContext string) (string, error) {
	return e.sh.executeStep(ctx, wsID, instruction, priorContext, e.confirm)
}

// ExecuteStep implements agent.StepExecutor in run autonomy.
func (sh *Shell) ExecuteStep(ctx context.Context, wsID, instruction, priorContext string) (string, error) {
	return sh.executeStep(ctx, wsID, instruction, priorContext, nil)
}

// executeStep routes a single orchestration step to the appropriate agent
// based on workspace type.
//
// What confirm autonomy asks the user to approve depends on where the side
// effect is:
//   - mcp and web steps: the CONCRETE action — tool name with its full
//     arguments, or the exact URL — as planned by the nested agent and
//     validated by its contract. The step's English instruction is not shown
//     for approval, because approving it would approve nothing: the tool and
//     arguments are chosen afterwards. Declining skips the step; the plan
//     continues.
//   - document/code/list steps: the step itself. The side effect is a local,
//     versioned workspace edit whose content only exists after the LLM call;
//     it is undoable and never leaves the machine.
func (sh *Shell) executeStep(ctx context.Context, wsID, instruction, priorContext string, confirm ConfirmFunc) (string, error) {
	ws, err := sh.workspaces.Get(wsID)
	if err != nil || ws == nil {
		return "", fmt.Errorf("workspace %q not found", wsID)
	}

	// Prepend prior context to instruction if present
	fullInstruction := instruction
	if priorContext != "" {
		fullInstruction = priorContext + "\n\n" + instruction
	}

	autonomy := agent.AutonomyRun
	var actionConfirm agent.ActionConfirmer
	if confirm != nil {
		autonomy = agent.AutonomyConfirm
		actionConfirm = func(p agent.ActionPreview) bool {
			// Full preview, never truncated: the arguments ARE the action.
			return confirm(fmt.Sprintf("[%s] %s", wsID, p.String()))
		}
	}

	switch ws.Type {
	case "mcp":
		result, err := sh.HandleMCPAction(ctx, wsID, fullInstruction, autonomy, actionConfirm)
		if err != nil {
			return "", err
		}
		if result.Declined {
			return fmt.Sprintf("(skipped: %s)", agent.ActionPreview{Kind: agent.ActionMCPTool, ToolName: result.ToolCall.ToolName, Arguments: result.ToolCall.Arguments}), nil
		}
		if result.Output != "" {
			return result.Output, nil
		}
		return result.Suggestion, nil
	case "web":
		result, err := sh.HandleWebAction(ctx, wsID, fullInstruction, autonomy, actionConfirm)
		if err != nil {
			return "", err
		}
		if result.Declined {
			return fmt.Sprintf("(skipped: %s)", agent.ActionPreview{Kind: agent.ActionWebNavigate, URL: result.NavigateURL}), nil
		}
		if result.Answer != "" {
			return result.Answer, nil
		}
		if result.NewPage != nil {
			return result.NewPage.Text, nil
		}
		return "", nil
	default:
		if confirm != nil && !confirm(fmt.Sprintf("[%s] edit %s workspace %q: %s", wsID, ws.Type, ws.Title, instruction)) {
			return "(skipped)", nil
		}
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
