package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/labstack/echo/v4"

	"literouter/internal/translator"
)

// anthropicPassthroughStream serves an Anthropic request on an Anthropic-native
// upstream without translating it. Everything the OpenAI shape cannot carry —
// prompt-cache breakpoints, thinking signatures, the model's own stop_reason and
// token counts — survives only on this path, so it is tried first whenever the
// resolved model supports it.
//
// It reports handled=false when no candidate could serve the turn and nothing has
// been written yet, leaving the translated path free to try the rest of the chain.
func (s *Service) anthropicPassthroughStream(c echo.Context, request translator.AnthropicRequest, raw []byte) (bool, error) {
	if s.oauthInference == nil || len(raw) == 0 {
		return false, nil
	}
	// Deployments that route everything to Codex or Gemini never reach an
	// Anthropic-native candidate, so nothing here may cost them work. The payload
	// is only rebuilt once a candidate actually claims the turn.
	var affinity string
	attempted := false
	var lastErr error
	// Caps apply here too. An Anthropic-native model usually accepts what Claude Code
	// asks for, but not always — Haiku's ceiling is lower than Opus's — and an explicit
	// router.max_output_tokens entry is a decision that must hold on every path.
	// appliedLimit tracks what the cached payload was built for, so it is only rebuilt
	// when a candidate needs a different budget.
	var payload []byte
	appliedLimit := -1
	clampAttempts := map[string]int{}
	chain := s.modelChain(request.Model)
	for index := 0; index < len(chain); index++ {
		model := chain[index]
		if !s.oauthInference.SupportsAnthropicPassthrough(model) {
			continue
		}
		limit := s.outputLimit(model)
		if payload == nil || limit != appliedLimit {
			built, err := forceAnthropicStream(raw, limit)
			if err != nil {
				return false, nil
			}
			payload, appliedLimit = built, limit
			// Anthropic caches per account, so a turn landing on a different account
			// pays a cold prefix. Give the selector a stable affinity key either way.
			affinity = conversationID(withConversationID(c))
			if affinity == "" {
				affinity = firstAnthropicUserText(request)
			}
		}
		attempted = true
		betas := c.Request().Header.Get("anthropic-beta")
		body, err := s.oauthInference.DoAnthropicStream(c.Request().Context(), payload, model, affinity, betas)
		if err != nil {
			lastErr = err
			// Learned here too, even though this path forwards the caller's own bytes and so
			// cannot trim them: the window recorded is what later turns are budgeted against
			// and what the dashboard's auto max-context resolves to, whichever endpoint
			// eventually serves them.
			s.learnContextWindow(model, 0, err)
			// Nothing has been written yet, so the rejection can be absorbed: learn the
			// cap and re-attempt this candidate with a payload built for it.
			if s.learnOutputLimit(model, anthropicMaxTokens(raw, appliedLimit), clampAttempts[model], err) {
				clampAttempts[model]++
				index--
				continue
			}
			continue
		}
		relay := &anthropicRelay{openBlock: -1, model: model}
		readErr := withStreamIdleTimeout(c.Request().Context(), body, upstreamStreamIdleTimeout, func(reader io.Reader) error {
			return relay.copy(c, reader)
		})
		_ = body.Close()
		if c.Request().Context().Err() != nil || errors.Is(readErr, context.Canceled) {
			return true, nil
		}
		if readErr == nil && !relay.delivered {
			readErr = errEmptyUpstreamStream
		}
		if readErr != nil {
			if !relay.delivered {
				lastErr = readErr
				continue
			}
			slog.Warn("anthropic passthrough ended early", "model", relay.reportedModel(), "status", streamErrorStatus(readErr), "error", readErr)
			s.recordUsage(relay.usageEvent(streamErrorStatus(readErr)))
			return true, relay.closeTruncated(c)
		}
		// A bytes-only calibration sample: this path never translates, so it has no
		// estimate, but the payload length against the upstream's own count teaches
		// the byte ratio that long-context routing thresholds ride on.
		s.observeTokenScale(model, len(payload), 0, relay.promptTotal())
		s.recordUsage(relay.usageEvent(""))
		return true, relay.closeTruncated(c)
	}
	if !attempted {
		return false, nil
	}
	// Every Anthropic-native candidate failed before producing output. Nothing has
	// reached the client, so the translated path can still serve the rest of chain.
	slog.Warn("anthropic passthrough unavailable; falling back to translation", "model", request.Model, "error", lastErr)
	return false, nil
}

// forceAnthropicStream keeps the caller's body byte-for-byte apart from the fields
// the gateway owns. Re-encoding from the parsed struct would silently drop any field
// LiteRouter does not model, which is exactly the fidelity being bought.
//
// A limit of zero or less leaves max_tokens alone; the cap is only ever lowered, so a
// model that accepts the caller's budget keeps it.
func forceAnthropicStream(raw []byte, limit int) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	fields["stream"] = json.RawMessage("true")
	if limit > 0 && decodeAnthropicMaxTokens(fields) > limit {
		encoded, err := json.Marshal(limit)
		if err != nil {
			return nil, err
		}
		fields["max_tokens"] = encoded
	}
	return json.Marshal(fields)
}

