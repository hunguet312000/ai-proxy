package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/labstack/echo/v4"

	"literouter/internal/translator"
)

func (s *Service) responsesStream(c echo.Context, request translator.ResponsesRequest, upstream translator.OpenAIRequest) error {
	upstream.Stream = true
	opener := s.newStreamOpener(c, upstream)
	for {
		body, err := opener.next()
		if err != nil {
			return gatewayError(err)
		}
		state := newResponsesStreamState(request, upstream.Model)
		delivered := false
		readErr := readOpenAIStreamWithIdleTimeout(c.Request().Context(), body, upstreamStreamIdleTimeout, func(chunk OpenAIStreamChunk) error {
			if !delivered {
				delivered = true
				// Commit headers only once the upstream actually produced something,
				// so a dead candidate can still be replaced by the next one.
				prepareSSE(c)
				if startErr := state.start(c); startErr != nil {
					return startErr
				}
			}
			return state.emit(c, chunk)
		})
		_ = body.Close()
		if c.Request().Context().Err() != nil || errors.Is(readErr, context.Canceled) {
			return nil
		}
		if readErr != nil {
			if !delivered && !opener.exhausted() {
				continue
			}
			if !delivered {
				return gatewayError(readErr)
			}
			// Already streaming: complete the response instead of emitting an error
			// frame, which clients report as a mid-response server failure.
			s.recordUsage(UsageEvent{Provider: s.providerNameFor(state.response.Model), Model: state.response.Model, Endpoint: "/v1/responses", Status: streamErrorStatus(readErr), Effort: opener.sentEffort})
			return state.finish(c)
		}
		if !delivered {
			prepareSSE(c)
			if err := state.start(c); err != nil {
				return err
			}
		}
		if err := state.finish(c); err != nil {
			return err
		}
		output := state.text.String() + state.reasoning.String()
		for _, tool := range state.tools {
			output += tool.arguments.String()
		}
		usage, promptEstimated, completionEstimated := estimateStreamUsage(upstream, output, state.usage)
		s.recordUsage(UsageEvent{
			Provider: s.providerNameFor(state.response.Model), Model: state.response.Model, Endpoint: "/v1/responses",
			PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
			CachedTokens:          usage.PromptTokensDetails.CachedTokens,
			PromptTokensEstimated: promptEstimated, CompletionTokensEstimated: completionEstimated,
			CachedTokensReported: usage.PromptTokensDetails.CachedTokensReported,
			Effort:               opener.sentEffort,
		})
		return nil
	}
}

type responsesStreamTool struct {
	outputIndex int
	itemID      string
	callID      string
	name        string
	namespace   string
	arguments   strings.Builder
}

type responsesStreamState struct {
	request        translator.ResponsesRequest
	response       translator.ResponsesResponse
	sequence       int
	started        bool
	finished       bool
	messageIndex   int
	messageID      string
	text           strings.Builder
	reasoningIndex int
	reasoningID    string
	reasoning      strings.Builder
	tools          map[int]*responsesStreamTool
	toolOrder      []int
	usage          translator.OpenAIUsage
}

func newResponsesStreamState(request translator.ResponsesRequest, model string) *responsesStreamState {
	response := translator.NewResponsesResponse("", model, request)
	return &responsesStreamState{
		request: request, response: response, messageIndex: -1, reasoningIndex: -1,
		tools: make(map[int]*responsesStreamTool),
	}
}

func (state *responsesStreamState) start(c echo.Context) error {
	if state.started {
		return nil
	}
	state.started = true
	if err := state.emitEvent(c, "response.created", map[string]any{"type": "response.created", "response": state.response}); err != nil {
		return err
	}
	return state.emitEvent(c, "response.in_progress", map[string]any{"type": "response.in_progress", "response": state.response})
}

func (state *responsesStreamState) emit(c echo.Context, chunk OpenAIStreamChunk) error {
	if chunk.Model != "" {
		state.response.Model = chunk.Model
	}
	if chunk.Usage != nil {
		state.usage = *chunk.Usage
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Reasoning != "" {
			if err := state.reasoningDelta(c, choice.Delta.Reasoning); err != nil {
				return err
			}
		}
		if choice.Delta.Content != "" {
			if err := state.textDelta(c, choice.Delta.Content); err != nil {
				return err
			}
		}
		for _, call := range choice.Delta.ToolCalls {
			if err := state.toolDelta(c, call); err != nil {
				return err
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason == "length" {
			state.response.Status = "incomplete"
			state.response.IncompleteDetails = map[string]string{"reason": "max_output_tokens"}
		}
	}
	return nil
}

func (state *responsesStreamState) textDelta(c echo.Context, delta string) error {
	if state.messageIndex < 0 {
		state.messageIndex = len(state.response.Output)
		state.messageID = state.response.ID + "_message"
		item := translator.ResponsesOutputItem{Type: "message", ID: state.messageID, Status: "in_progress", Role: "assistant", Content: []translator.ResponsesOutputPart{}}
		state.response.Output = append(state.response.Output, item)
		if err := state.emitEvent(c, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": state.messageIndex, "item": item,
		}); err != nil {
			return err
		}
		if err := state.emitEvent(c, "response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": state.messageID, "output_index": state.messageIndex, "content_index": 0,
			"part": translator.ResponsesOutputPart{Type: "output_text", Text: "", Annotations: []any{}},
		}); err != nil {
			return err
		}
	}
	state.text.WriteString(delta)
	return state.emitEvent(c, "response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": state.messageID, "output_index": state.messageIndex, "content_index": 0, "delta": delta,
	})
}

