// Package shell is the CAS session manager.
// It wires intent detection, workspace lifecycle, LLM calls, and persistence.
package shell

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/goweft/cas/internal/intent"
	"github.com/goweft/cas/internal/llm"
	"github.com/goweft/cas/internal/store"
	"github.com/goweft/cas/internal/workspace"
)

// Message is a single conversation turn.
type Message struct {
	ID        string
	SessionID string
	Role      string // "user" | "shell"
	Text      string
	Timestamp time.Time
}

// Session is a single CAS conversation session.
type Session struct {
	ID        string
	CreatedAt time.Time
	History   []Message
}

func (s *Session) addMessage(role, text string) Message {
	msg := Message{
		ID:        newID(),
		SessionID: s.ID,
		Role:      role,
		Text:      text,
		Timestamp: time.Now().UTC(),
	}
	s.History = append(s.History, msg)
	return msg
}

// Response is returned from Shell.ProcessMessage.
type Response struct {
	ChatReply string
	Workspace *workspace.Workspace // non-nil when a workspace was created or updated
	Intent    intent.Kind
}

// StreamResponse is returned from Shell.StreamMessage.
type StreamResponse struct {
	ChatReply string
	Workspace *workspace.Workspace
	Intent    intent.Kind
}

// Shell is the central CAS coordinator. Wire it with NewShell.
type Shell struct {
	store      store.Store
	workspaces *workspace.Manager
	sessions   map[string]*Session
}

// NewShell creates a Shell backed by the given store.
// Call Restore() after creation to reload persisted state.
func NewShell(s store.Store) *Shell {
	return &Shell{
		store:      s,
		workspaces: workspace.NewManager(s),
		sessions:   make(map[string]*Session),
	}
}

// Restore loads persisted sessions and workspaces from the store.
func (sh *Shell) Restore() error {
	if err := sh.workspaces.Restore(); err != nil {
		return fmt.Errorf("restore workspaces: %w", err)
	}
	rows, err := sh.store.LoadSessions()
	if err != nil {
		return fmt.Errorf("restore sessions: %w", err)
	}
	for _, row := range rows {
		sess := &Session{ID: row.ID, CreatedAt: row.CreatedAt}
		msgs, err := sh.store.LoadMessages(row.ID)
		if err != nil {
			continue
		}
		for _, m := range msgs {
			sess.History = append(sess.History, Message{
				ID: m.ID, SessionID: m.SessionID,
				Role: m.Role, Text: m.Text, Timestamp: m.Timestamp,
			})
		}
		sh.sessions[sess.ID] = sess
	}
	return nil
}

// CreateSession starts a new conversation session.
func (sh *Shell) CreateSession() (*Session, error) {
	sess := &Session{
		ID:        newID(),
		CreatedAt: time.Now().UTC(),
	}
	sh.sessions[sess.ID] = sess
	return sess, sh.store.SaveSession(sess.ID, sess.CreatedAt)
}

// GetSession returns the session with the given ID.
func (sh *Shell) GetSession(id string) (*Session, error) {
	sess, ok := sh.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %q not found", id)
	}
	return sess, nil
}

// LatestSession returns the most recently created session, or nil.
func (sh *Shell) LatestSession() *Session {
	var latest *Session
	for _, s := range sh.sessions {
		if latest == nil || s.CreatedAt.After(latest.CreatedAt) {
			latest = s
		}
	}
	return latest
}

// Workspaces returns the workspace manager.
func (sh *Shell) Workspaces() *workspace.Manager { return sh.workspaces }

// ProcessMessage classifies the message, calls the LLM synchronously,
// and returns a Response. For streaming, use StreamMessage.
func (sh *Shell) ProcessMessage(ctx context.Context, sessionID, message string) (*Response, error) {
	sess, err := sh.GetSession(sessionID)
	if err != nil {
		return nil, err
	}

	in := intent.Detect(message)
	userMsg := sess.addMessage("user", message)
	if err := sh.store.SaveMessage(toStoreMsg(userMsg)); err != nil {
		return nil, err
	}

	var resp *Response
	switch in.Kind {
	case intent.KindCreate:
		resp, err = sh.handleCreate(ctx, sess, in, message)
	case intent.KindEdit:
		resp, err = sh.handleEdit(ctx, sess, message)
	case intent.KindClose:
		resp, err = sh.handleClose(sess)
	default:
		resp, err = sh.handleChat(ctx, sess, message)
	}
	if err != nil {
		return nil, err
	}

	shellMsg := sess.addMessage("shell", resp.ChatReply)
	if err := sh.store.SaveMessage(toStoreMsg(shellMsg)); err != nil {
		return nil, err
	}
	return resp, nil
}

