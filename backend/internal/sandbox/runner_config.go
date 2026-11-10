package sandbox

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/viper"

	"github.com/wins/jaz/backend/internal/mesh"
)

// RunnerConfig configures `jaz runner`. ReadOnly, ReadWrite and Writable hold
// paths as Jaz sees them; Mounts translates those to host paths.
type RunnerConfig struct {
	Socket string
	// SocketOwner is the "uid:gid" of the Jaz process allowed to connect.
	SocketOwner string
	Podman      string
	Image       string
	Network     string
	DNS         []string
	// Hosts are extra "name:ip" entries for the containers' /etc/hosts.
	Hosts []string
	// User is the container user, matching the owner of Jaz's files.
	User string
	// Home replaces the HOME a launch asks for, so sessions keep their own
	// dotfiles and caches apart from the Jaz process's home.
	Home        string
	StopTimeout time.Duration
	Mounts      []PathMapping
	// ReadOnly and ReadWrite are mounted into every container.
	ReadOnly  []string
	ReadWrite []string
	// Writable are the roots under which a launch may request writable paths.
	Writable []string
	// Args are extra `podman run` arguments, such as resource limits.
	Args []string
	// Identity, when set, gives sessions SPIFFE identities from SPIRE.
	Identity RunnerIdentity
	// Mesh, when set, tunnels sessions' connections to chosen destinations
	// over mTLS with their identities.
	Mesh mesh.Config
}

type PathMapping struct {
	Container string
	Host      string
}

func loadRunnerConfig(path string) (RunnerConfig, error) {
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return RunnerConfig{}, err
	}
	var cfg RunnerConfig
	if err := v.Unmarshal(&cfg); err != nil {
		return RunnerConfig{}, err
	}
	return cfg, cfg.normalize()
}

func (c *RunnerConfig) normalize() error {
	if c.Podman == "" {
		c.Podman = "podman"
	}
	if c.StopTimeout == 0 {
		c.StopTimeout = 10 * time.Second
	}
	for name, value := range map[string]string{"socket": c.Socket, "image": c.Image, "network": c.Network} {
		if value == "" {
			return fmt.Errorf("runner config: %s is required", name)
		}
	}
	if c.Home != "" && !filepath.IsAbs(c.Home) {
		return fmt.Errorf("runner config: home %s must be absolute", c.Home)
	}
	for _, mount := range c.Mounts {
		if !filepath.IsAbs(mount.Container) || !filepath.IsAbs(mount.Host) {
			return fmt.Errorf("runner config: mount %s -> %s must use absolute paths", mount.Container, mount.Host)
		}
	}
	for _, list := range []*[]string{&c.ReadOnly, &c.ReadWrite, &c.Writable} {
		for i, path := range *list {
			path, err := cleanPath(path)
			if err != nil {
				return fmt.Errorf("runner config: %w", err)
			}
			if _, ok := c.hostPath(path); !ok {
				return fmt.Errorf("runner config: %s is not under a mount", path)
			}
			(*list)[i] = path
		}
	}
	return nil
}
