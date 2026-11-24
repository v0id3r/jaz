package acpv2

import (
	"encoding/json"
	"fmt"
)

// The schema source is agent-client-protocol schema/v2 (beta, see meta.json).
// Regenerate types_gen.go with the acp-transport genacp generator pointed at
// this directory: genacp -schema . -out types_gen.go -package acpv2.

type SessionUpdateKind string

const (
	SessionUpdateUserMessageChunk       SessionUpdateKind = "user_message_chunk"
	SessionUpdateUserMessage            SessionUpdateKind = "user_message"
	SessionUpdateAgentMessageChunk      SessionUpdateKind = "agent_message_chunk"
	SessionUpdateAgentMessage           SessionUpdateKind = "agent_message"
	SessionUpdateAgentThoughtChunk      SessionUpdateKind = "agent_thought_chunk"
	SessionUpdateAgentThought           SessionUpdateKind = "agent_thought"
	SessionUpdateState                  SessionUpdateKind = "state_update"
	SessionUpdateToolCallContentChunk   SessionUpdateKind = "tool_call_content_chunk"
	SessionUpdateToolCallUpdate         SessionUpdateKind = "tool_call_update"
	SessionUpdateTerminalUpdate         SessionUpdateKind = "terminal_update"
	SessionUpdateTerminalOutputChunk    SessionUpdateKind = "terminal_output_chunk"
	SessionUpdatePlan                   SessionUpdateKind = "plan_update"
	SessionUpdatePlanRemoved            SessionUpdateKind = "plan_removed"
	SessionUpdateAvailableCommands      SessionUpdateKind = "available_commands_update"
	SessionUpdateConfigOption           SessionUpdateKind = "config_option_update"
	SessionUpdateSessionInfo            SessionUpdateKind = "session_info_update"
	SessionUpdateUsage                  SessionUpdateKind = "usage_update"
	SessionUpdateNotice                 SessionUpdateKind = "notice"
	SessionUpdateCompaction             SessionUpdateKind = "compaction_update"
	SessionUpdateCompactionSummaryChunk SessionUpdateKind = "compaction_summary_chunk"
	SessionUpdateReplayComplete         SessionUpdateKind = "replay_complete"
	SessionUpdateSubagent               SessionUpdateKind = "subagent_update"
	SessionUpdateSessionMessage         SessionUpdateKind = "session_message"
	SessionUpdateSessionMessageChunk    SessionUpdateKind = "session_message_chunk"
)

type DecodedSessionUpdate interface {
	SessionUpdateKind() SessionUpdateKind
	RawJSON() json.RawMessage
}

type sessionUpdateBase struct {
	SessionUpdate SessionUpdateKind `json:"sessionUpdate"`
	raw           json.RawMessage
}

func (u sessionUpdateBase) SessionUpdateKind() SessionUpdateKind { return u.SessionUpdate }
func (u sessionUpdateBase) RawJSON() json.RawMessage {
	return append(json.RawMessage(nil), u.raw...)
}
func (u *sessionUpdateBase) setRaw(raw json.RawMessage) {
	u.raw = append(json.RawMessage(nil), raw...)
}

type UserMessageChunkUpdate struct {
	sessionUpdateBase
	ContentChunk
}

type UserMessageUpdate struct {
	sessionUpdateBase
	UserMessage
}

type AgentMessageChunkUpdate struct {
	sessionUpdateBase
	ContentChunk
}

type AgentMessageUpdate struct {
	sessionUpdateBase
	AgentMessage
}

type AgentThoughtChunkUpdate struct {
	sessionUpdateBase
	ContentChunk
}

type AgentThoughtUpdate struct {
	sessionUpdateBase
	AgentThought
}

type StateSessionUpdate struct {
	sessionUpdateBase
	StateUpdateEvent
}

type ToolCallUpdateSessionUpdate struct {
	sessionUpdateBase
	ToolCallUpdate
}

type ToolCallContentChunkUpdate struct {
	sessionUpdateBase
	ToolCallContentChunk
}

