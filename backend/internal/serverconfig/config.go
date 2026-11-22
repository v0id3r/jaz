package serverconfig

import (
	"net/netip"
	"net/url"
	"strings"
)

const (
	JazToolsMCPPath       = "/mcp/jaztools"
	JazToolsMCPCompatPath = "/mcp/jaz"
	JazmemMCPPath         = "/mcp/jazmem"
	MCPProxyPath          = "/mcp/proxy"
)

type Config struct {
	Addr               string
	PublicURL          string
	PreviewURLTemplate string
	AllowedOrigins     []string
	// AgentURL is the base URL of Jaz for agents that run outside its network
	// namespace; empty means the local listen address.
	AgentURL string
	// AgentNetworks are where such agents connect from; with the backend key
	// their requests reach the internal MCP endpoints the way loopback
	// requests do.
	AgentNetworks []netip.Prefix
	// MeshNetworks are agent networks whose agents reach the internal MCP
	// endpoints only through mesh tunnels, as the session their certificate
	// names; the backend key does not admit them.
	MeshNetworks []netip.Prefix
}

type URLs struct {
	JazToolsMCP string
	JazmemMCP   string
	MCPProxy    string
}

func New(addr string, publicURL ...string) Config {
	cfg := Config{Addr: strings.TrimSpace(addr)}
	if len(publicURL) > 0 {
		cfg.PublicURL = strings.TrimSpace(publicURL[0])
	}
	return cfg
}

func NewURLs(config Config) URLs {
	base := agentBaseURL(config)
	return URLs{
		JazToolsMCP: base + JazToolsMCPPath,
		JazmemMCP:   base + JazmemMCPPath,
		MCPProxy:    base + MCPProxyPath,
	}
}

func DisplayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "http://" + addr
}

func ClientURL(config Config, key string) string {
	base := ClientBaseURL(config)
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return base + "#key=" + url.QueryEscape(key)
	}
	u.Fragment = "key=" + url.QueryEscape(key)
	return u.String()
}

func ClientBaseURL(config Config) string {
	base := strings.TrimSpace(config.PublicURL)
	if base == "" {
		base = config.Addr
	}
	u, err := url.Parse(DisplayAddr(base))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return DisplayAddr(base)
	}
	u.Path = ""
	u.RawPath = ""
	u.Fragment = ""
	u.RawQuery = ""
	return u.String()
}

func (config Config) PublicBaseURL() string {
	if strings.TrimSpace(config.PublicURL) == "" {
		return ""
	}
	return ClientBaseURL(config)
}

func agentBaseURL(config Config) string {
	if config.AgentURL != "" {
		return strings.TrimRight(config.AgentURL, "/")
	}
	host := strings.TrimSpace(config.Addr)
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	return "http://" + host
}
