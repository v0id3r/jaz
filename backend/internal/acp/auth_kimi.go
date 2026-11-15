package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/wins/jaz/backend/internal/processenv"
	"github.com/wins/jaz/backend/internal/runtimefiles"
	"github.com/wins/jaz/backend/internal/skills"
)

var errKimiModelNotConfigured = errors.New("kimi sign-in did not finish configuring a model; sign in again")

type kimiConfig struct {
	DefaultModel    string                    `toml:"default_model"`
	DefaultProvider string                    `toml:"default_provider"`
	ExtraSkillDirs  []string                  `toml:"extra_skill_dirs"`
	Providers       map[string]kimiProvider   `toml:"providers"`
	Models          map[string]kimiModelAlias `toml:"models"`
}

type kimiProvider struct {
	OAuth struct {
		Key string `toml:"key"`
	} `toml:"oauth"`
}

type kimiModelAlias struct {
	Provider string `toml:"provider"`
}

func resolveKimiAuth(auth AgentAuthConfig, cfg AgentConfig, root string, env map[string]string) resolvedAgentAuth {
	jaz := runtimefiles.New(root).ACPKimiHome
	existing := expandAuthPath(firstNonEmpty(existingAuthPath(auth), cfg.Env["KIMI_CODE_HOME"], env["KIMI_CODE_HOME"], os.Getenv("KIMI_CODE_HOME"), defaultHomePath(".kimi-code")))
	mode := auth.Mode
	if mode == AuthModeAuto || mode == "" {
		mode = AuthModeJazProfile
		if !kimiTokenAvailable(kimiAuthPath(jaz)) && kimiTokenAvailable(kimiAuthPath(existing)) {
			mode = AuthModeExistingCLI
		}
	}
	home := jaz
	if mode == AuthModeExistingCLI {
		home = existing
	}
	storagePath := kimiAuthPath(home)
	source := mode
	status := resolvedAgentAuth{
		Config:      AgentAuthConfig{Mode: mode, Path: home},
		StoragePath: storagePath,
		Source:      source,
	}
	if kimiTokenAvailable(storagePath) {
		status.markAuthenticated("oauth_json", AuthKindOAuth)
	} else {
		status.Reason = "Kimi login at " + storagePath
	}
	return status
}

func kimiTokenAvailable(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	return json.Unmarshal(data, &token) == nil && token.AccessToken != ""
}

// kimiAuthPath follows the token slot Kimi recorded for its managed login;
// a kimi.ai login uses a scoped slot instead of the default kimi-code.
func kimiAuthPath(home string) string {
	cfg, _ := readKimiConfig(home)
	key := firstNonEmpty(cfg.Providers["managed:kimi-code"].OAuth.Key, "oauth/kimi-code")
	return filepath.Join(home, "credentials", filepath.Base(strings.TrimPrefix(key, "oauth/"))+".json")
}

func readKimiConfig(home string) (kimiConfig, bool) {
	var cfg kimiConfig
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil || toml.Unmarshal(data, &cfg) != nil {
		return kimiConfig{}, false
	}
	return cfg, true
}

func kimiModelConfigReady(home string) error {
	cfg, ok := readKimiConfig(home)
	if !ok {
		return errKimiModelNotConfigured
	}
	modelID := strings.TrimSpace(cfg.DefaultModel)
	model, ok := cfg.Models[modelID]
	if modelID == "" || !ok {
		return errKimiModelNotConfigured
	}
	providerID := strings.TrimSpace(model.Provider)
	if providerID == "" {
		providerID = strings.TrimSpace(cfg.DefaultProvider)
	}
	if _, ok := cfg.Providers[providerID]; providerID == "" || !ok {
		return errKimiModelNotConfigured
	}
	return nil
}

func (m *Manager) prepareKimiProcessEnv(root string, cfg AgentConfig, env map[string]string, cwd string, prepare bool) error {
	processenv.PreserveHost(env,
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"LANG",
		"LC_ALL",
		"LC_CTYPE",
		"LOGNAME",
		"NO_PROXY",
		"SHELL",
		"SSH_AUTH_SOCK",
		"USER",
	)
	auth := resolveAgentAuthWithProviders(AgentKimi, cfg, root, env, m.providers())
	env["KIMI_CODE_HOME"] = auth.Config.Path
	// Jaztools calls (memory search workers, thread waits) legitimately outrun
	// Kimi's 60s default MCP tool timeout; keep the raise overridable via env.
	if env["KIMI_MCP_TOOL_TIMEOUT_MS"] == "" {
		env["KIMI_MCP_TOOL_TIMEOUT_MS"] = "300000"
	}
	if !prepare {
		return nil
	}
	if auth.Authenticated {
		if err := kimiModelConfigReady(auth.Config.Path); err != nil {
			return err
		}
	}
	catalog, err := skills.LoadForWorkspace(root, cwd)
	if err != nil || len(catalog.Skills) == 0 {
		return err
	}
	return ensureKimiSkillDirs(auth.Config.Path, skills.RootsForWorkspace(root, cwd))
}

func ensureKimiSkillDirs(home string, dirs []string) error {
	path := filepath.Join(home, "config.toml")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read kimi config %s: %w", path, err)
	}
	var cfg kimiConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse kimi config %s: %w", path, err)
	}
	merged := append([]string(nil), cfg.ExtraSkillDirs...)
	for _, dir := range dirs {
		if !slices.Contains(merged, dir) {
			merged = append(merged, dir)
		}
	}
	if slices.Equal(merged, cfg.ExtraSkillDirs) {
		return nil
	}
	updated, err := setTopLevelTOMLStringArray(data, "extra_skill_dirs", merged)
	if err != nil {
		return fmt.Errorf("update kimi config %s: %w", path, err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("prepare kimi profile %s: %w", home, err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		return fmt.Errorf("write kimi config %s: %w", path, err)
	}
	return nil
}

func setTopLevelTOMLStringArray(data []byte, key string, values []string) ([]byte, error) {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = strconv.Quote(value)
	}
	replacement := key + " = [" + strings.Join(quoted, ", ") + "]"
	lines := strings.SplitAfter(string(data), "\n")
	for i, existing := range lines {
		trimmed := strings.TrimSpace(existing)
		if strings.HasPrefix(trimmed, "[") {
			break
		}
		if topLevelTOMLKey(existing) != key {
			continue
		}
		for end := i + 1; end <= len(lines); end++ {
			var decoded map[string][]string
			if err := toml.Unmarshal([]byte(strings.Join(lines[i:end], "")), &decoded); err != nil {
				continue
			}
			ending := ""
			if strings.HasSuffix(lines[end-1], "\n") {
				ending = "\n"
			}
			lines[i] = replacement + ending
			return []byte(strings.Join(append(lines[:i+1], lines[end:]...), "")), nil
		}
		return nil, fmt.Errorf("invalid %s value", key)
	}
	if len(data) == 0 {
		return []byte(replacement + "\n"), nil
	}
	return []byte(replacement + "\n\n" + string(data)), nil
}
