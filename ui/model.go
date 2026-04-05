// Package ui implements the CAS terminal interface using Bubble Tea.
//
// Layout: split panel — chat (40%) left, tabbed workspace (60%) right.
//
// Focus states:
//   FocusChat      — typing in chat input, navigating history
//   FocusWorkspace — viewing workspace, switching tabs, entering edit mode
//   FocusEdit      — inline editing via bubbles/textarea, Esc saves + exits
//
// Streaming: buffered channel + recursive tea.Cmd (one event per tick).
package ui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
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
	colEdit      = lipgloss.AdaptiveColor{Light: "#D18E00", Dark: "#FFA657"} // amber in edit mode
	colDim       = lipgloss.AdaptiveColor{Light: "#9B9B9B", Dark: "#5C5C5C"}

	stylePanel       = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colBorder).Padding(0, 1)
	styleActivePanel = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colActive).Padding(0, 1)
	styleWSPanel     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colWorkspace).Padding(0, 1)
	styleEditPanel   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colEdit).Padding(0, 1)

	styleTitle       = lipgloss.NewStyle().Foreground(colActive).Bold(true)
	styleWSType      = lipgloss.NewStyle().Foreground(colWorkspace).Italic(true)
	styleEditBadge   = lipgloss.NewStyle().Foreground(colEdit).Bold(true)
	styleDim         = lipgloss.NewStyle().Foreground(colDim)
	styleUser        = lipgloss.NewStyle().Foreground(lipgloss.Color("#79c0ff"))
	styleShell       = lipgloss.NewStyle().Foreground(lipgloss.Color("#7ee787"))
	styleInput       = lipgloss.NewStyle().Foreground(lipgloss.Color("#e6edf3"))
	styleStatus      = lipgloss.NewStyle().Foreground(colDim).Italic(true)
	styleCode        = lipgloss.NewStyle().Foreground(lipgloss.Color("#e6edf3"))

	styleTabActive   = lipgloss.NewStyle().Foreground(colWorkspace).Bold(true).Padding(0, 1)
	styleTabInactive = lipgloss.NewStyle().Foreground(colDim).Padding(0, 1)
	styleTabEditing  = lipgloss.NewStyle().Foreground(colEdit).Bold(true).Padding(0, 1)
)

// ── Tab state ─────────────────────────────────────────────────────

type tabState struct {
	ws      *workspace.Workspace // nil while generating (placeholder)
	title   string
	wsType  string
	content string // current content (may differ from ws.Content while editing)
	scroll  int
}

func tabFromWorkspace(ws *workspace.Workspace) tabState {
	return tabState{ws: ws, title: ws.Title, wsType: ws.Type, content: ws.Content}
}

// ── Stream event ──────────────────────────────────────────────────

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
	FocusEdit      Focus = iota // inline editing — textarea is active
)

// ── Model ─────────────────────────────────────────────────────────

type Model struct {
	sh        *shell.Shell
	sessionID string

	// Chat
	messages   []shell.Message
	input      string
	chatScroll int

	// Workspace tabs
	tabs      []tabState
	activeTab int

	// Edit mode
	editor    textarea.Model
	editDirty bool // content changed since last save

	// Streaming
	streaming bool
	streamBuf strings.Builder
	streamCh  chan streamEvent

	// Layout
	width  int
	height int
	focus  Focus

	// Status
	status string
}

// New creates a model seeded with existing session state.
func New(sh *shell.Shell, sessionID string, history []shell.Message, workspaces []*workspace.Workspace) Model {
	m := Model{
		sh:        sh,
		sessionID: sessionID,
		messages:  history,
		focus:     FocusChat,
	}
	for _, ws := range workspaces {
		m.tabs = append(m.tabs, tabFromWorkspace(ws))
	}
	if len(m.tabs) > 0 {
		m.activeTab = len(m.tabs) - 1
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
		// Resize textarea if edit mode is active
		if m.focus == FocusEdit {
			m.editor.SetWidth(m.editorWidth())
			m.editor.SetHeight(m.editorHeight())
		}
		return m, nil

	case tea.KeyMsg:
		// Edit mode intercepts all keys except its own exit bindings
		if m.focus == FocusEdit {
			return m.handleEditKey(msg)
		}
		return m.handleKey(msg)

	case tokenMsg:
		m.streamBuf.WriteString(string(msg))
		if m.activeTab < len(m.tabs) {
			m.tabs[m.activeTab].content = m.streamBuf.String()
		}
		return m, listenStream(m.streamCh)

	case responseMsg:
		return m.handleResponse(msg)
	}

	// Delegate to textarea in edit mode for non-key messages (paste, etc.)
	if m.focus == FocusEdit {
		var cmd tea.Cmd
		m.editor, cmd = m.editor.Update(msg)
		return m, cmd
	}

	return m, nil
}

