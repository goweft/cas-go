package shell

import (
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/goweft/cas/internal/llm"
	"github.com/goweft/cas/internal/store"
	"github.com/goweft/cas/internal/workspace"
)

// crossWorkspaceRefs finds workspaces referenced in the message other than the target.
// Used to include additional context in edit prompts.
func crossWorkspaceRefs(message string, active []*workspace.Workspace, target *workspace.Workspace) []*workspace.Workspace {
	if len(active) < 2 {
		return nil
	}
	msg := strings.ToLower(message)
	var refs []*workspace.Workspace
	for _, ws := range active {
		if ws.ID == target.ID {
			continue
		}
		if titleMatchScore(msg, ws.Title) > 0 {
			refs = append(refs, ws)
		}
	}
	return refs
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

// chatHistoryWindow bounds the turns sent as chat context. It matches the
// truncation inside llm.BuildChatMessages, and keeps the ChatAgent's
// history_not_excessive precondition (max 20) satisfiable no matter how
// long a session grows.
const chatHistoryWindow = 6

// chatHistory returns the conversation context for the chat agents.
//
// The router appends the current user message to sess.History before
// dispatching (so the message is persisted even if the turn fails), which
// means the final entry is the in-flight message. It is excluded here
// because the agents receive the current message separately — including
// it would send it to the model twice. The rest is truncated to the most
// recent chatHistoryWindow entries.
func chatHistory(sess *Session) []llm.Message {
	h := sess.History
	if len(h) == 0 {
		return nil
	}
	h = h[:len(h)-1] // drop the in-flight message (passed separately)
	if len(h) > chatHistoryWindow {
		h = h[len(h)-chatHistoryWindow:]
	}
	out := make([]llm.Message, 0, len(h))
	for _, m := range h {
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

func titleOrDefault(hint string) string {
	if hint == "" {
		return "Untitled"
	}
	return hint
}
