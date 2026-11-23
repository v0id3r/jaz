package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gluonfield/acp-transport/jsonrpc"
)

func TestClaudeACPUpdatesAccountSubscription(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "acp", "claude")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	writeAccount := func(id string) {
		t.Helper()
		if err := os.WriteFile(claudeAuthPath(profile), []byte(`{"oauthAccount":{"accountUuid":"`+id+`"}}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeAccount("first")
	cfg := AgentConfig{Auth: AgentAuthConfig{Mode: AuthModeJazProfile}}
	m := NewManager(nil, Config{Root: root, Agents: AgentCatalog{AgentClaude: cfg}}, nil)
	state := newConnectionState()
	state.subscription = m.claudeSubscriptionAccount(AgentClaude, cfg, m.probeEnv(AgentClaude, cfg))
	handle := state.handler(m, jsonrpc.HandlerFunc(m.handleJSONRPC))

	wire, err := os.Open("testdata/claude-prompt-suggestions/before-restart.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer wire.Close()
	lines := bufio.NewScanner(wire)
	for lines.Scan() {
		var note sessionNotification
		if json.Unmarshal(lines.Bytes(), &note) == nil && len(note.Update) > 0 {
			if _, err := handle.HandleJSONRPC(context.Background(), jsonrpc.Request{Method: "session/update", Params: json.RawMessage(lines.Bytes())}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}

	cache := m.subscriptionCache(AgentClaude)
	cache.Lock()
	cache.loaded = time.Now()
	cache.Unlock()
	got, err := m.SubscriptionUsage(context.Background(), AgentClaude)
	if err != nil || got.Status != "ready" || len(got.Windows) != 2 {
		t.Fatalf("subscription = %+v, %v", got, err)
	}
	if got.Windows[0].ID != "five_hour" || got.Windows[0].UsedPercent != 64 ||
		got.Windows[1].ID != "seven_day" || got.Windows[1].UsedPercent != 17 {
		t.Fatalf("windows = %+v", got.Windows)
	}

	writeAccount("second")
	got, err = m.SubscriptionUsage(context.Background(), AgentClaude)
	if err != nil || got.Status != "unavailable" || len(got.Windows) != 0 {
		t.Fatalf("previous account's usage survived reconnect: %+v, %v", got, err)
	}
}
