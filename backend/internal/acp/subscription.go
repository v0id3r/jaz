package acp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wins/jaz/backend/internal/provider"
)

const subscriptionRefreshInterval = time.Minute

type SubscriptionUsage struct {
	Status     string               `json:"status"`
	ObservedAt time.Time            `json:"observed_at,omitzero"`
	Windows    []SubscriptionWindow `json:"windows,omitempty"`
}

type SubscriptionWindow struct {
	ID          string    `json:"id"`
	Label       string    `json:"label"`
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
}

type subscriptionAccount struct {
	agent   string
	profile string
	id      [32]byte
}

type subscriptionCache struct {
	sync.Mutex
	loaded, observed time.Time
	account          subscriptionAccount
	windows          []SubscriptionWindow
	failed           bool
	pending          chan struct{}
}

func (m *Manager) SubscriptionUsage(ctx context.Context, agent string) (SubscriptionUsage, error) {
	cfg, ok, err := m.metadataAgentConfig(agent)
	if err != nil {
		return SubscriptionUsage{}, err
	}
	if !ok || cfg.URL != "" || cfg.Local {
		return SubscriptionUsage{Status: "unavailable"}, nil
	}
	if agent == AgentCodex {
		cfg.ModelProvider = provider.ProviderOpenAI
	}
	env := m.probeEnv(agent, cfg)
	auth := resolveAgentAuthWithProviders(agent, cfg, m.cfg.Root, env, m.providers())
	if !auth.Authenticated || auth.Kind != AuthKindOAuth {
		return SubscriptionUsage{Status: "not_connected"}, nil
	}
	account, token := subscriptionProfile(agent, env, auth)
	if account.agent == "" {
		return SubscriptionUsage{Status: "unavailable"}, nil
	}
	cache := m.subscriptionCache(agent)
	cache.Lock()
	if cache.account != account {
		cache.account, cache.windows = account, nil
		cache.loaded, cache.observed = time.Time{}, time.Time{}
		cache.failed, cache.pending = false, nil
	}
	if pending := cache.pending; pending != nil {
		cache.Unlock()
		select {
		case <-pending:
		case <-ctx.Done():
			return SubscriptionUsage{}, ctx.Err()
		}
		cache.Lock()
		defer cache.Unlock()
		if cache.account != account {
			return SubscriptionUsage{Status: "unavailable"}, nil
		}
		return cache.snapshot(), nil
	}
	if time.Since(cache.loaded) < subscriptionRefreshInterval {
		usage := cache.snapshot()
		cache.Unlock()
		return usage, nil
	}
	pending := make(chan struct{})
	cache.pending = pending
	started := time.Now()
	cache.Unlock()

	probe, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	windows, err := m.readSubscription(probe, cfg, env, account, token)
	cancel()

	cache.Lock()
	defer cache.Unlock()
	close(pending)
	if cache.pending != pending {
		return SubscriptionUsage{Status: "unavailable"}, nil
	}
	cache.pending = nil
	cache.loaded = time.Now().UTC()
	cache.failed = err != nil
	if err == nil {
		if cache.observed.After(started) {
			for _, window := range cache.windows {
				if window.ID == "five_hour" || window.ID == "seven_day" {
					windows = replaceSubscriptionWindow(windows, window)
				}
			}
		}
		cache.windows, cache.observed = windows, cache.loaded
	}
	return cache.snapshot(), nil
}

func (m *Manager) subscriptionCache(agent string) *subscriptionCache {
	if cache, ok := m.subscriptions.Load(agent); ok {
		return cache.(*subscriptionCache)
	}
	cache, _ := m.subscriptions.LoadOrStore(agent, new(subscriptionCache))
	return cache.(*subscriptionCache)
}

func (c *subscriptionCache) snapshot() SubscriptionUsage {
	status := "ready"
	if len(c.windows) == 0 {
		status = "unavailable"
	} else if c.failed || time.Since(c.observed) >= 2*subscriptionRefreshInterval {
		status = "stale"
	}
	return SubscriptionUsage{Status: status, ObservedAt: c.observed, Windows: c.windows}
}

