package acp

import (
	"errors"
	"testing"

	"github.com/wins/jaz/backend/internal/sandbox"
	"github.com/wins/jaz/backend/internal/storage"
	sqlitestore "github.com/wins/jaz/backend/internal/storage/sqlite"
)

func TestSandboxedThreadsStayInTheSandbox(t *testing.T) {
	store, err := sqlitestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for id, cwd := range map[string]string{"sb": "/sb/a", "sb2": "/sb/b", "admin": "/var/lib/jaz/admin"} {
		if err := store.SaveSession(storage.Session{ID: id, RuntimeRef: &storage.RuntimeRef{Cwd: cwd}}); err != nil {
			t.Fatal(err)
		}
	}
	launcher, err := sandbox.New(sandbox.Config{Runtime: "runner", Socket: "/run/runner.sock", Paths: []string{"/sb"}})
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, Config{Launcher: launcher}, nil)

	for _, tc := range []struct {
		from, target string
		allowed      bool
	}{
		{"sb", "sb2", true},
		{"sb", "admin", false},
		{"sb", "missing", false},
		{"admin", "sb", true},
		{"admin", "admin", true},
		{"", "admin", true},
	} {
		if err := m.confine(tc.from, tc.target); (err == nil) != tc.allowed {
			t.Errorf("%s -> %s: err %v, allowed %t", tc.from, tc.target, err, tc.allowed)
		}
	}

	req, err := m.confineSpawn(SpawnRequest{ParentID: "sb"}, AgentConfig{})
	if err != nil || req.Directory != "/sb/a" {
		t.Fatalf("child of a sandboxed thread: dir %q, err %v; want the parent's directory", req.Directory, err)
	}
	for name, tc := range map[string]struct {
		req SpawnRequest
		cfg AgentConfig
	}{
		"outside":  {SpawnRequest{ParentID: "sb", Directory: "/var/lib/jaz/admin"}, AgentConfig{}},
		"relative": {SpawnRequest{ParentID: "sb", Directory: "sb/a"}, AgentConfig{}},
		"escape":   {SpawnRequest{ParentID: "sb", Directory: "/sb/../var/lib/jaz"}, AgentConfig{}},
		"worktree": {SpawnRequest{ParentID: "sb", Directory: "/sb/a", Worktree: true}, AgentConfig{}},
		"home":     {SpawnRequest{ParentID: "sb", Home: "/var/lib/jaz/chats"}, AgentConfig{}},
		"local":    {SpawnRequest{ParentID: "sb", Directory: "/sb/a"}, AgentConfig{Local: true}},
	} {
		if _, err := m.confineSpawn(tc.req, tc.cfg); err == nil {
			t.Errorf("%s: a sandboxed thread started a thread outside the sandbox", name)
		}
	}
	if req, err := m.confineSpawn(SpawnRequest{ParentID: "admin", Directory: "/var/lib/jaz/other", Worktree: true}, AgentConfig{Local: true}); err != nil || req.Directory != "/var/lib/jaz/other" {
		t.Fatalf("a thread outside the sandbox was confined: %v", err)
	}
	if !errors.Is(m.confine("sb", "admin"), errConfined) {
		t.Fatal("confine error is not errConfined")
	}
}
