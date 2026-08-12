package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
	"literouter/internal/toolvalidate"
	"literouter/internal/translator"
)

type blockingReadCloser struct {
	closed chan struct{}
}

func (reader *blockingReadCloser) Read([]byte) (int, error) {
	<-reader.closed
	return 0, io.ErrClosedPipe
}

func (reader *blockingReadCloser) Close() error {
	select {
	case <-reader.closed:
	default:
		close(reader.closed)
	}
	return nil
}

func TestReadOpenAIStreamAcceptsLargeBoundedLine(t *testing.T) {
	content := strings.Repeat("x", 128*1024)
	input := "data: {\"id\":\"one\",\"choices\":[{\"delta\":{\"content\":\"" + content + "\"}}]}\n\ndata: [DONE]\n\n"
	var got string
	err := readOpenAIStream(strings.NewReader(input), func(chunk OpenAIStreamChunk) error {
		got = chunk.Choices[0].Delta.Content
		return nil
	})
	if err != nil || got != content {
		t.Fatalf("large stream content = %d bytes, error = %v", len(got), err)
	}
}

func TestReadOpenAIStreamRejectsOversizedLine(t *testing.T) {
	input := "data: " + strings.Repeat("x", maxSSELineBytes+1) + "\n\n"
	err := readOpenAIStreamData(strings.NewReader(input), func([]byte) error { return nil })
	if !errors.Is(err, errSSELineTooLarge) {
		t.Fatalf("error = %v", err)
	}
}

