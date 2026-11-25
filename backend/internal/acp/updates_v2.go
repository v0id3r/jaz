package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"time"

	acpschema "github.com/gluonfield/acp-transport/acp"
	acpv2 "github.com/gluonfield/acp-transport/acp/v2"
	"github.com/gluonfield/acp-transport/jsonrpc"
	"github.com/wins/jaz/backend/internal/sessionevents"
	"github.com/wins/jaz/backend/internal/storage"
)

// applyJobUpdateV2 translates one v2 session/update into the same job
// mutations the v1 path applies. All v2 wire knowledge stays in this file and
// acpv2; the job state and events are protocol-agnostic.
func (m *Manager) applyJobUpdateV2(job *jobState, raw json.RawMessage) {
	if m.applySessionControlsV2(job, raw) {
		return
	}
	m.recordRawUsage(job, raw)
	update, err := acpv2.DecodeSessionUpdate(raw)
	if err != nil {
		return
	}
	var title string
	var currentTitle string
	var publishACP bool
	var messageChunk string
	var messageID string
	var thoughtChunk string
	var thoughtMessageID string
	var toolEvent *sessionevents.ACPToolCall
	var attention bool
	var warning *sessionFailure
	var planned bool
	now := time.Now().UTC()
	job.mu.Lock()
	switch event := update.(type) {
	case acpv2.StateSessionUpdate:
		job.mu.Unlock()
		m.applyStateUpdateV2(job, event)
		return
	case acpv2.SubagentSessionUpdate:
		job.mu.Unlock()
		m.applySubagentUpdateV2(job, event)
		return
	case acpv2.AgentMessageChunkUpdate:
		m.confirmPromptV2Locked(job)
		messageChunk = acpv2.TextFromContent([]acpv2.ContentBlock{event.Content})
		messageID = string(event.MessageID)
		job.noteStreamedMessageV2Locked(messageID)
		m.appendMessageV2Locked(job, messageID, messageChunk)
	case acpv2.AgentMessageUpdate:
		m.confirmPromptV2Locked(job)
		messageID = string(event.MessageID)
		if text := acpv2.TextFromContent(event.Content); text != "" && job.claimMessageV2Locked(messageID) {
			messageChunk = text
			m.appendMessageV2Locked(job, messageID, messageChunk)
		}
	case acpv2.AgentThoughtChunkUpdate:
		thoughtChunk = acpv2.TextFromContent([]acpv2.ContentBlock{event.Content})
		thoughtMessageID = string(event.MessageID)
		job.noteStreamedMessageV2Locked(thoughtMessageID)
		job.appendThoughtLocked(thoughtChunk)
	case acpv2.AgentThoughtUpdate:
		if text := acpv2.TextFromContent(event.Content); text != "" && job.claimMessageV2Locked(string(event.MessageID)) {
			thoughtChunk = text
			thoughtMessageID = string(event.MessageID)
			job.appendThoughtLocked(thoughtChunk)
		}
	case acpv2.ToolCallUpdateSessionUpdate:
		m.confirmPromptV2Locked(job)
		job.endReplyLocked()
		toolEvent = m.recordToolV2Locked(job, event.ToolCallUpdate, now)
	case acpv2.ToolCallContentChunkUpdate:
		toolEvent = m.appendToolContentV2Locked(job, event, now)
	case acpv2.TerminalSessionUpdate:
		exitReported, exitCode, reason := terminalExitV2(raw)
		m.applyTerminalUpdateV2Locked(job, event.TerminalUpdate, exitReported, exitCode, reason, now)
		toolEvent = m.terminalToolEventV2Locked(job, string(event.TerminalID))
	case acpv2.TerminalOutputChunkUpdate:
		m.appendTerminalOutputV2Locked(job, string(event.TerminalID), event.Data)
		if id := job.terminalTool[string(event.TerminalID)]; id != "" {
			m.deferToolPublishLocked(job, id)
		}
	case acpv2.PlanSessionUpdate:
		m.confirmPromptV2Locked(job)
		publishACP, attention = applyPlanV2Locked(job, event)
		planned = publishACP
	case acpv2.PlanRemovedSessionUpdate:
		if len(job.Plan) > 0 {
			job.Plan = sessionevents.PlanCleared
			publishACP = true
		}
	case acpv2.SessionInfoSessionUpdate:
		warning = sessionWarning(event.Meta)
		if warning != nil && warning.notice() {
			messageChunk = "Warning: " + warning.message() + "\n\n"
			job.appendAssistantLocked(messageChunk)
			job.endReplyLocked()
		}
		if nextTitle := strings.TrimSpace(event.Title); nextTitle != "" && !job.promptWrapped {
			currentTitle = job.Title
			title = nextTitle
		}
		if prompt, ok := suggestedPrompt(event.Meta); ok && prompt != job.suggestedPrompt {
			job.suggestedPrompt = prompt
			publishACP = true
		}
	case acpv2.NoticeSessionUpdate:
		text := strings.TrimSpace(event.Title)
		if event.Description != "" {
			text = strings.TrimSpace(text + "\n" + event.Description)
		}
		if text != "" {
			messageChunk = text + "\n"
			messageID = ""
			job.appendAssistantLocked(messageChunk)
			job.endReplyLocked()
		}
	}
	if job.turn != nil {
		progress := warning == nil && (messageChunk != "" || thoughtChunk != "" || toolEvent != nil || planned)
		job.turn.trackProviderRetries(warning, progress)
		retrying := warning != nil && !warning.notice()
		if progress {
			job.turn.agentFailure = nil
		}
		if progress || retrying {
			nextError := ""
			if retrying {
				nextError = warning.message()
			}
			if job.Error != nextError {
				job.Error = nextError
				publishACP = true
			}
		}
	}
	job.UpdatedAt = now
	job.LastEventAt = now
	sessionID := job.ID
	job.mu.Unlock()

	if attention {
		m.touchJobAttention(job)
	}
	if title != "" {
		if session, updated, err := m.store.UpdateSessionTitleFromRuntime(sessionID, title); err == nil {
			job.mu.Lock()
			job.Title = session.Title
			job.mu.Unlock()
			publishACP = publishACP || updated && session.Title != currentTitle
		}
	}
	if messageChunk != "" {
		m.queueACPMessageWithID(job, messageChunk, messageID)
	}
	if thoughtChunk != "" {
		m.queueACPThoughtWithID(job, thoughtChunk, thoughtMessageID)
	}
	if toolEvent != nil {
		m.publishACPTool(job.eventView(), *toolEvent)
	}
	if publishACP {
		m.publishACP(job.eventView())
	}
}

