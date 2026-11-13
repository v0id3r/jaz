package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func configureMusePrompt(dir string, env map[string]string, prompt string) error {
	source := env["XDG_CONFIG_HOME"]
	if source == "" {
		source = filepath.Join(firstNonEmpty(env["HOME"], env["USERPROFILE"]), ".config")
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	var settings struct {
		Permissions struct {
			DefaultProfile string `json:"default_profile"`
		} `json:"permissions"`
	}
	if data, err := os.ReadFile(filepath.Join(source, "muse", "settings.json")); err == nil {
		if json.Unmarshal(data, &settings) == nil && settings.Permissions.DefaultProfile == ":auto-review" {
			return fmt.Errorf("muse's :auto-review permission profile requires its native UI; select a served-runtime profile in Muse settings")
		}
	}
	if prompt == "" {
		return nil
	}
	rules, err := os.ReadFile(filepath.Join(source, "muse", "AGENTS.md"))
	if os.IsNotExist(err) {
		rules, err = os.ReadFile(filepath.Join(source, "muse", "CLAUDE.md"))
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read Muse user rules: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(source, "muse"), 0o700); err != nil {
		return err
	}
	view, err := os.MkdirTemp(dir, "muse-config-*")
	if err != nil {
		return err
	}
	if err := linkMuseConfig(source, view, "muse"); err != nil {
		return err
	}
	target := filepath.Join(view, "muse")
	if err := os.Mkdir(target, 0o700); err != nil {
		return err
	}
	if err := linkMuseConfig(filepath.Join(source, "muse"), target, "AGENTS.md",
		"auth.json", "trust.json", "settings.json", ".auth.json.lock", ".trust.json.lock", ".settings.json.lock"); err != nil {
		return err
	}
	if len(rules) > 0 {
		prompt = string(rules) + "\n\n" + prompt
	}
	if err := os.WriteFile(filepath.Join(target, "AGENTS.md"), []byte(prompt), 0o600); err != nil {
		return err
	}
	env["XDG_CONFIG_HOME"] = view
	return nil
}

func linkMuseConfig(source, target, exclude string, required ...string) error {
	entries, err := os.ReadDir(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, name := range required {
		names[name] = true
	}
	for _, entry := range entries {
		names[entry.Name()] = true
	}
	delete(names, exclude)
	for name := range names {
		if err := os.Symlink(filepath.Join(source, name), filepath.Join(target, name)); err != nil {
			return fmt.Errorf("link Muse configuration: %w", err)
		}
	}
	return nil
}
