package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluonfield/acp-transport/jsonrpc"
	"github.com/wins/jaz/backend/internal/storage"
)

func TestReadTextFileLineContract(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "lines.txt")
	content := "alpha\nβeta\r\n\ngamma"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(nil, Config{}, nil)
	job := &jobState{Job: Job{ID: "test", ACPSession: "test-acp", Cwd: dir}}
	manager.addJob(job, nil)
	for _, tt := range []struct {
		name  string
		line  int
		limit int
		want  string
	}{
		{"whole file", 0, 0, content},
		{"first line", 0, 1, "alpha\n"},
		{"unicode and CRLF", 2, 1, "βeta\r\n"},
		{"blank line", 3, 1, "\n"},
		{"remaining lines", 2, 0, "βeta\r\n\ngamma"},
		{"past end", 9, 1, ""},
		{"limit past end", 4, 99, "gamma"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, rpcErr := manager.handleJSONRPC(context.Background(), jsonrpc.Request{
				Method: "fs/read_text_file",
				Params: mustJSON(t, map[string]any{
					"sessionId": job.ACPSession,
					"path":      file,
					"line":      tt.line,
					"limit":     tt.limit,
				}),
			})
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			var response struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatal(err)
			}
			if response.Content != tt.want {
				t.Fatalf("content = %q, want %q", response.Content, tt.want)
			}
		})
	}
}

func TestFilesystemSessionRoots(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "jaz")
	memory := filepath.Join(base, "memory")
	cwd := filepath.Join(base, "worktree")
	project := filepath.Join(base, "project")
	manager := NewManager(nil, Config{Root: root, MemoryRoot: memory}, nil)
	job := newIdleJob(storage.Session{
		ID: "test",
		RuntimeRef: &storage.RuntimeRef{
			Cwd:         cwd,
			ProjectPath: project,
		},
	}, AgentKimi, "test-acp", cwd, ModeState{})
	manager.addJob(job, nil)

	for _, tt := range []struct {
		name string
		path string
		disk string
	}{
		{"relative working directory path", "file.txt", filepath.Join(cwd, "file.txt")},
		{"absolute working directory path", filepath.Join(cwd, "absolute.txt"), filepath.Join(cwd, "absolute.txt")},
		{"worktree source repository", filepath.Join(project, "file.txt"), filepath.Join(project, "file.txt")},
		{"global skill", filepath.Join(root, "skills", "jazmem", "SKILL.md"), filepath.Join(root, "skills", "jazmem", "SKILL.md")},
		{"memory page", filepath.Join(memory, "daily", "2026-11-15.md"), filepath.Join(memory, "daily", "2026-11-15.md")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.MkdirAll(filepath.Dir(tt.disk), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tt.disk, []byte("before"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, rpcErr := manager.handleJSONRPC(context.Background(), jsonrpc.Request{
				Method: "fs/read_text_file",
				Params: mustJSON(t, map[string]any{"sessionId": job.ACPSession, "path": tt.path}),
			})
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			_, rpcErr = manager.handleJSONRPC(context.Background(), jsonrpc.Request{
				Method: "fs/write_text_file",
				Params: mustJSON(t, map[string]any{"sessionId": job.ACPSession, "path": tt.path, "content": "after"}),
			})
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			data, err := os.ReadFile(tt.disk)
			if err != nil || string(data) != "after" {
				t.Fatalf("content = %q, %v", data, err)
			}
		})
	}

	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"fs/read_text_file", "fs/write_text_file"} {
		_, rpcErr := manager.handleJSONRPC(context.Background(), jsonrpc.Request{
			Method: method,
			Params: mustJSON(t, map[string]any{"sessionId": job.ACPSession, "path": outside, "content": "after"}),
		})
		if rpcErr == nil {
			t.Fatalf("%s accepted path outside session roots", method)
		}
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "before" {
		t.Fatalf("outside content = %q, %v", data, err)
	}
}
