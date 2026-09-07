package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/goweft/cas/internal/agent"
	mcpclient "github.com/goweft/cas/internal/mcp"
)

// offlineConn is an MCP connection with a tool list but no live client.
// Any attempt to Call it fails at the connection layer with a distinctive
// error, so tests can tell "the agent tried to execute" apart from "the
// contract stopped it first".
func offlineConn() *mcpclient.Connection {
	return &mcpclient.Connection{
		ServerURL: "http://fake",
		Tools:     []mcpclient.Tool{{Name: "list_issues", Description: "list issues"}},
	}
}

// TestMCPAgentUnknownToolRefusedBeforeCall: an LLM plan naming a tool the
// server does not expose must be refused by the contract before any call
// reaches the connection. Pre-fix, tool_name_known was checked only after
// Connection.Call — the invented name was sent to the server first and the
// contract saw it afterwards. Reaching the connection layer here is the
// failure case.
func TestMCPAgentUnknownToolRefusedBeforeCall(t *testing.T) {
	for _, mode := range []agent.Autonomy{agent.AutonomyConfirm, agent.AutonomyRun} {
		t.Run(string(mode), func(t *testing.T) {
			fakeLLMPlan(t, `{"tool":"delete_everything","args":{},"summary":"wipe it"}`)
			a := agent.NewMCPAgent()
			_, err := a.Act(context.Background(), agent.MCPRequest{
				Instruction: "clean up",
				Connection:  offlineConn(),
				Autonomy:    mode,
				// confirm mode now requires a confirmer; approve everything so
				// the only thing that can stop the call is the contract.
				Confirm: func(agent.ActionPreview) bool { return true },
			})
			if err == nil {
				t.Fatal("expected contract violation for unknown tool, got nil")
			}
			if strings.Contains(err.Error(), "no live client") {
				t.Fatalf("unknown tool reached the connection layer before the contract: %v", err)
			}
			if !strings.Contains(err.Error(), "tool_name_known") {
				t.Fatalf("expected tool_name_known violation, got: %v", err)
			}
		})
	}
}

// TestMCPAgentKnownToolReachesConnection: the positive control — a plan
// naming a real tool passes the contract and proceeds to execution, which
// fails at the (offline) connection layer. Reaching that layer is the proof
// the contract let a valid plan through.
func TestMCPAgentKnownToolReachesConnection(t *testing.T) {
	fakeLLMPlan(t, `{"tool":"list_issues","args":{"state":"open"},"summary":"list open issues"}`)
	a := agent.NewMCPAgent()
	_, err := a.Act(context.Background(), agent.MCPRequest{
		Instruction: "list open issues",
		Connection:  offlineConn(),
		Autonomy:    agent.AutonomyRun,
	})
	if err == nil || !strings.Contains(err.Error(), "no live client") {
		t.Fatalf("expected the call to reach the connection layer, got: %v", err)
	}
}

// TestMCPAgentSuggestModeUnknownToolRefused: suggest mode never executes,
// but an invented tool name is still not a valid suggestion — the contract
// refuses it there too, so the shell never presents a phantom tool for
// confirmation.
func TestMCPAgentSuggestModeUnknownToolRefused(t *testing.T) {
	fakeLLMPlan(t, `{"tool":"delete_everything","args":{},"summary":"wipe it"}`)
	a := agent.NewMCPAgent()
	_, err := a.Act(context.Background(), agent.MCPRequest{
		Instruction: "clean up",
		Connection:  offlineConn(),
		Autonomy:    agent.AutonomySuggest,
	})
	if err == nil || !strings.Contains(err.Error(), "tool_name_known") {
		t.Fatalf("expected tool_name_known violation, got: %v", err)
	}
}
