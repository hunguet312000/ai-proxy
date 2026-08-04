package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"literouter/internal/pool"
)

type ChatUsage struct {
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
}

// ChatTest sends a tiny non-streaming probe using a live OAuth account from the pool.
// Used by the dashboard model tester so connected sessions work without API keys.
func (m *CredentialManager) ChatTest(ctx context.Context, accountPool *pool.Pool, provider, model string) (accountID, preview string, usage ChatUsage, err error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "xai" {
		provider = "grok"
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", ChatUsage{}, fmt.Errorf("model is required")
	}
	model = resolveUpstreamModel(provider, model)

	account, ok := pickAccount(accountPool, provider)
	if !ok {
		return "", "", ChatUsage{}, fmt.Errorf("no active %s OAuth connection — add one under Connections", displayProvider(provider))
	}
	_, token, info, err := m.LoadFresh(ctx, account.ID)
	if err != nil {
		return account.ID, "", ChatUsage{}, fmt.Errorf("refresh OAuth session: %w", err)
	}
	if token.AccessToken == "" {
		return account.ID, "", ChatUsage{}, fmt.Errorf("OAuth session has no access token")
	}

	client := &http.Client{Timeout: 45 * time.Second}
	switch provider {
	case "codex":
		// 9router path: ChatGPT backend Codex responses — NOT api.openai.com (no api.responses.write scope).
		preview, usage, err = chatCodexBackend(ctx, client, token.AccessToken, info.ID, model)
	case "claude":
		preview, usage, err = chatClaude(ctx, client, token.AccessToken, model)
	case "grok":
		preview, usage, err = chatOpenAICompatible(ctx, client, "https://api.x.ai/v1/chat/completions", token.AccessToken, model, map[string]string{
			"x-xai-token-auth":         "xai-grok-cli",
			"x-grok-client-identifier": "grok-shell",
			"x-grok-client-mode":       "headless",
		})
	default:
		return account.ID, "", ChatUsage{}, fmt.Errorf("unsupported provider %q", provider)
	}
	if err != nil {
		return account.ID, "", usage, err
	}
	preview = strings.Join(strings.Fields(strings.TrimSpace(preview)), " ")
	if len(preview) > 160 {
		preview = preview[:157] + "..."
	}
	if preview == "" {
		preview = "(empty response)"
	}
	return account.ID, preview, usage, nil
}

// resolveUpstreamModel mirrors 9router catalog mapping:
// - strip provider prefix (cx/, xai/)
// - *-review virtual models map to base upstreamModelId for ChatGPT Codex accounts
func resolveUpstreamModel(provider, model string) string {
	model = strings.TrimSpace(model)
	switch {
	case strings.HasPrefix(model, "cx/"):
		model = strings.TrimPrefix(model, "cx/")
	case strings.HasPrefix(model, "xai/"):
		model = strings.TrimPrefix(model, "xai/")
	}
	// Preserve trailing effort markers like " (high)" if present.
	suffix := ""
	if i := strings.LastIndex(model, " ("); i >= 0 && strings.HasSuffix(model, ")") {
		suffix = model[i:]
		model = strings.TrimSpace(model[:i])
	}
	if provider == "codex" || provider == "cx" {
		if strings.HasSuffix(model, "-review") {
			model = strings.TrimSuffix(model, "-review")
		}
	}
	return model + suffix
}

func pickAccount(accountPool *pool.Pool, provider string) (pool.Account, bool) {
	if accountPool == nil {
		return pool.Account{}, false
	}
	var best pool.Account
	found := false
	for _, account := range accountPool.List() {
		p := strings.ToLower(account.Provider)
		if p == "xai" {
			p = "grok"
		}
		if p != provider || !account.Enabled || account.QuotaExhausted {
			continue
		}
		if !found || account.Weight > best.Weight {
			best = account
			found = true
		}
	}
	// Fallback: allow exhausted/disabled? no — only enabled.
	if !found {
		for _, account := range accountPool.List() {
			p := strings.ToLower(account.Provider)
			if p == "xai" {
				p = "grok"
			}
			if p == provider && account.Enabled {
				return account, true
			}
		}
	}
	return best, found
}

func displayProvider(provider string) string {
	switch provider {
	case "codex":
		return "Codex"
	case "claude":
		return "Claude"
	case "grok":
		return "xAI"
	default:
		return provider
	}
}

