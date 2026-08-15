package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"literouter/internal/cache"
	"literouter/internal/contextguard"
	"literouter/internal/provider"
	"literouter/internal/translator"
)

type fakeSummarizer struct {
	calls int
	input contextguard.SummaryInput
	text  string
	err   error
}

func (fake *fakeSummarizer) Summarize(_ context.Context, input contextguard.SummaryInput) (string, error) {
	fake.calls++
	fake.input = input
	return fake.text, fake.err
}

type fakeClient struct {
	calls int
	last  translator.OpenAIRequest
	err   error
}

type fakeOAuthInference struct {
	calls int
	last  translator.OpenAIRequest
	err   error
}

func (fake *fakeOAuthInference) DoJSON(_ context.Context, request translator.OpenAIRequest, _ string) (translator.OpenAIResponse, error) {
	fake.calls++
	fake.last = request
	if fake.err != nil {
		return translator.OpenAIResponse{}, fake.err
	}
	return translator.OpenAIResponse{Choices: []translator.OpenAIChoice{{Message: translator.OpenAIMessage{Role: "assistant", Content: "summary"}}}}, nil
}

func (fake *fakeOAuthInference) DoStream(context.Context, translator.OpenAIRequest, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

func (fake *fakeOAuthInference) SupportsAnthropicPassthrough(string) bool { return false }

func (fake *fakeOAuthInference) DoAnthropicStream(context.Context, []byte, string, string, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

type fakeResponseClient struct {
	calls  int
	mutate func(*translator.OpenAIResponse)
}

func (f *fakeResponseClient) DoJSON(_ context.Context, _ string, requestBody, responseBody any) error {
	f.calls++
	request := requestBody.(translator.OpenAIRequest)
	response := responseBody.(*translator.OpenAIResponse)
	*response = translator.OpenAIResponse{
		ID: "response", Model: request.Model,
		Choices: []translator.OpenAIChoice{{Message: translator.OpenAIMessage{Role: "assistant", Content: "done"}, FinishReason: "stop"}},
	}
	if f.mutate != nil {
		f.mutate(response)
	}
	return nil
}

func (f *fakeClient) DoJSON(_ context.Context, _ string, requestBody, responseBody any) error {
	f.calls++
	f.last = requestBody.(translator.OpenAIRequest)
	if f.err != nil {
		return f.err
	}
	response := responseBody.(*translator.OpenAIResponse)
	*response = translator.OpenAIResponse{
		ID: "response", Model: f.last.Model,
		Choices: []translator.OpenAIChoice{{Message: translator.OpenAIMessage{Role: "assistant", Content: "done"}, FinishReason: "stop"}},
	}
	return nil
}

func TestServiceChatRoutesAndCaches(t *testing.T) {
	openAI := &fakeClient{}
	xai := &fakeClient{}
	service := New(Options{OpenAI: openAI, XAI: xai, ResponseCache: cache.NewResponseCache(10, 0), PromptMinBytes: 1})
	temperature := 0.0
	request := translator.OpenAIRequest{Model: "gpt-4", Messages: []translator.OpenAIMessage{{Role: "user", Content: "hello"}}, MaxTokens: 10, Temperature: &temperature}
	for range 2 {
		response, err := service.Chat(context.Background(), request)
		if err != nil || response.ID != "response" {
			t.Fatalf("Chat() = %#v, %v", response, err)
		}
	}
	if openAI.calls != 1 || xai.calls != 0 || openAI.last.PromptCacheKey == "" {
		t.Fatalf("calls = openai %d xai %d; request = %#v", openAI.calls, xai.calls, openAI.last)
	}
	request.Model = "grok-4"
	if _, err := service.Chat(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if xai.calls != 1 {
		t.Fatalf("xai calls = %d", xai.calls)
	}
}

func TestServiceResponseCacheRequiresExplicitZeroTemperature(t *testing.T) {
	client := &fakeClient{}
	service := New(Options{OpenAI: client, ResponseCache: cache.NewResponseCache(10, time.Minute)})
	request := translator.OpenAIRequest{Model: "model", Messages: []translator.OpenAIMessage{{Role: "user", Content: "hello"}}}
	for range 2 {
		if _, err := service.Chat(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if client.calls != 2 {
		t.Fatalf("omitted temperature calls = %d", client.calls)
	}
	temperature := 0.0
	request.Temperature = &temperature
	for range 2 {
		if _, err := service.Chat(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if client.calls != 3 {
		t.Fatalf("explicit zero temperature calls = %d", client.calls)
	}
}

func TestServiceResponseCacheRejectsUnsafeRequestsAndResponses(t *testing.T) {
	temperature := 0.0
	tests := []struct {
		name    string
		request translator.OpenAIRequest
		mutate  func(*translator.OpenAIResponse)
	}{
		{name: "tools", request: translator.OpenAIRequest{Tools: []translator.OpenAITool{{Type: "function", Function: translator.OpenAIFunction{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}}}},
		{name: "length", mutate: func(response *translator.OpenAIResponse) { response.Choices[0].FinishReason = "length" }},
		{name: "empty", mutate: func(response *translator.OpenAIResponse) { response.Choices[0].Message.Content = "" }},
		{name: "tool call", mutate: func(response *translator.OpenAIResponse) {
			response.Choices[0].FinishReason = "tool_calls"
			response.Choices[0].Message.ToolCalls = []translator.OpenAIToolCall{{ID: "call", Type: "function", Function: translator.OpenAIFunctionCall{Name: "read", Arguments: `{}`}}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeResponseClient{mutate: test.mutate}
			service := New(Options{OpenAI: client, ResponseCache: cache.NewResponseCache(10, time.Minute)})
			request := test.request
			request.Model = "model"
			request.Messages = []translator.OpenAIMessage{{Role: "user", Content: "hello"}}
			request.Temperature = &temperature
			for range 2 {
				if _, err := service.Chat(context.Background(), request); err != nil {
					t.Fatal(err)
				}
			}
			if client.calls != 2 {
				t.Fatalf("upstream calls = %d", client.calls)
			}
		})
	}
}

func TestServicePromptCacheKeyPreservesIncomingAndUsesConversation(t *testing.T) {
	client := &fakeClient{}
	service := New(Options{OpenAI: client, PromptMinBytes: 1})
	request := translator.OpenAIRequest{Model: "gpt-4.1-mini", User: "person@example.com", Messages: []translator.OpenAIMessage{{Role: "user", Content: "secret prompt"}}}
	ctx := context.WithValue(context.Background(), conversationIDKey{}, "conversation-private")
	if _, err := service.Chat(ctx, request); err != nil {
		t.Fatal(err)
	}
	conversationKey := client.last.PromptCacheKey
	if len(conversationKey) != 64 || strings.Contains(conversationKey, "conversation-private") {
		t.Fatalf("conversation key = %q", conversationKey)
	}
	request.PromptCacheKey = "client-key"
	if _, err := service.Chat(ctx, request); err != nil {
		t.Fatal(err)
	}
	if client.last.PromptCacheKey != "client-key" {
		t.Fatalf("preserved key = %q", client.last.PromptCacheKey)
	}
}

func TestServicePromptCacheMinimumBytes(t *testing.T) {
	client := &fakeClient{}
	service := New(Options{OpenAI: client, PromptMinBytes: 1_000})
	request := translator.OpenAIRequest{Model: "gpt-4.1", Messages: []translator.OpenAIMessage{{Role: "user", Content: "short"}}}
	if _, err := service.Chat(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if client.last.PromptCacheKey != "" {
		t.Fatalf("short prompt key = %q", client.last.PromptCacheKey)
	}
	request.Messages[0].Content = strings.Repeat("long", 300)
	if _, err := service.Chat(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if client.last.PromptCacheKey == "" {
		t.Fatal("long prompt key is empty")
	}
}

func TestServiceGrokPromptCacheRequiresOptIn(t *testing.T) {
	request := translator.OpenAIRequest{Model: "grok-4", Messages: []translator.OpenAIMessage{{Role: "user", Content: "hello"}}}
	disabled := &fakeClient{}
	if _, err := New(Options{XAI: disabled}).Chat(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if disabled.last.PromptCacheKey != "" {
		t.Fatalf("disabled key = %q", disabled.last.PromptCacheKey)
	}
	enabled := &fakeClient{}
	if _, err := New(Options{XAI: enabled, XAIPromptCache: true, PromptMinBytes: 1}).Chat(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if enabled.last.PromptCacheKey == "" {
		t.Fatal("enabled key is empty")
	}
}

func TestServiceXAIPrefixedModelRoutingAndPromptCache(t *testing.T) {
	request := translator.OpenAIRequest{Model: "xai/grok-4", PromptCacheKey: "caller-key", Messages: []translator.OpenAIMessage{{Role: "user", Content: "hello"}}}
	disabled := &fakeClient{}
	if _, err := New(Options{XAI: disabled}).Chat(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if disabled.last.Model != "grok-4" || disabled.last.PromptCacheKey != "" {
		t.Fatalf("disabled request = %#v", disabled.last)
	}
	enabled := &fakeClient{}
	if _, err := New(Options{XAI: enabled, XAIPromptCache: true}).Chat(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if enabled.last.Model != "grok-4" || enabled.last.PromptCacheKey != "caller-key" {
		t.Fatalf("enabled request = %#v", enabled.last)
	}
}

func TestServiceAliasPreparesEachConcreteModel(t *testing.T) {
	openAI := &fakeClient{err: &provider.ProviderError{StatusCode: 429, Message: "limited"}}
	xai := &fakeClient{}
	var resolved []string
	service := New(Options{
		OpenAI: openAI, XAI: xai, Aliases: map[string][]string{"fast": {"gpt-4.1", "grok-4"}},
		ContextEnabled: true, ContextLimits: contextguard.Limits{Default: 128_000}, ContextPolicy: contextguard.DefaultPolicy(),
		ContextWindow: func(_ context.Context, model string) (int, error) {
			resolved = append(resolved, model)
			if model == "gpt-4.1" {
				return 1_000_000, nil
			}
			return 256_000, nil
		},
	})
	request := translator.OpenAIRequest{Model: "fast", Messages: []translator.OpenAIMessage{{Role: "user", Content: "hello"}}}
	if _, err := service.Chat(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 2 || resolved[0] != "gpt-4.1" || resolved[1] != "grok-4" {
		t.Fatalf("resolved = %#v", resolved)
	}
}

func TestServiceAliasFallsBackAcrossProviders(t *testing.T) {
	openAI := &fakeClient{err: &provider.ProviderError{StatusCode: 429, Message: "limited"}}
	xai := &fakeClient{}
	service := New(Options{
		OpenAI: openAI, XAI: xai,
		Aliases: map[string][]string{"fast": {"gpt-4.1", "grok-4"}},
	})
	request := translator.OpenAIRequest{Model: "fast", Messages: []translator.OpenAIMessage{{Role: "user", Content: "hello"}}}
	response, err := service.Chat(context.Background(), request)
	if err != nil || response.ID != "response" || openAI.calls != 1 || xai.calls != 1 || xai.last.Model != "grok-4" {
		t.Fatalf("response = %#v, error = %v, calls = %d/%d, last = %#v", response, err, openAI.calls, xai.calls, xai.last)
	}
}

func TestServiceSummarizesNearLimitOnceAndCaches(t *testing.T) {
	client := &fakeClient{}
	summarizer := &fakeSummarizer{text: "preserved facts"}
	service := New(Options{
		OpenAI: client, ContextEnabled: true,
		// The window has to hold the backlog plus the summary itself, otherwise the
		// summarization request cannot fit either and the guard trims instead.
		ContextLimits: contextguard.Limits{Default: 12_000},
		ContextPolicy: contextguard.Policy{
			SoftRatio: 0.50, SummarizeRatio: 0.60, HardRatio: 0.95, KeepRecentTurns: 1,
		},
		Summarizer: summarizer, SummaryMaxTokens: 321, SummaryTimeout: time.Second,
	})
	request := translator.OpenAIRequest{Model: "model", Messages: []translator.OpenAIMessage{
		{Role: "user", Content: strings.Repeat("older fact ", 1500)},
		{Role: "assistant", Content: strings.Repeat("older response ", 400)},
		{Role: "user", Content: "latest instruction"},
	}}
	for range 2 {
		if _, err := service.Chat(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if summarizer.calls != 1 || summarizer.input.MaxTokens != 321 || len(summarizer.input.Messages) != 2 {
		t.Fatalf("summarizer = calls %d, input %#v", summarizer.calls, summarizer.input)
	}
	if len(client.last.Messages) != 2 || !strings.Contains(client.last.Messages[0].Content.(string), "preserved facts") || client.last.Messages[1].Content != "latest instruction" {
		t.Fatalf("upstream messages = %#v", client.last.Messages)
	}
}

// A backlog larger than one window used to be refused outright. That was the right rule
// for a single-shot summarizer and the wrong one for this summarizer, which map-reduces
// through SummaryBatches and handles any size — so the refusal fired in exactly the cases
// batching exists for. In the production log it fired on all eight trims: eight refusals,
// no summary ever attempted, 726 messages dropped instead.
func TestServiceSummarizesABacklogLargerThanOneWindow(t *testing.T) {
	client := &fakeClient{}
	summarizer := &fakeSummarizer{text: "preserved facts"}
	service := New(Options{
		OpenAI: client, ContextEnabled: true,
		ContextLimits: contextguard.Limits{Default: 12_000},
		ContextPolicy: contextguard.Policy{
			SoftRatio: 0.50, SummarizeRatio: 0.60, HardRatio: 0.95, KeepRecentTurns: 1,
		},
		Summarizer: summarizer, SummaryMaxTokens: 321, SummaryTimeout: time.Second,
	})
	// ~15.5k estimated tokens of backlog against a 12k window: more than one batch,
	// comfortably fewer than maxSummaryBatches.
	request := translator.OpenAIRequest{Model: "model", Messages: []translator.OpenAIMessage{
		{Role: "user", Content: strings.Repeat("older fact ", 3000)},
		{Role: "assistant", Content: strings.Repeat("older response ", 900)},
		{Role: "user", Content: "latest instruction"},
	}}
	if _, err := service.Chat(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	if summarizer.calls != 1 {
		t.Fatalf("summarizer calls = %d, want the oversized backlog summarized", summarizer.calls)
	}
	// And the turn actually carries the summary rather than a hole where the history was.
	last := client.last.Messages
	if len(last) == 0 || last[len(last)-1].Content != "latest instruction" {
		t.Fatalf("upstream messages = %#v", last)
	}
	if text, _ := last[0].Content.(string); !strings.Contains(text, "preserved facts") {
		t.Fatalf("summary missing from the upstream request: %#v", last[0])
	}
}

func TestServiceSkipsSummaryWhenBacklogExceedsWindow(t *testing.T) {
	client := &fakeClient{}
	// A summarizer that would block for the whole timeout if it were ever called.
	// At 550k against a 400k window this cost a flat 60s of time-to-first-token
	// before the deterministic trim ran anyway.
	summarizer := &blockingSummarizer{}
	service := New(Options{
		OpenAI: client, ContextEnabled: true,
		ContextLimits: contextguard.Limits{Default: 4_000},
		ContextPolicy: contextguard.Policy{
			SoftRatio: 0.50, SummarizeRatio: 0.60, HardRatio: 0.95, KeepRecentTurns: 1,
		},
		Summarizer: summarizer, SummaryMaxTokens: 321, SummaryTimeout: time.Minute,
	})
	request := translator.OpenAIRequest{Model: "model", Messages: []translator.OpenAIMessage{
		{Role: "user", Content: strings.Repeat("older fact ", 1500)},
		{Role: "assistant", Content: strings.Repeat("older response ", 400)},
		{Role: "user", Content: "latest instruction"},
	}}
	started := time.Now()
	if _, err := service.Chat(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if summarizer.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", summarizer.calls)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("prepare took %s; the doomed summary attempt was not skipped", elapsed)
	}
	if len(client.last.Messages) == 0 || client.last.Messages[len(client.last.Messages)-1].Content != "latest instruction" {
		t.Fatalf("upstream messages = %#v", client.last.Messages)
	}
}

type blockingSummarizer struct{ calls int }

func (s *blockingSummarizer) Summarize(ctx context.Context, _ contextguard.SummaryInput) (string, error) {
	s.calls++
	<-ctx.Done()
	return "", ctx.Err()
}

func TestSummaryClientUsesOAuthInference(t *testing.T) {
	oauth := &fakeOAuthInference{}
	service := New(Options{OAuthInference: oauth, ContextLimits: contextguard.Limits{Default: 2_000}})
	text, err := (summaryClient{service: service}).Summarize(context.Background(), contextguard.SummaryInput{
		Model: "gpt-5.6-sol", MaxTokens: 100,
		Messages: []provider.Message{{Role: "user", Content: []provider.Content{{Type: "text", Text: "older context"}}}},
	})
	if err != nil || text != "summary" || oauth.calls != 1 {
		t.Fatalf("Summarize() = %q, %v; OAuth calls = %d", text, err, oauth.calls)
	}
	if oauth.last.Model != "gpt-5.6-sol" {
		t.Fatalf("OAuth model = %q", oauth.last.Model)
	}
	// A summary request that names no effort is sent at "high" by the Codex payload
	// builder, which turned every batch into a full-effort reasoning call over a
	// quarter-million tokens. Compression is not a reasoning task.
	if oauth.last.Effort != summaryEffort {
		t.Fatalf("summary effort = %q, want %q", oauth.last.Effort, summaryEffort)
	}
}

// concurrentOAuthInference answers slowly enough that overlapping calls are observable,
// and echoes back a marker from the batch it was given so ordering can be checked.
type concurrentOAuthInference struct {
	mu       sync.Mutex
	calls    int
	inFlight int
	peak     int
}

func (fake *concurrentOAuthInference) DoJSON(_ context.Context, request translator.OpenAIRequest, _ string) (translator.OpenAIResponse, error) {
	fake.mu.Lock()
	fake.calls++
	fake.inFlight++
	fake.peak = max(fake.peak, fake.inFlight)
	fake.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	fake.mu.Lock()
	fake.inFlight--
	fake.mu.Unlock()

	marker := "unknown"
	for _, message := range request.Messages {
		text := fmt.Sprintf("%v", message.Content)
		if index := strings.Index(text, "batch-"); index >= 0 {
			marker = text[index : index+7]
			break
		}
	}
	return translator.OpenAIResponse{Choices: []translator.OpenAIChoice{
		{Message: translator.OpenAIMessage{Role: "assistant", Content: marker}}}}, nil
}

func (fake *concurrentOAuthInference) DoStream(context.Context, translator.OpenAIRequest, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}
func (fake *concurrentOAuthInference) SupportsAnthropicPassthrough(string) bool { return false }
func (fake *concurrentOAuthInference) DoAnthropicStream(context.Context, []byte, string, string, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

// The map phase used to run its batches one after another, so a backlog needing more than
// one batch had to fit the whole chain inside a single summaryTimeout — and mostly did
// not, which is half of why summarizing was refused outright. They are independent calls,
// so they overlap; the results still have to come back in batch order or the summary
// reassembles the conversation out of sequence.
func TestSummaryBatchesRunConcurrentlyAndKeepTheirOrder(t *testing.T) {
	oauth := &concurrentOAuthInference{}
	service := New(Options{OAuthInference: oauth, ContextLimits: contextguard.Limits{Default: 2_000}})
	batches := make([][]provider.Message, 4)
	for index := range batches {
		batches[index] = []provider.Message{{Role: "user", Content: []provider.Content{
			{Type: "text", Text: fmt.Sprintf("batch-%d contents", index)}}}}
	}

	texts, err := (summaryClient{service: service}).summarizeBatches(
		context.Background(), "gpt-5.6-sol", "instruction", batches, 100)
	if err != nil {
		t.Fatal(err)
	}

	if oauth.calls != 4 {
		t.Fatalf("upstream calls = %d, want one per batch", oauth.calls)
	}
	if oauth.peak < 2 {
		t.Fatalf("peak concurrent calls = %d; the batches ran one after another", oauth.peak)
	}
	if oauth.peak > maxConcurrentSummaryBatches {
		t.Fatalf("peak concurrent calls = %d, over the bound of %d", oauth.peak, maxConcurrentSummaryBatches)
	}
	for index, text := range texts {
		if want := fmt.Sprintf("batch-%d", index); text != want {
			t.Fatalf("texts[%d] = %q, want %q", index, text, want)
		}
	}
}

func TestSummaryClientRejectsOversizedStructuredUnitBeforeUpstream(t *testing.T) {
	client := &fakeClient{}
	service := New(Options{OpenAI: client, ContextLimits: contextguard.Limits{Default: 2_000}})
	input := contextguard.SummaryInput{
		Model: "model", MaxTokens: 100,
		Messages: []provider.Message{{Role: "assistant", Content: []provider.Content{{
			Type: "tool_use", Name: "large", Input: json.RawMessage(`{"data":"` + strings.Repeat("x", 10_000) + `"}`),
		}}}},
	}
	_, err := (summaryClient{service: service}).Summarize(context.Background(), input)
	var tooLarge *contextguard.SummaryUnitTooLargeError
	if !errors.As(err, &tooLarge) || client.calls != 0 {
		t.Fatalf("Summarize() error = %T %v; upstream calls = %d", err, err, client.calls)
	}
}

func TestServiceSummaryKeyFailureBypassesCache(t *testing.T) {
	summarizer := &fakeSummarizer{text: "preserved facts"}
	service := New(Options{
		ContextEnabled: true, ContextLimits: contextguard.Limits{Default: 5_000},
		ContextPolicy: contextguard.Policy{SoftRatio: 0.1, SummarizeRatio: 0.2, HardRatio: 0.95, KeepRecentTurns: 1},
		Summarizer:    summarizer, SummaryMaxTokens: 100,
	})
	request := provider.Request{Model: "model", Messages: []provider.Message{
		{Role: "assistant", Content: []provider.Content{{Type: "tool_use", Name: "bad", Input: json.RawMessage(`{`)}}},
		{Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat("older ", 500)}}},
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "latest"}}},
	}}
	for range 2 {
		if _, err := service.prepareContext(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if summarizer.calls != 2 {
		t.Fatalf("summarizer calls = %d", summarizer.calls)
	}
}

func TestServiceSummaryAdaptivelyKeepsFewerRecentTurns(t *testing.T) {
	summarizer := &fakeSummarizer{text: "preserved facts"}
	service := New(Options{
		ContextEnabled: true, ContextLimits: contextguard.Limits{Default: 3_000},
		ContextPolicy: contextguard.Policy{SoftRatio: 0.2, SummarizeRatio: 0.3, HardRatio: 0.9, KeepRecentTurns: 6},
		Summarizer:    summarizer, SummaryMaxTokens: 100,
	})
	messages := []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat("old ", 600)}}},
		{Role: "assistant", Content: []provider.Content{{Type: "text", Text: "old answer"}}},
	}
	for index := range 6 {
		messages = append(messages,
			provider.Message{Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat(fmt.Sprintf("recent-%d ", index), 150)}}},
			provider.Message{Role: "assistant", Content: []provider.Content{{Type: "text", Text: "answer"}}},
		)
	}
	prepared, err := service.prepareContext(context.Background(), provider.Request{Model: "model", Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Messages) >= len(messages) || !strings.Contains(prepared.Messages[0].Content[0].Text, "preserved facts") {
		t.Fatalf("adaptive summary = %#v", prepared.Messages)
	}
	if prepared.Messages[len(prepared.Messages)-2].Role != "user" {
		t.Fatalf("latest turn was not preserved: %#v", prepared.Messages)
	}
}

func TestServiceSummaryFailureFallsBackBelowHardLimit(t *testing.T) {
	client := &fakeClient{}
	summarizer := &fakeSummarizer{err: errors.New("summary failed")}
	service := New(Options{
		OpenAI: client, ContextEnabled: true,
		ContextLimits: contextguard.Limits{Default: 5000},
		ContextPolicy: contextguard.Policy{
			SoftRatio: 0.50, SummarizeRatio: 0.60, HardRatio: 0.95, KeepRecentTurns: 1,
		},
		Summarizer: summarizer,
	})
	request := translator.OpenAIRequest{Model: "model", Messages: []translator.OpenAIMessage{
		{Role: "user", Content: strings.Repeat("older fact ", 900)},
		{Role: "user", Content: "latest instruction"},
	}}
	if _, err := service.Chat(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if summarizer.calls != 1 || client.calls != 1 {
		t.Fatalf("calls = summary %d, upstream %d", summarizer.calls, client.calls)
	}
}

func TestServiceSummaryFailureTrimsOldestTurnsAboveHardLimit(t *testing.T) {
	client := &fakeClient{}
	summarizer := &fakeSummarizer{err: errors.New("summary failed")}
	service := New(Options{
		OpenAI: client, ContextEnabled: true,
		ContextLimits: contextguard.Limits{Default: 3000},
		ContextPolicy: contextguard.Policy{
			SoftRatio: 0.50, SummarizeRatio: 0.60, HardRatio: 0.70, KeepRecentTurns: 1,
		},
		Summarizer: summarizer,
	})
	request := translator.OpenAIRequest{Model: "model", Messages: []translator.OpenAIMessage{
		{Role: "user", Content: strings.Repeat("older fact ", 2000)},
		{Role: "user", Content: "latest instruction"},
	}}
	// Summarization is unavailable and the request is over the hard limit. Dropping
	// the oldest turn keeps the session alive; rejecting it ends the turn outright.
	if _, err := service.Chat(context.Background(), request); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("upstream calls = %d", client.calls)
	}
	sent, err := json.Marshal(client.last.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sent), "latest instruction") {
		t.Fatalf("newest turn was dropped: %s", sent)
	}
	if strings.Contains(string(sent), "older fact") {
		t.Fatalf("oldest turn was not trimmed: %s", sent)
	}
}

func TestContextGuardDefersEstimatedOverageWithoutMutation(t *testing.T) {
	client := &fakeClient{}
	service := New(Options{
		OpenAI: client, ContextGuard: true,
		ContextLimits: contextguard.Limits{Default: 2_000},
		ContextWindow: func(_ context.Context, model string) (int, error) {
			if model != "model" {
				t.Fatalf("model = %q", model)
			}
			return 2_000, nil
		},
		ContextPolicy: contextguard.DefaultPolicy(),
	})
	content := strings.Repeat("large context ", 2_000)
	request := translator.OpenAIRequest{Model: "model", Messages: []translator.OpenAIMessage{{Role: "user", Content: content}}}
	if _, err := service.Chat(context.Background(), request); err != nil || client.calls != 1 {
		t.Fatalf("Chat() error = %v, upstream calls = %d", err, client.calls)
	}
	if request.Messages[0].Content != content || client.last.Messages[0].Content != content {
		t.Fatal("guard mutated request")
	}
}

func TestContextGuardAllowsRequestWithinDatabaseWindow(t *testing.T) {
	client := &fakeClient{}
	service := New(Options{
		OpenAI: client, ContextGuard: true,
		ContextLimits: contextguard.Limits{Default: 1_000},
		ContextWindow: func(context.Context, string) (int, error) { return 200_000, nil },
		ContextPolicy: contextguard.DefaultPolicy(),
	})
	request := translator.OpenAIRequest{Model: "model", Messages: []translator.OpenAIMessage{{Role: "user", Content: strings.Repeat("safe context ", 2_000)}}}
	if _, err := service.Chat(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 {
		t.Fatalf("upstream calls = %d", client.calls)
	}
}

func TestServicePropagatesUpstreamError(t *testing.T) {
	client := &fakeClient{err: errors.New("upstream failed")}
	service := New(Options{OpenAI: client, ResponseCache: cache.NewResponseCache(10, 0)})
	request := translator.OpenAIRequest{Model: "model", Messages: []translator.OpenAIMessage{{Role: "user", Content: "hello"}}}
	if _, err := service.Chat(context.Background(), request); err == nil {
		t.Fatal("Chat() error = nil")
	}
}

func TestGatewayRejectsTrailingJSON(t *testing.T) {
	service := New(Options{})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]} {}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestCountTokensHandler(t *testing.T) {
	service := New(Options{})
	e := echo.New()
	service.Register(e)

	body := `{
		"model":"claude-test",
		"system":"system prompt",
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":[{"type":"text","text":"world"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"done"}]}
		],
		"tools":[{"name":"lookup","description":"look up data","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}]
	}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.InputTokens <= 0 {
		t.Fatalf("response = %#v, error = %v", response, err)
	}
}

func TestCountTokensHandlerRejectsInvalidRequest(t *testing.T) {
	service := New(Options{})
	e := echo.New()
	service.Register(e)

	for name, body := range map[string]string{
		"malformed":   `{"model":`,
		"no model":    `{"messages":[{"role":"user","content":"hello"}]}`,
		"no messages": `{"model":"claude-test","messages":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			e.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"type":"error"`) {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestGatewayHandlers(t *testing.T) {
	client := &fakeClient{}
	service := New(Options{OpenAI: client, Models: []string{"model"}})
	e := echo.New()
	service.Register(e)

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response translator.OpenAIResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.ID != "response" {
		t.Fatalf("response = %#v, error = %v", response, err)
	}

	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRecorder := httptest.NewRecorder()
	e.ServeHTTP(modelsRecorder, modelsRequest)
	if modelsRecorder.Code != http.StatusOK || !strings.Contains(modelsRecorder.Body.String(), `"id":"model"`) || !strings.Contains(modelsRecorder.Body.String(), `"type":"model"`) {
		t.Fatalf("models = %d %s", modelsRecorder.Code, modelsRecorder.Body.String())
	}

	modelRequest := httptest.NewRequest(http.MethodGet, "/v1/models/model", nil)
	modelRecorder := httptest.NewRecorder()
	e.ServeHTTP(modelRecorder, modelRequest)
	if modelRecorder.Code != http.StatusOK || !strings.Contains(modelRecorder.Body.String(), `"display_name":"model"`) {
		t.Fatalf("model = %d %s", modelRecorder.Code, modelRecorder.Body.String())
	}

	missingRequest := httptest.NewRequest(http.MethodGet, "/v1/models/missing", nil)
	missingRecorder := httptest.NewRecorder()
	e.ServeHTTP(missingRecorder, missingRequest)
	if missingRecorder.Code != http.StatusNotFound || !strings.Contains(missingRecorder.Body.String(), `"type":"error"`) {
		t.Fatalf("missing model = %d %s", missingRecorder.Code, missingRecorder.Body.String())
	}
}

func TestServiceUsesDynamicContextWindow(t *testing.T) {
	resolved := 0
	service := New(Options{
		ContextEnabled: true,
		ContextLimits:  contextguard.Limits{Default: 1_000},
		ContextWindow: func(_ context.Context, model string) (int, error) {
			if model != "cx/custom-large" {
				t.Fatalf("model = %q", model)
			}
			resolved++
			return 400_000, nil
		},
		ContextPolicy: contextguard.DefaultPolicy(),
	})
	request := provider.Request{
		Model: "cx/custom-large",
		Messages: []provider.Message{{
			Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat("quality context ", 10_000)}},
		}},
	}
	prepared, err := service.prepareContext(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != 1 || len(prepared.Messages) != 1 || prepared.Messages[0].Content[0].Text != request.Messages[0].Content[0].Text {
		t.Fatalf("resolved=%d prepared=%+v", resolved, prepared)
	}
}

func TestContextOverflowReportsInvalidRequest(t *testing.T) {
	// Only reached when summarization and trimming both fail. Claude Code keys its
	// own compaction off this Anthropic error shape; a 422 api_error made it treat
	// the turn as a server fault and retry the same oversized prompt instead.
	service := New(Options{
		OpenAI: &fakeClient{}, ContextEnabled: true,
		ContextLimits: contextguard.Limits{Default: 1_000},
		ContextPolicy: contextguard.Policy{SoftRatio: 0.50, SummarizeRatio: 0.60, HardRatio: 0.70, KeepRecentTurns: 1},
	})
	e := echo.New()
	service.Register(e)
	body := `{"model":"model","max_tokens":10,"messages":[{"role":"user","content":"` + strings.Repeat("overflow ", 4000) + `"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"type":"invalid_request_error"`) || !strings.Contains(recorder.Body.String(), "prompt is too long") {
		t.Fatalf("overflow error shape = %s", recorder.Body.String())
	}
}

// emptyThenRealClient returns a content-free response first, then a real one.
type emptyThenRealClient struct {
	calls int
	text  string
}

func (c *emptyThenRealClient) DoJSON(_ context.Context, _ string, _ any, out any) error {
	c.calls++
	response := out.(*translator.OpenAIResponse)
	*response = translator.OpenAIResponse{ID: "r", Model: "m", Choices: []translator.OpenAIChoice{{
		Message: translator.OpenAIMessage{Role: "assistant", Content: ""}, FinishReason: "stop",
	}}}
	if c.calls > 1 {
		response.Choices[0].Message.Content = c.text
	}
	return nil
}

func TestCompleteRetriesContentFreeResponse(t *testing.T) {
	// The non-streaming twin of the empty turn: /compact takes this path when the
	// client falls back off streaming, and a blank reply there is reported to the
	// user as an empty response with no summary.
	client := &emptyThenRealClient{text: "here is the summary"}
	service := New(Options{OpenAI: client})
	response, err := service.Messages(context.Background(), translator.AnthropicRequest{
		Model: "gemini-3.1-pro-high", MaxTokens: 100,
		Messages: []translator.AnthropicMessage{{Role: "user", Content: []translator.AnthropicContent{
			{Type: "text", Text: "summarize"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Fatalf("upstream attempts = %d, want the blank reply retried", client.calls)
	}
	if len(response.Content) == 0 || response.Content[0].Text != "here is the summary" {
		t.Fatalf("response = %#v", response.Content)
	}
}

// alwaysEmptyClient never returns content.
type alwaysEmptyClient struct{ calls int }

func (c *alwaysEmptyClient) DoJSON(_ context.Context, _ string, _ any, out any) error {
	c.calls++
	*out.(*translator.OpenAIResponse) = translator.OpenAIResponse{
		ID: "r", Model: "m", Choices: []translator.OpenAIChoice{{
			Message: translator.OpenAIMessage{Role: "assistant", Content: " "}, FinishReason: "stop",
		}}}
	return nil
}

func TestCompleteReturnsEmptyResponseAfterBoundedReplays(t *testing.T) {
	client := &alwaysEmptyClient{}
	service := New(Options{OpenAI: client})
	// A genuinely silent model must not become an error, but the attempts are bounded.
	if _, err := service.Messages(context.Background(), translator.AnthropicRequest{
		Model: "gemini-3.1-pro-high", MaxTokens: 100,
		Messages: []translator.AnthropicMessage{{Role: "user", Content: []translator.AnthropicContent{
			{Type: "text", Text: "summarize"}}}},
	}); err != nil {
		t.Fatalf("a silent model became an error: %v", err)
	}
	if client.calls != 1+maxEmptyTurnReplays {
		t.Fatalf("upstream attempts = %d, want %d", client.calls, 1+maxEmptyTurnReplays)
	}
}

func TestRequestAlreadySummarized(t *testing.T) {
	summaryText := contextguard.ProxySummaryMarker + " Historical context from earlier turns."
	cases := []struct {
		name     string
		messages []provider.Message
		want     bool
	}{
		{
			name: "proxy summary as the oldest user message",
			messages: []provider.Message{
				{Role: "user", Content: []provider.Content{{Type: "text", Text: summaryText}}},
				{Role: "assistant", Content: []provider.Content{{Type: "text", Text: "ok"}}},
				{Role: "user", Content: []provider.Content{{Type: "text", Text: "continue"}}},
			},
			want: true,
		},
		{
			name: "post-compact continuation quoting the summary first",
			messages: []provider.Message{
				{Role: "user", Content: []provider.Content{{Type: "text", Text: "Continue the conversation.\n\n" + summaryText}}},
				{Role: "user", Content: []provider.Content{{Type: "text", Text: "finish the task"}}},
			},
			want: true,
		},
		{
			name: "summary marker quoted in a later message is history, not the request",
			messages: []provider.Message{
				{Role: "user", Content: []provider.Content{{Type: "text", Text: "ordinary opening"}}},
				{Role: "user", Content: []provider.Content{{Type: "text", Text: summaryText}}},
			},
		},
		{
			name: "no summary marker",
			messages: []provider.Message{
				{Role: "user", Content: []provider.Content{{Type: "text", Text: "ordinary opening"}}},
				{Role: "user", Content: []provider.Content{{Type: "text", Text: "continue"}}},
			},
		},
		{
			name: "oldest message is a tool result, not a summary",
			messages: []provider.Message{
				{Role: "user", Content: []provider.Content{{Type: "tool_result", ToolUseID: "t1", Text: "some output"}}},
				{Role: "user", Content: []provider.Content{{Type: "text", Text: summaryText}}},
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := provider.Request{Model: "model", Messages: testCase.messages}
			if got := requestAlreadySummarized(request); got != testCase.want {
				t.Fatalf("requestAlreadySummarized = %v, want %v", got, testCase.want)
			}
		})
	}
}