// StreamMessage classifies the message, then streams tokens via onToken.
// Returns a StreamResponse with the complete reply when generation finishes.
func (sh *Shell) StreamMessage(ctx context.Context, sessionID, message string, onToken func(string)) (*StreamResponse, error) {
	sess, err := sh.GetSession(sessionID)
	if err != nil {
		return nil, err
	}

	in := intent.Detect(message)
	userMsg := sess.addMessage("user", message)
	if err := sh.store.SaveMessage(toStoreMsg(userMsg)); err != nil {
		return nil, err
	}

	var resp *StreamResponse
	switch in.Kind {
	case intent.KindCreate:
		resp, err = sh.streamCreate(ctx, sess, in, message, onToken)
	case intent.KindEdit:
		resp, err = sh.streamEdit(ctx, sess, message, onToken)
	case intent.KindClose:
		r, e := sh.handleClose(sess)
		if e != nil {
			return nil, e
		}
		resp = &StreamResponse{ChatReply: r.ChatReply, Workspace: r.Workspace, Intent: r.Intent}
		err = nil
	default:
		resp, err = sh.streamChat(ctx, sess, message, onToken)
	}
	if err != nil {
		return nil, err
	}

	shellMsg := sess.addMessage("shell", resp.ChatReply)
	if err := sh.store.SaveMessage(toStoreMsg(shellMsg)); err != nil {
		return nil, err
	}
	return resp, nil
}

// ── Handlers ──────────────────────────────────────────────────────

func (sh *Shell) handleCreate(ctx context.Context, sess *Session, in intent.Intent, message string) (*Response, error) {
	title := in.TitleHint
	if title == "" {
		title = "Untitled"
	}
	sys := llm.SystemFor(llm.WorkspaceSystem, string(in.WSType), "")
	msgs := llm.BuildWorkspaceMessages(sys, title, message)
	content, err := llm.Complete(ctx, msgs, llm.ModelFor(string(in.WSType)), 0.6)
	if err != nil {
		return nil, err
	}
	content = normaliseContent(content, string(in.WSType), title)
	ws, err := sh.workspaces.Create(newID(), string(in.WSType), title, content, sess.ID)
	if err != nil {
		return nil, err
	}
	reply := fmt.Sprintf("Created %s workspace %q. Edit directly or ask me to make changes.", in.WSType, ws.Title)
	return &Response{ChatReply: reply, Workspace: ws, Intent: in.Kind}, nil
}

func (sh *Shell) streamCreate(ctx context.Context, sess *Session, in intent.Intent, message string, onToken func(string)) (*StreamResponse, error) {
	title := in.TitleHint
	if title == "" {
		title = "Untitled"
	}
	sys := llm.SystemFor(llm.WorkspaceSystem, string(in.WSType), "")
	msgs := llm.BuildWorkspaceMessages(sys, title, message)
	content, err := llm.Stream(ctx, msgs, llm.ModelFor(string(in.WSType)), 0.6, onToken)
	if err != nil {
		return nil, err
	}
	content = normaliseContent(content, string(in.WSType), title)
	ws, err := sh.workspaces.Create(newID(), string(in.WSType), title, content, sess.ID)
	if err != nil {
		return nil, err
	}
	reply := fmt.Sprintf("Created %s workspace %q. Edit directly or ask me to make changes.", in.WSType, ws.Title)
	return &StreamResponse{ChatReply: reply, Workspace: ws, Intent: in.Kind}, nil
}

