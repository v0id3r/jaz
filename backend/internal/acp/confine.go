package acp

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/wins/jaz/backend/internal/sandbox"
)

// A sandboxed thread stays in the sandbox through its thread tools too: the
// threads it starts run sandboxed, and it acts only on sandboxed threads.
// Callers are known by the session Jaz verified, so this holds for threads
// whose requests reach Jaz only through the mesh.

var errConfined = errors.New("a sandboxed thread can act only on threads in the sandbox")

// threadCwd is the working directory of a thread given by ID or, while it
// runs, by slug.
func (m *Manager) threadCwd(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	if job, err := m.job(ref); err == nil {
		return job.Snapshot().Cwd, true
	}
	session, err := m.store.LoadSession(ref)
	if err != nil || session.RuntimeRef == nil {
		return "", false
	}
	return session.RuntimeRef.Cwd, true
}

func (m *Manager) sandboxedThread(ref string) bool {
	cwd, ok := m.threadCwd(ref)
	return ok && cwd != "" && sandbox.Sandboxes(m.cfg.Launcher, cwd)
}

// confine refuses a sandboxed thread an action on a thread outside the
// sandbox, or on one it cannot be told is inside.
func (m *Manager) confine(from, target string) error {
	if !m.sandboxedThread(from) || m.sandboxedThread(target) {
		return nil
	}
	return errConfined
}

// confineSpawn keeps a thread a sandboxed thread starts in the sandbox: in
// the parent's directory unless the request names another sandboxed one,
// never on an agent that runs in Jaz itself, and never in a worktree, which
// Jaz would create by running git on the sandbox's repository.
func (m *Manager) confineSpawn(req SpawnRequest, cfg AgentConfig) (SpawnRequest, error) {
	if !m.sandboxedThread(req.ParentID) {
		return req, nil
	}
	if cfg.Local {
		return req, fmt.Errorf("a sandboxed thread cannot start the local agent %q", req.ACPAgent)
	}
	if req.Worktree || req.Home != "" {
		return req, fmt.Errorf("a sandboxed thread can start threads only in a sandboxed directory, without a worktree")
	}
	if req.Directory == "" {
		req.Directory, _ = m.threadCwd(req.ParentID)
	}
	if !filepath.IsAbs(req.Directory) || !sandbox.Sandboxes(m.cfg.Launcher, filepath.Clean(req.Directory)) {
		return req, fmt.Errorf("%w: %s is outside it", errConfined, req.Directory)
	}
	return req, nil
}
