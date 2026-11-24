package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	acpschema "github.com/gluonfield/acp-transport/acp"
	acpv2 "github.com/gluonfield/acp-transport/acp/v2"
	"github.com/gluonfield/acp-transport/jsonrpc"
	"github.com/wins/jaz/backend/internal/promptmodule"
	"github.com/wins/jaz/backend/internal/storage"
)

// protocolV2 marks a connection negotiated at ACP v2; version is a property
// of the connection, and everything after initialize speaks only that version.
const protocolV2 = 2

// steerV2Prompt steers a v2 turn with a plain session/prompt: v2 agents take
// mid-turn prompts into the running turn, and the turn ends on state_update.
const steerV2Prompt steerMethod = "acp-v2/session/prompt"

var errProtocolV2Rejected = errors.New("acp agent does not speak protocol v2")

func (m *Manager) protocolV2Enabled(name string) bool {
	cfg, ok, err := m.configuredAgent(name)
	return err == nil && ok && cfg.ProtocolV2
}

func (c *agentConn) steerMethod() steerMethod {
	if c.protocol == protocolV2 {
		return steerV2Prompt
	}
	return supportedSteerMethod(c.initRaw)
}

func v2InitializeRequest() acpv2.InitializeRequest {
	return acpv2.InitializeRequest{
		ProtocolVersion: acpv2.ProtocolVersion(protocolV2),
		Info:            acpv2.Implementation{Name: "jaz", Title: "Jaz", Version: "0.1.0"},
		Capabilities: &acpv2.ClientCapabilities{
			Elicitation: &acpv2.ElicitationCapabilities{Form: &acpv2.ElicitationFormCapabilities{}},
			Meta:        map[string]any{"jaz.dev/widget": map[string]any{"version": 1}},
		},
	}
}

