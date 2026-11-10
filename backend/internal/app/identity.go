package app

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/log"
	"go.uber.org/fx"

	"github.com/wins/jaz/backend/internal/identity"
	"github.com/wins/jaz/backend/internal/sandbox"
	sqlitestore "github.com/wins/jaz/backend/internal/storage/sqlite"
)

type IdentityConfig struct {
	// Delegate is the SPIRE agent's Delegated Identity API socket. When set
	// with the runner sandbox, agents outside the sandbox get the SPIFFE
	// identity of their session too.
	Delegate string
	Mesh     MeshConfig
}

// MeshConfig lets Jaz terminate session tunnels (HBONE over mTLS with the
// sessions' SVIDs) on its own listener.
type MeshConfig struct {
	// WorkloadAPI is where Jaz gets its own SVID, e.g.
	// unix:///run/spire-agent/public/api.sock; empty turns the mesh off.
	WorkloadAPI string
	// Require lists agent networks, as CIDRs, that reach the internal MCP
	// endpoints only through the mesh; sandboxed agents then get no backend
	// key in their MCP configuration.
	Require []string
}

// Networks parses Require.
func (c MeshConfig) Networks() ([]netip.Prefix, error) {
	networks := make([]netip.Prefix, 0, len(c.Require))
	for _, cidr := range c.Require {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return nil, fmt.Errorf("identity.mesh.require: %w", err)
		}
		networks = append(networks, prefix)
	}
	if len(networks) > 0 && c.WorkloadAPI == "" {
		return nil, fmt.Errorf("identity.mesh.require needs identity.mesh.workloadAPI")
	}
	return networks, nil
}

// NewLauncher builds the process launcher, with session identities for local
// processes when a SPIRE delegate socket is configured.
func NewLauncher(lc fx.Lifecycle, cfg Config, store *sqlitestore.Store, logger *log.Logger) (sandbox.Launcher, error) {
	sb := cfg.Sandbox
	if cfg.Identity.Delegate != "" && sb.Runtime == "runner" {
		provider, err := identity.New(identity.Config{
			Delegate: cfg.Identity.Delegate,
			Runner:   sb.Socket,
			Dir:      filepath.Join(store.RootDir(), "run", "identity"),
		}, logger.WithPrefix("identity"))
		if err != nil {
			return nil, err
		}
		lc.Append(fx.StopHook(provider.Close))
		sb.Identity = provider
	}
	return sandbox.New(sb)
}
