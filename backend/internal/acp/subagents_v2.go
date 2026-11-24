package acp

import (
	"encoding/json"
	"sync"
	"time"

	acpv2 "github.com/gluonfield/acp-transport/acp/v2"
	"github.com/wins/jaz/backend/internal/sessionevents"
)

// subagentLinkV2 is one child session a v2 agent announced with
// subagent_update. The child's transcript lives in the parent session's
// journal as subagent_message events, apart from the parent thread's feed.
type subagentLinkV2 struct {
	parentID string
	agent    string
	id       string
	title    string
	task     string
	state    string
}

type subagentRegistryV2 struct {
	mu    sync.Mutex
	links map[string]*subagentLinkV2
}

func newSubagentRegistryV2() *subagentRegistryV2 {
	return &subagentRegistryV2{links: map[string]*subagentLinkV2{}}
}

func (r *subagentRegistryV2) lookup(childID string) *subagentLinkV2 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.links[childID]
}

// dropParent forgets the children of a session whose agent process ended.
func (r *subagentRegistryV2) dropParent(parentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, link := range r.links {
		if link.parentID == parentID {
			delete(r.links, id)
		}
	}
}

func (r *subagentRegistryV2) title(childID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if link := r.links[childID]; link != nil {
		return link.title
	}
	return ""
}

// applySubagentUpdateV2 registers or patches the child session a parent
// announced and publishes the panel record for it.
func (m *Manager) applySubagentUpdateV2(job *jobState, update acpv2.SubagentSessionUpdate) {
	childID := string(update.SessionID)
	if childID == "" {
		return
	}
	registry := m.subagentsV2
	registry.mu.Lock()
	link := registry.links[childID]
	if link == nil {
		link = &subagentLinkV2{parentID: job.ID, agent: job.ACPAgent, id: childID}
		registry.links[childID] = link
	}
	if update.Title != "" {
		link.title = update.Title
	}
	if update.Description != "" {
		link.task = update.Description
	}
	if update.State != nil {
		var state struct {
			State string `json:"state"`
		}
		if json.Unmarshal(*update.State, &state) == nil && state.State != "" {
			link.state = state.State
		}
	}
	record := subagentPanelRecordV2(link)
	registry.mu.Unlock()
	m.publishProviderSubagents(job.eventView(), []sessionevents.ProviderSubagentEvent{record})
}

func subagentPanelRecordV2(link *subagentLinkV2) sessionevents.ProviderSubagentEvent {
	status := sessionevents.SubagentRunning
	switch link.state {
	case "idle":
		status = sessionevents.SubagentCompleted
	case "unknown":
		// No information is not a failure: report no change.
		status = ""
	}
	summary := link.task
	if summary == "" {
		summary = link.title
	}
	return sessionevents.ProviderSubagentEvent{
		Provider: link.agent,
		ID:       link.id,
		Name:     link.title,
		Task:     link.task,
		Status:   status,
		Summary:  summary,
	}
}

// applySubagentChildUpdateV2 records a child session's own updates as
// subagent_message events of the parent's journal, apart from the parent
// thread's feed, so the transcript survives a Jaz restart.
func (m *Manager) applySubagentChildUpdateV2(childID string, raw json.RawMessage) {
	link := m.subagentsV2.lookup(childID)
	if link == nil {
		return
	}
	update, err := acpv2.DecodeSessionUpdate(raw)
	if err != nil {
		return
	}
	entry := &sessionevents.SubagentMessageEvent{ID: childID}
	switch event := update.(type) {
	case acpv2.AgentMessageChunkUpdate:
		entry.Role, entry.Content = "assistant", acpv2.TextFromContent([]acpv2.ContentBlock{event.Content})
	case acpv2.AgentMessageUpdate:
		entry.Role, entry.Content = "assistant", acpv2.TextFromContent(event.Content)
	case acpv2.AgentThoughtChunkUpdate:
		entry.Role, entry.Content = "thought", acpv2.TextFromContent([]acpv2.ContentBlock{event.Content})
	case acpv2.SessionMessageUpdate:
		entry.Role, entry.Content = "user", acpv2.TextFromContent(event.Content)
	case acpv2.SessionMessageChunkUpdate:
		entry.Role, entry.Content = "user", acpv2.TextFromContent([]acpv2.ContentBlock{event.Content})
	case acpv2.ToolCallUpdateSessionUpdate:
		entry.Role, entry.Content = "tool", event.Title
		if event.Status != nil {
			entry.Status = string(*event.Status)
		}
	case acpv2.StateSessionUpdate:
		m.subagentsV2.mu.Lock()
		link.state = event.State
		record := subagentPanelRecordV2(link)
		m.subagentsV2.mu.Unlock()
		if parent := m.jobByID(link.parentID); parent != nil {
			m.publishProviderSubagents(parent.eventView(), []sessionevents.ProviderSubagentEvent{record})
		}
		return
	default:
		return
	}
	if entry.Content == "" && entry.Role != "tool" {
		return
	}
	// The transcript goes to the same journals as the subagent's card, so a
	// parent-visible thread can open it too.
	sessionIDs := []string{link.parentID}
	if parent := m.jobByID(link.parentID); parent != nil {
		sessionIDs = surfaceSessionIDs(parent.eventView())
	}
	now := time.Now().UTC()
	events := make([]sessionevents.Event, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		message := *entry
		events = append(events, sessionevents.Event{
			SessionID:       sessionID,
			Type:            sessionevents.TypeSubagentMessage,
			SubagentMessage: &message,
			At:              now,
		})
	}
	m.recordAndPublishEventListDirect(events)
}