// initializeConn negotiates the connection's protocol version. With v2
// enabled it asks for version 2; an agent that answers another version or
// rejects the request sends the caller back to start over at v1, since the
// agent has already committed this connection to a version.
func (m *Manager) initializeConn(ctx context.Context, peer *jsonrpc.Peer, name string, stderr *processStderrTail, allowV2 bool) (json.RawMessage, int, error) {
	if allowV2 && m.protocolV2Enabled(name) {
		raw, err := peer.Call(ctx, acpv2.AgentMethodInitialize, v2InitializeRequest())
		if err == nil {
			var resp acpv2.InitializeResponse
			if json.Unmarshal(raw, &resp) == nil && resp.ProtocolVersion == acpv2.ProtocolVersion(protocolV2) {
				return raw, protocolV2, nil
			}
			return nil, 0, errProtocolV2Rejected
		}
		var rpcErr *jsonrpc.Error
		if errors.As(err, &rpcErr) && rpcErr.Code == -32022 {
			return nil, 0, errProtocolV2Rejected
		}
		return nil, 0, fmt.Errorf("initialize acp agent: %w", withProcessStderr(err, stderr))
	}
	raw, err := peer.Call(ctx, acpschema.AgentMethodInitialize, acpschema.InitializeRequest{
		ProtocolVersion: acpschema.ProtocolVersion(acpschema.ProtocolVersionNumber),
		ClientInfo: &acpschema.Implementation{
			Name:    "jaz",
			Title:   "Jaz",
			Version: "0.1.0",
		},
		ClientCapabilities: &acpschema.ClientCapabilities{
			Meta: map[string]any{
				"terminal-auth":         true,
				"terminal_output_delta": true,
				"jetbrains":             map[string]any{"air": map[string]any{"version": 1, "capabilities": []string{"asyncTasks", "recommendedValue", "sessionFailure"}}},
				"jaz.dev/widget":        map[string]any{"version": 1},
			},
			FS: &acpschema.FileSystemCapabilities{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
			Elicitation: &acpschema.ElicitationCapabilities{Form: &acpschema.ElicitationFormCapabilities{}},
		},
	})
	if err != nil {
		return nil, 0, fmt.Errorf("initialize acp agent: %w", withProcessStderr(err, stderr))
	}
	var initialized acpschema.InitializeResponse
	if err := json.Unmarshal(raw, &initialized); err != nil {
		return nil, 0, fmt.Errorf("decode acp initialization: %w", err)
	}
	if initialized.ProtocolVersion != acpschema.ProtocolVersion(acpschema.ProtocolVersionNumber) {
		return nil, 0, fmt.Errorf("unsupported acp protocol version %d", initialized.ProtocolVersion)
	}
	return raw, 1, nil
}

// sessionSystemPromptV2 lifts the agent-specific _meta system prompt into
// v2's standard session field, returning the remaining session _meta.
func sessionSystemPromptV2(meta map[string]any) (*acpv2.SystemPrompt, map[string]any) {
	raw, ok := meta["systemPrompt"]
	if !ok {
		return nil, meta
	}
	rest := make(map[string]any, len(meta))
	for key, value := range meta {
		if key != "systemPrompt" {
			rest[key] = value
		}
	}
	if len(rest) == 0 {
		rest = nil
	}
	mode := acpv2.SystemPromptModeAppend
	var text string
	switch value := raw.(type) {
	case string:
		text = value
	case map[string]any:
		text, _ = value["append"].(string)
		if text == "" {
			text, _ = value["override"].(string)
			if text != "" {
				mode = acpv2.SystemPromptModeOverride
			}
		}
	}
	if text == "" {
		return nil, rest
	}
	block, err := json.Marshal(acpv2.TextContentBlock{Kind: acpv2.ContentBlockText, TextContent: acpv2.TextContent{Text: text}})
	if err != nil {
		return nil, rest
	}
	return &acpv2.SystemPrompt{Mode: mode, Content: []acpv2.ContentBlock{block}}, rest
}

// mcpServersV2 adds the transport discriminator v2 requires on every MCP
// server config; v1 stdio configs carry no type. SSE servers have no v2
// representation and are dropped.
func (m *Manager) mcpServersV2(servers []json.RawMessage, agent string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(servers))
	for _, raw := range servers {
		var server map[string]any
		if json.Unmarshal(raw, &server) != nil {
			continue
		}
		if _, ok := server["type"]; !ok {
			if _, stdio := server["command"]; stdio {
				server["type"] = "stdio"
			} else if _, http := server["url"]; http {
				server["type"] = "http"
			}
		}
		if server["type"] == "sse" {
			m.log.Warn("dropping sse mcp server for acp v2 session", "agent", agent, "server", server["name"])
			continue
		}
		converted, err := json.Marshal(server)
		if err != nil {
			continue
		}
		out = append(out, converted)
	}
	return out
}

// normalizeConfigOptionsV2 renames the v2 configId key to the v1 id in any
// payload carrying configOptions, so the shared config-option parsing reads
// both versions. Payloads without configId pass through unchanged.
func normalizeConfigOptionsV2(raw json.RawMessage) json.RawMessage {
	var doc map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &doc) != nil {
		return raw
	}
	optionsRaw, ok := doc["configOptions"]
	if !ok {
		return raw
	}
	var options []map[string]json.RawMessage
	if json.Unmarshal(optionsRaw, &options) != nil {
		return raw
	}
	changed := false
	for _, option := range options {
		if id, ok := option["configId"]; ok {
			option["id"] = id
			delete(option, "configId")
			changed = true
		}
	}
	if !changed {
		return raw
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		return raw
	}
	doc["configOptions"] = encoded
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

type newSessionRequestV2 struct {
	Meta         map[string]any      `json:"_meta,omitempty"`
	Cwd          string              `json:"cwd"`
	MCPServers   []json.RawMessage   `json:"mcpServers,omitempty"`
	SystemPrompt *acpv2.SystemPrompt `json:"systemPrompt,omitempty"`
}

