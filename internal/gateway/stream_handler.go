package gateway

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
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
	"literouter/internal/toolvalidate"
	"literouter/internal/translator"
)

func (s *Service) chatStream(c echo.Context, request translator.OpenAIRequest) error {
	if request.Model == "" || len(request.Messages) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "model and messages are required")
	}
	request.Stream = true
	opener := s.newStreamOpener(c, request)
	for {
		body, err := opener.next()
		if err != nil {
			return gatewayError(err)
		}
		delivered := false
		var lastUsage translator.OpenAIUsage
		var output strings.Builder
		lastModel := request.Model
		readErr := readOpenAIStreamWithIdleTimeout(c.Request().Context(), body, upstreamStreamIdleTimeout, func(chunk OpenAIStreamChunk) error {
			if !delivered {
				delivered = true
				prepareSSE(c)
			}
			if chunk.Model != "" {
				lastModel = chunk.Model
			}
			if chunk.Usage != nil {
				lastUsage = *chunk.Usage
			}
			for _, choice := range chunk.Choices {
				output.WriteString(choice.Delta.Content)
				output.WriteString(choice.Delta.Reasoning)
			}
			payload, err := json.Marshal(chunk)
			if err != nil {
				return err
			}
			return writeSSEData(c, payload)
		})
		_ = body.Close()
		if c.Request().Context().Err() != nil || errors.Is(readErr, context.Canceled) {
			return nil
		}
		if readErr != nil {
			if !delivered && !opener.exhausted() && !isContextOverflow(readErr) {
				slog.Warn("retrying upstream stream before first token", "endpoint", "/v1/chat/completions", "model", lastModel, "error", readErr)
				continue
			}
			if !delivered {
				return gatewayError(readErr)
			}
			// Bytes already reached the client. An SSE error frame here is what the
			// caller reports as a mid-response server error, so close the stream
			// cleanly instead and let the caller keep the partial completion.
			slog.Warn("upstream stream ended early", "endpoint", "/v1/chat/completions", "model", lastModel, "status", streamErrorStatus(readErr), "error", readErr)
			s.recordUsage(UsageEvent{Provider: s.providerNameFor(request.Model), Model: lastModel, Endpoint: "/v1/chat/completions", Status: streamErrorStatus(readErr)})
			return writeSSERaw(c, "[DONE]")
		}
		if !delivered {
			prepareSSE(c)
		}
		lastUsage, promptEstimated, completionEstimated := estimateStreamUsage(request, output.String(), lastUsage)
		s.recordUsage(UsageEvent{
			Provider: s.providerNameFor(request.Model), Model: lastModel, RequestModel: request.Model, Endpoint: "/v1/chat/completions",
			PromptTokens: lastUsage.PromptTokens, CompletionTokens: lastUsage.CompletionTokens,
			CachedTokens:          lastUsage.PromptTokensDetails.CachedTokens,
			PromptTokensEstimated: promptEstimated, CompletionTokensEstimated: completionEstimated,
			CachedTokensReported: lastUsage.PromptTokensDetails.CachedTokensReported,
		})
		return writeSSERaw(c, "[DONE]")
	}
}

func streamErrorStatus(err error) string {
	switch {
	case errors.Is(err, errUpstreamStreamIdle):
		return "idle_timeout"
	case errors.Is(err, errEmptyUpstreamStream):
		return "empty_stream"
	case errors.Is(err, errSSELineTooLarge), errors.Is(err, errSSEEventTooLarge):
		return "malformed_stream"
	case isContextOverflow(err):
		// The request was larger than the model accepts. That is a valid, actionable
		// answer rather than a proxy fault, so it must not be counted as a broken
		// stream — doing so inflated the very metric used to judge stability.
		return "context_overflow"
	default:
		return "malformed_stream"
	}
}

func estimateTextBytes(bytes int) int {
	if bytes <= 0 {
		return 0
	}
	return (bytes + 3) / 4
}

func estimateStreamUsage(request translator.OpenAIRequest, output string, usage translator.OpenAIUsage) (translator.OpenAIUsage, bool, bool) {
	// Synthetic OAuth terminal chunks sometimes include usage:{prompt_tokens:0,...}.
	// That sets Reported=true via UnmarshalJSON and previously blocked estimation.
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		usage.PromptTokensReported = false
		usage.CompletionTokensReported = false
	}
	promptEstimated := !usage.PromptTokensReported
	completionEstimated := !usage.CompletionTokensReported
	if promptEstimated {
		if unified, err := translator.FromOpenAIRequest(request); err == nil {
			usage.PromptTokens = contextguard.EstimateRequest(unified)
		}
	}
	if completionEstimated {
		usage.CompletionTokens = contextguard.EstimateText(output)
	}
	return usage, promptEstimated, completionEstimated
}