// handleKey handles keys in chat and workspace view modes.
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

	case tea.KeySpace:
		if m.focus == FocusChat && !m.streaming {
			m.input += " "
		}
		return m, nil

	case tea.KeyBackspace:
		if m.focus == FocusChat && len(m.input) > 0 {
			runes := []rune(m.input)
			m.input = string(runes[:len(runes)-1])
		}
		return m, nil

	case tea.KeyUp:
		switch m.focus {
		case FocusWorkspace:
			if m.activeTab < len(m.tabs) && m.tabs[m.activeTab].scroll > 0 {
				m.tabs[m.activeTab].scroll--
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
			if m.activeTab < len(m.tabs) {
				m.tabs[m.activeTab].scroll++
			}
		case FocusChat:
			if m.chatScroll > 0 {
				m.chatScroll--
			}
		}
		return m, nil

	case tea.KeyPgUp:
		if m.focus == FocusWorkspace && m.activeTab < len(m.tabs) {
			m.tabs[m.activeTab].scroll -= 10
			if m.tabs[m.activeTab].scroll < 0 {
				m.tabs[m.activeTab].scroll = 0
			}
		}
		return m, nil

	case tea.KeyPgDown:
		if m.focus == FocusWorkspace && m.activeTab < len(m.tabs) {
			m.tabs[m.activeTab].scroll += 10
		}
		return m, nil

	case tea.KeyRunes:
		if m.focus == FocusWorkspace {
			switch string(msg.Runes) {
			case "[":
				if m.activeTab > 0 {
					m.activeTab--
				}
				return m, nil
			case "]":
				if m.activeTab < len(m.tabs)-1 {
					m.activeTab++
				}
				return m, nil
			case "e", "E":
				// Enter edit mode for confirmed workspaces only
				if !m.streaming && m.activeTab < len(m.tabs) && m.tabs[m.activeTab].ws != nil {
					return m.enterEditMode()
				}
				return m, nil
			}
		}
		if m.focus == FocusChat && !m.streaming {
			m.input += string(msg.Runes)
		}
		return m, nil
	}

	return m, nil
}

// handleEditKey handles keys while the textarea is active.
func (m Model) handleEditKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		// Save and exit edit mode
		return m.exitEditMode(true)

	case tea.KeyCtrlS:
		// Save without leaving edit mode
		return m.saveEdit()

	case tea.KeyCtrlC:
		// Discard and exit
		return m.exitEditMode(false)

	default:
		// All other keys go to the textarea
		var cmd tea.Cmd
		m.editor, cmd = m.editor.Update(msg)
		m.editDirty = true
		return m, cmd
	}
}

// enterEditMode initialises the textarea with the current tab's content.
func (m Model) enterEditMode() (Model, tea.Cmd) {
	tab := m.tabs[m.activeTab]

	ta := textarea.New()
	ta.SetValue(tab.content)
	ta.SetWidth(m.editorWidth())
	ta.SetHeight(m.editorHeight())
	ta.ShowLineNumbers = false
	ta.CharLimit = 0 // unlimited

	// Style the textarea to blend with the pane
	ta.FocusedStyle.Base = lipgloss.NewStyle().Foreground(lipgloss.Color("#e6edf3"))
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle().Background(lipgloss.Color("#21262d"))
	ta.BlurredStyle.Base = ta.FocusedStyle.Base

	// Place cursor at end
	ta.CursorEnd()

	m.editor = ta
	m.editDirty = false
	m.focus = FocusEdit
	m.status = ""

	return m, textarea.Blink
}

// exitEditMode saves (if save=true) and returns to workspace view.
func (m Model) exitEditMode(save bool) (Model, tea.Cmd) {
	if save && m.editDirty && m.activeTab < len(m.tabs) && m.tabs[m.activeTab].ws != nil {
		newContent := m.editor.Value()
		ws := m.tabs[m.activeTab].ws
		updated, err := m.sh.Workspaces().Update(ws.ID, ws.Title, newContent)
		if err != nil {
			m.status = "save failed: " + err.Error()
		} else {
			m.tabs[m.activeTab].ws = updated
			m.tabs[m.activeTab].content = newContent
			m.status = "saved"
		}
	}
	m.focus = FocusWorkspace
	m.editDirty = false
	return m, nil
}

// saveEdit persists without leaving edit mode.
func (m Model) saveEdit() (Model, tea.Cmd) {
	if m.activeTab < len(m.tabs) && m.tabs[m.activeTab].ws != nil {
		newContent := m.editor.Value()
		ws := m.tabs[m.activeTab].ws
		updated, err := m.sh.Workspaces().Update(ws.ID, ws.Title, newContent)
		if err != nil {
			m.status = "save failed: " + err.Error()
		} else {
			m.tabs[m.activeTab].ws = updated
			m.tabs[m.activeTab].content = newContent
			m.editDirty = false
			m.status = "saved"
		}
	}
	return m, nil
}

