package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	acpschema "github.com/gluonfield/acp-transport/acp"
	acpv2 "github.com/gluonfield/acp-transport/acp/v2"
	"github.com/gluonfield/acp-transport/jsonrpc"
	"github.com/wins/jaz/backend/internal/sessionevents"
)

// runPromptCallV2 sends a v2 prompt. The response only acknowledges the
// prompt's insertion; the turn ends when the agent reports state_update idle,
// which applyStateUpdateV2 turns into the shared completion path.
func (m *Manager) runPromptCallV2(ctx context.Context, job *jobState, done chan struct{}, req acpschema.PromptRequest) {
	peer := m.peer(job.ID)
	if peer == nil {
		m.failPromptCall(done, job, fmt.Errorf("acp peer is not active"))
		return
	}
	m.withACPTranscriptBarrier(job.eventView(), nil)
	if _, err := peer.Call(ctx, acpv2.AgentMethodSessionPrompt, req); err != nil {
		if jsonrpc.IsClosed(err) && !errors.Is(err, context.Canceled) {
			err = m.waitServeErr(peer, err)
		}
		m.failPromptCall(done, job, err)
		return
	}
}

// applyStateUpdateV2 applies the v2 foreground state machine: running work no
// turn holds becomes an agent run, and idle ends the running turn or run.
func (m *Manager) applyStateUpdateV2(job *jobState, update acpv2.StateSessionUpdate) {
	event := update.StateUpdateEvent
	job.mu.Lock()
	job.work.reports = true
	job.mu.Unlock()
	finished := m.applyBackgroundV2(job, event)
	switch event.State {
	case "running", "requires_action":
		m.startAgentRun(job, update.RawJSON())
		m.takeIn(job, finished)
	case "idle":
		m.takeIn(job, finished)
		// An idle without a stop reason only reports the background list (a
		// background task ended while no turn ran); it ends no turn.
		if event.StopReason != "" || event.Error != nil {
			m.endWorkV2(job, event)
		}
	case "unknown":
	}
}

// endWorkV2 ends the running turn on the agent's idle report. A failure the
// error stop reason carries becomes the turn's session failure, which keeps
// the shared retry and failure handling.
func (m *Manager) endWorkV2(job *jobState, event acpv2.StateUpdateEvent) {
	job.mu.Lock()
	turn := job.turn
	if turn == nil {
		job.mu.Unlock()
		return
	}
	run := turn.agentRun
	turn.agentRun = false
	done := turn.done
	job.mu.Unlock()
	failure := failureFromStateV2(event)
	if run && failure != nil {
		go m.failPromptCall(done, job, errors.New(failure.message()))
		return
	}
	go m.completePromptCall(done, job, event.StopReason, failure)
}

// failureFromStateV2 reads the failure an idle report carries: the adapter's
// session failure metadata when it sends one (with the retry offers that
// schedule Jaz-side retries, like on v1), else the error object of an error
// stop reason.
func failureFromStateV2(event acpv2.StateUpdateEvent) *sessionFailure {
	if event.Meta != nil {
		raw, err := json.Marshal(event.Meta)
		if err == nil {
			var meta sessionFailureMeta
			if json.Unmarshal(raw, &meta) == nil {
				if failure := meta.terminal(); failure != nil {
					return failure
				}
			}
		}
	}
	if event.StopReason != "error" && event.Error == nil {
		return nil
	}
	failure := &sessionFailure{Title: "agent run failed"}
	if event.Error != nil {
		if message := strings.TrimSpace(event.Error.Message); message != "" {
			failure.Title = message
		}
		if event.Error.Data != nil {
			failure.Details = jsonRPCErrorDataMessage(*event.Error.Data)
		}
	}
	return failure
}

// applyBackgroundV2 replaces the job's background work with the reported
// snapshot, marking items the snapshot dropped as completed, and returns the
// ids of the tasks that finished.
func (m *Manager) applyBackgroundV2(job *jobState, event acpv2.StateUpdateEvent) []string {
	if !event.BackgroundReported {
		return nil
	}
	job.mu.Lock()
	if job.backgroundTasks == nil {
		job.backgroundTasks = map[string]sessionevents.AgentTask{}
	}
	seen := map[string]bool{}
	var changed []sessionevents.AgentTask
	for _, item := range event.Background {
		id := backgroundWorkIDV2(item)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		task := job.backgroundTasks[id]
		fresh := task.ID == ""
		task.ID = id
		task.ConnectionLost = false
		if fresh {
			task.Name = backgroundWorkNameV2Locked(job, item)
			task.Type = item.Type
			task.State = "running"
		}
		task.CanStop = backgroundWorkStoppableV2Locked(job, item)
		if task.State != "running" {
			task.State = "running"
		}
		job.backgroundTasks[id] = task
		if fresh {
			changed = append(changed, task)
		}
	}
	var finished []string
	for id, task := range job.backgroundTasks {
		if seen[id] || !taskLive(task) {
			continue
		}
		task.State = "completed"
		task.CanStop = false
		job.backgroundTasks[id] = task
		finished = append(finished, id)
		changed = append(changed, task)
	}
	job.mu.Unlock()
	for _, task := range changed {
		m.noteBackgroundWork(job, task)
		m.publishBackgroundTask(job, task)
	}
	m.saveBackgroundWork(job)
	return finished
}

func backgroundWorkIDV2(item acpv2.BackgroundWorkItem) string {
	switch item.Type {
	case "terminal":
		return string(item.Terminal)
	case "tool_call":
		return string(item.ToolCall)
	case "session":
		return string(item.Session)
	}
	return ""
}

func backgroundWorkNameV2Locked(job *jobState, item acpv2.BackgroundWorkItem) string {
	switch item.Type {
	case "terminal":
		if terminal := job.terminalsV2[string(item.Terminal)]; terminal != nil {
			return terminal.command
		}
	case "tool_call":
		return job.toolByID[string(item.ToolCall)].Title
	}
	return ""
}

func backgroundWorkStoppableV2Locked(job *jobState, item acpv2.BackgroundWorkItem) bool {
	switch item.Type {
	case "terminal":
		terminal := job.terminalsV2[string(item.Terminal)]
		return terminal != nil && terminal.cancellable && !terminal.exited
	case "tool_call":
		return job.cancellableToolsV2[string(item.ToolCall)]
	}
	return false
}

// stopBackgroundTaskV2 cancels background work through the v2 cancel requests
// of the entity that runs it.
func (m *Manager) stopBackgroundTaskV2(ctx context.Context, job *jobState, task sessionevents.AgentTask) error {
	peer := m.peer(job.ID)
	if peer == nil {
		return fmt.Errorf("agent connection is unavailable")
	}
	switch task.Type {
	case "terminal":
		_, err := peer.Call(ctx, acpv2.AgentMethodTerminalCancel, acpv2.CancelTerminalRequest{
			SessionID:  acpv2.SessionID(job.ACPSession),
			TerminalID: acpv2.TerminalID(task.ID),
		})
		return err
	case "tool_call":
		_, err := peer.Call(ctx, acpv2.AgentMethodToolCallCancel, acpv2.CancelToolCallRequest{
			SessionID:  acpv2.SessionID(job.ACPSession),
			ToolCallID: acpv2.ToolCallID(task.ID),
		})
		return err
	}
	return fmt.Errorf("background task %q of type %q is not stoppable", task.ID, task.Type)
}
