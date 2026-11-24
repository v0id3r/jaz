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
		SystemPrompt: livePrompt("You are a terse test agent. Follow instructions exactly and keep replies under ten words."),
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
		SystemPrompt: livePrompt("You are a terse test agent. Follow instructions exactly and keep replies under ten words."),
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
	if !strings.Contains(resumedMCP.Assistant, "mcp-echo:again") {
		t.Errorf("[%s] resumed session did not see the MCP server added while Jaz was down: %q", label, truncateRunes(resumedMCP.Assistant, 200))
	}
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
