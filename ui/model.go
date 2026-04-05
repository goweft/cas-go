// Package ui implements the CAS terminal interface using Bubble Tea.
//
// Layout: split panel — chat (40%) on the left, workspace (60%) on the right.
//
// Streaming pattern: a channel stored in Model feeds one token per tea.Cmd
// tick into the event loop. This is the correct Bubble Tea approach — a
// goroutine fills the channel, a recursive cmd drains it one item at a time,
// and a final responseMsg signals completion.
package ui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/glamour"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/goweft/cas/internal/intent"
	"github.com/goweft/cas/internal/shell"
	"github.com/goweft/cas/internal/workspace"
)

// ── Palette ───────────────────────────────────────────────────────

var (
	colBorder    = lipgloss.AdaptiveColor{Light: "#C8C6C0", Dark: "#383838"}
	colActive    = lipgloss.AdaptiveColor{Light: "#874BFD", Dark: "#7D56F4"}
	colWorkspace = lipgloss.AdaptiveColor{Light: "#43BF6D", Dark: "#73F59F"}
	colDim       = lipgloss.AdaptiveColor{Light: "#9B9B9B", Dark: "#5C5C5C"}

	stylePanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colBorder).
			Padding(0, 1)

	styleActivePanel = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colActive).
				Padding(0, 1)

	styleWSPanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colWorkspace).
			Padding(0, 1)

	styleTitle  = lipgloss.NewStyle().Foreground(colActive).Bold(true)
	styleWSType = lipgloss.NewStyle().Foreground(colWorkspace).Italic(true)
	styleDim    = lipgloss.NewStyle().Foreground(colDim)
	styleUser   = lipgloss.NewStyle().Foreground(lipgloss.Color("#79c0ff"))
	styleShell  = lipgloss.NewStyle().Foreground(lipgloss.Color("#7ee787"))
	styleInput  = lipgloss.NewStyle().Foreground(lipgloss.Color("#e6edf3"))
	styleStatus = lipgloss.NewStyle().Foreground(colDim).Italic(true)
	styleCode   = lipgloss.NewStyle().Foreground(lipgloss.Color("#e6edf3"))
)

// ── Stream event ─────────────────────────────────────────────────
// streamEvent is a discriminated union sent over the stream channel.
// Either Token is set (mid-stream) or Resp/Err are set (final).

type streamEvent struct {
	Token string
	Resp  *shell.StreamResponse
	Err   error
}

// ── Tea messages ──────────────────────────────────────────────────

type tokenMsg string

type responseMsg struct {
	resp *shell.StreamResponse
	err  error
}

// ── Focus ─────────────────────────────────────────────────────────

type Focus int

const (
	FocusChat      Focus = iota
	FocusWorkspace Focus = iota
)

// ── Model ─────────────────────────────────────────────────────────

type Model struct {
	sh        *shell.Shell
	sessionID string

	// Chat
	messages   []shell.Message
	input      string
	chatScroll int // how many lines scrolled up in chat history

	// Workspace
	activeWS  *workspace.Workspace
	wsContent string // raw accumulated content (may be mid-stream)
	wsScroll  int    // line offset in workspace pane

	// Streaming
	streaming bool
	streamBuf strings.Builder
	streamCh  chan streamEvent // non-nil while streaming

	// Layout
	width  int
	height int
	focus  Focus

	// Status / error
	status string
}

