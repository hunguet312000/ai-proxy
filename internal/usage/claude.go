package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

type ClaudeFetcher struct {
	client *http.Client
	url    string
}

func NewClaudeFetcher(client *http.Client) *ClaudeFetcher {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &ClaudeFetcher{client: client, url: claudeUsageURL}
}

func (f *ClaudeFetcher) Provider() string { return "claude" }

func (f *ClaudeFetcher) Fetch(ctx context.Context, accessToken, _ string) (Quota, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return Quota{}, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	response, err := f.client.Do(request)
	if err != nil {
		return Quota{}, fmt.Errorf("fetch Claude quota: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Quota{}, fmt.Errorf("read Claude quota: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Quota{}, fmt.Errorf("fetch Claude quota: HTTP %d", response.StatusCode)
	}
	quota, err := ParseClaudeQuota(body)
	if err != nil {
		return Quota{}, err
	}
	quota.FetchedAt = time.Now().UTC()
	return quota, nil
}

func ParseClaudeQuota(body []byte) (Quota, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return Quota{}, fmt.Errorf("decode Claude quota: %w", err)
	}
	quota := Quota{Provider: "claude", Plan: "Claude Code"}
	keys := make([]string, 0, len(root))
	for key := range root {
		if key == "five_hour" || key == "seven_day" || strings.HasPrefix(key, "seven_day_") {
			keys = append(keys, key)
		}
	}
	slicesSort(keys)
	for _, key := range keys {
		window, ok := object(root[key])
		if !ok {
			continue
		}
		used, exists := number(window["utilization"])
		if !exists {
			continue
		}
		name := key
		switch key {
		case "five_hour":
			name = "session"
		case "seven_day":
			name = "weekly"
		default:
			name = "weekly_" + strings.TrimPrefix(key, "seven_day_")
		}
		quota.Windows = append(quota.Windows, percentageWindow(name, used, parseReset(window["resets_at"])))
	}
	return quota, nil
}

func number(value any) (float64, bool) {
	result, ok := value.(float64)
	return result, ok
}

func slicesSort(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
