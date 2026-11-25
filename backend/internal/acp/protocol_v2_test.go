package acp_test

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"
	"github.com/wins/jaz/backend/internal/acp"
	"github.com/wins/jaz/backend/internal/jaztools"
	mcpruntime "github.com/wins/jaz/backend/internal/mcp"
	mcpconfig "github.com/wins/jaz/backend/internal/mcpconfig"
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

// A v2 session's config options reach the agent_session event: the real
// adapters mix a boolean fast-mode option into the select ones, and its JSON
// boolean must not blank the whole list.
func TestV2SessionExposesConfigOptions(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	defer manager.Close()
	spawned := spawnV2Session(t, manager, "v2-options")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	status, err := manager.Status(spawned.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Model != "fake-small" {
		t.Errorf("model = %q", status.Model)
	}
	if status.Modes.PlanModeID != "plan" {
		t.Errorf("plan mode from the mode config option not detected: %#v", status.Modes)
	}
	var session *sessionevents.AgentSession
	events, err := store.LoadSessionEvents(spawned.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.AgentSession != nil {
			state := *event.AgentSession
			session = &state
		}
	}
	if session == nil {
		t.Fatal("no agent_session event")
	}
	categories := map[string]string{}
	for _, option := range session.ConfigOptions {
		categories[option.Category] = option.CurrentValue
	}
	if categories["model"] != "fake-small" {
		t.Errorf("agent_session model option = %q, options: %#v", categories["model"], session.ConfigOptions)
	}
	if categories["mode"] != "default" {
		t.Errorf("agent_session mode option = %q, options: %#v", categories["mode"], session.ConfigOptions)
	}
}

// A v2 Codex session takes its permission baseline through the separate mode
// config option, at spawn and again before each turn, while plan rides the
// collaboration_mode option.
func TestV2CodexBaselineMode(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := acp.NewManager(store, acp.Config{
		Root:         t.TempDir(),
		Workspace:    t.TempDir(),
		SystemPrompt: staticPrompt("skill prompt"),
		Agents: map[string]acp.AgentConfig{
			"codex": {
				ProtocolV2: true,
				Command:    os.Args[0],
				Args:       []string{"-test.run=TestFakeACPAgentV2Process"},
				Env:        fakeV2ManagerConfig(t, map[string]string{"JAZ_FAKE_ACP_V2_CODEX_MODES": "1"}),
			},
		},
	}, log.New(io.Discard))
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	spawned, err := manager.Spawn(ctx, acp.SpawnRequest{ACPAgent: "codex", Slug: "v2-codex-modes"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	modeValues := func() map[string]string {
		t.Helper()
		events, err := store.LoadSessionEvents(spawned.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		values := map[string]string{}
		for _, event := range events {
			if event.AgentSession == nil {
				continue
			}
			for _, option := range event.AgentSession.ConfigOptions {
				values[option.ID] = option.CurrentValue
			}
		}
		return values
	}
	if values := modeValues(); values["mode"] != "agent-full-access" {
		t.Fatalf("mode after spawn = %q, want agent-full-access (%v)", values["mode"], values)
	}

	if _, err := manager.Send(ctx, acp.SendRequest{Session: spawned.SessionID, Message: "make a plan", Completion: acp.CompletionInline, PlanRequested: true}); err != nil {
		t.Fatal(err)
	}
	job, err := manager.Wait(ctx, acp.WaitRequest{Session: spawned.SessionID, Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != acp.StateIdle {
		t.Fatalf("plan turn ended %s (%s)", job.State, job.Error)
	}
	if values := modeValues(); values["mode"] != "agent-full-access" || values["collaboration_mode"] != "plan" {
		t.Fatalf("after the plan turn: %v", values)
	}

	if _, err := manager.Send(ctx, acp.SendRequest{Session: spawned.SessionID, Message: "approved", Completion: acp.CompletionInline}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Wait(ctx, acp.WaitRequest{Session: spawned.SessionID, Timeout: 15 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if values := modeValues(); values["mode"] != "agent-full-access" || values["collaboration_mode"] != "default" {
		t.Fatalf("after the ordinary turn: %v", values)
	}
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
	if status.Model != "fake-small" {
		t.Fatalf("model = %q", status.Model)
	}
}

// A v2 agent that advertises HTTP MCP support gets the session's HTTP MCP
// servers: the v2 initialize capabilities read as the v1 shape.
func TestV2SessionGetsHTTPMCPServers(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := acp.NewManager(store, acp.Config{
		Root:      t.TempDir(),
		Workspace: t.TempDir(),
		MCPStore: staticMCPStore{servers: []mcpconfig.Server{
			mcpruntime.ProxyServerConfig("http://127.0.0.1:5299/mcp/proxy"),
			jaztools.ServerConfig("http://127.0.0.1:5299/mcp/jaztools"),
		}},
		Agents: map[string]acp.AgentConfig{
			"fake": {
				ProtocolV2: true,
				Command:    os.Args[0],
				Args:       []string{"-test.run=TestFakeACPAgentV2Process"},
				Env: map[string]string{
					"JAZ_FAKE_ACP_V2_AGENT":   "1",
					"JAZ_FAKE_ACP_EXPECT_MCP": "1",
				},
			},
		},
	}, log.New(io.Discard))
	defer manager.Close()
	spawned := spawnV2Session(t, manager, "v2-mcp")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()
}

// A configured model is set through session/set_config_option, whose v2
// value carries its type tag; an untagged value is invalid params.
func TestV2ConfiguredModelSetsConfigOption(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := acp.NewManager(store, acp.Config{
		Root:         t.TempDir(),
		Workspace:    t.TempDir(),
		SystemPrompt: staticPrompt("skill prompt"),
		Agents: map[string]acp.AgentConfig{
			// The claude policy sends the model through session/set_config_option.
			"claude": {
				ProtocolV2: true,
				Model:      "fake-large",
				Command:    os.Args[0],
				Args:       []string{"-test.run=TestFakeACPAgentV2Process"},
				Env:        fakeV2ManagerConfig(t, nil),
			},
		},
	}, log.New(io.Discard))
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	spawned, err := manager.Spawn(ctx, acp.SpawnRequest{ACPAgent: "claude", Slug: "v2-model"})
	if err != nil {
		t.Fatal(err)
	}
	// The strict fake already refused an untagged value at spawn; the
	// mid-session path goes through the same request.
	if err := manager.SetSessionConfig(ctx, spawned.SessionID, "model", "fake-large"); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(spawned.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Model != "fake-large" {
		t.Fatalf("model = %q", status.Model)
	}
}

// A session-start json-rpc failure keeps the agent's error data, so the
// recorded reason says what was rejected, not only "Invalid params".
func TestV2SessionStartErrorKeepsRPCData(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := acp.NewManager(store, acp.Config{
		Root:         t.TempDir(),
		Workspace:    t.TempDir(),
		SystemPrompt: staticPrompt("skill prompt"),
		Agents: map[string]acp.AgentConfig{
			"claude": {
				ProtocolV2: true,
				Model:      "fake-rejected",
				Command:    os.Args[0],
				Args:       []string{"-test.run=TestFakeACPAgentV2Process"},
				Env:        fakeV2ManagerConfig(t, nil),
			},
		},
	}, log.New(io.Discard))
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = manager.Spawn(ctx, acp.SpawnRequest{ACPAgent: "claude", Slug: "v2-rejected"})
	if err == nil {
		t.Fatal("spawn with a rejected model succeeded")
	}
	if !strings.Contains(err.Error(), "model is not available on this account") {
		t.Fatalf("spawn error without the rpc data: %v", err)
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

// A prompt that reaches the agent while a cycle it started on its own runs
// waits in its queue: that cycle's idle ends no turn of the client's, and
// the prompt's own idle does.
func TestV2QueuedPromptOutlivesForeignIdle(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-queued")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "queued-race")
	job := waitTurnV2(t, manager, spawned.SessionID)
	if job.State != acp.StateIdle || job.StopReason != "end_turn" {
		t.Fatalf("state=%s stop=%s err=%s", job.State, job.StopReason, job.Error)
	}
	if !strings.Contains(job.Assistant, "queued answer") {
		t.Fatalf("the foreign cycle's idle ended the turn early: assistant = %q", job.Assistant)
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
	// The published record the Overview reads carries the name and the stop
	// offer the tool call brought after the task's first report.
	waitForV2Job(t, 10*time.Second, func() bool {
		events, err := store.LoadSessionEvents(spawned.SessionID)
		if err != nil {
			return false
		}
		return slices.ContainsFunc(events, func(event sessionevents.Event) bool {
			task := event.AgentTask
			return task != nil && task.ID == "bg-1" && task.Name == "Watch build" && task.CanStop
		})
	})

	sendV2(t, manager, spawned.Slug, "background-finish now")
	waitTurnV2(t, manager, spawned.SessionID)
	waitForV2Job(t, 10*time.Second, func() bool {
		tasks := manager.BackgroundTasksForTest(spawned.SessionID)
		return len(tasks) == 1 && tasks[0].State == "completed"
	})
}

// A background tool call stopped from the Overview ends stopped, not
// completed: the cancel went through the client.
func TestV2BackgroundToolCallStop(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-background-stop")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "background watch")
	waitTurnV2(t, manager, spawned.SessionID)
	tasks := manager.BackgroundTasksForTest(spawned.SessionID)
	if len(tasks) != 1 || tasks[0].ID != "bg-1" || !tasks[0].CanStop {
		t.Fatalf("background tasks = %#v", tasks)
	}
	if err := manager.StopBackgroundTask(context.Background(), spawned.SessionID, "bg-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitForV2Job(t, 10*time.Second, func() bool {
		tasks := manager.BackgroundTasksForTest(spawned.SessionID)
		return len(tasks) == 1 && tasks[0].State == "stopped"
	})
}

// A background tool call cancelled from its card mid-turn ends stopped too.
func TestV2BackgroundToolCallCancel(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-background-cancel")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "open-background watch")
	waitForV2Job(t, 10*time.Second, func() bool {
		job, err := manager.Status(spawned.SessionID)
		return err == nil && job.State == acp.StateRunning &&
			slices.ContainsFunc(job.ToolCalls, func(call sessionevents.ACPToolCall) bool {
				return call.ID == "bg-1" && call.Cancellable
			})
	})
	if err := manager.CancelToolCall(context.Background(), spawned.SessionID, "bg-1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitTurnV2(t, manager, spawned.SessionID)
	waitForV2Job(t, 10*time.Second, func() bool {
		tasks := manager.BackgroundTasksForTest(spawned.SessionID)
		return len(tasks) == 1 && tasks[0].State == "stopped"
	})
}

// A background terminal that outlives its turn keeps its state: the stop
// offer survives the next turn's start, and the exit the agent reports in a
// later turn decides the task's end state.
func TestV2BackgroundTerminalSurvivesTurn(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	defer manager.Close()
	spawned := spawnV2Session(t, manager, "v2-terminal-survives")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "terminal-background watch")
	waitTurnV2(t, manager, spawned.SessionID)
	tasks := manager.BackgroundTasksForTest(spawned.SessionID)
	if len(tasks) != 1 || tasks[0].ID != "term-1" || !tasks[0].CanStop {
		t.Fatalf("background tasks = %#v", tasks)
	}

	sendV2(t, manager, spawned.Slug, "keep-background")
	waitTurnV2(t, manager, spawned.SessionID)
	waitForV2Job(t, 10*time.Second, func() bool {
		tasks := manager.BackgroundTasksForTest(spawned.SessionID)
		return len(tasks) == 1 && tasks[0].State == "running" && tasks[0].CanStop
	})
}

// A background terminal's end state is the one the agent reported: its exit
// code when it exited, and a neutral ended when it left the list without a
// reported end.
func TestV2BackgroundTerminalOutcomes(t *testing.T) {
	for _, test := range []struct {
		name, prompt, want string
	}{
		{"exit zero", "terminal-exit-zero", "completed"},
		{"exit nonzero", "terminal-exit-nonzero", "failed"},
		{"codex stop", "terminal-codex-stop", "stopped"},
		{"no reported end", "terminal-vanish", "ended"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := jsonstore.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager := newV2TestManager(t, store, nil)
			defer manager.Close()
			spawned := spawnV2Session(t, manager, "v2-terminal-outcome")
			defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

			sendV2(t, manager, spawned.Slug, "terminal-background watch")
			waitTurnV2(t, manager, spawned.SessionID)

			sendV2(t, manager, spawned.Slug, test.prompt)
			waitTurnV2(t, manager, spawned.SessionID)
			waitForV2Job(t, 10*time.Second, func() bool {
				tasks := manager.BackgroundTasksForTest(spawned.SessionID)
				return len(tasks) == 1 && tasks[0].State == test.want
			})
			tasks := manager.BackgroundTasksForTest(spawned.SessionID)
			if len(tasks) != 1 || tasks[0].State != test.want {
				t.Fatalf("background tasks = %#v, want state %s", tasks, test.want)
			}
		})
	}
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
	// The client asked for the stop, so the task ends stopped, not completed.
	waitForV2Job(t, 10*time.Second, func() bool {
		tasks := manager.BackgroundTasksForTest(spawned.SessionID)
		return len(tasks) == 1 && tasks[0].State == "stopped"
	})
}

// A v2 agent that offers per-call cancellation gets a tool_call/cancel for
// the one call, and the turn the call held ends without a session cancel.
func TestV2ToolCallCancel(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-tool-cancel")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "cancellable-tool please")
	waitForV2Job(t, 10*time.Second, func() bool {
		job, err := manager.Status(spawned.SessionID)
		return err == nil && len(job.ToolCalls) == 1 && job.ToolCalls[0].ID == "call-c" && job.ToolCalls[0].Cancellable
	})
	if err := manager.CancelToolCall(context.Background(), spawned.SessionID, "nope"); err == nil {
		t.Fatal("cancelling an unknown tool call succeeded")
	}
	if err := manager.CancelToolCall(context.Background(), spawned.SessionID, "call-c"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	job := waitTurnV2(t, manager, spawned.SessionID)
	if job.State != acp.StateIdle || !strings.Contains(job.Assistant, "tool cancelled") {
		t.Fatalf("turn ended %s with %q", job.State, job.Assistant)
	}
	if job.ToolCalls[0].Status != "cancelled" {
		t.Fatalf("cancelled tool call = %#v", job.ToolCalls[0])
	}
}

// A tool call running in a cancellable terminal cancels through
// terminal/cancel, the request of the entity that runs it.
func TestV2ToolCallCancelUsesTerminal(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-tool-cancel-terminal")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "terminal-foreground watch")
	// The cancel capability arrives only on the terminal's update, as the
	// real adapter sends it, and still marks the call's card cancellable.
	waitForV2Job(t, 10*time.Second, func() bool {
		job, err := manager.Status(spawned.SessionID)
		return err == nil && len(job.ToolCalls) == 1 && job.ToolCalls[0].ID == "call-t" &&
			job.ToolCalls[0].Runtime.TerminalID == "term-2" && job.ToolCalls[0].Cancellable
	})
	if err := manager.CancelToolCall(context.Background(), spawned.SessionID, "call-t"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	job := waitTurnV2(t, manager, spawned.SessionID)
	if job.State != acp.StateIdle {
		t.Fatalf("turn ended %s", job.State)
	}
	// The agent reported the killed command's end without an exit code: the
	// card must not invent exit 0. The exit update precedes the turn's idle,
	// so the last projection of the call already carries the end.
	waitForV2Job(t, 10*time.Second, func() bool {
		events, err := store.LoadSessionEvents(spawned.SessionID)
		if err != nil {
			return false
		}
		var last *sessionevents.ACPToolCall
		for _, event := range events {
			if event.ACP == nil {
				continue
			}
			for _, call := range event.ACP.ToolCalls {
				if call.ID == "call-t" {
					last = &call
				}
			}
		}
		return last != nil && last.Runtime.TerminalExitCode == nil
	})
}

// The terminal may advertise its cancel after the tool call already linked
// it; the call's card becomes cancellable then.
func TestV2ToolCallCancelLateTerminalCapability(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-tool-cancel-late")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "terminal-late watch")
	waitForV2Job(t, 10*time.Second, func() bool {
		job, err := manager.Status(spawned.SessionID)
		return err == nil && len(job.ToolCalls) == 1 && job.ToolCalls[0].ID == "call-l" && job.ToolCalls[0].Cancellable
	})
	if err := manager.CancelToolCall(context.Background(), spawned.SessionID, "call-l"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if job := waitTurnV2(t, manager, spawned.SessionID); job.State != acp.StateIdle {
		t.Fatalf("turn ended %s", job.State)
	}
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

// v2 terminal output is base64 on the wire, each chunk independently; the
// job keeps decoded text, a split UTF-8 character reassembles, and an
// invalid chunk is dropped without failing the turn.
func TestV2TerminalOutputDecodesBase64(t *testing.T) {
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newV2TestManager(t, store, nil)
	spawned := spawnV2Session(t, manager, "v2-terminal-output")
	defer func() { _, _ = manager.Cancel(context.Background(), spawned.SessionID) }()

	sendV2(t, manager, spawned.Slug, "terminal-output please")
	waitTurnV2(t, manager, spawned.SessionID)
	// Chunk publishes are coalesced on a short timer; the stored tool call
	// carries the full output once it lands.
	var output string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, err := store.LoadSessionEvents(spawned.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.ACP == nil {
				continue
			}
			for _, call := range event.ACP.ToolCalls {
				if call.ID == "call-term-out" {
					output = call.Runtime.TerminalOutput
				}
			}
		}
		if output == "out: café" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("terminal output = %q", output)
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
