package gateway

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

	"github.com/labstack/echo/v4"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
	"literouter/internal/translator"
)

const maxRequestBody = 16 << 20

type conversationIDKey struct{}
type promptCacheSeedKey struct{}

func withConversationID(c echo.Context) context.Context {
	value := strings.TrimSpace(c.Request().Header.Get("X-Conversation-ID"))
	if value == "" {
		return c.Request().Context()
	}
	return context.WithValue(c.Request().Context(), conversationIDKey{}, value)
}

func conversationID(ctx context.Context) string {
	value, _ := ctx.Value(conversationIDKey{}).(string)
	return value
}

func withPromptCacheSeed(ctx context.Context, request translator.OpenAIRequest) context.Context {
	return withPromptCacheSeedValue(ctx, request.User, firstUserMessage(request.Messages))
}

func withPromptCacheSeedValue(ctx context.Context, user, firstUser string) context.Context {
	if conversationID(ctx) != "" || user != "" || firstUser == "" {
		return ctx
	}
	return context.WithValue(ctx, promptCacheSeedKey{}, firstUser)
}

func promptCacheSeed(ctx context.Context) string {
	value, _ := ctx.Value(promptCacheSeedKey{}).(string)
	return value
}

func (s *Service) Register(e *echo.Echo) {
	e.POST("/v1/chat/completions", s.chatHandler)
	e.POST("/v1/messages", s.messagesHandler)
	e.POST("/v1/messages/count_tokens", s.countTokensHandler)
	e.POST("/v1/responses", s.responsesHandler)
	e.GET("/v1/responses/*", s.responsesUnsupported)
	e.POST("/v1/responses/*", s.responsesUnsupported)
	e.GET("/v1/models", s.modelsHandler)
	e.GET("/v1/models/:id", s.modelHandler)
}

func (s *Service) chatHandler(c echo.Context) error {
	var request translator.OpenAIRequest
	if err := decodeBody(c, &request); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}
	if request.Stream {
		return s.chatStream(c, request)
	}
	response, err := s.Chat(withConversationID(c), request)
	if err != nil {
		return gatewayError(err)
	}
	return c.JSON(http.StatusOK, response)
}

func (s *Service) messagesHandler(c echo.Context) error {
	var request translator.AnthropicRequest
	// The raw body is retained so an Anthropic-native upstream can be handed the
	// caller's exact payload; re-encoding the parsed struct would drop every field
	// LiteRouter does not model, along with the prompt-cache breakpoints.
	raw, err := decodeBodyRaw(c, &request)
	if err != nil {
		return anthropicError(c, http.StatusBadRequest, "invalid request body", "invalid_request_error", nil)
	}
	// raw is deliberately left carrying the caller's model. Only the passthrough
	// path reads it, and that path rewrites the model field to the candidate it
	// actually selected, so patching it here would cost a re-encode of the whole
	// payload to reach the same bytes.
	//
	// len(raw) rather than a token count on purpose: routing only needs to know whether
	// the turn is large, and counting properly means translating the whole history, which
	// is the dominant per-turn cost once a session is big — the exact turns this decides.
	decision, routeErr := s.routeModel(c.Request().Context(), request, len(raw))
	if routeErr != nil {
		// invalid_request_error rather than an api_error: the request as sent cannot be
		// served by any candidate, so a retry is wasted and the client should surface the
		// message to the user instead of treating it as a transient fault.
		logGatewayFailure("/v1/messages", http.StatusBadRequest, "invalid_request_error", routeErr)
		return anthropicError(c, http.StatusBadRequest, routeErr.Error(), "invalid_request_error", routeErr)
	}
	if decision.overrides() {
		// Plan mode fires on every turn while it is active, so it stays at debug. Everything
		// else here is exceptional and changes what the turn costs or what the model can see,
		// which is worth a line the operator will actually find afterwards.
		target := decision.Model
		if target == "" {
			target = request.Model
		}
		if decision.Reason == "plan mode" {
			slog.Debug("router model override", "from", request.Model, "to", target, "reason", decision.Reason)
		} else {
			slog.Info("router model override", "from", request.Model, "to", target,
				"reason", decision.Reason, "request_bytes", len(raw))
		}
		if decision.StripImages {
			// Only the parsed request is rewritten. raw keeps the caller's bytes for the
			// passthrough path, which cannot be reached here: a model declared text-only is
			// not an Anthropic-native one, so that path never claims this turn.
			var stripped int
			request, stripped = stripUnreadableImages(request)
			slog.Warn("images omitted for a text-only model",
				"model", target, "images", stripped)
		}
		if decision.Model != "" {
			request.Model = decision.Model
		}
	}
	logAnthropicPayload("ingress", request, int64(len(raw)), 0, int64(len(raw)))
	if request.Stream {
		return s.messagesStream(c, request, raw)
	}
	response, err := s.Messages(withConversationID(c), request)
	if err != nil {
		return messagesGatewayError(c, err)
	}
	return c.JSON(http.StatusOK, response)
}

