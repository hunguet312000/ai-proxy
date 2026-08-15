package oauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"literouter/internal/pool"
	"literouter/internal/provider"
	"literouter/internal/storage"
	"literouter/internal/toolvalidate"
	"literouter/internal/translator"
)

const (
	oauthRetryWaves = 2
	oauthRetryDelay = 250 * time.Millisecond
)

var errOAuthPoolExhausted = errors.New("OAuth account pool exhausted")

// Inference routes gateway traffic through connected OAuth accounts selected by
// the pool selector. API-key clients remain a separate gateway fallback.
type Inference struct {
	credentials *CredentialManager
	selector    *pool.Selector
	client      *http.Client
}

func NewInference(credentials *CredentialManager, selector *pool.Selector) *Inference {
	// No client-level Timeout: Claude/Codex streams and long tool turns must not be hard-killed.
	// Per-attempt deadlines stay on the request context where needed.
	return &Inference{credentials: credentials, selector: selector, client: &http.Client{}}
}

func (inference *Inference) DoJSON(ctx context.Context, request translator.OpenAIRequest, conversationID string) (translator.OpenAIResponse, error) {
	return inference.complete(ctx, request, conversationID)
}

func (inference *Inference) DoStream(ctx context.Context, request translator.OpenAIRequest, conversationID string) (io.ReadCloser, error) {
	request.Stream = true
	// Preserve upstream streaming for tool turns. The gateway incrementally buffers
	// and validates only tool arguments, rather than the entire completion.
	body, accountID, providerName, err := inference.open(ctx, request, conversationID)
	if err != nil {
		return nil, err
	}
	// A live body is the only success signal this path ever gets, and the selector needs
	// it: every failure calls ReportError/ReportRateLimit, but nothing cleared the
	// counters, so for a Claude CLI session — which is streaming end to end — errors
	// accumulated for the life of the process and five scattered failures eventually
	// circuit-broke an account that had served hundreds of turns in between. Tokens are
	// still unknown here; the stream has not been read yet, so the hourly counter stays
	// fed by the non-streaming path alone.
	inference.selector.ReportSuccess(accountID, 0)
	if providerName == "codex" {
		return codexSSEToChatStream(body, request.Model), nil
	}
	if providerName == "antigravity" {
		// Pass the derived session key, not the raw header: it is what the signature
		// cache is keyed on, and it stays stable across the turns of one session.
		return antigravitySSEToChatStream(body, request.Model, antigravitySessionKey(request, conversationID)), nil
	}
	return body, nil
}

func validateOAuthToolCalls(response translator.OpenAIResponse, schemas toolvalidate.Schemas) error {
	for _, choice := range response.Choices {
		for _, call := range choice.Message.ToolCalls {
			if err := schemas.Validate(call.Function.Name, call.Function.Arguments); err != nil {
				return err
			}
		}
	}
	return nil
}

