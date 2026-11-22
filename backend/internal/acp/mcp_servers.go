package acp

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strings"

	acpschema "github.com/gluonfield/acp-transport/acp"
	mcpconfig "github.com/wins/jaz/backend/internal/mcpconfig"
	"github.com/wins/jaz/backend/internal/mcpdeferred"
	"github.com/wins/jaz/backend/internal/mcpsession"
	"github.com/wins/jaz/backend/internal/serverconfig"
)

const jaztoolsSurfaceQueryParam = "jaztools_surface"

type withoutBackendKeyKey struct{}

// mcpAuthContext keeps the backend key out of the MCP servers of a sandboxed
// agent when Jaz admits sandboxed agents by their session certificate.
func (m *Manager) mcpAuthContext(ctx context.Context, sandboxed bool) context.Context {
	if !sandboxed || !m.cfg.SandboxMCPWithoutKey {
		return ctx
	}
	return context.WithValue(ctx, withoutBackendKeyKey{}, true)
}

func withoutBackendKey(ctx context.Context) bool {
	without, _ := ctx.Value(withoutBackendKeyKey{}).(bool)
	return without
}

type httpMCPServer struct {
	Type    string             `json:"type"`
	Name    string             `json:"name"`
	URL     string             `json:"url"`
	Headers []mcpconfig.Header `json:"headers"`
}

func (m *Manager) mcpServersForAgent(ctx context.Context, initRaw json.RawMessage, policy, agent string) []json.RawMessage {
	var init struct {
		AgentCapabilities acpschema.AgentCapabilities `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(initRaw, &init); err != nil {
		return []json.RawMessage{}
	}
	if init.AgentCapabilities.MCPCapabilities == nil || !init.AgentCapabilities.MCPCapabilities.HTTP {
		return []json.RawMessage{}
	}
	servers, err := enabledHTTPMCPServers(ctx, m.cfg.MCPStore, policy)
	if err != nil {
		m.log.Warn("load mcp servers for acp failed", "error", err)
		servers = nil
	}
	if servers == nil {
		return []json.RawMessage{}
	}
	if agentAcceptsDeferredTools(agent, policy) {
		for i, raw := range servers {
			var server httpMCPServer
			if json.Unmarshal(raw, &server) != nil {
				continue
			}
			u, err := url.Parse(server.URL)
			if err != nil || u.Path != serverconfig.MCPProxyPath {
				continue
			}
			query := u.Query()
			query.Set(mcpdeferred.QueryParam, "1")
			u.RawQuery = query.Encode()
			server.URL = u.String()
			servers[i], _ = json.Marshal(server)
		}
	}
	return servers
}

func enabledHTTPMCPServers(ctx context.Context, store mcpconfig.ServerReader, policy string) ([]json.RawMessage, error) {
	if store == nil {
		return nil, nil
	}
	servers, err := store.ListMCPServers()
	if err != nil {
		return nil, err
	}
	var out []json.RawMessage
	for _, server := range servers {
		if !server.Enabled {
			continue
		}
		if server.Transport != mcpconfig.TransportStreamableHTTP {
			continue
		}
		if !mcpServerAllowed(policy, server) {
			continue
		}
		headers := resolvedACPHeaders(ctx, server)
		if withoutBackendKey(ctx) {
			headers = slices.DeleteFunc(headers, func(header mcpconfig.Header) bool {
				return strings.EqualFold(header.Name, "Authorization")
			})
		}
		if headers == nil {
			// codex-acp's schema requires an array; null fails session/new.
			headers = []mcpconfig.Header{}
		}
		payload := httpMCPServer{
			Type:    "http",
			Name:    server.Name,
			URL:     mcpServerURL(policy, server),
			Headers: headers,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(data))
	}
	return out, nil
}

func mcpServerAllowed(policy string, server mcpconfig.Server) bool {
	policy = strings.TrimSpace(policy)
	if restrictedWorkerPolicy(policy) {
		return isJaztoolsServer(server)
	}
	return fullMCPPolicy(policy)
}

// agentAcceptsDeferredTools reports whether an agent receives the deferred
// search_tools/call_tool catalogue on the MCP proxy connection for the
// session's policy. The JazTools server stays inline; restricted worker
// policies keep their inline surface everywhere.
func agentAcceptsDeferredTools(agent, policy string) bool {
	switch CanonicalAgentName(agent) {
	case AgentKimi:
		return fullMCPPolicy(policy)
	case AgentOpenCode:
		return policy == MCPServerPolicyBot
	}
	return false
}

// mcpServerURL routes the jaztools server to the tool surface named by the
// policy itself: the policy string and the jaztools surface query param are one
// and the same, so they cannot drift out of sync.
func mcpServerURL(policy string, server mcpconfig.Server) string {
	policy = strings.TrimSpace(policy)
	if policy != MCPServerPolicyAll && isJaztoolsServer(server) {
		return jaztoolsSurfaceURL(server.URL, policy)
	}
	return server.URL
}

func isJaztoolsServer(server mcpconfig.Server) bool {
	return strings.EqualFold(strings.TrimSpace(server.ID), "jaztools") ||
		strings.EqualFold(strings.TrimSpace(server.Name), "jaztools")
}

func jaztoolsSurfaceURL(raw, surface string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set(jaztoolsSurfaceQueryParam, surface)
	u.RawQuery = q.Encode()
	return u.String()
}

func resolvedACPHeaders(ctx context.Context, server mcpconfig.Server) []mcpconfig.Header {
	server.Headers = resolveSessionHeaderValues(ctx, server.Headers)
	headers, err := mcpconfig.ResolvedHeaders(server, false)
	if err != nil {
		return nil
	}
	return headers
}

func resolveSessionHeaderValues(ctx context.Context, headers []mcpconfig.Header) []mcpconfig.Header {
	sessionID := mcpsession.ID(ctx)
	out := make([]mcpconfig.Header, 0, len(headers))
	for _, header := range headers {
		header.Name = strings.TrimSpace(header.Name)
		if !strings.EqualFold(header.Name, mcpsession.HeaderName) {
			out = append(out, header)
			continue
		}
		if header.Value != mcpsession.HeaderPlaceholder {
			if strings.TrimSpace(header.Value) != "" {
				out = append(out, header)
			}
			continue
		}
		if sessionID != "" {
			header.Value = sessionID
			out = append(out, header)
		}
	}
	return out
}
