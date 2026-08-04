package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"literouter/internal/provider"
	"literouter/internal/translator"
)

func TestOpenAIToCodexRequestPreservesTools(t *testing.T) {
	request := translator.OpenAIRequest{
		Messages: []translator.OpenAIMessage{
			{Role: "system", Content: "rules"},
			{Role: "user", Content: "question"},
			{Role: "assistant", ToolCalls: []translator.OpenAIToolCall{{ID: "call", Type: "function", Function: translator.OpenAIFunctionCall{Name: "read", Arguments: `{}`}}}},
			{Role: "tool", ToolCallID: "call", Content: "result"},
		},
		Tools: []translator.OpenAITool{{Type: "function", Function: translator.OpenAIFunction{Name: "read"}}},
	}
	payload := openAIToCodexRequest(request, "gpt-5.6-sol")
	input, ok := payload["input"].([]map[string]any)
	if !ok || len(input) != 3 || payload["instructions"] != "rules" {
		t.Fatalf("payload = %#v", payload)
	}
	if input[1]["type"] != "function_call" || input[2]["type"] != "function_call_output" {
		t.Fatalf("input = %#v", input)
	}
}

func TestOpenAIToCodexRequestPreservesEffort(t *testing.T) {
	for _, test := range []struct {
		effort string
		want   string
	}{{effort: "xhigh", want: "xhigh"}, {want: "high"}} {
		payload := openAIToCodexRequest(translator.OpenAIRequest{
			Messages: []translator.OpenAIMessage{{Role: "user", Content: "question"}}, Effort: test.effort,
		}, "gpt-5.6-sol")
		reasoning := payload["reasoning"].(map[string]string)
		if reasoning["effort"] != test.want {
			t.Fatalf("effort %q = %q, want %q", test.effort, reasoning["effort"], test.want)
		}
	}
}

func TestOpenAIToCodexRequestPreservesImages(t *testing.T) {
	request := translator.OpenAIRequest{
		Messages: []translator.OpenAIMessage{{Role: "user", Content: []translator.OpenAIContentPart{
			{Type: "text", Text: "inspect"},
			{Type: "image_url", ImageURL: &translator.OpenAIImageURL{URL: "data:image/png;base64,AAAA"}},
		}}},
	}
	payload := openAIToCodexRequest(request, "gpt-5.6-sol")
	input := payload["input"].([]map[string]any)
	content := input[0]["content"].([]map[string]any)
	if len(content) != 2 || content[1]["type"] != "input_image" || content[1]["image_url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("content = %#v", content)
	}
}

func TestCodexSSEToOpenAI(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp","model":"gpt-5.6-sol","usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":3}}}}`,
		"",
	}, "\n")
	response, err := codexSSEToOpenAI(strings.NewReader(sse), "fallback")
	if err != nil || response.ID != "resp" || response.Choices[0].Message.Content != "hello" || response.Usage.PromptTokens != 10 {
		t.Fatalf("response = %#v, error = %v", response, err)
	}
}

func TestOAuthSSERequiresTerminalEvent(t *testing.T) {
	if _, err := openAISSEToResponse(strings.NewReader(`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}

`), "model"); err == nil {
		t.Fatal("OpenAI OAuth stream without finish reason succeeded")
	}
	if _, err := codexSSEToOpenAI(strings.NewReader(`data: {"type":"response.output_text.delta","delta":"partial"}

`), "model"); err == nil {
		t.Fatal("Codex OAuth stream without response.completed succeeded")
	}
}

func TestOpenAIResponseToChatStreamIsComplete(t *testing.T) {
	response := translator.OpenAIResponse{
		ID: "response", Model: "model",
		Choices: []translator.OpenAIChoice{{
			Message: translator.OpenAIMessage{Role: "assistant", ToolCalls: []translator.OpenAIToolCall{{
				ID: "call", Type: "function", Function: translator.OpenAIFunctionCall{Name: "read", Arguments: `{}`},
			}}},
			FinishReason: "tool_calls",
		}},
	}
	body, err := openAIResponseToChatStream(response)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	encoded, err := io.ReadAll(body)
	if err != nil || !strings.Contains(string(encoded), `"finish_reason":"tool_calls"`) || !strings.Contains(string(encoded), "[DONE]") {
		t.Fatalf("stream = %q, error = %v", encoded, err)
	}
}

