package acp_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wins/jaz/backend/internal/acp"
	sqlitestore "github.com/wins/jaz/backend/internal/storage/sqlite"
)

func TestRecommendedValueDeclaredOnlyWhereConsumed(t *testing.T) {
	for _, tc := range []struct {
		agent   string
		declare bool
	}{
		{acp.AgentCodex, true},
		{acp.AgentClaude, false},
		{acp.AgentKimi, false},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			root := t.TempDir()
			requestLog := filepath.Join(root, "requests.jsonl")
			store, err := sqlitestore.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			manager := newFakeNamedAgentManagerWithOptions(t, store, root, tc.agent, map[string]string{
				"JAZ_FAKE_ACP_REQUEST_LOG": requestLog,
			}, "", "")
			t.Cleanup(manager.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			if _, err := manager.Spawn(ctx, acp.SpawnRequest{ACPAgent: tc.agent, Slug: tc.agent}); err != nil {
				t.Fatal(err)
			}
			var caps []string
			for _, request := range readFakeACPRequests(t, requestLog) {
				if request.Method != "initialize" {
					continue
				}
				var params struct {
					ClientCapabilities struct {
						Meta struct {
							Jetbrains struct {
								Air struct {
									Capabilities []string `json:"capabilities"`
								} `json:"air"`
							} `json:"jetbrains"`
						} `json:"_meta"`
					} `json:"clientCapabilities"`
				}
				if err := json.Unmarshal(request.Params, &params); err != nil {
					t.Fatal(err)
				}
				caps = params.ClientCapabilities.Meta.Jetbrains.Air.Capabilities
			}
			hasRecommended := slices.Contains(caps, "recommendedValue")
			if hasRecommended != tc.declare {
				t.Errorf("initialize capabilities %v, recommendedValue declared=%v, want %v", caps, hasRecommended, tc.declare)
			}
			if !slices.Contains(caps, "sessionFailure") {
				t.Errorf("initialize capabilities %v, sessionFailure always declared", caps)
			}
		})
	}
}