func (s *Service) messagesStream(c echo.Context, request translator.AnthropicRequest, raw []byte) error {
	s.warnOnInflatedWindowBelief(c, request.Model)
	// An Anthropic-native upstream can serve this byte-for-byte. Try that first:
	// translation is what costs the prompt cache, the thinking signatures, and the
	// model's own stop_reason.
	if handled, err := s.anthropicPassthroughStream(c, request, raw); handled {
		return err
	}
	unified, err := translator.FromAnthropicRequest(request)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	upstream, err := translator.ToOpenAIRequest(unified)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	toolSchemas := toolvalidate.Compile(upstream.Tools)
	upstream.Stream = true
	// Claude Code drives its own compaction off the input token count it sees on
	// the wire. Reporting zero left it blind, so it kept growing the context until
	// LiteRouter rejected the turn instead of compacting first.
	promptTokens := contextguard.EstimateRequest(unified)
	opener := s.newUnifiedStreamOpener(c, upstream, unified)
	for {
		body, err := opener.next()
		if err != nil {
			return messagesGatewayError(c, err)
		}
		state := &anthropicStreamState{tools: make(map[int]*bufferedToolCall), toolSchemas: toolSchemas,
			promptTokens: promptTokens, toolsAllowed: len(upstream.Tools) > 0}
		readErr := readOpenAIStreamWithIdleTimeout(c.Request().Context(), body, upstreamStreamIdleTimeout, func(chunk OpenAIStreamChunk) error {
			return state.emit(c, chunk)
		})
		_ = body.Close()
		if c.Request().Context().Err() != nil || errors.Is(readErr, context.Canceled) {
			return nil
		}
		if readErr == nil {
			switch {
			case !state.producedOutput():
				readErr = errEmptyUpstreamStream
			case !state.terminalSent:
				readErr = fmt.Errorf("upstream stream ended without finish reason")
			}
		}
		if readErr != nil {
			// Retrying is only invisible while no bytes have reached the client, and is
			// only right when there is nothing worth salvaging: a buffered tool call is
			// a usable turn and must be delivered rather than thrown away. A context
			// overflow is deterministic across candidates, so it is never retried.
			retryable := !state.started && !state.producedOutput()
			if retryable && !opener.exhausted() && !isContextOverflow(readErr) {
				slog.Warn("retrying upstream stream before first token", "endpoint", "/v1/messages", "model", request.Model, "error", readErr)
				continue
			}
			// Out of candidates, but an output-free turn is worth asking the same one
			// again: it is nondeterministic, and the alternative is an empty turn that
			// stops the agent.
			if retryable && errors.Is(readErr, errEmptyUpstreamStream) && opener.replayCurrent() {
				slog.Warn("upstream produced no output; retrying the same candidate",
					"endpoint", "/v1/messages", "model", request.Model, "attempt", opener.replays)
				continue
			}
			// Same reasoning for an explicitly transient upstream failure. A model with
			// no alias chain has exactly one candidate, so "no candidates left" was
			// reached on the first overloaded response and the caller saw a 502 for a
			// condition the upstream itself described as temporary. Back off briefly so
			// the retry is not part of the overload.
			if retryable && transientUpstreamError(readErr) && opener.replayCurrent() {
				delay := time.Duration(opener.replays) * transientRetryBackoff
				slog.Warn("upstream reported a transient failure; retrying the same candidate after backoff",
					"endpoint", "/v1/messages", "model", request.Model,
					"attempt", opener.replays, "backoff", delay, "error", readErr)
				select {
				case <-time.After(delay):
				case <-c.Request().Context().Done():
					return nil
				}
				continue
			}
			if retryable {
				// Failures carried no token count, so every failed turn landed in the
				// smallest bucket and made "does this break at large context?"
				// unanswerable from the data. Record the estimate here too.
				s.recordUsage(UsageEvent{Provider: s.providerNameFor(request.Model), Model: request.Model, Endpoint: "/v1/messages",
					Status: streamErrorStatus(readErr), PromptTokens: promptTokens, PromptTokensEstimated: true})
				if errors.Is(readErr, errEmptyUpstreamStream) {
					// Every candidate produced an output-free turn. The client still needs a
					// well-formed message, so fall back to the empty-turn padding here —
					// after the retries, not instead of them.
					slog.Warn("no candidate produced any output; returning an empty turn",
						"endpoint", "/v1/messages", "model", request.Model)
					return state.finishEmptyTurn(c)
				}
				if isContextOverflow(readErr) {
					// Learn before anything else: the number the upstream just named is the
					// window, and reading it here means the budget the trim below aims at —
					// and the figure handed to the client if the trim cannot save the turn —
					// uses the real limit rather than the catalog's guess about it.
					s.learnContextWindow(request.Model, opener.sentTokens(), readErr)
					// Same backstop as at open time. An upstream that accepts the connection
					// and only then refuses the prompt lands here instead, and the turn is
					// just as salvageable.
					if opener.retryAfterTrimmingContext(c.Request().Context(), request.Model, readErr) {
						continue
					}
					return messagesContextOverflowError(c, readErr, promptTokens,
						s.resolveContextWindow(c.Request().Context(), request.Model))
				}
				return messagesGatewayError(c, readErr)
			}
			return s.closeBrokenStream(c, request, state, readErr)
		}
		if err := state.finish(c); err != nil {
			return err
		}
		usage := state.lastUsage
		if usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
			usage.PromptTokensReported = false
			usage.CompletionTokensReported = false
		}
		promptEstimated := !usage.PromptTokensReported
		completionEstimated := !usage.CompletionTokensReported
		if promptEstimated {
			usage.PromptTokens = promptTokens
		}
		if completionEstimated {
			usage.CompletionTokens = estimateTextBytes(state.completionBytes)
		}
		// The one place all three numbers for the same payload exist at once: the bytes
		// that arrived, what LiteRouter thought they were, and what the upstream says they
		// were. Only useful when the last of those is real, which is why it is gated on the
		// count having been reported rather than filled in above.
		//
		// "The same payload" is the whole point, and it used not to hold: promptTokens and
		// raw describe what the client sent, while the reported count describes what the
		// pipeline sent after compacting it. Every mutated turn taught the compaction ratio
		// as if it were the tokenizer's, which inflated the scale, inflated the budget the
		// guard believed it had, and kept spread too high for the measurement to ever be
		// trusted. When the pipeline rewrote the payload, its own estimate is the only one
		// that matches, and the ingress byte count no longer matches anything.
		if !promptEstimated {
			estimate, ingressBytes := promptTokens, len(raw)
			if opener.sentEstimate > 0 {
				estimate, ingressBytes = opener.sentEstimate, 0
			}
			s.observeTokenScale(request.Model, ingressBytes, estimate, usage.PromptTokens)
		}
		s.recordUsage(UsageEvent{
			Provider: s.providerNameFor(request.Model), Model: request.Model, Endpoint: "/v1/messages",
			PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
			CachedTokens:          usage.PromptTokensDetails.CachedTokens,
			PromptTokensEstimated: promptEstimated, CompletionTokensEstimated: completionEstimated,
			CachedTokensReported: usage.PromptTokensDetails.CachedTokensReported,
		})
		return writeSSE(c, "message_stop", map[string]string{"type": "message_stop"})
	}
}

