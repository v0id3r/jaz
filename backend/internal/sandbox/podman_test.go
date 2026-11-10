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
	}, "")
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
	_, _, err := cfg.runArgs("jaz-x", Request{Args: []string{"agent"}, Dir: "/jaz/work/escape"}, "")
	if err == nil || !strings.Contains(err.Error(), "resolves outside") {
		t.Fatalf("err = %v, want a symlink escape rejection", err)
	}
}

func TestRunArgsGiveTheSessionItsIdentity(t *testing.T) {
	data := t.TempDir()
	cfg := RunnerConfig{
		Image: "jaz:test", Network: "sessions",
		Mounts:   []PathMapping{{Container: "/var/lib/jaz", Host: data}},
		Writable: []string{"/var/lib/jaz"},
		Identity: RunnerIdentity{Server: "/run/spire-server/api.sock", WorkloadAPI: "/run/spire-agent/public"},
	}
	args, _, err := cfg.runArgs("jaz-x", Request{Args: []string{"agent"}, Dir: "/var/lib/jaz", Session: "20261110T030025-2100cd54"}, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--label jaz.session=20261110T030025-2100cd54",
		"--volume /run/spire-agent/public:/run/spire/agent:ro",
		"--env SPIFFE_ENDPOINT_SOCKET=unix:///run/spire/agent/api.sock",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args lack %q:\n%s", want, joined)
		}
	}
	for _, session := range []string{"../x", "a b", "a=b", ""} {
		req := Request{Args: []string{"agent"}, Dir: "/var/lib/jaz", Session: session}
		args, _, err := cfg.runArgs("jaz-x", req, "")
		if session != "" && err == nil {
			t.Fatalf("session %q accepted", session)
		}
		if session == "" && (err != nil || strings.Contains(strings.Join(args, " "), "jaz.session")) {
			t.Fatalf("no session: err %v, args %v", err, args)
		}
	}
}

func TestSessionsGetTheirOwnHome(t *testing.T) {
	data, homes := t.TempDir(), filepath.Join(t.TempDir(), "homes")
	seed := filepath.Join(data, "agent-home")
	for name, content := range map[string]string{".bashrc": "rc", ".gitconfig": "shared", ".cache/x/blob": "cache", ".config/go/env": "GOFLAGS="} {
		path := filepath.Join(seed, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/var/lib/jaz/.local/share/mise", filepath.Join(seed, "mise")); err != nil {
		t.Fatal(err)
	}
	cfg := RunnerConfig{
		Image: "jaz:test", Network: "sessions", Home: "/var/lib/jaz-agent",
		Homes: homes, HomeSkip: []string{".cache"}, HomeShared: []string{".gitconfig", ".missing"},
		Mounts: []PathMapping{
			{Container: "/var/lib/jaz", Host: data},
			{Container: "/var/lib/jaz-agent", Host: seed},
		},
		ReadWrite: []string{"/var/lib/jaz-agent"},
		Writable:  []string{"/var/lib/jaz"},
	}

	a, _, err := cfg.sessionHome("jaz-a", "session-a")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := cfg.sessionHome("jaz-b", "session-b")
	if err != nil || a == b {
		t.Fatalf("homes %q and %q, err %v", a, b, err)
	}
	if err := os.WriteFile(filepath.Join(a, ".bashrc"), []byte("poisoned"), 0o640); err != nil {
		t.Fatal(err)
	}
	if again, _, err := cfg.sessionHome("jaz-c", "session-a"); err != nil || again != a {
		t.Fatalf("relaunch got %q, err %v; want the session's home %q", again, err, a)
	}
	for path, want := range map[string]string{
		filepath.Join(b, ".bashrc"):        "rc",
		filepath.Join(seed, ".bashrc"):     "rc",
		filepath.Join(a, ".config/go/env"): "GOFLAGS=",
	} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(a, ".cache")); err == nil {
		t.Fatal("seed copied the cache")
	}
	if got, err := os.ReadFile(filepath.Join(a, ".gitconfig")); err != nil || len(got) != 0 {
		t.Fatalf("shared .gitconfig placeholder = %q, %v; want an empty mount point", got, err)
	}
	if link, err := os.Readlink(filepath.Join(a, "mise")); err != nil || link != "/var/lib/jaz/.local/share/mise" {
		t.Fatalf("symlink copied as %q, %v", link, err)
	}

	args, _, err := cfg.runArgs("jaz-a", Request{Args: []string{"agent"}, Dir: "/var/lib/jaz", Session: "session-a"}, a)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--volume " + a + ":/var/lib/jaz-agent:idmap",
		"--volume " + filepath.Join(seed, ".gitconfig") + ":/var/lib/jaz-agent/.gitconfig:idmap,ro",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, seed+":") || strings.Contains(joined, ".missing") {
		t.Fatalf("args mount the shared home or a missing entry:\n%s", joined)
	}

	launch, cleanup, err := cfg.sessionHome("jaz-d", "")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Lstat(launch); err == nil {
		t.Fatal("a launch without a session kept its home")
	}
	if _, _, err := cfg.sessionHome("jaz-e", "../escape"); err == nil {
		t.Fatal("invalid session id accepted")
	}
}
