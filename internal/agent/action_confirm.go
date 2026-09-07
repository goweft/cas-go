package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ActionKind names the concrete side effect an agent is about to perform.
type ActionKind string

const (
	ActionMCPTool     ActionKind = "mcp_tool"     // call a named tool on the bound MCP server
	ActionWebNavigate ActionKind = "web_navigate" // fetch a URL in the web session
)

// ActionPreview is the concrete, fully-resolved action an agent will take
// if allowed to proceed. It is what confirm autonomy actually asks the user
// to approve: the tool name and its arguments, or the exact URL — not the
// instruction that led to them. Nothing in it is abstract or truncated.
type ActionPreview struct {
	Kind      ActionKind
	ToolName  string                 // ActionMCPTool
	Arguments map[string]interface{} // ActionMCPTool
	URL       string                 // ActionWebNavigate
}

// String renders the preview for a confirmation prompt. Arguments are
// rendered in full with sorted keys so the same action always reads the
// same way; hiding or shortening them would defeat the point of the gate.
func (p ActionPreview) String() string {
	switch p.Kind {
	case ActionMCPTool:
		if len(p.Arguments) == 0 {
			return fmt.Sprintf("call tool %s with no arguments", p.ToolName)
		}
		keys := make([]string, 0, len(p.Arguments))
		for k := range p.Arguments {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			v, err := json.Marshal(p.Arguments[k])
			if err != nil {
				v = []byte(fmt.Sprintf("%v", p.Arguments[k]))
			}
			parts = append(parts, k+"="+string(v))
		}
		return fmt.Sprintf("call tool %s with %s", p.ToolName, strings.Join(parts, " "))
	case ActionWebNavigate:
		return "navigate to " + p.URL
	default:
		return string(p.Kind)
	}
}

// ActionConfirmer is asked, in confirm autonomy, whether a concrete action
// may proceed. It is called after the agent's postconditions have validated
// the plan and before the side effect. Returning false declines the action;
// the agent then returns its validated plan without executing it.
//
// Confirm autonomy without a confirmer is a contract violation, not a
// silent fallback to run: a gate with nobody at it is not a gate.
type ActionConfirmer func(ActionPreview) bool
