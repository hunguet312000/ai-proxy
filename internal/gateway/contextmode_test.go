package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
	"literouter/internal/translator"
)

func TestContextModeSeedingAndRuntimeFlip(t *testing.T) {
	off := New(Options{})
	if got := off.ContextMode(); got != ContextModeOff {
		t.Fatalf("disabled boot mode = %q", got)
	}
	safe := New(Options{ContextEnabled: true})
	if got := safe.ContextMode(); got != ContextModeSafe {
		t.Fatalf("enabled default mode = %q", got)
	}
	aggressive := New(Options{ContextEnabled: true, ContextMode: "Aggressive"})
	if got := aggressive.ContextMode(); got != ContextModeAggressive {
		t.Fatalf("aggressive boot mode = %q", got)
	}
	// Mode set while enabled=false must not switch the pipeline on behind the
	// master flag's back.
	contradicted := New(Options{ContextEnabled: false, ContextMode: "aggressive"})
	if got := contradicted.ContextMode(); got != ContextModeOff {
		t.Fatalf("mode overrode enabled=false: %q", got)
	}

	if err := off.SetContextMode("aggressive"); err != nil || !off.contextPrepEnabled() {
		t.Fatalf("runtime flip failed: %v", err)
	}
	if !off.activeContextPolicy().Aggressive {
		t.Fatal("aggressive mode did not activate the aggressive policy")
	}
	if err := off.SetContextMode("bogus"); err == nil {
		t.Fatal("invalid mode accepted")
	}
	if err := off.SetContextMode("off"); err != nil || off.contextPrepEnabled() {
		t.Fatalf("runtime off failed: %v", err)
	}
}

func TestResolveSummaryModelChain(t *testing.T) {
	service := New(Options{})
	if got := service.resolveSummaryModel("session"); got != "session" {
		t.Fatalf("bare fallback = %q", got)
	}
	service.SetCompactModel("fast")
	if got := service.resolveSummaryModel("session"); got != "fast" {
		t.Fatalf("compact fallback = %q", got)
	}
	explicit := New(Options{SummaryModel: "dedicated", CompactModel: "fast"})
	if got := explicit.resolveSummaryModel("session"); got != "dedicated" {
		t.Fatalf("explicit summarizer = %q", got)
	}
}

// overflowOnceInference rejects the first attempt as an oversized prompt and
// accepts whatever comes after, recording the message counts it saw.
type overflowOnceInference struct {
	calls         int
	messageCounts []int
}

func (f *overflowOnceInference) DoJSON(_ context.Context, request translator.OpenAIRequest, _ string) (translator.OpenAIResponse, error) {
	f.calls++
	f.messageCounts = append(f.messageCounts, len(request.Messages))
	if f.calls == 1 {
		return translator.OpenAIResponse{}, &provider.ProviderError{
			Provider: "test", StatusCode: http.StatusBadRequest,
			Message: "prompt is too long: 210000 tokens > 200000 maximum",
		}
	}
	return translator.OpenAIResponse{
		Model: "model",
		Choices: []translator.OpenAIChoice{{
			Message: translator.OpenAIMessage{Role: "assistant", Content: "recovered"}, FinishReason: "stop",
		}},
	}, nil
}

