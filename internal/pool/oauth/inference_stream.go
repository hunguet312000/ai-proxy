package oauth

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"literouter/internal/provider"
	"literouter/internal/translator"
)

const (
	oauthMaxSSELineBytes  = 8 << 20
	oauthMaxSSEEventBytes = 16 << 20
)

type responsesStreamEvent struct {
	Type   string `json:"type"`
	Delta  string `json:"delta"`
	Text   string `json:"text"`
	ItemID string `json:"item_id"`
	Error  struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Param   string `json:"param"`
	} `json:"error"`
	Item struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			InputDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
}

func codexStreamError(event responsesStreamEvent, raw []byte) error {
	message := strings.TrimSpace(event.Error.Message)
	if message == "" {
		message = "Codex upstream stream failed"
	}
	status := http.StatusBadGateway
	switch event.Error.Code {
	case "context_length_exceeded", "max_tokens_exceeded":
		status = http.StatusRequestEntityTooLarge
	case "invalid_request_error", "invalid_prompt", "invalid_tool_schema":
		status = http.StatusBadRequest
	case "rate_limit_exceeded", "rate_limit_error":
		status = http.StatusTooManyRequests
	}
	if event.Error.Type == "invalid_request_error" && status == http.StatusBadGateway {
		status = http.StatusBadRequest
	}
	if message == "Codex upstream stream failed" && len(raw) > 0 {
		message = fmt.Sprintf("%s: %s", message, strings.TrimSpace(string(raw)))
	}
	return &provider.ProviderError{
		Provider: "codex OAuth", StatusCode: status, Code: event.Error.Code, Message: message,
	}
}

func codexSSEToOpenAI(body io.Reader, fallbackModel string) (translator.OpenAIResponse, error) {
	response := translator.OpenAIResponse{Model: fallbackModel}
	message := translator.OpenAIMessage{Role: "assistant"}
	var text strings.Builder
	toolIndexes := map[string]int{}
	completed := false
	err := readSSEJSON(body, func(raw []byte) error {
		var event responsesStreamEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil
		}
		switch event.Type {
		case "response.output_text.delta":
			text.WriteString(event.Delta)
		case "response.output_item.added":
			if event.Item.Type == "function_call" {
				index := len(message.ToolCalls)
				toolIndexes[event.Item.ID] = index
				message.ToolCalls = append(message.ToolCalls, translator.OpenAIToolCall{
					ID: event.Item.CallID, Type: "function",
					Function: translator.OpenAIFunctionCall{Name: event.Item.Name},
				})
			}
		case "response.function_call_arguments.delta":
			itemID := event.ItemID
			if itemID == "" {
				itemID = event.Item.ID
			}
			index, ok := toolIndexes[itemID]
			if !ok {
				return fmt.Errorf("Codex argument delta references unknown item %q", itemID)
			}
			message.ToolCalls[index].Function.Arguments += event.Delta
		case "response.output_item.done":
			if event.Item.Type == "function_call" {
				index, ok := toolIndexes[event.Item.ID]
				if !ok {
					index = len(message.ToolCalls)
					message.ToolCalls = append(message.ToolCalls, translator.OpenAIToolCall{ID: event.Item.CallID, Type: "function"})
				}
				message.ToolCalls[index].Function.Name = event.Item.Name
				if event.Item.Arguments != "" {
					message.ToolCalls[index].Function.Arguments = event.Item.Arguments
				}
			}
		case "response.completed":
			completed = true
			response.ID = event.Response.ID
			if event.Response.Model != "" {
				response.Model = event.Response.Model
			}
			response.Usage.PromptTokens = event.Response.Usage.InputTokens
			response.Usage.CompletionTokens = event.Response.Usage.OutputTokens
			response.Usage.PromptTokensReported = true
			response.Usage.CompletionTokensReported = true
			response.Usage.PromptTokensDetails.CachedTokens = event.Response.Usage.InputDetails.CachedTokens
			response.Usage.PromptTokensDetails.CachedTokensReported = true
		case "error", "response.failed":
			return codexStreamError(event, raw)
		}
		return nil
	})
	if err != nil {
		return translator.OpenAIResponse{}, err
	}
	if !completed {
		return translator.OpenAIResponse{}, fmt.Errorf("Codex OAuth stream ended without response.completed")
	}
	message.Content = text.String()
	finish := "stop"
	if len(message.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	response.Choices = []translator.OpenAIChoice{{Message: message, FinishReason: finish}}
	if response.ID == "" {
		response.ID = "oauth-response"
	}
	return response, nil
}

