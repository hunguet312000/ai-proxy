package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
	"literouter/internal/translator"
)

// recordingStream keeps every request it was handed, and fails the named model with a
// retryable error so the chain advances to the next candidate.
type recordingStream struct {
	failModel string
	content   string
	sent      []translator.OpenAIRequest
}

func (r *recordingStream) DoStream(_ context.Context, _ string, request any) (io.ReadCloser, error) {
	sent := request.(translator.OpenAIRequest)
	r.sent = append(r.sent, sent)
	if sent.Model == r.failModel {
		return nil, &provider.ProviderError{Provider: "test", StatusCode: 503, Message: "overloaded"}
	}
	return io.NopCloser(strings.NewReader(r.content)), nil
}

// A turn that fits the model it was aimed at must still be compressed when the chain hands
// it to a model with a smaller window — the case where a 1M-context model swaps to a 256k
// one mid-session. The window is resolved per candidate and the candidate is rebuilt from
// the original request, so the second attempt is compressed against 256k rather than
// forwarded at the size the first model could take.
func TestChainRecompressesForASmallerWindowCandidate(t *testing.T) {
	const big, small = "big-window-model", "small-window-model"
	stream := &recordingStream{failModel: big, content: strings.Join([]string{
		`data: {"id":"one","model":"` + small + `","choices":[{"index":0,"delta":{"content":"ok"}}]}`, "",
		`data: {"id":"one","model":"` + small + `","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "",
		"data: [DONE]", "",
	}, "\n")}
	service := New(Options{
		OpenAIStream:   stream,
		ContextEnabled: true,
		Aliases:        map[string][]string{"swap": {big, small}},
		ContextLimits: contextguard.Limits{
			Default: 1_000_000,
			Models:  map[string]int{big: 1_000_000, small: 60_000},
		},
		ContextPolicy: contextguard.Policy{
			SoftRatio: 0.15, SummarizeRatio: 0.98, HardRatio: 0.99, KeepRecentTurns: 1,
		},
	})
	e := echo.New()
	service.Register(e)

	// Comfortable for the 1M model, far too large for the 60k one.
	filler := strings.Repeat("bối cảnh lịch sử của phiên làm việc ", 2400)
	body := `{"model":"swap","max_tokens":64,"stream":true,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"` + filler + `"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"` + filler + `"}]},` +
		`{"role":"user","content":[{"type":"text","text":"lượt gần nhất"}]}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if len(stream.sent) != 2 {
		t.Fatalf("attempts = %d, want 2 (the big-window model then the small one)", len(stream.sent))
	}

	sizeOf := func(sent translator.OpenAIRequest) int {
		unified, err := translator.FromOpenAIRequest(sent)
		if err != nil {
			t.Fatal(err)
		}
		return contextguard.EstimateRequest(unified)
	}
	first, second := sizeOf(stream.sent[0]), sizeOf(stream.sent[1])
	if stream.sent[0].Model != big || stream.sent[1].Model != small {
		t.Fatalf("attempt models = %q then %q", stream.sent[0].Model, stream.sent[1].Model)
	}
	// The point of the test: the same turn is smaller on the way to the smaller model.
	if second >= first {
		t.Fatalf("small-window attempt was %d tokens against %d for the big-window one; it was not recompressed", second, first)
	}
	if second > 60_000 {
		t.Fatalf("small-window attempt was %d tokens, over that model's 60,000 window", second)
	}
}

// The reverse direction, and the one the fallback made reachable: a turn too large for the
// candidate it was aimed at must still reach a later candidate with a bigger window instead
// of failing the whole chain. Live shape — a ~35k custom-provider model in front of the
// router's 370k fallback.
func TestChainAdvancesToABiggerWindowInsteadOfFailing(t *testing.T) {
	const small, big = "small-window-model", "big-window-model"
	stream := &recordingStream{content: strings.Join([]string{
		`data: {"id":"one","model":"` + big + `","choices":[{"index":0,"delta":{"content":"ok"}}]}`, "",
		`data: {"id":"one","model":"` + big + `","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "",
		"data: [DONE]", "",
	}, "\n")}
	service := New(Options{
		OpenAIStream:   stream,
		ContextEnabled: true,
		Aliases:        map[string][]string{"swap": {small, big}},
		ContextLimits: contextguard.Limits{
			Default: 200_000,
			// Small enough that even trimming to one turn cannot fit it.
			Models: map[string]int{small: 17_000, big: 200_000},
		},
		ContextPolicy: contextguard.Policy{
			SoftRatio: 0.15, SummarizeRatio: 0.98, HardRatio: 0.99, KeepRecentTurns: 1,
		},
	})
	e := echo.New()
	service.Register(e)

	// One indivisible turn far larger than the small model's window.
	filler := strings.Repeat("một lượt duy nhất không thể chia nhỏ ", 3000)
	body := `{"model":"swap","max_tokens":64,"stream":true,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"` + filler + `"}]}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q — the chain gave up instead of trying the bigger model", recorder.Code, recorder.Body.String())
	}
	if len(stream.sent) != 1 || stream.sent[0].Model != big {
		models := make([]string, 0, len(stream.sent))
		for _, sent := range stream.sent {
			models = append(models, sent.Model)
		}
		t.Fatalf("upstream attempts = %v, want just the big-window model", models)
	}
}
