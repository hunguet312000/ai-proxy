package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"literouter/internal/pool"
	"literouter/internal/translator"
)

// The chain the five call sites get, with and without the fallback configured. The
// requested model must stay first: a fallback that jumped the queue would silently serve
// every turn on the wrong model.
func TestModelChainAppendsTheFallbackLast(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		aliases  map[string][]string
		fallback string
		model    string
		want     []string
	}{
		{
			name: "off by default", model: "claude-opus-5",
			want: []string{"claude-opus-5"},
		},
		{
			name: "appended after the requested model", model: "claude-opus-5",
			fallback: "cx/gpt-5.6-luna",
			want:     []string{"claude-opus-5", "cx/gpt-5.6-luna"},
		},
		{
			name: "appended after an alias chain", model: "fast",
			aliases:  map[string][]string{"fast": {"a", "b"}},
			fallback: "cx/gpt-5.6-luna",
			want:     []string{"a", "b", "cx/gpt-5.6-luna"},
		},
		{
			// Spending a second attempt to prove the same model unservable is pure latency.
			name: "not duplicated when already reachable", model: "cx/gpt-5.6-luna",
			fallback: "cx/gpt-5.6-luna",
			want:     []string{"cx/gpt-5.6-luna"},
		},
		{
			name: "not duplicated when the alias chain already has it", model: "fast",
			aliases:  map[string][]string{"fast": {"a", "cx/gpt-5.6-luna"}},
			fallback: "cx/gpt-5.6-luna",
			want:     []string{"a", "cx/gpt-5.6-luna"},
		},
		{
			name: "matched case-insensitively", model: "CX/GPT-5.6-Luna",
			fallback: "cx/gpt-5.6-luna",
			want:     []string{"CX/GPT-5.6-Luna"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := New(Options{Aliases: testCase.aliases, FallbackModel: testCase.fallback})
			if got := service.modelChain(testCase.model); !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("modelChain(%q) = %v, want %v", testCase.model, got, testCase.want)
			}
		})
	}
}

// The alias chain is copied before the fallback is appended. Appending into the map's own
// slice would leak the fallback into the stored alias list, so the second request for the
// same alias would carry it twice — and keep growing.
func TestModelChainDoesNotMutateTheAliasTable(t *testing.T) {
	aliases := map[string][]string{"fast": {"a", "b"}}
	service := New(Options{Aliases: aliases, FallbackModel: "cx/gpt-5.6-luna"})
	first := service.modelChain("fast")
	second := service.modelChain("fast")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("chain changed between calls: %v then %v", first, second)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(aliases["fast"], want) {
		t.Fatalf("alias table mutated to %v, want %v", aliases["fast"], want)
	}
}

// Live reconfiguration, the same contract the plan and compact overrides have.
func TestSetFallbackModelAppliesWithoutRestart(t *testing.T) {
	service := New(Options{})
	if got := service.FallbackModel(); got != "" {
		t.Fatalf("FallbackModel() = %q, want empty", got)
	}
	service.SetFallbackModel("  cx/gpt-5.6-luna  ")
	if got := service.FallbackModel(); got != "cx/gpt-5.6-luna" {
		t.Fatalf("FallbackModel() = %q, want the trimmed value", got)
	}
	if got := service.modelChain("claude-opus-5"); len(got) != 2 {
		t.Fatalf("modelChain did not pick up the new fallback: %v", got)
	}
	service.SetFallbackModel("")
	if got := service.modelChain("claude-opus-5"); len(got) != 1 {
		t.Fatalf("clearing the fallback left it in the chain: %v", got)
	}
}

// A fallback that serves the turn must also be the one the usage row is filed under.
//
// It was not: Provider came from the id the client asked for, which was correct only while
// the requested and serving models always belonged to the same upstream. The fallback broke
// that assumption on its first use — a turn asked for as claude-opus-5 and answered by
// gpt-5.6-luna showed up in the dashboard as "Claude / GPT 5.6 Luna", crediting requests,
// tokens and cost to an upstream that was never called.
//
// Shaped like the real deployment: the refusal comes from the OAuth pool, which is what
// "no eligible account" is, and there is no stream client to fall through to — that
// combination is what lets the chain advance to the next candidate.
func TestUsageIsFiledUnderTheProviderThatServed(t *testing.T) {
	events := make(chan UsageEvent, 4)
	oauth := &refusingOAuth{refuse: "claude-opus-5", content: strings.Join([]string{
		`data: {"id":"one","model":"gpt-5.6-luna","choices":[{"index":0,"delta":{"content":"hi"}}]}`, "",
		`data: {"id":"one","model":"gpt-5.6-luna","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":1}}`, "",
		"data: [DONE]", "",
	}, "\n")}
	service := New(Options{
		OAuthInference: oauth,
		FallbackModel:  "gpt-5.6-luna",
		OnUsage:        func(event UsageEvent) { events <- event },
	})
	e := echo.New()
	service.Register(e)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":16,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if oauth.refused == 0 {
		t.Fatal("the requested model was never attempted, so the fallback was not exercised")
	}

	select {
	case event := <-events:
		if event.Provider == "claude" {
			t.Fatalf("usage filed under the requested model's provider: %#v", event)
		}
		if event.Provider != "codex" {
			t.Fatalf("Provider = %q, want the upstream that actually served", event.Provider)
		}
		// And the requested id is still recorded, so the handover stays traceable.
		if event.RequestModel != "claude-opus-5" {
			t.Fatalf("RequestModel = %q, want the id the client asked for", event.RequestModel)
		}
	case <-time.After(time.Second):
		t.Fatal("no usage event recorded")
	}
}

// Refuses one model the way a provider with no enabled account does, and serves the rest.
type refusingOAuth struct {
	refuse  string
	content string
	refused int
}

func (r *refusingOAuth) DoStream(_ context.Context, request translator.OpenAIRequest, _ string) (io.ReadCloser, error) {
	if request.Model == r.refuse {
		r.refused++
		return nil, pool.ErrNoAccount
	}
	return io.NopCloser(strings.NewReader(r.content)), nil
}

func (r *refusingOAuth) DoJSON(_ context.Context, request translator.OpenAIRequest, _ string) (translator.OpenAIResponse, error) {
	if request.Model == r.refuse {
		r.refused++
		return translator.OpenAIResponse{}, pool.ErrNoAccount
	}
	return translator.OpenAIResponse{}, pool.ErrNoAccount
}

func (r *refusingOAuth) SupportsAnthropicPassthrough(string) bool { return false }

func (r *refusingOAuth) DoAnthropicStream(context.Context, []byte, string, string, string) (io.ReadCloser, error) {
	return nil, pool.ErrNoAccount
}
