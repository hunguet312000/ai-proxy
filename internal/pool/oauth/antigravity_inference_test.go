package oauth

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"literouter/internal/translator"
)

func TestBuildAntigravityEnvelopeCorrelatesToolResultAndSignature(t *testing.T) {
	request := translator.OpenAIRequest{
		Model: "gemini-pro-agent",
		Tools: []translator.OpenAITool{{Type: "function", Function: translator.OpenAIFunction{Name: "lookup tool", Parameters: []byte(`{"type":"object"}`)}}},
		Messages: []translator.OpenAIMessage{
			{Role: "assistant", ToolCalls: []translator.OpenAIToolCall{{ID: "call_1", Type: "function", Function: translator.OpenAIFunctionCall{Name: "lookup tool", Arguments: `{"q":"x"}`}, ThoughtSignature: "signature"}}},
			{Role: "tool", ToolCallID: "call_1", Content: "result"},
		},
	}
	envelope, err := buildAntigravityEnvelope(request, "project", "session")
	if err != nil {
		t.Fatal(err)
	}
	call := envelope.Request.Contents[0].Parts[0]
	if call.FunctionCall.Name != "lookup_tool" || call.ThoughtSignature != "signature" {
		t.Fatalf("function call = %#v", call)
	}
	response := envelope.Request.Contents[1].Parts[0].FunctionResponse
	if response == nil || response.Name != "lookup_tool" {
		t.Fatalf("function response = %#v", response)
	}
}

func TestBuildAntigravityEnvelopeAddsSignatureFallback(t *testing.T) {
	request := translator.OpenAIRequest{Messages: []translator.OpenAIMessage{{
		Role: "assistant",
		ToolCalls: []translator.OpenAIToolCall{{
			ID: "call_1", Type: "function",
			Function: translator.OpenAIFunctionCall{Name: "lookup", Arguments: `{}`},
		}},
	}}}
	envelope, err := buildAntigravityEnvelope(request, "project", "session")
	if err != nil {
		t.Fatal(err)
	}
	if got := envelope.Request.Contents[0].Parts[0].ThoughtSignature; got != "skip_thought_signature_validator" {
		t.Fatalf("thought signature = %q", got)
	}
}

func TestAntigravitySignatureSurvivesHistoryReplay(t *testing.T) {
	// The client resends the whole conversation every turn, so one tool call is
	// looked up once per remaining turn. Consuming the entry on first read made
	// every older call degrade to the skip placeholder, which is what stopped the
	// model mid-task.
	store := antigravitySignatureStore{entries: make(map[string]antigravitySignatureEntry)}
	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	store.put("session", "call", "signature", now)
	for turn := 1; turn <= 5; turn++ {
		if got := store.lookup("session", "call", now.Add(time.Duration(turn)*time.Minute)); got != "signature" {
			t.Fatalf("turn %d: signature = %q, want it still cached", turn, got)
		}
	}
	// Each replay refreshes the entry, so an active session cannot expire mid-run.
	if got := store.lookup("session", "call", now.Add(5*time.Minute).Add(antigravitySignatureTTL-time.Second)); got != "signature" {
		t.Fatalf("refreshed signature = %q", got)
	}
}

func TestAntigravitySessionKeyFallsBackToFirstUserTurn(t *testing.T) {
	// Claude Code sends no X-Conversation-ID, and an empty session id disabled the
	// signature cache entirely.
	request := translator.OpenAIRequest{Messages: []translator.OpenAIMessage{
		{Role: "system", Content: "rules"},
		{Role: "user", Content: "audit the billing package"},
	}}
	key := antigravitySessionKey(request, "")
	if key == "" {
		t.Fatal("session key is empty without a conversation header")
	}
	// Stable across the turns of one session: later messages must not change it.
	grown := request
	grown.Messages = append(append([]translator.OpenAIMessage{}, request.Messages...),
		translator.OpenAIMessage{Role: "assistant", Content: "working"},
		translator.OpenAIMessage{Role: "user", Content: "continue"})
	if regrown := antigravitySessionKey(grown, ""); regrown != key {
		t.Fatalf("session key changed as the conversation grew: %q -> %q", key, regrown)
	}
	// A different conversation must not share signatures.
	other := translator.OpenAIRequest{Messages: []translator.OpenAIMessage{
		{Role: "user", Content: "something else entirely"},
	}}
	if antigravitySessionKey(other, "") == key {
		t.Fatal("different conversations collide on the same session key")
	}
	// An explicit header still wins.
	if got := antigravitySessionKey(request, "explicit"); got != "explicit" {
		t.Fatalf("header session id = %q", got)
	}
}

