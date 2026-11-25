package acp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexConfigProcessHelper(t *testing.T) {
	if os.Getenv("JAZ_CODEX_CONFIG_PROBE") != "1" {
		return
	}
	raw := os.Getenv("CODEX_CONFIG")
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	// The system prompt travels session/new's standard field, not the launch config.
	if _, ok := config["developer_instructions"]; ok {
		t.Fatal("launch config carries the system prompt")
	}
	if config["model_provider"] != "openai" || config["custom"] != "preserved" {
		t.Fatal("config changed")
	}
	if _, ok := config["model"]; ok {
		t.Fatal("native model selection overridden")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("JAZ_CODEX_CONFIG_RECEIPT"), []byte("read"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// The launch config reaches the process inline, preserves the incoming
// CODEX_CONFIG, and writes nothing into CODEX_HOME.
func TestCodexConfigProcessLifecycle(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	manager := NewManager(nil, Config{Root: t.TempDir()}, nil)
	home := t.TempDir()
	auth := filepath.Join(home, "auth.json")
	if err := os.WriteFile(auth, []byte("test-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		receipt := filepath.Join(t.TempDir(), "receipt")
		env := map[string]string{"CODEX_HOME": home, "CODEX_CONFIG": `{"custom":"preserved"}`, "JAZ_CODEX_CONFIG_PROBE": "1", "JAZ_CODEX_CONFIG_RECEIPT": receipt}
		conn, stderr, err := manager.openConn(ctx, AgentCodex, AgentConfig{Command: executable, Args: []string{"-test.run=^TestCodexConfigProcessHelper$"}}, env, t.TempDir(), "")
		if err != nil {
			t.Fatal(err)
		}
		for {
			if data, err := os.ReadFile(receipt); err == nil && len(data) > 0 {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("helper did not read config:", ctx.Err())
			case <-stderr.done:
				detail, err := stderr.snapshot()
				t.Fatalf("helper exited before read: %v %s", err, detail)
			case <-time.After(10 * time.Millisecond):
			}
		}
		_ = conn.Close()
		select {
		case <-stderr.done:
		case <-time.After(10 * time.Second):
			t.Fatal("helper did not stop")
		}
	}
	data, err := os.ReadFile(auth)
	if err != nil || string(data) != "test-credential" {
		t.Fatal("auth changed")
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		t.Fatal("shared config written")
	}
}