func chatOpenAICompatible(ctx context.Context, client *http.Client, endpoint, accessToken, model string, extraHeaders map[string]string) (string, ChatUsage, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with exactly: pong"},
		},
		"max_tokens":  16,
		"temperature": 0,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", ChatUsage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", ChatUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range extraHeaders {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	res, err := client.Do(req)
	if err != nil {
		return "", ChatUsage{}, fmt.Errorf("chat request failed: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", ChatUsage{}, fmt.Errorf("%s", friendlyProviderErr("chat", res.StatusCode, raw))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", ChatUsage{}, fmt.Errorf("decode chat response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", ChatUsage{}, fmt.Errorf("chat returned no choices")
	}
	usage := ChatUsage{
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
		CachedTokens:     parsed.Usage.PromptTokensDetails.CachedTokens,
	}
	return contentString(parsed.Choices[0].Message.Content), usage, nil
}

func chatCodexResponses(ctx context.Context, client *http.Client, accessToken, accountID, model string) (string, error) {
	payload := map[string]any{
		"model":             model,
		"input":             []map[string]any{{"role": "user", "content": "Reply with exactly: pong"}},
		"store":             false,
		"max_output_tokens": 32,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/responses", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("responses request failed: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("%s", friendlyProviderErr("responses", res.StatusCode, raw))
	}
	// Responses API shapes vary: output_text, or output[].content[].text
	var parsed struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("decode responses: %w", err)
	}
	if strings.TrimSpace(parsed.OutputText) != "" {
		return parsed.OutputText, nil
	}
	var b strings.Builder
	for _, item := range parsed.Output {
		for _, part := range item.Content {
			if part.Text != "" {
				b.WriteString(part.Text)
			}
		}
	}
	return b.String(), nil
}

func chatCodexBackend(ctx context.Context, client *http.Client, accessToken, accountID, model string) (string, ChatUsage, error) {
	// Mirrors 9router transport:
	//   baseUrl: https://chatgpt.com/backend-api/codex/responses
	//   headers: originator=codex_cli_rs, User-Agent=codex_cli_rs/..., ChatGPT-Account-ID
	//   format: openai-responses, store=false
	payload := map[string]any{
		"model":        model,
		"input":        []map[string]any{{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "Reply with exactly: pong"}}}},
		"store":        false,
		"stream":       true,
		"instructions": "You are a connectivity probe. Reply with exactly: pong",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", ChatUsage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
	if err != nil {
		return "", ChatUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("User-Agent", "codex_cli_rs/0.136.0")
	req.Header.Set("OpenAI-Beta", "codex-1")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
	res, err := client.Do(req)
	if err != nil {
		return "", ChatUsage{}, fmt.Errorf("codex backend request failed: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", ChatUsage{}, fmt.Errorf("%s", friendlyProviderErr("codex-backend", res.StatusCode, raw))
	}
	if text := extractSSEText(string(raw)); text != "" {
		return text, usageFromSSE(raw), nil
	}
	if text := extractResponsesText(raw); text != "" {
		return text, usageFromJSON(raw), nil
	}
	// Stream completed with only control events still counts as connectivity OK.
	if strings.Contains(string(raw), "data:") || strings.Contains(string(raw), "response.") {
		return "pong", usageFromSSE(raw), nil
	}
	if strings.TrimSpace(string(raw)) == "" {
		return "(ok)", usageFromSSE(raw), nil
	}
	return strings.TrimSpace(string(raw)), usageFromSSE(raw), nil
}

func extractResponsesText(raw []byte) string {
	var parsed struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return ""
	}
	if s := strings.TrimSpace(parsed.OutputText); s != "" {
		return s
	}
	var b strings.Builder
	for _, item := range parsed.Output {
		for _, part := range item.Content {
			if part.Text != "" {
				b.WriteString(part.Text)
			}
		}
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		return s
	}
	if len(parsed.Choices) > 0 {
		return strings.TrimSpace(contentString(parsed.Choices[0].Message.Content))
	}
	return ""
}