// anthropicMaxTokens reports the output budget the payload was actually sent with,
// which is what the upstream compared its cap against.
func anthropicMaxTokens(raw []byte, appliedLimit int) int {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return appliedLimit
	}
	requested := decodeAnthropicMaxTokens(fields)
	if appliedLimit > 0 && appliedLimit < requested {
		return appliedLimit
	}
	return requested
}

func decodeAnthropicMaxTokens(fields map[string]json.RawMessage) int {
	value, ok := fields["max_tokens"]
	if !ok {
		return 0
	}
	var maxTokens int
	if json.Unmarshal(value, &maxTokens) != nil {
		return 0
	}
	return maxTokens
}

func firstAnthropicUserText(request translator.AnthropicRequest) string {
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		for _, block := range message.Content {
			if block.Type == "text" && block.Text != "" {
				return block.Text
			}
		}
	}
	return ""
}

type anthropicRelay struct {
	model         string
	upstreamModel string
	delivered     bool
	sawStop       bool
	openBlock     int
	inputTokens   int
	outputTokens  int
	cacheRead     int
	cacheWrite    int
}

// copy forwards the upstream SSE verbatim. Re-serialising events would risk
// reordering or dropping fields the client depends on, so the bytes are relayed
// unchanged and only observed in passing for usage and truncation state.
func (relay *anthropicRelay) copy(c echo.Context, reader io.Reader) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxSSELineBytes)
	event := ""
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			relay.observe(event, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if !relay.delivered {
			relay.delivered = true
			prepareSSE(c)
		}
		if err := writeSSELine(c, line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return errSSELineTooLarge
		}
		return fmt.Errorf("read Anthropic SSE: %w", err)
	}
	return nil
}

func (relay *anthropicRelay) observe(event, data string) {
	switch event {
	case "message_start":
		var payload struct {
			Message struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(data), &payload) != nil {
			return
		}
		relay.upstreamModel = payload.Message.Model
		relay.inputTokens = payload.Message.Usage.InputTokens
		relay.cacheRead = payload.Message.Usage.CacheReadInputTokens
		relay.cacheWrite = payload.Message.Usage.CacheCreationInputTokens
	case "content_block_start":
		var payload struct {
			Index int `json:"index"`
		}
		if json.Unmarshal([]byte(data), &payload) == nil {
			relay.openBlock = payload.Index
		}
	case "content_block_stop":
		relay.openBlock = -1
	case "message_delta":
		var payload struct {
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &payload) == nil && payload.Usage.OutputTokens > 0 {
			relay.outputTokens = payload.Usage.OutputTokens
		}
	case "message_stop":
		relay.sawStop = true
	}
}

// closeTruncated completes a message the upstream left open. As on the translated
// path, the turn is closed as truncated rather than as an error, because an
// `event: error` after partial output is what clients report as a server failure.
func (relay *anthropicRelay) closeTruncated(c echo.Context) error {
	if relay.sawStop || !relay.delivered {
		return nil
	}
	if relay.openBlock >= 0 {
		if err := writeSSE(c, "content_block_stop", map[string]any{"type": "content_block_stop", "index": relay.openBlock}); err != nil {
			return err
		}
		relay.openBlock = -1
	}
	if err := writeSSE(c, "message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": "max_tokens"},
		"usage": map[string]int{"output_tokens": relay.outputTokens},
	}); err != nil {
		return err
	}
	relay.sawStop = true
	return writeSSE(c, "message_stop", map[string]string{"type": "message_stop"})
}

// promptTotal is the full prompt size the upstream counted: Anthropic reports
// cache reads and writes separately from input_tokens, and all three were sent.
func (relay *anthropicRelay) promptTotal() int {
	return relay.inputTokens + relay.cacheRead + relay.cacheWrite
}

func (relay *anthropicRelay) reportedModel() string {
	if relay.upstreamModel != "" {
		return relay.upstreamModel
	}
	return relay.model
}

func (relay *anthropicRelay) usageEvent(status string) UsageEvent {
	return UsageEvent{
		Provider: "claude", Model: relay.reportedModel(), Endpoint: "/v1/messages", Status: status,
		PromptTokens: relay.inputTokens, CompletionTokens: relay.outputTokens,
		CachedTokens: relay.cacheRead, CachedTokensReported: true,
	}
}

func writeSSELine(c echo.Context, line string) error {
	response := c.Response()
	if _, err := response.Write([]byte(line + "\n")); err != nil {
		return err
	}
	if line == "" {
		response.Flush()
	}
	return nil
}