func TestCodexSSEToChatStreamStreamsParallelToolCalls(t *testing.T) {
	sse := io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"item-a","type":"function_call","call_id":"call-a","name":"Read"}}`,
		"", `data: {"type":"response.output_item.added","item":{"id":"item-b","type":"function_call","call_id":"call-b","name":"Search"}}`,
		"", `data: {"type":"response.function_call_arguments.delta","item_id":"item-b","delta":"{\"query\":\"needle\"}"}`,
		"", `data: {"type":"response.function_call_arguments.delta","item_id":"item-a","delta":"{\"file_path\":\"a\"}"}`,
		"", `data: {"type":"response.completed","response":{"id":"resp","model":"model"}}`, "",
	}, "\n")))
	body := codexSSEToChatStream(sse, "model")
	defer body.Close()
	var arguments = map[int]string{}
	err := readSSEJSON(body, func(raw []byte) error {
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int `json:"index"`
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return err
		}
		for _, choice := range chunk.Choices {
			for _, call := range choice.Delta.ToolCalls {
				if call.Function.Arguments != "" {
					arguments[call.Index] += call.Function.Arguments
				}
			}
		}
		return nil
	})
	if err != nil || arguments[0] != `{"file_path":"a"}` || arguments[1] != `{"query":"needle"}` {
		t.Fatalf("parallel tool arguments = %#v, error = %v", arguments, err)
	}
}

func TestCodexSSEToOpenAIMapsParallelToolArgumentsByItemID(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"item-a","type":"function_call","call_id":"call-a","name":"Read"}}`,
		"", `data: {"type":"response.output_item.added","item":{"id":"item-b","type":"function_call","call_id":"call-b","name":"Search"}}`,
		"", `data: {"type":"response.function_call_arguments.delta","item_id":"item-b","delta":"{\"query\":\"needle\"}"}`,
		"", `data: {"type":"response.function_call_arguments.delta","item_id":"item-a","delta":"{\"file_path\":\"a\"}"}`,
		"", `data: {"type":"response.completed","response":{"id":"resp","model":"model"}}`, "",
	}, "\n")
	response, err := codexSSEToOpenAI(strings.NewReader(sse), "model")
	if err != nil {
		t.Fatal(err)
	}
	calls := response.Choices[0].Message.ToolCalls
	if len(calls) != 2 || calls[0].Function.Arguments != `{"file_path":"a"}` || calls[1].Function.Arguments != `{"query":"needle"}` {
		t.Fatalf("tool calls = %#v", calls)
	}
}

func TestCodexSSEToChatStreamRequiresTerminal(t *testing.T) {
	body := codexSSEToChatStream(io.NopCloser(strings.NewReader(`data: {"type":"response.output_text.delta","delta":"partial"}

`)), "model")
	defer body.Close()
	_, err := io.ReadAll(body)
	if err == nil || !strings.Contains(err.Error(), "without response.completed") {
		t.Fatalf("terminal error = %v", err)
	}
}

func TestCodexSSEToChatStreamTypesErrorFrames(t *testing.T) {
	// The gateway decides the client-facing status from the ProviderError code. When
	// this frame arrived as a bare error the code was lost and a context overflow
	// surfaced as 502, which clients retry unchanged.
	frame := `data: {"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model.","param":"input"},"sequence_number":2}`
	body := codexSSEToChatStream(io.NopCloser(strings.NewReader(frame+"\n\n")), "model")
	defer body.Close()
	_, err := io.ReadAll(body)
	var providerError *provider.ProviderError
	if !errors.As(err, &providerError) {
		t.Fatalf("error = %v (%T), want *provider.ProviderError", err, err)
	}
	if providerError.Code != "context_length_exceeded" || providerError.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("provider error = %#v", providerError)
	}
	if !strings.Contains(providerError.Message, "exceeds the context window") {
		t.Fatalf("message = %q", providerError.Message)
	}
}