func (inference *Inference) complete(ctx context.Context, request translator.OpenAIRequest, conversationID string) (translator.OpenAIResponse, error) {
	request.Stream = true
	schemas := toolvalidate.Compile(request.Tools)
	toolRetry := false
	excluded := make(map[string]struct{})
	var lastErr error
	wave := 1
	for {
		body, accountID, providerName, err := inference.openWithExcluded(ctx, request, conversationID, excluded)
		if err != nil {
			retryErr := err
			if lastErr != nil {
				retryErr = lastErr
			}
			if errors.Is(err, errOAuthPoolExhausted) && wave < oauthRetryWaves && retryableOAuthTransient(retryErr) {
				if err := waitOAuthRetry(ctx, wave); err != nil {
					return translator.OpenAIResponse{}, err
				}
				slog.Debug("retry OAuth inference wave", "provider", oauthProviderForModel(request.Model), "model", request.Model, "wave", wave+1, "previous_accounts", len(excluded), "error", retryErr)
				clear(excluded)
				lastErr = nil
				wave++
				continue
			}
			if lastErr != nil {
				err = lastErr
			}
			logOAuthFailure(request.Model, wave, len(excluded), err)
			return translator.OpenAIResponse{}, err
		}
		var response translator.OpenAIResponse
		switch providerName {
		case "codex":
			response, err = codexSSEToOpenAI(body, request.Model)
		case "antigravity":
			response, err = antigravitySSEToOpenAI(body, request.Model, antigravitySessionKey(request, conversationID))
		default:
			response, err = openAISSEToResponse(body, request.Model)
		}
		_ = body.Close()
		if err != nil {
			if ctx.Err() != nil {
				// Caller disconnected/canceled: account and upstream are not at fault.
				return translator.OpenAIResponse{}, ctx.Err()
			}
			if !retryAcrossOAuthAccounts(err) {
				// Deterministic request error (context/tool/payload): changing account cannot help.
				// Return the typed upstream error immediately and keep account health unchanged.
				return translator.OpenAIResponse{}, err
			}
			inference.selector.ReportError(accountID)
			lastErr = err
			continue
		}
		if validationErr := validateOAuthToolCalls(response, schemas); validationErr != nil {
			var toolErr *toolvalidate.Error
			if !errors.As(validationErr, &toolErr) || toolRetry {
				return translator.OpenAIResponse{}, validationErr
			}
			toolRetry = true
			request.Messages = append(request.Messages, translator.OpenAIMessage{
				Role:    "user",
				Content: fmt.Sprintf("Your %s tool call had invalid arguments (%s). Generate the tool call again using exactly the declared JSON schema. Do not explain the correction.", toolErr.Tool, toolErr.Category),
			})
			slog.Warn("retry invalid OAuth tool call", "provider", providerName, "model", request.Model, "tool", toolErr.Tool, "category", toolErr.Category, "bytes", toolErr.Bytes, "retry", 1)
			clear(excluded)
			lastErr = nil
			wave = 1
			continue
		}
		inference.selector.ReportSuccess(accountID, response.Usage.PromptTokens+response.Usage.CompletionTokens)
		return response, nil
	}
}

func (inference *Inference) open(ctx context.Context, request translator.OpenAIRequest, conversationID string) (io.ReadCloser, string, string, error) {
	var lastErr error
	for wave := 1; wave <= oauthRetryWaves; wave++ {
		excluded := make(map[string]struct{})
		body, accountID, providerName, err := inference.openWithExcluded(ctx, request, conversationID, excluded)
		if err == nil {
			return body, accountID, providerName, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, "", "", ctx.Err()
		}
		if !errors.Is(err, errOAuthPoolExhausted) || wave == oauthRetryWaves || !retryableOAuthTransient(err) {
			logOAuthFailure(request.Model, wave, len(excluded), err)
			return nil, "", "", err
		}
		if err := waitOAuthRetry(ctx, wave); err != nil {
			return nil, "", "", err
		}
		slog.Debug("retry OAuth stream open wave", "provider", oauthProviderForModel(request.Model), "model", request.Model, "wave", wave+1, "previous_accounts", len(excluded), "error", err)
	}
	return nil, "", "", lastErr
}

func (inference *Inference) openWithExcluded(ctx context.Context, request translator.OpenAIRequest, conversationID string, excluded map[string]struct{}) (io.ReadCloser, string, string, error) {
	if inference == nil || inference.credentials == nil || inference.selector == nil {
		return nil, "", "", pool.ErrNoAccount
	}
	providerName := oauthProviderForModel(request.Model)
	selectionModel := request.Model
	if providerName == "antigravity" {
		selectionModel = resolveAntigravityModel(request.Model)
	}
	// Claude OAuth is onboarding/quota only today — fail fast so gateway can use API-key path
	// instead of burning selector retries on an unsupported provider path.
	if providerName == "claude" {
		return nil, "", "", pool.ErrNoAccount
	}
	var lastErr error
	for {
		selected, err := inference.selector.Select(pool.SelectRequest{
			Provider: providerName, Model: selectionModel, ConversationID: conversationID,
			FirstMessage: firstOpenAIUserMessage(request.Messages), ExcludeIDs: excluded,
		})
		if err != nil {
			if lastErr != nil {
				return nil, "", "", errors.Join(errOAuthPoolExhausted, lastErr)
			}
			if len(excluded) > 0 {
				return nil, "", "", errors.Join(errOAuthPoolExhausted, err)
			}
			return nil, "", "", err
		}
		excluded[selected.Account.ID] = struct{}{}
		reservationID := selected.ReservationID
		_, token, info, err := inference.credentials.LoadFresh(ctx, selected.Account.ID)
		if err != nil {
			inference.selector.CancelRequest(reservationID)
			inference.selector.ReportError(selected.Account.ID)
			lastErr = err
			continue
		}
		upstreamAccountID := info.ID
		if selected.Account.Provider == "antigravity" {
			upstreamAccountID = token.ProjectID
		}
		body, err := inference.call(ctx, selected.Account.Provider, token, upstreamAccountID, selected.ResolvedModel, conversationID, request, reservationID)
		if err == nil {
			return body, selected.Account.ID, providerName, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			// Request owner canceled. Do not rotate/cooldown/penalize a healthy account.
			return nil, "", "", ctx.Err()
		}
		var providerErr *provider.ProviderError
		switch {
		case errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusTooManyRequests:
			// DecodeProviderError already read Retry-After off the response; passing it on
			// is what turns the selector's fixed 30s/1m/2m ladder into the wait the
			// upstream actually asked for.
			if selected.Account.Provider == "antigravity" {
				inference.selector.ReportModelRateLimitAfter(selected.Account.ID, selected.ResolvedModel, providerErr.RetryAfter)
			} else {
				inference.selector.ReportRateLimitAfter(selected.Account.ID, providerErr.RetryAfter)
			}
		case retiresAccount(err):
			inference.selector.ReportError(selected.Account.ID)
			errors.As(err, &providerErr)
			inference.credentials.RetireRejectedAccount(ctx, selected.Account.ID,
				selected.Account.Provider, providerErr.Message)
		default:
			inference.selector.ReportError(selected.Account.ID)
		}
		slog.Debug("OAuth account inference failed", "provider", providerName, "model", selected.ResolvedModel, "account_id", selected.Account.ID, "error", err)
		if !retryAcrossOAuthAccounts(err) {
			return nil, "", "", err
		}
	}
}

