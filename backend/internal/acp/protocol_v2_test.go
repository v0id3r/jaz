package acp_test

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"
	"github.com/wins/jaz/backend/internal/acp"
	"github.com/wins/jaz/backend/internal/sessionevents"
	jsonstore "github.com/wins/jaz/backend/internal/storage/json"
)

func newV2TestManager(t *testing.T, store *jsonstore.Store, extraEnv map[string]string) *acp.Manager {
	t.Helper()
	return acp.NewManager(store, acp.Config{
		Root:         t.TempDir(),
		Workspace:    t.TempDir(),
		SystemPrompt: staticPrompt("skill prompt"),
		Agents: map[string]acp.AgentConfig{
			"fake": {
				ProtocolV2: true,
				Command:    os.Args[0],
				Args:       []string{"-test.run=TestFakeACPAgentV2Process"},
				Env:        fakeV2ManagerConfig(t, extraEnv),
			},
		},
	}, log.New(io.Discard))
}

func spawnV2Session(t *testing.T, manager *acp.Manager, slug string) acp.SpawnResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	spawned, err := manager.Spawn(ctx, acp.SpawnRequest{ACPAgent: "fake", Slug: slug})
	if err != nil {
		t.Fatal(err)
	}
	return spawned
}

func sendV2(t *testing.T, manager *acp.Manager, session, message string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.Send(ctx, acp.SendRequest{Session: session, Message: message, Completion: acp.CompletionInline}); err != nil {
		t.Fatal(err)
	}
}

