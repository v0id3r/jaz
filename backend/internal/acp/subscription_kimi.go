package acp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/wins/jaz/backend/internal/processenv"
	"github.com/wins/jaz/backend/internal/subprocess"
)

func kimiSubscriptionIdentity(data []byte) [32]byte {
	var credentials struct {
		Token string `json:"access_token"`
	}
	_ = json.Unmarshal(data, &credentials)
	parts := strings.SplitN(credentials.Token, ".", 3)
	if len(parts) == 3 {
		var claims struct {
			Issuer  string `json:"iss"`
			Subject string `json:"sub"`
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err == nil && json.Unmarshal(payload, &claims) == nil && claims.Subject != "" {
			return sha256.Sum256([]byte(claims.Issuer + "\x00" + claims.Subject))
		}
	}
	return sha256.Sum256(data)
}

func (m *Manager) readKimiSubscription(ctx context.Context, cfg AgentConfig, env map[string]string) ([]SubscriptionWindow, error) {
	executable := ""
	if cfg.ManagedAdapter != "" && m.cfg.Adapters != nil {
		launch, err := m.cfg.Adapters.ResolveAdapter(ctx, cfg.ManagedAdapter)
		if err != nil {
			return nil, err
		}
		executable = launch.Command
	}
	if executable == "" {
		var err error
		executable, err = resolveLoginExecutable(loginBinDirs(cfg), "kimi")
		if err != nil {
			return nil, err
		}
	}
	dir, err := os.MkdirTemp("", "jaz-kimi-quota-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	run, stop := context.WithCancel(ctx)
	defer stop()
	command := exec.CommandContext(run, executable, "web", "--json", "--host", "127.0.0.1", "--port", "0")
	command.Dir = dir
	command.Env = processenv.List(env)
	process := subprocess.New(command)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	defer func() {
		stop()
		_ = command.Wait()
	}()
	if err := process.Started(); err != nil {
		return nil, err
	}
	var ready struct {
		Origin string `json:"origin"`
		Token  string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(stdout, 8192)).Decode(&ready); err != nil {
		return nil, err
	}
	origin, err := url.Parse(ready.Origin)
	if err != nil || origin.Scheme != "http" || origin.Hostname() != "127.0.0.1" || origin.Port() == "" || ready.Token == "" {
		return nil, errors.New("Kimi did not announce a local quota endpoint")
	}
	var response struct {
		Data struct {
			Kind  string `json:"kind"`
			Quota struct {
				Usages map[string]struct {
					UsedRatio *float64 `json:"usedRatio"`
					ResetAt   string   `json:"resetAt"`
				} `json:"usages"`
			} `json:"quota"`
		} `json:"data"`
	}
	endpoint := (&url.URL{Scheme: "http", Host: origin.Host, Path: "/api/v1/oauth/usage"}).String()
	if err := readSubscriptionJSON(ctx, endpoint, ready.Token, "", &response); err != nil {
		return nil, err
	}
	if response.Data.Kind != "ok" {
		return nil, errors.New("Kimi quota is unavailable")
	}
	var windows []SubscriptionWindow
	for _, entry := range []struct{ id, label string }{
		{"limit5h", "5 hours"}, {"limit7d", "7 days"},
		{"monthTotal", "Monthly credits"}, {"monthCode", "Monthly Code"},
	} {
		window := response.Data.Quota.Usages[entry.id]
		if window.UsedRatio != nil {
			reset, _ := time.Parse(time.RFC3339, window.ResetAt)
			windows = append(windows, SubscriptionWindow{entry.id, entry.label, *window.UsedRatio * 100, reset})
		}
	}
	return windows, nil
}

func readSubscriptionJSON(ctx context.Context, endpoint, token, beta string, data any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("User-Agent", "Mozilla/5.0")
	if beta != "" {
		request.Header.Set("anthropic-beta", beta)
	}
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if strings.HasPrefix(endpoint, "http://127.0.0.1:") {
		client.Transport = subscriptionLocalTransport
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("subscription endpoint returned HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(data)
}

var subscriptionLocalTransport = &http.Transport{}