// retiresAccount reports whether a failure means this account's credentials are finished
// rather than this request being unlucky.
//
// Only 401. LoadFresh has already refreshed a token that was near expiry before the call,
// so a refusal at request time is the provider saying the credential itself is no good, and
// it will say the same on every later turn. Everything else stays in the pool: a 429 is
// quota, a 5xx is an upstream fault, and a 403 can be a per-model permission — retiring on
// any of those would drain the pool during an incident that ends on its own.
func retiresAccount(err error) bool {
	var providerErr *provider.ProviderError
	return errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized
}

func retryAcrossOAuthAccounts(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var providerErr *provider.ProviderError
	if errors.As(err, &providerErr) {
		switch strings.ToLower(strings.TrimSpace(providerErr.Code)) {
		case "context_length_exceeded", "max_tokens_exceeded", "invalid_prompt", "invalid_tool_schema",
			"ai_model_not_found", "model_not_found", "model_name_not_valid":
			return false
		}
		switch providerErr.StatusCode {
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusNotFound,
			http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
			// Request/model/payload failures are deterministic across accounts.
			return false
		default:
			// A named-model refusal is deterministic too: another account faces the same
			// catalog. Retrying it only burns waves on a loop that cannot resolve.
			message := strings.ToLower(providerErr.Message)
			for _, phrase := range []string{"model name is not valid", "ai model not found", "model not found", "not supported"} {
				if strings.Contains(message, phrase) {
					return false
				}
			}
			return true
		}
	}
	return true
}

func retryableOAuthTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var providerErr *provider.ProviderError
	if errors.As(err, &providerErr) {
		switch strings.ToLower(strings.TrimSpace(providerErr.Code)) {
		case "ai_model_not_found", "model_not_found", "model_name_not_valid":
			return false
		}
		message := strings.ToLower(providerErr.Message)
		if strings.Contains(message, "model name is not valid") || strings.Contains(message, "ai model not found") {
			return false
		}
		return providerErr.StatusCode == http.StatusRequestTimeout ||
			providerErr.StatusCode == http.StatusTooEarly ||
			providerErr.StatusCode == http.StatusTooManyRequests ||
			providerErr.StatusCode >= http.StatusInternalServerError
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	lower := strings.ToLower(err.Error())
	for _, permanent := range []string{"invalid_request", "invalid request", "unsupported", "context_length", "too many tokens", "malformed", "invalid tool"} {
		if strings.Contains(lower, permanent) {
			return false
		}
	}
	// Stream truncation / missing terminal event is safe to retry before the gateway writes 200.
	return strings.Contains(lower, "stream") || strings.Contains(lower, "response.completed") || strings.Contains(lower, "finish reason")
}

