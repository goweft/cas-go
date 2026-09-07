package agent_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/goweft/cas/internal/agent"
	"github.com/goweft/cas/internal/webview"
)

// ── Confirm autonomy gates the CONCRETE action ───────────────────────
//
// Review round 3, finding 1: confirm mode approved an abstract step, then
// the nested MCP/Web agent planned and executed under run autonomy — the
// tool name, arguments, and URL were never shown. These tests pin the new
// contract: in confirm autonomy the agent hands the fully-resolved action
// to the Confirm callback after its postconditions pass and before the
// side effect; a decline means the side effect does not happen; and confirm
// autonomy with nobody to ask is a precondition failure, not run mode.

func TestMCPAgentConfirmReceivesConcreteToolCall(t *testing.T) {
	fakeLLMPlan(t, `{"tool":"list_issues","args":{"state":"open","limit":5},"summary":"list"}`)
	var seen *agent.ActionPreview
	_, err := agent.NewMCPAgent().Act(context.Background(), agent.MCPRequest{
		Instruction: "list open issues",
		Connection:  offlineConn(),
		Autonomy:    agent.AutonomyConfirm,
		Confirm: func(p agent.ActionPreview) bool {
			seen = &p
			return true
		},
	})
	if seen == nil {
		t.Fatal("Confirm was never asked")
	}
	if seen.Kind != agent.ActionMCPTool || seen.ToolName != "list_issues" {
		t.Errorf("preview = %+v, want the planned tool", *seen)
	}
	if seen.Arguments["state"] != "open" || seen.Arguments["limit"] != float64(5) {
		t.Errorf("preview arguments = %v, want the planned arguments", seen.Arguments)
	}
	if s := seen.String(); !strings.Contains(s, "list_issues") || !strings.Contains(s, `limit=5`) || !strings.Contains(s, `state="open"`) {
		t.Errorf("preview text hides the action: %q", s)
	}
	// Approved: the call proceeds and reaches the (offline) connection.
	if err == nil || !strings.Contains(err.Error(), "no live client") {
		t.Fatalf("approved call did not reach the connection layer: %v", err)
	}
}

func TestMCPAgentConfirmDeclineDoesNotCall(t *testing.T) {
	fakeLLMPlan(t, `{"tool":"list_issues","args":{"state":"open"},"summary":"list"}`)
	res, err := agent.NewMCPAgent().Act(context.Background(), agent.MCPRequest{
		Instruction: "list open issues",
		Connection:  offlineConn(),
		Autonomy:    agent.AutonomyConfirm,
		Confirm:     func(agent.ActionPreview) bool { return false },
	})
	if err != nil {
		t.Fatalf("a decline is not an error, got: %v", err)
	}
	if !res.Declined {
		t.Error("result must be marked Declined")
	}
	if res.Output != "" {
		t.Errorf("declined call produced output %q — it executed", res.Output)
	}
	if res.ToolCall == nil || res.ToolCall.ToolName != "list_issues" {
		t.Errorf("declined result should still carry the plan: %+v", res.ToolCall)
	}
}

