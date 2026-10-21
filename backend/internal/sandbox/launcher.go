package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Spec is a process to start. ReadOnly and ReadWrite list paths the process
// needs besides Dir, which is always writable; a sandboxing launcher exposes
// nothing else of the host's private state.
type Spec struct {
	Path      string
	Args      []string
	Env       []string
	Dir       string
	ReadOnly  []string
	ReadWrite []string
}

// Launcher turns a Spec into the command to execute. The returned cleanup
// must run once the command has exited.
type Launcher interface {
	Command(spec Spec) (Spec, func(), error)
}

type Config struct {
	Runtime string
	Socket  string
	// Paths limits the sandbox to processes whose working directory is under
	// one of them; others run locally. Empty sandboxes every process.
	Paths []string
}

func New(cfg Config) (Launcher, error) {
	var sandbox Launcher
	switch runtime := strings.TrimSpace(cfg.Runtime); runtime {
	case "":
		return Local{}, nil
	case "runner":
		socket := strings.TrimSpace(cfg.Socket)
		if socket == "" {
			return nil, fmt.Errorf("sandbox runtime runner needs a socket")
		}
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		sandbox = runnerLauncher{self: self, socket: socket}
	default:
		return nil, fmt.Errorf("unknown sandbox runtime %q", runtime)
	}
	if len(cfg.Paths) == 0 {
		return sandbox, nil
	}
	paths := make([]string, 0, len(cfg.Paths))
	for _, path := range cfg.Paths {
		path, err := cleanPath(strings.TrimSpace(path))
		if err != nil {
			return nil, fmt.Errorf("sandbox paths: %w", err)
		}
		paths = append(paths, path)
	}
	return scoped{paths: paths, sandbox: sandbox}, nil
}

// scoped sandboxes only processes started under its paths.
type scoped struct {
	paths   []string
	sandbox Launcher
}

func (s scoped) Command(spec Spec) (Spec, func(), error) {
	dir := filepath.Clean(spec.Dir)
	if slices.ContainsFunc(s.paths, func(path string) bool { return within(path, dir) }) {
		return s.sandbox.Command(spec)
	}
	return Local{}.Command(spec)
}

// Local runs processes directly on the host.
type Local struct{}

func (Local) Command(spec Spec) (Spec, func(), error) {
	return spec, func() {}, nil
}

// runnerLauncher starts each process in a runner-managed container through
// `jaz sandbox-attach`, which keeps the local process semantics (stdio,
// signals, exit status) the caller already supervises.
type runnerLauncher struct {
	self   string
	socket string
}

func (r runnerLauncher) Command(spec Spec) (Spec, func(), error) {
	args := []string{"sandbox-attach", "--socket", r.socket}
	for _, path := range spec.ReadOnly {
		args = append(args, "--ro", path)
	}
	for _, path := range spec.ReadWrite {
		args = append(args, "--rw", path)
	}
	args = append(append(args, "--", spec.Path), spec.Args...)
	return Spec{Path: r.self, Args: args, Env: spec.Env, Dir: spec.Dir}, func() {}, nil
}
