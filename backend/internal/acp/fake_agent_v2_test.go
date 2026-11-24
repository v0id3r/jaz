package acp_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gluonfield/acp-transport/jsonrpc"
	"github.com/gluonfield/acp-transport/stdio"
)

// TestFakeACPAgentV2Process is a minimal ACP v2 agent the manager tests
// re-execute as a subprocess. Behaviors key off the prompt text: "tool" runs
// a tool call upsert, "background" reports background work across the turn,
// "subagent" announces a child session, "fail" ends the turn with an error,
// "hold" keeps the turn running until a steer or cancel arrives.
func TestFakeACPAgentV2Process(t *testing.T) {
	if os.Getenv("JAZ_FAKE_ACP_V2_AGENT") != "1" {
		return
	}
	conn := stdio.New(os.Stdin, os.Stdout)
	defer conn.Close()
	ctx := context.Background()
	sessionID := "fake-v2-session"
	childID := "fake-v2-child"
	configOptions := []any{
		map[string]any{
			"type": "select", "configId": "model", "name": "Model", "category": "model",
			"currentValue": "fake-large",
			"options":      []any{map[string]any{"value": "fake-large", "name": "Fake Large"}},
		},
		map[string]any{
			"type": "select", "configId": "mode", "name": "Mode", "category": "mode",
			"currentValue": "default",
			"options": []any{
				map[string]any{"value": "default", "name": "Default"},
				map[string]any{"value": "plan", "name": "Plan"},
			},
		},
	}
	prompts := 0
	declineV2 := os.Getenv("JAZ_FAKE_ACP_V2_DECLINE") == "1"

	notify := func(update map[string]any) {
		notifyRaw(conn, sessionID, update)
	}
	notifyChild := func(update map[string]any) {
		notifyRaw(conn, childID, update)
	}
	state := func(fields map[string]any) {
		update := map[string]any{"sessionUpdate": "state_update"}
		for key, value := range fields {
			update[key] = value
		}
		notify(update)
	}
	text := func(messageID, chunk string) {
		notify(map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"messageId":     messageID,
			"content":       map[string]any{"type": "text", "text": chunk},
		})
	}
	answer := func(id json.RawMessage, result any) {
		resp, err := jsonrpc.NewResult(id, result)
		if err == nil {
			_ = conn.Send(ctx, resp)
		}
	}

	var heldPrompt bool
	for {
		msg, err := conn.Receive(ctx)
		if err != nil {
			return
		}
		if !msg.IsRequest() {
			if msg.Method == "session/cancel" {
				state(map[string]any{"state": "idle", "stopReason": "cancelled", "background": []any{}})
			}
			continue
		}
		switch msg.Method {
		case "initialize":
			var req struct {
				ProtocolVersion int `json:"protocolVersion"`
			}
			_ = json.Unmarshal(msg.Params, &req)
			if req.ProtocolVersion < 2 || declineV2 {
				// A v1-only agent: answer v1 shapes so the manager's fallback works.
				answer(*msg.ID, map[string]any{
					"protocolVersion":   1,
					"agentInfo":         map[string]any{"name": "fake-v2-agent", "version": "test"},
					"agentCapabilities": map[string]any{"loadSession": false, "sessionCapabilities": map[string]any{"resume": map[string]any{}}},
				})
				continue
			}
			answer(*msg.ID, map[string]any{
				"protocolVersion": 2,
				"info":            map[string]any{"name": "fake-v2-agent", "version": "test"},
				"capabilities":    map[string]any{"session": map[string]any{}},
			})
		case "session/new", "session/resume":
			result := map[string]any{"configOptions": configOptions}
			if msg.Method == "session/new" {
				result["sessionId"] = sessionID
			}
			answer(*msg.ID, result)
		case "session/set_config_option":
			var req struct {
				ConfigID string `json:"configId"`
				Value    string `json:"value"`
			}
			_ = json.Unmarshal(msg.Params, &req)
			for _, raw := range configOptions {
				option := raw.(map[string]any)
				if option["configId"] == req.ConfigID {
					option["currentValue"] = req.Value
				}
			}
			answer(*msg.ID, map[string]any{"configOptions": configOptions})
		case "session/prompt":
			prompts++
			var req struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			}
			_ = json.Unmarshal(msg.Params, &req)
			message := ""
			if len(req.Prompt) > 0 {
				message = req.Prompt[0].Text
			}
			answer(*msg.ID, map[string]any{"messageId": "user-1"})
			switch {
			case strings.Contains(message, "hold"):
				if !heldPrompt {
					heldPrompt = true
					state(map[string]any{"state": "running", "background": []any{}})
					text("a1", "working on it")
				}
			case strings.Contains(message, "steer-done"):
				state(map[string]any{"state": "idle", "stopReason": "end_turn", "background": []any{}})
				heldPrompt = false
			case strings.Contains(message, "tool"):
				state(map[string]any{"state": "running", "background": []any{}})
				notify(map[string]any{
					"sessionUpdate": "tool_call_update", "toolCallId": "call-1",
					"title": "Read file", "kind": "read", "status": "in_progress",
					"rawInput": map[string]any{"path": "/tmp/x"},
				})
				notify(map[string]any{
					"sessionUpdate": "tool_call_update", "toolCallId": "call-1",
					"status": "completed", "rawOutput": "file body",
				})
				text("a1", "tool done")
				notify(map[string]any{"sessionUpdate": "usage_update", "used": 120, "size": 1000})
				state(map[string]any{"state": "idle", "stopReason": "end_turn", "background": []any{}})
			case strings.Contains(message, "background-finish"):
				notify(map[string]any{
					"sessionUpdate": "tool_call_update", "toolCallId": "bg-1",
					"status": "completed",
				})
				state(map[string]any{"state": "running", "background": []any{}})
				text("a2", "build finished")
				state(map[string]any{"state": "idle", "stopReason": "end_turn", "background": []any{}})
			case strings.Contains(message, "background"):
				state(map[string]any{"state": "running", "background": []any{
					map[string]any{"type": "tool_call", "toolCallId": "bg-1"},
				}})
				notify(map[string]any{
					"sessionUpdate": "tool_call_update", "toolCallId": "bg-1",
					"title": "Watch build", "status": "in_progress",
					"capabilities": map[string]any{"cancel": map[string]any{}},
				})
				text("a1", "watching")
				state(map[string]any{"state": "idle", "stopReason": "end_turn", "background": []any{
					map[string]any{"type": "tool_call", "toolCallId": "bg-1"},
				}})
			case strings.Contains(message, "subagent"):
				state(map[string]any{"state": "running", "background": []any{}})
				notify(map[string]any{
					"sessionUpdate": "subagent_update",
					"sessionId":     childID,
					"title":         "researcher",
					"description":   "research the question",
					"state":         map[string]any{"state": "running"},
				})
				notifyChild(map[string]any{
					"sessionUpdate": "session_message", "messageId": "m1",
					"senderSessionId": sessionID,
					"content":         []any{map[string]any{"type": "text", "text": "go research"}},
				})
				notifyChild(map[string]any{
					"sessionUpdate": "agent_message_chunk", "messageId": "c1",
					"content": map[string]any{"type": "text", "text": "child findings"},
				})
				notifyChild(map[string]any{"sessionUpdate": "state_update", "state": "idle", "stopReason": "end_turn"})
				notify(map[string]any{
					"sessionUpdate": "subagent_update",
					"sessionId":     childID,
					"state":         map[string]any{"state": "idle", "stopReason": "end_turn"},
				})
				text("a1", "subagent done")
				state(map[string]any{"state": "idle", "stopReason": "end_turn", "background": []any{}})
			case strings.Contains(message, "fail"):
				state(map[string]any{"state": "running", "background": []any{}})
				state(map[string]any{"state": "idle", "stopReason": "error", "error": map[string]any{"code": -32603, "message": "provider exploded"}})
			case strings.Contains(message, "overload"):
				state(map[string]any{"state": "running", "background": []any{}})
				state(map[string]any{
					"state": "idle", "stopReason": "error",
					"error": map[string]any{"code": -32603, "message": "Internal error: API Error: 529 Overloaded"},
					"_meta": map[string]any{"jaz": map[string]any{"sessionFailure": map[string]any{
						"id": "turn-1:error", "revision": 1, "category": "service",
						"severity": "error", "title": "API Error: 529 Overloaded", "actions": []any{"retry"},
					}}},
				})
			case strings.Contains(message, "phased"):
				state(map[string]any{"state": "running", "background": []any{}})
				notify(map[string]any{
					"sessionUpdate": "agent_message_chunk", "messageId": "c1",
					"content": map[string]any{"type": "text", "text": "checking things"},
					"_meta":   map[string]any{"jaz": map[string]any{"phase": "commentary"}},
				})
				text("a1", "final answer")
				state(map[string]any{"state": "idle", "stopReason": "end_turn", "background": []any{}})
			default:
				state(map[string]any{"state": "running", "background": []any{}})
				text("a1", "hello ")
				text("a1", "world")
				notify(map[string]any{"sessionUpdate": "usage_update", "used": 10, "size": 1000})
				state(map[string]any{"state": "idle", "stopReason": "end_turn", "background": []any{}})
			}
		default:
			resp, _ := jsonrpc.NewErrorResponse(*msg.ID, jsonrpc.MethodNotFound(msg.Method))
			_ = conn.Send(ctx, resp)
		}
	}
}

func notifyRaw(conn jsonrpc.MessageConn, sessionID string, update map[string]any) {
	msg, err := jsonrpc.NewNotification("session/update", map[string]any{"sessionId": sessionID, "update": update})
	if err == nil {
		_ = conn.Send(context.Background(), msg)
	}
}

func fakeV2ManagerConfig(t *testing.T, extraEnv map[string]string) map[string]string {
	t.Helper()
	env := map[string]string{"JAZ_FAKE_ACP_V2_AGENT": "1"}
	for key, value := range extraEnv {
		env[key] = value
	}
	return env
}

func waitForV2Job(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
