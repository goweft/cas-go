// orchestrator.go — OrchestratorAgent: coordinates multi-workspace tasks.
//
// When a user instruction spans two or more workspaces (e.g. "read the Linear
// issue and open a GitHub PR for it"), the OrchestratorAgent decomposes the
// task into a sequence of single-agent steps and executes them in order,
// passing each step's output as context to the next.
//
// The orchestrator never acts directly on workspaces. It delegates each step
// to the appropriate bound agent (MCPAgent, WebAgent, GenerationAgent, etc.)
// via the StepExecutor interface. This keeps orchestration logic separate from
// execution logic and preserves each agent's own contract enforcement.
//
// Contract:
//   Pre:  instruction non-empty, ≥2 workspaces provided, all workspaces have IDs
//   Post: every planned step has a workspace ID that exists in the provided set,
//         and all steps completed without contract violation

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/goweft/cas/internal/contract"
	"github.com/goweft/cas/internal/llm"
)

// WorkspaceInfo describes a workspace available for orchestration.
type WorkspaceInfo struct {
	ID          string
	Title       string
	Type        string // "document" | "code" | "list" | "mcp" | "web"
	ToolSummary string // populated for mcp workspaces
	ContentSnip string // first 200 chars of content, for context
}

// OrchestrationStep is a single step in an orchestration plan.
type OrchestrationStep struct {
	WorkspaceID string
	Instruction string
	// Output is populated after the step executes.
	Output string
}

// OrchestrationPlan is the sequence of steps the orchestrator will execute.
type OrchestrationPlan struct {
	Steps       []OrchestrationStep
	Explanation string // why these steps in this order
}

// OrchestrationResult is the final output of an orchestration run.
type OrchestrationResult struct {
	Plan    *OrchestrationPlan
	Outputs []string // per-step outputs in order
	Summary string   // LLM-generated summary of what was accomplished
}

// StepExecutor executes a single orchestration step against a workspace.
// The shell implements this interface, delegating to the appropriate agent.
type StepExecutor interface {
	ExecuteStep(ctx context.Context, wsID, instruction, priorContext string) (string, error)
}

// OrchestratorRequest is the input to OrchestratorAgent.
type OrchestratorRequest struct {
	Instruction string
	Workspaces  []WorkspaceInfo
	Executor    StepExecutor
	Autonomy    Autonomy
	UserContext string
	Temperature float64
}

// OrchestratorAgent coordinates multi-workspace tasks.
// It owns the planning LLM call and delegates execution to the StepExecutor.
type OrchestratorAgent struct{}

// NewOrchestratorAgent returns an OrchestratorAgent.
func NewOrchestratorAgent() *OrchestratorAgent { return &OrchestratorAgent{} }

// Orchestrate plans and executes a multi-workspace task.
func (a *OrchestratorAgent) Orchestrate(ctx context.Context, req OrchestratorRequest) (*OrchestrationResult, error) {
	if err := a.contract(req, nil).CheckPreconditions(); err != nil {
		return nil, err
	}

	// Step 1: plan
	plan, err := a.plan(ctx, req)
	if err != nil {
		return nil, err
	}

	if err := a.contract(req, plan).CheckPostconditions(); err != nil {
		return nil, err
	}

	// In suggest mode, return the plan without executing.
	if req.Autonomy == AutonomySuggest {
		return &OrchestrationResult{Plan: plan}, nil
	}

	// Step 2: execute each step in sequence.
	outputs := make([]string, len(plan.Steps))
	var priorContext string

	for i, step := range plan.Steps {
		out, err := req.Executor.ExecuteStep(ctx, step.WorkspaceID, step.Instruction, priorContext)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: step %d (%s): %w", i+1, step.WorkspaceID, err)
		}
		outputs[i] = out
		plan.Steps[i].Output = out
		// Feed this step's output as context for the next step.
		priorContext = fmt.Sprintf("Step %d result (%s):\n%s", i+1, step.WorkspaceID, truncate(out, 1024))
	}

	// Step 3: summarise what was accomplished.
	summary, err := a.summarise(ctx, req, plan, outputs)
	if err != nil {
		summary = fmt.Sprintf("Completed %d steps.", len(plan.Steps))
	}

	return &OrchestrationResult{
		Plan:    plan,
		Outputs: outputs,
		Summary: summary,
	}, nil
}

// plan asks the LLM to decompose the instruction into ordered steps.
func (a *OrchestratorAgent) plan(ctx context.Context, req OrchestratorRequest) (*OrchestrationPlan, error) {
	sys := a.planningPrompt(req)
	msgs := []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: req.Instruction},
	}

	raw, err := llm.Complete(ctx, msgs, llm.ModelFor("chat"), req.Temperature)
	if err != nil {
		return nil, fmt.Errorf("orchestrator plan: %w", err)
	}

	plan, err := parsePlan(raw, req.Workspaces)
	if err != nil {
		return nil, fmt.Errorf("orchestrator plan parse: %w", err)
	}
	return plan, nil
}

