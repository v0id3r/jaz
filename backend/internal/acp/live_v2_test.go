//go:build acplive && !windows

package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/log"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wins/jaz/backend/internal/mcpconfig"
	"github.com/wins/jaz/backend/internal/sessionevents"
	jsonstore "github.com/wins/jaz/backend/internal/storage/json"
)

// liveMCPStore is a mutable MCP server list for the live probe.
type liveMCPStore struct {
	mu      sync.Mutex
	servers []mcpconfig.Server
}

func (s *liveMCPStore) ListMCPServers() ([]mcpconfig.Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mcpconfig.Server(nil), s.servers...), nil
}

func (s *liveMCPStore) add(server mcpconfig.Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.servers = append(s.servers, server)
}

// liveMCPToolServer serves one echo tool over streamable HTTP, the shape of
// the MCP servers Jaz hands to agents.
func liveMCPToolServer(t *testing.T, tool string) string {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: tool, Version: "test"}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        tool,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "mcp-echo:" + args.Text}}}, nil
	})
	httpServer := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return server
	}, &mcpsdk.StreamableHTTPOptions{JSONResponse: true}))
	t.Cleanup(httpServer.Close)
	return httpServer.URL
}

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
	mcpStore := &liveMCPStore{}
	mcpStore.add(mcpconfig.Server{ID: "live_echo", Name: "live_echo", Transport: mcpconfig.TransportStreamableHTTP, URL: liveMCPToolServer(t, "live_echo"), Enabled: true})
	manager := NewManager(store, Config{
		Root:         root,
		Workspace:    cwd,
		SystemPrompt: livePrompt("You are a terse test agent. Follow instructions exactly and keep replies under ten words. Your codeword is jaz-owl-77."),
		MCPStore:     mcpStore,
		Agents: map[string]AgentConfig{
			agent: {
				ProtocolV2:      v2,
				Command:         command,
				Env:             liveAgentEnv(),
				Model:           os.Getenv("ACP_LIVE_MODEL"),
				ReasoningEffort: os.Getenv("ACP_LIVE_EFFORT"),
			},
		},
	}, log.New(os.Stderr))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	spawned, err := manager.Spawn(ctx, SpawnRequest{ACPAgent: agent, Slug: "live-" + label})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Logf("[%s] spawned session %s", label, spawned.SessionID)

	// Without config options the baseline mode never lands and the UI loses
	// its model, effort and mode selectors.
	if got := liveConfigOptionValue(manager, spawned.SessionID, "model"); got == "" {
		t.Errorf("[%s] no model config option after spawn", label)
	}
	if got := liveConfigOptionValue(manager, spawned.SessionID, "mode"); got == "" {
		t.Errorf("[%s] no mode config option after spawn", label)
	}
	// The v2 Codex adapter advertises fast mode as a boolean option; setting
	// it must reach the adapter with v2's boolean type tag, which the adapter
	// requires and Jaz's select-only path could not send.
	if v2 && agent == "codex" {
		if got := liveConfigOptionByID(manager, spawned.SessionID, "fast-mode"); got != "off" && got != "on" {
			t.Errorf("[%s] no boolean fast-mode option after spawn: %q, options: %s", label, got, liveConfigOptionsSummary(manager, spawned.SessionID))
		} else if err := manager.SetSessionConfig(ctx, spawned.SessionID, "fast-mode", "on"); err != nil {
			t.Errorf("[%s] set fast-mode on: %v", label, err)
		} else if got := liveConfigOptionByID(manager, spawned.SessionID, "fast-mode"); got != "on" {
			t.Errorf("[%s] fast-mode after set = %q, want on", label, got)
		}
	}

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
		t.Logf("[%s] %s result: %s", label, name, truncateRunes(job.Result, 300))
		return job
	}

	plain := turn("plain", "Reply with exactly: ok")
	if plain.State != StateIdle || !strings.Contains(strings.ToLower(plain.Assistant), "ok") {
		t.Errorf("[%s] plain turn unexpected: %s %q", label, plain.State, plain.Assistant)
	}
	codeword := turn("codeword", "What is your codeword? Reply with exactly the codeword.")
	if !strings.Contains(codeword.Result, "jaz-owl-77") {
		t.Errorf("[%s] system prompt codeword missing from the result: %q", label, truncateRunes(codeword.Result, 200))
	}
	// An adapter-generated title must be meaningful — not the raw prompt and
	// not leaking the system prompt — and Claude asks for it right after the
	// first prompt, so it must arrive within the turns above.
	titleDeadline := time.Now().Add(30 * time.Second)
	title := ""
	for time.Now().Before(titleDeadline) {
		session, err := store.LoadSession(spawned.SessionID)
		if err == nil {
			title = strings.TrimSpace(session.Title)
		}
		if title != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("[%s] session title: %q", label, title)
	if strings.Contains(title, "jaz-owl-77") {
		t.Errorf("[%s] session title leaks the system prompt: %q", label, title)
	}
	if agent == AgentClaude && title == "" {
		t.Errorf("[%s] claude returned no session title", label)
	}
	// The turn's prepareModeForTurn sets the agent's baseline mode through the
	// mode config option; Codex left in its default mode sandboxes every
	// command into bwrap failures.
	if v2 {
		if want := map[string]string{"codex": "agent-full-access", "claude": "bypassPermissions"}[agent]; want != "" {
			if got := liveConfigOptionValue(manager, spawned.SessionID, "mode"); got != want {
				t.Errorf("[%s] mode after the first turn = %q, want %s", label, got, want)
			}
		}
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
	// A sandboxed attempt may fail with a readable error of its own; the
	// check is that some terminal shows the command's output as text.
	terminalSeen := false
	for _, call := range tool.ToolCalls {
		if call.Runtime.TerminalOutput == "" {
			continue
		}
		t.Logf("[%s] terminal output of %s: %q", label, call.ID, truncateRunes(call.Runtime.TerminalOutput, 120))
		if strings.Contains(call.Runtime.TerminalOutput, "jaz-live-check") {
			terminalSeen = true
		}
	}
	if !terminalSeen {
		t.Errorf("[%s] no terminal output with the command's text", label)
	}

	mcpTurn := turn("mcp", "Call the live_echo MCP tool with text hello. Reply with exactly what the tool returned.")
	if !strings.Contains(mcpTurn.Assistant, "mcp-echo:hello") {
		t.Errorf("[%s] mcp turn did not return the tool's echo: %q", label, truncateRunes(mcpTurn.Assistant, 200))
	}

	if os.Getenv("ACP_LIVE_SKIP_STEER") == "" {
		runSteerScenario(t, ctx, manager, spawned.SessionID, label)
	}

	turn("background", "Start this as a background task without waiting for it: sleep 60 && echo jaz-bg-done. Then immediately reply with exactly: started")
	t.Logf("[%s] background tasks after turn: %s", label, backgroundTaskSummary(manager, spawned.SessionID))
	if taskID := stoppableBackgroundTask(manager, spawned.SessionID); taskID != "" {
		if err := manager.StopBackgroundTask(ctx, spawned.SessionID, taskID); err != nil {
			t.Errorf("[%s] stop background task %s: %v", label, taskID, err)
		} else {
			t.Logf("[%s] stopped background task %s: %s", label, taskID, backgroundTaskSummary(manager, spawned.SessionID))
		}
	} else {
		t.Logf("[%s] no stoppable background task reported", label)
	}

	subagent := turn("subagent", "Use your subagent/task tool once: ask a subagent what 2+2 is and wait for its reply. Then reply with just the number the subagent gave you.")
	t.Logf("[%s] subagent turn state=%s; subagent records: %s", label, subagent.State, providerSubagentSummary(manager, store, spawned.SessionID))
	subagentEntries := len(subagentTranscriptEvents(store, spawned.SessionID))
	t.Logf("[%s] subagent transcript entries in the journal: %d", label, subagentEntries)
	if v2 && subagentEntries == 0 {
		t.Errorf("[%s] no subagent_message events in the journal", label)
	}

	manager.Close()
	if after := len(subagentTranscriptEvents(store, spawned.SessionID)); after != subagentEntries {
		t.Errorf("[%s] subagent transcript entries after restart: %d, want %d", label, after, subagentEntries)
	}

	// A server added while Jaz was down must reach the resumed session: v2
	// has no mid-session MCP refresh, so resume is where the new list lands.
	mcpStore.add(mcpconfig.Server{ID: "live_echo_two", Name: "live_echo_two", Transport: mcpconfig.TransportStreamableHTTP, URL: liveMCPToolServer(t, "live_echo_two"), Enabled: true})
	restarted := NewManager(store, Config{
		Root:         root,
		Workspace:    cwd,
		SystemPrompt: livePrompt("You are a terse test agent. Follow instructions exactly and keep replies under ten words. Your codeword is jaz-owl-77."),
		MCPStore:     mcpStore,
		Agents: map[string]AgentConfig{
			agent: {
				ProtocolV2:      v2,
				Command:         command,
				Env:             liveAgentEnv(),
				Model:           os.Getenv("ACP_LIVE_MODEL"),
				ReasoningEffort: os.Getenv("ACP_LIVE_EFFORT"),
			},
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
	if _, err := restarted.Send(ctx, SendRequest{Session: spawned.SessionID, Message: "Call the live_echo_two MCP tool with text again. Reply with exactly what the tool returned.", Completion: CompletionInline}); err != nil {
		t.Fatalf("[%s] resumed mcp send: %v", label, err)
	}
	resumedMCP, err := restarted.Wait(ctx, WaitRequest{Session: spawned.SessionID, Timeout: 3 * time.Minute})
	if err != nil {
		t.Fatalf("[%s] resumed mcp wait: %v", label, err)
	}
	t.Logf("[%s] resumed mcp: state=%s assistant=%s", label, resumedMCP.State, truncateRunes(resumedMCP.Assistant, 200))
	// A cheap model may reuse the first server's argument; the check is that
	// the new server's tool was callable at all.
	resumedMCPSeen := strings.Contains(resumedMCP.Assistant, "mcp-echo:again")
	for _, call := range resumedMCP.ToolCalls {
		if strings.Contains(call.Title, "live_echo_two") {
			resumedMCPSeen = true
		}
	}
	if !resumedMCPSeen {
		t.Errorf("[%s] resumed session did not see the MCP server added while Jaz was down: %q (%s)", label, truncateRunes(resumedMCP.Assistant, 200), toolCallSummary(resumedMCP))
	}
	if resumed.State != StateIdle {
		t.Errorf("[%s] resumed turn state: %s (%s)", label, resumed.State, resumed.Error)
	}
	if !strings.Contains(strings.ToLower(resumed.Assistant), "done") {
		t.Logf("[%s] resume answer did not recall 'done': %q", label, resumed.Assistant)
	}
	// The user's fast-mode pick rides restoreSessionControls into the resumed
	// session, through the same boolean set path.
	if v2 && agent == "codex" {
		if got := liveConfigOptionByID(restarted, spawned.SessionID, "fast-mode"); got != "on" {
			t.Errorf("[%s] fast-mode after resume = %q, want the restored on", label, got)
		}
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

// TestLiveBackgroundOutcomes drives a real v2 adapter through the three
// background-task endings the client must not guess: a command that exits on
// its own with code 0 and with a nonzero code, a task that outlives its turn
// and keeps its stop offer, and a stop from the client. ACP_LIVE_V2=1 and
// ACP_LIVE_AGENT/ACP_LIVE_AGENT_COMMAND/ACP_LIVE_ROOT as in
// TestLiveV2Scenarios.
func TestLiveBackgroundOutcomes(t *testing.T) {
	agent := os.Getenv("ACP_LIVE_AGENT")
	command := os.Getenv("ACP_LIVE_AGENT_COMMAND")
	root := os.Getenv("ACP_LIVE_ROOT")
	if agent == "" || command == "" || root == "" || os.Getenv("ACP_LIVE_V2") != "1" {
		t.Skip("ACP_LIVE_AGENT, ACP_LIVE_AGENT_COMMAND, ACP_LIVE_ROOT and ACP_LIVE_V2=1 are required")
	}
	store, err := jsonstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, Config{
		Root:         root,
		Workspace:    t.TempDir(),
		SystemPrompt: livePrompt("You are a terse test agent. Follow instructions exactly and keep replies under ten words. Your codeword is jaz-owl-77."),
		Agents: map[string]AgentConfig{
			agent: {
				ProtocolV2: true,
				Command:    command,
				Env:        liveAgentEnv(),
				Model:      os.Getenv("ACP_LIVE_MODEL"),
			},
		},
	}, log.New(os.Stderr))
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	spawned, err := manager.Spawn(ctx, SpawnRequest{ACPAgent: agent, Slug: "live-outcomes-" + agent})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	turn := func(name, message string) {
		t.Helper()
		if _, err := manager.Send(ctx, SendRequest{Session: spawned.SessionID, Message: message, Completion: CompletionInline}); err != nil {
			t.Fatalf("[%s] %s send: %v", agent, name, err)
		}
		if _, err := manager.Wait(ctx, WaitRequest{Session: spawned.SessionID, Timeout: 3 * time.Minute}); err != nil {
			t.Fatalf("[%s] %s wait: %v", agent, name, err)
		}
	}
	seen := map[string]bool{}
	waitState := func(name, want string) {
		t.Helper()
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			for _, task := range manager.BackgroundTasksForTest(spawned.SessionID) {
				if seen[task.ID] {
					continue
				}
				if task.State == want {
					seen[task.ID] = true
					return
				}
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Errorf("[%s] %s: tasks = %s, want a new task in state %s", agent, name, backgroundTaskSummary(manager, spawned.SessionID), want)
	}

	// The command must outlive the agent's own yield window, and run as the
	// tool's own session: a shell-detached one (`&`, nohup) is invisible to
	// the adapter. A cheap model needs to be told so.
	background := "Use your shell tool to start this command as a long-running task and do not wait for it; run it exactly as given, without nohup, &, disown or redirections, so it stays the tool's own running session: %s. Then immediately reply with exactly: started"
	turn("exit-zero", fmt.Sprintf(background, "sleep 30 && exit 0"))
	waitState("exit-zero", "completed")

	turn("exit-nonzero", fmt.Sprintf(background, "sleep 30 && exit 7"))
	waitState("exit-nonzero", "failed")

	turn("survivor", fmt.Sprintf(background, "sleep 300"))
	if taskID := stoppableBackgroundTask(manager, spawned.SessionID); taskID == "" {
		t.Fatalf("[%s] survivor: no stoppable background task: %s", agent, backgroundTaskSummary(manager, spawned.SessionID))
	}
	turn("between", "Reply with exactly: ok")
	if taskID := stoppableBackgroundTask(manager, spawned.SessionID); taskID == "" {
		t.Errorf("[%s] survivor lost its stop offer across a turn: %s", agent, backgroundTaskSummary(manager, spawned.SessionID))
	} else if err := manager.StopBackgroundTask(ctx, spawned.SessionID, taskID); err != nil {
		t.Errorf("[%s] survivor stop: %v", agent, err)
	} else {
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			stopped := false
			for _, task := range manager.BackgroundTasksForTest(spawned.SessionID) {
				if task.ID == taskID && task.State == "stopped" {
					stopped = true
				}
			}
			if stopped {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		found := false
		for _, task := range manager.BackgroundTasksForTest(spawned.SessionID) {
			if task.ID == taskID && task.State == "stopped" {
				found = true
			}
		}
		if !found {
			t.Errorf("[%s] survivor stop: tasks = %s, want %s stopped", agent, backgroundTaskSummary(manager, spawned.SessionID), taskID)
		}
	}
	t.Logf("[%s] final tasks: %s", agent, backgroundTaskSummary(manager, spawned.SessionID))
}

// liveConfigOptionValue reads the current value of the session's config
// option of a category, as the agent_session event publishes it.
func liveConfigOptionValue(manager *Manager, session, category string) string {
	return liveConfigOption(manager, session, func(option sessionevents.AgentConfigOption) bool { return option.Category == category })
}

func liveConfigOptionByID(manager *Manager, session, id string) string {
	return liveConfigOption(manager, session, func(option sessionevents.AgentConfigOption) bool { return option.ID == id })
}

func liveConfigOption(manager *Manager, session string, match func(sessionevents.AgentConfigOption) bool) string {
	job, err := manager.job(session)
	if err != nil {
		return ""
	}
	job.mu.RLock()
	defer job.mu.RUnlock()
	for _, option := range job.agentSession.ConfigOptions {
		if match(option) {
			return option.CurrentValue
		}
	}
	return ""
}

func liveConfigOptionsSummary(manager *Manager, session string) string {
	job, err := manager.job(session)
	if err != nil {
		return "no job"
	}
	job.mu.RLock()
	defer job.mu.RUnlock()
	parts := make([]string, 0, len(job.agentSession.ConfigOptions))
	for _, option := range job.agentSession.ConfigOptions {
		parts = append(parts, fmt.Sprintf("%s[%s]=%s", option.ID, option.Type, option.CurrentValue))
	}
	return strings.Join(parts, ", ")
}

func stoppableBackgroundTask(manager *Manager, session string) string {
	job, err := manager.job(session)
	if err != nil {
		return ""
	}
	job.mu.RLock()
	defer job.mu.RUnlock()
	for id, task := range job.backgroundTasks {
		if task.CanStop && task.State == "running" {
			return id
		}
	}
	return ""
}

func subagentTranscriptEvents(store *jsonstore.Store, session string) []string {
	events, err := store.LoadSessionEvents(session)
	if err != nil {
		return nil
	}
	var out []string
	for _, event := range events {
		if event.Type == "subagent_message" && event.SubagentMessage != nil {
			out = append(out, event.SubagentMessage.Role+":"+event.SubagentMessage.Content)
		}
	}
	return out
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
	for _, key := range []string{"CODEX_PATH", "CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		if value := os.Getenv(key); value != "" {
			env[key] = value
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// TestLiveBackgroundStop drives a real adapter through the Overview's
// background-task stop: the task must end stopped, not completed. On v2 the
// agent confirms the end with a terminal_update carrying an exitStatus — the
// cancel itself leaves the referencing tool call's status alone (see
// docs/protocol/v2/tool-calls.mdx, "Cancelling terminals"); on v1 the
// adapter closes the call with the backgrounded result. Codex discovers a
// background terminal only from an exec session that outlives its turn, so
// there the test has the model yield the call (yield_time_ms) instead of
// asking for a shell background job. ACP_LIVE_V2=1 runs it on v2, otherwise
// on v1.
func TestLiveBackgroundStop(t *testing.T) {
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
	manager := NewManager(store, Config{
		Root:         root,
		Workspace:    t.TempDir(),
		SystemPrompt: livePrompt("You are a terse test agent. Follow instructions exactly and keep replies under ten words. Your codeword is jaz-owl-77."),
		Agents: map[string]AgentConfig{
			agent: {
				ProtocolV2: v2,
				Command:    command,
				Env:        liveAgentEnv(),
				Model:      os.Getenv("ACP_LIVE_MODEL"),
			},
		},
	}, log.New(os.Stderr))
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	spawned, err := manager.Spawn(ctx, SpawnRequest{ACPAgent: agent, Slug: "live-stop-" + label})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	prompt := "Start this as a background task without waiting for it: sleep 120 && echo jaz-bg-done. Then immediately reply with exactly: started"
	if v2 && agent == AgentCodex {
		// Codex discovers a background terminal only when an exec session outlives
		// its turn (upstream session-close-background test, PR #777): the call
		// yields while the process keeps running.
		prompt = "Run the shell command sleep 120 with yield_time_ms set to 1000 so the call returns while the command keeps running. Then reply with exactly: started"
	}
	if _, err := manager.Send(ctx, SendRequest{Session: spawned.SessionID, Message: prompt, Completion: CompletionInline}); err != nil {
		t.Fatalf("send: %v", err)
	}
	job, err := manager.Wait(ctx, WaitRequest{Session: spawned.SessionID, Timeout: 3 * time.Minute})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	t.Logf("[%s] turn: state=%s assistant=%q tools=%s", label, job.State, truncateRunes(job.Assistant, 120), toolCallSummary(job))
	var taskID, toolCallID string
	grace := time.Now().Add(10 * time.Second)
	for time.Now().Before(grace) && taskID == "" {
		for _, task := range manager.BackgroundTasksForTest(spawned.SessionID) {
			if task.CanStop && task.State == "running" {
				taskID, toolCallID = task.ID, task.ToolCallID
				break
			}
		}
		if taskID == "" {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if taskID == "" {
		t.Fatalf("[%s] no stoppable background task: %s", label, backgroundTaskSummary(manager, spawned.SessionID))
	}
	t.Logf("[%s] stopping %s at %s", label, taskID, time.Now().UTC().Format("15:04:05.000"))
	if err := manager.StopBackgroundTask(ctx, spawned.SessionID, taskID); err != nil {
		t.Fatalf("[%s] stop %s: %v", label, taskID, err)
	}
	var final string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, task := range manager.BackgroundTasksForTest(spawned.SessionID) {
			if task.ID == taskID && task.State != "running" && task.State != "paused" {
				final = task.State
			}
		}
		if final != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("[%s] task %s ended %q: %s", label, taskID, final, backgroundTaskSummary(manager, spawned.SessionID))
	if final != "stopped" {
		t.Errorf("[%s] stopped task ended %q, want stopped", label, final)
	}
	if v2 {
		// ACP v2: terminal/cancel does not change the status of the tool call
		// that references the terminal; the agent confirms the end with a
		// terminal_update carrying an exitStatus. Wait for that report, and
		// read no exit 0 out of an end that named no code (spec: an exit
		// without details is unknown, not success).
		exited, exitCode, signal := false, (*int64)(nil), ""
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) && !exited {
			exited, exitCode, signal = manager.TerminalExitForTest(spawned.SessionID, taskID)
			time.Sleep(200 * time.Millisecond)
		}
		t.Logf("[%s] terminal %s exit reported=%v code=%v signal=%v", label, taskID, exited, exitCode, signal)
		if !exited {
			t.Errorf("[%s] no terminal_update with exitStatus after the stop", label)
		} else if exitCode != nil && *exitCode == 0 && signal == "" && final == "stopped" {
			t.Errorf("[%s] killed command shows exit 0", label)
		}
		return
	}
	// v1: the adapter sends an explicit exit_code 0 with the backgrounded
	// result itself; the journal's last projection of the call holds a final
	// status, so the call must not stay open after the stop.
	events, err := store.LoadSessionEvents(spawned.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var last *sessionevents.ACPToolCall
	for _, event := range events {
		if event.ACP == nil {
			continue
		}
		for _, call := range event.ACP.ToolCalls {
			if call.ID == taskID || call.ID == toolCallID || call.Runtime.TerminalID == taskID {
				last = &call
			}
		}
	}
	if last == nil {
		t.Fatalf("[%s] no tool call projection for %s", label, taskID)
	}
	t.Logf("[%s] tool call %s: status=%s exit=%v signal=%v", label, last.ID, last.Status, last.Runtime.TerminalExitCode, last.Runtime.TerminalExitSignal)
	if last.Status == "pending" || last.Status == "in_progress" {
		t.Errorf("[%s] tool call %s stayed %s after the stop", label, last.ID, last.Status)
	}
}
