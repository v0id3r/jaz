package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wins/jaz/backend/internal/modelcatalog"
)

func codexFastModeModels(cfg AgentConfig, root string) []string {
	auth := resolveCodexAuth(cfg.Auth, cfg, root, nil, nil)
	raw, err := os.ReadFile(filepath.Join(auth.Config.Path, "models_cache.json"))
	if err != nil {
		return nil
	}
	var cache struct {
		Models []struct {
			Slug                 string   `json:"slug"`
			AdditionalSpeedTiers []string `json:"additional_speed_tiers"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &cache) != nil {
		return nil
	}
	var models []string
	for _, model := range cache.Models {
		if model.Slug != "" && slices.Contains(model.AdditionalSpeedTiers, "fast") {
			models = append(models, model.Slug)
		}
	}
	return models
}

// resolveCodexCustomProviderContextWindow is the context window of a managed
// Codex's model on a custom provider, taken from the model catalog and
// carried to the agent through Codex's own model_context_window config key.
// A native or agent-owned provider needs no record.
func (m *Manager) resolveCodexCustomProviderContextWindow(name string, cfg AgentConfig) (int, error) {
	providerID := strings.TrimSpace(cfg.ModelProvider)
	modelID := strings.TrimSpace(cfg.Model)
	if CanonicalAgentName(name) != AgentCodex || !cfg.UsesProvider() || agentOwnsModelMetadata(name, providerID) {
		return 0, nil
	}
	if modelID == "" {
		return 0, fmt.Errorf("codex provider %q requires an explicit model", providerID)
	}
	if m.cfg.ModelCatalog == nil {
		return 0, fmt.Errorf("codex provider %q model %q requires model metadata", providerID, modelID)
	}
	models, err := (ModelCapabilities{Catalog: m.cfg.ModelCatalog}).ProviderModels(AgentCodex, providerID)
	if err != nil {
		return 0, fmt.Errorf("resolve Codex context window for provider %q model %q: %w", providerID, modelID, err)
	}
	model, ok := findCapabilityModel(models, modelID)
	if !ok {
		return 0, fmt.Errorf("codex provider %q has no metadata for model %q", providerID, modelID)
	}
	if model.ContextLength <= 0 || len(model.InputModalities) == 0 || model.Reasoning.Status != modelcatalog.ReasoningReady {
		return 0, fmt.Errorf("codex provider %q has incomplete metadata for model %q", providerID, modelID)
	}
	return model.ContextLength, nil
}