// planningPrompt builds the system prompt for step planning.
func (a *OrchestratorAgent) planningPrompt(req OrchestratorRequest) string {
	var sb strings.Builder
	sb.WriteString("You are coordinating a multi-step task across several workspaces in CAS.\n\n")
	sb.WriteString("Available workspaces:\n")
	for _, ws := range req.Workspaces {
		sb.WriteString(fmt.Sprintf("\n- id: %q\n  title: %q\n  type: %s\n", ws.ID, ws.Title, ws.Type))
		if ws.ToolSummary != "" {
			sb.WriteString("  tools:\n")
			for _, line := range strings.Split(ws.ToolSummary, "\n") {
				sb.WriteString("    " + line + "\n")
			}
		}
		if ws.ContentSnip != "" {
			sb.WriteString(fmt.Sprintf("  content: %q\n", ws.ContentSnip))
		}
	}
	sb.WriteString("\nDecompose the user's instruction into an ordered sequence of steps.\n")
	sb.WriteString("Each step targets one workspace. Steps execute in order; earlier results feed into later steps.\n\n")
	sb.WriteString("Respond with ONLY a JSON object:\n")
	sb.WriteString(`{"explanation": "<why these steps>", "steps": [{"workspace_id": "<id>", "instruction": "<what to do>"}]}`)
	sb.WriteString("\n\nRules:\n")
	sb.WriteString("- workspace_id must exactly match one of the IDs above.\n")
	sb.WriteString("- Each instruction should be self-contained but may reference 'the previous step result'.\n")
	sb.WriteString("- Use the minimum number of steps needed. Do not add unnecessary steps.\n")
	if req.UserContext != "" {
		sb.WriteString("\nUser context: " + req.UserContext)
	}
	return sb.String()
}

// parsePlan parses a JSON plan from the LLM and validates workspace IDs.
func parsePlan(raw string, workspaces []WorkspaceInfo) (*OrchestrationPlan, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		if len(lines) > 2 {
			raw = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var v struct {
		Explanation string `json:"explanation"`
		Steps       []struct {
			WorkspaceID string `json:"workspace_id"`
			Instruction string `json:"instruction"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if len(v.Steps) == 0 {
		return nil, fmt.Errorf("plan has no steps")
	}

	// Build ID set for validation
	idSet := make(map[string]bool, len(workspaces))
	for _, ws := range workspaces {
		idSet[ws.ID] = true
	}

	steps := make([]OrchestrationStep, len(v.Steps))
	for i, s := range v.Steps {
		if !idSet[s.WorkspaceID] {
			return nil, fmt.Errorf("step %d: unknown workspace_id %q", i+1, s.WorkspaceID)
		}
		if strings.TrimSpace(s.Instruction) == "" {
			return nil, fmt.Errorf("step %d: empty instruction", i+1)
		}
		steps[i] = OrchestrationStep{
			WorkspaceID: s.WorkspaceID,
			Instruction: s.Instruction,
		}
	}

	return &OrchestrationPlan{
		Steps:       steps,
		Explanation: v.Explanation,
	}, nil
}

// summarise asks the LLM to produce a brief summary of what was accomplished.
func (a *OrchestratorAgent) summarise(ctx context.Context, req OrchestratorRequest, plan *OrchestrationPlan, outputs []string) (string, error) {
	var sb strings.Builder
	sb.WriteString("The following multi-step task was completed:\n\n")
	sb.WriteString("Original instruction: " + req.Instruction + "\n\n")
	for i, step := range plan.Steps {
		sb.WriteString(fmt.Sprintf("Step %d (%s): %s\nResult: %s\n\n",
			i+1, step.WorkspaceID, step.Instruction, truncate(outputs[i], 300)))
	}
	sb.WriteString("Write a single short sentence summarising what was accomplished.")

	msgs := []llm.Message{
		{Role: "user", Content: sb.String()},
	}
	return llm.Complete(ctx, msgs, llm.ModelFor("chat"), 0.3)
}

// contract builds and freezes the OrchestratorAgent contract.
func (a *OrchestratorAgent) contract(req OrchestratorRequest, plan *OrchestrationPlan) *contract.Contract {
	// Build ID set once for postcondition check.
	idSet := make(map[string]bool, len(req.Workspaces))
	for _, ws := range req.Workspaces {
		idSet[ws.ID] = true
	}

	c := contract.New("orchestrator-agent")
	c.Preconditions = []contract.Rule{
		{
			Name:        "instruction_not_empty",
			Description: "orchestration instruction must not be empty",
			Check:       func() bool { return strings.TrimSpace(req.Instruction) != "" },
		},
		{
			Name:        "minimum_workspaces",
			Description: "orchestration requires at least 1 workspace",
			Check:       func() bool { return len(req.Workspaces) >= 1 },
		},
		{
			Name:        "workspaces_have_ids",
			Description: "all workspaces must have non-empty IDs",
			Check: func() bool {
				for _, ws := range req.Workspaces {
					if ws.ID == "" {
						return false
					}
				}
				return true
			},
		},
		{
			Name:        "executor_present",
			Description: "a StepExecutor must be provided",
			Check:       func() bool { return req.Executor != nil },
		},
		{
			Name:        "autonomy_valid",
			Description: "autonomy must be suggest, confirm, or run",
			Check: func() bool {
				return req.Autonomy == AutonomySuggest ||
					req.Autonomy == AutonomyConfirm ||
					req.Autonomy == AutonomyRun
			},
		},
	}
	c.Postconditions = []contract.Rule{
		{
			Name:        "plan_has_steps",
			Description: "plan must contain at least one step",
			Check: func() bool {
				return plan != nil && len(plan.Steps) > 0
			},
		},
		{
			Name:        "plan_steps_have_known_workspaces",
			Description: "every step workspace_id must exist in the provided workspace set",
			Check: func() bool {
				if plan == nil {
					return true
				}
				for _, step := range plan.Steps {
					if !idSet[step.WorkspaceID] {
						return false
					}
				}
				return true
			},
		},
	}
	return c.Freeze()
}

// truncate trims a string to at most n bytes, appending "…" if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