func waitOAuthRetry(ctx context.Context, wave int) error {
	delay := time.Duration(wave) * oauthRetryDelay
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func logOAuthFailure(model string, wave, accounts int, err error) {
	if errors.Is(err, context.Canceled) {
		// Normal when Claude CLI aborts/replaces a request or the user stops generation.
		slog.Debug("OAuth inference canceled by client", "provider", oauthProviderForModel(model), "model", model, "accounts_tried", accounts)
		return
	}
	slog.Warn("OAuth inference failed", "provider", oauthProviderForModel(model), "model", model, "waves", wave, "accounts_tried", accounts, "error", err)
}

func (inference *Inference) call(ctx context.Context, providerName string, credentials TokenSet, accountID, model, conversationID string, request translator.OpenAIRequest, reservationID uint64) (io.ReadCloser, error) {
	defer inference.selector.CancelRequest(reservationID)
	accessToken := credentials.AccessToken
	if accessToken == "" {
		return nil, fmt.Errorf("OAuth account has no access token")
	}
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	model = resolveUpstreamModel(providerName, model)
	var endpoint string
	var payload any
	// rawBody is set by providers whose wire format is not JSON.
	var rawBody []byte
	headers := map[string]string{}
	switch providerName {
	case "codex":
		endpoint = "https://chatgpt.com/backend-api/codex/responses"
		codexPayload := openAIToCodexRequest(request, model)
		warnOnCodexPrefixChange(codexPayload, oauthSessionKey(request, conversationID, "seed_"), model)
		payload = codexPayload
		headers["Accept"] = "text/event-stream"
		headers["originator"] = "codex_cli_rs"
		headers["User-Agent"] = "codex_cli_rs/0.136.0"
		headers["OpenAI-Beta"] = "codex-1"
		if accountID != "" {
			headers["ChatGPT-Account-ID"] = accountID
		}
	case "grok", "xai":
		endpoint = "https://api.x.ai/v1/chat/completions"
		request.Model = model
		payload = request
	case "antigravity":
		// Antigravity is served through the Cloud Code endpoint, which is scoped to the
		// account's project. An account that onboarded without a project cannot serve here;
		// report that clearly rather than attempting the plain Gemini API (which needs a
		// different OAuth scope the Antigravity token does not carry).
		if accountID == "" {
			return nil, fmt.Errorf("Antigravity account has no Cloud Code project — sign in at antigravity.google and create a project, then reconnect")
		}
		endpoint = "https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse"
		request.Model = resolveAntigravityModel(model)
		envelope, buildErr := buildAntigravityEnvelope(request, accountID, conversationID)
		if buildErr != nil {
			return nil, buildErr
		}
		payload = envelope
		headers["Accept"] = "text/event-stream"
		headers["User-Agent"] = "antigravity/1.23.2"
		headers["x-request-source"] = "local"
		headers["X-Client-Name"] = "antigravity"
		headers["X-Client-Version"] = "1.23.2"
	case "claude":
		return nil, fmt.Errorf("Claude OAuth inference is not configured for OpenAI-compatible gateway traffic")
	default:
		return nil, fmt.Errorf("unsupported OAuth provider %q", providerName)
	}
	var encoded []byte
	if rawBody != nil {
		// A provider that does not speak JSON supplies its own bytes; marshalling here
		// would wrap a protobuf body in quotes and produce a request nothing can read.
		encoded = rawBody
	} else {
		marshalled, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		encoded = marshalled
	}
	// Repeated tool output is the last large saving available on providers with no
	// server-side conversation state. Report it rather than act on it: collapsing it
	// rewrites what the model reads, and that is not a change to make on a hunch.
	if duplication := translator.MeasurePromptDuplication(request); duplication.DuplicateToolBytes > 0 {
		slog.Info("prompt carries repeated tool output", "provider", providerName, "model", model,
			"duplicate_results", duplication.DuplicateResults,
			"duplicate_bytes", duplication.DuplicateToolBytes,
			"tool_bytes", duplication.ToolBytes,
			"share_of_prompt", int(duplication.Ratio()*100))
	}
	// The account is what decides whether the upstream prompt cache can hit: each
	// one has its own cache, so a conversation that hops accounts re-pays for the
	// whole prefix. Log it alongside the payload so that is observable.
	slog.Debug("oauth upstream request", "provider", providerName, "model", model,
		"account", accountID, "bytes", len(encoded))
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		slog.Debug("oauth upstream payload", "provider", providerName, "model", model, "bytes", len(encoded), "payload", string(encoded))
	}
	var body io.Reader = bytes.NewReader(encoded)
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+accessToken)
	httpRequest.Header.Set("Content-Type", "application/json")
	// Provider headers are applied last so a non-JSON provider can replace the
	// defaults above rather than fight them.
	for key, value := range headers {
		httpRequest.Header.Set(key, value)
	}
	inference.selector.CommitRequest(reservationID)
	response, err := inference.client.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		return nil, decodeOAuthInferenceError(providerName, response)
	}
	return response.Body, nil
}

