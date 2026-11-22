package sandbox

import "testing"

func TestScopedSandboxesOnlyProcessesUnderItsPaths(t *testing.T) {
	launcher, err := New(Config{Runtime: "runner", Socket: "/run/runner.sock", Paths: []string{" /work/canary "}})
	if err != nil {
		t.Fatal(err)
	}
	for dir, sandboxed := range map[string]bool{
		"/work/canary":       true,
		"/work/canary/repo":  true,
		"/work/canary-other": false,
		"/work":              false,
	} {
		got, _, err := launcher.Command(Spec{Path: "/bin/agent", Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if (got.Path != "/bin/agent") != sandboxed {
			t.Fatalf("dir %s: launched %s %v, sandboxed want %v", dir, got.Path, got.Args, sandboxed)
		}
	}
}