// editorWidth returns the usable width for the textarea inside the pane.
func (m Model) editorWidth() int {
	chatW := m.width * 40 / 100
	wsW := m.width - chatW - 2
	if wsW < 28 {
		wsW = 28
	}
	return wsW - 6 // subtract border (2) + padding (2) + margin (2)
}

// editorHeight returns the usable height for the textarea inside the pane.
func (m Model) editorHeight() int {
	h := m.height - 4 - 4 // outer layout overhead + tab bar + sep + status
	if h < 3 {
		h = 3
	}
	return h
}

// submitMessage detects intent to prepare a placeholder tab for creates.
func (m Model) submitMessage() (Model, tea.Cmd) {
	message := strings.TrimSpace(m.input)
	in := intent.Detect(message)

	m.input = ""
	m.messages = append(m.messages, shell.Message{Role: "user", Text: message})
	m.streaming = true
	m.streamBuf.Reset()
	m.status = "thinking…"

	if in.Kind == intent.KindCreate {
		title := in.TitleHint
		if title == "" {
			title = "New Workspace"
		}
		m.tabs = append(m.tabs, tabState{title: title, wsType: string(in.WSType)})
		m.activeTab = len(m.tabs) - 1
	}

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

func listenStream(ch chan streamEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return responseMsg{err: fmt.Errorf("stream closed unexpectedly")}
		}
		if ev.Resp != nil || ev.Err != nil {
			return responseMsg{resp: ev.Resp, err: ev.Err}
		}
		return tokenMsg(ev.Token)
	}
}

func (m Model) handleResponse(msg responseMsg) (Model, tea.Cmd) {
	m.streaming = false
	m.streamCh = nil
	m.status = ""

	if msg.err != nil {
		m.status = "error: " + msg.err.Error()
		if m.activeTab < len(m.tabs) && m.tabs[m.activeTab].ws == nil {
			m.tabs = append(m.tabs[:m.activeTab], m.tabs[m.activeTab+1:]...)
			m.activeTab = clamp(m.activeTab-1, 0, len(m.tabs)-1)
		}
		return m, nil
	}

	resp := msg.resp
	m.messages = append(m.messages, shell.Message{Role: "shell", Text: resp.ChatReply})

	if resp.Workspace != nil {
		ws := resp.Workspace
		if resp.Intent == intent.KindClose {
			for i, tab := range m.tabs {
				if tab.ws != nil && tab.ws.ID == ws.ID {
					m.tabs = append(m.tabs[:i], m.tabs[i+1:]...)
					m.activeTab = clamp(m.activeTab, 0, len(m.tabs)-1)
					break
				}
			}
		} else {
			found := -1
			for i, tab := range m.tabs {
				if tab.ws != nil && tab.ws.ID == ws.ID {
					found = i
					break
				}
			}
			if found >= 0 {
				m.tabs[found].ws = ws
				m.tabs[found].title = ws.Title
				m.tabs[found].content = ws.Content
				m.activeTab = found
			} else if m.activeTab < len(m.tabs) && m.tabs[m.activeTab].ws == nil {
				m.tabs[m.activeTab].ws = ws
				m.tabs[m.activeTab].title = ws.Title
				m.tabs[m.activeTab].wsType = ws.Type
				m.tabs[m.activeTab].content = ws.Content
			} else {
				m.tabs = append(m.tabs, tabFromWorkspace(ws))
				m.activeTab = len(m.tabs) - 1
			}
		}
	}

	m.streamBuf.Reset()
	return m, nil
}

// ── View ──────────────────────────────────────────────────────────

