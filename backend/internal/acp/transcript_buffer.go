package acp

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wins/jaz/backend/internal/sessionevents"
)

const acpTranscriptFlushInterval = 100 * time.Millisecond

// acpTurnlessRunIdleSplit bounds how long a turn-less text run stays open:
// chunks of turns no prompt owns (the agent's internal turns, e.g.
// background-task notifications) share one transcript message until the
// stream goes quiet for this long or the event type changes.
const acpTurnlessRunIdleSplit = 30 * time.Second

type acpTranscriptRun struct {
	eventType         string
	chunks            []string
	upstreamMessageID string
	// engineTurnKey is the agent-reported engine turn id (session/update
	// `_meta.turnId` extension); when present it keys the run directly and
	// replaces both the host turn key and the turn-less heuristic.
	engineTurnKey string
	textRunID     string
}

type acpTranscriptBuffers struct {
	mu        sync.Mutex
	bySession map[string]*acpTranscriptBuffer
}

type acpTranscriptBuffer struct {
	sessionID string
	mu        sync.Mutex
	publishMu sync.Mutex
	timer     *time.Timer
	runs      []acpTranscriptRun

	turnKey        string
	runSeq         int
	currentRunType string
	currentRunID   string

	turnlessSeq         int
	turnlessRunType     string
	turnlessRunID       string
	turnlessLastChunkAt time.Time
}

func (m *Manager) queueACPMessage(job *jobState, content string) {
	m.queueACPMessageWithID(job, content, "")
}

func (m *Manager) queueACPMessageWithID(job *jobState, content string, upstreamMessageID string) {
	m.queueACPTranscript(job, acpTranscriptRun{eventType: "acp_message", chunks: []string{content}, upstreamMessageID: upstreamMessageID})
}

func (m *Manager) queueACPThought(job *jobState, content string) {
	m.queueACPThoughtWithID(job, content, "")
}

func (m *Manager) queueACPThoughtWithID(job *jobState, content string, upstreamMessageID string) {
	m.queueACPTranscript(job, acpTranscriptRun{eventType: "acp_thought", chunks: []string{content}, upstreamMessageID: upstreamMessageID})
}

func (m *Manager) queueACPTranscript(job *jobState, run acpTranscriptRun) {
	if len(run.chunks) == 0 || run.chunks[0] == "" {
		return
	}
	if job == nil {
		return
	}
	job.mu.RLock()
	sessionID := job.ID
	turnKey := transcriptTurnKey(job.turn)
	job.mu.RUnlock()
	if sessionID == "" {
		return
	}
	buffer := m.transcriptBuffers.get(sessionID, true)
	buffer.queue(m, run, turnKey)
}

func (m *Manager) withACPTranscriptBarrier(job eventView, publish func()) {
	if job.ID == "" {
		if publish != nil {
			publish()
		}
		return
	}
	buffer := m.transcriptBuffers.get(job.ID, false)
	if buffer == nil {
		if publish != nil {
			publish()
		}
		return
	}
	buffer.withBarrier(m, job, publish)
}

func (m *Manager) flushACPTranscriptBuffer(buffer *acpTranscriptBuffer) {
	if buffer == nil {
		return
	}
	buffer.flushTimer(m)
}

func (b *acpTranscriptBuffers) get(sessionID string, create bool) *acpTranscriptBuffer {
	if sessionID == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bySession == nil {
		if !create {
			return nil
		}
		b.bySession = map[string]*acpTranscriptBuffer{}
	}
	buffer := b.bySession[sessionID]
	if buffer == nil && create {
		buffer = &acpTranscriptBuffer{sessionID: sessionID}
		b.bySession[sessionID] = buffer
	}
	return buffer
}

func (b *acpTranscriptBuffers) delete(sessionID string) {
	if sessionID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bySession != nil {
		delete(b.bySession, sessionID)
	}
}

