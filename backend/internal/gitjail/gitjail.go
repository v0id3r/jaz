// Package gitjail runs git so that a repository cannot use it against Jaz.
//
// Git trusts a repository's own config, attributes and hooks: core.fsmonitor,
// clean filters, textconv, config-defined hooks and many more run commands,
// and core.worktree or a .git file send git to other directories (git(1),
// SECURITY). Jaz runs git in repositories that agents write, so on Linux each
// git runs in a Landlock domain: it may execute only git itself, write only
// the repository it works on, read only system files and its global config,
// and open no network or unix sockets (seccomp covers what this kernel's
// Landlock does not). Elsewhere git runs as before.
package gitjail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// helperArg makes the Jaz binary confine itself and exec git; see helper.
const helperArg = "__jaz_git_jail"

// Policy is what one git command may touch beyond system files.
type Policy struct {
	// Writable are the directories git may change.
	Writable []string `json:"w,omitempty"`
	// Readable are directories git may only read.
	Readable []string `json:"r,omitempty"`
	// Network lets git reach remotes over TCP, for push and fetch.
	Network bool `json:"n,omitempty"`
	// Shell lets git run /bin/sh, which its local transport needs. Commands
	// a repository configures then run too, but inside the same jail.
	Shell bool `json:"sh,omitempty"`
}

// Command returns `git args...` run under policy.
func Command(ctx context.Context, policy Policy, args ...string) (*exec.Cmd, error) {
	if !supported {
		return exec.CommandContext(ctx, "git", args...), nil
	}
	sys, err := system()
	if err != nil {
		return nil, err
	}
	policy.Readable = append(policy.Readable, globalConfig()...)
	payload, err := json.Marshal(request{Policy: policy, System: sys})
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, sys.self, append([]string{helperArg, string(payload), "--"}, args...)...), nil
}

type request struct {
	Policy
	System hostSystem `json:"s"`
}

// ForRepo is the policy for a git command in dir: it may write the working
// tree that holds dir and that tree's git directories. A .git file pointing
// elsewhere counts only when its git directory points back, as `git worktree
// add` leaves it; a repository cannot otherwise make git write outside it.
// Outside any repository git may only read dir.
func ForRepo(dir string) (Policy, error) {
	start, err := filepath.Abs(dir)
	if err == nil {
		start, err = filepath.EvalSymlinks(start)
	}
	if err != nil {
		return Policy{}, err
	}
	for top := start; ; {
		dotgit := filepath.Join(top, ".git")
		info, err := os.Lstat(dotgit)
		switch {
		case err == nil && info.IsDir():
			return Policy{Writable: []string{top}}, nil
		case err == nil && info.Mode().IsRegular():
			gitDir, common, err := linkedGitDirs(dotgit)
			if err != nil {
				return Policy{}, err
			}
			return Policy{Writable: []string{top, gitDir, common}}, nil
		case err == nil:
			return Policy{}, fmt.Errorf("%s is neither a directory nor a gitdir file", dotgit)
		case !errors.Is(err, os.ErrNotExist):
			return Policy{}, err
		}
		parent := filepath.Dir(top)
		if parent == top {
			return Policy{Readable: []string{start}}, nil
		}
		top = parent
	}
}

// linkedGitDirs follows a linked worktree's .git file to its git directory
// and the repository's common one, both resolved.
func linkedGitDirs(dotgit string) (gitDir, common string, err error) {
	data, err := os.ReadFile(dotgit)
	if err != nil {
		return "", "", err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	target, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return "", "", fmt.Errorf("%s has no gitdir line", dotgit)
	}
	gitDir, err = resolve(filepath.Dir(dotgit), strings.TrimSpace(target))
	if err != nil {
		return "", "", err
	}
	back, err := os.ReadFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return "", "", fmt.Errorf("%s is not a worktree of the repository it names: %w", dotgit, err)
	}
	backPath, err := resolve(gitDir, strings.TrimSpace(string(back)))
	own, ownErr := filepath.EvalSymlinks(dotgit)
	if err != nil || ownErr != nil || backPath != own {
		return "", "", fmt.Errorf("%s is not a worktree of the repository it names", dotgit)
	}
	commonRef, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return "", "", err
	}
	common, err = resolve(gitDir, strings.TrimSpace(string(commonRef)))
	if err != nil {
		return "", "", err
	}
	if filepath.Dir(gitDir) != filepath.Join(common, "worktrees") {
		return "", "", fmt.Errorf("%s does not belong to %s", gitDir, common)
	}
	return gitDir, common, nil
}

func resolve(base, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.EvalSymlinks(path)
}

// globalConfig are the places git reads its global config from, as the
// environment names them for this command.
func globalConfig() []string {
	var paths []string
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".gitconfig"), filepath.Join(home, ".config", "git"))
	}
	for _, name := range []string{"XDG_CONFIG_HOME", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		if value := os.Getenv(name); filepath.IsAbs(value) {
			if name == "XDG_CONFIG_HOME" {
				value = filepath.Join(value, "git")
			}
			paths = append(paths, value)
		}
	}
	return paths
}
