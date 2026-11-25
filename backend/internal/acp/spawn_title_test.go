package acp

import (
	"context"
	"testing"
	"time"

	acpschema "github.com/gluonfield/acp-transport/acp"
	"github.com/gluonfield/acp-transport/jsonrpc"
	sqlitestore "github.com/wins/jaz/backend/internal/storage/sqlite"
)

func TestSpawnTitleSurvivesRuntimeTitles(t *testing.T) {
	store, err := sqlitestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := NewManager(store, Config{}, nil)
	session, err := manager.createStoredSession(SpawnRequest{ACPAgent: AgentCodex, Title: "Проба Codex"}, AgentConfig{}, "")
	if err != nil {
		t.Fatal(err)
	}
	manager.jobsByID[session.ID] = &jobState{Job: Job{ID: session.ID, Title: session.Title, ACPAgent: AgentCodex, ACPSession: "acp-session", State: StateRunning}}
	manager.jobsByACP["acp-session"] = manager.jobsByID[session.ID]

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, rpcErr := manager.handleJSONRPC(ctx, jsonrpc.Request{
		Method: acpschema.ClientMethodSessionUpdate,
		Params: mustJSON(t, map[string]any{
			"sessionId": "acp-session",
			"update":    map[string]any{"sessionUpdate": "session_info_update", "title": "Agent title"},
		}),
	}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	loaded, err := store.LoadSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Title != "Проба Codex" {
		t.Fatalf("stored title = %q, want the spawn title", loaded.Title)
	}
}