// closeBrokenStream ends a partially delivered Anthropic message without writing
// an `event: error` frame. Claude Code treats a mid-stream error event as a failed
// turn and reports "Server error mid-response"; a well-formed truncation lets the
// caller keep what it already received and continue from there.
func (s *Service) closeBrokenStream(c echo.Context, request translator.AnthropicRequest, state *anthropicStreamState, cause error) error {
	slog.Warn("upstream stream ended early", "endpoint", "/v1/messages", "model", request.Model,
		"status", streamErrorStatus(cause), "prompt_tokens", state.promptTokens, "error", cause)
	s.recordUsage(UsageEvent{
		Provider: s.providerNameFor(request.Model), Model: request.Model, Endpoint: "/v1/messages",
		Status: streamErrorStatus(cause), PromptTokens: state.promptTokens, PromptTokensEstimated: true,
		CompletionTokens: estimateTextBytes(state.completionBytes), CompletionTokensEstimated: true,
	})
	if err := state.abort(c); err != nil {
		return err
	}
	return writeSSE(c, "message_stop", map[string]string{"type": "message_stop"})
}

type anthropicStreamState struct {
	started      bool
	terminalSent bool
	sawTool      bool
	emittedTool  bool
	nextIndex    int
	activeIndex  int
	activeType   string
	promptTokens int
	tools        map[int]*bufferedToolCall
	toolSchemas  toolvalidate.Schemas
	// toolsAllowed records whether the caller declared any tool. Gemini imitates the
	// functionCall parts still present in a replayed history and calls a tool even
	// when none is offered; forwarding that produces a turn with a tool_use the
	// client has no schema for and no text at all, which a /compact request reports
	// as an empty response.
	toolsAllowed    bool
	toolOrder       []int
	lastUsage       translator.OpenAIUsage
	completionBytes int
	messageID       string
	messageModel    string
}

type bufferedToolCall struct {
	id        string
	name      string
	arguments string
}

func (s *anthropicStreamState) emit(c echo.Context, chunk OpenAIStreamChunk) error {
	// Identity is recorded but the message is not opened yet. Committing
	// message_start on the first chunk meant a terminal-only chunk — an upstream that
	// produced no text and no tool call — already had a message on the wire, so it
	// could no longer be retried and was finished off with a placeholder instead.
	if chunk.ID != "" {
		s.messageID = chunk.ID
	}
	if chunk.Model != "" {
		s.messageModel = chunk.Model
	}
	if chunk.Usage != nil {
		s.lastUsage = *chunk.Usage
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Reasoning != "" {
			// OpenAI reasoning_content has no Anthropic thinking signature, so it used
			// to be forwarded as assistant text. That put internal deliberation into
			// the transcript, where the model re-read it as its own final answer and
			// repeated work it had already done. Count it for usage and drop it.
			s.completionBytes += len(choice.Delta.Reasoning)
		}
		if choice.Delta.Content != "" {
			s.completionBytes += len(choice.Delta.Content)
			if err := s.flushTools(c); err != nil {
				return err
			}
			if err := s.contentDelta(c, "text", map[string]any{"type": "text_delta", "text": choice.Delta.Content}); err != nil {
				return err
			}
		}
		for _, call := range choice.Delta.ToolCalls {
			if !s.toolsAllowed {
				slog.Warn("dropping upstream tool call for a request that declared no tools",
					"tool", call.Function.Name)
				continue
			}
			if err := s.toolDelta(c, call); err != nil {
				return err
			}
		}
		if choice.FinishReason != nil {
			if err := s.sendTerminal(c, anthropicStreamStop(*choice.FinishReason)); err != nil {
				return err
			}
		}
	}
	return nil
}

// producedOutput reports whether the upstream gave this turn anything usable —
// a block already written, or a tool call still buffered. It is deliberately
// distinct from started, which only tracks whether bytes reached the client:
// a turn holding an unflushed tool call has produced output but written none.
func (s *anthropicStreamState) producedOutput() bool {
	return s.nextIndex > 0 || len(s.toolOrder) > 0 || s.emittedTool
}

