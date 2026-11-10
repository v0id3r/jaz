//go:build linux && (amd64 || arm64)

package gitjail

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func jailed(t *testing.T, dir string, policy Policy, args ...string) string {
	t.Helper()
	repo, err := ForRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	policy.Writable = append(policy.Writable, repo.Writable...)
	cmd, err := Command(context.Background(), policy, append([]string{"-C", dir}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func repository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	run(t, dir, "config", "user.email", "x@x")
	run(t, dir, "config", "user.name", "x")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "f")
	run(t, dir, "commit", "-qm", "init")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A repository's config cannot make jailed git run anything but git.
func TestRepositoryCommandsDoNotRun(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	touch, err := exec.LookPath("touch")
	if err != nil {
		t.Skip("no touch")
	}
	for name, tc := range map[string]struct {
		config [][2]string
		attrs  string
		args   []string
	}{
		"fsmonitor":      {[][2]string{{"core.fsmonitor", "sh -c 'touch " + marker + "'"}}, "", []string{"status", "--short"}},
		"clean filter":   {[][2]string{{"filter.x.clean", "sh -c 'touch " + marker + "; cat'"}}, "f filter=x\n", []string{"diff"}},
		"config hook":    {[][2]string{{"hook.p.event", "pre-commit"}, {"hook.p.command", touch + " " + marker}}, "", []string{"commit", "-qam", "t"}},
		"binary in repo": {[][2]string{{"core.fsmonitor", "REPO/evil"}}, "", []string{"status", "--short"}},
	} {
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(marker)
			dir := repository(t)
			evil := filepath.Join(dir, "evil")
			data, err := os.ReadFile(touch)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(evil, data, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, kv := range tc.config {
				run(t, dir, "config", kv[0], strings.ReplaceAll(kv[1], "REPO", dir))
			}
			if tc.attrs != "" {
				if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte(tc.attrs), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out := jailed(t, dir, Policy{}, tc.args...)
			// evil is touch: run by fsmonitor with "2 <token>" it would create "2".
			for _, path := range []string{marker, filepath.Join(dir, "2")} {
				if _, err := os.Stat(path); err == nil {
					t.Fatalf("the repository ran a command (%s); git said:\n%s", path, out)
				}
			}
		})
	}
}

// Jailed git still does its own work.
func TestJailedGitWorks(t *testing.T) {
	dir := repository(t)
	if out := jailed(t, dir, Policy{}, "status", "--short"); !strings.Contains(out, " M f") {
		t.Fatalf("status = %q", out)
	}
	jailed(t, dir, Policy{}, "commit", "-qam", "change")
	if out := jailed(t, dir, Policy{}, "log", "--oneline"); !strings.Contains(out, "change") {
		t.Fatalf("log = %q", out)
	}
}

// core.worktree cannot point jailed git at another directory.
func TestRepositoryCannotReadElsewhere(t *testing.T) {
	dir := repository(t)
	secret := t.TempDir()
	if err := os.WriteFile(filepath.Join(secret, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "config", "core.worktree", secret)
	if out := jailed(t, dir, Policy{}, "status", "--short", "--untracked-files=all"); strings.Contains(out, "auth.json") {
		t.Fatalf("jailed git listed a file outside the repository:\n%s", out)
	}
}

// A .git file may name another git directory only as a linked worktree.
func TestForRepoFollowsOnlyLinkedWorktrees(t *testing.T) {
	main := repository(t)
	linked := filepath.Join(t.TempDir(), "wt")
	run(t, main, "worktree", "add", "-q", linked)
	policy, err := ForRepo(linked)
	if err != nil {
		t.Fatal(err)
	}
	realMain, _ := filepath.EvalSymlinks(main)
	if len(policy.Writable) != 3 || policy.Writable[2] != filepath.Join(realMain, ".git") {
		t.Fatalf("linked worktree policy = %v", policy.Writable)
	}

	forged := t.TempDir()
	if err := os.WriteFile(filepath.Join(forged, ".git"), []byte("gitdir: "+filepath.Join(main, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ForRepo(forged); err == nil {
		t.Fatal("a .git file pointing at another repository was followed")
	}
	if err := os.WriteFile(filepath.Join(forged, ".git"), []byte("gitdir: "+filepath.Join(main, ".git", "worktrees", "wt")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ForRepo(forged); err == nil {
		t.Fatal("a .git file borrowing another worktree's git directory was followed")
	}
}

// Without Network jailed git opens no connections.
func TestNetworkOnlyWhenAllowed(t *testing.T) {
	dir := repository(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	connected := make(chan struct{}, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			connected <- struct{}{}
			conn.Close()
		}
	}()
	url := "http://" + ln.Addr().String() + "/repo.git"
	jailed(t, dir, Policy{}, "ls-remote", url)
	select {
	case <-connected:
		t.Fatal("jailed git connected without Network")
	case <-time.After(200 * time.Millisecond):
	}
	jailed(t, dir, Policy{Network: true}, "ls-remote", url)
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("jailed git did not connect with Network")
	}
}
