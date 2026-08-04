package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

func TestResponsesHandlerNonStream(t *testing.T) {
	client := &fakeClient{}
	usageEvents := make(chan UsageEvent, 1)
	service := New(Options{OpenAI: client, OnUsage: func(event UsageEvent) { usageEvents <- event }})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"object":"response"`) || !strings.Contains(recorder.Body.String(), `"output_text":"done"`) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
	select {
	case event := <-usageEvents:
		if event.Endpoint != "/v1/responses" || event.Model != "model" {
			t.Fatalf("usage event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("usage event was not recorded")
	}
}

func TestResponsesHandlerStreaming(t *testing.T) {
	stream := &fakeStreamClient{content: strings.Join([]string{
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		"",
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
		"",
		"data: [DONE]", "",
	}, "\n")}
	usageEvents := make(chan UsageEvent, 1)
	service := New(Options{OpenAIStream: stream, OnUsage: func(event UsageEvent) { usageEvents <- event }})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"hello","stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, event := range []string{"event: response.created", "event: response.output_text.delta", "event: response.completed"} {
		if !strings.Contains(body, event) {
			t.Fatalf("missing %q in %s", event, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("Responses stream contains chat terminator: %s", body)
	}
	select {
	case event := <-usageEvents:
		if event.Endpoint != "/v1/responses" || event.Model != "model" || event.PromptTokens != 3 || event.CompletionTokens != 1 {
			t.Fatalf("usage event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("usage event was not recorded")
	}
}

func TestResponsesHandlerStreamingEstimatesMissingUsage(t *testing.T) {
	stream := &fakeStreamClient{content: "data: {\"id\":\"one\",\"model\":\"model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"}
	usageEvents := make(chan UsageEvent, 1)
	service := New(Options{OpenAIStream: stream, OnUsage: func(event UsageEvent) { usageEvents <- event }})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"hello","stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	select {
	case event := <-usageEvents:
		if event.Endpoint != "/v1/responses" || event.PromptTokens <= 0 || event.CompletionTokens <= 0 {
			t.Fatalf("estimated usage event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("estimated usage event was not recorded")
	}
}

func TestResponsesUnsupportedAndInvalid(t *testing.T) {
	service := New(Options{})
	e := echo.New()
	service.Register(e)
	compact := httptest.NewRecorder()
	e.ServeHTTP(compact, httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{}`)))
	if compact.Code != http.StatusNotImplemented {
		t.Fatalf("compact status = %d", compact.Code)
	}
	invalid := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"hi","previous_response_id":"resp"}`))
	request.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(invalid, request)
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "previous_response_id") {
		t.Fatalf("invalid = %d %s", invalid.Code, invalid.Body.String())
	}
}

func TestResponsesStreamToolArguments(t *testing.T) {
	stream := &fakeStreamClient{content: "data: {\"id\":\"one\",\"model\":\"model\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"}
	service := New(Options{OpenAIStream: stream})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"hi","stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Body.String(), "response.function_call_arguments.delta") || !strings.Contains(recorder.Body.String(), `"call_id":"call"`) {
		t.Fatalf("stream = %s", recorder.Body.String())
	}
}