func (s *Service) countTokensHandler(c echo.Context) error {
	var request translator.AnthropicRequest
	if err := decodeBody(c, &request); err != nil {
		return anthropicError(c, http.StatusBadRequest, "invalid request body", "invalid_request_error", nil)
	}
	unified, err := translator.FromAnthropicRequest(request)
	if err != nil {
		return anthropicError(c, http.StatusBadRequest, err.Error(), "invalid_request_error", err)
	}
	return c.JSON(http.StatusOK, map[string]int{"input_tokens": contextguard.EstimateRequest(unified)})
}

func (s *Service) responsesHandler(c echo.Context) error {
	if strings.EqualFold(c.Request().Header.Get("Upgrade"), "websocket") {
		return responsesError(c, http.StatusNotImplemented, "Responses WebSocket transport is unsupported", "unsupported_endpoint")
	}
	var request translator.ResponsesRequest
	if err := decodeBody(c, &request); err != nil {
		return responsesError(c, http.StatusBadRequest, "invalid request body", "invalid_request_error")
	}
	upstream, err := translator.FromResponsesRequest(request)
	if err != nil {
		return responsesError(c, http.StatusBadRequest, err.Error(), "invalid_request_error")
	}
	if request.Stream {
		return s.responsesStream(c, request, upstream)
	}
	response, err := s.chat(withConversationID(c), upstream, "/v1/responses")
	if err != nil {
		return gatewayError(err)
	}
	return c.JSON(http.StatusOK, translator.ToResponsesResponse(response, request))
}

func (s *Service) responsesUnsupported(c echo.Context) error {
	return responsesError(c, http.StatusNotImplemented, "LiteRouter implements POST /v1/responses only; response retrieval, cancellation, and compaction are unsupported", "unsupported_endpoint")
}

func responsesError(c echo.Context, status int, message, code string) error {
	return c.JSON(status, map[string]any{"error": map[string]any{
		"message": message, "type": "invalid_request_error", "param": nil, "code": code,
	}})
}

func (s *Service) modelsHandler(c echo.Context) error {
	models := make([]map[string]any, 0, len(s.models))
	for _, model := range s.models {
		models = append(models, modelMetadata(model))
	}
	return c.JSON(http.StatusOK, map[string]any{
		"data": models, "has_more": false, "first_id": firstModelID(s.models), "last_id": lastModelID(s.models),
	})
}

func (s *Service) modelHandler(c echo.Context) error {
	id := c.Param("id")
	for _, model := range s.models {
		if model == id {
			return c.JSON(http.StatusOK, modelMetadata(model))
		}
	}
	return anthropicError(c, http.StatusNotFound, "model not found", "not_found_error", nil)
}

func modelMetadata(id string) map[string]any {
	return map[string]any{"type": "model", "id": id, "display_name": id}
}

func firstModelID(models []string) any {
	if len(models) == 0 {
		return nil
	}
	return models[0]
}

func lastModelID(models []string) any {
	if len(models) == 0 {
		return nil
	}
	return models[len(models)-1]
}

func decodeBody(c echo.Context, target any) error {
	_, err := decodeBodyBytes(c, target)
	return err
}

func decodeBodyRaw(c echo.Context, target any) ([]byte, error) {
	request := c.Request()
	request.Body = http.MaxBytesReader(c.Response(), request.Body, maxRequestBody)
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return raw, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return raw, err
		}
		return raw, errors.New("multiple JSON values are not allowed")
	}
	return raw, nil
}

