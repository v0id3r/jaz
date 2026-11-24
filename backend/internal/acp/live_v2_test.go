//go:build acplive && !windows

package acp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"
	jsonstore "github.com/wins/jaz/backend/internal/storage/json"
)

// TestLiveV2Scenarios drives a real adapter (claude or codex) through the
// pilot scenarios, at v2 when ACP_LIVE_V2=1 and at v1 otherwise. It needs
// ACP_LIVE_ROOT with the adapter credentials already placed under
// acp/claude and acp/codex-home, and ACP_LIVE_AGENT_COMMAND pointing at the
// adapter binary.
func TestLiveV2Scenarios(t *testing.T) {
	agent := os.Getenv("ACP_LIVE_AGENT")
	command := os.Getenv("ACP_LIVE_AGENT_COMMAND")
	root := os.Getenv("ACP_LIVE_ROOT")
	if agent == "" || command == "" || root == "" {
		t.Skip("ACP_LIVE_AGENT, ACP_LIVE_AGENT_COMMAND and ACP_LIVE_ROOT are required")
	}
	v2 := os.Getenv("ACP_LIVE_V2") == "1"
	label := agent + "/v1"
	if v2 {
		label = agent + "/v2"
	}
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	manager := NewManager(store, Config{
		Root:         root,
		Workspace:    cwd,
		SystemPrompt: livePrompt("You are a terse test agent. Follow instructions exactly and keep replies under ten words."),
		Agents: map[string]AgentConfig{
			agent: {ProtocolV2: v2, Command: command, Env: liveAgentEnv()},
		},
	}, log.New(os.Stderr))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	spawned, err := manager.Spawn(ctx, SpawnRequest{ACPAgent: agent, Slug: "live-" + label})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Logf("[%s] spawned session %s", label, spawned.SessionID)

	turn := func(name, message string) Job {
		t.Helper()
		start := time.Now()
		if _, err := manager.Send(ctx, SendRequest{Session: spawned.SessionID, Message: message, Completion: CompletionInline}); err != nil {
			t.Fatalf("[%s] %s send: %v", label, name, err)
		}
		job, err := manager.Wait(ctx, WaitRequest{Session: spawned.SessionID, Timeout: 3 * time.Minute})
		if err != nil {
			t.Fatalf("[%s] %s wait: %v", label, name, err)
		}
		t.Logf("[%s] %s: state=%s stop=%s err=%q in %s", label, name, job.State, job.StopReason, job.Error, time.Since(start).Round(time.Second))
		t.Logf("[%s] %s assistant: %s", label, name, truncateRunes(job.Assistant, 300))
		return job
	}

	plain := turn("plain", "Reply with exactly: ok")
	if plain.State != StateIdle || !strings.Contains(strings.ToLower(plain.Assistant), "ok") {
		t.Errorf("[%s] plain turn unexpected: %s %q", label, plain.State, plain.Assistant)
	}

	tool := turn("tool", "Run this exact shell command with your shell tool: echo jaz-live-check. Then reply with exactly: done")
	if tool.State != StateIdle {
		t.Errorf("[%s] tool turn state: %s (%s)", label, tool.State, tool.Error)
	}
	if len(tool.ToolCalls) == 0 {
		t.Errorf("[%s] tool turn reported no tool calls", label)
	} else {
		t.Logf("[%s] tool calls: %s", label, toolCallSummary(tool))
	}

	if os.Getenv("ACP_LIVE_SKIP_STEER") == "" {
		runSteerScenario(t, ctx, manager, spawned.SessionID, label)
	}

	turn("background", "Start this as a background task without waiting for it: sleep 20 && echo jaz-bg-done. Then immediately reply with exactly: started")
	t.Logf("[%s] background tasks after turn: %s", label, backgroundTaskSummary(manager, spawned.SessionID))

	subagent := turn("subagent", "Use your subagent/task tool once: ask a subagent what 2+2 is. Then reply with just the number.")
	t.Logf("[%s] subagent turn state=%s; subagent records: %s", label, subagent.State, providerSubagentSummary(manager, store, spawned.SessionID))

	manager.Close()

	restarted := NewManager(store, Config{
		Root:         root,
		Workspace:    cwd,
		SystemPrompt: livePrompt("You are a terse test agent. Follow instructions exactly and keep replies under ten words."),
		Agents: map[string]AgentConfig{
			agent: {ProtocolV2: v2, Command: command, Env: liveAgentEnv()},
		},
	}, log.New(os.Stderr))
	defer restarted.Close()
	if _, err := restarted.Send(ctx, SendRequest{Session: spawned.SessionID, Message: "Earlier I asked you to run a shell command. What word did I ask you to reply with? One word.", Completion: CompletionInline}); err != nil {
		t.Fatalf("[%s] resume send: %v", label, err)
	}
	resumed, err := restarted.Wait(ctx, WaitRequest{Session: spawned.SessionID, Timeout: 3 * time.Minute})
	if err != nil {
		t.Fatalf("[%s] resume wait: %v", label, err)
	}
	t.Logf("[%s] resume: state=%s assistant=%s", label, resumed.State, truncateRunes(resumed.Assistant, 300))
	if resumed.State != StateIdle {
		t.Errorf("[%s] resumed turn state: %s (%s)", label, resumed.State, resumed.Error)
	}
	if !strings.Contains(strings.ToLower(resumed.Assistant), "done") {
		t.Logf("[%s] resume answer did not recall 'done': %q", label, resumed.Assistant)
	}
}

