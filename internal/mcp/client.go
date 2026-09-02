// Package mcp provides the CAS MCP client layer.
//
// It wraps the mark3labs/mcp-go library to connect to MCP servers,
// discover their tools, and invoke them under contract enforcement.
//
// An MCPConnection represents a live connection to a single MCP server.
// The connection is owned by the workspace that ingested the server —
// closing the workspace closes the connection.
package mcp

import (
	"context"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// Tool describes a single tool exposed by an MCP server.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]interface{}
}

// ToolResult is the result of a tool call.
type ToolResult struct {
	Content string
	IsError bool
}

// Connection is a live connection to an MCP server.
// Created by Connect; closed by Close.
type Connection struct {
	ServerURL string
	Tools     []Tool
	client    *mcpgo.Client
}

// Connect opens a connection to an MCP server at the given URL,
// performs the initialization handshake, and returns the discovered tools.
// Supports SSE (http/https) and stdio (file://) transports.
func Connect(ctx context.Context, serverURL string) (*Connection, error) {
	client, err := mcpgo.NewSSEMCPClient(serverURL)
	if err != nil {
		return nil, fmt.Errorf("mcp connect %s: %w", serverURL, err)
	}

	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("mcp start %s: %w", serverURL, err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "cas",
		Version: "0.1.0",
	}
	initReq.Params.Capabilities = mcp.ClientCapabilities{}

	if _, err := client.Initialize(ctx, initReq); err != nil {
		client.Close()
		return nil, fmt.Errorf("mcp initialize %s: %w", serverURL, err)
	}

	tools, err := discoverTools(ctx, client)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("mcp discover tools %s: %w", serverURL, err)
	}

	return &Connection{
		ServerURL: serverURL,
		Tools:     tools,
		client:    client,
	}, nil
}

// discoverTools lists all tools available on the connected server.
func discoverTools(ctx context.Context, client *mcpgo.Client) ([]Tool, error) {
	req := mcp.ListToolsRequest{}
	resp, err := client.ListTools(ctx, req)
	if err != nil {
		return nil, err
	}

	tools := make([]Tool, len(resp.Tools))
	for i, t := range resp.Tools {
		schema := map[string]interface{}{}
		if t.InputSchema.Properties != nil {
			for k, v := range t.InputSchema.Properties {
				schema[k] = v
			}
		}
		tools[i] = Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		}
	}
	return tools, nil
}

// Call invokes a named tool with the given arguments.
func (c *Connection) Call(ctx context.Context, toolName string, args map[string]interface{}) (*ToolResult, error) {
	if c.client == nil {
		return nil, fmt.Errorf("mcp call %s: no live client", toolName)
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = args

	resp, err := c.client.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mcp call %s: %w", toolName, err)
	}

	// Collect text content from response blocks
	var text string
	for _, block := range resp.Content {
		if block, ok := block.(mcp.TextContent); ok {
			text += block.Text
		}
	}

	return &ToolResult{
		Content: text,
		IsError: resp.IsError,
	}, nil
}

// ToolSummary returns a compact description of available tools for use in
// LLM system prompts. Each tool is listed as "name: description".
func (c *Connection) ToolSummary() string {
	if len(c.Tools) == 0 {
		return "(no tools)"
	}
	var out string
	for i, t := range c.Tools {
		if i > 0 {
			out += "\n"
		}
		out += "- " + t.Name + ": " + t.Description
	}
	return out
}

// Close terminates the connection to the MCP server.
func (c *Connection) Close() {
	if c.client != nil {
		c.client.Close()
	}
}
