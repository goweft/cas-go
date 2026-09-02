// mcp_agent.go — MCPAgent: owns all MCP tool-call LLM interactions.
//
// The MCPAgent reasons about which tool to call and with what arguments,
// then executes the call under a scoped contract.  Its contract strictness
// is governed by the workspace autonomy dial:
//
//   suggest  — agent produces a tool-call suggestion; user must confirm
//   confirm  — agent executes but records every action for audit
//   run      — agent executes freely within its workspace scope
//
// The agent never calls tools outside its bound connection, and the contract
// enforces that the tool name exists on the server before execution: the
// LLM's plan is validated against the postconditions first, and only a plan
// that passes is sent to the connection.  A check that runs after the call
// would be a comment, not a boundary.

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/goweft/cas/internal/contract"
	"github.com/goweft/cas/internal/llm"
	mcpclient "github.com/goweft/cas/internal/mcp"
)

// Autonomy governs how freely the MCPAgent may act.
type Autonomy string

const (
	// AutonomySuggest — agent produces a suggestion; the shell presents it
	// to the user before any tool is called.
	AutonomySuggest Autonomy = "suggest"

	// AutonomyConfirm — agent calls tools but every call is logged and the
	// result is surfaced to the user before continuing.
	AutonomyConfirm Autonomy = "confirm"

	// AutonomyRun — agent calls tools freely within its workspace scope.
	AutonomyRun Autonomy = "run"
)

// MCPRequest is the input to MCPAgent.
type MCPRequest struct {
	Instruction string                // what the user asked
	Connection  *mcpclient.Connection // the bound MCP server
	Autonomy    Autonomy
	UserContext string
	Temperature float64
}

// MCPResult is the output from MCPAgent.
type MCPResult struct {
	// ToolCall is set when the agent decided to call a tool (all autonomy levels).
	ToolCall *MCPToolCall
	// Output is the tool's response (empty in suggest mode — tool not yet called).
	Output string
	// Suggestion is a natural-language description of what the agent wants to do.
	// Always populated; used as the workspace content.
	Suggestion string
}

// MCPToolCall records what tool was selected and with what arguments.
type MCPToolCall struct {
	ToolName  string
	Arguments map[string]interface{}
}

// MCPAgent handles ingest workspace interactions.
// It owns all KindIngest LLM calls and MCP tool invocations.
type MCPAgent struct{}

// NewMCPAgent returns an MCPAgent.
func NewMCPAgent() *MCPAgent { return &MCPAgent{} }

// Act reasons about the instruction, selects a tool (or none), and acts
// according to the autonomy dial.
func (a *MCPAgent) Act(ctx context.Context, req MCPRequest) (*MCPResult, error) {
	if err := a.contract(req, nil).CheckPreconditions(); err != nil {
		return nil, err
	}

	// Step 1: ask the LLM which tool to call and with what arguments.
	toolCall, suggestion, err := a.planToolCall(ctx, req)
	if err != nil {
		return nil, err
	}

	// Step 2: validate the plan against the contract BEFORE any side effect.
	// An invented tool name is refused here and never reaches the server.
	if err := a.contract(req, toolCall).CheckPostconditions(); err != nil {
		return nil, err
	}

	result := &MCPResult{
		ToolCall:   toolCall,
		Suggestion: suggestion,
	}

	// In suggest mode, return the validated plan without executing it.
	if req.Autonomy == AutonomySuggest || toolCall == nil {
		return result, nil
	}

	// Step 3 (confirm and run): execute the validated tool call.
	toolResult, err := req.Connection.Call(ctx, toolCall.ToolName, toolCall.Arguments)
	if err != nil {
		return nil, fmt.Errorf("mcp-agent: tool call failed: %w", err)
	}
	if toolResult.IsError {
		return nil, fmt.Errorf("mcp-agent: tool %q returned error: %s", toolCall.ToolName, toolResult.Content)
	}

	result.Output = toolResult.Content
	return result, nil
}

