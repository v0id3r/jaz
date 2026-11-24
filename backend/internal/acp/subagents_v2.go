package acp

import (
	"encoding/json"
	"sync"
	"time"

	acpv2 "github.com/gluonfield/acp-transport/acp/v2"
	"github.com/wins/jaz/backend/internal/sessionevents"
)

// subagentLinkV2 is one child session a v2 agent announced with
// subagent_update. The child's transcript is kept here, apart from the parent
// thread's feed; a dedicated UI for it is a later step.
type subagentLinkV2 struct {
	parentID   string
	agent      string
	id         string
	title      string
	task       string
	state      string
	transcript []subagentTranscriptEntryV2
}

type subagentTranscriptEntryV2 struct {
	Kind string    `json:"kind"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
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
		status = sessionevents.SubagentFailed
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

// applySubagentChildUpdateV2 records a child session's own updates in its
// link, apart from the parent thread's transcript.
func (m *Manager) applySubagentChildUpdateV2(childID string, raw json.RawMessage) {
	link := m.subagentsV2.lookup(childID)
	if link == nil {
		return
	}
	update, err := acpv2.DecodeSessionUpdate(raw)
	if err != nil {
		return
	}
	entry := subagentTranscriptEntryV2{At: time.Now().UTC()}
	republish := false
	switch event := update.(type) {
	case acpv2.AgentMessageChunkUpdate:
		entry.Kind, entry.Text = "assistant", acpv2.TextFromContent([]acpv2.ContentBlock{event.Content})
	case acpv2.AgentMessageUpdate:
		entry.Kind, entry.Text = "assistant", acpv2.TextFromContent(event.Content)
	case acpv2.AgentThoughtChunkUpdate:
		entry.Kind, entry.Text = "thought", acpv2.TextFromContent([]acpv2.ContentBlock{event.Content})
	case acpv2.SessionMessageUpdate:
		entry.Kind, entry.Text = "message", acpv2.TextFromContent(event.Content)
	case acpv2.SessionMessageChunkUpdate:
		entry.Kind, entry.Text = "message", acpv2.TextFromContent([]acpv2.ContentBlock{event.Content})
	case acpv2.ToolCallUpdateSessionUpdate:
		entry.Kind = "tool"
		entry.Text = event.Title
		if event.Status != nil {
			entry.Text += " (" + string(*event.Status) + ")"
		}
	case acpv2.StateSessionUpdate:
		link.state = event.State
		republish = true
	default:
		return
	}
	m.subagentsV2.mu.Lock()
	if entry.Text != "" || entry.Kind == "tool" {
		link.transcript = append(link.transcript, entry)
	}
	record := subagentPanelRecordV2(link)
	m.subagentsV2.mu.Unlock()
	if republish {
		if parent := m.jobByID(link.parentID); parent != nil {
			m.publishProviderSubagents(parent.eventView(), []sessionevents.ProviderSubagentEvent{record})
		}
	}
}

// subagentTranscriptV2 returns the child's recorded transcript; the read side
// for a dedicated UI.
func (m *Manager) subagentTranscriptV2(childID string) []subagentTranscriptEntryV2 {
	link := m.subagentsV2.lookup(childID)
	if link == nil {
		return nil
	}
	m.subagentsV2.mu.Lock()
	defer m.subagentsV2.mu.Unlock()
	return append([]subagentTranscriptEntryV2(nil), link.transcript...)
}
