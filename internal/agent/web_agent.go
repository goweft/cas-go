// web_agent.go — WebAgent: owns all web workspace LLM interactions.
//
// The WebAgent reasons about what to do with a web page — extract data,
// summarise, navigate to a linked page, answer questions — and acts
// according to the autonomy dial. Like MCPAgent, it never leaves its
// workspace scope: its contract enforces that navigation stays within the
// session's origin or follows a link present on the current page, and the
// check runs before any fetch — an out-of-scope URL is never requested.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/goweft/cas/internal/contract"
	"github.com/goweft/cas/internal/llm"
	"github.com/goweft/cas/internal/webview"
)

// WebAction names what the WebAgent decided to do.
type WebAction string

const (
	WebActionAnswer   WebAction = "answer"   // answer from current page content
	WebActionNavigate WebAction = "navigate" // fetch a new URL
	WebActionExtract  WebAction = "extract"  // extract specific data from the page
)

// WebRequest is the input to WebAgent.
type WebRequest struct {
	Instruction string
	Session     *webview.Session
	PageState   *webview.PageState // current page snapshot
	Autonomy    Autonomy
	UserContext string
	Temperature float64
}

// WebResult is the output from WebAgent.
type WebResult struct {
	Action      WebAction
	Answer      string             // populated for WebActionAnswer and WebActionExtract
	NavigateURL string             // populated for WebActionNavigate
	NewPage     *webview.PageState // populated after navigation
}

// WebAgent handles web workspace interactions.
// It owns all KindBrowse LLM calls and web navigation.
type WebAgent struct{}

// NewWebAgent returns a WebAgent.
func NewWebAgent() *WebAgent { return &WebAgent{} }

// Act reasons about the instruction against the current page state,
// then acts according to the autonomy dial.
func (a *WebAgent) Act(ctx context.Context, req WebRequest) (*WebResult, error) {
	if err := a.contract(req, "").CheckPreconditions(); err != nil {
		return nil, err
	}

	plan, err := a.planAction(ctx, req)
	if err != nil {
		return nil, err
	}

	// In suggest mode, return the plan without acting.
	if req.Autonomy == AutonomySuggest {
		result := &WebResult{Action: plan.action, Answer: plan.answer, NavigateURL: plan.navigateURL}
		if err := a.contract(req, plan.navigateURL).CheckPostconditions(); err != nil {
			return nil, err
		}
		return result, nil
	}

	// confirm and run: execute the action.
	switch plan.action {
	case WebActionNavigate:
		// Validate the planned URL before the fetch: the postconditions
		// judge the LLM's plan, and the fetch is the side effect of that
		// plan — checking afterwards cannot un-send the request.
		if err := a.contract(req, plan.navigateURL).CheckPostconditions(); err != nil {
			return nil, err
		}
		newPage, err := req.Session.Fetch(ctx, plan.navigateURL)
		if err != nil {
			return nil, fmt.Errorf("web-agent: navigate: %w", err)
		}
		return &WebResult{
			Action:      WebActionNavigate,
			NavigateURL: plan.navigateURL,
			NewPage:     newPage,
		}, nil

	default: // answer or extract — no network call
		result := &WebResult{Action: plan.action, Answer: plan.answer}
		if err := a.contract(req, "").CheckPostconditions(); err != nil {
			return nil, err
		}
		return result, nil
	}
}

type webPlan struct {
	action      WebAction
	answer      string
	navigateURL string
}

// planAction asks the LLM what to do with the current page.
func (a *WebAgent) planAction(ctx context.Context, req WebRequest) (*webPlan, error) {
	sys := a.systemPrompt(req)
	pageContext := formatPageContext(req.PageState)

	msgs := []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: pageContext + "\n\nInstruction: " + req.Instruction},
	}

	raw, err := llm.Complete(ctx, msgs, llm.ModelFor("chat"), req.Temperature)
	if err != nil {
		return nil, fmt.Errorf("web-agent: plan: %w", err)
	}

	return parseWebPlanResponse(raw), nil
}

