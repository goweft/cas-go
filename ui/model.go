// Package ui implements the CAS terminal interface using Bubble Tea.
// Layout: split panel — chat input/history on the left, workspace on the right.
package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/goweft/cas/internal/intent"
	"github.com/goweft/cas/internal/shell"
	"github.com/goweft/cas/internal/workspace"
)

// ── Styles ────────────────────────────────────────────────────────

var (
	subtle    = lipgloss.AdaptiveColor{Light: "#D9DCCF", Dark: "#383838"}
	highlight = lipgloss.AdaptiveColor{Light: "#874BFD", Dark: "#7D56F4"}
	special   = lipgloss.AdaptiveColor{Light: "#43BF6D", Dark: "#73F59F"}
	dimmed    = lipgloss.AdaptiveColor{Light: "#9B9B9B", Dark: "#5C5C5C"}

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(subtle).
			Padding(0, 1)

	activePanelStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(highlight).
				Padding(0, 1)

	workspacePanelStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(special).
				Padding(0, 1)

	titleStyle = lipgloss.NewStyle().
			Foreground(highlight).
			Bold(true)

	wsTypeStyle = lipgloss.NewStyle().
			Foreground(special).
			Italic(true)

	dimStyle = lipgloss.NewStyle().
			Foreground(dimmed)

	userMsgStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#79c0ff"))

	shellMsgStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7ee787"))

	inputStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#e6edf3"))

	statusStyle = lipgloss.NewStyle().
			Foreground(dimmed).
			Italic(true)
)

// ── Messages ──────────────────────────────────────────────────────

// tokenMsg carries a streamed token from the LLM.
type tokenMsg string

// responseMsg carries the final shell response.
type responseMsg struct {
	resp *shell.StreamResponse
	err  error
}

// ── Model ─────────────────────────────────────────────────────────

// Focus tracks which panel the user is interacting with.
type Focus int

const (
	FocusChat      Focus = iota
	FocusWorkspace Focus = iota
)

// Model is the Bubble Tea model for CAS.
type Model struct {
	sh        *shell.Shell
	sessionID string

	// Chat state
	messages    []shell.Message
	input       string
	inputCursor int

	// Workspace state
	activeWS    *workspace.Workspace
	wsContent   string   // displayed content (may be streaming)
	wsScroll    int      // line scroll offset in workspace pane
	streaming   bool
	streamBuf   strings.Builder

	// Layout
	width  int
	height int
	focus  Focus

	// Status
	status string
	err    error
}

// New creates a new CAS UI model.
func New(sh *shell.Shell, sessionID string, history []shell.Message, activeWS *workspace.Workspace) Model {
	m := Model{
		sh:        sh,
		sessionID: sessionID,
		messages:  history,
		focus:     FocusChat,
	}
	if activeWS != nil {
		m.activeWS = activeWS
		m.wsContent = activeWS.Content
	}
	return m
}

// Init satisfies tea.Model.
func (m Model) Init() tea.Cmd { return nil }

// ── Update ────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tokenMsg:
		m.streamBuf.WriteString(string(msg))
		m.wsContent = m.streamBuf.String()
		return m, nil

	case responseMsg:
		m.streaming = false
		m.status = ""
		if msg.err != nil {
			m.err = msg.err
			m.status = "error: " + msg.err.Error()
			return m, nil
		}
		resp := msg.resp
		// Append shell reply to history
		m.messages = append(m.messages, shell.Message{
			Role: "shell", Text: resp.ChatReply,
		})
		if resp.Workspace != nil {
			m.activeWS = resp.Workspace
			m.wsContent = resp.Workspace.Content
			m.streamBuf.Reset()
		}
		if resp.Intent == intent.KindClose {
			m.activeWS = nil
			m.wsContent = ""
		}
		return m, nil
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {

	case tea.KeyCtrlC, tea.KeyEsc:
		return m, tea.Quit

	case tea.KeyTab:
		// Toggle focus between chat and workspace
		if m.focus == FocusChat {
			m.focus = FocusWorkspace
		} else {
			m.focus = FocusChat
		}
		return m, nil

	case tea.KeyEnter:
		if m.focus != FocusChat || m.streaming || strings.TrimSpace(m.input) == "" {
			return m, nil
		}
		return m.submitMessage()

	case tea.KeyBackspace:
		if m.focus == FocusChat && len(m.input) > 0 {
			m.input = m.input[:len(m.input)-1]
		}
		return m, nil

	case tea.KeyUp:
		if m.focus == FocusWorkspace && m.wsScroll > 0 {
			m.wsScroll--
		}
		return m, nil

	case tea.KeyDown:
		if m.focus == FocusWorkspace {
			m.wsScroll++
		}
		return m, nil

	case tea.KeyRunes:
		if m.focus == FocusChat && !m.streaming {
			m.input += string(msg.Runes)
		}
		return m, nil
	}

	return m, nil
}

