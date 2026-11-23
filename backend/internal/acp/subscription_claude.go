package acp

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"
)

func readClaudeSubscription(ctx context.Context, token string) ([]SubscriptionWindow, error) {
	if token == "" {
		return nil, errors.New("Claude subscription credentials are unavailable")
	}
	var response struct {
		Limits []struct {
			Kind     string          `json:"kind"`
			Scope    json.RawMessage `json:"scope"`
			Percent  *float64        `json:"percent"`
			ResetsAt time.Time       `json:"resets_at"`
		} `json:"limits"`
		FiveHour       *claudeUsageWindow `json:"five_hour"`
		SevenDay       *claudeUsageWindow `json:"seven_day"`
		SevenDayOpus   *claudeUsageWindow `json:"seven_day_opus"`
		SevenDaySonnet *claudeUsageWindow `json:"seven_day_sonnet"`
	}
	if err := readSubscriptionJSON(ctx, "https://api.anthropic.com/api/oauth/usage", token, "oauth-2025-04-20", &response); err != nil {
		return nil, err
	}
	windows := make(map[string]SubscriptionWindow)
	for _, entry := range []struct {
		id, label string
		window    *claudeUsageWindow
	}{
		{"five_hour", "5 hours", response.FiveHour},
		{"seven_day", "7 days", response.SevenDay},
		{"seven_day:opus", "7 days · Opus", response.SevenDayOpus},
		{"seven_day:sonnet", "7 days · Sonnet", response.SevenDaySonnet},
	} {
		if entry.window != nil && entry.window.Utilization != nil {
			windows[entry.id] = SubscriptionWindow{entry.id, entry.label, *entry.window.Utilization, entry.window.ResetsAt}
		}
	}
	for _, limit := range response.Limits {
		if limit.Percent == nil {
			continue
		}
		switch limit.Kind {
		case "session":
			windows["five_hour"] = SubscriptionWindow{"five_hour", "5 hours", *limit.Percent, limit.ResetsAt}
		case "weekly_all":
			windows["seven_day"] = SubscriptionWindow{"seven_day", "7 days", *limit.Percent, limit.ResetsAt}
		case "weekly_scoped":
			var scope struct {
				Model struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			}
			if json.Unmarshal(limit.Scope, &scope) == nil && scope.Model.DisplayName != "" {
				id := "seven_day:" + strings.ToLower(scope.Model.DisplayName)
				windows[id] = SubscriptionWindow{id, "7 days · " + scope.Model.DisplayName, *limit.Percent, limit.ResetsAt}
			}
		}
	}
	return slices.SortedFunc(maps.Values(windows), func(a, b SubscriptionWindow) int { return strings.Compare(a.ID, b.ID) }), nil
}

type claudeUsageWindow struct {
	Utilization *float64  `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"`
}

func claudeSubscriptionEventWindows(raw json.RawMessage) map[string]SubscriptionWindow {
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    int64    `json:"resetsAt"`
	}
	var update struct {
		Meta struct {
			Limit struct {
				window
				Type    string            `json:"rateLimitType"`
				Windows map[string]window `json:"unifiedWindows"`
			} `json:"_claude/rateLimit"`
		} `json:"_meta"`
	}
	if json.Unmarshal(raw, &update) != nil {
		return nil
	}
	limit := update.Meta.Limit
	windows := make(map[string]SubscriptionWindow)
	for _, entry := range []struct{ id, label string }{{"five_hour", "5 hours"}, {"seven_day", "7 days"}} {
		value := limit.Windows[entry.id]
		if value.Utilization == nil && limit.Type == entry.id {
			value = limit.window
		}
		if value.Utilization != nil {
			windows[entry.id] = SubscriptionWindow{entry.id, entry.label, *value.Utilization * 100, subscriptionReset(value.ResetsAt)}
		}
	}
	return windows
}

func subscriptionReset(seconds int64) time.Time {
	if seconds > 0 {
		return time.Unix(seconds, 0).UTC()
	}
	return time.Time{}
}