// ensureStarted commits the response headers and message_start on first real
// output. Deferring it is what keeps a content-free upstream turn retryable.
func (s *anthropicStreamState) ensureStarted(c echo.Context) error {
	if s.started {
		return nil
	}
	s.started = true
	prepareSSE(c)
	return writeSSE(c, "message_start", map[string]any{
		"type": "message_start", "message": map[string]any{
			"id": s.messageID, "type": "message", "role": "assistant", "model": s.messageModel,
			"content": []any{}, "stop_reason": nil,
			"usage": map[string]int{
				"input_tokens": s.promptTokens, "output_tokens": 0,
				"cache_read_input_tokens": 0, "cache_creation_input_tokens": 0,
			},
		},
	})
}

func (s *anthropicStreamState) contentDelta(c echo.Context, blockType string, delta any) error {
	if err := s.ensureStarted(c); err != nil {
		return err
	}
	if s.activeType != blockType {
		if err := s.closeBlock(c); err != nil {
			return err
		}
		s.activeType = blockType
		s.activeIndex = s.nextIndex
		s.nextIndex++
		block := map[string]string{"type": blockType}
		if blockType == "text" {
			block["text"] = ""
		} else {
			block["thinking"] = ""
		}
		if err := writeSSE(c, "content_block_start", map[string]any{
			"type": "content_block_start", "index": s.activeIndex, "content_block": block,
		}); err != nil {
			return err
		}
	}
	return writeSSE(c, "content_block_delta", map[string]any{"type": "content_block_delta", "index": s.activeIndex, "delta": delta})
}

// A single Write/Edit tool call can legitimately carry a large file body. The old
// 1 MiB ceiling aborted those turns mid-stream; this bound is a runaway guard only.
const maxToolArgumentsBytes = 16 << 20

func (s *anthropicStreamState) toolDelta(_ echo.Context, call OpenAIStreamToolCall) error {
	s.sawTool = true
	tool := s.tools[call.Index]
	if tool == nil {
		tool = &bufferedToolCall{}
		s.tools[call.Index] = tool
		s.toolOrder = append(s.toolOrder, call.Index)
	}
	if call.ID != "" {
		tool.id = call.ID
	}
	if call.Function.Name != "" {
		tool.name = call.Function.Name
	}
	arguments := mergeToolArguments(tool.arguments, call.Function.Arguments)
	if len(arguments) > maxToolArgumentsBytes {
		return fmt.Errorf("tool arguments exceed 16 MiB")
	}
	tool.arguments = arguments
	s.completionBytes += len(call.Function.Name) + len(call.Function.Arguments)
	return nil
}

func mergeToolArguments(current, incoming string) string {
	return current + incoming
}

func (s *anthropicStreamState) flushTools(c echo.Context) error {
	if len(s.toolOrder) == 0 {
		return nil
	}
	if err := s.ensureStarted(c); err != nil {
		return err
	}
	if err := s.closeBlock(c); err != nil {
		return err
	}
	for _, toolIndex := range s.toolOrder {
		tool := s.tools[toolIndex]
		arguments, repaired := repairToolArguments(tool.arguments)
		if repaired {
			slog.Warn("repaired malformed tool arguments", "tool", tool.name, "bytes", len(tool.arguments))
		}
		if err := s.toolSchemas.Validate(tool.name, arguments); err != nil {
			// Never abort a live stream over tool arguments. The client turns a bad
			// call into an error tool_result and the model corrects itself next turn;
			// aborting discards the whole turn and surfaces as a failed file edit.
			slog.Warn("forwarding tool call that failed validation", "tool", tool.name, "error", err)
		}
		index := s.nextIndex
		s.nextIndex++
		s.emittedTool = true
		if err := writeSSE(c, "content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "tool_use", "id": tool.id, "name": tool.name, "input": map[string]any{}},
		}); err != nil {
			return err
		}
		if arguments != "" {
			if err := writeSSE(c, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": arguments},
			}); err != nil {
				return err
			}
		}
		if err := writeSSE(c, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
			return err
		}
	}
	s.tools = make(map[int]*bufferedToolCall)
	s.toolOrder = s.toolOrder[:0]
	return nil
}

func (s *anthropicStreamState) finish(c echo.Context) error {
	if !s.started || s.terminalSent {
		return nil
	}
	reason := "end_turn"
	if s.sawTool || len(s.toolOrder) > 0 {
		reason = "tool_use"
	}
	return s.sendTerminal(c, reason)
}

func (s *anthropicStreamState) sendTerminal(c echo.Context, reason string) error {
	if s.terminalSent {
		return nil
	}
	if s.nextIndex == 0 && len(s.toolOrder) == 0 {
		// The upstream finished having produced no text and no tool call. Passing that
		// on as a completed message — previously padded with a single space so the
		// Anthropic shape stayed valid — reaches the agent as end_turn, so the client
		// treats the task as done and waits for the user. That is the "it stops and I
		// have to type continue" stall. Report it instead, so the caller can retry it
		// on another candidate; only an exhausted caller falls back to the padding.
		return errEmptyUpstreamStream
	}
	if err := s.flushTools(c); err != nil {
		return err
	}
	if err := s.closeBlock(c); err != nil {
		return err
	}
	// A message that carries tool_use blocks must never claim end_turn. Upstreams
	// that report finish_reason "stop" alongside tool calls otherwise make the
	// client skip the call and re-prompt, which is the repeated-tool-call loop.
	if s.emittedTool && (reason == "end_turn" || reason == "") {
		reason = "tool_use"
	}
	return s.writeTerminal(c, reason)
}

