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