func subscriptionProfile(agent string, env map[string]string, auth resolvedAgentAuth) (subscriptionAccount, string) {
	profile := auth.Config.Path
	switch agent {
	case AgentClaude:
		if env["ANTHROPIC_BASE_URL"] != "" || env["ANTHROPIC_API_KEY"] != "" || env["ANTHROPIC_AUTH_TOKEN"] != "" ||
			env["ANTHROPIC_PROFILE"] != "" || env["CLAUDE_CODE_USE_BEDROCK"] != "" || env["CLAUDE_CODE_USE_VERTEX"] != "" ||
			env["CLAUDE_CODE_USE_FOUNDRY"] != "" {
			return subscriptionAccount{}, ""
		}
		if token := env["CLAUDE_CODE_OAUTH_TOKEN"]; token != "" {
			return subscriptionAccount{agent, profile, sha256.Sum256([]byte(token))}, token
		}
		if profile == "" {
			profile = defaultHomePath(".claude")
		}
		token, _ := claudeStoredAccessToken(profile)
		data, _ := os.ReadFile(claudeAuthPath(profile))
		var config claudeConfigJSON
		_ = json.Unmarshal(data, &config)
		if token == "" && config.OAuthAccount.AccountUUID == "" {
			return subscriptionAccount{}, ""
		}
		return subscriptionAccount{agent, profile, sha256.Sum256([]byte(firstNonEmpty(config.OAuthAccount.AccountUUID, token)))}, token
	case AgentCodex, AgentKimi:
		data, _ := os.ReadFile(auth.StoragePath)
		if agent == AgentCodex {
			return subscriptionAccount{agent, profile, codexSubscriptionIdentity(data)}, ""
		}
		return subscriptionAccount{agent, profile, kimiSubscriptionIdentity(data)}, ""
	}
	return subscriptionAccount{}, ""
}

func (m *Manager) readSubscription(ctx context.Context, cfg AgentConfig, env map[string]string, account subscriptionAccount, token string) ([]SubscriptionWindow, error) {
	switch account.agent {
	case AgentClaude:
		return readClaudeSubscription(ctx, token)
	case AgentKimi:
		return m.readKimiSubscription(ctx, cfg, env)
	default:
		raw, err := m.codexAccountRPC(ctx, cfg, account.profile, "account/rateLimits/read", nil)
		if err != nil {
			return nil, err
		}
		return codexSubscriptionWindows(raw)
	}
}

func (m *Manager) claudeSubscriptionAccount(name string, cfg AgentConfig, env map[string]string) subscriptionAccount {
	if name != AgentClaude || cfg.URL != "" {
		return subscriptionAccount{}
	}
	auth := resolveAgentAuthWithProviders(name, cfg, m.cfg.Root, env, m.providers())
	if auth.Kind != AuthKindOAuth {
		return subscriptionAccount{}
	}
	account, _ := subscriptionProfile(name, env, auth)
	return account
}

func (m *Manager) observeClaudeSubscription(account subscriptionAccount, raw json.RawMessage) {
	windows := claudeSubscriptionEventWindows(raw)
	if len(windows) == 0 {
		return
	}
	cache := m.subscriptionCache(account.agent)
	cache.Lock()
	defer cache.Unlock()
	if cache.account.agent == "" {
		cache.account = account
	}
	if cache.account != account {
		return
	}
	merged := slices.Clone(cache.windows)
	for _, window := range windows {
		merged = replaceSubscriptionWindow(merged, window)
	}
	slices.SortFunc(merged, func(a, b SubscriptionWindow) int { return strings.Compare(a.ID, b.ID) })
	cache.windows, cache.observed = merged, time.Now().UTC()
}

func replaceSubscriptionWindow(windows []SubscriptionWindow, window SubscriptionWindow) []SubscriptionWindow {
	for i := range windows {
		if windows[i].ID == window.ID {
			windows[i] = window
			return windows
		}
	}
	return append(windows, window)
}
