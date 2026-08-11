package translator

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"literouter/internal/provider"
)

func TestOpenAIUsageTracksReportedFields(t *testing.T) {
	var omitted OpenAIUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":10,"completion_tokens":2}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if !omitted.PromptTokensReported || !omitted.CompletionTokensReported || omitted.PromptTokensDetails.CachedTokensReported {
		t.Fatalf("omitted usage = %#v", omitted)
	}
	var zero OpenAIUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0}}`), &zero); err != nil {
		t.Fatal(err)
	}
	if !zero.PromptTokensDetails.CachedTokensReported || zero.PromptTokensDetails.CachedTokens != 0 {
		t.Fatalf("zero usage = %#v", zero)
	}
}

func TestOpenAIRoundTripPreservesContentAndTools(t *testing.T) {
	temperature := 0.2
	request := provider.Request{
		Model: "model", Temperature: &temperature, MaxTokens: 100,
		System: []provider.Content{{Type: "text", Text: "system"}},
		Messages: []provider.Message{
			{Role: "user", Content: []provider.Content{{Type: "text", Text: "look"}, {Type: "image", MediaType: "image/png", Data: "AAAA"}}},
			{Role: "assistant", Content: []provider.Content{{Type: "thinking", Thinking: "reason"}, {Type: "tool_use", ToolUseID: "call", Name: "lookup", Input: json.RawMessage(`{"q":"x"}`)}}},
			{Role: "tool", Content: []provider.Content{{Type: "tool_result", ToolUseID: "call", Text: "result"}}},
		},
		Tools:      []provider.Tool{{Name: "lookup", Description: "Lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: provider.ToolChoice{Type: "tool", Name: "lookup"},
	}
	openAI, err := ToOpenAIRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := FromOpenAIRequest(openAI)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.Model != request.Model || len(roundTrip.System) != 1 || len(roundTrip.Messages) != 3 || len(roundTrip.Tools) != 1 {
		t.Fatalf("round trip = %#v", roundTrip)
	}
	if roundTrip.Messages[0].Content[1].Data != "AAAA" || len(roundTrip.Messages[1].Content) != 1 || roundTrip.Messages[1].Content[0].Name != "lookup" || roundTrip.Messages[2].Content[0].Text != "result" {
		t.Fatalf("content = %#v", roundTrip.Messages)
	}
}

func TestOpenAIRequestOmitsAnthropicReasoning(t *testing.T) {
	request := provider.Request{
		Model: "grok", MaxTokens: 100,
		System: []provider.Content{
			{Type: "thinking", Thinking: "system reason"},
			{Type: "text", Text: "system"},
		},
		Messages: []provider.Message{
			{Role: "assistant", Content: []provider.Content{{Type: "thinking", Thinking: "reason only"}}},
			{Role: "user", Content: []provider.Content{{Type: "text", Text: "continue"}}},
			{Role: "assistant", Content: []provider.Content{
				{Type: "redacted_thinking", Data: "secret"},
				{Type: "text", Text: "answer"},
				{Type: "tool_use", ToolUseID: "call", Name: "lookup", Input: json.RawMessage(`{"q":"x"}`)},
			}},
			{Role: "user", Content: []provider.Content{{Type: "tool_result", ToolUseID: "call", Text: "result"}}},
		},
	}

	openAI, err := ToOpenAIRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(openAI)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"type":"thinking"`) || strings.Contains(string(encoded), `"thinking":`) || strings.Contains(string(encoded), "secret") {
		t.Fatalf("reasoning leaked into OpenAI payload: %s", encoded)
	}
	if len(openAI.Messages) != 4 || openAI.Messages[0].Role != "system" || openAI.Messages[1].Role != "user" {
		t.Fatalf("messages = %#v", openAI.Messages)
	}
	assistant := openAI.Messages[2]
	if assistant.Content != "answer" || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call" {
		t.Fatalf("assistant = %#v", assistant)
	}
	if tool := openAI.Messages[3]; tool.Role != "tool" || tool.ToolCallID != "call" || tool.Content != "result" {
		t.Fatalf("tool = %#v", tool)
	}
}

