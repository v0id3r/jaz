package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSupportedSteerMethod(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want steerMethod
	}{
		{"prompt queue", `{"agentCapabilities":{"_meta":{"claudeCode":{"promptQueueing":true}}}}`, steerPromptQueueing},
		{"native", `{"_meta":{"steering":{"supported":true,"waitForCompletion":true}}}`, steerNative},
		{"Grok 1.0.25 initialization", `{"_meta":{"grokShell":true,"agentVersion":"1.0.25"}}`, steerGrokInterject},
		{"unknown agent", `{"agentCapabilities":{"loadSession":true}}`, steerUnsupported},
		{"native wins over prompt queue", `{"agentCapabilities":{"_meta":{"claudeCode":{"promptQueueing":true}}},"_meta":{"steering":{"supported":true,"waitForCompletion":true}}}`, steerNative},
		{"prompt queue without native completion", `{"agentCapabilities":{"_meta":{"claudeCode":{"promptQueueing":true}}},"_meta":{"steering":{"supported":true}}}`, steerPromptQueueing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := supportedSteerMethod(json.RawMessage(test.raw)); got != test.want {
				t.Fatalf("supportedSteerMethod() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestValidateProcessLifecycleRequiresManagedKimiPromptAppend(t *testing.T) {
	without := json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}`)
	with := json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"loadSession":true},"_meta":{"jaz.dev/systemPromptAppend":true}}`)
	managed := AgentConfig{ManagedAdapter: AgentKimi}
	if err := validateProcessLifecycle(AgentKimi, managed, without); err == nil || !strings.Contains(err.Error(), systemPromptAppendMeta) {
		t.Fatalf("managed kimi without prompt append: %v", err)
	}
	if err := validateProcessLifecycle(AgentKimi, managed, with); err != nil {
		t.Fatal(err)
	}
	if err := validateProcessLifecycle(AgentKimi, AgentConfig{Command: "kimi"}, without); err != nil {
		t.Fatalf("user-configured kimi command: %v", err)
	}
}