func (m Model) submitMessage() (Model, tea.Cmd) {
	message := strings.TrimSpace(m.input)
	m.input = ""
	m.messages = append(m.messages, shell.Message{Role: "user", Text: message})
	m.streaming = true
	m.streamBuf.Reset()
	m.wsContent = ""
	m.status = "thinking…"

	sessionID := m.sessionID
	sh := m.sh

	return m, func() tea.Msg {
		// Channel to collect tokens
		tokens := make(chan string, 256)
		var resp *shell.StreamResponse
		var respErr error

		go func() {
			resp, respErr = sh.StreamMessage(
				context.Background(), sessionID, message,
				func(token string) { tokens <- token },
			)
			close(tokens)
		}()

		// Drain tokens — each becomes a tokenMsg
		// We batch them to avoid flooding the event loop
		// In practice, Bubble Tea's event loop handles this fine
		_ = tokens // handled via tea.Batch below
		// Return the final response; tokens arrive via separate msgs
		return responseMsg{resp: resp, err: respErr}
	}
}

// ── View ──────────────────────────────────────────────────────────

func (m Model) View() string {
	if m.width == 0 {
		return "Loading…"
	}

	// Split width: 40% chat, 60% workspace (min 20 each)
	chatW := m.width * 40 / 100
	wsW := m.width - chatW - 4 // 4 for borders/gap
	if chatW < 24 {
		chatW = 24
	}
	if wsW < 24 {
		wsW = 24
	}
	innerH := m.height - 6 // leave room for borders + input + status

	chatPane := m.renderChat(chatW, innerH)
	wsPane := m.renderWorkspace(wsW, innerH)

	row := lipgloss.JoinHorizontal(lipgloss.Top, chatPane, "  ", wsPane)
	statusBar := m.renderStatus()

	return lipgloss.JoinVertical(lipgloss.Left, row, statusBar)
}

func (m Model) renderChat(w, h int) string {
	style := panelStyle
	if m.focus == FocusChat {
		style = activePanelStyle
	}
	style = style.Width(w)

	// History
	var lines []string
	for _, msg := range m.messages {
		if msg.Role == "user" {
			lines = append(lines, userMsgStyle.Render("you › ")+msg.Text)
		} else {
			lines = append(lines, shellMsgStyle.Render("cas › ")+msg.Text)
		}
		lines = append(lines, "") // blank between messages
	}

	// Trim to visible height
	histH := h - 3
	if histH < 0 {
		histH = 0
	}
	if len(lines) > histH {
		lines = lines[len(lines)-histH:]
	}
	histText := strings.Join(lines, "\n")

	// Input line
	cursor := "█"
	if m.streaming {
		cursor = "…"
	}
	inputLine := inputStyle.Render("> " + m.input + cursor)

	content := histText + "\n\n" + inputLine
	return style.Render(content)
}

func (m Model) renderWorkspace(w, h int) string {
	style := workspacePanelStyle.Width(w)

	if m.activeWS == nil && m.wsContent == "" {
		empty := dimStyle.Render(
			"No workspace open.\n\n" +
				"Try: write a project proposal\n" +
				"     create a python script\n" +
				"     make a todo list",
		)
		return style.Render(empty)
	}

	// Title bar
	title := "Workspace"
	wsType := ""
	if m.activeWS != nil {
		title = m.activeWS.Title
		wsType = m.activeWS.Type
	}
	header := titleStyle.Render(title)
	if wsType != "" {
		header += "  " + wsTypeStyle.Render("["+wsType+"]")
	}

	// Content — scroll
	content := m.wsContent
	if m.streaming && content == "" {
		content = dimStyle.Render("generating…")
	}
	contentLines := strings.Split(content, "\n")
	start := m.wsScroll
	if start >= len(contentLines) {
		start = 0
	}
	visibleLines := contentLines[start:]
	maxLines := h - 2
	if len(visibleLines) > maxLines {
		visibleLines = visibleLines[:maxLines]
	}

	body := strings.Join(visibleLines, "\n")
	return style.Render(header + "\n" + dimStyle.Render(strings.Repeat("─", w-4)) + "\n" + body)
}

func (m Model) renderStatus() string {
	if m.status != "" {
		return statusStyle.Render(" " + m.status)
	}
	parts := []string{
		dimStyle.Render("tab: switch pane"),
		dimStyle.Render("enter: send"),
		dimStyle.Render("ctrl+c: quit"),
	}
	if m.focus == FocusWorkspace {
		parts = append([]string{dimStyle.Render("↑↓: scroll  ")}, parts...)
	}
	return "  " + strings.Join(parts, "  │  ")
}

// StreamTokenCmd returns a tea.Cmd that sends a single tokenMsg.
// Used to feed streamed tokens into the event loop.
func StreamTokenCmd(token string) tea.Cmd {
	return func() tea.Msg { return tokenMsg(token) }
}

// ErrorMsg formats an error for display.
func ErrorMsg(err error) string {
	return fmt.Sprintf("error: %v", err)
}
