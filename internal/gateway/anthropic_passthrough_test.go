package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"literouter/internal/translator"
)

type fakePassthroughInference struct {
	responses []string
	errs      []error
	calls     int
	payloads  [][]byte
	betas     []string
}

func (f *fakePassthroughInference) DoJSON(context.Context, translator.OpenAIRequest, string) (translator.OpenAIResponse, error) {
	return translator.OpenAIResponse{}, errors.New("unused")
}

func (f *fakePassthroughInference) DoStream(context.Context, translator.OpenAIRequest, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

func (f *fakePassthroughInference) SupportsAnthropicPassthrough(model string) bool {
	return strings.HasPrefix(model, "claude")
}

func (f *fakePassthroughInference) DoAnthropicStream(_ context.Context, payload []byte, _, _, betas string) (io.ReadCloser, error) {
	index := f.calls
	f.calls++
	f.payloads = append(f.payloads, payload)
	f.betas = append(f.betas, betas)
	if index < len(f.errs) && f.errs[index] != nil {
		return nil, f.errs[index]
	}
	if index >= len(f.responses) {
		return nil, errors.New("no passthrough candidates left")
	}
	return io.NopCloser(strings.NewReader(f.responses[index])), nil
}

const anthropicUpstreamStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4-5","usage":{"input_tokens":1200,"cache_read_input_tokens":900,"cache_creation_input_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

const anthropicClientBody = `{"model":"claude-opus-4-5","max_tokens":100,` +
	`"system":[{"type":"text","text":"you are claude code","cache_control":{"type":"ephemeral"}}],` +
	`"metadata":{"user_id":"abc"},` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`

func postAnthropic(t *testing.T, service *Service, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-beta", "claude-code-20250219")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	return recorder
}

func TestAnthropicPassthroughRelaysUpstreamVerbatim(t *testing.T) {
	oauth := &fakePassthroughInference{responses: []string{anthropicUpstreamStream}}
	usageEvents := make(chan UsageEvent, 1)
	service := New(Options{OAuthInference: oauth, ContextEnabled: true, OnUsage: func(event UsageEvent) { usageEvents <- event }})
	recorder := postAnthropic(t, service, anthropicClientBody)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", recorder.Code, body)
	}
	// The client must receive the upstream's own frames. Anything re-synthesised
	// here is a chance to lose a field the caller depends on.
	for _, expected := range []string{
		"event: message_start", "event: content_block_delta", "event: message_stop",
		`"stop_reason":"end_turn"`, `"cache_read_input_tokens":900`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing %q in relayed stream: %q", expected, body)
		}
	}
	// Prompt-cache breakpoints and unmodelled fields only survive byte passthrough.
	sent := string(oauth.payloads[0])
	for _, expected := range []string{`"cache_control"`, `"ephemeral"`, `"metadata"`, `"user_id"`} {
		if !strings.Contains(sent, expected) {
			t.Fatalf("payload lost %q: %s", expected, sent)
		}
	}
	if !strings.Contains(oauth.betas[0], "oauth-2025-04-20") && !strings.Contains(oauth.betas[0], "claude-code-20250219") {
		t.Fatalf("client betas were not forwarded: %q", oauth.betas[0])
	}
	select {
	case event := <-usageEvents:
		if event.Provider != "claude" || event.PromptTokens != 1200 || event.CompletionTokens != 7 || event.CachedTokens != 900 {
			t.Fatalf("usage event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("passthrough usage event was not recorded")
	}
}

func TestAnthropicPassthroughTruncationClosesCleanly(t *testing.T) {
	truncated := strings.Split(anthropicUpstreamStream, "event: content_block_stop")[0]
	oauth := &fakePassthroughInference{responses: []string{truncated}}
	service := New(Options{OAuthInference: oauth, ContextEnabled: true})
	recorder := postAnthropic(t, service, anthropicClientBody)

	body := recorder.Body.String()
	if strings.Contains(body, "event: error") {
		t.Fatalf("truncated passthrough emitted an error frame: %q", body)
	}
	for _, expected := range []string{"event: content_block_stop", `"stop_reason":"max_tokens"`, "event: message_stop"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing %q in truncated close: %q", expected, body)
		}
	}
}

func TestAnthropicPassthroughFallsBackToTranslation(t *testing.T) {
	// No Anthropic-native account can serve the turn, but nothing has been written
	// yet, so the translated path must still get its chance.
	oauth := &fakePassthroughInference{errs: []error{errors.New("no claude account")}}
	stream := &fakeStreamClient{content: strings.Join([]string{
		`data: {"id":"one","model":"claude-opus-4-5","choices":[{"index":0,"delta":{"content":"translated"},"finish_reason":null}]}`,
		"", `data: {"id":"one","model":"claude-opus-4-5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"", "data: [DONE]", "",
	}, "\n")}
	service := New(Options{OAuthInference: oauth, OpenAIStream: stream})
	recorder := postAnthropic(t, service, anthropicClientBody)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "translated") {
		t.Fatalf("translation fallback did not run: %d %q", recorder.Code, body)
	}
	if oauth.calls != 1 {
		t.Fatalf("passthrough attempts = %d", oauth.calls)
	}
}

func TestAnthropicPassthroughSkippedForNonClaudeModels(t *testing.T) {
	// A Codex-only deployment must not pay for the passthrough attempt at all:
	// no upstream call, and no rebuilding of the request payload.
	oauth := &fakePassthroughInference{}
	stream := &fakeStreamClient{content: strings.Join([]string{
		`data: {"id":"one","model":"cx/gpt-5.6-sol","choices":[{"index":0,"delta":{"content":"codex"},"finish_reason":null}]}`,
		"", `data: {"id":"one","model":"cx/gpt-5.6-sol","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"", "data: [DONE]", "",
	}, "\n")}
	service := New(Options{OAuthInference: oauth, OpenAIStream: stream})
	body := `{"model":"cx/gpt-5.6-sol","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`
	recorder := postAnthropic(t, service, body)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "codex") {
		t.Fatalf("codex turn did not complete: %d %q", recorder.Code, recorder.Body.String())
	}
	if oauth.calls != 0 || len(oauth.payloads) != 0 {
		t.Fatalf("passthrough touched a non-Claude turn: calls=%d payloads=%d", oauth.calls, len(oauth.payloads))
	}
}