func openAISSEToResponse(body io.Reader, fallbackModel string) (translator.OpenAIResponse, error) {
	response := translator.OpenAIResponse{Model: fallbackModel}
	message := translator.OpenAIMessage{Role: "assistant"}
	var text strings.Builder
	toolIndexes := make(map[int]int)
	finish := ""
	err := readSSEJSON(body, func(raw []byte) error {
		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content   any `json:"content"`
					ToolCalls []struct {
						Index    int                           `json:"index"`
						ID       string                        `json:"id"`
						Type     string                        `json:"type"`
						Function translator.OpenAIFunctionCall `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *translator.OpenAIUsage `json:"usage"`
		}
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return err
		}
		if chunk.ID != "" {
			response.ID = chunk.ID
		}
		if chunk.Model != "" {
			response.Model = chunk.Model
		}
		for _, choice := range chunk.Choices {
			if value, ok := choice.Delta.Content.(string); ok {
				text.WriteString(value)
			}
			for _, call := range choice.Delta.ToolCalls {
				index, exists := toolIndexes[call.Index]
				if !exists {
					index = len(message.ToolCalls)
					toolIndexes[call.Index] = index
					message.ToolCalls = append(message.ToolCalls, translator.OpenAIToolCall{ID: call.ID, Type: call.Type})
				}
				if call.ID != "" {
					message.ToolCalls[index].ID = call.ID
				}
				if call.Type != "" {
					message.ToolCalls[index].Type = call.Type
				}
				if call.Function.Name != "" {
					message.ToolCalls[index].Function.Name = call.Function.Name
				}
				message.ToolCalls[index].Function.Arguments += call.Function.Arguments
			}
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
		if chunk.Usage != nil {
			response.Usage = *chunk.Usage
		}
		return nil
	})
	if err != nil {
		return translator.OpenAIResponse{}, err
	}
	if finish == "" {
		return translator.OpenAIResponse{}, fmt.Errorf("OAuth stream ended without finish reason")
	}
	message.Content = text.String()
	response.Choices = []translator.OpenAIChoice{{Message: message, FinishReason: finish}}
	return response, nil
}

func openAIResponseToChatStream(response translator.OpenAIResponse) (io.ReadCloser, error) {
	if len(response.Choices) == 0 {
		return nil, fmt.Errorf("OAuth response has no choices")
	}
	choice := response.Choices[0]
	chunk := map[string]any{
		"id": response.ID, "object": "chat.completion.chunk", "model": response.Model,
		"choices": []any{map[string]any{"index": 0, "delta": choice.Message, "finish_reason": nil}},
	}
	first, err := json.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	terminalPayload := map[string]any{
		"id": response.ID, "object": "chat.completion.chunk", "model": response.Model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": choice.FinishReason}},
	}
	// Only attach usage when upstream actually reported tokens. Emitting a zero
	// usage object makes the gateway treat metrics as "reported" and skip estimation,
	// which is why Grok OAuth rows showed 0/0/$0 despite successful completions.
	if response.Usage.PromptTokens > 0 || response.Usage.CompletionTokens > 0 ||
		response.Usage.PromptTokensReported || response.Usage.CompletionTokensReported {
		terminalPayload["usage"] = response.Usage
	}
	terminal, err := json.Marshal(terminalPayload)
	if err != nil {
		return nil, err
	}
	var stream bytes.Buffer
	fmt.Fprintf(&stream, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", first, terminal)
	return io.NopCloser(bytes.NewReader(stream.Bytes())), nil
}

func codexSSEToChatStream(body io.ReadCloser, fallbackModel string) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer body.Close()
		toolIndexes := map[string]int{}
		finishReason := "stop"
		completed := false
		err := readSSEJSON(body, func(raw []byte) error {
			var event responsesStreamEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				return nil
			}
			chunk := map[string]any{"id": event.Response.ID, "object": "chat.completion.chunk", "model": fallbackModel}
			delta := map[string]any{}
			switch event.Type {
			case "response.output_text.delta":
				delta["content"] = event.Delta
			case "response.output_item.added":
				if event.Item.Type != "function_call" {
					return nil
				}
				index := len(toolIndexes)
				toolIndexes[event.Item.ID] = index
				delta["tool_calls"] = []any{map[string]any{"index": index, "id": event.Item.CallID, "type": "function", "function": map[string]string{"name": event.Item.Name, "arguments": ""}}}
				finishReason = "tool_calls"
			case "response.function_call_arguments.delta":
				itemID := event.ItemID
				if itemID == "" {
					itemID = event.Item.ID
				}
				index, ok := toolIndexes[itemID]
				if !ok {
					return fmt.Errorf("Codex argument delta references unknown item %q", itemID)
				}
				delta["tool_calls"] = []any{map[string]any{"index": index, "function": map[string]string{"arguments": event.Delta}}}
			case "response.completed":
				completed = true
				chunk["model"] = event.Response.Model
				chunk["usage"] = map[string]any{"prompt_tokens": event.Response.Usage.InputTokens, "completion_tokens": event.Response.Usage.OutputTokens, "total_tokens": event.Response.Usage.InputTokens + event.Response.Usage.OutputTokens, "prompt_tokens_details": map[string]int{"cached_tokens": event.Response.Usage.InputDetails.CachedTokens}}
				chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}}
			case "error", "response.failed":
				// Type this the same way the non-streaming path does. Returning a bare
				// error erased the upstream code, so a context-window rejection reached
				// the client as a generic 502 and the client retried the same oversized
				// request instead of compacting.
				return codexStreamError(event, raw)
			default:
				return nil
			}
			if _, ok := chunk["choices"]; !ok {
				chunk["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}
			}
			encoded, err := json.Marshal(chunk)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(writer, "data: %s\n\n", encoded)
			return err
		})
		if err == nil && !completed {
			err = fmt.Errorf("Codex OAuth stream ended without response.completed")
		}
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		if _, err := io.WriteString(writer, "data: [DONE]\n\n"); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.Close()
	}()
	return reader
}

func readSSEJSON(reader io.Reader, emit func([]byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), oauthMaxSSELineBytes)
	var data strings.Builder
	flush := func() error {
		value := strings.TrimSpace(data.String())
		data.Reset()
		if value == "" || value == "[DONE]" {
			return nil
		}
		return emit([]byte(value))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data.Len()+len(payload) > oauthMaxSSEEventBytes {
				return fmt.Errorf("upstream SSE event exceeds 16 MiB")
			}
			data.WriteString(payload)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}