func TestAntigravitySignatureStoreBoundsAndExpires(t *testing.T) {
	store := antigravitySignatureStore{entries: make(map[string]antigravitySignatureEntry)}
	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	store.put("session", "expired", "signature", now)
	if got := store.lookup("session", "expired", now.Add(antigravitySignatureTTL)); got != "" {
		t.Fatalf("expired signature = %q", got)
	}
	store.put("session", "large", strings.Repeat("x", antigravitySignatureMaxSize+1), now)
	if len(store.entries) != 0 {
		t.Fatalf("oversized signature was cached")
	}
	for i := 0; i < antigravitySignatureMax+1; i++ {
		store.put("session", fmt.Sprintf("call-%d", i), "signature", now.Add(time.Duration(i)*time.Second))
	}
	if len(store.entries) != antigravitySignatureMax {
		t.Fatalf("entries = %d", len(store.entries))
	}
}

func TestBuildAntigravityEnvelopeSanitizesUnsupportedSchemaFields(t *testing.T) {
	request := translator.OpenAIRequest{Tools: []translator.OpenAITool{{Function: translator.OpenAIFunction{
		Name: "lookup", Parameters: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","propertyNames":{"type":"string"},"properties":{"value":{"type":"integer","exclusiveMinimum":0},"$schema":{"type":"string"}},"required":["value"]}`),
	}}}}
	envelope, err := buildAntigravityEnvelope(request, "project", "session")
	if err != nil {
		t.Fatal(err)
	}
	parameters := string(envelope.Request.Tools[0].FunctionDeclarations[0].Parameters)
	if strings.Contains(parameters, `"$schema":"https`) || strings.Contains(parameters, `"propertyNames"`) || strings.Contains(parameters, `"exclusiveMinimum"`) {
		t.Fatalf("unsupported schema fields remain: %s", parameters)
	}
	if !strings.Contains(parameters, `"properties":{"$schema":{"type":"string"},"value":{"type":"integer"}}`) {
		t.Fatalf("property names or supported fields were removed: %s", parameters)
	}
}

func TestBuildAntigravityEnvelopeRejectsSanitizedNameCollision(t *testing.T) {
	_, err := buildAntigravityEnvelope(translator.OpenAIRequest{Tools: []translator.OpenAITool{
		{Function: translator.OpenAIFunction{Name: "lookup tool"}},
		{Function: translator.OpenAIFunction{Name: "lookup@tool"}},
	}}, "project", "session")
	if err == nil || !strings.Contains(err.Error(), "collide") {
		t.Fatalf("error = %v", err)
	}
}

func TestReadAntigravitySSESupportsMultilineAndEOF(t *testing.T) {
	stream := "event: message\n" +
		"data: {\"response\":\n" +
		"data: {\"value\":1}}\n\n" +
		"data: {\"last\":true}"
	var events []string
	err := readAntigravitySSE(strings.NewReader(stream), func(raw []byte) error {
		events = append(events, string(raw))
		return nil
	})
	if err != nil || len(events) != 2 || events[0] != "{\"response\":\n{\"value\":1}}" || events[1] != "{\"last\":true}" {
		t.Fatalf("events = %#v, error = %v", events, err)
	}
}

func TestAntigravitySSEDeduplicatesCumulativeContentAndCalls(t *testing.T) {
	stream := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"},{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"q\":\"x\"}}}]}}]}}\n\n" +
		"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello world\"},{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"q\":\"x\"}}}]},\"finishReason\":\"MAX_TOKENS\"}]}}\n\n"
	response, err := antigravitySSEToOpenAI(strings.NewReader(stream), "model", "session")
	if err != nil {
		t.Fatal(err)
	}
	if response.Choices[0].Message.Content != "Hello world" || len(response.Choices[0].Message.ToolCalls) != 1 || response.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("response = %#v", response)
	}
}

func TestAntigravityFinishReason(t *testing.T) {
	if got := antigravityFinishReason("MAX_TOKENS", false); got != "length" {
		t.Fatalf("MAX_TOKENS = %q", got)
	}
	if got := antigravityFinishReason("SAFETY", false); got != "content_filter" {
		t.Fatalf("SAFETY = %q", got)
	}
}

func TestAntigravitySSEPreservesThoughtSignature(t *testing.T) {
	stream := `data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}},"thoughtSignature":"signature"}]},"finishReason":"STOP"}]}}` + "\n"
	response, err := antigravitySSEToOpenAI(strings.NewReader(stream), "model", "session")
	if err != nil {
		t.Fatal(err)
	}
	call := response.Choices[0].Message.ToolCalls[0]
	if call.ThoughtSignature != "signature" {
		t.Fatalf("tool call = %#v", call)
	}
}

