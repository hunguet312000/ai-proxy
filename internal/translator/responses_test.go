package translator

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFromResponsesRequestMapsMessagesAndTools(t *testing.T) {
	parallel := true
	request := ResponsesRequest{
		Model: "model", Instructions: "system", ParallelToolCalls: &parallel,
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"},
			{"type":"function_call_output","call_id":"call_1","output":"done"}
		]`),
		Tools:      []ResponsesTool{{Type: "function", Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: json.RawMessage(`{"type":"function","name":"lookup"}`),
	}
	result, err := FromResponsesRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 4 || result.Messages[0].Role != "system" || result.Messages[2].ToolCalls[0].ID != "call_1" || result.Messages[3].ToolCallID != "call_1" {
		t.Fatalf("messages = %#v", result.Messages)
	}
	if len(result.Tools) != 1 || result.ParallelToolCalls == nil || !*result.ParallelToolCalls {
		t.Fatalf("request = %#v", result)
	}
}

func TestFromResponsesRequestIgnoresDisabledWebSearch(t *testing.T) {
	disabled := false
	request := ResponsesRequest{Model: "model", Input: json.RawMessage(`"hi"`), Tools: []ResponsesTool{{Type: "web_search", ExternalWebAccess: &disabled}}}
	result, err := FromResponsesRequest(request)
	if err != nil || len(result.Tools) != 0 {
		t.Fatalf("request = %#v, %v", result, err)
	}
}

func TestFromResponsesRequestIgnoresAdditionalToolsInputItem(t *testing.T) {
	request := ResponsesRequest{
		Model: "model",
		Input: json.RawMessage(`[
			{"type":"additional_tools","tools":[{"type":"computer_use_preview"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
		]`),
	}
	result, err := FromResponsesRequest(request)
	if err != nil || len(result.Messages) != 1 || result.Messages[0].Role != "user" {
		t.Fatalf("request = %#v, %v", result, err)
	}
	content, ok := result.Messages[0].Content.([]OpenAIContentPart)
	if !ok || len(content) != 1 || content[0].Type != "text" || content[0].Text != "hello" {
		t.Fatalf("content = %#v", result.Messages[0].Content)
	}
}

func TestFromResponsesRequestFlattensNamespaceTools(t *testing.T) {
	request := ResponsesRequest{
		Model: "model", Input: json.RawMessage(`"hi"`),
		Tools: []ResponsesTool{{Type: "namespace", Name: "agents", Tools: []ResponsesTool{{
			Type: "function", Name: "spawn", Parameters: json.RawMessage(`{"type":"object"}`),
		}}}},
	}
	result, err := FromResponsesRequest(request)
	if err != nil || len(result.Tools) != 1 || result.Tools[0].Function.Name != "agents__spawn" {
		t.Fatalf("request = %#v, %v", result, err)
	}
}

func TestFromResponsesRequestRejectsStateAndHostedTools(t *testing.T) {
	tests := []ResponsesRequest{
		{Model: "model", Input: json.RawMessage(`"hi"`), PreviousResponseID: "resp"},
		{Model: "model", Input: json.RawMessage(`"hi"`), Background: true},
		{Model: "model", Input: json.RawMessage(`"hi"`), Tools: []ResponsesTool{{Type: "web_search"}}},
		{Model: "model", Input: json.RawMessage(`[{"type":"computer_call"}]`)},
	}
	for _, request := range tests {
		if _, err := FromResponsesRequest(request); err == nil {
			t.Fatalf("request accepted: %#v", request)
		}
	}
}

func TestToResponsesResponseMapsTextToolsAndUsage(t *testing.T) {
	response := OpenAIResponse{
		ID: "chatcmpl-one", Model: "model",
		Choices: []OpenAIChoice{{Message: OpenAIMessage{Content: "hello", ToolCalls: []OpenAIToolCall{{
			ID: "call", Type: "function", Function: OpenAIFunctionCall{Name: "lookup", Arguments: `{}`},
		}}}, FinishReason: "tool_calls"}},
		Usage: OpenAIUsage{PromptTokens: 10, CompletionTokens: 4},
	}
	result := ToResponsesResponse(response, ResponsesRequest{Model: "model"})
	if result.ID != "resp_one" || result.OutputText != "hello" || len(result.Output) != 2 || result.Output[1].CallID != "call" || result.Usage.TotalTokens != 14 {
		t.Fatalf("response = %#v", result)
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), `"object":"response"`) {
		t.Fatalf("JSON = %s", encoded)
	}
}