// confirmPromptV2Locked mirrors the v1 path's prompt confirmation on visible
// output; the wire-level ack usually confirms first on v2.
func (m *Manager) confirmPromptV2Locked(job *jobState) {
	if err := m.setPromptStateLocked(job, storage.PromptConfirmed); err != nil {
		m.log.Error("record prompt confirmation", "session", job.ID, "error", err)
	}
}

// appendMessageV2Locked appends an agent message chunk: every v2 message
// counts toward the result; the reply still resets on tools.
func (m *Manager) appendMessageV2Locked(job *jobState, messageID, chunk string) {
	job.appendAssistantLocked(chunk)
	if chunk != "" {
		job.savedAssistantLen = len(job.Assistant)
	}
	job.appendResultMessageLocked(messageID, "", chunk)
	job.replyLocked(messageID, chunk)
}

// claimMessageV2Locked reports whether a whole-message upsert brings a
// message the job has not seen; streamed messages are already in the
// transcript, and a v2 content correction has no transcript representation.
func (j *jobState) claimMessageV2Locked(messageID string) bool {
	if messageID == "" {
		return true
	}
	if j.seenMessagesV2[messageID] || messageID == j.replyID {
		return false
	}
	j.noteStreamedMessageV2Locked(messageID)
	return true
}

// noteStreamedMessageV2Locked remembers a message ID the job already holds,
// streamed or upserted, so a later whole-message upsert of it is a
// correction, not new content.
func (j *jobState) noteStreamedMessageV2Locked(messageID string) {
	if messageID == "" {
		return
	}
	if j.seenMessagesV2 == nil {
		j.seenMessagesV2 = map[string]bool{}
	}
	j.seenMessagesV2[messageID] = true
}

