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

const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type CodexFetcher struct {
	client *http.Client
	url    string
}

func NewCodexFetcher(client *http.Client) *CodexFetcher {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &CodexFetcher{client: client, url: codexUsageURL}
}

func (f *CodexFetcher) Provider() string { return "codex" }

func (f *CodexFetcher) Fetch(ctx context.Context, accessToken, accountID string) (Quota, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return Quota{}, fmt.Errorf("create Codex quota request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	if accountID != "" {
		request.Header.Set("ChatGPT-Account-ID", accountID)
	}
	response, err := f.client.Do(request)
	if err != nil {
		return Quota{}, fmt.Errorf("fetch Codex quota: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Quota{}, fmt.Errorf("read Codex quota: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusUnauthorized {
			// Typed so the refresh loop can retire the account instead of only logging. An
			// account nothing routes to is never rejected at request time, so without this a
			// dead credential stays invisible for as long as the pool has a working peer.
			return Quota{}, fmt.Errorf("fetch Codex quota: HTTP 401: %w", ErrCredentialsRejected)
		}
		return Quota{}, fmt.Errorf("fetch Codex quota: HTTP %d", response.StatusCode)
	}
	quota, err := ParseCodexQuota(body)
	if err != nil {
		return Quota{}, err
	}
	quota.FetchedAt = time.Now().UTC()
	return quota, nil
}

func ParseCodexQuota(body []byte) (Quota, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return Quota{}, fmt.Errorf("decode Codex quota: %w", err)
	}
	quota := Quota{Provider: "codex", Plan: firstString(root, "plan_type")}
	if summary, ok := object(root["summary"]); ok && quota.Plan == "" {
		quota.Plan = firstString(summary, "plan")
	}

	normal := firstObject(root, "rate_limit", "rate_limits")
	if normal == nil {
		if byID, ok := object(root["rate_limits_by_limit_id"]); ok {
			normal = firstObject(byID, "codex")
		}
	}
	appendCodexWindows(&quota, "", normal)
	if boolValue(root["limit_reached"]) || boolValue(normal["limit_reached"]) {
		quota.LimitReached = true
	}

	review := firstObject(root, "code_review_rate_limit", "review_rate_limit")
	if review == nil {
		if byID, ok := object(root["rate_limits_by_limit_id"]); ok {
			review = firstObject(byID, "code_review", "codex_review", "review")
		}
	}
	appendCodexWindows(&quota, "review_", review)

	// Session reset credits (ChatGPT Plus/Pro perk).
	if credits := firstObject(root, "rate_limit_reset_credits"); credits != nil {
		quota.ResetCredits = int(firstNumber(credits, "available_count", "availableCount"))
		if quota.ResetCredits < 0 {
			quota.ResetCredits = 0
		}
	}
	return quota, nil
}

func appendCodexWindows(quota *Quota, prefix string, snapshot map[string]any) {
	if snapshot == nil {
		return
	}
	if nested, ok := object(snapshot["rate_limit"]); ok {
		if boolValue(nested["limit_reached"]) {
			quota.LimitReached = true
		}
		snapshot = nested
	}
	if boolValue(snapshot["limit_reached"]) {
		quota.LimitReached = true
	}
	if primary := firstObject(snapshot, "primary_window", "primary"); primary != nil {
		quota.Windows = append(quota.Windows, codexWindow(prefix+"session", primary))
	}
	if secondary := firstObject(snapshot, "secondary_window", "secondary"); secondary != nil {
		quota.Windows = append(quota.Windows, codexWindow(prefix+"weekly", secondary))
	}
}

func codexWindow(key string, value map[string]any) Window {
	used := firstNumber(value, "used_percent", "percent_used")
	return percentageWindow(key, used, parseReset(firstValue(value, "reset_at", "resets_at", "resetAt")))
}

func parseReset(value any) time.Time {
	switch reset := value.(type) {
	case float64:
		if reset > 0 && reset < 1e12 {
			reset *= 1000
		}
		return time.UnixMilli(int64(reset)).UTC()
	case string:
		if parsed, err := time.Parse(time.RFC3339, reset); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func object(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok
}

func firstObject(values map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value, ok := object(values[key]); ok {
			return value
		}
	}
	return nil
}

func firstValue(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return nil
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNumber(values map[string]any, keys ...string) float64 {
	for _, key := range keys {
		switch value := values[key].(type) {
		case float64:
			return value
		case json.Number:
			parsed, _ := value.Float64()
			return parsed
		}
	}
	return 0
}

func boolValue(value any) bool {
	result, _ := value.(bool)
	return result
}

const (
	codexResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	codexResetConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
)

// FetchCodexResetCredits returns how many session-reset credits remain.
func FetchCodexResetCredits(ctx context.Context, client *http.Client, accessToken, accountID string) (int, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexResetCreditsURL, nil)
	if err != nil {
		return 0, err
	}
	setCodexHeaders(req, accessToken, accountID)
	res, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch codex reset credits: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return 0, fmt.Errorf("fetch codex reset credits: HTTP %d: %s", res.StatusCode, trimBody(body))
	}
	var payload struct {
		AvailableCount float64 `json:"available_count"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// try nested
		var root map[string]any
		if json.Unmarshal(body, &root) != nil {
			return 0, fmt.Errorf("decode reset credits: %w", err)
		}
		n := firstNumber(root, "available_count", "availableCount")
		if n < 0 {
			n = 0
		}
		return int(n), nil
	}
	n := int(payload.AvailableCount)
	if n < 0 {
		n = 0
	}
	return n, nil
}

// ConsumeCodexResetCredit redeems one session-reset credit for the account.
func ConsumeCodexResetCredit(ctx context.Context, client *http.Client, accessToken, accountID, redeemRequestID string) error {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if strings.TrimSpace(redeemRequestID) == "" {
		return fmt.Errorf("redeem request id is required")
	}
	payload, _ := json.Marshal(map[string]string{"redeem_request_id": redeemRequestID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexResetConsumeURL, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	setCodexHeaders(req, accessToken, accountID)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("consume codex reset credit: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	var parsed struct {
		Code         string  `json:"code"`
		Message      string  `json:"message"`
		WindowsReset float64 `json:"windows_reset"`
	}
	_ = json.Unmarshal(body, &parsed)
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		if parsed.Code == "reset" || parsed.WindowsReset > 0 {
			return nil
		}
		if parsed.Code == "no_credit" {
			return fmt.Errorf("no Codex reset credits available")
		}
		// some success payloads may omit code
		if parsed.Code == "" && parsed.Message == "" {
			return nil
		}
		if parsed.Code != "" && parsed.Code != "reset" {
			return fmt.Errorf("codex reset credit: %s", parsed.Code)
		}
		return nil
	}
	if parsed.Code == "no_credit" {
		return fmt.Errorf("no Codex reset credits available")
	}
	if parsed.Message != "" {
		return fmt.Errorf("codex reset credit failed: %s", parsed.Message)
	}
	return fmt.Errorf("codex reset credit failed: HTTP %d: %s", res.StatusCode, trimBody(body))
}

func setCodexHeaders(req *http.Request, accessToken, accountID string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OpenAI-Beta", "codex-1")
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("User-Agent", "codex_cli_rs/0.136.0")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
}

func trimBody(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > 180 {
		return s[:177] + "..."
	}
	if s == "" {
		return "empty body"
	}
	return s
}
