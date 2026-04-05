// Package llm provides the multi-provider LLM bridge for CAS.
// Provider is selected via CAS_PROVIDER env var (ollama | anthropic).
// Model routing maps workspace type → model name per provider.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Provider selects the inference backend.
type Provider string

const (
	ProviderOllama    Provider = "ollama"
	ProviderAnthropic Provider = "anthropic"
)

// ActiveProvider returns the provider from CAS_PROVIDER env (default: ollama).
func ActiveProvider() Provider {
	p := strings.ToLower(os.Getenv("CAS_PROVIDER"))
	if p == "anthropic" {
		return ProviderAnthropic
	}
	return ProviderOllama
}

// defaultModels maps provider → wsType → model name.
var defaultModels = map[Provider]map[string]string{
	ProviderOllama: {
		"document": "qwen3.5:9b",
		"list":     "qwen3.5:9b",
		"code":     "qwen2.5-coder:7b",
		"chat":     "qwen3.5:9b",
	},
	ProviderAnthropic: {
		"document": "claude-sonnet-4-6",
		"list":     "claude-sonnet-4-6",
		"code":     "claude-haiku-4-5-20251001",
		"chat":     "claude-sonnet-4-6",
	},
}

// ModelFor returns the model name for a workspace type and the active provider.
// CAS_MODEL_{TYPE} env var overrides the default.
func ModelFor(wsType string) string {
	envKey := "CAS_MODEL_" + strings.ToUpper(wsType)
	if override := os.Getenv(envKey); override != "" {
		return override
	}
	p := ActiveProvider()
	if m, ok := defaultModels[p][wsType]; ok {
		return m
	}
	return defaultModels[p]["document"]
}

// Message is a single turn in a conversation.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Complete sends messages to the active provider and returns the full response.
func Complete(ctx context.Context, messages []Message, model string, temperature float64) (string, error) {
	switch ActiveProvider() {
	case ProviderAnthropic:
		return anthropicComplete(ctx, messages, model, temperature)
	default:
		return ollamaComplete(ctx, messages, model, temperature)
	}
}

// Stream sends messages and calls onToken for each streamed token.
// Returns the full accumulated text when done.
func Stream(ctx context.Context, messages []Message, model string, temperature float64, onToken func(string)) (string, error) {
	switch ActiveProvider() {
	case ProviderAnthropic:
		return anthropicStream(ctx, messages, model, temperature, onToken)
	default:
		return ollamaStream(ctx, messages, model, temperature, onToken)
	}
}

// ── Ollama ────────────────────────────────────────────────────────

func ollamaBase() string {
	if base := os.Getenv("OLLAMA_BASE_URL"); base != "" {
		return base
	}
	return "http://localhost:11434"
}

type ollamaRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Options  struct {
		Temperature float64 `json:"temperature"`
	} `json:"options"`
}

type ollamaResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done bool `json:"done"`
}

func ollamaComplete(ctx context.Context, messages []Message, model string, temperature float64) (string, error) {
	req := ollamaRequest{Model: model, Messages: messages, Stream: false}
	req.Options.Temperature = temperature

	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ollamaBase()+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()

	var result ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("ollama decode: %w", err)
	}
	return strings.TrimSpace(result.Message.Content), nil
}

func ollamaStream(ctx context.Context, messages []Message, model string, temperature float64, onToken func(string)) (string, error) {
	req := ollamaRequest{Model: model, Messages: messages, Stream: true}
	req.Options.Temperature = temperature

	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ollamaBase()+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("ollama stream: %w", err)
	}
	defer resp.Body.Close()

	var buf strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var chunk ollamaResponse
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		token := chunk.Message.Content
		if token != "" {
			onToken(token)
			buf.WriteString(token)
		}
		if chunk.Done {
			break
		}
	}
	return buf.String(), scanner.Err()
}

// ── Anthropic ────────────────────────────────────────────────────

const anthropicBase = "https://api.anthropic.com"
const anthropicVersion = "2023-06-01"

func anthropicKey() (string, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "", fmt.Errorf("ANTHROPIC_API_KEY is not set")
	}
	return key, nil
}

