package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

const (
	runnerLabel = "jaz.runner=1"
	podmanPath  = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// runArgs builds the `podman run` arguments for a request and the environment
// of the podman process. Environment values reach the container through
// podman's own environment (`--env NAME`), so secrets never appear in argv;
// only variables podman itself interprets are passed by value.
func (c RunnerConfig) runArgs(name string, req Request) ([]string, []string, error) {
	if len(req.Args) == 0 {
		return nil, nil, errors.New("request has no command")
	}
	dir, err := cleanPath(req.Dir)
	if err != nil {
		return nil, nil, err
	}
	mounts := map[string]bool{}
	for _, path := range c.ReadOnly {
		mounts[path] = false
	}
	for _, path := range req.ReadOnly {
		path, err := cleanPath(path)
		if err != nil {
			return nil, nil, err
		}
		// Paths outside every mount ship in the image itself.
		if _, ok := c.hostPath(path); ok {
			mounts[path] = false
		}
	}
	for _, path := range c.ReadWrite {
		mounts[path] = true
	}
	for _, path := range append([]string{dir}, req.ReadWrite...) {
		path, err := cleanPath(path)
		if err != nil {
			return nil, nil, err
		}
		if !c.writable(path) {
			return nil, nil, fmt.Errorf("%s is not under a writable root", path)
		}
		mounts[path] = true
	}

	args := []string{
		"run", "--rm", "-i", "--init", "--pull=never", "--log-driver=none",
		"--name", name, "--label", runnerLabel, "--userns=auto",
		"--cap-drop=all", "--security-opt=no-new-privileges",
		"--network", c.Network, "--workdir", dir,
	}
	if req.TTY {
		args = append(args, "-t")
	}
	if c.User != "" {
		args = append(args, "--user", c.User)
	}
	for _, dns := range c.DNS {
		args = append(args, "--dns", dns)
	}
	for _, host := range c.Hosts {
		args = append(args, "--add-host", host)
	}
	paths := make([]string, 0, len(mounts))
	for path := range mounts {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		host, err := c.resolve(path, mounts[path])
		if err != nil {
			return nil, nil, err
		}
		options := "idmap"
		if !mounts[path] {
			options += ",ro"
		}
		args = append(args, "--volume", host+":"+path+":"+options)
	}
	env := []string{podmanPath}
	if c.Home != "" {
		args = append(args, "--env", "HOME="+c.Home)
	}
	for _, entry := range req.Env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || key == "HOME" && c.Home != "" {
			continue
		}
		if podmanReads(key) {
			args = append(args, "--env", entry)
			continue
		}
		args = append(args, "--env", key)
		env = append(env, entry)
	}
	entrypoint, err := json.Marshal(req.Args)
	if err != nil {
		return nil, nil, err
	}
	args = append(args, c.Args...)
	return append(args, "--entrypoint", string(entrypoint), c.Image), env, nil
}

func podmanReads(key string) bool {
	return key == "PATH" || key == "HOME" || key == "TMPDIR" ||
		strings.HasPrefix(key, "XDG_") || strings.HasPrefix(key, "CONTAINER")
}

// resolve maps a Jaz path to the host path to mount. Symlinks are resolved on
// the host and the target must still be a path this mount may expose.
func (c RunnerConfig) resolve(path string, writable bool) (string, error) {
	host, ok := c.hostPath(path)
	if !ok {
		return "", fmt.Errorf("%s is not under a mount", path)
	}
	real, err := filepath.EvalSymlinks(host)
	if err != nil {
		return "", err
	}
	back, ok := c.jazPath(real)
	if !ok || writable && !c.writable(back) {
		return "", fmt.Errorf("%s resolves outside the allowed paths", path)
	}
	return real, nil
}

func (c RunnerConfig) hostPath(path string) (string, bool) {
	best := -1
	for i, mount := range c.Mounts {
		if within(mount.Container, path) && (best < 0 || len(mount.Container) > len(c.Mounts[best].Container)) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	rel, _ := filepath.Rel(c.Mounts[best].Container, path)
	return filepath.Join(c.Mounts[best].Host, rel), true
}

func (c RunnerConfig) jazPath(host string) (string, bool) {
	best := -1
	for i, mount := range c.Mounts {
		if within(mount.Host, host) && (best < 0 || len(mount.Host) > len(c.Mounts[best].Host)) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	rel, _ := filepath.Rel(c.Mounts[best].Host, host)
	return filepath.Join(c.Mounts[best].Container, rel), true
}

func (c RunnerConfig) writable(path string) bool {
	return slices.ContainsFunc(c.Writable, func(root string) bool { return within(root, path) })
}

func within(root, path string) bool {
	return path == root || root == "/" || strings.HasPrefix(path, root+"/")
}

func cleanPath(path string) (string, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, ":,") {
		return "", fmt.Errorf("invalid path %q", path)
	}
	return filepath.Clean(path), nil
}
