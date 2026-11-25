package acp

import (
	"testing"

	"github.com/wins/jaz/backend/internal/sessionevents"
	"github.com/wins/jaz/backend/internal/storage"
	jsonstore "github.com/wins/jaz/backend/internal/storage/json"
)

// resultRuleJob is a running-turn job the result-rule tests drive with raw
// session updates, the way the adapters send them.
func resultRuleJob(t *testing.T) (*Manager, *jobState) {
	t.Helper()
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(storage.CreateSession{Slug: "result-rule", Runtime: storage.RuntimeACP})
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, Config{}, nil)
	manager.Events = sessionevents.New()
	job := newIdleJob(session, AgentCodex, "acp-session", "", ModeState{})
	job.startTurn(CompletionInline, false, false)
	return manager, job
}

func applyResultUpdate(t *testing.T, manager *Manager, job *jobState, update map[string]any) {
	t.Helper()
	manager.applyJobUpdate(job, mustJSON(t, update))
}

func airPhase(phase string) map[string]any {
	return map[string]any{"jetbrains": map[string]any{"air": map[string]any{"phase": phase}}}
}

func textChunk(id, text string, meta map[string]any) map[string]any {
	update := map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"messageId":     id,
		"content":       map[string]any{"type": "text", "text": text},
	}
	if meta != nil {
		update["_meta"] = meta
	}
	return update
}

// The Codex shape: a partial answer on the way and the turn's final message;
// the result keeps the partial and the final, dropping nothing between them.
func TestTurnResultKeepsPartialAnswersAndTheFinalMessage(t *testing.T) {
	manager, job := resultRuleJob(t)
	applyResultUpdate(t, manager, job, textChunk("item-1", "The sum is 4. ", airPhase("partial_answer")))
	applyResultUpdate(t, manager, job, textChunk("item-2", "4.", airPhase("final_answer")))
	if job.Result != "The sum is 4. 4." {
		t.Fatalf("result = %q", job.Result)
	}
	if job.Assistant != "The sum is 4. 4." {
		t.Fatalf("transcript changed: %q", job.Assistant)
	}
}

// A turn that ends on commentary (a stop, a limit, an error) takes the
// commentary as its last message; the partial answers it wrote on the way
// stay in the result, plain commentary does not.
func TestTurnResultAbortedOnCommentaryKeepsPartials(t *testing.T) {
	manager, job := resultRuleJob(t)
	applyResultUpdate(t, manager, job, textChunk("item-1", "Let me check. ", airPhase("commentary")))
	applyResultUpdate(t, manager, job, textChunk("item-2", "Found it. ", airPhase("partial_answer")))
	applyResultUpdate(t, manager, job, textChunk("item-3", "Running low on time.", airPhase("commentary")))
	if job.Result != "Found it. Running low on time." {
		t.Fatalf("result = %q", job.Result)
	}
}

// Without phase marks (Claude, Kimi, a custom provider) the result is the
// last message alone, however many the turn wrote.
func TestTurnResultWithoutPhaseIsTheLastMessage(t *testing.T) {
	manager, job := resultRuleJob(t)
	applyResultUpdate(t, manager, job, textChunk("item-1", "first ", nil))
	applyResultUpdate(t, manager, job, textChunk("item-2", "second", nil))
	if job.Result != "second" {
		t.Fatalf("result = %q", job.Result)
	}
}

// Two messages in a row, no tool call between: the message id alone splits
// them, and only the last one counts.
func TestTurnResultTwoMessagesBackToBackSplitById(t *testing.T) {
	manager, job := resultRuleJob(t)
	applyResultUpdate(t, manager, job, textChunk("item-1", "draft ", nil))
	applyResultUpdate(t, manager, job, textChunk("item-2", "answer", nil))
	if job.Result != "answer" {
		t.Fatalf("result = %q", job.Result)
	}
}

// Adapters that mark no message id (Kimi on v1) split anonymous messages at
// reply-breaking updates; a tool call ends one message and the next chunks
// are the next message, which alone becomes the result.
func TestTurnResultAnonymousMessagesSplitAtToolCalls(t *testing.T) {
	manager, job := resultRuleJob(t)
	applyResultUpdate(t, manager, job, textChunk("", "earlier ", nil))
	applyResultUpdate(t, manager, job, map[string]any{
		"sessionUpdate": "tool_call", "toolCallId": "tool-1", "title": "work", "status": "completed",
	})
	applyResultUpdate(t, manager, job, textChunk("", "later", nil))
	if job.Result != "later" {
		t.Fatalf("result = %q", job.Result)
	}
	// Without a boundary the anonymous chunks are one message, and it is the
	// last, so the result keeps everything.
	manager2, job2 := resultRuleJob(t)
	applyResultUpdate(t, manager2, job2, textChunk("", "one ", nil))
	applyResultUpdate(t, manager2, job2, textChunk("", "two", nil))
	if job2.Result != "one two" {
		t.Fatalf("result = %q", job2.Result)
	}
}

// Chunks of one message id stream together into a single message however
// they interleave with nothing; the turn result is never empty once a
// message wrote text.
func TestTurnResultChunksOfOneMessageStayTogether(t *testing.T) {
	manager, job := resultRuleJob(t)
	applyResultUpdate(t, manager, job, textChunk("item-1", "a", nil))
	applyResultUpdate(t, manager, job, textChunk("item-1", "b", nil))
	applyResultUpdate(t, manager, job, textChunk("item-1", "c", nil))
	if job.Result != "abc" {
		t.Fatalf("result = %q", job.Result)
	}
}

// A turn with no agent message at all has an empty result; the failure
// records are not agent messages.
func TestTurnResultEmptyTurnHasNoResult(t *testing.T) {
	manager, job := resultRuleJob(t)
	applyResultUpdate(t, manager, job, map[string]any{
		"sessionUpdate": "session_info_update",
		"_meta": map[string]any{"jetbrains": map[string]any{"air": map[string]any{
			"sessionFailure": sessionFailure{ID: "retry-1", Category: "limit", Severity: "warning", Title: "429 Too Many Requests"},
		}}},
	})
	if job.Result != "" {
		t.Fatalf("result = %q", job.Result)
	}
}