type TerminalSessionUpdate struct {
	sessionUpdateBase
	TerminalUpdate
}

type TerminalOutputChunkUpdate struct {
	sessionUpdateBase
	TerminalOutputChunk
}

type PlanSessionUpdate struct {
	sessionUpdateBase
	PlanUpdate
}

type PlanRemovedSessionUpdate struct {
	sessionUpdateBase
	PlanRemoved
}

type AvailableCommandsSessionUpdate struct {
	sessionUpdateBase
	AvailableCommandsUpdate
}

type ConfigOptionSessionUpdate struct {
	sessionUpdateBase
	ConfigOptionUpdate
}

type SessionInfoSessionUpdate struct {
	sessionUpdateBase
	SessionInfoUpdate
}

type UsageSessionUpdate struct {
	sessionUpdateBase
	UsageUpdate
}

type NoticeSessionUpdate struct {
	sessionUpdateBase
	Notice
}

type CompactionSessionUpdate struct {
	sessionUpdateBase
	CompactionUpdate
}

type CompactionSummaryChunkUpdate struct {
	sessionUpdateBase
	CompactionSummaryChunk
}

type ReplayCompleteSessionUpdate struct {
	sessionUpdateBase
	ReplayComplete
}

type SubagentSessionUpdate struct {
	sessionUpdateBase
	SubagentUpdate
}

type SessionMessageUpdate struct {
	sessionUpdateBase
	SessionMessage
}

type SessionMessageChunkUpdate struct {
	sessionUpdateBase
	SessionMessageChunk
}

type UnknownSessionUpdate struct {
	sessionUpdateBase
}

// sessionUpdatePtr constrains *T to a session-update value whose embedded base
// can be stamped with the original raw JSON after decoding.
type sessionUpdatePtr[T any] interface {
	*T
	DecodedSessionUpdate
	setRaw(json.RawMessage)
}

func decodeUpdateAs[T DecodedSessionUpdate, PT sessionUpdatePtr[T]](raw json.RawMessage) (DecodedSessionUpdate, error) {
	out := new(T)
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, err
	}
	PT(out).setRaw(raw)
	value := *out
	return value, nil
}

func decodeStateUpdate(raw json.RawMessage) (DecodedSessionUpdate, error) {
	var event StateUpdateEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, err
	}
	event.Background = decodeBackground(raw)
	event.BackgroundReported = event.hasBackground(raw)
	base := sessionUpdateBase{SessionUpdate: SessionUpdateState}
	base.setRaw(raw)
	return StateSessionUpdate{sessionUpdateBase: base, StateUpdateEvent: event}, nil
}

var sessionUpdateDecoders = map[SessionUpdateKind]func(json.RawMessage) (DecodedSessionUpdate, error){
	SessionUpdateUserMessageChunk:       decodeUpdateAs[UserMessageChunkUpdate],
	SessionUpdateUserMessage:            decodeUpdateAs[UserMessageUpdate],
	SessionUpdateAgentMessageChunk:      decodeUpdateAs[AgentMessageChunkUpdate],
	SessionUpdateAgentMessage:           decodeUpdateAs[AgentMessageUpdate],
	SessionUpdateAgentThoughtChunk:      decodeUpdateAs[AgentThoughtChunkUpdate],
	SessionUpdateAgentThought:           decodeUpdateAs[AgentThoughtUpdate],
	SessionUpdateState:                  decodeStateUpdate,
	SessionUpdateToolCallContentChunk:   decodeUpdateAs[ToolCallContentChunkUpdate],
	SessionUpdateToolCallUpdate:         decodeUpdateAs[ToolCallUpdateSessionUpdate],
	SessionUpdateTerminalUpdate:         decodeUpdateAs[TerminalSessionUpdate],
	SessionUpdateTerminalOutputChunk:    decodeUpdateAs[TerminalOutputChunkUpdate],
	SessionUpdatePlan:                   decodeUpdateAs[PlanSessionUpdate],
	SessionUpdatePlanRemoved:            decodeUpdateAs[PlanRemovedSessionUpdate],
	SessionUpdateAvailableCommands:      decodeUpdateAs[AvailableCommandsSessionUpdate],
	SessionUpdateConfigOption:           decodeUpdateAs[ConfigOptionSessionUpdate],
	SessionUpdateSessionInfo:            decodeUpdateAs[SessionInfoSessionUpdate],
	SessionUpdateUsage:                  decodeUpdateAs[UsageSessionUpdate],
	SessionUpdateNotice:                 decodeUpdateAs[NoticeSessionUpdate],
	SessionUpdateCompaction:             decodeUpdateAs[CompactionSessionUpdate],
	SessionUpdateCompactionSummaryChunk: decodeUpdateAs[CompactionSummaryChunkUpdate],
	SessionUpdateReplayComplete:         decodeUpdateAs[ReplayCompleteSessionUpdate],
	SessionUpdateSubagent:               decodeUpdateAs[SubagentSessionUpdate],
	SessionUpdateSessionMessage:         decodeUpdateAs[SessionMessageUpdate],
	SessionUpdateSessionMessageChunk:    decodeUpdateAs[SessionMessageChunkUpdate],
}