// New returns a model seeded with existing history and active workspace.
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
		// One token arrived — update display and schedule reading the next
		m.streamBuf.WriteString(string(msg))
		m.wsContent = m.streamBuf.String()
		return m, listenStream(m.streamCh)

	case responseMsg:
		// Stream complete
		m.streaming = false
		m.streamCh = nil
		m.status = ""
		if msg.err != nil {
			m.status = "error: " + msg.err.Error()
			return m, nil
		}
		resp := msg.resp
		m.messages = append(m.messages, shell.Message{
			Role: "shell", Text: resp.ChatReply,
		})
		if resp.Workspace != nil {
			m.activeWS = resp.Workspace
			m.wsContent = resp.Workspace.Content
			m.wsScroll = 0
		}
		if resp.Intent == intent.KindClose {
			m.activeWS = nil
			m.wsContent = ""
		}
		m.streamBuf.Reset()
		return m, nil
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {

	case tea.KeyCtrlC:
		return m, tea.Quit

	case tea.KeyEsc:
		if m.focus == FocusWorkspace {
			m.focus = FocusChat
		} else {
			return m, tea.Quit
		}
		return m, nil

	case tea.KeyTab:
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
			runes := []rune(m.input)
			m.input = string(runes[:len(runes)-1])
		}
		return m, nil

	case tea.KeyUp:
		switch m.focus {
		case FocusWorkspace:
			if m.wsScroll > 0 {
				m.wsScroll--
			}
		case FocusChat:
			if m.chatScroll < len(m.messages) {
				m.chatScroll++
			}
		}
		return m, nil

	case tea.KeyDown:
		switch m.focus {
		case FocusWorkspace:
			m.wsScroll++
		case FocusChat:
			if m.chatScroll > 0 {
				m.chatScroll--
			}
		}
		return m, nil

	case tea.KeyPgUp:
		m.wsScroll -= 10
		if m.wsScroll < 0 {
			m.wsScroll = 0
		}
		return m, nil

	case tea.KeyPgDown:
		m.wsScroll += 10
		return m, nil

	case tea.KeyRunes:
		if m.focus == FocusChat && !m.streaming {
			m.input += string(msg.Runes)
		}
		return m, nil
	}

	return m, nil
}

// submitMessage sends the input to the shell and sets up the stream loop.
func (m Model) submitMessage() (Model, tea.Cmd) {
	message := strings.TrimSpace(m.input)
	m.input = ""
	m.messages = append(m.messages, shell.Message{Role: "user", Text: message})
	m.streaming = true
	m.streamBuf.Reset()
	m.wsContent = ""
	m.wsScroll = 0
	m.status = "thinking…"

	// Buffered channel — goroutine never blocks on slow event loop
	ch := make(chan streamEvent, 512)
	m.streamCh = ch

	sessionID := m.sessionID
	sh := m.sh

	go func() {
		resp, err := sh.StreamMessage(
			context.Background(), sessionID, message,
			func(token string) { ch <- streamEvent{Token: token} },
		)
		ch <- streamEvent{Resp: resp, Err: err}
		close(ch)
	}()

	return m, listenStream(ch)
}

// listenStream returns a tea.Cmd that reads exactly one event from ch.
// Called recursively from Update until the stream closes.
func listenStream(ch chan streamEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			// Channel closed without sending a final event — shouldn't happen
			return responseMsg{err: fmt.Errorf("stream closed unexpectedly")}
		}
		if ev.Resp != nil || ev.Err != nil {
			return responseMsg{resp: ev.Resp, err: ev.Err}
		}
		return tokenMsg(ev.Token)
	}
}

// ── View ──────────────────────────────────────────────────────────

func (m Model) View() string {
	if m.width == 0 {
		return "Loading…"
	}

	// 40% chat, 60% workspace, leaving 2 chars between panels
	chatW := m.width * 40 / 100
	wsW := m.width - chatW - 2
	if chatW < 28 {
		chatW = 28
	}
	if wsW < 28 {
		wsW = 28
	}
	innerH := m.height - 4

	chatPane := m.renderChat(chatW, innerH)
	wsPane := m.renderWorkspace(wsW, innerH)
	row := lipgloss.JoinHorizontal(lipgloss.Top, chatPane, " ", wsPane)

	return lipgloss.JoinVertical(lipgloss.Left, row, m.renderStatus())
}