func TestCodexSSEToChatStream(t *testing.T) {
	sse := io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
	body := codexSSEToChatStream(sse, "model")
	defer body.Close()
	encoded, err := io.ReadAll(body)
	if err != nil || !strings.Contains(string(encoded), `"content":"hello"`) || !strings.Contains(string(encoded), "[DONE]") {
		t.Fatalf("stream = %q, error = %v", encoded, err)
	}
}

func TestOpenAIResponseToChatStreamOmitsEmptyUsage(t *testing.T) {
	body, err := openAIResponseToChatStream(translator.OpenAIResponse{
		ID: "r1", Model: "grok-4.5",
		Choices: []translator.OpenAIChoice{{
			Message: translator.OpenAIMessage{Role: "assistant", Content: "hi"}, FinishReason: "stop",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(body)
	_ = body.Close()
	if strings.Contains(string(raw), `"usage"`) {
		t.Fatalf("empty usage should be omitted, got %s", raw)
	}
	body, err = openAIResponseToChatStream(translator.OpenAIResponse{
		ID: "r2", Model: "grok-4.5",
		Choices: []translator.OpenAIChoice{{
			Message: translator.OpenAIMessage{Role: "assistant", Content: "hi"}, FinishReason: "stop",
		}},
		Usage: translator.OpenAIUsage{PromptTokens: 11, CompletionTokens: 3, PromptTokensReported: true, CompletionTokensReported: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(body)
	_ = body.Close()
	if !strings.Contains(string(raw), `"prompt_tokens":11`) {
		t.Fatalf("expected usage retained, got %s", raw)
	}
}

func TestOAuthRetryClassification(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		acrossAccount bool
		secondWave    bool
	}{
		{name: "rate limit", err: &provider.ProviderError{StatusCode: 429}, acrossAccount: true, secondWave: true},
		{name: "server error", err: &provider.ProviderError{StatusCode: 503}, acrossAccount: true, secondWave: true},
		{name: "invalid payload", err: &provider.ProviderError{StatusCode: 400}, acrossAccount: false, secondWave: false},
		{name: "invalid tool", err: errors.New("invalid tool schema"), acrossAccount: true, secondWave: false},
		{name: "truncated stream", err: errors.New("Codex OAuth stream ended without response.completed"), acrossAccount: true, secondWave: true},
		{name: "canceled", err: context.Canceled, acrossAccount: false, secondWave: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := retryAcrossOAuthAccounts(test.err); got != test.acrossAccount {
				t.Fatalf("retryAcrossOAuthAccounts() = %v, want %v", got, test.acrossAccount)
			}
			if got := retryableOAuthTransient(test.err); got != test.secondWave {
				t.Fatalf("retryableOAuthTransient() = %v, want %v", got, test.secondWave)
			}
		})
	}
}

func TestCodexSSEContextLengthErrorIsTypedAndPermanent(t *testing.T) {
	sse := `data: {"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model.","param":"input"},"sequence_number":2}

`
	_, err := codexSSEToOpenAI(strings.NewReader(sse), "gpt-5.6-sol")
	if err == nil {
		t.Fatal("expected context length error")
	}
	var providerErr *provider.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("error type = %T, want ProviderError: %v", err, err)
	}
	if providerErr.StatusCode != 413 || providerErr.Code != "context_length_exceeded" ||
		providerErr.Message != "Your input exceeds the context window of this model." {
		t.Fatalf("provider error = %#v", providerErr)
	}
	if retryAcrossOAuthAccounts(err) || retryableOAuthTransient(err) {
		t.Fatalf("context length error must fail fast: %v", err)
	}
}