func (sh *Shell) handleEdit(ctx context.Context, sess *Session, message string) (*Response, error) {
	active := sh.workspaces.Active()
	if len(active) == 0 {
		return &Response{ChatReply: "No active workspace to edit. Ask me to create one first.", Intent: intent.KindEdit}, nil
	}
	ws := active[len(active)-1]
	sys := llm.SystemFor(llm.EditSystem, ws.Type, "")
	msgs := llm.BuildEditMessages(sys, ws.Title, ws.Content, message)
	content, err := llm.Complete(ctx, msgs, llm.ModelFor(ws.Type), 0.3)
	if err != nil {
		return nil, err
	}
	ws, err = sh.workspaces.Update(ws.ID, ws.Title, content)
	if err != nil {
		return nil, err
	}
	reply := fmt.Sprintf("Updated workspace %q.", ws.Title)
	return &Response{ChatReply: reply, Workspace: ws, Intent: intent.KindEdit}, nil
}

func (sh *Shell) streamEdit(ctx context.Context, sess *Session, message string, onToken func(string)) (*StreamResponse, error) {
	active := sh.workspaces.Active()
	if len(active) == 0 {
		return &StreamResponse{ChatReply: "No active workspace to edit. Ask me to create one first.", Intent: intent.KindEdit}, nil
	}
	ws := active[len(active)-1]
	sys := llm.SystemFor(llm.EditSystem, ws.Type, "")
	msgs := llm.BuildEditMessages(sys, ws.Title, ws.Content, message)
	content, err := llm.Stream(ctx, msgs, llm.ModelFor(ws.Type), 0.3, onToken)
	if err != nil {
		return nil, err
	}
	ws, err = sh.workspaces.Update(ws.ID, ws.Title, content)
	if err != nil {
		return nil, err
	}
	reply := fmt.Sprintf("Updated workspace %q.", ws.Title)
	return &StreamResponse{ChatReply: reply, Workspace: ws, Intent: intent.KindEdit}, nil
}

func (sh *Shell) handleClose(sess *Session) (*Response, error) {
	active := sh.workspaces.Active()
	if len(active) == 0 {
		return &Response{ChatReply: "No active workspace to close.", Intent: intent.KindClose}, nil
	}
	ws := active[len(active)-1]
	ws, err := sh.workspaces.Close(ws.ID)
	if err != nil {
		return nil, err
	}
	return &Response{ChatReply: fmt.Sprintf("Closed workspace %q.", ws.Title), Workspace: ws, Intent: intent.KindClose}, nil
}

func (sh *Shell) handleChat(ctx context.Context, sess *Session, message string) (*Response, error) {
	history := sessionHistory(sess)
	msgs := llm.BuildChatMessages(llm.ChatSystem, history, message)
	reply, err := llm.Complete(ctx, msgs, llm.ModelFor("chat"), 0.7)
	if err != nil {
		return nil, err
	}
	if reply == "" {
		reply = `To create a workspace, say: "write a [document type]".`
	}
	return &Response{ChatReply: reply, Intent: intent.KindChat}, nil
}

func (sh *Shell) streamChat(ctx context.Context, sess *Session, message string, onToken func(string)) (*StreamResponse, error) {
	history := sessionHistory(sess)
	msgs := llm.BuildChatMessages(llm.ChatSystem, history, message)
	reply, err := llm.Stream(ctx, msgs, llm.ModelFor("chat"), 0.7, onToken)
	if err != nil {
		return nil, err
	}
	if reply == "" {
		reply = `To create a workspace, say: "write a [document type]".`
	}
	return &StreamResponse{ChatReply: reply, Intent: intent.KindChat}, nil
}

// ── Helpers ───────────────────────────────────────────────────────

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func toStoreMsg(m Message) store.MessageRow {
	return store.MessageRow{
		ID: m.ID, SessionID: m.SessionID,
		Role: m.Role, Text: m.Text, Timestamp: m.Timestamp,
	}
}

func sessionHistory(sess *Session) []llm.Message {
	out := make([]llm.Message, 0, len(sess.History))
	for _, m := range sess.History {
		role := "assistant"
		if m.Role == "user" {
			role = "user"
		}
		out = append(out, llm.Message{Role: role, Content: m.Text})
	}
	return out
}

func normaliseContent(content, wsType, title string) string {
	content = strings.TrimSpace(content)
	if wsType == "code" {
		return content
	}
	if !strings.HasPrefix(content, "#") {
		return "# " + title + "\n\n" + content
	}
	return content
}
