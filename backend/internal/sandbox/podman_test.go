package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRunArgsMapsMountsAndKeepsSecretsOutOfArgv(t *testing.T) {
	data, workspaces := t.TempDir(), t.TempDir()
	for _, dir := range []string{filepath.Join(data, "acp", "claude"), filepath.Join(data, "tools"), filepath.Join(workspaces, "repo")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := RunnerConfig{
		Image: "jaz:test", Network: "sessions", User: "10001:999", Home: "/var/lib/jaz-agent",
		Mounts: []PathMapping{
			{Container: "/var/lib/jaz", Host: data},
			{Container: "/var/lib/jaz/workspaces/work", Host: workspaces},
		},
		ReadOnly: []string{"/var/lib/jaz/tools"},
		Writable: []string{"/var/lib/jaz/workspaces", "/var/lib/jaz/acp"},
	}
	args, env, err := cfg.runArgs("jaz-x", Request{
		Args:      []string{"/usr/bin/agent", "--acp"},
		Env:       []string{"ANTHROPIC_API_KEY=secret", "PATH=/agent/bin", "HOME=/var/lib/jaz"},
		Dir:       "/var/lib/jaz/workspaces/work/repo",
		ReadOnly:  []string{"/usr/bin"},
		ReadWrite: []string{"/var/lib/jaz/acp/claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--volume " + filepath.Join(workspaces, "repo") + ":/var/lib/jaz/workspaces/work/repo:idmap",
		"--volume " + filepath.Join(data, "acp", "claude") + ":/var/lib/jaz/acp/claude:idmap",
		"--volume " + filepath.Join(data, "tools") + ":/var/lib/jaz/tools:idmap,ro",
		"--userns=auto --cap-drop=all --security-opt=no-new-privileges",
		"--env HOME=/var/lib/jaz-agent --env ANTHROPIC_API_KEY --env PATH=/agent/bin --entrypoint",
		`--entrypoint ["/usr/bin/agent","--acp"] jaz:test`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "secret") || strings.Contains(joined, "HOME=/var/lib/jaz ") || strings.Contains(joined, ":/usr/bin") {
		t.Fatalf("args leak a secret or mount an image path:\n%s", joined)
	}
	if !slices.Contains(env, "ANTHROPIC_API_KEY=secret") || slices.Contains(env, "PATH=/agent/bin") {
		t.Fatalf("podman env = %v", env)
	}
}

func TestRunArgsRejectsSymlinkOutOfWritableRoots(t *testing.T) {
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(data, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(data, "private"), filepath.Join(data, "work", "escape")); err != nil {
		t.Fatal(err)
	}
	cfg := RunnerConfig{Image: "jaz:test", Network: "sessions", Mounts: []PathMapping{{Container: "/jaz", Host: data}}, Writable: []string{"/jaz/work"}}
	_, _, err := cfg.runArgs("jaz-x", Request{Args: []string{"agent"}, Dir: "/jaz/work/escape"})
	if err == nil || !strings.Contains(err.Error(), "resolves outside") {
		t.Fatalf("err = %v, want a symlink escape rejection", err)
	}
}
