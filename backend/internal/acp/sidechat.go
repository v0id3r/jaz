package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	acpschema "github.com/gluonfield/acp-transport/acp"
	acpv2 "github.com/gluonfield/acp-transport/acp/v2"
	"github.com/gluonfield/acp-transport/jsonrpc"
	"github.com/wins/jaz/backend/internal/sessionevents"
	"github.com/wins/jaz/backend/internal/storage"
)

// Side chats are forks of the agent's session: Jaz forks the session with the
// standard session/fork request, prompts the fork, and closes it once the
// fork's turn answered. The fork's session id scopes every event the side
// chat produces; no adapter-side extension carries the scope.
const (
	sideChatCommand = "side"
	agentMethodFork = "session/fork"
)

type SideChatRequest struct {
	Session     string
	ID          string
	Message     string
	Contexts    []storage.MessageContext
	Attachments []storage.Attachment
}

type sideChatScope struct {
	ID              string
	Command         string
	ParentSessionID string
	ThreadID        string
}

// sideChatFork is a running side chat's fork of the agent's session.
type sideChatFork struct {
	jobID    string
	protocol int
	scope    sideChatScope
}

func (m *Manager) sideChatForkByACPSession(acpSessionID string) (sideChatFork, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	fork, ok := m.sideChats[acpSessionID]
	return fork, ok
}

func (m *Manager) rememberSideChatFork(acpSessionID string, fork sideChatFork) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sideChats[acpSessionID] = fork
}

func (m *Manager) forgetSideChatFork(acpSessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sideChats, acpSessionID)
}

// SendSideChat forks the session, prompts the fork, and closes it. The fork's
// events arrive under the fork's session id and are routed by
// sideChatForkByACPSession, so the parent turn's bookkeeping never sees them.
func (m *Manager) SendSideChat(ctx context.Context, req SideChatRequest) error {
	sideChatID := strings.TrimSpace(req.ID)
	message := strings.TrimSpace(req.Message)
	if sideChatID == "" {
		return fmt.Errorf("side chat id is required")
	}
	if !storage.HasMessageContent(req.Message, req.Contexts, req.Attachments) {
		return fmt.Errorf("message is required")
	}
	job, err := m.job(req.Session)
	if err != nil {
		if job, err = m.resume(ctx, req.Session); err != nil {
			return err
		}
	}
	job, err = m.acquireSessionProcess(ctx, job)
	if err != nil {
		return err
	}
	defer job.release()
	peer := m.peer(job.ID)
	if peer == nil {
		return fmt.Errorf("acp peer is not active")
	}
	scope := sideChatScope{ID: sideChatID, Command: sideChatCommand, ParentSessionID: job.ID}
	forkRaw, err := peer.Call(ctx, agentMethodFork, map[string]any{
		"sessionId": job.ACPSession,
		"cwd":       job.Cwd,
	})
	if err != nil {
		m.publishSideChatMessage(job.eventView(), scope, "error", err.Error(), "error")
		return fmt.Errorf("side chat fork: %w", err)
	}
	var forked struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(forkRaw, &forked) != nil || forked.SessionID == "" {
		m.publishSideChatMessage(job.eventView(), scope, "error", "the agent's session/fork answer carried no session id", "error")
		return fmt.Errorf("side chat fork: no session id in %s", forkRaw)
	}
	m.rememberSideChatFork(forked.SessionID, sideChatFork{jobID: job.ID, protocol: job.protocol, scope: scope})
	defer m.closeSideChatFork(context.Background(), peer, job.protocol, forked.SessionID)
	resolver := localAttachmentResources
	if len(req.Attachments) > 0 {
		if resolver, err = m.attachmentResourceResolver(job); err != nil {
			m.publishSideChatMessage(job.eventView(), scope, "error", err.Error(), "error")
			return err
		}
	}
	promptMessage, contexts := promptMessageAndContexts(req.Message, req.Contexts)
	prompt, err := promptContentBlocks(promptMessage, req.Attachments, resolver)
	if err != nil {
		m.publishSideChatMessage(job.eventView(), scope, "error", err.Error(), "error")
		return err
	}
	m.publishSideChatUserMessage(job.eventView(), scope, message, contexts, req.Attachments)

	method := acpschema.AgentMethodSessionPrompt
	if job.protocol == protocolV2 {
		method = acpv2.AgentMethodSessionPrompt
	}
	raw, err := peer.Call(ctx, method, acpschema.PromptRequest{
		SessionID: acpschema.SessionID(forked.SessionID),
		Prompt:    prompt,
	})
	if err != nil {
		m.publishSideChatMessage(job.eventView(), scope, "error", err.Error(), "error")
		return err
	}
	m.recordRawUsage(job, raw)
	return nil
}

