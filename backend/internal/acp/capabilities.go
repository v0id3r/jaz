package acp

import (
	"encoding/json"
	"fmt"

	acpschema "github.com/gluonfield/acp-transport/acp"
)

// systemPromptAppendMeta is advertised in initialize _meta by Jaz-patched
// adapters that apply session _meta.systemPrompt.
const systemPromptAppendMeta = "jaz.dev/systemPromptAppend"

type steerMethod string

const (
	steerUnsupported    steerMethod = ""
	steerPromptQueueing steerMethod = acpschema.AgentMethodSessionPrompt
	steerNative         steerMethod = "_session/steering"
	steerGrokInterject  steerMethod = "_x.ai/interject"
)

func supportedSteerMethod(raw json.RawMessage) steerMethod {
	var resp acpschema.InitializeResponse
	if json.Unmarshal(raw, &resp) != nil {
		return steerUnsupported
	}
	steering, _ := resp.Meta["steering"].(map[string]any)
	if boolMeta(steering, "supported") && boolMeta(steering, "waitForCompletion") {
		return steerNative
	}
	if resp.AgentCapabilities != nil && metaPromptQueueing(resp.AgentCapabilities.Meta) {
		return steerPromptQueueing
	}
	if boolMeta(resp.Meta, "grokShell") {
		return steerGrokInterject
	}
	return steerUnsupported
}

func sessionRestoreMethod(raw json.RawMessage) string {
	var resp acpschema.InitializeResponse
	if json.Unmarshal(raw, &resp) != nil || resp.AgentCapabilities == nil {
		return ""
	}
	if sessions := resp.AgentCapabilities.SessionCapabilities; sessions != nil && sessions.Resume != nil {
		return acpschema.AgentMethodSessionResume
	}
	if resp.AgentCapabilities.LoadSession {
		return acpschema.AgentMethodSessionLoad
	}
	return ""
}

func validateProcessLifecycle(agent string, cfg AgentConfig, raw json.RawMessage) error {
	if cfg.URL != "" || cfg.Local {
		return nil
	}
	agent = CanonicalAgentName(agent)
	if sessionRestoreMethod(raw) == "" {
		return fmt.Errorf("managed ACP agent %q requires session/resume or session/load support", agent)
	}
	if cfg.ManagedAdapter != "" && agentPolicyForAgent(agent).systemPromptAppendMeta && !initializeMetaFlag(raw, systemPromptAppendMeta) {
		return fmt.Errorf("managed ACP agent %q does not advertise %s, so it would drop Jaz's system prompt", agent, systemPromptAppendMeta)
	}
	return nil
}

func initializeMetaFlag(raw json.RawMessage, key string) bool {
	var resp acpschema.InitializeResponse
	return json.Unmarshal(raw, &resp) == nil && boolMeta(resp.Meta, key)
}

func sessionMaterializesOnPrompt(agent string, cfg AgentConfig) bool {
	return (cfg.URL == "" && !cfg.Local) && agentPolicyForAgent(agent).materializesOnPrompt
}

func metaPromptQueueing(meta map[string]any) bool {
	if boolMeta(meta, "promptQueueing") {
		return true
	}
	claudeCode, _ := meta["claudeCode"].(map[string]any)
	return boolMeta(claudeCode, "promptQueueing")
}

func boolMeta(meta map[string]any, key string) bool {
	if v, ok := meta[key].(bool); ok && v {
		return true
	}
	return false
}