// abort terminates a partially delivered message after an upstream failure.
// Tool calls the upstream had finished writing are still delivered — dropping a
// complete call leaves an empty turn and the agent simply asks for it again — but
// calls whose JSON never terminated are discarded as unusable.
// finishEmptyTurn emits a protocol-valid but empty message. It is the fallback for
// when every candidate returned nothing: the client still needs a well-formed
// message rather than a truncated HTTP body.
func (s *anthropicStreamState) finishEmptyTurn(c echo.Context) error {
	if s.terminalSent {
		return nil
	}
	if err := s.contentDelta(c, "text", map[string]any{"type": "text_delta", "text": " "}); err != nil {
		return err
	}
	if err := s.closeBlock(c); err != nil {
		return err
	}
	if err := s.writeTerminal(c, "end_turn"); err != nil {
		return err
	}
	return writeSSE(c, "message_stop", map[string]string{"type": "message_stop"})
}

func (s *anthropicStreamState) abort(c echo.Context) error {
	if s.terminalSent {
		return nil
	}
	complete := s.toolOrder[:0]
	for _, index := range s.toolOrder {
		if tool := s.tools[index]; tool != nil && toolArgumentsComplete(tool.arguments) {
			complete = append(complete, index)
			continue
		}
		delete(s.tools, index)
	}
	s.toolOrder = complete
	if err := s.flushTools(c); err != nil {
		return err
	}
	if err := s.closeBlock(c); err != nil {
		return err
	}
	reason := "max_tokens"
	if s.emittedTool {
		reason = "tool_use"
	}
	return s.writeTerminal(c, reason)
}

func toolArgumentsComplete(arguments string) bool {
	trimmed := strings.TrimSpace(arguments)
	return trimmed == "" || json.Valid([]byte(trimmed))
}

func (s *anthropicStreamState) writeTerminal(c echo.Context, reason string) error {
	// A terminal cannot be sent for a message that was never opened. Deferring
	// message_start made that reachable: abort() drops every truncated tool call, and
	// with nothing left to flush it went straight here, emitting message_delta as the
	// first event on the wire. Guarding at the single choke point covers every
	// terminal path rather than each caller.
	if err := s.ensureStarted(c); err != nil {
		return err
	}
	usage := map[string]int{
		"input_tokens":                s.lastUsage.PromptTokens,
		"output_tokens":               s.lastUsage.CompletionTokens,
		"cache_read_input_tokens":     s.lastUsage.PromptTokensDetails.CachedTokens,
		"cache_creation_input_tokens": 0,
	}
	if err := writeSSE(c, "message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": reason}, "usage": usage,
	}); err != nil {
		return err
	}
	s.terminalSent = true
	return nil
}

// repairToolArguments keeps a tool call usable when an upstream emits slightly
// malformed JSON. Streaming merges commonly concatenate two complete objects, and
// a dropped chunk leaves one unterminated; in both cases returning parseable JSON
// matters more than fidelity, because the client cannot act on anything else.
func repairToolArguments(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	if json.Valid([]byte(trimmed)) {
		return trimmed, trimmed != raw
	}
	if object, ok := firstJSONObject(trimmed); ok {
		return object, true
	}
	return "{}", true
}

func firstJSONObject(raw string) (string, bool) {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return "", false
	}
	depth, inString, escaped := 0, false, false
	for index := start; index < len(raw); index++ {
		char := raw[index]
		switch {
		case escaped:
			escaped = false
		case inString && char == '\\':
			escaped = true
		case char == '"':
			inString = !inString
		case inString:
		case char == '{':
			depth++
		case char == '}':
			depth--
			if depth == 0 {
				candidate := raw[start : index+1]
				return candidate, json.Valid([]byte(candidate))
			}
		}
	}
	return "", false
}

func (s *anthropicStreamState) closeBlock(c echo.Context) error {
	if s.activeType == "" {
		return nil
	}
	err := writeSSE(c, "content_block_stop", map[string]any{"type": "content_block_stop", "index": s.activeIndex})
	s.activeType = ""
	return err
}

// streamOpener hands out upstream bodies one candidate model at a time. Keeping
// the position across calls lets a reader that failed before delivering any bytes
// fall through to the next candidate, which a single-shot open cannot do.
type streamOpener struct {
	service *Service
	echo    echo.Context
	request translator.OpenAIRequest
	// unified is the already-translated form of request. The Anthropic endpoint
	// has it in hand, and reusing it skips re-parsing the whole history back out
	// of the OpenAI shape on every attempt — the dominant per-turn CPU cost once
	// a coding session carries six figures of context.
	unified   *provider.Request
	models    []string
	index     int
	prevBytes int64
	lastErr   error
	// replays counts same-candidate re-attempts spent on output-free turns. An
	// upstream that returns no text and no tool call is a transient quirk, not a
	// property of the candidate, so moving to the next model is not the fix — and
	// with a single-candidate model there is no next one to move to.
	replays int
	// clamped counts, per model, how many output caps were tried during this turn.
	// Bounded by maxOutputLimitAttempts: past that the cap the gateway is deriving is
	// wrong, and looping on it is worse than reporting the rejection.
	clamped map[string]int
	// trims counts how often this turn's history was cut down after an upstream refused
	// it as too long. See retryAfterTrimmingContext.
	trims int
	// sentEstimate is the estimate of the payload the last attempt actually sent, or 0
	// when the pipeline passed the caller's request through untouched. Only this number
	// describes the same bytes the upstream counted, so it is the one calibration may
	// learn from.
	sentEstimate int
}