func openAIToCodexRequest(request translator.OpenAIRequest, model string) map[string]any {
	input := make([]map[string]any, 0, len(request.Messages))
	instructions := make([]string, 0, 1)
	for _, message := range request.Messages {
		if message.Role == "system" || message.Role == "developer" {
			text := strings.TrimSpace(openAIContentText(message.Content))
			if text == "" {
				continue
			}
			// Only the instruction messages that open the request become `instructions`.
			// That string sits ahead of the conversation and is what the Codex prompt
			// cache is keyed on, so a per-turn block hoisted into it moves the cacheable
			// prefix and the whole system prompt is charged again. A later one is placed
			// in the conversation instead, at the point the client put it.
			if len(input) == 0 {
				instructions = append(instructions, text)
			} else {
				input = append(input, map[string]any{"type": "message", "role": "user",
					"content": codexMessageContent("user", text)})
			}
			continue
		}
		switch message.Role {
		case "user", "assistant":
			content := codexMessageContent(message.Role, message.Content)
			if len(content) > 0 {
				input = append(input, map[string]any{"type": "message", "role": message.Role, "content": content})
			}
			for _, call := range message.ToolCalls {
				input = append(input, map[string]any{"type": "function_call", "call_id": codexCallID(call.ID), "name": call.Function.Name, "arguments": call.Function.Arguments})
			}
		case "tool":
			input = append(input, map[string]any{"type": "function_call_output", "call_id": codexCallID(message.ToolCallID), "output": openAIContentText(message.Content)})
		}
	}
	instruction := strings.Join(instructions, "\n\n")
	if instruction == "" {
		instruction = "You are a helpful coding assistant."
	}
	tools := make([]map[string]any, 0, len(request.Tools))
	for _, tool := range request.Tools {
		definition := map[string]any{
			"type": "function", "name": tool.Function.Name,
			"description": tool.Function.Description, "parameters": tool.Function.Parameters,
		}
		if tool.Function.Strict != nil {
			definition["strict"] = *tool.Function.Strict
		}
		tools = append(tools, definition)
	}
	effort := request.Effort
	if effort == "" {
		effort = "high"
	}
	payload := map[string]any{
		// store must stay false: the Codex backend rejects store:true with HTTP 400,
		// so server-side conversation reuse is not available on this path.
		"model": model, "input": input, "instructions": instruction, "stream": true, "store": false,
		// Measured: dropping the reasoning summary saves no billed output tokens, and
		// output is 0.36% of all tokens spent, so there is nothing to tune here.
		"reasoning": map[string]string{"effort": effort, "summary": "auto"},
	}
	// The "off" override means no reasoning effort at all — the reasoning block is
	// dropped from the payload rather than defaulted to "high" like an absent
	// effort would be.
	if request.Effort == storage.EffortOff {
		delete(payload, "reasoning")
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	// The Codex backend rejects max_output_tokens outright ("Unsupported
	// parameter"), so forwarding the caller's max_tokens fails every request. The
	// turn is bounded by the model's own limits instead.
	if request.ToolChoice != nil {
		payload["tool_choice"] = codexToolChoice(request.ToolChoice)
	}
	if request.ParallelToolCalls != nil {
		payload["parallel_tool_calls"] = *request.ParallelToolCalls
	}
	if len(request.ResponseFormat) > 0 {
		payload["text"] = map[string]any{"format": request.ResponseFormat}
	}
	// Forwarded when the caller sets one, but do not reach for it as a way to raise the
	// cache hit rate. Measured 2026-07-29 over 725M prompt tokens: prompt_cache_key was
	// 5.1% against 3.0% across six A/B rounds, and a Codex-CLI-shaped session id gave
	// exactly 62.5% both on and off. Hits on this backend are binary — 0% or 78-98%,
	// the signature of instance routing — and the same fixture in the same hour swung
	// between them while a prefix-hash detector proved the payload never changed. The
	// only lever that moves from here is sending less conversation.
	if request.PromptCacheKey != "" {
		payload["prompt_cache_key"] = request.PromptCacheKey
	}
	return payload
}

// codexCallID keeps tool-call identifiers within the Responses API's 64-byte
// limit while preserving short IDs unchanged. The hash is deterministic, so a
// later function_call_output still references the matching function_call.
func codexCallID(id string) string {
	if len(id) <= 64 {
		return id
	}
	digest := sha256.Sum256([]byte(id))
	return "call_" + hex.EncodeToString(digest[:])[:58]
}

func codexToolChoice(choice any) any {
	if object, ok := choice.(map[string]any); ok {
		if function, ok := object["function"].(map[string]string); ok {
			if name := function["name"]; name != "" {
				return map[string]any{"type": "function", "name": name}
			}
		}
		if function, ok := object["function"].(map[string]any); ok {
			if name, _ := function["name"].(string); name != "" {
				return map[string]any{"type": "function", "name": name}
			}
		}
	}
	return choice
}

func codexMessageContent(role string, content any) []map[string]any {
	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}
	parts := openAIContentParts(content)
	result := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		if part.Text != "" {
			result = append(result, map[string]any{"type": textType, "text": part.Text})
		}
		if role == "user" && part.ImageURL != nil && part.ImageURL.URL != "" {
			result = append(result, map[string]any{"type": "input_image", "image_url": part.ImageURL.URL})
		}
	}
	return result
}