func (m *Manager) recordToolV2Locked(job *jobState, update acpv2.ToolCallUpdate, now time.Time) *sessionevents.ACPToolCall {
	src := toolUpdateSnapshotV2(update)
	src.SubagentID = toolCallSubagentID(job.ACPAgent, update.Meta)
	if update.Capabilities != nil && update.Capabilities.Cancel != nil {
		if job.cancellableToolsV2 == nil {
			job.cancellableToolsV2 = map[string]bool{}
		}
		job.cancellableToolsV2[src.ID] = true
		src.Cancellable = true
	}
	switch src.Status {
	case "completed", "failed", "cancelled":
		delete(job.cancellableToolsV2, src.ID)
	}
	for _, content := range update.Content {
		if terminalID := toolContentTerminalID(content); terminalID != "" {
			if job.terminalTool == nil {
				job.terminalTool = map[string]string{}
			}
			job.terminalTool[terminalID] = src.ID
			src.Runtime.TerminalID = terminalID
			// A terminal may have advertised its cancel before the call
			// linked it (see applyTerminalUpdateV2Locked).
			if terminal := job.terminalsV2[terminalID]; terminal != nil && terminal.cancellable {
				src.Cancellable = true
			}
		}
	}
	src.UpdatedAt = now
	if job.toolByID == nil {
		job.toolByID = map[string]sessionevents.ACPToolCall{}
	}
	_, exists := job.toolByID[src.ID]
	call := job.toolByID[src.ID]
	if call.StartedAt.IsZero() {
		call.StartedAt = now
	}
	mergeToolCall(&call, src)
	previous := job.toolByID[src.ID]
	if exists && previous.EqualTranscript(call) {
		return nil
	}
	job.toolByID[src.ID] = call
	job.ToolCalls = sortedToolCalls(job.toolByID)
	job.LastToolAt = now
	if exists && terminalOutputOnly(previous, call) {
		m.deferToolPublishLocked(job, src.ID)
		return nil
	}
	delete(job.deferredToolIDs, src.ID)
	return &call
}

func toolUpdateSnapshotV2(update acpv2.ToolCallUpdate) sessionevents.ACPToolCall {
	src := sessionevents.ACPToolCall{
		ID:        string(update.ToolCallID),
		Title:     update.Title,
		ToolName:  update.Name,
		Content:   normalizeToolContent(v1ToolContent(update.Content)),
		Locations: normalizeToolLocationsV2(update.Locations),
		RawInput:  boundedRawInput(update.RawInput),
		RawOutput: boundedRawOutput(update.RawOutput),
		Runtime:   acpToolRuntime(update.Meta),
	}
	if src.ToolName == "" {
		src.ToolName = toolUpdateName(acpschema.ToolCallUpdate{Meta: update.Meta})
	}
	if update.Kind != nil {
		src.Kind = string(*update.Kind)
	}
	if update.Status != nil {
		src.Status = string(*update.Status)
	}
	normalizeToolCall(&src)
	return src
}

func v1ToolContent(content []acpv2.ToolCallContent) []acpschema.ToolCallContent {
	if len(content) == 0 {
		return nil
	}
	out := make([]acpschema.ToolCallContent, 0, len(content))
	for _, item := range content {
		out = append(out, acpschema.ToolCallContent(item))
	}
	return out
}

func normalizeToolLocationsV2(locations []acpv2.ToolCallLocation) []sessionevents.ACPToolLocation {
	if len(locations) == 0 {
		return nil
	}
	converted := make([]acpschema.ToolCallLocation, 0, len(locations))
	for _, location := range locations {
		converted = append(converted, acpschema.ToolCallLocation{Line: location.Line, Path: string(location.Path)})
	}
	return normalizeToolLocations(converted)
}

