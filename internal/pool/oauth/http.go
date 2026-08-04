package oauth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const maxProviderResponseBytes = 1 << 20

func decodeJSONResponse(response *http.Response, target any) error {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read provider response: %w", err)
	}
	if len(body) > maxProviderResponseBytes {
		return fmt.Errorf("provider response exceeds %d bytes", maxProviderResponseBytes)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return parseProviderError(response.StatusCode, body)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode provider response: %w", err)
	}
	return nil
}

func parseProviderError(status int, body []byte) error {
	var envelope struct {
		Error any    `json:"error"`
		Code  string `json:"code"`
		Msg   string `json:"message"`
	}
	_ = json.Unmarshal(body, &envelope)

	code := envelope.Code
	message := envelope.Msg
	switch value := envelope.Error.(type) {
	case string:
		if code == "" {
			code = value
		}
	case map[string]any:
		if candidate, ok := value["code"].(string); ok {
			code = candidate
		}
		if candidate, ok := value["message"].(string); ok {
			message = candidate
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return &ProviderError{
		StatusCode: status,
		Code:       code,
		Message:    message,
		Permanent:  isPermanentOAuthError(code),
	}
}

func isPermanentOAuthError(code string) bool {
	switch code {
	case "invalid_grant", "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated":
		return true
	default:
		return false
	}
}