func (state *responsesStreamState) reasoningDelta(c echo.Context, delta string) error {
	if state.reasoningIndex < 0 {
		state.reasoningIndex = len(state.response.Output)
		state.reasoningID = state.response.ID + "_reasoning"
		item := translator.ResponsesOutputItem{Type: "reasoning", ID: state.reasoningID, Status: "in_progress", Summary: []translator.ResponsesSummaryPart{}}
		state.response.Output = append(state.response.Output, item)
		if err := state.emitEvent(c, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": state.reasoningIndex, "item": item,
		}); err != nil {
			return err
		}
	}
	state.reasoning.WriteString(delta)
	return state.emitEvent(c, "response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "item_id": state.reasoningID, "output_index": state.reasoningIndex, "summary_index": 0, "delta": delta,
	})
}

func (state *responsesStreamState) toolDelta(c echo.Context, call OpenAIStreamToolCall) error {
	tool := state.tools[call.Index]
	if tool == nil {
		outputIndex := len(state.response.Output)
		callID := call.ID
		if callID == "" {
			callID = fmt.Sprintf("call_%d", call.Index)
		}
		namespace, name := translator.SplitResponsesToolName(call.Function.Name)
		tool = &responsesStreamTool{
			outputIndex: outputIndex, itemID: fmt.Sprintf("%s_fc_%d", state.response.ID, call.Index),
			callID: callID, name: name, namespace: namespace,
		}
		state.tools[call.Index] = tool
		item := translator.ResponsesOutputItem{
			Type: "function_call", ID: tool.itemID, Status: "in_progress", CallID: tool.callID, Name: tool.name, Namespace: tool.namespace, Arguments: "",
		}
		state.response.Output = append(state.response.Output, item)
		if err := state.emitEvent(c, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": outputIndex, "item": item,
		}); err != nil {
			return err
		}
	}
	if call.ID != "" {
		tool.callID = call.ID
	}
	if call.Function.Name != "" {
		tool.namespace, tool.name = translator.SplitResponsesToolName(call.Function.Name)
	}
	if call.Function.Arguments == "" {
		return nil
	}
	tool.arguments.WriteString(call.Function.Arguments)
	return state.emitEvent(c, "response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": tool.itemID, "output_index": tool.outputIndex, "delta": call.Function.Arguments,
	})
}

func (state *responsesStreamState) finish(c echo.Context) error {
	if state.finished {
		return nil
	}
	state.finished = true
	if state.reasoningIndex >= 0 {
		item := translator.ResponsesOutputItem{Type: "reasoning", ID: state.reasoningID, Status: "completed", Summary: []translator.ResponsesSummaryPart{{Type: "summary_text", Text: state.reasoning.String()}}}
		state.response.Output[state.reasoningIndex] = item
		if err := state.emitEvent(c, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": state.reasoningIndex, "item": item}); err != nil {
			return err
		}
	}
	if state.messageIndex >= 0 {
		part := translator.ResponsesOutputPart{Type: "output_text", Text: state.text.String(), Annotations: []any{}}
		if err := state.emitEvent(c, "response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": state.messageID, "output_index": state.messageIndex, "content_index": 0, "text": state.text.String(),
		}); err != nil {
			return err
		}
		if err := state.emitEvent(c, "response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": state.messageID, "output_index": state.messageIndex, "content_index": 0, "part": part,
		}); err != nil {
			return err
		}
		item := translator.ResponsesOutputItem{Type: "message", ID: state.messageID, Status: "completed", Role: "assistant", Content: []translator.ResponsesOutputPart{part}}
		state.response.Output[state.messageIndex] = item
		state.response.OutputText = state.text.String()
		if err := state.emitEvent(c, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": state.messageIndex, "item": item}); err != nil {
			return err
		}
	}
	for _, tool := range state.tools {
		arguments := tool.arguments.String()
		if err := state.emitEvent(c, "response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": tool.itemID, "output_index": tool.outputIndex, "arguments": arguments, "name": tool.name,
		}); err != nil {
			return err
		}
		item := translator.ResponsesOutputItem{Type: "function_call", ID: tool.itemID, Status: "completed", CallID: tool.callID, Name: tool.name, Namespace: tool.namespace, Arguments: arguments}
		state.response.Output[tool.outputIndex] = item
		if err := state.emitEvent(c, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": tool.outputIndex, "item": item}); err != nil {
			return err
		}
	}
	if state.response.Status != "incomplete" {
		state.response.Status = "completed"
	}
	state.response.Usage = translator.ResponsesUsage{
		InputTokens:         state.usage.PromptTokens,
		InputTokensDetails:  translator.ResponsesInputTokenDetails{CachedTokens: state.usage.PromptTokensDetails.CachedTokens},
		OutputTokens:        state.usage.CompletionTokens,
		OutputTokensDetails: translator.ResponsesOutputTokenDetails{},
		TotalTokens:         state.usage.PromptTokens + state.usage.CompletionTokens,
	}
	return state.emitEvent(c, "response.completed", map[string]any{"type": "response.completed", "response": state.response})
}

func (state *responsesStreamState) emitEvent(c echo.Context, event string, payload map[string]any) error {
	payload["sequence_number"] = state.sequence
	state.sequence++
	return writeSSE(c, event, payload)
}