// closeSideChatFork ends the forked session; the fork served its one turn.
func (m *Manager) closeSideChatFork(ctx context.Context, peer *jsonrpc.Peer, protocol int, acpSessionID string) {
	m.forgetSideChatFork(acpSessionID)
	method := acpschema.AgentMethodSessionClose
	params := any(acpschema.CloseSessionRequest{SessionID: acpschema.SessionID(acpSessionID)})
	if protocol == protocolV2 {
		method, params = acpv2.AgentMethodSessionDelete, map[string]any{"sessionId": acpSessionID}
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _ = peer.Call(callCtx, method, params)
}

// applySideChatUpdate turns one of a fork's updates into a side chat event.
func (m *Manager) applySideChatUpdate(fork sideChatFork, raw json.RawMessage) {
	job, err := m.job(fork.jobID)
	if err != nil {
		return
	}
	if fork.protocol == protocolV2 {
		update, err := acpv2.DecodeSessionUpdate(raw)
		if err != nil {
			return
		}
		switch event := update.(type) {
		case acpv2.AgentMessageChunkUpdate:
			m.publishSideChatMessage(job.eventView(), fork.scope, "assistant", acpv2.TextFromContent([]acpv2.ContentBlock{event.Content}), "running")
		case acpv2.AgentThoughtChunkUpdate:
			m.publishSideChatMessage(job.eventView(), fork.scope, "thought", acpv2.TextFromContent([]acpv2.ContentBlock{event.Content}), "running")
		case acpv2.ToolCallUpdateSessionUpdate:
			m.publishSideChatMessage(job.eventView(), fork.scope, "tool", firstNonEmpty(event.ToolCallUpdate.Title, string(event.ToolCallUpdate.ToolCallID)), toolStatusV2(event.ToolCallUpdate.Status))
		case acpv2.PlanSessionUpdate:
		}
		return
	}
	update, err := acpschema.DecodeSessionUpdate(raw)
	if err != nil {
		return
	}
	switch event := update.(type) {
	case acpschema.AgentMessageChunkUpdate:
		m.publishSideChatMessage(job.eventView(), fork.scope, "assistant", contentText(event.Content), "running")
	case acpschema.AgentThoughtChunkUpdate:
		m.publishSideChatMessage(job.eventView(), fork.scope, "thought", contentText(event.Content), "running")
	case acpschema.ToolCallSessionUpdate:
		status := ""
		if event.Status != nil {
			status = string(*event.Status)
		}
		m.publishSideChatMessage(job.eventView(), fork.scope, "tool", firstNonEmpty(event.Title, string(event.ToolCallID)), status)
	case acpschema.ToolCallUpdateSessionUpdate:
		status := ""
		if event.Status != nil {
			status = string(*event.Status)
		}
		m.publishSideChatMessage(job.eventView(), fork.scope, "tool", firstNonEmpty(event.Title, string(event.ToolCallID)), status)
	case acpschema.PlanSessionUpdate:
	}
}

func toolStatusV2(status *acpv2.ToolCallStatus) string {
	if status == nil {
		return ""
	}
	return string(*status)
}

func (m *Manager) publishSideChatMessage(job eventView, scope sideChatScope, role, content, status string) {
	m.publishSideChatEvent(job, scope, role, content, status, nil, nil)
}

func (m *Manager) publishSideChatUserMessage(job eventView, scope sideChatScope, content string, contexts []storage.MessageContext, attachments []storage.Attachment) {
	m.publishSideChatEvent(job, scope, "user", content, "", storage.NormalizeMessageContexts(contexts), attachments)
}

func (m *Manager) publishSideChatEvent(job eventView, scope sideChatScope, role, content, status string, contexts []storage.MessageContext, attachments []storage.Attachment) {
	if strings.TrimSpace(scope.ID) == "" {
		return
	}
	if content == "" && status == "" && len(contexts) == 0 && len(attachments) == 0 {
		return
	}
	m.recordAndPublishDirect(sessionevents.Event{
		SessionID: job.ID,
		Type:      sessionevents.TypeSideChatMessage,
		SideChat: &sessionevents.SideChatEvent{
			ID:              scope.ID,
			Command:         firstNonEmpty(scope.Command, sideChatCommand),
			ParentSessionID: firstNonEmpty(scope.ParentSessionID, job.ID),
			ThreadID:        scope.ThreadID,
			Role:            role,
			Content:         content,
			Status:          status,
			Contexts:        contexts,
			Attachments:     attachments,
		},
		At: time.Now().UTC(),
	})
}