// systemPrompt builds the system prompt for web action planning.
func (a *WebAgent) systemPrompt(req WebRequest) string {
	var sb strings.Builder
	sb.WriteString("You are an assistant operating inside CAS (Conversational Agent Shell).\n")
	sb.WriteString("You are working with a web page. You can do three things:\n\n")
	sb.WriteString("1. ANSWER — answer a question from the page content directly.\n")
	sb.WriteString("2. NAVIGATE — go to a different URL (must be a real link from the page).\n")
	sb.WriteString("3. EXTRACT — pull out specific structured data from the page.\n\n")
	sb.WriteString("Respond with ONLY a JSON object:\n")
	sb.WriteString(`{"action": "answer"|"navigate"|"extract", "answer": "<text>", "navigate_url": "<url>"}`)
	sb.WriteString("\n\nFor 'answer' and 'extract': populate 'answer'. Leave 'navigate_url' empty.")
	sb.WriteString("\nFor 'navigate': populate 'navigate_url' with a URL from the page links. Leave 'answer' empty.")
	sb.WriteString("\n\nScope rule: navigate_url must be a real URL visible in the page content. Never invent URLs.")
	if req.UserContext != "" {
		sb.WriteString("\n\nUser context: " + req.UserContext)
	}
	return sb.String()
}

// parseWebPlanResponse parses a JSON action plan from the LLM.
// Falls back to a plain-text answer if the response is not JSON.
func parseWebPlanResponse(raw string) *webPlan {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		if len(lines) > 2 {
			raw = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var v struct {
		Action      string `json:"action"`
		Answer      string `json:"answer"`
		NavigateURL string `json:"navigate_url"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		// Plain text — treat as answer
		return &webPlan{action: WebActionAnswer, answer: strings.TrimSpace(raw)}
	}

	switch WebAction(v.Action) {
	case WebActionNavigate:
		return &webPlan{action: WebActionNavigate, navigateURL: v.NavigateURL}
	case WebActionExtract:
		return &webPlan{action: WebActionExtract, answer: v.Answer}
	default:
		return &webPlan{action: WebActionAnswer, answer: v.Answer}
	}
}

// formatPageContext produces a compact page summary for the LLM prompt.
func formatPageContext(ps *webview.PageState) string {
	if ps == nil {
		return "(no page loaded)"
	}
	var sb strings.Builder
	sb.WriteString("Current page: " + ps.URL + "\n")
	if ps.Title != "" {
		sb.WriteString("Title: " + ps.Title + "\n")
	}
	if len(ps.Headings) > 0 {
		sb.WriteString("Headings: " + strings.Join(ps.Headings, " | ") + "\n")
	}
	if len(ps.Links) > 0 {
		sb.WriteString("Links:\n")
		for _, l := range ps.Links {
			sb.WriteString("  - " + l.Text + ": " + l.Href + "\n")
		}
	}
	// Truncate text to ~1 KB for prompt
	text := ps.Text
	if len(text) > 1024 {
		text = text[:1024] + "…"
	}
	if text != "" {
		sb.WriteString("Content excerpt:\n" + text + "\n")
	}
	return sb.String()
}

// contract builds and freezes the WebAgent contract.
func (a *WebAgent) contract(req WebRequest, navigateURL string) *contract.Contract {
	c := contract.New("web-agent")
	c.Preconditions = []contract.Rule{
		{
			Name:        "instruction_not_empty",
			Description: "instruction must not be empty",
			Check:       func() bool { return strings.TrimSpace(req.Instruction) != "" },
		},
		{
			Name:        "session_present",
			Description: "a web session must be provided",
			Check:       func() bool { return req.Session != nil },
		},
		{
			Name:        "page_state_present",
			Description: "a page state must be provided",
			Check:       func() bool { return req.PageState != nil },
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
			Name:        "navigate_url_valid",
			Description: "navigate_url must be a parseable absolute URL if set",
			Check: func() bool {
				if navigateURL == "" {
					return true
				}
				u, err := url.Parse(navigateURL)
				return err == nil && u.IsAbs()
			},
		},
		{
			Name:        "navigate_url_in_scope",
			Description: "navigate_url must share the session's origin or be a link on the current page",
			Check: func() bool {
				if navigateURL == "" {
					return true
				}
				return navigateURLInScope(req, navigateURL)
			},
		},
	}
	return c.Freeze()
}

// navigateURLInScope reports whether a planned navigation target is
// permitted: it must share the session's start-URL origin (scheme + host)
// or appear verbatim among the current page's extracted links (which the
// webview layer resolves to absolute URLs). The contract runs this before
// any fetch, so out-of-scope URLs are refused without a request. A URL
// that merely appears in page text — the prompt-injection channel — is
// neither, and fails closed.
func navigateURLInScope(req WebRequest, target string) bool {
	u, err := url.Parse(target)
	if err != nil || !u.IsAbs() {
		return false
	}
	if req.Session != nil {
		if start, err := url.Parse(req.Session.StartURL); err == nil &&
			strings.EqualFold(u.Scheme, start.Scheme) &&
			strings.EqualFold(u.Host, start.Host) {
			return true
		}
	}
	if req.PageState != nil {
		for _, l := range req.PageState.Links {
			if l.Href == target {
				return true
			}
		}
	}
	return false
}
