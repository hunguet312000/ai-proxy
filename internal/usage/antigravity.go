package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
)

const antigravityModelsURL = "https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"

type AntigravityFetcher struct {
	client *http.Client
	url    string
}

func NewAntigravityFetcher(client *http.Client) *AntigravityFetcher {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &AntigravityFetcher{client: client, url: antigravityModelsURL}
}

func (f *AntigravityFetcher) Provider() string { return "antigravity" }

func (f *AntigravityFetcher) Fetch(ctx context.Context, accessToken, projectID string) (Quota, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return Quota{}, fmt.Errorf("fetch Antigravity quota: project ID is required")
	}
	body, err := json.Marshal(map[string]string{"project": projectID})
	if err != nil {
		return Quota{}, fmt.Errorf("encode Antigravity quota request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url, bytes.NewReader(body))
	if err != nil {
		return Quota{}, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "google-api-nodejs-client/9.15.1")
	request.Header.Set("X-Goog-Api-Client", "google-cloud-sdk vscode-antigravity/1.107.0")
	request.Header.Set("Client-Metadata", fmt.Sprintf(`{"ideType":9,"platform":%d,"pluginType":2}`, antigravityQuotaPlatform()))
	request.Header.Set("x-request-source", "local")

	response, err := f.client.Do(request)
	if err != nil {
		return Quota{}, fmt.Errorf("fetch Antigravity quota: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return Quota{}, fmt.Errorf("read Antigravity quota: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Quota{}, fmt.Errorf("fetch Antigravity quota: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	quota, err := ParseAntigravityQuota(responseBody)
	if err != nil {
		return Quota{}, err
	}
	quota.FetchedAt = time.Now().UTC()
	return quota, nil
}

func ParseAntigravityQuota(body []byte) (Quota, error) {
	var response struct {
		Models map[string]struct {
			Model       string `json:"model"`
			DisplayName string `json:"displayName"`
			QuotaInfo   *struct {
				RemainingFraction *float64 `json:"remainingFraction"`
				ResetTime         string   `json:"resetTime"`
			} `json:"quotaInfo"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return Quota{}, fmt.Errorf("decode Antigravity quota: %w", err)
	}
	if len(response.Models) == 0 {
		return Quota{}, fmt.Errorf("Antigravity quota response has no models")
	}

	keys := make([]string, 0, len(response.Models))
	for key := range response.Models {
		keys = append(keys, key)
	}
	slicesSort(keys)
	quota := Quota{Provider: "antigravity", Plan: "Google Cloud"}
	for _, key := range keys {
		model := response.Models[key]
		if model.QuotaInfo == nil || model.QuotaInfo.RemainingFraction == nil {
			continue
		}
		name := strings.TrimSpace(model.Model)
		if name == "" {
			name = key
		}
		remaining := *model.QuotaInfo.RemainingFraction * 100
		if remaining < 0 {
			remaining = 0
		} else if remaining > 100 {
			remaining = 100
		}
		resetAt := parseReset(model.QuotaInfo.ResetTime)
		quota.Windows = append(quota.Windows, Window{
			Key: name, Used: 100 - remaining, Total: 100, Remaining: remaining,
			RemainingPercent: remaining, ResetAt: resetAt, Exhausted: remaining <= 0,
		})
	}
	if len(quota.Windows) == 0 {
		return Quota{}, fmt.Errorf("Antigravity quota response has no model quota information")
	}
	return quota, nil
}

func antigravityQuotaPlatform() int {
	switch runtime.GOOS {
	case "darwin":
		return 1
	case "windows":
		return 2
	default:
		return 3
	}
}