func (m Model) renderChat(w, h int) string {
	st := stylePanel
	if m.focus == FocusChat {
		st = styleActivePanel
	}
	st = st.Width(w - 2) // subtract border

	// Build message lines (newest last)
	var lines []string
	for _, msg := range m.messages {
		wrapped := wordWrap(msg.Text, w-6)
		if msg.Role == "user" {
			for i, l := range wrapped {
				if i == 0 {
					lines = append(lines, styleUser.Render("you › ")+l)
				} else {
					lines = append(lines, "      "+l)
				}
			}
		} else {
			for i, l := range wrapped {
				if i == 0 {
					lines = append(lines, styleShell.Render("cas › ")+l)
				} else {
					lines = append(lines, "      "+l)
				}
			}
		}
		lines = append(lines, "")
	}

	// Input area takes 2 lines at bottom
	histH := h - 5
	if histH < 0 {
		histH = 0
	}

	// Apply chat scroll (scroll up = show older messages)
	total := len(lines)
	end := total - m.chatScroll
	if end < 0 {
		end = 0
	}
	start := end - histH
	if start < 0 {
		start = 0
	}
	visible := lines[start:end]
	// Pad to fill height
	for len(visible) < histH {
		visible = append([]string{""}, visible...)
	}

	cursor := "█"
	if m.streaming {
		cursor = styleDim.Render("…")
	}
	inputLine := styleInput.Render("> " + m.input + cursor)

	sep := styleDim.Render(strings.Repeat("─", w-4))
	content := strings.Join(visible, "\n") + "\n" + sep + "\n" + inputLine
	return st.Render(content)
}

func (m Model) renderWorkspace(w, h int) string {
	st := styleWSPanel.Width(w - 2)

	if m.activeWS == nil && !m.streaming {
		hint := styleDim.Render(
			"No workspace open.\n\n" +
				"  write a project proposal\n" +
				"  create a python script\n" +
				"  make a todo list",
		)
		return st.Render(hint)
	}

	// Header
	title := "generating…"
	wsType := ""
	if m.activeWS != nil {
		title = m.activeWS.Title
		wsType = m.activeWS.Type
	}
	header := styleTitle.Render(title)
	if wsType != "" {
		header += "  " + styleWSType.Render("["+wsType+"]")
	}
	sep := styleDim.Render(strings.Repeat("─", w-4))

	// Content — render markdown for document/list, raw for code
	contentArea := h - 3 // header + sep + padding
	body := m.renderContent(wsType, m.wsContent, w-4, contentArea)

	return st.Render(header + "\n" + sep + "\n" + body)
}

// renderContent applies glamour markdown for documents and lists,
// raw monospace for code, with scroll offset applied.
func (m Model) renderContent(wsType, content string, w, h int) string {
	if content == "" {
		if m.streaming {
			return styleDim.Render("generating…")
		}
		return ""
	}

	var rendered string
	if wsType == "code" {
		// Raw code — monospace, no markdown processing
		rendered = styleCode.Render(content)
	} else {
		// Markdown via glamour
		renderer, err := glamour.NewTermRenderer(
			glamour.WithAutoStyle(),
			glamour.WithWordWrap(w),
		)
		if err == nil {
			if out, err := renderer.Render(content); err == nil {
				rendered = strings.TrimRight(out, "\n")
			} else {
				rendered = content
			}
		} else {
			rendered = content
		}
	}

	lines := strings.Split(rendered, "\n")

	// Clamp scroll
	maxScroll := len(lines) - h
	if maxScroll < 0 {
		maxScroll = 0
	}
	scroll := m.wsScroll
	if scroll > maxScroll {
		scroll = maxScroll
	}

	end := scroll + h
	if end > len(lines) {
		end = len(lines)
	}
	visible := lines[scroll:end]

	// Scroll indicator
	if len(lines) > h {
		pct := 0
		if maxScroll > 0 {
			pct = scroll * 100 / maxScroll
		}
		indicator := styleDim.Render(fmt.Sprintf(" ↕ %d%% ", pct))
		if len(visible) > 0 {
			visible[len(visible)-1] = indicator
		}
	}

	return strings.Join(visible, "\n")
}

func (m Model) renderStatus() string {
	if m.status != "" {
		return styleStatus.Render(" " + m.status)
	}
	hints := []string{
		styleDim.Render("tab: switch panel"),
		styleDim.Render("↑↓: scroll"),
		styleDim.Render("enter: send"),
		styleDim.Render("ctrl+c: quit"),
	}
	return "  " + strings.Join(hints, "  │  ")
}

// ── Helpers ───────────────────────────────────────────────────────

// wordWrap splits text into lines of at most width runes.
func wordWrap(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}

	var lines []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) <= width {
			line += " " + w
		} else {
			lines = append(lines, line)
			line = w
		}
	}
	return append(lines, line)
}