func TestReadOpenAIStreamIdleTimeout(t *testing.T) {
	reader := &blockingReadCloser{closed: make(chan struct{})}
	started := time.Now()
	err := readOpenAIStreamWithIdleTimeout(context.Background(), reader, 10*time.Millisecond, func(OpenAIStreamChunk) error { return nil })
	if !errors.Is(err, errUpstreamStreamIdle) {
		t.Fatalf("error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("idle timeout did not close the blocked stream")
	}
}

func TestReadOpenAIStreamCancellationClosesBody(t *testing.T) {
	reader := &blockingReadCloser{closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := readOpenAIStreamWithIdleTimeout(ctx, reader, time.Minute, func(OpenAIStreamChunk) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	select {
	case <-reader.closed:
	default:
		t.Fatal("cancellation did not close the upstream body")
	}
}

type fakeStreamClient struct {
	content string
	body    io.ReadCloser
	err     error
	calls   int
	last    translator.OpenAIRequest
}

func (f *fakeStreamClient) DoStream(_ context.Context, _ string, request any) (io.ReadCloser, error) {
	f.calls++
	f.last = request.(translator.OpenAIRequest)
	if f.err != nil {
		return nil, f.err
	}
	if f.body != nil {
		return f.body, nil
	}
	return io.NopCloser(strings.NewReader(f.content)), nil
}

func TestStreamOpenAIParsesSSE(t *testing.T) {
	client := fakeStreamClient{content: "data: {\"id\":\"one\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"}
	var chunks []OpenAIStreamChunk
	err := streamOpenAI(context.Background(), &client, translator.OpenAIRequest{}, func(chunk OpenAIStreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil || len(chunks) != 1 || chunks[0].Choices[0].Delta.Content != "hello" {
		t.Fatalf("chunks = %#v, error = %v", chunks, err)
	}
}

func TestOpenAIStreamingHandler(t *testing.T) {
	stream := &fakeStreamClient{content: "data: {\"id\":\"one\",\"model\":\"model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"}
	service := New(Options{OpenAIStream: stream})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(recorder.Body.String(), `"content":"hello"`) {
		t.Fatalf("response = %d %#v %q", recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

func TestOpenAIStreamingRecordsEstimatedUsage(t *testing.T) {
	stream := &fakeStreamClient{content: "data: {\"id\":\"one\",\"model\":\"model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"}
	usageEvents := make(chan UsageEvent, 1)
	service := New(Options{OpenAIStream: stream, OnUsage: func(event UsageEvent) { usageEvents <- event }})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	select {
	case event := <-usageEvents:
		if event.Endpoint != "/v1/chat/completions" || event.Model != "model" || event.PromptTokens <= 0 || event.CompletionTokens <= 0 {
			t.Fatalf("estimated usage event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("estimated usage event was not recorded")
	}
}

func TestAnthropicStreamingCancellationDoesNotRecordMalformedUsage(t *testing.T) {
	reader := &blockingReadCloser{closed: make(chan struct{})}
	stream := &fakeStreamClient{body: reader}
	usageEvents := make(chan UsageEvent, 1)
	service := New(Options{OpenAIStream: stream, OnUsage: func(event UsageEvent) { usageEvents <- event }})
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	c := echo.New().NewContext(request, recorder)
	cancel()
	if err := service.messagesStream(c, translator.AnthropicRequest{
		Model: "model", MaxTokens: 10,
		Messages: []translator.AnthropicMessage{{Role: "user", Content: []translator.AnthropicContent{{Type: "text", Text: "hi"}}}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-usageEvents:
		t.Fatalf("cancellation recorded usage event: %#v", event)
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-reader.closed:
	default:
		t.Fatal("cancellation did not close the upstream body")
	}
}

func TestStreamingPromptCacheUsesConversationHeader(t *testing.T) {
	stream := &fakeStreamClient{content: "data: [DONE]\n\n"}
	service := New(Options{OpenAIStream: stream, PromptMinBytes: 1})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4.1-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Conversation-ID", "private-thread")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || len(stream.last.PromptCacheKey) != 64 || strings.Contains(stream.last.PromptCacheKey, "private-thread") {
		t.Fatalf("response = %d, request = %#v", recorder.Code, stream.last)
	}
}

func TestAnthropicStreamingAppliesPromptCacheKey(t *testing.T) {
	stream := &fakeStreamClient{content: "data: [DONE]\n\n"}
	service := New(Options{OpenAIStream: stream, PromptMinBytes: 1})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || stream.last.PromptCacheKey == "" {
		t.Fatalf("response = %d, request = %#v", recorder.Code, stream.last)
	}
}

func TestStreamingXAIPrefixedModelUsesXAIClient(t *testing.T) {
	openAI := &fakeStreamClient{}
	xai := &fakeStreamClient{content: "data: [DONE]\n\n"}
	service := New(Options{OpenAIStream: openAI, XAIStream: xai})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"xai/grok-4","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || openAI.calls != 0 || xai.calls != 1 || xai.last.Model != "grok-4" {
		t.Fatalf("response = %d; calls = %d/%d; last = %#v", recorder.Code, openAI.calls, xai.calls, xai.last)
	}
}

func TestStreamingAliasFallsBackAcrossProviders(t *testing.T) {
	openAI := &fakeStreamClient{err: &provider.ProviderError{StatusCode: 429, Message: "limited"}}
	xai := &fakeStreamClient{content: "data: [DONE]\n\n"}
	service := New(Options{
		OpenAIStream: openAI, XAIStream: xai,
		Aliases: map[string][]string{"fast": {"gpt-4.1", "grok-4"}},
	})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"fast","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || openAI.calls != 1 || xai.calls != 1 || xai.last.Model != "grok-4" {
		t.Fatalf("response = %d %q; calls = %d/%d; last = %#v", recorder.Code, recorder.Body.String(), openAI.calls, xai.calls, xai.last)
	}
}

func TestAnthropicStreamingInterleavedToolCalls(t *testing.T) {
	stream := &fakeStreamClient{content: strings.Join([]string{
		`data: {"id":"one","model":"xai/grok-4.5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-a","type":"function","function":{"name":"read","arguments":"{\"path\":"}},{"index":1,"id":"call-b","type":"function","function":{"name":"search","arguments":"{\"query\":"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"one","model":"xai/grok-4.5","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"needle\"}"}},{"index":0,"function":{"arguments":"\"file\"}"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"one","model":"xai/grok-4.5","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		"",
		"data: [DONE]", "",
	}, "\n")}
	service := New(Options{XAIStream: stream})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"xai/grok-4.5","messages":[{"role":"user","content":"use tools"}],"tools":[{"name":"read","input_schema":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}},{"name":"search","input_schema":{"type":"object","required":["query"],"properties":{"query":{"type":"string"}}}}],"max_tokens":100,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, expected := range []string{
		`"id":"call-a","input":{},"name":"read","type":"tool_use"`,
		`"partial_json":"{\"path\":\"file\"}"`,
		`"id":"call-b","input":{},"name":"search","type":"tool_use"`,
		`"partial_json":"{\"query\":\"needle\"}"`,
		`"stop_reason":"tool_use"`, "event: message_stop",
	} {
		if recorder.Code != http.StatusOK || !strings.Contains(body, expected) {
			t.Fatalf("missing %q in response %d %q", expected, recorder.Code, body)
		}
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("unexpected stream error: %q", body)
	}
}

func TestMergeToolArguments(t *testing.T) {
	for _, test := range []struct {
		name     string
		current  string
		incoming string
		want     string
	}{
		{name: "fragment", current: `{"command":`, incoming: `"pwd"}`, want: `{"command":"pwd"}`},
		{name: "repeated delta is data", current: `"a"`, incoming: `"a"`, want: `"a""a"`},
		{name: "valid scalar delta", current: `{"value":`, incoming: `10`, want: `{"value":10`},
		{name: "prefix fragment", current: `{"command":"pwd"`, incoming: `}`, want: `{"command":"pwd"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := mergeToolArguments(test.current, test.incoming); got != test.want {
				t.Fatalf("mergeToolArguments(%q, %q) = %q, want %q", test.current, test.incoming, got, test.want)
			}
		})
	}
}

func TestValidateToolArguments(t *testing.T) {
	schemas := toolvalidate.Compile([]translator.OpenAITool{{Type: "function", Function: translator.OpenAIFunction{
		Name: "Bash", Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{"command":{"type":"string"},"options":{"type":"object","properties":{"timeout":{"type":"integer"}},"required":["timeout"]}},
			"required":["command"],
			"additionalProperties":false
		}`),
	}}})
	for _, test := range []struct {
		name       string
		arguments  string
		undeclared bool
		wantError  string
	}{
		{name: "object", arguments: `{"command":"printf \"hello\"\n"}`},
		{name: "nested", arguments: `{"command":"pwd","options":{"timeout":30}}`},
		{name: "empty", arguments: "", wantError: "schema_mismatch"},
		{name: "malformed", arguments: `{"command":`, wantError: "malformed_json"},
		{name: "array", arguments: `["pwd"]`, wantError: "non_object"},
		{name: "null", arguments: `null`, wantError: "non_object"},
		{name: "missing required", arguments: `{"input":"sensitive-command"}`, wantError: "schema_mismatch"},
		{name: "wrong type", arguments: `{"command":42}`, wantError: "schema_mismatch"},
		{name: "nested violation", arguments: `{"command":"pwd","options":{"timeout":"slow"}}`, wantError: "schema_mismatch"},
		{name: "additional property", arguments: `{"command":"pwd","secret":"do-not-leak"}`, wantError: "schema_mismatch"},
		{name: "undeclared", arguments: `{"command":"pwd"}`, undeclared: true, wantError: "undeclared_tool"},
	} {
		t.Run(test.name, func(t *testing.T) {
			validator := schemas
			if test.undeclared {
				validator = nil
			}
			err := validator.Validate("Bash", test.arguments)
			if test.wantError == "" && err != nil {
				t.Fatalf("validateToolArguments() error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("validateToolArguments() error = %v, want %q", err, test.wantError)
			}
			if err != nil && test.arguments != "" && strings.Contains(err.Error(), test.arguments) {
				t.Fatalf("error leaked arguments: %v", err)
			}
		})
	}
}

// An unusable schema must cost that one tool its validation, not the turn. Refusing the
// request meant a single tool the model might never call took down every turn in the
// session, and the tool list is whatever MCP servers the client has loaded.
func TestCompileToolSchemasSkipsAnInvalidSchemaWithoutFailingTheTurn(t *testing.T) {
	schemas := toolvalidate.Compile([]translator.OpenAITool{
		{Type: "function", Function: translator.OpenAIFunction{
			Name: "Bash", Parameters: json.RawMessage(`{"type":"not-a-json-schema-type"}`),
		}},
		{Type: "function", Function: translator.OpenAIFunction{
			Name: "Read", Parameters: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`),
		}},
	})
	// The healthy sibling keeps its validation.
	if err := schemas.Validate("Read", `{}`); err == nil {
		t.Fatal("Read should still be validated against its schema")
	}
	// The unusable one is advisory only: the caller logs and forwards the call.
	if err := schemas.Validate("Bash", `{"command":"ls"}`); err == nil {
		t.Fatal("a skipped schema should report undeclared rather than pass silently")
	}
}

func TestAnthropicStreamingRejectsMissingToolTerminalAtCleanEOF(t *testing.T) {
	stream := &fakeStreamClient{content: strings.Join([]string{
		`data: {"id":"one","model":"xai/grok-4.5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-a","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":null}]}`,
		"",
		"data: [DONE]", "",
	}, "\n")}
	usageEvents := make(chan UsageEvent, 1)
	service := New(Options{XAIStream: stream, OnUsage: func(event UsageEvent) { usageEvents <- event }})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"xai/grok-4.5","messages":[{"role":"user","content":"use tool"}],"tools":[{"name":"read","input_schema":{"type":"object"}}],"max_tokens":100,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	// The upstream died before its finish reason, but the tool call it did finish
	// writing is still valid. Delivering it with a proper terminal keeps the turn
	// usable; an `event: error` here is what surfaces as a mid-response failure.
	if recorder.Code != http.StatusOK || strings.Contains(body, "event: error") {
		t.Fatalf("error frame emitted for recoverable truncation: %d %q", recorder.Code, body)
	}
	if !strings.Contains(body, `"name":"read"`) || !strings.Contains(body, `"stop_reason":"tool_use"`) || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("truncated tool turn not closed cleanly: %q", body)
	}
	select {
	case event := <-usageEvents:
		if event.Status != "malformed_stream" {
			t.Fatalf("usage event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("malformed stream usage event was not recorded")
	}
}

func TestAnthropicStreamingBrokenAfterFirstTokenClosesCleanly(t *testing.T) {
	stream := &fakeStreamClient{content: strings.Join([]string{
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`,
		"", `data: {malformed}`, "",
	}, "\n")}
	service := New(Options{OpenAIStream: stream})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	// Tokens already reached the client, so the turn is closed as truncated rather
	// than as an error, and it must not claim a successful end_turn either.
	if recorder.Code != http.StatusOK || strings.Contains(body, "event: error") {
		t.Fatalf("error frame emitted after first token: %d %q", recorder.Code, body)
	}
	if !strings.Contains(body, `"delta":{"text":"partial","type":"text_delta"}`) {
		t.Fatalf("partial content discarded: %q", body)
	}
	if !strings.Contains(body, `"stop_reason":"max_tokens"`) || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("truncated stream not terminated correctly: %q", body)
	}
	if strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("truncated stream reported success: %q", body)
	}
}

func TestAnthropicStreamingEmptyCompletionGetsTextBlock(t *testing.T) {
	stream := &fakeStreamClient{content: strings.Join([]string{
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"", "data: [DONE]", "",
	}, "\n")}
	service := New(Options{OpenAIStream: stream})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `"content_block":{"text":"","type":"text"}`) ||
		!strings.Contains(body, `"delta":{"text":" ","type":"text_delta"}`) {
		t.Fatalf("unsafe empty stream = %d %q", recorder.Code, body)
	}
}

func TestAnthropicStreamingKeepsReasoningOutOfTranscript(t *testing.T) {
	stream := &fakeStreamClient{content: strings.Join([]string{
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{"reasoning_content":"analysis only"},"finish_reason":null}]}`,
		"",
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{"content":"final answer"},"finish_reason":null}]}`,
		"",
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]", "",
	}, "\n")}
	service := New(Options{OpenAIStream: stream})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	// reasoning_content has no Anthropic thinking signature. Forwarding it as
	// assistant text put internal deliberation into the transcript, which the model
	// then re-read as its own answer and acted on twice.
	if recorder.Code != http.StatusOK || strings.Contains(body, "analysis only") || strings.Contains(body, "thinking_delta") {
		t.Fatalf("reasoning leaked into transcript = %d %q", recorder.Code, body)
	}
	if !strings.Contains(body, `"delta":{"text":"final answer","type":"text_delta"}`) {
		t.Fatalf("answer text missing: %q", body)
	}
}

func TestAnthropicSSEProtocolOrder(t *testing.T) {
	stream := &fakeStreamClient{content: strings.Join([]string{
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		"", `data: {"id":"one","model":"model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"", "data: [DONE]", "",
	}, "\n")}
	service := New(Options{OpenAIStream: stream})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	events := []string{"event: message_start", "event: content_block_start", "event: content_block_delta", "event: content_block_stop", "event: message_delta", "event: message_stop"}
	previous := -1
	for _, event := range events {
		index := strings.Index(body, event)
		if index <= previous {
			t.Fatalf("protocol event %q out of order: %q", event, body)
		}
		previous = index
	}
}

func TestAnthropicStreamingHandler(t *testing.T) {
	stream := &fakeStreamClient{content: strings.Join([]string{
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		"",
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`,
		"",
		"data: [DONE]", "",
	}, "\n")}
	usageEvents := make(chan UsageEvent, 1)
	service := New(Options{OpenAIStream: stream, OnUsage: func(event UsageEvent) { usageEvents <- event }})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":10,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, expected := range []string{
		"event: message_start", "text_delta",
		`"usage":{"cache_creation_input_tokens":0,"cache_read_input_tokens":2,"input_tokens":7,"output_tokens":1}`,
		"event: message_stop",
	} {
		if recorder.Code != http.StatusOK || !strings.Contains(body, expected) {
			t.Fatalf("missing %q in response %d %q", expected, recorder.Code, body)
		}
	}
	// message_start must carry a real input estimate. Reporting zero blinded Claude
	// Code's own context accounting, so it never compacted before hitting the limit.
	start := body[:strings.Index(body, "event: content_block_start")]
	if strings.Contains(start, `"input_tokens":0`) {
		t.Fatalf("message_start reported no input tokens: %q", start)
	}
	select {
	case event := <-usageEvents:
		if event.Provider != "openai" || event.Model != "model" || event.Endpoint != "/v1/messages" || event.PromptTokens != 7 || event.CompletionTokens != 1 || event.CachedTokens != 2 {
			t.Fatalf("usage event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("usage event was not recorded")
	}
}

func TestEstimateStreamUsageIgnoresZeroReportedUsage(t *testing.T) {
	request := translator.OpenAIRequest{
		Model:    "xai/grok-4.5",
		Messages: []translator.OpenAIMessage{{Role: "user", Content: "hello from literouter usage test"}},
	}
	// Simulated synthetic OAuth terminal chunk: usage present but all zeros with Reported=true.
	usage := translator.OpenAIUsage{PromptTokensReported: true, CompletionTokensReported: true}
	got, promptEst, completionEst := estimateStreamUsage(request, "assistant reply text", usage)
	if !promptEst || !completionEst {
		t.Fatalf("expected estimation flags, got prompt=%v completion=%v", promptEst, completionEst)
	}
	if got.PromptTokens <= 0 || got.CompletionTokens <= 0 {
		t.Fatalf("estimated usage still zero: %#v", got)
	}
}

func TestMessagesContextLengthErrorIsReturnedToCLI(t *testing.T) {
	client := &fakeStreamClient{err: &provider.ProviderError{
		StatusCode: http.StatusRequestEntityTooLarge,
		Code:       "context_length_exceeded",
		Message:    "Your input exceeds the context window of this model.",
	}}
	service := New(Options{OpenAIStream: client})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cx/gpt-5.6-sol","messages":[{"role":"user","content":"long"}],"max_tokens":10,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	// 400 invalid_request_error with "prompt is too long" is the shape Claude Code
	// compacts on. A 413 — or the 502 this used to be — reads as a server fault and
	// gets retried with the same oversized prompt.
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"invalid_request_error"`) ||
		!strings.Contains(body, "prompt is too long") ||
		!strings.Contains(body, "Your input exceeds the context window of this model.") {
		t.Fatalf("CLI error body = %s", body)
	}
}

// errorBodyStreamClient hands out a fresh body per call that fails on the first
// read, so retry behaviour is observable through calls.
type errorBodyStreamClient struct {
	err   error
	calls int
}

func (c *errorBodyStreamClient) DoStream(_ context.Context, _ string, _ any) (io.ReadCloser, error) {
	c.calls++
	return io.NopCloser(&failingReader{err: c.err}), nil
}

type failingReader struct{ err error }

func (r *failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestMessagesMidStreamContextErrorIsNotRetried(t *testing.T) {
	client := &errorBodyStreamClient{err: &provider.ProviderError{
		Provider:   "codex OAuth",
		StatusCode: http.StatusRequestEntityTooLarge,
		Code:       "context_length_exceeded",
		Message:    "Your input exceeds the context window of this model.",
	}}
	service := New(Options{OpenAIStream: client})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cx/gpt-5.6-sol","messages":[{"role":"user","content":"long"}],"max_tokens":10,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "prompt is too long") {
		t.Fatalf("CLI error body = %s", recorder.Body.String())
	}
	// Every candidate carries the same conversation, so a second attempt can only
	// waste an upstream round trip before failing identically.
	if client.calls != 1 {
		t.Fatalf("upstream attempts = %d, want 1", client.calls)
	}
}

type sequenceStreamClient struct {
	responses []string
	calls     int
}

func (c *sequenceStreamClient) DoStream(context.Context, string, any) (io.ReadCloser, error) {
	index := c.calls
	c.calls++
	if index >= len(c.responses) {
		return nil, errors.New("no candidates left")
	}
	return io.NopCloser(strings.NewReader(c.responses[index])), nil
}

func TestAnthropicStreamingToolCallWithStopFinishReportsToolUse(t *testing.T) {
	// Some OpenAI-compatible upstreams report finish_reason "stop" even though they
	// emitted tool calls. Passing end_turn through makes the client skip the call
	// and re-prompt, which is the repeated-tool-call loop.
	stream := &fakeStreamClient{content: strings.Join([]string{
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-a","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"", "data: [DONE]", "",
	}, "\n")}
	service := New(Options{OpenAIStream: stream})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"go"}],"tools":[{"name":"read","input_schema":{"type":"object"}}],"max_tokens":100,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, `"stop_reason":"tool_use"`) || strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("tool turn reported wrong stop reason: %q", body)
	}
}

func TestAnthropicStreamingMalformedToolArgumentsStayInStream(t *testing.T) {
	// Two concatenated objects is a common streaming-merge defect. Aborting the turn
	// over it loses the whole response; repairing keeps the edit alive.
	stream := &fakeStreamClient{content: strings.Join([]string{
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-a","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.go\"}{\"path\":\"a.go\"}"}}]},"finish_reason":"tool_calls"}]}`,
		"", "data: [DONE]", "",
	}, "\n")}
	service := New(Options{OpenAIStream: stream})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"go"}],"tools":[{"name":"read","input_schema":{"type":"object"}}],"max_tokens":100,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if strings.Contains(body, "event: error") {
		t.Fatalf("malformed tool arguments aborted the stream: %q", body)
	}
	if !strings.Contains(body, `{\"path\":\"a.go\"}`) || !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Fatalf("tool call not repaired and forwarded: %q", body)
	}
}

func TestAnthropicStreamingRetriesNextCandidateBeforeFirstToken(t *testing.T) {
	// The first candidate produces nothing, so no bytes reached the client yet and
	// the next candidate can still serve the turn transparently.
	stream := &sequenceStreamClient{responses: []string{
		"",
		strings.Join([]string{
			`data: {"id":"one","model":"model-b","choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":null}]}`,
			"", `data: {"id":"one","model":"model-b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			"", "data: [DONE]", "",
		}, "\n"),
	}}
	service := New(Options{OpenAIStream: stream, Aliases: map[string][]string{"alias": {"model-a", "model-b"}}})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if stream.calls != 2 {
		t.Fatalf("upstream attempts = %d", stream.calls)
	}
	if strings.Contains(body, "event: error") || !strings.Contains(body, "recovered") {
		t.Fatalf("failed candidate was not retried: %q", body)
	}
}

func TestStreamErrorStatusSeparatesContextOverflow(t *testing.T) {
	// A context rejection is an answer, not a proxy fault. Bucketing it with
	// malformed_stream made the failure rate unusable for judging stability.
	overflow := &provider.ProviderError{
		Provider: "codex OAuth", StatusCode: http.StatusRequestEntityTooLarge,
		Code: "context_length_exceeded", Message: "Your input exceeds the context window of this model.",
	}
	if got := streamErrorStatus(fmt.Errorf("read upstream SSE: %w", overflow)); got != "context_overflow" {
		t.Fatalf("status = %q, want context_overflow", got)
	}
	if got := streamErrorStatus(errUpstreamStreamIdle); got != "idle_timeout" {
		t.Fatalf("idle status = %q", got)
	}
	if got := streamErrorStatus(errors.New("garbage frame")); got != "malformed_stream" {
		t.Fatalf("default status = %q", got)
	}
}

func TestContextOverflowMessageIsParseableByClaudeCode(t *testing.T) {
	// Claude Code extracts the token gap with this exact expression and uses it to
	// size its reactive compaction. Keep the wire format matching it.
	pattern := regexp.MustCompile(`(?i)prompt is too long[^0-9]*(\d+)\s*tokens?\s*>\s*(\d+)`)

	client := &errorBodyStreamClient{err: &provider.ProviderError{
		Provider: "codex OAuth", StatusCode: http.StatusRequestEntityTooLarge,
		Code: "context_length_exceeded", Message: "Your input exceeds the context window of this model.",
	}}
	service := New(Options{
		OpenAIStream:  client,
		ContextLimits: contextguard.Limits{Default: 400_000, Models: map[string]int{"cx/gpt-5.6-sol": 400_000}},
	})
	e := echo.New()
	service.Register(e)
	body := `{"model":"cx/gpt-5.6-sol","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"` +
		strings.Repeat("token ", 300_000) + `"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Fatalf("error type = %q", payload.Error.Type)
	}
	match := pattern.FindStringSubmatch(payload.Error.Message)
	if match == nil {
		t.Fatalf("message is not parseable as a token gap: %q", payload.Error.Message)
	}
	actual, limit := match[1], match[2]
	if actual == limit {
		t.Fatalf("actual and limit are equal, gap is zero: %q", payload.Error.Message)
	}
	// 300000, not the catalog's 400000: this rejection named no number, so the window is
	// stepped down before the message is built and the client is told the lowered figure.
	// That is the safe direction — it compacts harder — and it is the only way the belief
	// improves for an upstream whose refusals carry no numbers at all.
	if limit != "300000" {
		t.Fatalf("reported limit = %s, want the stepped-down 300000", limit)
	}
}

func TestAnthropicEmptyTurnIsRetriedNotPaddedToEndTurn(t *testing.T) {
	// Gemini via Antigravity sometimes finishes a turn having produced no text and no
	// tool call. Padding that with a space and reporting end_turn told the agent the
	// task was finished, which is the stall the user has to break with "continue".
	empty := `data: {"id":"one","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	real := `data: {"id":"two","model":"m","choices":[{"index":0,"delta":{"content":"here it is"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	stream := &sequenceStreamClient{responses: []string{empty, real}}
	service := New(Options{OpenAIStream: stream, Aliases: map[string][]string{"alias": {"a", "b"}}})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if stream.calls != 2 {
		t.Fatalf("upstream attempts = %d, want the empty turn to be retried", stream.calls)
	}
	if !strings.Contains(body, "here it is") {
		t.Fatalf("retry result not delivered: %q", body)
	}
	// Exactly one message must reach the client, not one per attempt.
	if got := strings.Count(body, "event: message_start"); got != 1 {
		t.Fatalf("message_start count = %d, want 1: %q", got, body)
	}
}

func TestAnthropicEmptyTurnFallsBackWhenNoCandidateProducesOutput(t *testing.T) {
	// If every candidate comes back empty the client still needs a well-formed
	// message; a truncated body would surface as a transport failure.
	empty := `data: {"id":"one","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	// Enough for both candidates plus the bounded same-candidate replays.
	stream := &sequenceStreamClient{responses: []string{empty, empty, empty, empty, empty, empty}}
	service := New(Options{OpenAIStream: stream, Aliases: map[string][]string{"alias": {"a", "b"}}})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, body)
	}
	for _, want := range []string{"event: message_start", "event: content_block_start",
		"event: message_delta", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("fallback turn is not a well-formed message, missing %s: %q", want, body)
		}
	}
	// Bounded: two candidates plus maxEmptyTurnReplays, never an unbounded loop.
	if stream.calls != 2+maxEmptyTurnReplays {
		t.Fatalf("upstream attempts = %d, want %d", stream.calls, 2+maxEmptyTurnReplays)
	}
	if got := strings.Count(body, "event: message_start"); got != 1 {
		t.Fatalf("message_start count = %d, want 1", got)
	}
}

func TestAnthropicEmptyTurnReplaysSingleCandidate(t *testing.T) {
	// The stalling model has no alias chain, so there is no next candidate to fall
	// through to; without a same-candidate replay the empty turn reached the client
	// immediately and stopped the agent.
	empty := `data: {"id":"one","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	real := `data: {"id":"two","model":"m","choices":[{"index":0,"delta":{"content":"resumed"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	stream := &sequenceStreamClient{responses: []string{empty, real}}
	service := New(Options{OpenAIStream: stream})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gemini-3.1-pro-low","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if stream.calls != 2 {
		t.Fatalf("upstream attempts = %d, want the single candidate replayed once", stream.calls)
	}
	if !strings.Contains(body, "resumed") {
		t.Fatalf("replay result not delivered: %q", body)
	}
	if strings.Contains(body, `"text":" "`) {
		t.Fatalf("empty-turn padding leaked into a recovered turn: %q", body)
	}
}

func TestAnthropicToolCallDroppedWhenNoToolsDeclared(t *testing.T) {
	// Reproduces the /compact empty response: Gemini imitates the functionCall parts
	// in a replayed history and calls a tool even though the compact request offers
	// none. Forwarded, that turn has a tool_use the client cannot run and no text at
	// all, which Claude Code reports as "no summary text in response".
	toolOnly := `data: {"id":"one","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"one","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
	summary := `data: {"id":"two","model":"m","choices":[{"index":0,"delta":{"content":"the summary"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	stream := &sequenceStreamClient{responses: []string{toolOnly, summary}}
	service := New(Options{OpenAIStream: stream})
	e := echo.New()
	service.Register(e)
	// No "tools" key at all — exactly what a compact request looks like.
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gemini-3.1-pro-high","messages":[{"role":"user","content":"summarize the session"}],"max_tokens":1000,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if strings.Contains(body, "tool_use") {
		t.Fatalf("undeclared tool call forwarded to a caller with no tools: %q", body)
	}
	if stream.calls != 2 {
		t.Fatalf("upstream attempts = %d, want the text-free turn retried", stream.calls)
	}
	if !strings.Contains(body, "the summary") {
		t.Fatalf("retry did not deliver the summary: %q", body)
	}
}

func TestAnthropicToolCallKeptWhenToolsDeclared(t *testing.T) {
	toolOnly := `data: {"id":"one","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"one","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
	stream := &sequenceStreamClient{responses: []string{toolOnly}}
	service := New(Options{OpenAIStream: stream})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gemini-3.1-pro-high","messages":[{"role":"user","content":"read a file"}],"tools":[{"name":"Read","input_schema":{"type":"object"}}],"max_tokens":1000,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, `"name":"Read"`) || !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Fatalf("declared tool call was not delivered: %q", body)
	}
	if stream.calls != 1 {
		t.Fatalf("upstream attempts = %d, want 1", stream.calls)
	}
}

func TestTransientUpstreamErrorClassification(t *testing.T) {
	overloaded := &provider.ProviderError{Provider: "codex OAuth", StatusCode: 502,
		Code: "server_is_overloaded", Message: "Our servers are currently overloaded."}
	if !transientUpstreamError(fmt.Errorf("read upstream SSE: %w", overloaded)) {
		t.Fatal("an overloaded 502 must count as transient")
	}
	// A rate limit belongs on another account, not on the same one again.
	rateLimited := &provider.ProviderError{StatusCode: 429, Code: "rate_limit_exceeded"}
	if transientUpstreamError(rateLimited) {
		t.Fatal("429 must not be replayed on the same candidate")
	}
	overflow := &provider.ProviderError{StatusCode: 413, Code: "context_length_exceeded"}
	if transientUpstreamError(overflow) {
		t.Fatal("a context overflow is not transient")
	}
	if transientUpstreamError(errors.New("plain")) {
		t.Fatal("an untyped error must not be treated as transient")
	}
}

func TestAnthropicTransientFailureReplaysSingleCandidate(t *testing.T) {
	// The user's most-used model has no alias chain, so "no candidates left" was true
	// on the first overloaded response and a temporary upstream condition reached the
	// client as a 502.
	overloaded := &provider.ProviderError{Provider: "codex OAuth", StatusCode: 502,
		Code: "server_is_overloaded", Message: "Our servers are currently overloaded."}
	real := `data: {"id":"two","model":"m","choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	client := &errorThenBodyClient{err: overloaded, body: real}
	service := New(Options{OpenAIStream: client})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cx/gpt-5.6-luna","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "recovered") {
		t.Fatalf("replay result not delivered: %q", recorder.Body.String())
	}
	if client.calls != 2 {
		t.Fatalf("upstream attempts = %d, want the single candidate replayed once", client.calls)
	}
}

// openErrorThenBodyClient refuses to open the first stream, then serves body.
type openErrorThenBodyClient struct {
	err   error
	body  string
	calls int
}

func (c *openErrorThenBodyClient) DoStream(_ context.Context, _ string, _ any) (io.ReadCloser, error) {
	c.calls++
	if c.calls == 1 {
		return nil, c.err
	}
	return io.NopCloser(strings.NewReader(c.body)), nil
}

// The same overload, refused a moment earlier. Read from an open stream it is replayed;
// raised while opening the stream it used to fall straight out of the candidate loop,
// because a model with no alias chain is out of candidates on its first fault. Whether a
// temporary upstream condition killed the turn came down to timing.
func TestAnthropicTransientFailureAtOpenReplaysSingleCandidate(t *testing.T) {
	overloaded := &provider.ProviderError{Provider: "codex OAuth", StatusCode: 502,
		Code: "server_is_overloaded", Message: "Our servers are currently overloaded."}
	real := `data: {"id":"two","model":"m","choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	client := &openErrorThenBodyClient{err: overloaded, body: real}
	service := New(Options{OpenAIStream: client})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"cx/gpt-5.6-luna","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "recovered") {
		t.Fatalf("replay result not delivered: %q", recorder.Body.String())
	}
	if client.calls != 2 {
		t.Fatalf("upstream attempts = %d, want the single candidate replayed once", client.calls)
	}
}

// The open path must not replay an oversized prompt either: transient accepts any 5xx,
// and re-sending the payload the upstream just measured cannot change its size.
func TestAnthropicOversizedPromptAtOpenIsNotReplayed(t *testing.T) {
	refused := &provider.ProviderError{Provider: "codex OAuth", StatusCode: 500,
		Code: "server_error", Message: "prompt is too long: 719000 tokens > 256000 maximum"}
	client := &openErrorThenBodyClient{err: refused, body: "data: [DONE]\n\n"}
	service := New(Options{OpenAIStream: client})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"cx/gpt-5.6-luna","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	start := time.Now()
	e.ServeHTTP(recorder, request)

	if client.calls != 1 {
		t.Fatalf("upstream attempts = %d, want the refused prompt sent once", client.calls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("request spent %v, which means it backed off and replayed", elapsed)
	}
}

// alwaysFailingStreamClient fails every read with the same error.
type alwaysFailingStreamClient struct {
	err   error
	calls int
}

func (c *alwaysFailingStreamClient) DoStream(_ context.Context, _ string, _ any) (io.ReadCloser, error) {
	c.calls++
	return io.NopCloser(&failingReader{err: c.err}), nil
}

// An upstream that answers an oversized prompt with a 5xx satisfies both classifiers:
// transientUpstreamError accepts any 5xx, and isContextOverflow reads the message. The
// transient branch would then replay the identical prompt that was just refused — twice,
// with a second and then two seconds of backoff — before the overflow handling that can
// actually fix it ever ran.
func TestAnthropicOversizedPromptIsNotReplayedAsTransient(t *testing.T) {
	refused := &provider.ProviderError{Provider: "codex OAuth", StatusCode: 500,
		Code: "server_error", Message: "prompt is too long: 719000 tokens > 256000 maximum"}
	client := &alwaysFailingStreamClient{err: refused}
	service := New(Options{OpenAIStream: client})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"cx/gpt-5.6-luna","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	start := time.Now()
	e.ServeHTTP(recorder, request)

	if client.calls != 1 {
		t.Fatalf("upstream attempts = %d, want the refused prompt sent once", client.calls)
	}
	// The backoff is the other half of the cost: replaying spends it before giving up.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("request spent %v, which means it backed off and replayed", elapsed)
	}
	if recorder.Code == http.StatusBadGateway {
		t.Fatalf("reported as a transient upstream fault rather than an oversized prompt: %s", recorder.Body.String())
	}
}

// errorThenBodyClient fails the first read with err, then serves body.
type errorThenBodyClient struct {
	err   error
	body  string
	calls int
}

func (c *errorThenBodyClient) DoStream(_ context.Context, _ string, _ any) (io.ReadCloser, error) {
	c.calls++
	if c.calls == 1 {
		return io.NopCloser(&failingReader{err: c.err}), nil
	}
	return io.NopCloser(strings.NewReader(c.body)), nil
}

func TestAnthropicTerminalNeverPrecedesMessageStart(t *testing.T) {
	// A turn whose only tool call arrives with truncated arguments: abort() discards
	// it, leaving nothing to flush, so the terminal used to be the first event out.
	truncated := `data: {"id":"one","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":"}}]},"finish_reason":null}]}` + "\n\n"
	stream := &fakeStreamClient{body: io.NopCloser(strings.NewReader(truncated))}
	service := New(Options{OpenAIStream: stream})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read","input_schema":{"type":"object"}}],"max_tokens":100,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if body == "" {
		t.Fatal("no response written")
	}
	startIndex := strings.Index(body, "event: message_start")
	deltaIndex := strings.Index(body, "event: message_delta")
	if startIndex < 0 {
		t.Fatalf("message_start missing entirely: %q", body)
	}
	if deltaIndex >= 0 && deltaIndex < startIndex {
		t.Fatalf("message_delta was emitted before message_start: %q", body)
	}
}

func TestTransientUpstreamErrorCoversTransportResets(t *testing.T) {
	// A real failure seen in production: HTTP/2 peer reset with no typed status. Keying
	// only on ProviderError meant a dropped connection became a hard 502.
	reset := errors.New("read upstream SSE: stream error: stream ID 1167; INTERNAL_ERROR; received from peer")
	if !transientUpstreamError(reset) {
		t.Fatal("an HTTP/2 peer reset must be replayable")
	}
	for _, err := range []error{
		io.ErrUnexpectedEOF,
		errors.New("read tcp 1.2.3.4:443: connection reset by peer"),
		errors.New("http2: stream closed"),
	} {
		if !transientUpstreamError(err) {
			t.Fatalf("transport failure not recognised: %v", err)
		}
	}
	// Things that must still not be replayed on the same candidate.
	if transientUpstreamError(&provider.ProviderError{StatusCode: 429, Code: "rate_limit_exceeded"}) {
		t.Fatal("a rate limit must go to another account")
	}
	if transientUpstreamError(&provider.ProviderError{StatusCode: 413, Code: "context_length_exceeded"}) {
		t.Fatal("a context overflow is deterministic")
	}
	if transientUpstreamError(errors.New("tool arguments failed schema validation")) {
		t.Fatal("a translator-level fault must not be masked by a retry")
	}
	if transientUpstreamError(nil) {
		t.Fatal("nil is not a transient failure")
	}
}

// overflowThenServeStreamClient refuses the first N attempts the way an upstream past
// its context window does, naming the limit, then serves normally.
type overflowThenServeStreamClient struct {
	refusals int
	calls    int
	sizes    []int
}

func (c *overflowThenServeStreamClient) DoStream(_ context.Context, _ string, body any) (io.ReadCloser, error) {
	c.calls++
	encoded, _ := json.Marshal(body)
	c.sizes = append(c.sizes, len(encoded))
	if c.calls <= c.refusals {
		return nil, &provider.ProviderError{
			Provider: "codex OAuth", StatusCode: http.StatusBadRequest, Code: "context_length_exceeded",
			Message: "prompt is too long: 513800 tokens > 400000 tokens",
		}
	}
	return io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n")), nil
}

// The incident this exists for: a conversation grows past the model's real window, the
// client cannot compact its way out because a compaction request costs roughly twice the
// conversation, and the turn used to die with context_overflow. It must now be salvaged.
func TestMessagesOverWindowTurnIsTrimmedAndServed(t *testing.T) {
	client := &overflowThenServeStreamClient{refusals: 1}
	service := New(Options{
		OpenAIStream:  client,
		ContextGuard:  true,
		ContextPolicy: contextguard.DefaultPolicy(),
		ContextWindow: func(context.Context, string) (int, error) { return 1_000_000, nil },
	})
	e := echo.New()
	service.Register(e)

	// Many turns, so there is older history available to drop.
	turn := strings.Repeat("x", 20_000)
	var messages []string
	for index := 0; index < 40; index++ {
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, fmt.Sprintf(`{"role":%q,"content":%q}`, role, turn))
	}
	payload := fmt.Sprintf(`{"model":"cx/gpt-5.6-sol","messages":[%s],"max_tokens":1024,"stream":true}`,
		strings.Join(messages, ","))
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "message_stop") {
		t.Fatalf("turn did not complete: %s", recorder.Body.String())
	}
	if client.calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (refused once, then served the trimmed turn)", client.calls)
	}
	if client.sizes[1] >= client.sizes[0] {
		t.Fatalf("retry was not smaller: %d then %d", client.sizes[0], client.sizes[1])
	}
	// The window the rejection named must survive the turn, so later turns are budgeted
	// and reported against 400k rather than the 1M the catalog claimed.
	if window := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-sol"); window != 400_000 {
		t.Fatalf("learned window = %d, want 400000", window)
	}
}

// Trimming is bounded. An upstream that refuses everything must still produce the
// compactable 400 rather than looping.
func TestMessagesRelentlessOverflowStopsAndReports(t *testing.T) {
	client := &overflowThenServeStreamClient{refusals: 99}
	service := New(Options{
		OpenAIStream:  client,
		ContextGuard:  true,
		ContextPolicy: contextguard.DefaultPolicy(),
		ContextWindow: func(context.Context, string) (int, error) { return 400_000, nil },
	})
	e := echo.New()
	service.Register(e)

	turn := strings.Repeat("y", 20_000)
	var messages []string
	for index := 0; index < 40; index++ {
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, fmt.Sprintf(`{"role":%q,"content":%q}`, role, turn))
	}
	payload := fmt.Sprintf(`{"model":"cx/gpt-5.6-sol","messages":[%s],"max_tokens":1024,"stream":true}`,
		strings.Join(messages, ","))
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"invalid_request_error"`) || !strings.Contains(body, "prompt is too long") {
		t.Fatalf("body is not the shape the client compacts on: %s", body)
	}
	if client.calls > 1+maxContextTrimRetries {
		t.Fatalf("upstream calls = %d, want at most %d", client.calls, 1+maxContextTrimRetries)
	}
}