// toolContentTerminalID reads the terminal reference of a terminal tool-call
// content item.
func toolContentTerminalID(raw acpv2.ToolCallContent) string {
	var item struct {
		Type       string `json:"type"`
		TerminalID string `json:"terminalId"`
	}
	if json.Unmarshal(raw, &item) != nil || item.Type != "terminal" {
		return ""
	}
	return item.TerminalID
}

func (m *Manager) appendToolContentV2Locked(job *jobState, event acpv2.ToolCallContentChunkUpdate, now time.Time) *sessionevents.ACPToolCall {
	id := string(event.ToolCallID)
	call, ok := job.toolByID[id]
	if !ok {
		return nil
	}
	content := normalizeToolContent(v1ToolContent([]acpv2.ToolCallContent{event.Content}))
	if len(content) == 0 {
		return nil
	}
	call.Content = append(call.Content, content...)
	call.UpdatedAt = now
	job.toolByID[id] = call
	job.ToolCalls = sortedToolCalls(job.toolByID)
	job.LastToolAt = now
	return &call
}

type terminalStateV2 struct {
	command     string
	cancellable bool
	exited      bool
	exitCode    *int64
	signal      string
	reason      string
}

// terminalExitV2 reads the exit of a raw terminal_update: whether it carries
// one, its exit code, and the reason its `_meta` names. The generated type
// decodes an absent exitCode as zero, and an agent that reports no exit code
// (a background command's end) must not read as exit 0.
func terminalExitV2(raw json.RawMessage) (reported bool, exitCode *int64, reason string) {
	var probe struct {
		ExitStatus *struct {
			ExitCode *int64         `json:"exitCode"`
			Meta     map[string]any `json:"_meta"`
		} `json:"exitStatus"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.ExitStatus == nil {
		return false, nil, ""
	}
	// The adapters namespace their metadata: claudeCode for Claude, codex for Codex.
	for _, key := range []string{"claudeCode", "codex"} {
		if meta, _ := probe.ExitStatus.Meta[key].(map[string]any); meta != nil {
			if reason, _ := meta["reason"].(string); reason != "" {
				return true, probe.ExitStatus.ExitCode, reason
			}
		}
	}
	return true, probe.ExitStatus.ExitCode, ""
}

func (m *Manager) applyTerminalUpdateV2Locked(job *jobState, update acpv2.TerminalUpdate, exitReported bool, exitCode *int64, reason string, now time.Time) {
	id := string(update.TerminalID)
	if job.terminalsV2 == nil {
		job.terminalsV2 = map[string]*terminalStateV2{}
	}
	terminal := job.terminalsV2[id]
	if terminal == nil {
		terminal = &terminalStateV2{}
		job.terminalsV2[id] = terminal
	}
	if update.Command != "" {
		terminal.command = update.Command
	}
	if update.Capabilities != nil && update.Capabilities.Cancel != nil {
		terminal.cancellable = true
	}
	if update.ExitStatus != nil {
		terminal.exited = true
		terminal.exitCode = exitCode
		terminal.signal = update.ExitStatus.Signal
		terminal.reason = reason
	}
	if callID := job.terminalTool[id]; callID != "" {
		call := job.toolByID[callID]
		call.Runtime.TerminalID = id
		// A cancellable terminal makes its call cancellable: the cancel goes
		// to the terminal (see CancelToolCall), but the card offers it.
		if terminal.cancellable {
			call.Cancellable = true
		}
		if update.Cwd != nil {
			call.Runtime.TerminalCwd = string(*update.Cwd)
		}
		if update.Output != nil && update.Output.Data != "" {
			if decoded := decodeTerminalOutputV2(update.Output.Data); decoded != nil {
				call.Runtime.TerminalOutput = tailToolText(string(decoded))
			}
		}
		if update.ExitStatus != nil {
			if exitReported && exitCode != nil {
				code := int(*exitCode)
				call.Runtime.TerminalExitCode = &code
			}
			if update.ExitStatus.Signal != "" {
				signal := update.ExitStatus.Signal
				call.Runtime.TerminalExitSignal = &signal
			}
		}
		call.UpdatedAt = now
		job.toolByID[callID] = call
		job.ToolCalls = sortedToolCalls(job.toolByID)
	}
}

func (m *Manager) terminalToolEventV2Locked(job *jobState, terminalID string) *sessionevents.ACPToolCall {
	callID := job.terminalTool[terminalID]
	if callID == "" {
		return nil
	}
	call := job.toolByID[callID]
	return &call
}

func (m *Manager) appendTerminalOutputV2Locked(job *jobState, terminalID, data string) {
	callID := job.terminalTool[terminalID]
	if callID == "" || data == "" {
		return
	}
	decoded := decodeTerminalOutputV2(data)
	if len(decoded) == 0 {
		return
	}
	call := job.toolByID[callID]
	call.Runtime.TerminalID = terminalID
	// Bytes, not strings: a UTF-8 character may split across chunks.
	call.Runtime.TerminalOutput = tailToolText(string(append([]byte(call.Runtime.TerminalOutput), decoded...)))
	job.toolByID[callID] = call
}

// decodeTerminalOutputV2 decodes a terminal output payload: v2 carries
// terminal bytes base64-encoded, each chunk independently. Invalid data is
// dropped, never fatal to the turn.
func decodeTerminalOutputV2(data string) []byte {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil
	}
	return decoded
}

func applyPlanV2Locked(job *jobState, event acpv2.PlanSessionUpdate) (publish, attention bool) {
	var plan struct {
		Type    string `json:"type"`
		Entries []struct {
			Content  string `json:"content"`
			Status   string `json:"status"`
			Priority string `json:"priority"`
		} `json:"entries"`
	}
	if json.Unmarshal(event.Plan, &plan) != nil || plan.Type != "items" {
		return false, false
	}
	entries := make([]sessionevents.PlanEntry, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		entries = append(entries, sessionevents.PlanEntry{Content: entry.Content, Status: entry.Status, Priority: entry.Priority})
	}
	normalized, ok := sessionevents.NormalizeProgressEntries(entries)
	if !ok {
		if len(job.Plan) > 0 {
			job.Plan = sessionevents.PlanCleared
			return true, false
		}
		return false, false
	}
	wasEmpty := len(job.Plan) == 0
	publish = !slices.Equal(job.Plan, normalized)
	job.Plan = normalized
	return publish, publish && wasEmpty && len(normalized) > 0
}

// applySessionControlsV2 routes the v2 config and command payloads through
// the shared session-controls parsing after configId normalization. A
// set_config_option response carries configOptions without a sessionUpdate
// kind, so the content, not the kind, decides.
func (m *Manager) applySessionControlsV2(job *jobState, raw json.RawMessage) bool {
	return m.applySessionControls(job, normalizeConfigOptionsV2(raw))
}

// requestPermissionV2 translates the v2 permission request (title, subject)
// into the shared permission flow.
func (m *Manager) requestPermissionV2(ctx context.Context, raw json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	var req acpv2.RequestPermissionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, jsonrpc.InvalidParams("invalid session/request_permission", map[string]any{"error": err.Error()})
	}
	job := m.jobByACP(string(req.SessionID))
	if job == nil {
		return nil, jsonrpc.InvalidParams("unknown acp session", nil)
	}
	translated := acpschema.RequestPermissionRequest{SessionID: acpschema.SessionID(req.SessionID)}
	for _, option := range req.Options {
		translated.Options = append(translated.Options, acpschema.PermissionOption{
			OptionID: acpschema.PermissionOptionID(option.OptionID),
			Name:     option.Name,
			Kind:     acpschema.PermissionOptionKind(option.Kind),
		})
	}
	if req.Subject != nil {
		var subject struct {
			Type     string          `json:"type"`
			ToolCall json.RawMessage `json:"toolCall"`
		}
		if json.Unmarshal(*req.Subject, &subject) == nil && len(subject.ToolCall) > 0 {
			var toolCall acpschema.ToolCallUpdate
			if json.Unmarshal(subject.ToolCall, &toolCall) == nil {
				translated.ToolCall = toolCall
			}
		}
	}
	if translated.ToolCall.Title == "" {
		translated.ToolCall.Title = req.Title
	}
	return m.awaitPermission(ctx, job, translated)
}