func TestMCPAgentConfirmWithoutConfirmerRefused(t *testing.T) {
	fakeLLMPlan(t, `{"tool":"list_issues","args":{},"summary":"list"}`)
	_, err := agent.NewMCPAgent().Act(context.Background(), agent.MCPRequest{
		Instruction: "list open issues",
		Connection:  offlineConn(),
		Autonomy:    agent.AutonomyConfirm,
	})
	if err == nil || !strings.Contains(err.Error(), "confirm_mode_has_confirmer") {
		t.Fatalf("expected confirm_mode_has_confirmer precondition, got: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "no live client") {
		t.Fatal("confirm mode with no confirmer executed as if it were run mode")
	}
}

// Contract ordering: an invented tool is refused by the postcondition
// before the user is ever asked. The gate must not present phantom tools.
func TestMCPAgentConfirmNotAskedForUnknownTool(t *testing.T) {
	fakeLLMPlan(t, `{"tool":"delete_everything","args":{},"summary":"wipe"}`)
	asked := false
	_, err := agent.NewMCPAgent().Act(context.Background(), agent.MCPRequest{
		Instruction: "clean up",
		Connection:  offlineConn(),
		Autonomy:    agent.AutonomyConfirm,
		Confirm:     func(agent.ActionPreview) bool { asked = true; return true },
	})
	if err == nil || !strings.Contains(err.Error(), "tool_name_known") {
		t.Fatalf("expected tool_name_known, got: %v", err)
	}
	if asked {
		t.Error("user was asked to confirm a tool that does not exist")
	}
}

func TestWebAgentConfirmReceivesExactURL(t *testing.T) {
	target, hits := targetServer(t)
	next := target.URL + "/next?x=1"
	fakeLLMPlan(t, fmt.Sprintf(`{"action":"navigate","navigate_url":%q}`, next))
	req := newWebRequest(t, target.URL, &webview.PageState{URL: target.URL, Text: "a page"})
	req.Autonomy = agent.AutonomyConfirm
	var seen *agent.ActionPreview
	req.Confirm = func(p agent.ActionPreview) bool { seen = &p; return true }

	res, err := agent.NewWebAgent().Act(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if seen == nil || seen.Kind != agent.ActionWebNavigate || seen.URL != next {
		t.Fatalf("Confirm saw %+v, want the exact URL %q", seen, next)
	}
	if hits.Load() != 1 || res.NewPage == nil {
		t.Errorf("approved navigation did not fetch: hits=%d page=%v", hits.Load(), res.NewPage != nil)
	}
}

func TestWebAgentConfirmDeclineDoesNotFetch(t *testing.T) {
	target, hits := targetServer(t)
	next := target.URL + "/next"
	fakeLLMPlan(t, fmt.Sprintf(`{"action":"navigate","navigate_url":%q}`, next))
	req := newWebRequest(t, target.URL, &webview.PageState{URL: target.URL, Text: "a page"})
	req.Autonomy = agent.AutonomyConfirm
	req.Confirm = func(agent.ActionPreview) bool { return false }

	res, err := agent.NewWebAgent().Act(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Declined || res.NewPage != nil {
		t.Errorf("declined navigation executed: %+v", res)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("declined navigation fetched %d time(s)", got)
	}
}

func TestWebAgentConfirmWithoutConfirmerRefused(t *testing.T) {
	target, hits := targetServer(t)
	fakeLLMPlan(t, fmt.Sprintf(`{"action":"navigate","navigate_url":%q}`, target.URL+"/next"))
	req := newWebRequest(t, target.URL, &webview.PageState{URL: target.URL, Text: "a page"})
	req.Autonomy = agent.AutonomyConfirm

	_, err := agent.NewWebAgent().Act(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "confirm_mode_has_confirmer") {
		t.Fatalf("expected confirm_mode_has_confirmer precondition, got: %v", err)
	}
	if hits.Load() != 0 {
		t.Error("confirm mode with no confirmer fetched as if it were run mode")
	}
}

// Off-scope URLs are refused by the contract before the user is asked.
func TestWebAgentConfirmNotAskedForOffScopeURL(t *testing.T) {
	target, hits := targetServer(t)
	fakeLLMPlan(t, `{"action":"navigate","navigate_url":"http://169.254.169.254/latest/meta-data/"}`)
	req := newWebRequest(t, target.URL, &webview.PageState{URL: target.URL, Text: "a page"})
	req.Autonomy = agent.AutonomyConfirm
	asked := false
	req.Confirm = func(agent.ActionPreview) bool { asked = true; return true }

	_, err := agent.NewWebAgent().Act(context.Background(), req)
	if err == nil {
		t.Fatal("expected scope refusal")
	}
	if asked {
		t.Error("user was asked to confirm an out-of-scope URL")
	}
	if hits.Load() != 0 {
		t.Error("unexpected fetch")
	}
}
