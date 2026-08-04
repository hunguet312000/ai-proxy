package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type OpenAICompatibleClient struct {
	name    string
	baseURL string
	apiKey  string
	client  *http.Client
}

type ProviderError struct {
	Provider   string
	StatusCode int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("%s provider: HTTP %d: %s", e.Provider, e.StatusCode, e.Message)
}

func NewOpenAICompatibleClient(name, baseURL, apiKey string, client *http.Client) (*OpenAICompatibleClient, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid %s base URL", name)
	}
	if parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" {
		return nil, fmt.Errorf("%s base URL must use HTTPS", name)
	}
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2: true, MaxIdleConns: 100, MaxIdleConnsPerHost: 32,
			// Reasoning models can withhold response headers for minutes on hard
			// prompts; 120s turned those turns into spurious mid-request failures.
			TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 5 * time.Minute, IdleConnTimeout: 90 * time.Second,
		}}
	}
	return &OpenAICompatibleClient{name: name, baseURL: parsed.String(), apiKey: apiKey, client: client}, nil
}

func NewOpenAIClient(apiKey string, client *http.Client) (*OpenAICompatibleClient, error) {
	return NewOpenAICompatibleClient("openai", "https://api.openai.com/v1", apiKey, client)
}

func NewXAIClient(apiKey string, client *http.Client) (*OpenAICompatibleClient, error) {
	return NewOpenAICompatibleClient("xai", "https://api.x.ai/v1", apiKey, client)
}

func (c *OpenAICompatibleClient) DoJSON(ctx context.Context, path string, requestBody, responseBody any) error {
	if c.apiKey == "" {
		return fmt.Errorf("%s API key is required", c.name)
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", c.name, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+strings.TrimLeft(path, "/"), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create %s request: %w", c.name, err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("call %s provider: %w", c.name, err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, 16<<20)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return DecodeProviderError(c.name, response, limited)
	}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(responseBody); err != nil {
		return fmt.Errorf("decode %s response: %w", c.name, err)
	}
	return nil
}

func (c *OpenAICompatibleClient) DoStream(ctx context.Context, path string, requestBody any) (io.ReadCloser, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("%s API key is required", c.name)
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", c.name, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+strings.TrimLeft(path, "/"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create %s request: %w", c.name, err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call %s provider: %w", c.name, err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		return nil, DecodeProviderError(c.name, response, io.LimitReader(response.Body, 1<<20))
	}
	return response.Body, nil
}

func DecodeProviderError(provider string, response *http.Response, body io.Reader) error {
	data, _ := io.ReadAll(body)
	var payload struct {
		Error   json.RawMessage `json:"error"`
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	_ = json.Unmarshal(data, &payload)

	message := strings.TrimSpace(payload.Message)
	code := jsonScalar(payload.Code)
	if len(payload.Error) > 0 {
		var nested struct {
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
			Detail  string          `json:"detail"`
		}
		if json.Unmarshal(payload.Error, &nested) == nil {
			if message == "" {
				message = strings.TrimSpace(nested.Message)
			}
			if message == "" {
				message = strings.TrimSpace(nested.Detail)
			}
			if code == "" {
				code = jsonScalar(nested.Code)
			}
		} else if message == "" {
			message = jsonScalar(payload.Error)
		}
	}
	if message == "" {
		message = strings.TrimSpace(payload.Detail)
	}
	if message == "" {
		message = errorSnippet(data)
	}
	if message == "" {
		message = response.Status
	}

	var retryAfter time.Duration
	if value := response.Header.Get("Retry-After"); value != "" {
		if seconds, err := time.ParseDuration(value + "s"); err == nil {
			retryAfter = seconds
		}
	}
	return &ProviderError{
		Provider: provider, StatusCode: response.StatusCode, Code: code,
		Message: message, RetryAfter: retryAfter,
	}
}

func jsonScalar(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return strings.TrimSpace(value)
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}

func errorSnippet(data []byte) string {
	const limit = 1024
	message := strings.Join(strings.Fields(string(data)), " ")
	if len(message) > limit {
		message = message[:limit] + "..."
	}
	return message
}
