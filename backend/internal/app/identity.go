package app

import (
	"path/filepath"

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
