package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"literouter/internal/pool"
	"literouter/internal/provider"
)

const (
	anthropicMessagesURL = "https://api.anthropic.com/v1/messages"
	anthropicVersion     = "2023-06-01"
	anthropicOAuthBeta   = "oauth-2025-04-20"
)

// SupportsAnthropicPassthrough reports whether a model can be served by an
// Anthropic-native upstream. When it can, the gateway forwards the caller's
// payload unchanged rather than translating it through the OpenAI shape, which
// is the only way prompt-cache breakpoints and thinking signatures survive.
func (inference *Inference) SupportsAnthropicPassthrough(model string) bool {
	if inference == nil || inference.credentials == nil || inference.selector == nil {
		return false
	}
	return oauthProviderForModel(model) == "claude"
}

// DoAnthropicStream sends an Anthropic Messages payload to Anthropic as-is,
// rotating pool accounts on failure exactly like the translated path does.
func (inference *Inference) DoAnthropicStream(ctx context.Context, payload []byte, model, conversationID, betas string) (io.ReadCloser, error) {
	if !inference.SupportsAnthropicPassthrough(model) {
		return nil, pool.ErrNoAccount
	}
	var lastErr error
	for wave := 1; wave <= oauthRetryWaves; wave++ {
		excluded := make(map[string]struct{})
		body, err := inference.openAnthropic(ctx, payload, model, conversationID, betas, excluded)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !errors.Is(err, errOAuthPoolExhausted) || wave == oauthRetryWaves || !retryableOAuthTransient(err) {
			return nil, err
		}
		if err := waitOAuthRetry(ctx, wave); err != nil {
			return nil, err
		}
		slog.Debug("retry Anthropic passthrough wave", "model", model, "wave", wave+1, "error", err)
	}
	return nil, lastErr
}

func (inference *Inference) openAnthropic(ctx context.Context, payload []byte, model, conversationID, betas string, excluded map[string]struct{}) (io.ReadCloser, error) {
	var lastErr error
	for {
		selected, err := inference.selector.Select(pool.SelectRequest{
			Provider: "claude", Model: model, ConversationID: conversationID, ExcludeIDs: excluded,
		})
		if err != nil {
			if lastErr != nil {
				return nil, errors.Join(errOAuthPoolExhausted, lastErr)
			}
			if len(excluded) > 0 {
				return nil, errors.Join(errOAuthPoolExhausted, err)
			}
			return nil, err
		}
		excluded[selected.Account.ID] = struct{}{}
		_, token, _, err := inference.credentials.LoadFresh(ctx, selected.Account.ID)
		if err != nil {
			inference.selector.CancelRequest(selected.ReservationID)
			inference.selector.ReportError(selected.Account.ID)
			lastErr = err
			continue
		}
		body, err := inference.callAnthropic(ctx, token.AccessToken, selected.ResolvedModel, betas, payload, selected.ReservationID)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var providerErr *provider.ProviderError
		if errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusTooManyRequests {
			inference.selector.ReportRateLimit(selected.Account.ID)
		} else {
			inference.selector.ReportError(selected.Account.ID)
		}
		slog.Debug("Anthropic passthrough account failed", "model", selected.ResolvedModel, "account_id", selected.Account.ID, "error", err)
		if !retryAcrossOAuthAccounts(err) {
			return nil, err
		}
	}
}

func (inference *Inference) callAnthropic(ctx context.Context, accessToken, model, betas string, payload []byte, reservationID uint64) (io.ReadCloser, error) {
	body, err := rewriteAnthropicModel(payload, model)
	if err != nil {
		inference.selector.CancelRequest(reservationID)
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicMessagesURL, bytes.NewReader(body))
	if err != nil {
		inference.selector.CancelRequest(reservationID)
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("anthropic-version", anthropicVersion)
	request.Header.Set("anthropic-beta", mergeAnthropicBetas(betas))
	inference.selector.CommitRequest(reservationID)
	response, err := inference.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		return nil, decodeOAuthInferenceError("claude", response)
	}
	return response.Body, nil
}

// mergeAnthropicBetas keeps the caller's beta flags — Claude Code negotiates
// features through them — while guaranteeing the OAuth flag the token requires.
func mergeAnthropicBetas(betas string) string {
	result := []string{anthropicOAuthBeta}
	for _, beta := range strings.Split(betas, ",") {
		beta = strings.TrimSpace(beta)
		if beta == "" || beta == anthropicOAuthBeta {
			continue
		}
		result = append(result, beta)
	}
	return strings.Join(result, ",")
}

func rewriteAnthropicModel(payload []byte, model string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, fmt.Errorf("decode Anthropic payload: %w", err)
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	fields["model"] = encoded
	return json.Marshal(fields)
}
