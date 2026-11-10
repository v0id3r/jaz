package mcpsession

import (
	"context"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	HeaderName        = "X-Jaz-Session-ID"
	HeaderPlaceholder = "{{jaz_session_id}}"
	// TunnelHeader is set by Jaz on requests that came through a session
	// tunnel; a client that sends it only confines itself.
	TunnelHeader = "X-Jaz-Tunnel"
)

type key struct{}

func With(ctx context.Context, id string) context.Context {
	id = strings.TrimSpace(id)
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, key{}, id)
}

func ID(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return strings.TrimSpace(id)
}

func Header(id string) http.Header {
	header := http.Header{}
	id = strings.TrimSpace(id)
	if id != "" {
		header.Set(HeaderName, id)
	}
	return header
}

// SessionID reads the parent Jaz session id an ACP agent attaches to its MCP
// tool calls via HeaderName. Empty when the tool is called outside a session.
func SessionID(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil {
		return ""
	}
	return strings.TrimSpace(req.Extra.Header.Get(HeaderName))
}

// Tunneled reports whether req came through a session tunnel.
func Tunneled(req *mcp.CallToolRequest) bool {
	return req != nil && req.Extra != nil && req.Extra.Header.Get(TunnelHeader) != ""
}