func openAIContentParts(content any) []translator.OpenAIContentPart {
	switch value := content.(type) {
	case string:
		return []translator.OpenAIContentPart{{Type: "text", Text: value}}
	case []translator.OpenAIContentPart:
		return value
	case []any:
		encoded, _ := json.Marshal(value)
		var parts []translator.OpenAIContentPart
		if json.Unmarshal(encoded, &parts) == nil {
			return parts
		}
	}
	return nil
}

func openAIContentText(content any) string {
	var text strings.Builder
	for _, part := range openAIContentParts(content) {
		text.WriteString(part.Text)
		if part.ImageURL != nil && part.ImageURL.URL != "" {
			// The Cursor agent prompt is a single flat string — the image parts
			// would otherwise be dropped and a transcription call would reach the
			// model with no image at all, which is what made it cogitate and hang.
			// Embedding the data URI lets the vision model actually see the image.
			if strings.HasPrefix(part.ImageURL.URL, "data:") {
				text.WriteString(" [image data: " + part.ImageURL.URL + "]")
			} else {
				text.WriteString(" [image url: " + part.ImageURL.URL + "]")
			}
		}
	}
	return text.String()
}

func firstOpenAIUserMessage(messages []translator.OpenAIMessage) string {
	for _, message := range messages {
		if message.Role == "user" {
			return openAIContentText(message.Content)
		}
	}
	return ""
}

func oauthProviderForModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(model, "ag/") || strings.HasPrefix(model, "antigravity/") || strings.HasPrefix(model, "gemini-") || strings.HasPrefix(model, "gemini/") {
		return "antigravity"
	}
	switch {
	case strings.HasPrefix(model, "xai/"), strings.HasPrefix(model, "grok"):
		return "grok"
	case strings.HasPrefix(model, "claude"), strings.HasPrefix(model, "anthropic"):
		return "claude"
	default:
		return "codex"
	}
}

func resolveAntigravityModel(model string) string {
	model = strings.TrimSpace(model)
	model = strings.TrimPrefix(model, "antigravity/")
	model = strings.TrimPrefix(model, "ag/")
	aliases := map[string]string{
		"gemini-default":             "gemini-3.5-flash-low",
		"gemini-3.5-flash-high":      "gemini-3-flash-agent",
		"gemini-3.5-flash-medium":    "gemini-3.5-flash-low",
		"gemini-3.5-flash-extra-low": "gemini-3.5-flash-extra-low",
		"gemini-3.1-pro-high":        "gemini-pro-agent",
		"gemini-3-pro-high":          "gemini-pro-agent",
		"gemini-3-pro-low":           "gemini-3.1-pro-low",
	}
	if resolved, ok := aliases[strings.ToLower(model)]; ok {
		return resolved
	}
	return model
}

func decodeOAuthInferenceError(providerName string, response *http.Response) error {
	return provider.DecodeProviderError(providerName+" OAuth", response, io.LimitReader(response.Body, 1<<20))
}