func decodeBodyBytes(c echo.Context, target any) (int64, error) {
	request := c.Request()
	request.Body = http.MaxBytesReader(c.Response(), request.Body, maxRequestBody)
	counter := &countingReader{reader: request.Body}
	decoder := json.NewDecoder(counter)
	if err := decoder.Decode(target); err != nil {
		return counter.bytes, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return counter.bytes, err
		}
		return counter.bytes, errors.New("multiple JSON values are not allowed")
	}
	return counter.bytes, nil
}

type countingReader struct {
	reader io.Reader
	bytes  int64
}

func (reader *countingReader) Read(buffer []byte) (int, error) {
	n, err := reader.reader.Read(buffer)
	reader.bytes += int64(n)
	return n, err
}

// logAnthropicPayload measures the request by marshalling it four times over. On a
// six-figure-token coding turn that is megabytes of work per request, so it only
// runs when someone is actually reading debug output.
func logAnthropicPayload(stage string, request translator.AnthropicRequest, ingressBytes int64, attempt int, previousBytes int64) int64 {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return previousBytes
	}
	systemBytes := jsonBytes(request.System)
	messageBytes := jsonBytes(request.Messages)
	toolBytes := jsonBytes(request.Tools)
	totalBytes := jsonBytes(request)
	slog.Debug("Anthropic payload", "stage", stage, "model", request.Model, "ingress_bytes", ingressBytes,
		"total_bytes", totalBytes, "system_bytes", systemBytes, "messages_bytes", messageBytes,
		"tools_bytes", toolBytes, "message_count", len(request.Messages), "tool_count", len(request.Tools),
		"attempt", attempt, "previous_bytes", previousBytes, "growth_bytes", totalBytes-previousBytes)
	return totalBytes
}

func jsonBytes(value any) int64 {
	encoded, err := json.Marshal(value)
	if err != nil {
		return -1
	}
	return int64(len(encoded))
}

func messagesGatewayError(c echo.Context, err error) error {
	status, message, errorType := gatewayErrorDetails(err)
	// A failed turn used to produce no log line at all: the caller saw an API error
	// and there was nothing on this side to explain it. Always record the cause.
	logGatewayFailure("/v1/messages", status, errorType, err)
	return anthropicError(c, status, message, errorType, err)
}

// messagesContextOverflowError reports an over-window request with the token
// counts attached. Claude Code parses "<actual> tokens > <limit>" out of the
// message to size the summary its reactive compaction needs; without the numbers
// it still compacts, but cannot tell how much of the session to drop.
func messagesContextOverflowError(c echo.Context, err error, promptTokens, window int) error {
	status, message, errorType := gatewayErrorDetails(err)
	if status == http.StatusBadRequest && promptTokens > 0 && window > 0 && promptTokens > window {
		message = fmt.Sprintf("prompt is too long: %d tokens > %d tokens (proxy estimate; upstream rejected the request)",
			promptTokens, window)
	}
	logGatewayFailure("/v1/messages", status, errorType, err)
	return anthropicError(c, status, message, errorType, err)
}

// resolveContextWindow reports the window LiteRouter believes the model has.
//
// Configuration and the catalog supply the opening bid; runtime evidence overrides
// it, on the reasoning set out in contextwindow.go. Every consumer goes through here
// — the guard, the summarizer, and the overflow message the client compacts against
// — so a window learned once is not still being contradicted somewhere else.
func (s *Service) resolveContextWindow(ctx context.Context, model string) int {
	window, _ := s.resolveContextWindowErr(ctx, model)
	return window
}

// resolveContextWindowErr is resolveContextWindow for the one caller that treats a
// failed catalog lookup as fatal rather than as a missing value. Split out so that
// caller does not have to look the window up a second time to see the error — the
// lookup scans the catalog by model prefix, and this runs on every guarded turn.
func (s *Service) resolveContextWindowErr(ctx context.Context, model string) (int, error) {
	base := 0
	var lookupErr error
	if s.contextWindow != nil {
		window, err := s.contextWindow(ctx, model)
		if err != nil {
			lookupErr = err
		} else if window > 0 {
			base = window
		}
	}
	if base <= 0 {
		base = s.contextLimits.Window(model)
	}
	ceiling, floor := s.learnedContextBounds(model)
	switch {
	case ceiling > 0 && floor > ceiling:
		// A ceiling below a prompt the upstream actually served was misread. The
		// observation is a fact about a completed request; prefer it.
		return floor, lookupErr
	case ceiling > 0:
		return ceiling, lookupErr
	case floor > base:
		return floor, lookupErr
	default:
		return base, lookupErr
	}
}