func (m *Manager) newACPSessionV2(ctx context.Context, ac *agentConn, agent string, cfg AgentConfig, cwd string, session storage.Session, systemPromptExtensions promptmodule.Modules) (acpSessionInfo, error) {
	meta, err := m.sessionMeta(ctx, agent, cfg, cwd, session.RuntimeRef.ArtifactSurface, session.RuntimeRef.MCPServerPolicy, session.SourceType, systemPromptExtensions)
	if err != nil {
		return acpSessionInfo{}, err
	}
	systemPrompt, meta := sessionSystemPromptV2(meta)
	raw, err := ac.peer.Call(ctx, acpv2.AgentMethodSessionNew, newSessionRequestV2{
		Meta:         meta,
		Cwd:          cwd,
		MCPServers:   m.mcpServersV2(m.mcpServersForAgent(m.mcpAuthContext(ctx, ac.sandboxed), ac.initRaw, session.RuntimeRef.MCPServerPolicy, agent), agent),
		SystemPrompt: systemPrompt,
	})
	if err != nil {
		return acpSessionInfo{}, withSignInHint(fmt.Errorf("create acp %s session: %w", "agent", ac.withProcessStderr(err)))
	}
	raw = normalizeConfigOptionsV2(raw)
	var acpSession acpschema.NewSessionResponse
	if err := json.Unmarshal(raw, &acpSession); err != nil {
		return acpSessionInfo{}, err
	}
	if acpSession.SessionID == "" {
		return acpSessionInfo{}, fmt.Errorf("acp session/new returned empty session id")
	}
	ac.state.initial = raw
	return newACPSessionInfo(raw, acpSession), nil
}

// restoreACPSessionV2 resumes a stored provider session over v2; without
// replayFrom the agent replays nothing, matching the v1 session/resume path.
func (m *Manager) restoreACPSessionV2(ctx context.Context, ac *agentConn, agentName string, session storage.Session, cfg AgentConfig, cwd, mcpServerPolicy string, systemPromptExtensions promptmodule.Modules) (string, ModeState, bool, error) {
	storedID := session.RuntimeRef.SessionID
	if storedID != "" {
		meta, err := m.sessionRestoreMeta(ctx, agentName, cfg, cwd, session.RuntimeRef.ArtifactSurface, mcpServerPolicy, session.SourceType, systemPromptExtensions)
		if err != nil {
			return "", ModeState{}, false, err
		}
		systemPrompt, meta := sessionSystemPromptV2(meta)
		raw, err := ac.peer.Call(ctx, acpv2.AgentMethodSessionResume, struct {
			Meta         map[string]any      `json:"_meta,omitempty"`
			Cwd          string              `json:"cwd"`
			MCPServers   []json.RawMessage   `json:"mcpServers,omitempty"`
			SessionID    acpv2.SessionID     `json:"sessionId"`
			SystemPrompt *acpv2.SystemPrompt `json:"systemPrompt,omitempty"`
		}{
			Meta:         meta,
			Cwd:          cwd,
			MCPServers:   m.mcpServersV2(m.mcpServersForAgent(m.mcpAuthContext(ctx, ac.sandboxed), ac.initRaw, mcpServerPolicy, agentName), agentName),
			SessionID:    acpv2.SessionID(storedID),
			SystemPrompt: systemPrompt,
		})
		if err == nil {
			raw = normalizeConfigOptionsV2(raw)
			ac.state.initial = raw
			modes, err := m.configuredModeState(ctx, ac, agentName, newACPSessionInfo(raw, acpschema.NewSessionResponse{
				SessionID: acpschema.SessionID(storedID),
			}), cfg)
			return storedID, modes, true, err
		}
		replaceable, inspectErr := m.replaceableUnmaterializedSession(session, agentName, cfg, err)
		if inspectErr != nil {
			return "", ModeState{}, false, inspectErr
		}
		if !replaceable {
			return "", ModeState{}, false, withSignInHint(fmt.Errorf("resume ACP session %s: %w", storedID, err))
		}
		m.log.Warn("replacing unmaterialized agent session", "agent", agentName, "session", session.ID, "acp_session", storedID)
	}
	acpSession, err := m.newACPSessionV2(ctx, ac, agentName, cfg, cwd, session, systemPromptExtensions)
	if err != nil {
		return "", ModeState{}, false, err
	}
	modes, err := m.configuredModeState(ctx, ac, agentName, acpSession, cfg)
	return string(acpSession.response.SessionID), modes, false, err
}
