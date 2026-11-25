package acp

import (
	"encoding/json"
	"testing"
)

func TestSessionSystemPromptSupportedV1(t *testing.T) {
	raw := json.RawMessage(`{"agentCapabilities":{"sessionCapabilities":{"systemPrompt":{}}}}`)
	if !sessionSystemPromptSupported(raw, 1) {
		t.Fatal("v1 sessionCapabilities.systemPrompt not detected")
	}
	raw = json.RawMessage(`{"agentCapabilities":{"sessionCapabilities":{"resume":{}}}}`)
	if sessionSystemPromptSupported(raw, 1) {
		t.Fatal("v1 capability detected without systemPrompt")
	}
}

func TestSessionSystemPromptSupportedV2(t *testing.T) {
	raw := json.RawMessage(`{"capabilities":{"session":{"systemPrompt":{}}}}`)
	if !sessionSystemPromptSupported(raw, protocolV2) {
		t.Fatal("v2 capabilities.session.systemPrompt not detected")
	}
	raw = json.RawMessage(`{"capabilities":{"session":{"fork":{}}}}`)
	if sessionSystemPromptSupported(raw, protocolV2) {
		t.Fatal("v2 capability detected without systemPrompt")
	}
}

func TestSessionSystemPromptV1LiftsMetaPrompt(t *testing.T) {
	for _, test := range []struct {
		name     string
		meta     map[string]any
		wantMode string
		wantText string
		wantMeta int
	}{
		{"bare string", map[string]any{"systemPrompt": "hello", "keep": "x"}, "append", "hello", 1},
		{"append object", map[string]any{"systemPrompt": map[string]any{"append": "a"}}, "append", "a", 0},
		{"override object", map[string]any{"systemPrompt": map[string]any{"override": "o"}}, "override", "o", 0},
		{"no prompt", map[string]any{"keep": "x"}, "", "", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			prompt, rest := sessionSystemPromptV1(test.meta)
			if test.wantText == "" {
				if prompt != nil {
					t.Fatalf("prompt = %v, want none", prompt)
				}
				return
			}
			if prompt == nil || string(prompt.Mode) != test.wantMode || len(rest) != test.wantMeta {
				t.Fatalf("prompt = %v, rest = %v", prompt, rest)
			}
			var block struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(prompt.Content[0], &block); err != nil || block.Type != "text" || block.Text != test.wantText {
				t.Fatalf("block = %q: %v", prompt.Content[0], err)
			}
		})
	}
}