func TestAnthropicRoundTripPreservesRequest(t *testing.T) {
	request := AnthropicRequest{
		Model: "claude", MaxTokens: 100, OutputConfig: AnthropicOutputConfig{Effort: "xhigh"},
		System: []AnthropicContent{{Type: "text", Text: "system", CacheControl: map[string]string{"type": "ephemeral"}}},
		Messages: []AnthropicMessage{
			{Role: "user", Content: []AnthropicContent{{Type: "text", Text: "hello"}}},
			{Role: "assistant", Content: []AnthropicContent{{Type: "thinking", Thinking: "reason"}, {Type: "tool_use", ID: "tool", Name: "lookup", Input: json.RawMessage(`{"q":1}`)}}},
			{Role: "user", Content: []AnthropicContent{{Type: "tool_result", ToolUseID: "tool", Content: "done", IsError: true}}},
		},
		Tools: []AnthropicTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	unified, err := FromAnthropicRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := ToAnthropicRequest(unified)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.Model != request.Model || roundTrip.OutputConfig.Effort != "xhigh" || len(roundTrip.Messages) != 3 || roundTrip.Messages[2].Content[0].IsError != true {
		t.Fatalf("round trip = %#v", roundTrip)
	}
	if !reflect.DeepEqual(roundTrip.Tools[0].InputSchema, request.Tools[0].InputSchema) {
		t.Fatalf("tool schema = %s", roundTrip.Tools[0].InputSchema)
	}
}

func TestTranslatorPreservesMultipleToolResultsAndChoice(t *testing.T) {
	request := provider.Request{
		Model: "model", Messages: []provider.Message{
			{Role: "user", Content: []provider.Content{{Type: "text", Text: "context"}, {Type: "tool_result", ToolUseID: "a", Text: "one"}, {Type: "tool_result", ToolUseID: "b", Text: "two"}}},
		},
		ToolChoice: provider.ToolChoice{Type: "any"},
	}
	openAI, err := ToOpenAIRequest(request)
	if err != nil || len(openAI.Messages) != 3 || openAI.ToolChoice != "required" {
		t.Fatalf("OpenAI request = %#v, %v", openAI, err)
	}
	fromOpenAI, err := FromOpenAIRequest(openAI)
	if err != nil || fromOpenAI.Messages[1].Role != "user" || fromOpenAI.ToolChoice.Type != "any" {
		t.Fatalf("unified = %#v, %v", fromOpenAI, err)
	}
	anthropic, err := ToAnthropicRequest(fromOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range anthropic.Messages {
		if message.Role != "user" && message.Role != "assistant" {
			t.Fatalf("invalid Anthropic role %q", message.Role)
		}
	}
}

func TestAnthropicInBandInstructionsBecomeSystem(t *testing.T) {
	request := AnthropicRequest{
		Model: "model", MaxTokens: 10,
		System: []AnthropicContent{{Type: "text", Text: "top-level"}},
		Messages: []AnthropicMessage{
			{Role: "system", Content: []AnthropicContent{{Type: "text", Text: "system"}}},
			{Role: "developer", Content: []AnthropicContent{{Type: "text", Text: "developer"}}},
			{Role: "user", Content: []AnthropicContent{{Type: "text", Text: "hello"}}},
		},
	}
	unified, err := FromAnthropicRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(unified.System) != 3 || len(unified.Messages) != 1 || unified.Messages[0].Role != "user" {
		t.Fatalf("unified = %#v", unified)
	}
	if unified.System[0].Text != "top-level" || unified.System[1].Text != "system" || unified.System[2].Text != "developer" {
		t.Fatalf("system = %#v", unified.System)
	}
}

// A reminder injected after the conversation has started must stay in the conversation.
// System becomes the front of the upstream payload — the Codex `instructions` string is
// what the prompt cache is keyed on — so hoisting a per-turn block into it re-bills the
// whole system prompt on every turn that carries one.
func TestAnthropicMidConversationInstructionsStayInTheConversation(t *testing.T) {
	reminder := "<system-reminder>Todo list: 1 item in progress.</system-reminder>"
	request := AnthropicRequest{
		Model: "model", MaxTokens: 10,
		System: []AnthropicContent{{Type: "text", Text: "static prompt"}},
		Messages: []AnthropicMessage{
			{Role: "user", Content: []AnthropicContent{{Type: "text", Text: "start"}}},
			{Role: "assistant", Content: []AnthropicContent{{Type: "text", Text: "working"}}},
			{Role: "system", Content: []AnthropicContent{{Type: "text", Text: reminder}}},
			{Role: "user", Content: []AnthropicContent{{Type: "text", Text: "continue"}}},
		},
	}
	unified, err := FromAnthropicRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(unified.System) != 1 || unified.System[0].Text != "static prompt" {
		t.Fatalf("the reminder reached System: %#v", unified.System)
	}
	if len(unified.Messages) != 4 {
		t.Fatalf("messages = %d, want 4: %#v", len(unified.Messages), unified.Messages)
	}
	// Carried as a user message: "system" is not a role provider.Request accepts, so
	// keeping the original role would fail validation and reject the turn outright.
	if got := unified.Messages[2]; got.Role != "user" || got.Content[0].Text != reminder {
		t.Fatalf("reminder message = %#v", got)
	}
}

func TestAnthropicStringAndRedactedThinking(t *testing.T) {
	var request AnthropicRequest
	if err := json.Unmarshal([]byte(`{"model":"model","system":"system","messages":[{"role":"user","content":"hello"}],"max_tokens":10}`), &request); err != nil {
		t.Fatal(err)
	}
	if request.System[0].Text != "system" || request.Messages[0].Content[0].Text != "hello" {
		t.Fatalf("request = %#v", request)
	}
	unified, err := FromAnthropicResponse(AnthropicResponse{Content: []AnthropicContent{{Type: "redacted_thinking", Data: "secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	back := ToAnthropicResponse(unified)
	if back.Content[0].Type != "redacted_thinking" || back.Content[0].Data != "secret" {
		t.Fatalf("response = %#v", back)
	}
}

func TestAnthropicResponseNeverEmitsMissingText(t *testing.T) {
	response := ToAnthropicResponse(provider.Response{ID: "id", Model: "model", Role: "assistant", StopReason: "end_turn"})
	if len(response.Content) != 1 || response.Content[0].Type != "text" || response.Content[0].Text == "" {
		t.Fatalf("unsafe empty response = %#v", response)
	}

	translated, err := FromOpenAIResponse(OpenAIResponse{
		ID: "id", Model: "model",
		Choices: []OpenAIChoice{{Message: OpenAIMessage{Role: "assistant", Content: ""}, FinishReason: "stop"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response = ToAnthropicResponse(translated)
	if len(response.Content) != 1 || response.Content[0].Type != "text" || response.Content[0].Text == "" {
		t.Fatalf("unsafe translated response = %#v", response)
	}
}

func TestResponseTranslationUsageAndStopReason(t *testing.T) {
	response, err := FromOpenAIResponse(OpenAIResponse{
		ID: "id", Model: "model",
		Choices: []OpenAIChoice{{Message: OpenAIMessage{Role: "assistant", Content: "done"}, FinishReason: "length"}},
		Usage: func() OpenAIUsage {
			usage := OpenAIUsage{PromptTokens: 10, CompletionTokens: 5}
			usage.PromptTokensDetails.CachedTokens = 4
			return usage
		}(),
	})
	if err != nil || response.StopReason != "max_tokens" || response.Usage.InputTokens != 10 || response.Usage.CacheReadTokens != 4 {
		t.Fatalf("response = %#v, %v", response, err)
	}
	back, err := ToOpenAIResponse(response)
	if err != nil || back.Choices[0].FinishReason != "length" || back.Usage.PromptTokensDetails.CachedTokens != 4 {
		t.Fatalf("back = %#v, %v", back, err)
	}
}

func TestToolResultCarriesToolNameAfterHistoryLoss(t *testing.T) {
	// Providers that key tool results by function name (Antigravity/Gemini) cannot
	// resolve the name once the originating assistant turn has been compacted away,
	// so the name must travel on the tool_result itself.
	request := provider.Request{
		Model: "model", Messages: []provider.Message{
			{Role: "assistant", Content: []provider.Content{{Type: "tool_use", ToolUseID: "call-1", Name: "read", Input: json.RawMessage(`{}`)}}},
			{Role: "user", Content: []provider.Content{{Type: "tool_result", ToolUseID: "call-1", Text: "file body"}}},
		},
	}
	openAI, err := ToOpenAIRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	var toolMessage OpenAIMessage
	for _, message := range openAI.Messages {
		if message.Role == "tool" {
			toolMessage = message
		}
	}
	if toolMessage.Name != "read" || toolMessage.ToolCallID != "call-1" {
		t.Fatalf("tool result lost its name: %#v", toolMessage)
	}
	roundTrip, err := FromOpenAIRequest(openAI)
	if err != nil {
		t.Fatal(err)
	}
	last := roundTrip.Messages[len(roundTrip.Messages)-1]
	if last.Content[0].Name != "read" {
		t.Fatalf("round trip dropped the tool name: %#v", last.Content[0])
	}
}

func TestAnthropicRoundTripPreservesCacheControl(t *testing.T) {
	// Prompt-cache breakpoints are what keep long Claude Code sessions cheap and
	// fast. They must survive the structured path too, not only byte passthrough.
	breakpoint := map[string]string{"type": "ephemeral"}
	request := AnthropicRequest{
		Model: "claude-opus-4-5", MaxTokens: 100,
		System: []AnthropicContent{{Type: "text", Text: "system", CacheControl: breakpoint}},
		Tools:  []AnthropicTool{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`), CacheControl: breakpoint}},
		Messages: []AnthropicMessage{
			{Role: "user", Content: []AnthropicContent{{Type: "text", Text: "hello", CacheControl: breakpoint}}},
		},
	}
	unified, err := FromAnthropicRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if unified.System[0].CacheControl == nil || unified.Tools[0].CacheControl == nil || unified.Messages[0].Content[0].CacheControl == nil {
		t.Fatalf("cache_control dropped on the way in: %#v", unified)
	}
	roundTrip, err := ToAnthropicRequest(unified)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.System[0].CacheControl["type"] != "ephemeral" ||
		roundTrip.Tools[0].CacheControl["type"] != "ephemeral" ||
		roundTrip.Messages[0].Content[0].CacheControl["type"] != "ephemeral" {
		t.Fatalf("cache_control dropped on the way out: %#v", roundTrip)
	}
}

// An image inside a tool result is what Read produces for any image file, and what MCP
// screenshot tools produce. It used to be dropped in translation, so the model answered
// confidently about a picture it had never been shown — the worst available failure, since
// nothing reports it.
func TestImageInsideToolResultSurvivesTranslation(t *testing.T) {
	raw := `{"model":"m","max_tokens":16,"messages":[
	  {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/a.png"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[
	     {"type":"text","text":"Here is the screenshot:"},
	     {"type":"image","source":{"type":"base64","media_type":"image/png","data":"UE5HDATA"}}]}]}]}`
	var request AnthropicRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatal(err)
	}
	unified, err := FromAnthropicRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	last := unified.Messages[len(unified.Messages)-1]
	if len(last.Content) != 2 || last.Content[0].Type != "tool_result" || last.Content[1].Type != "image" {
		t.Fatalf("unified content = %+v, want a tool_result followed by the hoisted image", last.Content)
	}
	if last.Content[1].Data != "UE5HDATA" || last.Content[1].MediaType != "image/png" {
		t.Fatalf("image block lost its payload: %+v", last.Content[1])
	}

	upstream, err := ToOpenAIRequest(unified)
	if err != nil {
		t.Fatal(err)
	}
	// A tool message must come straight after the assistant turn that called the tool, so
	// the hoisted image follows it rather than cutting in front of it.
	roles := make([]string, 0, len(upstream.Messages))
	for _, message := range upstream.Messages {
		roles = append(roles, message.Role)
	}
	want := []string{"assistant", "tool", "user"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for index := range want {
		if roles[index] != want[index] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}
	encoded, _ := json.Marshal(upstream)
	if !strings.Contains(string(encoded), "UE5HDATA") {
		t.Fatalf("image did not reach the upstream payload: %s", encoded)
	}
}

// Text typed in the same turn as tool results moves behind them for the same reason, which
// is also a correctness fix: it used to be emitted first.
func TestTextAlongsideToolResultsFollowsThem(t *testing.T) {
	unified := provider.Request{Model: "m", Messages: []provider.Message{{
		Role: "user", Content: []provider.Content{
			{Type: "tool_result", ToolUseID: "t1", Text: "done"},
			{Type: "text", Text: "now do the next one"},
		},
	}}}
	upstream, err := ToOpenAIRequest(unified)
	if err != nil {
		t.Fatal(err)
	}
	if len(upstream.Messages) != 2 || upstream.Messages[0].Role != "tool" || upstream.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v, want tool then user", upstream.Messages)
	}
}

// A message that really needs the array form keeps it: an image cannot be a string.
func TestMixedTextAndImageKeepsTheArrayForm(t *testing.T) {
	unified := provider.Request{Model: "m", Messages: []provider.Message{{
		Role: "user", Content: []provider.Content{
			{Type: "text", Text: "what is this?"},
			{Type: "image", MediaType: "image/png", Data: "AAA"},
		},
	}}}
	upstream, err := ToOpenAIRequest(unified)
	if err != nil {
		t.Fatal(err)
	}
	parts, ok := upstream.Messages[0].Content.([]OpenAIContentPart)
	if !ok {
		t.Fatalf("content is %T, want the parts array", upstream.Messages[0].Content)
	}
	if len(parts) != 2 || parts[1].Type != "image_url" {
		t.Fatalf("parts = %+v", parts)
	}
}
