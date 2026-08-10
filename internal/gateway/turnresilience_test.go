package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"literouter/internal/contextguard"
	"literouter/internal/translator"
)

// The context guard defers every real decision to the upstream tokenizer — all it can do
// is log. It must not be able to end a turn, and the window lookup it depends on is a
// catalog read that can fail transiently. Returning that error surfaced as a 502 on every
// turn for as long as the read kept failing, even though the lookup still hands back a
// serviceable window from configuration.
//
// This is the guard-only path: enabled=false with guard_enabled=true, which is what
// config.example.yaml ships.
func TestWindowLookupFailureDoesNotEndTheTurn(t *testing.T) {
	service := New(Options{
		OpenAI:        &fakeClient{},
		ContextGuard:  true,
		ContextLimits: contextguard.Limits{Default: 128_000},
		ContextPolicy: contextguard.DefaultPolicy(),
		ContextWindow: func(context.Context, string) (int, error) {
			return 0, errors.New("database is locked")
		},
	})

	_, err := service.Chat(context.Background(), translator.OpenAIRequest{
		Model:    "gpt-4.1",
		Messages: []translator.OpenAIMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("a transient catalog read must not end the turn: %v", err)
	}
}

// A conversation too large for the body cap is an overflow, not a malformed request. The
// client can only act on the former: told its body was invalid it resends the same payload
// and the agent is stuck, because nothing downstream can shrink a payload the gateway
// refused to read.
func TestOversizedBodyIsReportedAsAnOverflow(t *testing.T) {
	service := New(Options{OpenAI: &fakeClient{}})
	e := echo.New()
	service.Register(e)

	// One byte past the cap, with a valid prefix so only the size can be the objection.
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"` +
		strings.Repeat("x", maxRequestBody) + `"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "prompt is too long") {
		t.Fatalf("body = %s; the client keys its compaction off this phrasing", recorder.Body.String())
	}
}