func (b *acpTranscriptBuffer) queue(m *Manager, run acpTranscriptRun, turnKey string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	run.textRunID = b.textRunIDLocked(run, turnKey, time.Now())
	if n := len(b.runs); n > 0 && run.textRunID != "" && b.runs[n-1].eventType == run.eventType && b.runs[n-1].textRunID == run.textRunID {
		b.runs[n-1].chunks = append(b.runs[n-1].chunks, run.chunks...)
	} else {
		b.runs = append(b.runs, run)
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(acpTranscriptFlushInterval, func() {
			m.flushACPTranscriptBuffer(b)
		})
	}
}

func textRunID(upstreamMessageID string) string {
	if upstreamMessageID != "" {
		return "message:" + upstreamMessageID
	}
	return ""
}

func transcriptTurnKey(turn *activeTurn) string {
	if turn == nil {
		return ""
	}
	return strconv.FormatInt(turn.startedAt.UnixNano(), 10)
}

func (b *acpTranscriptBuffer) textRunIDLocked(run acpTranscriptRun, turnKey string, now time.Time) string {
	if run.upstreamMessageID != "" {
		b.currentRunType = ""
		b.currentRunID = ""
		return textRunID(run.upstreamMessageID)
	}
	if run.engineTurnKey != "" {
		return "engine:" + run.engineTurnKey
	}
	eventType := run.eventType
	if turnKey == "" {
		// Turn-less chunks: without a run id every chunk would flush as its
		// own transcript message. Keep one run open per continuous stream so
		// the projection merges the whole turn into a single message.
		if b.turnlessRunID == "" || b.turnlessRunType != eventType || now.Sub(b.turnlessLastChunkAt) > acpTurnlessRunIdleSplit {
			b.turnlessSeq++
			b.turnlessRunType = eventType
			b.turnlessRunID = "turnless:" + strconv.Itoa(b.turnlessSeq)
		}
		b.turnlessLastChunkAt = now
		return b.turnlessRunID
	}
	if b.turnKey != turnKey {
		b.turnKey = turnKey
		b.runSeq = 0
		b.currentRunType = ""
		b.currentRunID = ""
	}
	if b.currentRunID != "" && b.currentRunType == eventType {
		return b.currentRunID
	}
	b.runSeq++
	b.currentRunType = eventType
	b.currentRunID = "turn:" + turnKey + ":" + strconv.Itoa(b.runSeq)
	return b.currentRunID
}

func (b *acpTranscriptBuffer) withBarrier(m *Manager, job eventView, publish func()) {
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	runs := b.drainBarrier()
	m.publishACPTranscriptRuns(job, runs)
	if publish != nil {
		publish()
	}
}

func (b *acpTranscriptBuffer) flushTimer(m *Manager) {
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	runs := b.drain()
	if len(runs) == 0 {
		return
	}
	var job eventView
	if live := m.jobByID(b.sessionID); live != nil {
		job = live.eventView()
	}
	m.publishACPTranscriptRuns(job, runs)
}

func (b *acpTranscriptBuffer) drain() []acpTranscriptRun {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.drainLocked()
}

func (b *acpTranscriptBuffer) drainBarrier() []acpTranscriptRun {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.currentRunType = ""
	b.currentRunID = ""
	return b.drainLocked()
}

func (b *acpTranscriptBuffer) drainLocked() []acpTranscriptRun {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	runs := append([]acpTranscriptRun(nil), b.runs...)
	b.runs = nil
	return runs
}

func (m *Manager) publishACPTranscriptRuns(job eventView, runs []acpTranscriptRun) {
	if job.ID == "" || len(runs) == 0 {
		return
	}
	for _, sessionID := range childSessionIDs(job) {
		events := make([]sessionevents.Event, 0, len(runs))
		envelope := acpEventEnvelope(job)
		for _, run := range runs {
			content := strings.Join(run.chunks, "")
			acp := *envelope
			event := sessionevents.Event{
				SessionID: sessionID,
				Type:      run.eventType,
				ACP:       &acp,
				At:        time.Now().UTC(),
			}
			if run.eventType == "acp_thought" {
				event.ACP.Thought = content
			} else {
				event.Content = content
			}
			event.ACP.TextRunID = run.textRunID
			events = append(events, event)
		}
		m.recordAndPublishEventsDirect(sessionID, events)
	}
}