func DecodeSessionUpdate(raw json.RawMessage) (DecodedSessionUpdate, error) {
	var env struct {
		SessionUpdate SessionUpdateKind `json:"sessionUpdate"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if decode, ok := sessionUpdateDecoders[env.SessionUpdate]; ok {
		return decode(raw)
	}
	base := sessionUpdateBase{SessionUpdate: env.SessionUpdate}
	base.setRaw(raw)
	return UnknownSessionUpdate{sessionUpdateBase: base}, nil
}

// StateUpdateEvent is the merged view of the four state_update variants. The
// error stop reason carries Error beside stopReason; usage never appears here
// (the Agent reports it with usage_update).
type StateUpdateEvent struct {
	State      string
	StopReason string
	Error      *Error
	Meta       map[string]any
	Background []BackgroundWorkItem
	// BackgroundReported distinguishes an omitted background list (not
	// reported) from a concrete one ([] means no background work).
	BackgroundReported bool
}

func (e StateUpdateEvent) Terminal() bool { return e.State == "idle" }

type BackgroundWorkItem struct {
	Type     string
	Terminal TerminalID
	ToolCall ToolCallID
	Session  SessionID
}

func decodeBackground(raw json.RawMessage) []BackgroundWorkItem {
	var env struct {
		Background *[]json.RawMessage `json:"background"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Background == nil {
		return nil
	}
	items := make([]BackgroundWorkItem, 0, len(*env.Background))
	for _, entry := range *env.Background {
		var item struct {
			Type       string     `json:"type"`
			TerminalID TerminalID `json:"terminalId"`
			ToolCallID ToolCallID `json:"toolCallId"`
			SessionID  SessionID  `json:"sessionId"`
		}
		if json.Unmarshal(entry, &item) != nil || item.Type == "" {
			continue
		}
		items = append(items, BackgroundWorkItem{Type: item.Type, Terminal: item.TerminalID, ToolCall: item.ToolCallID, Session: item.SessionID})
	}
	return items
}

// BackgroundReportedIn reports whether raw carries a concrete background list.
func (e StateUpdateEvent) hasBackground(raw json.RawMessage) bool {
	var env struct {
		Background json.RawMessage `json:"background"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return false
	}
	return len(env.Background) > 0 && string(env.Background) != "null"
}

type ContentBlockKind string

const (
	ContentBlockText         ContentBlockKind = "text"
	ContentBlockImage        ContentBlockKind = "image"
	ContentBlockAudio        ContentBlockKind = "audio"
	ContentBlockResourceLink ContentBlockKind = "resource_link"
	ContentBlockResource     ContentBlockKind = "resource"
)

type DecodedContentBlock interface {
	ContentKind() ContentBlockKind
}

type TextContentBlock struct {
	Kind ContentBlockKind `json:"type"`
	TextContent
}

func (b TextContentBlock) ContentKind() ContentBlockKind { return b.Kind }

type ImageContentBlock struct {
	Kind ContentBlockKind `json:"type"`
	ImageContent
}

func (b ImageContentBlock) ContentKind() ContentBlockKind { return b.Kind }

type AudioContentBlock struct {
	Kind ContentBlockKind `json:"type"`
	AudioContent
}

func (b AudioContentBlock) ContentKind() ContentBlockKind { return b.Kind }

type ResourceLinkContentBlock struct {
	Kind ContentBlockKind `json:"type"`
	ResourceLink
}

func (b ResourceLinkContentBlock) ContentKind() ContentBlockKind { return b.Kind }

type EmbeddedResourceContentBlock struct {
	Kind ContentBlockKind `json:"type"`
	EmbeddedResource
}

func (b EmbeddedResourceContentBlock) ContentKind() ContentBlockKind { return b.Kind }

type UnknownContentBlock struct {
	Kind ContentBlockKind `json:"type"`
	Raw  json.RawMessage  `json:"-"`
}

func (b UnknownContentBlock) ContentKind() ContentBlockKind { return b.Kind }

func decodeContentAs[T DecodedContentBlock](data []byte) (DecodedContentBlock, error) {
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

var contentBlockDecoders = map[ContentBlockKind]func([]byte) (DecodedContentBlock, error){
	ContentBlockText:         decodeContentAs[TextContentBlock],
	ContentBlockImage:        decodeContentAs[ImageContentBlock],
	ContentBlockAudio:        decodeContentAs[AudioContentBlock],
	ContentBlockResourceLink: decodeContentAs[ResourceLinkContentBlock],
	ContentBlockResource:     decodeContentAs[EmbeddedResourceContentBlock],
}

func DecodeContentBlock(raw ContentBlock) (DecodedContentBlock, error) {
	data := json.RawMessage(raw)
	var env struct {
		Type ContentBlockKind `json:"type"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if decode, ok := contentBlockDecoders[env.Type]; ok {
		return decode(data)
	}
	return UnknownContentBlock{Kind: env.Type, Raw: append(json.RawMessage(nil), data...)}, nil
}

// TextFromContent renders the plain text of a content array; non-text blocks
// are skipped.
func TextFromContent(content []ContentBlock) string {
	var out []byte
	for _, block := range content {
		decoded, err := DecodeContentBlock(block)
		if err != nil {
			continue
		}
		if text, ok := decoded.(TextContentBlock); ok {
			out = append(out, text.Text...)
		}
	}
	return string(out)
}

const (
	RequestPermissionOutcomeCancelled = "cancelled"
	RequestPermissionOutcomeSelected  = "selected"
)

func CancelledPermissionOutcomeValue() RequestPermissionOutcome {
	return RequestPermissionOutcome(mustRaw(map[string]any{"outcome": RequestPermissionOutcomeCancelled}))
}

func SelectedPermissionOutcomeFor(optionID PermissionOptionID) RequestPermissionOutcome {
	return RequestPermissionOutcome(mustRaw(map[string]any{"outcome": RequestPermissionOutcomeSelected, "optionId": optionID}))
}

func RequestPermissionResponseCancelled() RequestPermissionResponse {
	return RequestPermissionResponse{Outcome: CancelledPermissionOutcomeValue()}
}

func RequestPermissionResponseSelected(optionID PermissionOptionID) RequestPermissionResponse {
	return RequestPermissionResponse{Outcome: SelectedPermissionOutcomeFor(optionID)}
}

// EncodeSessionConfigValue encodes the value-id variant of a
// session/set_config_option value, the default when the request carries no
// type discriminator.
func EncodeSessionConfigValue(id SessionConfigValueID) json.RawMessage {
	return mustRaw(id)
}

// ValueID decodes the value-id variant of the request's value.
func (r SetSessionConfigOptionRequest) ValueID() (SessionConfigValueID, bool) {
	var id SessionConfigValueID
	return id, json.Unmarshal(r.Value, &id) == nil
}

func mustRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal ACP helper value: %v", err))
	}
	return raw
}