func runSteerScenario(t *testing.T, ctx context.Context, manager *Manager, sessionID, label string) {
	if _, err := manager.Send(ctx, SendRequest{Session: sessionID, Message: "Run this exact shell command with your shell tool: sleep 20. When it finishes, reply with exactly: unstopped", Completion: CompletionInline}); err != nil {
		t.Fatalf("[%s] steer setup: %v", label, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if job, err := manager.Status(sessionID); err == nil && job.State == StateRunning {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	steerCtx, steerCancel := context.WithTimeout(ctx, 30*time.Second)
	_, steerErr := manager.Steer(steerCtx, SteerRequest{Session: sessionID, Message: "Skip the rest and reply with exactly: steered"})
	steerCancel()
	if steerErr != nil {
		t.Logf("[%s] steer: unsupported or refused: %v", label, steerErr)
		return
	}
	steered, err := manager.Wait(ctx, WaitRequest{Session: sessionID, Timeout: 3 * time.Minute})
	if err != nil {
		t.Fatalf("[%s] steer wait: %v", label, err)
	}
	t.Logf("[%s] steer result: state=%s assistant=%s", label, steered.State, truncateRunes(steered.Assistant, 300))
	if steered.State != StateIdle {
		t.Errorf("[%s] steered turn state: %s", label, steered.State)
	}
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func toolCallSummary(job Job) string {
	parts := make([]string, 0, len(job.ToolCalls))
	for _, call := range job.ToolCalls {
		parts = append(parts, fmt.Sprintf("%s[%s/%s]", call.Title, call.Kind, call.Status))
	}
	return strings.Join(parts, ", ")
}

func backgroundTaskSummary(manager *Manager, session string) string {
	job, err := manager.job(session)
	if err != nil {
		return "no job"
	}
	job.mu.RLock()
	defer job.mu.RUnlock()
	parts := make([]string, 0, len(job.backgroundTasks))
	for _, task := range job.backgroundTasks {
		parts = append(parts, fmt.Sprintf("%s[%s/%s stop=%v]", task.ID, task.Type, task.State, task.CanStop))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func providerSubagentSummary(manager *Manager, store *jsonstore.Store, session string) string {
	events, err := store.LoadSessionOverviewEvents(session)
	if err != nil {
		return "events error: " + err.Error()
	}
	parts := []string{}
	for _, event := range events {
		if event.ProviderSubagent != nil {
			subagent := event.ProviderSubagent
			parts = append(parts, fmt.Sprintf("%s[%s/%s]", subagent.ID, subagent.Name, subagent.Status))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

type livePrompt string

func (p livePrompt) ACPPromptForContext(context.Context, string, string) (string, error) {
	return string(p), nil
}

// liveAgentEnv forwards adapter runtime pointers such as CODEX_PATH into the
// agent's scrubbed environment.
func liveAgentEnv() map[string]string {
	env := map[string]string{}
	for _, key := range []string{"CODEX_PATH"} {
		if value := os.Getenv(key); value != "" {
			env[key] = value
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}
