package shell

import (
	"context"
	"fmt"
	"strings"

	"github.com/goweft/cas/internal/agent"
	"github.com/goweft/cas/internal/intent"
	"github.com/goweft/cas/internal/plugin"
	"github.com/goweft/cas/internal/runner"
)

func (sh *Shell) handleCreate(ctx context.Context, sess *Session, in intent.Intent, message string) (*Response, error) {
	title := titleOrDefault(in.TitleHint)
	result, err := sh.genAgent.Generate(ctx, agent.GenerationRequest{
		WSType:      string(in.WSType),
		Title:       title,
		Prompt:      message,
		UserContext: sh.conductor.UserContext(),
		Temperature: 0.6,
	})
	if err != nil {
		return nil, err
	}
	content := normaliseContent(result.Content, string(in.WSType), title)
	ws, err := sh.workspaces.Create(newID(), string(in.WSType), title, content, sess.ID)
	if err != nil {
		return nil, err
	}
	reply := fmt.Sprintf("Created %s workspace %q. Edit directly or ask me to make changes.", in.WSType, ws.Title)
	return &Response{ChatReply: reply, Workspace: ws, Intent: in.Kind}, nil
}

func (sh *Shell) streamCreate(ctx context.Context, sess *Session, in intent.Intent, message string, onToken func(string)) (*StreamResponse, error) {
	title := titleOrDefault(in.TitleHint)
	result, err := sh.genAgent.Stream(ctx, agent.GenerationRequest{
		WSType:      string(in.WSType),
		Title:       title,
		Prompt:      message,
		UserContext: sh.conductor.UserContext(),
		Temperature: 0.6,
	}, onToken)
	if err != nil {
		return nil, err
	}
	content := normaliseContent(result.Content, string(in.WSType), title)
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
	ws := resolveTarget(message, active)
	refs := crossWorkspaceRefs(message, active, ws)
	refData := make([]struct{ Title, Content string }, len(refs))
	for i, r := range refs {
		refData[i] = struct{ Title, Content string }{r.Title, r.Content}
	}
	result, err := sh.editAgent.Edit(ctx, agent.EditRequest{
		WSType:         ws.Type,
		Title:          ws.Title,
		CurrentContent: ws.Content,
		EditRequest:    message,
		UserContext:    sh.conductor.UserContext(),
		Refs:           refData,
		Temperature:    0.3,
	})
	if err != nil {
		return nil, err
	}
	ws, err = sh.workspaces.Update(ws.ID, ws.Title, result.Content)
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
	ws := resolveTarget(message, active)
	refs := crossWorkspaceRefs(message, active, ws)
	refData := make([]struct{ Title, Content string }, len(refs))
	for i, r := range refs {
		refData[i] = struct{ Title, Content string }{r.Title, r.Content}
	}
	result, err := sh.editAgent.Stream(ctx, agent.EditRequest{
		WSType:         ws.Type,
		Title:          ws.Title,
		CurrentContent: ws.Content,
		EditRequest:    message,
		UserContext:    sh.conductor.UserContext(),
		Refs:           refData,
		Temperature:    0.3,
	}, onToken)
	if err != nil {
		return nil, err
	}
	ws, err = sh.workspaces.Update(ws.ID, ws.Title, result.Content)
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
	result, err := sh.chatAgent.Chat(ctx, agent.ChatRequest{
		Message:     message,
		History:     chatHistory(sess),
		UserContext: sh.conductor.UserContext(),
		Temperature: 0.7,
	})
	if err != nil {
		return nil, err
	}
	return &Response{ChatReply: result.Reply, Intent: intent.KindChat}, nil
}

func (sh *Shell) streamChat(ctx context.Context, sess *Session, message string, onToken func(string)) (*StreamResponse, error) {
	result, err := sh.chatAgent.Stream(ctx, agent.ChatRequest{
		Message:     message,
		History:     chatHistory(sess),
		UserContext: sh.conductor.UserContext(),
		Temperature: 0.7,
	}, onToken)
	if err != nil {
		return nil, err
	}
	return &StreamResponse{ChatReply: result.Reply, Intent: intent.KindChat}, nil
}

func (sh *Shell) handleRun(ctx context.Context, sess *Session) (*Response, error) {
	active := sh.workspaces.Active()
	if len(active) == 0 {
		return &Response{ChatReply: "No active workspace to run. Create a code workspace first.", Intent: intent.KindRun}, nil
	}
	ws := active[len(active)-1]
	if ws.Type != "code" {
		return &Response{
			ChatReply: fmt.Sprintf("Cannot run a %s workspace. Only code workspaces can be executed.", ws.Type),
			Workspace: ws,
			Intent:    intent.KindRun,
		}, nil
	}
	if strings.TrimSpace(ws.Content) == "" {
		return &Response{
			ChatReply: fmt.Sprintf("Workspace %q is empty — nothing to run.", ws.Title),
			Workspace: ws,
			Intent:    intent.KindRun,
		}, nil
	}

	result, err := runner.Run(ctx, ws.Content, runner.DefaultTimeout)
	if err != nil {
		return &Response{
			ChatReply: fmt.Sprintf("Run of %q failed: %v", ws.Title, err),
			Workspace: ws,
			Intent:    intent.KindRun,
		}, nil
	}

	return &Response{
		ChatReply: fmt.Sprintf("Workspace %q: %s", ws.Title, runner.FormatResult(result)),
		Workspace: ws,
		Intent:    intent.KindRun,
	}, nil
}