type anthropicRequest struct {
	Model       string    `json:"model"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
}

// splitSystem separates a leading system message from user/assistant turns.
func splitSystem(messages []Message) (system string, rest []Message) {
	if len(messages) > 0 && messages[0].Role == "system" {
		return messages[0].Content, messages[1:]
	}
	return "", messages
}

func anthropicComplete(ctx context.Context, messages []Message, model string, temperature float64) (string, error) {
	key, err := anthropicKey()
	if err != nil {
		return "", err
	}

	system, rest := splitSystem(messages)
	req := anthropicRequest{
		Model:       model,
		MaxTokens:   4096,
		Temperature: temperature,
		System:      system,
		Messages:    rest,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		anthropicBase+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", key)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("anthropic: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("anthropic decode: %w", err)
	}

	var sb strings.Builder
	for _, block := range result.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

func anthropicStream(ctx context.Context, messages []Message, model string, temperature float64, onToken func(string)) (string, error) {
	key, err := anthropicKey()
	if err != nil {
		return "", err
	}

	system, rest := splitSystem(messages)
	req := anthropicRequest{
		Model:       model,
		MaxTokens:   4096,
		Temperature: temperature,
		System:      system,
		Messages:    rest,
		Stream:      true,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		anthropicBase+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", key)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("anthropic stream: %w", err)
	}
	defer resp.Body.Close()

	var buf strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if event.Type == "content_block_delta" && event.Delta.Type == "text_delta" {
			token := event.Delta.Text
			if token != "" {
				onToken(token)
				buf.WriteString(token)
			}
		}
	}
	return buf.String(), scanner.Err()
}

// BuildMessages constructs the message slice for a given prompt type.
func BuildWorkspaceMessages(system, title, userMessage string) []Message {
	return []Message{
		{Role: "system", Content: system},
		{Role: "user", Content: fmt.Sprintf("Title: %s\nRequest: %s", title, userMessage)},
	}
}

func BuildEditMessages(system, title, current, editRequest string) []Message {
	return []Message{
		{Role: "system", Content: system},
		{Role: "user", Content: fmt.Sprintf(
			"Title: %s\n\nCurrent content:\n%s\n\nChange request: %s",
			title, current, editRequest,
		)},
	}
}

func BuildChatMessages(system string, history []Message, userMessage string) []Message {
	msgs := make([]Message, 0, 1+len(history)+1)
	msgs = append(msgs, Message{Role: "system", Content: system})
	if len(history) > 6 {
		history = history[len(history)-6:]
	}
	msgs = append(msgs, history...)
	msgs = append(msgs, Message{Role: "user", Content: userMessage})
	return msgs

}

// ── System prompts ────────────────────────────────────────────────

var WorkspaceSystem = map[string]string{
	"document": "You are a document drafting assistant. " +
		"Produce a well-structured markdown document with appropriate headings, sections, and content. " +
		"Output only the document — no preamble, no explanation, no code fences.",
	"code": "You are a coding assistant. " +
		"Produce clean, well-commented code that fulfils the request. " +
		"Output ONLY the raw code — no markdown fences, no explanation. " +
		"Start directly with the code.",
	"list": "You are a list-making assistant. " +
		"Produce a clean, structured markdown list with a top-level heading. " +
		"Output only the list — no preamble, no explanation.",
}

var EditSystem = map[string]string{
	"document": "You are a precise document editor. " +
		"Apply the requested change and return the complete updated content in markdown. " +
		"Preserve all existing sections not affected by the change. Output only the updated document.",
	"code": "You are a precise code editor. " +
		"Apply the requested change and return the complete updated code. " +
		"Preserve all existing logic not affected by the change. Output only the updated code.",
	"list": "You are a precise list editor. " +
		"Apply the requested change and return the complete updated list in markdown. " +
		"Preserve all existing items not affected by the change. Output only the updated list.",
}

const ChatSystem = `You are CAS — a Conversational Agent Shell.
CAS creates and edits workspaces (documents, code, lists) from conversation.
Workspace operations are handled by the routing layer — you never simulate them.

RULES:
- Never say "workspace created" or "I've saved that" — only the routing layer does that.
- Never ask about names, types, or where to save. CAS handles all of that.
- Keep responses short. This is a shell, not a chatbot.
- Plain text only in chat replies — no markdown formatting.
- If the user wants a workspace, tell them: write a [document type].`

// SystemFor returns the system prompt for a given wsType, with optional user context appended.
func SystemFor(prompts map[string]string, wsType, userContext string) string {
	base, ok := prompts[wsType]
	if !ok {
		base = prompts["document"]
	}
	if userContext != "" {
		return base + "\n\nUser context: " + userContext
	}
	return base
}

// ReadAll drains an io.Reader — useful in tests.
func ReadAll(r io.Reader) string {
	b, _ := io.ReadAll(r)
	return string(b)
}