func (m Model) View() string {
	if m.width == 0 {
		return "Loading…"
	}

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

// ── Chat pane ─────────────────────────────────────────────────────

func (m Model) renderChat(w, h int) string {
	st := stylePanel
	if m.focus == FocusChat {
		st = styleActivePanel
	}
	st = st.Width(w - 2)

	var lines []string
	for _, msg := range m.messages {
		wrapped := wordWrap(msg.Text, w-8)
		if msg.Role == "user" {
			for i, l := range wrapped {
				prefix := "      "
				if i == 0 {
					prefix = styleUser.Render("you › ")
				}
				lines = append(lines, prefix+l)
			}
		} else {
			for i, l := range wrapped {
				prefix := "      "
				if i == 0 {
					prefix = styleShell.Render("cas › ")
				}
				lines = append(lines, prefix+l)
			}
		}
		lines = append(lines, "")
	}

	histH := h - 5
	if histH < 0 {
		histH = 0
	}
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
	for len(visible) < histH {
		visible = append([]string{""}, visible...)
	}

	cursor := "█"
	if m.streaming {
		cursor = styleDim.Render("…")
	}
	sep := styleDim.Render(strings.Repeat("─", w-4))
	inputLine := styleInput.Render("> " + m.input + cursor)
	return st.Render(strings.Join(visible, "\n") + "\n" + sep + "\n" + inputLine)
}

// ── Workspace pane ────────────────────────────────────────────────

func (m Model) renderWorkspace(w, h int) string {
	// Border colour signals mode
	var st lipgloss.Style
	switch m.focus {
	case FocusEdit:
		st = styleEditPanel
	case FocusWorkspace:
		st = styleWSPanel
	default:
		st = stylePanel
	}
	st = st.Width(w - 2)

	if len(m.tabs) == 0 {
		return st.Render(styleDim.Render(
			"No workspace open.\n\n" +
				"  write a project proposal\n" +
				"  create a python script\n" +
				"  make a todo list",
		))
	}

	tabBar := m.renderTabBar(w - 4)
	sep := styleDim.Render(strings.Repeat("─", w-4))
	contentH := h - 4
	if contentH < 1 {
		contentH = 1
	}

	var body string
	if m.focus == FocusEdit {
		body = m.editor.View()
	} else {
		body = m.renderTabContent(m.tabs[m.activeTab], w-4, contentH)
	}

	return st.Render(tabBar + "\n" + sep + "\n" + body)
}

func (m Model) renderTabBar(w int) string {
	var parts []string
	for i, tab := range m.tabs {
		badge := "?"
		if len(tab.wsType) > 0 {
			badge = string(tab.wsType[0])
		}
		title := truncate(tab.title, 18)
		if tab.ws == nil {
			title += "…"
		}
		label := fmt.Sprintf("[%s] %s", badge, title)

		switch {
		case i == m.activeTab && m.focus == FocusEdit:
			parts = append(parts, styleTabEditing.Render(label))
		case i == m.activeTab:
			parts = append(parts, styleTabActive.Render(label))
		default:
			parts = append(parts, styleTabInactive.Render(label))
		}
	}

	bar := strings.Join(parts, " ")
	runes := []rune(bar)
	if len(runes) > w {
		bar = string(runes[:w])
	}
	return bar
}

func (m Model) renderTabContent(tab tabState, w, h int) string {
	if tab.content == "" {
		if m.streaming {
			return styleDim.Render("generating…")
		}
		return styleDim.Render("(empty)")
	}

	var rendered string
	if tab.wsType == "code" {
		rendered = styleCode.Render(tab.content)
	} else {
		renderer, err := glamour.NewTermRenderer(
			glamour.WithAutoStyle(),
			glamour.WithWordWrap(w),
		)
		if err == nil {
			if out, err := renderer.Render(tab.content); err == nil {
				rendered = strings.TrimRight(out, "\n")
			} else {
				rendered = tab.content
			}
		} else {
			rendered = tab.content
		}
	}

	lines := strings.Split(rendered, "\n")
	maxScroll := len(lines) - h
	if maxScroll < 0 {
		maxScroll = 0
	}
	scroll := clamp(tab.scroll, 0, maxScroll)
	end := scroll + h
	if end > len(lines) {
		end = len(lines)
	}
	visible := lines[scroll:end]

	if len(lines) > h && maxScroll > 0 {
		pct := scroll * 100 / maxScroll
		if len(visible) > 0 {
			visible[len(visible)-1] = styleDim.Render(fmt.Sprintf(" ↕ %d%%", pct))
		}
	}

	return strings.Join(visible, "\n")
}

// ── Status bar ────────────────────────────────────────────────────

func (m Model) renderStatus() string {
	if m.status != "" {
		return styleStatus.Render(" " + m.status)
	}

	switch m.focus {
	case FocusEdit:
		return "  " + strings.Join([]string{
			styleEditBadge.Render("EDITING"),
			styleDim.Render("esc: save & exit"),
			styleDim.Render("ctrl+s: save"),
			styleDim.Render("ctrl+c: discard"),
		}, "  │  ")
	case FocusWorkspace:
		return "  " + strings.Join([]string{
			styleDim.Render("[/]: prev/next tab"),
			styleDim.Render("e: edit"),
			styleDim.Render("↑↓/pgup/pgdn: scroll"),
			styleDim.Render("tab: chat"),
			styleDim.Render("ctrl+c: quit"),
		}, "  │  ")
	default:
		return "  " + strings.Join([]string{
			styleDim.Render("↑↓: scroll history"),
			styleDim.Render("enter: send"),
			styleDim.Render("tab: workspace"),
			styleDim.Render("ctrl+c: quit"),
		}, "  │  ")
	}
}

// ── Helpers ───────────────────────────────────────────────────────

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

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