// planToolCall asks the LLM to choose a tool and arguments for the instruction.
// Returns (nil, suggestion, nil) if the LLM decides no tool call is needed.
func (a *MCPAgent) planToolCall(ctx context.Context, req MCPRequest) (*MCPToolCall, string, error) {
	sys := a.systemPrompt(req)
	msgs := []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: req.Instruction},
	}

	raw, err := llm.Complete(ctx, msgs, llm.ModelFor("chat"), req.Temperature)
	if err != nil {
		return nil, "", fmt.Errorf("mcp-agent: plan: %w", err)
	}

	// Try to parse a JSON tool call. If the model returns plain text, treat
	// it as a suggestion with no tool call.
	toolCall, suggestion, parseErr := parseToolCallResponse(raw)
	if parseErr != nil {
		// Plain text response — no tool call, return as suggestion.
		return nil, strings.TrimSpace(raw), nil
	}
	return toolCall, suggestion, nil
}

// systemPrompt builds the system prompt for tool-call planning.
func (a *MCPAgent) systemPrompt(req MCPRequest) string {
	var sb strings.Builder
	sb.WriteString("You are an assistant operating inside CAS (Conversational Agent Shell).\n")
	sb.WriteString("You are connected to an MCP server with the following tools:\n\n")
	sb.WriteString(req.Connection.ToolSummary())
	sb.WriteString("\n\nThe user has given you an instruction. ")
	sb.WriteString("If a tool can fulfil the instruction, respond with ONLY a JSON object:\n")
	sb.WriteString(`{"tool": "<name>", "args": {<key>: <value>, ...}, "summary": "<one-line description of what you are doing>"}`)
	sb.WriteString("\n\nIf no tool is needed or appropriate, respond with plain text explaining what you would do or why no tool applies.")
	sb.WriteString("\n\nScope rule: you may only call tools listed above. Never invent tool names.")
	if req.UserContext != "" {
		sb.WriteString("\n\nUser context: " + req.UserContext)
	}
	return sb.String()
}

// parseToolCallResponse parses a JSON tool-call response from the LLM.
// Returns the tool call and its summary, or an error if the response is not JSON.
func parseToolCallResponse(raw string) (*MCPToolCall, string, error) {
	raw = strings.TrimSpace(raw)
	// Strip markdown fences if present
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		if len(lines) > 2 {
			raw = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var v struct {
		Tool    string                 `json:"tool"`
		Args    map[string]interface{} `json:"args"`
		Summary string                 `json:"summary"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, "", err
	}
	if v.Tool == "" {
		return nil, "", fmt.Errorf("empty tool name")
	}
	return &MCPToolCall{
		ToolName:  v.Tool,
		Arguments: v.Args,
	}, v.Summary, nil
}

// contract builds and freezes the MCPAgent contract.
// toolCall is nil during precondition checks.
func (a *MCPAgent) contract(req MCPRequest, toolCall *MCPToolCall) *contract.Contract {
	c := contract.New("mcp-agent")
	c.Preconditions = []contract.Rule{
		{
			Name:        "instruction_not_empty",
			Description: "instruction must not be empty",
			Check:       func() bool { return strings.TrimSpace(req.Instruction) != "" },
		},
		{
			Name:        "connection_present",
			Description: "a live MCP connection must be provided",
			Check:       func() bool { return req.Connection != nil },
		},
		{
			Name:        "connection_has_tools",
			Description: "the MCP server must expose at least one tool",
			Check:       func() bool { return req.Connection != nil && len(req.Connection.Tools) > 0 },
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
			Name:        "tool_name_known",
			Description: "if a tool was selected, it must exist on the server",
			Check: func() bool {
				if toolCall == nil {
					return true // no tool selected — always valid
				}
				for _, t := range req.Connection.Tools {
					if t.Name == toolCall.ToolName {
						return true
					}
				}
				return false
			},
		},
	}
	return c.Freeze()
}