// retryAfterLearningOutputLimit turns an upstream's max_tokens rejection into a cap
// for this model and rewinds the opener so the same candidate is tried again, now
// clamped. It reports false when nothing was learned or this model has spent its
// re-attempts.
//
// Doing it here rather than surfacing the 400 is what makes the cap self-discovering:
// the client never sees the rejection, and every later turn is already correct.
func (o *streamOpener) retryAfterLearningOutputLimit(model string, requested int, err error) bool {
	if !o.service.learnOutputLimit(model, requested, o.clamped[model], err) {
		return false
	}
	if o.clamped == nil {
		o.clamped = map[string]int{}
	}
	o.clamped[model]++
	o.index--
	return true
}

// sentTokens estimates the prompt this opener last uploaded, for the one decision that
// needs it: whether a numberless refusal is plausibly about size at all.
func (o *streamOpener) sentTokens() int {
	if o.unified == nil {
		return 0
	}
	return estimateSentTokens(*o.unified)
}

// maxContextTrimRetries bounds how often one turn's history may be cut down and
// re-sent. TrimOldestTurns reaches its budget in a single pass, so a second round is
// only useful when the first budget came from a window that was itself wrong — which
// the second rejection will have corrected.
const maxContextTrimRetries = 2

// retryAfterTrimmingContext turns an upstream context rejection into a servable request
// by dropping the oldest whole turns, then re-attempts the same candidate.
//
// This is the backstop for the failure the client cannot dig itself out of. Once a
// conversation is over the window, compaction is not a way back: the compaction request
// re-sends the history, so it costs about twice what the conversation costs, and there
// is no size at which it fits. Dropping old turns is lossy — TrimOldestTurns says so in
// a system block, and keeps tool_use paired with its tool_result — but a degraded turn
// is the only alternative to a dead session.
//
// Keyed off an actual rejection, never off the estimate. EstimateRequest takes
// max(bytes/4, runes/3) and so reads roughly a third high on ASCII; trimming on that
// would cut history out of every long session to prevent a failure that has not
// happened. Here the upstream has already refused, so there is nothing left to lose.
func (o *streamOpener) retryAfterTrimmingContext(ctx context.Context, model string, err error) bool {
	if !o.service.trimAfterContextRejection(ctx, model, o.unified, &o.trims, err) {
		return false
	}
	o.index--
	return true
}

// trimAfterContextRejection shrinks a refused request in place and reports whether
// the caller should re-attempt the same candidate. Shared by the streaming opener
// and the non-streaming /v1/messages candidate loop.
func (s *Service) trimAfterContextRejection(ctx context.Context, model string, unified *provider.Request, trims *int, err error) bool {
	if unified == nil || *trims >= maxContextTrimRetries || !isContextOverflow(err) {
		return false
	}
	// Runs after learnContextWindow, so this picks up the window the rejection just
	// revealed rather than the catalog's guess about it.
	window := s.resolveContextWindow(ctx, model)
	if window <= 0 {
		return false
	}
	// Keyed under both names because HardBudget looks the window up by the unified
	// request's own model, which is the id the client asked for and not necessarily the
	// candidate being attempted.
	limits := contextguard.Limits{Default: window, Models: map[string]int{
		model: window, unified.Model: window,
	}}
	// activeContextPolicy already falls back to the defaults when the configured
	// policy is zero-valued, which would make HardBudget return nothing and
	// silently disable the backstop.
	policy := s.activeContextPolicy()
	// HardBudget answers in tokens, derived from the window. TrimOldestTurns compares
	// against EstimateRequest. Handing one to the other unconverted is what made this trim
	// far harsher than the model required: on prose the estimate reads well above the real
	// count, so a token budget used as an estimate budget cuts history the model could
	// have held. Observed live, 30 messages became 6 where 10 would have fit.
	budget := s.estimateBudgetForTokens(model, contextguard.HardBudget(*unified, limits, policy))
	// The upstream has refused a request that this budget may well call acceptable —
	// which only means the window belief or the estimate is wrong. Shrinking by a
	// quarter regardless is what guarantees the retry is smaller than what was just
	// rejected; without it, TrimOldestTurns reports success having changed nothing
	// (its contract for "already fits") and the same payload goes back up forever.
	sent := estimateSentTokens(*unified)
	if shrunk := sent * 3 / 4; budget <= 0 || shrunk < budget {
		budget = shrunk
	}
	// Mechanical compaction first, at maximal aggressiveness: the upstream has
	// already refused, so prefix stability is lost either way, and truncating old
	// tool output keeps every turn where trimming drops whole ones. The strict
	// shrink check keeps an already-compacted payload from being re-sent verbatim.
	base := *unified
	compacted := contextguard.AggressiveCompact(*unified, policy)
	if tokens := contextguard.EstimateRequest(compacted); tokens < sent {
		if tokens <= budget {
			*unified = compacted
			*trims++
			slog.Warn("upstream refused the prompt as too long; compacted old tool output and retrying",
				"model", model, "context_window", window, "budget_tokens", budget,
				"estimate_tokens", tokens, "attempt", *trims)
			return true
		}
		// Short of the budget but genuinely smaller, so trim from here rather than from
		// the original: discarding the compaction meant dropping whole turns where
		// truncating old tool output had already kept every one of them.
		base = compacted
	}
	trimmed, ok := contextguard.TrimOldestTurns(base, budget)
	before := len(unified.Messages)
	// Trimming that removed nothing cannot make the retry succeed. A single turn too
	// large for the model is the honest end of the line: there is no older history left
	// to drop, and reporting the overflow lets the client decide.
	if !ok || len(trimmed.Messages) >= before {
		return false
	}
	*unified = trimmed
	*trims++
	slog.Warn("upstream refused the prompt as too long; dropped oldest turns and retrying",
		"model", model, "context_window", window, "budget_tokens", budget,
		"before_messages", before, "after_messages", len(trimmed.Messages), "attempt", *trims)
	return true
}