func logGatewayFailure(endpoint string, status int, errorType string, err error) {
	attributes := []any{"endpoint", endpoint, "status", status, "error_type", errorType, "error", err}
	var providerError *provider.ProviderError
	if errors.As(err, &providerError) {
		attributes = append(attributes,
			"upstream_provider", providerError.Provider,
			"upstream_status", providerError.StatusCode,
			"upstream_code", providerError.Code,
			"upstream_message", providerError.Message)
	}
	slog.Warn("gateway returned an error", attributes...)
}

// anthropicError writes the Anthropic error envelope. cause is what the status was
// derived from — nil when the status is self-explanatory — and is used only to tell
// the client whether retrying can help.
func anthropicError(c echo.Context, status int, message, errorType string, cause error) error {
	applyRetryAdvice(c, status, cause)
	return c.JSON(status, map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errorType, "message": message},
	})
}

func gatewayErrorDetails(err error) (status int, message, errorType string) {
	switch {
	case errors.Is(err, context.Canceled):
		return 499, "request canceled by client", "request_aborted"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, errUpstreamStreamIdle):
		return http.StatusGatewayTimeout, "upstream stream timed out", "timeout_error"
	case errors.Is(err, contextguard.ErrBudgetExceeded):
		// Reached only after summarization and deterministic turn-trimming both fail,
		// so the request genuinely does not fit. Report it the way the Anthropic API
		// does: clients recognise this shape and compact instead of retrying blindly.
		return http.StatusBadRequest, "prompt is too long", "invalid_request_error"
	case errors.Is(err, ErrProviderUnavailable):
		return http.StatusServiceUnavailable, "provider is not configured", "api_error"
	case errors.Is(err, io.EOF):
		return http.StatusBadGateway, "upstream returned an empty response", "api_error"
	}
	var providerError *provider.ProviderError
	if errors.As(err, &providerError) {
		status = providerError.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		errorType = "api_error"
		if isUpstreamContextError(providerError) {
			// Report an upstream context rejection exactly as the Anthropic API does.
			// Clients key their own compaction off this shape; anything else — 413, or
			// a 502 api_error — reads as a server fault and gets retried unchanged,
			// which is how an over-long session turns into an endless retry loop.
			return http.StatusBadRequest, "prompt is too long: " + providerError.Message, "invalid_request_error"
		}
		if status == http.StatusRequestEntityTooLarge {
			// Generic 413 often means HTTP body infrastructure, not model context.
			status = http.StatusUnprocessableEntity
			errorType = "api_error"
		} else if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
			errorType = "invalid_request_error"
		} else if status == http.StatusTooManyRequests {
			errorType = "rate_limit_error"
		} else if status == http.StatusUnauthorized || status == http.StatusForbidden {
			errorType = "authentication_error"
		}
		return status, providerError.Message, errorType
	}
	return http.StatusBadGateway, "upstream request failed", "api_error"
}

func isUpstreamContextError(err *provider.ProviderError) bool {
	if err == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(err.Code)) {
	case "context_length_exceeded", "max_tokens_exceeded":
		return true
	}
	// Not every upstream sets a code; some only phrase it in the message. Getting
	// this wrong costs a retry loop, so match the wording too.
	message := strings.ToLower(err.Message)
	for _, phrase := range []string{
		"exceeds the context window",
		"maximum context length",
		"context_length_exceeded",
		"prompt is too long",
		"too many tokens",
		// "input length and max_tokens exceed context limit: 500000 + 8192 > 400000" —
		// phrased by upstreams that count the output budget against the window. Missing
		// it meant the turn was reported as a plain 400, which the client retries
		// unchanged instead of compacting.
		"context limit",
	} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

// isContextOverflow reports whether err is a context-window rejection from any
// layer. Retrying such a stream on another account cannot help — every candidate
// is handed the same conversation — so the retry waves are skipped for it.
func isContextOverflow(err error) bool {
	if errors.Is(err, contextguard.ErrBudgetExceeded) {
		return true
	}
	var providerError *provider.ProviderError
	return errors.As(err, &providerError) && isUpstreamContextError(providerError)
}

func gatewayError(err error) error {
	status, message, errorType := gatewayErrorDetails(err)
	logGatewayFailure("openai-compatible", status, errorType, err)
	return echo.NewHTTPError(status, message)
}