func TestAntigravityStreamEmitsIncrementalChunks(t *testing.T) {
	// Buffering the whole Antigravity response meant no bytes reached the caller
	// until the turn finished, which reads as a hung proxy on long generations.
	stream := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}]}}]}}\n\n" +
		"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello world\"}]}}]}}\n\n" +
		"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"q\":\"x\"}}}]},\"finishReason\":\"STOP\"}]}}\n\n"
	body := antigravitySSEToChatStream(io.NopCloser(strings.NewReader(stream)), "model", "session")
	var deltas []string
	var finish string
	err := readSSEJSON(body, func(raw []byte) error {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return err
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				deltas = append(deltas, choice.Delta.Content)
			}
			for _, call := range choice.Delta.ToolCalls {
				deltas = append(deltas, "tool:"+call.Function.Name)
			}
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Cumulative text must be forwarded as deltas, not repeated in full.
	if len(deltas) != 3 || deltas[0] != "Hello" || deltas[1] != " world" || deltas[2] != "tool:lookup" {
		t.Fatalf("chunks = %#v", deltas)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish reason = %q", finish)
	}
}

func TestAntigravityWhitespaceOnlyTurnProducesNoContent(t *testing.T) {
	// The turn that made the agent stop: a lone whitespace text part plus STOP and
	// no function call. Forwarded as content it becomes a normal end_turn and the
	// client treats the task as finished; emitting nothing lets the gateway spot the
	// empty turn and retry it.
	sse := io.NopCloser(strings.NewReader(
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":" "}]},"finishReason":"STOP"}]}}` + "\n\n"))
	body := antigravitySSEToChatStream(sse, "gemini-3.1-pro-high", "session")
	defer body.Close()
	encoded, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("stream error = %v", err)
	}
	if strings.Contains(string(encoded), `"content"`) {
		t.Fatalf("whitespace-only turn emitted content: %q", encoded)
	}
	if !strings.Contains(string(encoded), `"finish_reason":"stop"`) {
		t.Fatalf("terminal chunk missing: %q", encoded)
	}
}

func TestAntigravityWhitespaceIsKeptWhenRealOutputFollows(t *testing.T) {
	sse := io.NopCloser(strings.NewReader(
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":" "}]}}]}}` + "\n\n" +
			`data: {"response":{"candidates":[{"content":{"parts":[{"text":" done"}]},"finishReason":"STOP"}]}}` + "\n\n"))
	body := antigravitySSEToChatStream(sse, "gemini-3.1-pro-high", "session")
	defer body.Close()
	encoded, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("stream error = %v", err)
	}
	// Held-back whitespace must be restored, not dropped, once real text arrives.
	if !strings.Contains(string(encoded), `"content":" done"`) {
		t.Fatalf("real output was not forwarded intact: %q", encoded)
	}
}

func TestAntigravityWhitespaceIsFlushedBeforeToolCall(t *testing.T) {
	sse := io.NopCloser(strings.NewReader(
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":" "}]}}]}}` + "\n\n" +
			`data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"Read","args":{"file_path":"a"}}}]},"finishReason":"STOP"}]}}` + "\n\n"))
	body := antigravitySSEToChatStream(sse, "gemini-3.1-pro-high", "session")
	defer body.Close()
	encoded, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("stream error = %v", err)
	}
	if !strings.Contains(string(encoded), `"tool_calls"`) {
		t.Fatalf("tool call missing: %q", encoded)
	}
	if !strings.Contains(string(encoded), `"finish_reason":"tool_calls"`) {
		t.Fatalf("a turn with a tool call must not report stop: %q", encoded)
	}
}

func TestAntigravityReportsCachedPrefixTokens(t *testing.T) {
	// Without cachedContentTokenCount every Antigravity request looked like a 0%
	// cache hit, which made ~98M tokens of spend unmeasurable.
	sse := io.NopCloser(strings.NewReader(
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],` +
			`"usageMetadata":{"promptTokenCount":50000,"candidatesTokenCount":120,"cachedContentTokenCount":47000}}}` + "\n\n"))
	body := antigravitySSEToChatStream(sse, "gemini-3.1-pro-high", "session")
	defer body.Close()
	encoded, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("stream error = %v", err)
	}
	if !strings.Contains(string(encoded), `"cached_tokens":47000`) {
		t.Fatalf("cached prefix tokens not reported: %q", encoded)
	}
}

func TestAntigravityNonStreamingReportsCachedTokens(t *testing.T) {
	sse := strings.NewReader(
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],` +
			`"usageMetadata":{"promptTokenCount":50000,"candidatesTokenCount":120,"cachedContentTokenCount":47000}}}` + "\n\n")
	response, err := antigravitySSEToOpenAI(sse, "gemini-3.1-pro-high", "session")
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage.PromptTokensDetails.CachedTokens != 47000 ||
		!response.Usage.PromptTokensDetails.CachedTokensReported {
		t.Fatalf("usage = %#v", response.Usage)
	}
}