// learnContextWindowFrom reads a model's real context window out of an upstream's
// rejection.
//
// requestedOutputTokens reports the output budget actually sent, which is what the
// upstream compared its cap against.
func requestedOutputTokens(request translator.OpenAIRequest) int {
	return max(request.MaxTokens, request.MaxCompletionTokens)
}

// transientRetryBackoff is multiplied by the attempt number, so a single candidate
// waits ~1s then ~2s rather than hammering a backend that just said it is overloaded.
const transientRetryBackoff = time.Second

// transientUpstreamError reports whether a failure is worth attempting again on the
// same candidate. Overload and gateway errors qualify; a rate limit does not, because
// the right answer there is a different account rather than the same one again.
//
// Transport resets are included deliberately. An HTTP/2 peer reset arrives as a
// stream error with no typed status, so keying only on ProviderError let a connection
// the upstream itself dropped surface to the caller as a hard 502.
func transientUpstreamError(err error) bool {
	if err == nil {
		return false
	}
	var providerError *provider.ProviderError
	if errors.As(err, &providerError) {
		return providerError.StatusCode >= 500 && providerError.StatusCode <= 599
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	// The stdlib's bundled HTTP/2 returns unexported error types, so the wire-level
	// reset can only be recognised by its text.
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"internal_error",
		"stream error",
		"http2: stream closed",
		"connection reset by peer",
		"unexpected eof",
		"server closed idle connection",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// maxEmptyTurnReplays bounds how often one candidate may be re-attempted after
// producing nothing. Small on purpose: it only fires when the alternative is
// handing the agent an empty turn, which reads as end_turn and stops the session.
const maxEmptyTurnReplays = 2

// replayCurrent rewinds the opener so the candidate that just produced nothing is
// attempted again. It reports false once the allowance is spent.
func (o *streamOpener) replayCurrent() bool {
	if o.replays >= maxEmptyTurnReplays || o.index == 0 {
		return false
	}
	o.replays++
	o.index--
	return true
}

func (s *Service) newStreamOpener(c echo.Context, request translator.OpenAIRequest) *streamOpener {
	return &streamOpener{service: s, echo: c, request: request, models: s.modelChain(request.Model)}
}

func (s *Service) newUnifiedStreamOpener(c echo.Context, request translator.OpenAIRequest, unified provider.Request) *streamOpener {
	opener := s.newStreamOpener(c, request)
	opener.unified = &unified
	return opener
}

func (o *streamOpener) exhausted() bool {
	return o.index >= len(o.models)
}

func (o *streamOpener) next() (io.ReadCloser, error) {
	s, c := o.service, o.echo
	ctx := withPromptCacheSeed(withConversationID(c), o.request)
	for !o.exhausted() {
		attempt := o.index
		model := o.models[attempt]
		o.index++
		client := s.streamClientForModel(model)
		candidate := o.request
		candidate.Model = model
		if forced := forcedEffort(ctx); forced != "" {
			candidate.Effort = forced
		} else if effort := s.effortFor(model); effort != "" {
			candidate.Effort = effort
		}
		candidate, sentEstimate, err := s.prepareStreamCandidate(ctx, candidate, o.unified)
		o.sentEstimate = sentEstimate
		if err != nil {
			o.lastErr = err
			if !errors.Is(err, contextguard.ErrBudgetExceeded) {
				continue
			}
			// Every candidate shares the same context budget, so retrying cannot help.
			o.index = len(o.models)
			return nil, err
		}
		// After preparation, not before: prepareStreamCandidate rebuilds the candidate
		// from the unified request whenever context work is enabled, which restores the
		// caller's own max_tokens and would undo an earlier clamp.
		s.clampOpenAIOutput(&candidate)
		s.applyOpenAIPromptCache(ctx, &candidate)
		// Sizing the payload means marshalling the entire history. At info level that
		// ran several times per turn purely to produce a log line, which is real
		// latency once the context is large, so it is measured only when it is read.
		if debug := slog.Default().Enabled(c.Request().Context(), slog.LevelDebug); debug || attempt > 0 {
			candidateBytes := jsonBytes(candidate)
			if debug {
				slog.Debug("upstream payload", "endpoint", "/v1/messages", "model", candidate.Model,
					"fingerprint", streamRequestFingerprint(candidate),
					"total_bytes", candidateBytes, "messages_bytes", jsonBytes(candidate.Messages), "tools_bytes", jsonBytes(candidate.Tools),
					"message_count", len(candidate.Messages), "tool_count", len(candidate.Tools), "attempt", attempt+1)
			}
			if attempt > 0 && o.prevBytes > 0 && candidateBytes > o.prevBytes {
				slog.Warn("retry payload grew", "model", candidate.Model, "attempt", attempt+1, "previous_bytes", o.prevBytes, "total_bytes", candidateBytes)
			}
			o.prevBytes = candidateBytes
		}
		// A model claimed by a custom provider goes straight to that upstream. Trying
		// the OAuth pool first would be wrong: the pool has no account for it, and the
		// resulting ErrNoAccount would be reported as a provider outage.
		if target, ok := s.resolveCustomProvider(model); ok {
			path, pathErr := customUpstreamPath(target.APIType)
			if pathErr != nil {
				o.lastErr = pathErr
				o.index = len(o.models)
				return nil, pathErr
			}
			logCustomTarget(target, "/v1/messages")
			candidate.Model = target.Model
			body, streamErr := target.Client.DoStream(c.Request().Context(), path, candidate)
			if streamErr == nil {
				s.touchCustomProviderKey(target.KeyID)
				return body, nil
			}
			o.lastErr = streamErr
			o.service.learnContextWindow(model, o.sentTokens(), streamErr)
			if o.retryAfterTrimmingContext(ctx, model, streamErr) {
				continue
			}
			if o.retryAfterLearningOutputLimit(model, requestedOutputTokens(candidate), streamErr) {
				continue
			}
			if !retryableProviderError(streamErr) {
				o.index = len(o.models)
				return nil, streamErr
			}
			continue
		}
		var body io.ReadCloser
		if s.oauthInference != nil {
			body, err = s.oauthInference.DoStream(c.Request().Context(), candidate, conversationID(ctx))
		}
		if err != nil || s.oauthInference == nil {
			if client == nil {
				if err != nil {
					o.lastErr = err
				} else {
					o.lastErr = ErrProviderUnavailable
				}
				continue
			}
			candidate.Model = upstreamModel(model)
			body, err = client.DoStream(c.Request().Context(), "/chat/completions", candidate)
		}
		if err == nil {
			return body, nil
		}
		o.lastErr = err
		o.service.learnContextWindow(model, o.sentTokens(), err)
		if o.retryAfterTrimmingContext(ctx, model, err) {
			continue
		}
		if o.retryAfterLearningOutputLimit(model, requestedOutputTokens(candidate), err) {
			continue
		}
		if !retryableProviderError(err) {
			o.index = len(o.models)
			return nil, err
		}
	}
	if o.lastErr == nil {
		o.lastErr = ErrProviderUnavailable
	}
	return nil, o.lastErr
}

func streamRequestFingerprint(request translator.OpenAIRequest) string {
	request.Model = ""
	request.PromptCacheKey = ""
	encoded, err := json.Marshal(request)
	if err != nil {
		return "invalid"
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8])
}

func prepareSSE(c echo.Context) {
	response := c.Response()
	response.Header().Set(echo.HeaderContentType, "text/event-stream")
	response.Header().Set(echo.HeaderCacheControl, "no-cache")
	response.Header().Set(echo.HeaderConnection, "keep-alive")
	response.WriteHeader(http.StatusOK)
}

func writeSSE(c echo.Context, event string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var block bytes.Buffer
	if event != "" {
		fmt.Fprintf(&block, "event: %s\n", event)
	}
	block.WriteString("data: ")
	block.Write(encoded)
	block.WriteString("\n\n")
	response := c.Response()
	if _, err := response.Write(block.Bytes()); err != nil {
		return err
	}
	response.Flush()
	return nil
}

func writeSSEData(c echo.Context, data []byte) error {
	var block bytes.Buffer
	block.Grow(len(data) + len("data: \n\n"))
	block.WriteString("data: ")
	block.Write(data)
	block.WriteString("\n\n")
	response := c.Response()
	if _, err := response.Write(block.Bytes()); err != nil {
		return err
	}
	response.Flush()
	return nil
}

func writeSSERaw(c echo.Context, data string) error {
	return writeSSEData(c, []byte(data))
}

func (s *Service) streamClientForModel(model string) StreamClient {
	if isXAIModel(model) {
		return s.xaiStream
	}
	return s.openAIStream
}

// anthropicStreamStop maps an OpenAI finish_reason onto an Anthropic stop_reason.
// It must agree with translator.fromOpenAIStopReason: the streaming and
// non-streaming paths serve the same client, and a reason only one of them
// recognises is a bug that shows up on exactly half of the requests.
//
// Unrecognised reasons are forwarded verbatim rather than coerced. Claude Code
// matches specific stop_reason values and ignores the rest, so an unknown one reads
// as a completed turn — whereas guessing at, say, "refusal" would put the client
// into a refusal-handling path the model never asked for.
func anthropicStreamStop(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return reason
	}
}
