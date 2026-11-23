package acp

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

func codexSubscriptionIdentity(data []byte) [32]byte {
	var credentials struct {
		Tokens struct {
			Account string `json:"account_id"`
			Owner   string `json:"login_owner_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &credentials) == nil && credentials.Tokens.Owner != "" {
		data = []byte(credentials.Tokens.Account + "\x00" + credentials.Tokens.Owner)
	}
	return sha256.Sum256(data)
}

type codexLimitWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins int      `json:"windowDurationMins"`
	ResetsAt           int64    `json:"resetsAt"`
}

type codexLimitBucket struct {
	ID        string            `json:"limitId"`
	Name      string            `json:"limitName"`
	Primary   *codexLimitWindow `json:"primary"`
	Secondary *codexLimitWindow `json:"secondary"`
}

func codexSubscriptionWindows(raw json.RawMessage) ([]SubscriptionWindow, error) {
	var data struct {
		Primary *codexLimitBucket           `json:"rateLimits"`
		Buckets map[string]codexLimitBucket `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if len(data.Buckets) == 0 && data.Primary != nil {
		data.Buckets = map[string]codexLimitBucket{firstNonEmpty(data.Primary.ID, "codex"): *data.Primary}
	}
	var windows []SubscriptionWindow
	for _, id := range slices.Sorted(maps.Keys(data.Buckets)) {
		bucket := data.Buckets[id]
		for _, entry := range []struct {
			id     string
			window *codexLimitWindow
		}{{"primary", bucket.Primary}, {"secondary", bucket.Secondary}} {
			window := entry.window
			if window == nil || window.UsedPercent == nil {
				continue
			}
			label := subscriptionDurationLabel(window.WindowDurationMins)
			if id != "codex" {
				label = firstNonEmpty(bucket.Name, id) + " · " + label
			}
			windows = append(windows, SubscriptionWindow{id + ":" + entry.id, label, *window.UsedPercent, subscriptionReset(window.ResetsAt)})
		}
	}
	return windows, nil
}

func subscriptionDurationLabel(mins int) string {
	switch {
	case mins == 0:
		return "Usage limit"
	case mins%1440 == 0:
		return fmt.Sprintf("%d days", mins/1440)
	case mins%60 == 0:
		return fmt.Sprintf("%d hours", mins/60)
	default:
		return fmt.Sprintf("%d min", mins)
	}
}
