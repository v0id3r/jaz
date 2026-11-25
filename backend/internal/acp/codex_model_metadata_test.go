package acp

import (
	"errors"
	"testing"

	"github.com/wins/jaz/backend/internal/modelcatalog"
	"github.com/wins/jaz/backend/internal/provider"
)

type codexMetadataCatalog struct {
	models []modelcatalog.Model
	err    error
	calls  int
	lastID string
}

func (c *codexMetadataCatalog) ProviderModels(id string) ([]modelcatalog.Model, error) {
	c.calls++
	c.lastID = id
	return c.models, c.err
}

func (*codexMetadataCatalog) AgentModels(string) []modelcatalog.Model {
	return nil
}

func customProviderCodexConfig(providerID, model string) AgentConfig {
	return AgentConfig{
		ProviderMode:  AgentProviderModeAgentDefaults,
		ModelProvider: providerID,
		Model:         model,
	}
}

func readCodexConfigFile(t *testing.T, ref string) map[string]any {
	t.Helper()
	config, err := decodeCodexConfig(ref)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestResolveCodexCustomProviderContextWindowUsesCatalog(t *testing.T) {
	catalog := &codexMetadataCatalog{models: []modelcatalog.Model{{
		Value:           "qwen3.8-max-preview",
		Label:           "Qwen3.8 Max Preview",
		ContextLength:   1_000_000,
		InputModalities: []string{"text", "image"},
		Reasoning:       modelcatalog.Reasoning{Status: modelcatalog.ReasoningReady},
	}}}
	manager := NewManager(nil, Config{ModelCatalog: catalog}, nil)
	window, err := manager.resolveCodexCustomProviderContextWindow(AgentCodex, customProviderCodexConfig("qwen-cloud", "qwen3.8-max-preview"))
	if err != nil {
		t.Fatal(err)
	}
	if catalog.lastID != "qwen-cloud" || window != 1_000_000 {
		t.Fatalf("provider = %q, window = %d", catalog.lastID, window)
	}
}

func TestResolveCodexCustomProviderContextWindowPreservesNativeCatalog(t *testing.T) {
	for _, providerID := range []string{"", AgentCodex, provider.ProviderOpenAI, CodexProviderOpenAIAPIKey} {
		catalog := &codexMetadataCatalog{err: errors.New("must not be called")}
		manager := NewManager(nil, Config{ModelCatalog: catalog}, nil)
		window, err := manager.resolveCodexCustomProviderContextWindow(AgentCodex, customProviderCodexConfig(providerID, provider.OpenAIModelGPT6Sol))
		if err != nil || window != 0 || catalog.calls != 0 {
			t.Fatalf("provider %q: window = %d, error = %v, catalog calls = %d", providerID, window, err, catalog.calls)
		}
	}
}

func TestResolveCodexCustomProviderContextWindowRejectsIncompleteCatalog(t *testing.T) {
	manager := NewManager(nil, Config{ModelCatalog: &codexMetadataCatalog{models: []modelcatalog.Model{{Value: "unknown"}}}}, nil)
	window, err := manager.resolveCodexCustomProviderContextWindow(AgentCodex, customProviderCodexConfig("custom", "unknown"))
	if err == nil || window != 0 {
		t.Fatalf("window = %d, error = %v", window, err)
	}
}

func TestResolveCodexCustomProviderContextWindowPropagatesCatalogError(t *testing.T) {
	catalog := &codexMetadataCatalog{err: modelcatalog.ErrCatalogUnavailable}
	manager := NewManager(nil, Config{ModelCatalog: catalog}, nil)
	window, err := manager.resolveCodexCustomProviderContextWindow(AgentCodex, customProviderCodexConfig(provider.ProviderOpenRouter, "moonshotai/kimi-k3"))
	if err == nil || window != 0 {
		t.Fatalf("window = %d, error = %v", window, err)
	}
}

func TestConfigureCodexEnvWritesModelContextWindow(t *testing.T) {
	env := map[string]string{}
	err := configureCodexEnv(t.TempDir(), env, customProviderCodexConfig("custom", "qwen3.8-max-preview"), nil, 1_000_000, "")
	if err != nil {
		t.Fatal(err)
	}
	config := readCodexConfigFile(t, env["CODEX_CONFIG"])
	if window, ok := config["model_context_window"].(float64); !ok || window != 1_000_000 {
		t.Fatalf("model_context_window = %v in %v", config["model_context_window"], config)
	}
}

func TestConfigureCodexEnvOmitsModelContextWindow(t *testing.T) {
	env := map[string]string{}
	err := configureCodexEnv(t.TempDir(), env, customProviderCodexConfig("custom", "qwen3.8-max-preview"), nil, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	config := readCodexConfigFile(t, env["CODEX_CONFIG"])
	if _, ok := config["model_context_window"]; ok {
		t.Fatalf("model_context_window present in %v", config)
	}
}

func TestProcessEnvOnlySerializesProcessConfiguration(t *testing.T) {
	catalog := &codexMetadataCatalog{err: errors.New("must not be called")}
	manager := NewManager(nil, Config{ModelCatalog: catalog}, nil)
	manager.processEnv(AgentCodex, customProviderCodexConfig(provider.ProviderOpenRouter, "moonshotai/kimi-k3"))
	if catalog.calls != 0 {
		t.Fatalf("process environment performed %d catalog calls", catalog.calls)
	}
}
