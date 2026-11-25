package acp

import (
	"testing"
	"time"

	"github.com/wins/jaz/backend/internal/sessionevents"
)

// SetProviderRetryDelays shortens the waits before a failed turn's
// continuations for one test.
func SetProviderRetryDelays(t testing.TB, delays ...time.Duration) {
	previous := providerRetryDelays
	providerRetryDelays = delays
	t.Cleanup(func() { providerRetryDelays = previous })
}

// BackgroundTasksForTest exposes the job's background task records.
func (m *Manager) BackgroundTasksForTest(ref string) []sessionevents.AgentTask {
	job, err := m.job(ref)
	if err != nil {
		return nil
	}
	job.mu.RLock()
	defer job.mu.RUnlock()
	out := make([]sessionevents.AgentTask, 0, len(job.backgroundTasks))
	for _, task := range job.backgroundTasks {
		out = append(out, task)
	}
	return out
}

// TerminalExitForTest exposes a terminal's exit as the agent last reported
// it; live tests use it to wait out the terminal_update that confirms a
// cancel, since the cancel itself leaves the tool call's status alone.
func (m *Manager) TerminalExitForTest(ref, terminalID string) (exited bool, exitCode *int64, signal string) {
	job, err := m.job(ref)
	if err != nil {
		return false, nil, ""
	}
	job.mu.RLock()
	defer job.mu.RUnlock()
	terminal := job.terminalsV2[terminalID]
	if terminal == nil {
		return false, nil, ""
	}
	return terminal.exited, terminal.exitCode, terminal.signal
}
