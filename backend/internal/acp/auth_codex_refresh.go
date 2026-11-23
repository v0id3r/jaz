package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Codex owns token refresh and credential persistence: account/read refreshes an access token that is due.
// Forcing a refresh would rotate the login every Codex process on this home shares, so a 401 on a token
// that is not due ends in a new sign-in. No thread or turn is created.
func (m *Manager) RefreshCodexOAuth(ctx context.Context, cfg AgentConfig, home string) error {
	_, err := m.codexAccountRPC(ctx, cfg, home, "account/read", map[string]bool{"refreshToken": false})
	if err != nil {
		return fmt.Errorf("codex could not refresh your OpenAI account sign-in; sign in again in Agents → Codex: %w", err)
	}
	return nil
}

func (m *Manager) codexAccountRPC(ctx context.Context, cfg AgentConfig, home, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	executable := ""
	if cfg.ManagedAdapter != "" && m.cfg.Adapters != nil {
		launch, err := m.cfg.Adapters.ResolveAdapter(ctx, cfg.ManagedAdapter)
		if err != nil {
			return nil, fmt.Errorf("prepare Codex app-server: %w", err)
		}
		executable = launch.Env["CODEX_PATH"]
	}
	if executable == "" {
		var err error
		executable, err = resolveLoginExecutable(loginBinDirs(cfg), "codex")
		if err != nil {
			return nil, fmt.Errorf("Codex CLI is unavailable")
		}
	}
	command := exec.CommandContext(ctx, executable, "app-server")
	command.Dir = home
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "OPENAI_API_KEY", "OPENAI_APIKEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CODEX_HOME":
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "CODEX_HOME="+home)
	input, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = input.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	encoder := json.NewEncoder(input)
	decoder := json.NewDecoder(output)
	call := func(id int, method string, params any) (json.RawMessage, error) {
		if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return nil, err
		}
		for {
			var reply struct {
				ID     int             `json:"id"`
				Error  json.RawMessage `json:"error"`
				Result json.RawMessage `json:"result"`
			}
			if err := decoder.Decode(&reply); err != nil {
				return nil, fmt.Errorf("Codex app-server closed before replying")
			}
			if reply.ID != id {
				continue
			}
			if len(reply.Error) != 0 && string(reply.Error) != "null" {
				return nil, fmt.Errorf("Codex app-server rejected %s", method)
			}
			return reply.Result, nil
		}
	}
	if _, err := call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "jaz", "version": "1.0"}}); err != nil {
		return nil, err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	return call(2, method, params)
}