func (sh *Shell) handlePlugin(sess *Session, cmd *plugin.Command) (*Response, error) {
	// Build plugin context from active workspaces
	active := sh.workspaces.Active()
	wsInfos := make([]plugin.WorkspaceInfo, len(active))
	for i, ws := range active {
		wsInfos[i] = plugin.WorkspaceInfo{
			ID:      ws.ID,
			Type:    ws.Type,
			Title:   ws.Title,
			Content: ws.Content,
		}
	}

	ctx := &plugin.Context{Workspaces: wsInfos}
	reply, err := sh.plugins.Execute(cmd, ctx)
	if err != nil {
		reply = fmt.Sprintf("Plugin error: %v", err)
	}

	shellMsg := sess.addMessage("shell", reply)
	if err := sh.store.SaveMessage(toStoreMsg(shellMsg)); err != nil {
		return nil, err
	}

	sh.conductor.Observe(string(intent.KindPlugin), cmd.Name, "", "")
	return &Response{ChatReply: reply, Intent: intent.KindPlugin}, nil
}

func (sh *Shell) handleCombine(ctx context.Context, sess *Session, message string) (*Response, error) {
	active := sh.workspaces.Active()
	if len(active) < 2 {
		return &Response{
			ChatReply: "Need at least 2 active workspaces to combine.",
			Intent:    intent.KindCombine,
		}, nil
	}

	sources := resolveAll(message, active)
	if len(sources) < 2 {
		return &Response{
			ChatReply: "Could not identify 2 or more workspaces to combine. Try naming them explicitly.",
			Intent:    intent.KindCombine,
		}, nil
	}

	wsData := make([]struct{ Title, Type, Content string }, len(sources))
	for i, s := range sources {
		wsData[i] = struct{ Title, Type, Content string }{s.Title, s.Type, s.Content}
	}

	result, err := sh.combAgent.Combine(ctx, agent.CombineRequest{
		Sources:     wsData,
		Instruction: message,
		UserContext: sh.conductor.UserContext(),
		Temperature: 0.5,
	})
	if err != nil {
		return nil, err
	}

	titles := make([]string, len(sources))
	for i, s := range sources {
		titles[i] = s.Title
	}
	title := "Combined: " + strings.Join(titles, " + ")
	if len(title) > 64 {
		title = title[:61] + "..."
	}

	ws, err := sh.workspaces.Create(newID(), "document", title, result.Content, sess.ID)
	if err != nil {
		return nil, err
	}

	reply := fmt.Sprintf("Combined %d workspaces into %q.", len(sources), ws.Title)
	return &Response{ChatReply: reply, Workspace: ws, Intent: intent.KindCombine}, nil
}

func (sh *Shell) streamCombine(ctx context.Context, sess *Session, message string, onToken func(string)) (*StreamResponse, error) {
	active := sh.workspaces.Active()
	if len(active) < 2 {
		return &StreamResponse{
			ChatReply: "Need at least 2 active workspaces to combine.",
			Intent:    intent.KindCombine,
		}, nil
	}

	sources := resolveAll(message, active)
	if len(sources) < 2 {
		return &StreamResponse{
			ChatReply: "Could not identify 2 or more workspaces to combine. Try naming them explicitly.",
			Intent:    intent.KindCombine,
		}, nil
	}

	wsData := make([]struct{ Title, Type, Content string }, len(sources))
	for i, s := range sources {
		wsData[i] = struct{ Title, Type, Content string }{s.Title, s.Type, s.Content}
	}

	result, err := sh.combAgent.Stream(ctx, agent.CombineRequest{
		Sources:     wsData,
		Instruction: message,
		UserContext: sh.conductor.UserContext(),
		Temperature: 0.5,
	}, onToken)
	if err != nil {
		return nil, err
	}

	titles := make([]string, len(sources))
	for i, s := range sources {
		titles[i] = s.Title
	}
	title := "Combined: " + strings.Join(titles, " + ")
	if len(title) > 64 {
		title = title[:61] + "..."
	}

	ws, err := sh.workspaces.Create(newID(), "document", title, result.Content, sess.ID)
	if err != nil {
		return nil, err
	}

	reply := fmt.Sprintf("Combined %d workspaces into %q.", len(sources), ws.Title)
	return &StreamResponse{ChatReply: reply, Workspace: ws, Intent: intent.KindCombine}, nil
}