func (f *overflowOnceInference) DoStream(context.Context, translator.OpenAIRequest, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

func (f *overflowOnceInference) SupportsAnthropicPassthrough(string) bool { return false }

func (f *overflowOnceInference) DoAnthropicStream(context.Context, []byte, string, string, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

func TestCompleteRetriesAfterContextRejection(t *testing.T) {
	oauth := &overflowOnceInference{}
	service := New(Options{OAuthInference: oauth})
	var messages []provider.Message
	for index := range 12 {
		messages = append(messages,
			provider.Message{Role: "user", Content: []provider.Content{{Type: "text", Text: fmt.Sprintf("question %d %s", index, strings.Repeat("filler ", 400))}}},
			provider.Message{Role: "assistant", Content: []provider.Content{{Type: "text", Text: "answer"}}},
		)
	}
	response, err := service.complete(context.Background(), provider.Request{Model: "model", Messages: messages, MaxTokens: 100})
	if err != nil {
		t.Fatalf("complete() = %v", err)
	}
	if len(response.Content) == 0 || response.Content[0].Text != "recovered" {
		t.Fatalf("response = %#v", response)
	}
	if oauth.calls != 2 {
		t.Fatalf("calls = %d, want 2", oauth.calls)
	}
	if oauth.messageCounts[1] >= oauth.messageCounts[0] {
		t.Fatalf("retry did not shrink the history: %v", oauth.messageCounts)
	}
}

func TestObserveTokenScaleEstimateOnlySample(t *testing.T) {
	service := New(Options{})
	// Estimate-only sample: no raw bytes in hand. Must still teach estimatePerToken.
	service.observeTokenScale("model", 0, 18_000, 10_000)
	scale := service.tokenScaleFor("model")
	if scale.estimatePerToken != 1.8 {
		t.Fatalf("estimatePerToken = %v, want 1.8", scale.estimatePerToken)
	}
	if scale.bytesPerToken != fallbackBytesPerToken {
		t.Fatalf("bytesPerToken = %v, want the fallback", scale.bytesPerToken)
	}
	// A later full sample must not have its bytes ratio poisoned by the zero.
	service.observeTokenScale("model", 80_000, 18_000, 10_000)
	scale = service.tokenScaleFor("model")
	if scale.bytesPerToken <= fallbackBytesPerToken {
		t.Fatalf("bytesPerToken = %v, want blended above the fallback", scale.bytesPerToken)
	}
	// An estimate-only sample on a known model keeps the learned bytes ratio.
	learned := scale.bytesPerToken
	service.observeTokenScale("model", 0, 17_000, 10_000)
	if got := service.tokenScaleFor("model").bytesPerToken; got != learned {
		t.Fatalf("bytesPerToken moved on an estimate-only sample: %v -> %v", learned, got)
	}
}

func TestRequestContextPolicyClampsLearnedScale(t *testing.T) {
	service := New(Options{ContextEnabled: true})
	// Nothing learned: fallback 1.0 stays inside the clamp.
	if got := service.requestContextPolicy("model").EstimateScale; got != 1.0 {
		t.Fatalf("default scale = %v", got)
	}
	// An extreme learned ratio is clamped before it reaches budget math.
	service.observeTokenScale("model", 0, 40_000, 10_000) // 4.0, beyond the 1.5 cap
	if got := service.requestContextPolicy("model").EstimateScale; got != 1.5 {
		t.Fatalf("clamped scale = %v, want 1.5", got)
	}
	policy := service.requestContextPolicy("model")
	if err := policy.Validate(); err != nil {
		t.Fatalf("request policy invalid: %v", err)
	}
}

func TestPrepareContextUsesAggressiveTruncationWhenEnabled(t *testing.T) {
	service := New(Options{ContextEnabled: true, ContextMode: "aggressive",
		ContextLimits: contextguard.Limits{Default: 30_000}})
	var messages []provider.Message
	for index := range 10 {
		messages = append(messages, provider.Message{Role: "assistant", Content: []provider.Content{
			{Type: "tool_use", ToolUseID: fmt.Sprintf("call-%d", index), Name: "Bash", Input: []byte(fmt.Sprintf(`{"command":"run %d"}`, index))},
		}}, provider.Message{Role: "user", Content: []provider.Content{
			{Type: "tool_result", ToolUseID: fmt.Sprintf("call-%d", index), Text: strings.Repeat(fmt.Sprintf("output %d line\n", index), 700)},
		}})
	}
	messages = append(messages, provider.Message{Role: "user", Content: []provider.Content{{Type: "text", Text: "latest instruction"}}})
	request := provider.Request{Model: "model", Messages: messages, MaxTokens: 100}

	prepared, err := service.prepareContext(context.Background(), request)
	if err != nil {
		t.Fatalf("prepareContext() = %v", err)
	}
	var truncated int
	for _, message := range prepared.Messages {
		for _, block := range message.Content {
			if strings.HasPrefix(block.Text, "[literouter:truncate-v1") {
				truncated++
			}
		}
	}
	if truncated == 0 {
		t.Fatal("aggressive mode truncated nothing on an over-budget request")
	}
	if last := prepared.Messages[len(prepared.Messages)-1].Content[0].Text; last != "latest instruction" {
		t.Fatalf("latest instruction changed: %q", last)
	}
}
