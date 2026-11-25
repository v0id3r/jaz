package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/wins/jaz/backend/internal/mcpconfig"
	"github.com/wins/jaz/backend/internal/mcpsession"

	acpschema "github.com/gluonfield/acp-transport/acp"
)

const grokMCPUpdateMethod = "_x.ai/session/update_mcp_servers"

type MCPRevisionSource interface {
	Revision() uint64
}

func (m *Manager) SetMCPRevisionSource(source MCPRevisionSource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mcpRevisionSource = source
}

func (m *Manager) mcpRevision() uint64 {
	m.mu.RLock()
	source := m.mcpRevisionSource
	m.mu.RUnlock()
	if source == nil {
		return 0
	}
	return source.Revision()
}

func supportedMCPRefresh(raw json.RawMessage, policy string) string {
	if !fullMCPPolicy(policy) {
		return ""
	}
	var response acpschema.InitializeResponse
	if json.Unmarshal(raw, &response) != nil || response.AgentCapabilities == nil {
		return ""
	}
	capabilities := response.AgentCapabilities
	if capabilities.MCPCapabilities == nil || !capabilities.MCPCapabilities.HTTP {
		return ""
	}
	if boolMeta(response.Meta, "grokShell") && boolMeta(response.Meta, "x.ai/mcp/sdk") {
		return grokMCPUpdateMethod
	}
	return ""
}

// A changed MCP catalog reaches these agents only through a relaunch that
// resumes the thread with the fresh server list: upstream Claude replaces the
// query on a resume whose mcpServers differ, Codex keeps a loaded thread's
// tool catalog through reloads, and neither offers a live refresh method.
// Grok updates in place through its own method and never retires here.
func (m *Manager) retireStaleMCPCatalog(job *jobState, process *agentProcess) {
	if (job.ACPAgent != AgentCodex && job.ACPAgent != AgentClaude) || process.mcpRefresh != "" || !fullMCPPolicy(process.mcpPolicy) || process.mcpRevision == m.mcpRevision() {
		return
	}
	if !m.subagentsRunning(job) && !m.hasLivePermissionWaiter(job.ID) {
		job.retire()
	}
}

func (m *Manager) refreshMCPBeforeTurn(ctx context.Context, job *jobState) error {
	m.mu.RLock()
	process := m.processes[job.ID]
	m.mu.RUnlock()
	if process == nil || process.mcpRefresh == "" {
		return nil
	}
	revision := m.mcpRevision()
	if process.mcpRevision == revision {
		return nil
	}
	params := map[string]any{"sessionId": job.ACPSession}
	servers, err := m.grokMCPServers(m.mcpAuthContext(ctx, process.sandboxed), job, process.mcpPolicy, revision)
	if err != nil {
		return err
	}
	params["mcpServers"] = servers
	raw, err := process.peer.Call(ctx, process.mcpRefresh, params)
	if err != nil {
		return fmt.Errorf("refresh agent MCP tools: %w", err)
	}
	var response struct {
		Result struct {
			OK bool `json:"ok"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &response) != nil || !response.Result.OK {
		return fmt.Errorf("agent did not acknowledge MCP update")
	}
	process.mcpRevision = revision
	return nil
}

func (m *Manager) grokMCPServers(ctx context.Context, job *jobState, policy string, revision uint64) ([]json.RawMessage, error) {
	servers, err := enabledHTTPMCPServers(mcpsession.With(ctx, job.ID), m.cfg.MCPStore, policy)
	if err != nil {
		return nil, err
	}
	for i, raw := range servers {
		var server httpMCPServer
		if err := json.Unmarshal(raw, &server); err != nil {
			return nil, err
		}
		if server.Name != "jaz_mcp" {
			continue
		}
		// Grok reconnects only changed configurations; version the Jaz-owned connection.
		server.Headers = append(server.Headers, mcpconfig.Header{Name: "X-Jaz-MCP-Revision", Value: strconv.FormatUint(revision, 10)})
		servers[i], err = json.Marshal(server)
		if err != nil {
			return nil, err
		}
	}
	return servers, nil
}
