package shell

import (
	"context"
	"fmt"
	"strings"

	"github.com/goweft/cas/internal/agent"
	"github.com/goweft/cas/internal/intent"
	casmc "github.com/goweft/cas/internal/mcp"
	"github.com/goweft/cas/internal/webview"
)

// handleIngest connects to an MCP server and materializes it as a workspace.
func (sh *Shell) handleIngest(ctx context.Context, sess *Session, in intent.Intent) (*Response, error) {
	serverURL := in.TitleHint
	if serverURL == "" {
		return &Response{ChatReply: "No server URL found. Usage: ingest <url>", Intent: intent.KindIngest}, nil
	}

	conn, err := casmc.Connect(ctx, serverURL)
	if err != nil {
		return &Response{
			ChatReply: fmt.Sprintf("Failed to connect to %s: %v", serverURL, err),
			Intent:    intent.KindIngest,
		}, nil
	}

	title := "MCP: " + serverURL
	content := formatMCPWorkspace(conn)
	ws, err := sh.workspaces.Create(newID(), "mcp", title, content, sess.ID)
	if err != nil {
		conn.Close()
		return nil, err
	}

	sh.mcpConns[ws.ID] = conn

	reply := fmt.Sprintf("Connected to %s — %d tool(s) available. Tools run as orchestration steps — say \"use this server to <task>\".", serverURL, len(conn.Tools))
	return &Response{ChatReply: reply, Workspace: ws, Intent: intent.KindIngest}, nil
}

// HandleMCPAction executes a user instruction against the MCP workspace.
// Called by the shell when the active workspace is type "mcp".
func (sh *Shell) HandleMCPAction(ctx context.Context, wsID, instruction string, autonomy agent.Autonomy) (*agent.MCPResult, error) {
	conn, ok := sh.mcpConns[wsID]
	if !ok {
		return nil, fmt.Errorf("no MCP connection for workspace %q", wsID)
	}
	return sh.mcpAgent.Act(ctx, agent.MCPRequest{
		Instruction: instruction,
		Connection:  conn,
		Autonomy:    autonomy,
		UserContext: sh.conductor.UserContext(),
		Temperature: 0.3,
	})
}

// CloseMCPWorkspace closes the MCP connection when its workspace is closed.
func (sh *Shell) CloseMCPWorkspace(wsID string) {
	if conn, ok := sh.mcpConns[wsID]; ok {
		conn.Close()
		delete(sh.mcpConns, wsID)
	}
}

// formatMCPWorkspace produces the initial content for an MCP workspace panel.
func formatMCPWorkspace(conn *casmc.Connection) string {
	var sb strings.Builder
	sb.WriteString("# MCP Server\n\n")
	sb.WriteString("**URL:** " + conn.ServerURL + "\n\n")
	sb.WriteString("## Available Tools\n\n")
	for _, t := range conn.Tools {
		sb.WriteString("### " + t.Name + "\n")
		if t.Description != "" {
			sb.WriteString(t.Description + "\n")
		}
		sb.WriteString("\n")
	}
	if len(conn.Tools) == 0 {
		sb.WriteString("_(no tools discovered)_\n")
	}
	return sb.String()
}

// handleBrowse fetches a URL and materializes it as a web workspace.
func (sh *Shell) handleBrowse(ctx context.Context, sess *Session, in intent.Intent) (*Response, error) {
	pageURL := in.TitleHint
	if pageURL == "" {
		return &Response{ChatReply: "No URL found. Usage: browse <url>", Intent: intent.KindBrowse}, nil
	}

	webSess, err := webview.NewSession(ctx, pageURL)
	if err != nil {
		return &Response{
			ChatReply: fmt.Sprintf("Failed to create session for %s: %v", pageURL, err),
			Intent:    intent.KindBrowse,
		}, nil
	}

	page, err := webSess.Navigate(ctx)
	if err != nil {
		webSess.Close()
		return &Response{
			ChatReply: fmt.Sprintf("Failed to fetch %s: %v", pageURL, err),
			Intent:    intent.KindBrowse,
		}, nil
	}

	title := page.Title
	if title == "" {
		title = pageURL
	}
	wsTitle := "Web: " + title
	if len(wsTitle) > 64 {
		wsTitle = wsTitle[:61] + "..."
	}

	content := webview.FormatPageState(page)
	ws, err := sh.workspaces.Create(newID(), "web", wsTitle, content, sess.ID)
	if err != nil {
		webSess.Close()
		return nil, err
	}

	sh.webSessions[ws.ID] = webSess
	sh.webPages[ws.ID] = page

	reply := fmt.Sprintf("Loaded %s — %d headings, %d links found.", title, len(page.Headings), len(page.Links))
	return &Response{ChatReply: reply, Workspace: ws, Intent: intent.KindBrowse}, nil
}

// HandleWebAction executes a user instruction against a web workspace.
func (sh *Shell) HandleWebAction(ctx context.Context, wsID, instruction string, autonomy agent.Autonomy) (*agent.WebResult, error) {
	sess, ok := sh.webSessions[wsID]
	if !ok {
		return nil, fmt.Errorf("no web session for workspace %q", wsID)
	}
	page, ok := sh.webPages[wsID]
	if !ok {
		return nil, fmt.Errorf("no page state for workspace %q", wsID)
	}
	result, err := sh.webAgent.Act(ctx, agent.WebRequest{
		Instruction: instruction,
		Session:     sess,
		PageState:   page,
		Autonomy:    autonomy,
		UserContext: sh.conductor.UserContext(),
		Temperature: 0.3,
	})
	if err != nil {
		return nil, err
	}
	// If the agent navigated, update the stored page state
	if result.NewPage != nil {
		sh.webPages[wsID] = result.NewPage
	}
	return result, nil
}

// CloseWebWorkspace cleans up a web session when its workspace is closed.
func (sh *Shell) CloseWebWorkspace(wsID string) {
	if sess, ok := sh.webSessions[wsID]; ok {
		sess.Close()
		delete(sh.webSessions, wsID)
	}
	delete(sh.webPages, wsID)
}