func extractSSEText(raw string) string {
	var b strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if text := extractResponsesText([]byte(data)); text != "" {
			b.WriteString(text)
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Text  string `json:"text"`
		}
		if json.Unmarshal([]byte(data), &event) == nil {
			if event.Delta != "" {
				b.WriteString(event.Delta)
			} else if event.Text != "" {
				b.WriteString(event.Text)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func chatClaude(ctx context.Context, client *http.Client, accessToken, model string) (string, ChatUsage, error) {
	payload := map[string]any{
		"model":      model,
		"max_tokens": 16,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with exactly: pong"},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", ChatUsage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", ChatUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	res, err := client.Do(req)
	if err != nil {
		return "", ChatUsage{}, fmt.Errorf("claude request failed: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", ChatUsage{}, fmt.Errorf("%s", friendlyProviderErr("claude", res.StatusCode, raw))
	}
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", ChatUsage{}, fmt.Errorf("decode claude response: %w", err)
	}
	var b strings.Builder
	for _, part := range parsed.Content {
		if part.Text != "" {
			b.WriteString(part.Text)
		}
	}
	usage := ChatUsage{
		PromptTokens:     parsed.Usage.InputTokens,
		CompletionTokens: parsed.Usage.OutputTokens,
		CachedTokens:     parsed.Usage.CacheReadInputTokens,
	}
	return b.String(), usage, nil
}

func usageFromJSON(raw []byte) ChatUsage {
	var parsed struct {
		Usage struct {
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
		Response struct {
			Usage struct {
				InputTokens        int `json:"input_tokens"`
				OutputTokens       int `json:"output_tokens"`
				TotalTokens        int `json:"total_tokens"`
				InputTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return ChatUsage{}
	}
	u := parsed.Usage
	if u.PromptTokens == 0 && u.InputTokens == 0 && parsed.Response.Usage.InputTokens > 0 {
		u.InputTokens = parsed.Response.Usage.InputTokens
		u.OutputTokens = parsed.Response.Usage.OutputTokens
		u.InputTokensDetails.CachedTokens = parsed.Response.Usage.InputTokensDetails.CachedTokens
	}
	prompt := u.PromptTokens
	if prompt == 0 {
		prompt = u.InputTokens
	}
	completion := u.CompletionTokens
	if completion == 0 {
		completion = u.OutputTokens
	}
	cached := u.PromptTokensDetails.CachedTokens
	if cached == 0 {
		cached = u.InputTokensDetails.CachedTokens
	}
	if cached == 0 {
		cached = u.CacheReadInputTokens
	}
	return ChatUsage{PromptTokens: prompt, CompletionTokens: completion, CachedTokens: cached}
}

func usageFromSSE(raw []byte) ChatUsage {
	var best ChatUsage
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		u := usageFromJSON([]byte(data))
		if u.PromptTokens+u.CompletionTokens >= best.PromptTokens+best.CompletionTokens {
			best = u
		}
	}
	if best.PromptTokens+best.CompletionTokens == 0 {
		if u := usageFromJSON(raw); u.PromptTokens+u.CompletionTokens > 0 {
			return u
		}
	}
	return best
}

func contentString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	default:
		return fmt.Sprint(v)
	}
}

func friendlyProviderErr(kind string, status int, raw []byte) string {
	msg := trimErr(raw)
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "billing_not_active"), strings.Contains(lower, "billing details"):
		return "OpenAI says this OAuth account billing is not active for API calls. Subscription ChatGPT login may still work in the app, but api.openai.com rejected the token."
	case strings.Contains(lower, "insufficient_quota"):
		return "Provider quota exhausted for this OAuth account."
	case strings.Contains(lower, "missing scopes"), strings.Contains(lower, "insufficient permissions"):
		return "OAuth token lacks required permission for this endpoint. Re-connect Codex OAuth, or check account plan."
	case status == 401 && (strings.Contains(lower, "invalid_api_key") || strings.Contains(lower, "authentication") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "invalid token") || msg == "empty error body"):
		return "OAuth token rejected (expired/invalid). Re-connect the account."
	case status == 401:
		return fmt.Sprintf("provider HTTP 401: %s", msg)
	case status == 429:
		return "Rate limited by provider: " + msg
	default:
		return fmt.Sprintf("%s HTTP %d: %s", kind, status, msg)
	}
}

func trimErr(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if len(s) > 180 {
		return s[:177] + "..."
	}
	if s == "" {
		return "empty error body"
	}
	return s
}