func waitTurnV2(t *testing.T, manager *acp.Manager, session string) acp.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	job, err := manager.Wait(ctx, acp.WaitRequest{Session: session, Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func waitV2Idle(t *testing.T, manager *acp.Manager, session string) acp.Job {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		job, err := manager.Status(session)
		if err == nil && job.State != acp.StateRunning && job.State != acp.StateStarting {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	job, _ := manager.Status(session)
	t.Fatalf("session %s still %s", session, job.State)
	return job
}

func TestV2SimpleTurn(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-simple")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "say hello")
	job := waitTurnV2(t, manager, spawned.SessionID)
	if job.State != acp.StateIdle || job.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("turn ended %s/%s", job.State, job.StopReason)
	}
	if job.Assistant != "hello world" {
		t.Fatalf("assistant = %q", job.Assistant)
	}
	status, err := manager.Status(spawned.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Modes.PlanModeID != "plan" {
		t.Fatalf("plan mode from mode config option not detected: %#v", status.Modes)
	}
	if status.Model != "fake-large" {
		t.Fatalf("model = %q", status.Model)
	}
}

func TestV2ToolTurn(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-tool")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "run a tool")
	job := waitTurnV2(t, manager, spawned.SessionID)
	if len(job.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", job.ToolCalls)
	}
	call := job.ToolCalls[0]
	if call.ID != "call-1" || call.Title != "Read file" || call.Status != "completed" || call.Kind != "read" {
		t.Fatalf("tool call = %#v", call)
	}
	if job.Assistant != "tool done" {
		t.Fatalf("assistant = %q", job.Assistant)
	}
}

func TestV2SteeringJoinsTurn(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-steer")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "hold the turn")
	waitForV2Job(t, 10*time.Second, func() bool {
		job, err := manager.Status(spawned.SessionID)
		return err == nil && job.State == acp.StateRunning
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.Steer(ctx, acp.SteerRequest{Session: spawned.Slug, Message: "steer-done now"}); err != nil {
		t.Fatal(err)
	}
	job := waitTurnV2(t, manager, spawned.SessionID)
	if job.State != acp.StateIdle {
		t.Fatalf("steered turn ended %s", job.State)
	}
	if !strings.Contains(job.Assistant, "working on it") {
		t.Fatalf("assistant = %q", job.Assistant)
	}
}

func TestV2BackgroundWork(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-background")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "background watch")
	job := waitTurnV2(t, manager, spawned.SessionID)
	if job.State != acp.StateIdle {
		t.Fatalf("turn ended %s", job.State)
	}
	tasks := manager.BackgroundTasksForTest(spawned.SessionID)
	if len(tasks) != 1 || tasks[0].ID != "bg-1" || tasks[0].State != "running" || !tasks[0].CanStop {
		t.Fatalf("background tasks = %#v", tasks)
	}

	sendV2(t, manager, spawned.Slug, "background-finish now")
	waitTurnV2(t, manager, spawned.SessionID)
	waitForV2Job(t, 10*time.Second, func() bool {
		tasks := manager.BackgroundTasksForTest(spawned.SessionID)
		return len(tasks) == 1 && tasks[0].State == "completed"
	})
}

// A terminal linked to a turn's tool call must survive the turn's result
// being discarded: the agent reports the stop after the turn has ended.
func TestV2BackgroundTerminalStopAfterTurn(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-terminal-stop")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "terminal-background watch")
	job := waitTurnV2(t, manager, spawned.SessionID)
	if job.State != acp.StateIdle {
		t.Fatalf("turn ended %s", job.State)
	}
	tasks := manager.BackgroundTasksForTest(spawned.SessionID)
	if len(tasks) != 1 || tasks[0].ID != "term-1" || tasks[0].Type != "terminal" || !tasks[0].CanStop {
		t.Fatalf("background tasks = %#v", tasks)
	}

	if err := manager.StopBackgroundTask(context.Background(), spawned.SessionID, "term-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitForV2Job(t, 10*time.Second, func() bool {
		tasks := manager.BackgroundTasksForTest(spawned.SessionID)
		return len(tasks) == 1 && tasks[0].State == "completed"
	})
}

func TestV2SubagentTranscriptSeparate(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-subagent")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "use a subagent")
	job := waitTurnV2(t, manager, spawned.SessionID)
	if job.Assistant != "subagent done" {
		t.Fatalf("parent assistant = %q", job.Assistant)
	}
	if strings.Contains(job.Assistant, "child findings") || strings.Contains(job.Assistant, "go research") {
		t.Fatalf("child transcript leaked into parent: %q", job.Assistant)
	}
	events, err := store.LoadSessionEvents(spawned.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasACPMessage(events, "subagent done") || hasACPMessage(events, "child findings") {
		t.Fatalf("child activity in parent transcript events")
	}
	transcript := subagentTranscript(events, "fake-v2-child")
	if !strings.Contains(transcript, "child findings") || !strings.Contains(transcript, "go research") {
		t.Fatalf("child transcript = %q", transcript)
	}

	// The journal keeps the transcript across a Jaz restart.
	manager.Close()
	restarted := newV2TestManager(t, store, nil)
	defer restarted.Close()
	events, err = store.LoadSessionEvents(spawned.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got := subagentTranscript(events, "fake-v2-child"); got != transcript {
		t.Fatalf("child transcript after restart = %q, want %q", got, transcript)
	}
}

// subagentTranscript joins the session journal's subagent_message entries of
// one child, the read side of a subagent transcript.
func subagentTranscript(events []sessionevents.Event, childID string) string {
	var lines []string
	for _, event := range events {
		if event.Type == sessionevents.TypeSubagentMessage && event.SubagentMessage != nil && event.SubagentMessage.ID == childID {
			lines = append(lines, event.SubagentMessage.Role+":"+event.SubagentMessage.Content)
		}
	}
	return strings.Join(lines, "\n")
}

// A worker thread shown in its parent's feed publishes its subagent's
// transcript to the parent's journal too, next to the subagent's card.
func TestV2SubagentTranscriptReachesParentVisibleSurface(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	parent := spawnV2Session(t, manager, "v2-parent")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	worker, err := manager.Spawn(ctx, acp.SpawnRequest{ACPAgent: "fake", Slug: "v2-worker", ParentID: parent.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = manager.Cancel(context.Background(), worker.SessionID) }()

	if _, err := manager.Send(ctx, acp.SendRequest{Session: worker.SessionID, Message: "use a subagent", Completion: acp.CompletionInline, ParentVisible: true}); err != nil {
		t.Fatal(err)
	}
	waitTurnV2(t, manager, worker.SessionID)

	for _, session := range []string{worker.SessionID, parent.SessionID} {
		events, err := store.LoadSessionEvents(session)
		if err != nil {
			t.Fatal(err)
		}
		if transcript := subagentTranscript(events, "fake-v2-child"); !strings.Contains(transcript, "child findings") {
			t.Fatalf("session %s transcript = %q", session, transcript)
		}
	}
}

// A child session whose state the agent reports as unknown gives the panel
// no failure: unknown is not a made-up terminal result.
func TestV2SubagentUnknownStateIsNotFailure(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-subagent-unknown")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "subagent-unknown please")
	waitTurnV2(t, manager, spawned.SessionID)
	events, err := store.LoadSessionEvents(spawned.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var statuses []sessionevents.ProviderSubagentStatus
	for _, event := range events {
		if event.Type == sessionevents.TypeProviderSubagent && event.ProviderSubagent != nil {
			statuses = append(statuses, event.ProviderSubagent.Status)
		}
	}
	if len(statuses) == 0 {
		t.Fatal("no subagent panel records")
	}
	for _, status := range statuses {
		if status == sessionevents.SubagentFailed {
			t.Fatalf("unknown child state reported as failed: %v", statuses)
		}
	}
}

func TestV2Cancel(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-cancel")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "hold the turn")
	waitForV2Job(t, 10*time.Second, func() bool {
		job, err := manager.Status(spawned.SessionID)
		return err == nil && job.State == acp.StateRunning
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.Cancel(ctx, spawned.Slug); err != nil {
		t.Fatal(err)
	}
	job := waitV2Idle(t, manager, spawned.SessionID)
	if job.State != acp.StateCancelled {
		t.Fatalf("cancelled turn ended %s (%s)", job.State, job.StopReason)
	}
}

func TestV2FailureEndsTurn(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-fail")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "fail now")
	job := waitTurnV2(t, manager, spawned.SessionID)
	if job.State != acp.StateFailed {
		t.Fatalf("failed turn ended %s", job.State)
	}
	if !strings.Contains(job.Error, "provider exploded") {
		t.Fatalf("error = %q", job.Error)
	}
}

func TestV2SessionFailureRetryHint(t *testing.T) {
	acp.SetProviderRetryDelays(t, time.Millisecond)
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-overload")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	// The failure's retry action continues the turn, and the continuation
	// prompt ("Continue from where you left off.") gets the fake's answer.
	sendV2(t, manager, spawned.Slug, "overload please")
	job := waitTurnV2(t, manager, spawned.SessionID)
	if job.State != acp.StateIdle || job.Assistant != "hello world" {
		t.Fatalf("retried turn ended %s with %q", job.State, job.Assistant)
	}
}

// A provider failure the agent already retried itself (its warning preceded
// the error) must not get a Jaz-side continuation.
func TestV2ProviderRetryWarningSkipsJazRetry(t *testing.T) {
	acp.SetProviderRetryDelays(t, time.Millisecond)
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-overload-warned")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "overload-warned please")
	job := waitTurnV2(t, manager, spawned.SessionID)
	if job.State != acp.StateFailed {
		t.Fatalf("warned turn ended %s with %q", job.State, job.Assistant)
	}
	if !strings.Contains(job.Error, "Overloaded") {
		t.Fatalf("error = %q", job.Error)
	}
}

func TestV2MessagePhaseKeepsCommentaryOutOfResult(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-phased")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "phased reply")
	job := waitTurnV2(t, manager, spawned.SessionID)
	if job.State != acp.StateIdle {
		t.Fatalf("turn ended %s", job.State)
	}
	if job.Result != "final answer" {
		t.Fatalf("result = %q", job.Result)
	}
	if job.Assistant != "checking thingsfinal answer" {
		t.Fatalf("assistant = %q", job.Assistant)
	}
}

// A whole-message upsert after the message's chunks (or after a later
// message started) is a correction, not new content: no duplicates.
func TestV2UpsertAfterChunksDoesNotDuplicate(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-dedup")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "dedup please")
	job := waitTurnV2(t, manager, spawned.SessionID)
	if job.State != acp.StateIdle {
		t.Fatalf("turn ended %s", job.State)
	}
	if job.Assistant != "interim final" {
		t.Fatalf("assistant = %q", job.Assistant)
	}
	if job.Thought != "thinking" {
		t.Fatalf("thought = %q", job.Thought)
	}
	if job.Result != "final" {
		t.Fatalf("result = %q", job.Result)
	}
}

func TestV2ResumeAfterRestart(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-resume")

	sendV2(t, manager, spawned.Slug, "say hello")
	if job := waitTurnV2(t, manager, spawned.SessionID); job.State != acp.StateIdle {
		t.Fatalf("turn ended %s", job.State)
	}
	manager.Close()

	restarted := newV2TestManager(t, store, nil)
	defer func() { _, _ = restarted.Cancel(context.Background(), spawned.SessionID) }()
	sendV2(t, restarted, spawned.Slug, "say hello again")
	job := waitTurnV2(t, restarted, spawned.SessionID)
	if job.State != acp.StateIdle || job.Assistant != "hello world" {
		t.Fatalf("resumed turn: %s %q", job.State, job.Assistant)
	}
	messages, err := store.LoadMessages(spawned.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Two user prompts; a replayed resume would duplicate them.
	if len(messages) != 2 {
		t.Fatalf("stored user messages after resume = %d, want 2", len(messages))
	}
	events, err := store.LoadSessionEvents(spawned.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got := countACPMessage(events, "hello world"); got != 2 {
		t.Fatalf("assistant replies after resume = %d, want 2", got)
	}
}

func TestV2FallbackToV1(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, map[string]string{"JAZ_FAKE_ACP_V2_DECLINE": "1"})
	spawned := spawnV2Session(t, manager, "v2-decline")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	job, err := manager.Status(spawned.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != acp.StateIdle {
		t.Fatalf("fallback session state = %s", job.State)
	}
}
