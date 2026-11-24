package acpv2

import (
	"encoding/json"
	"testing"
)

func TestDecodeStateUpdateWithBackgroundAndError(t *testing.T) {
	raw := json.RawMessage(`{
		"sessionUpdate": "state_update",
		"state": "idle",
		"stopReason": "error",
		"error": {"code": -32000, "message": "Authentication required"},
		"background": [{"type": "terminal", "terminalId": "term-1"}, {"type": "tool_call", "toolCallId": "call-2"}]
	}`)
	update, err := DecodeSessionUpdate(raw)
	if err != nil {
		t.Fatal(err)
	}
	state, ok := update.(StateSessionUpdate)
	if !ok {
		t.Fatalf("update type = %T, want StateSessionUpdate", update)
	}
	if state.State != "idle" || !state.Terminal() || state.StopReason != "error" {
		t.Fatalf("state = %+v", state.StateUpdateEvent)
	}
	if state.Error == nil || state.Error.Code != -32000 || state.Error.Message != "Authentication required" {
		t.Fatalf("error = %+v", state.Error)
	}
	if !state.BackgroundReported {
		t.Fatal("background list present but not reported")
	}
	if len(state.Background) != 2 || state.Background[0].Terminal != "term-1" || state.Background[1].ToolCall != "call-2" {
		t.Fatalf("background = %+v", state.Background)
	}
}

func TestDecodeStateUpdateOmittedBackground(t *testing.T) {
	update, err := DecodeSessionUpdate(json.RawMessage(`{"sessionUpdate":"state_update","state":"running"}`))
	if err != nil {
		t.Fatal(err)
	}
	state := update.(StateSessionUpdate)
	if state.BackgroundReported || state.Background != nil {
		t.Fatalf("omitted background reported as %+v", state.Background)
	}
	if state.Terminal() {
		t.Fatal("running state reported terminal")
	}
}

func TestDecodeStateUpdateEmptyBackground(t *testing.T) {
	update, err := DecodeSessionUpdate(json.RawMessage(`{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn","background":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	state := update.(StateSessionUpdate)
	if !state.BackgroundReported || len(state.Background) != 0 {
		t.Fatalf("empty background list = reported %v items %v", state.BackgroundReported, state.Background)
	}
}

func TestDecodeToolCallUpdate(t *testing.T) {
	raw := json.RawMessage(`{
		"sessionUpdate": "tool_call_update",
		"toolCallId": "call-1",
		"title": "Read file",
		"kind": "read",
		"status": "in_progress",
		"capabilities": {"cancel": {}},
		"rawInput": {"path": "/tmp/x"}
	}`)
	update, err := DecodeSessionUpdate(raw)
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := update.(ToolCallUpdateSessionUpdate)
	if !ok {
		t.Fatalf("update type = %T", update)
	}
	if tool.ToolCallID != "call-1" || tool.Title != "Read file" || tool.Status == nil || *tool.Status != ToolCallStatusInProgress {
		t.Fatalf("tool call = %+v", tool.ToolCallUpdate)
	}
	if tool.Capabilities == nil || tool.Capabilities.Cancel == nil {
		t.Fatal("cancel capability lost")
	}
	if len(tool.RawJSON()) == 0 {
		t.Fatal("raw JSON not recorded")
	}
}

func TestDecodeUnknownUpdateKeepsKind(t *testing.T) {
	update, err := DecodeSessionUpdate(json.RawMessage(`{"sessionUpdate":"future_update","x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if update.SessionUpdateKind() != "future_update" {
		t.Fatalf("kind = %q", update.SessionUpdateKind())
	}
}

func TestTextFromContent(t *testing.T) {
	var content []ContentBlock
	for _, raw := range []string{
		`{"type":"text","text":"hello "}`,
		`{"type":"image","data":"eA==","mimeType":"image/png"}`,
		`{"type":"text","text":"world"}`,
	} {
		content = append(content, ContentBlock(raw))
	}
	if got := TextFromContent(content); got != "hello world" {
		t.Fatalf("text = %q", got)
	}
}
