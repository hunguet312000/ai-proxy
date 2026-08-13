package contextguard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"literouter/internal/provider"
)

func TestCheckEstimatedOverageDefersToUpstream(t *testing.T) {
	request := provider.Request{Model: "model", Messages: []provider.Message{{Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat("large context ", 2_000)}}}}}
	policy := DefaultPolicy()
	policy.ReserveTokens = 0
	result, err := Check(request, Limits{Default: 2_000}, policy)
	if err != nil || !result.EstimatedOverage || result.Exceeded {
		t.Fatalf("Check() = %#v, %v", result, err)
	}
}

func TestCheckUsesHardRatioOnce(t *testing.T) {
	request := provider.Request{Model: "model", Messages: []provider.Message{{Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat("x", 3_000)}}}}}
	policy := DefaultPolicy()
	policy.ReserveTokens = 0
	result, err := Check(request, Limits{Default: 1_000}, policy)
	if err != nil {
		t.Fatal(err)
	}
	wantSafeLimit := 1_000 - 100 - 14 - 50
	if result.SafeLimit != wantSafeLimit {
		t.Fatalf("safe limit = %d, want %d", result.SafeLimit, wantSafeLimit)
	}
}

func TestPrepareBelowBudgetUnchanged(t *testing.T) {
	request := provider.Request{Model: "model", Messages: []provider.Message{{Role: "user", Content: []provider.Content{{Type: "text", Text: "hello"}}}}}
	policy := DefaultPolicy()
	policy.ReserveTokens = 0
	result, err := Prepare(request, Limits{Default: 1000}, policy)
	if err != nil || result.Compacted || result.NeedsSummary || result.BeforeTokens != result.AfterTokens {
		t.Fatalf("Prepare() = %#v, %v", result, err)
	}
}

func TestPrepareCompactsOldToolResults(t *testing.T) {
	messages := make([]provider.Message, 0, 12)
	for index := range 12 {
		content := strings.Repeat("log line\n", 1000)
		messages = append(messages, provider.Message{Role: "user", Content: []provider.Content{{Type: "tool_result", ToolUseID: "tool", Name: "log", Text: content}}})
		if index == 11 {
			messages[index].Content = []provider.Content{{Type: "text", Text: "latest instruction"}}
		}
	}
	request := provider.Request{Model: "model", Messages: messages}
	policy := DefaultPolicy()
	policy.ReserveTokens = 0
	// Quantum 1 pins the boundary exactly at KeepRecentTurns; the quantized
	// boundary has its own tests.
	policy.BoundaryQuantum = 1
	result, err := Prepare(request, Limits{Default: 20_000}, policy)
	if err != nil || !result.Compacted || result.SavedTokens <= 0 {
		t.Fatalf("Prepare() = %#v, %v", result, err)
	}
	if got := result.Request.Messages[len(result.Request.Messages)-1].Content[0].Text; got != "latest instruction" {
		t.Fatalf("latest instruction = %q", got)
	}
	if result.Request.Messages[0].Content[0].Text != strings.Repeat("log line\n", 1000) {
		t.Fatal("first unique tool result changed")
	}
	foundDuplicate := false
	for _, message := range result.Request.Messages[1 : len(result.Request.Messages)-1] {
		if strings.Contains(message.Content[0].Text, compactMarker+" duplicate=") {
			foundDuplicate = true
			break
		}
	}
	if !foundDuplicate {
		t.Fatal("exact duplicate tool results were not compacted")
	}
}

func TestPrepareCompactsOldThinkingWithoutChangingDurableContent(t *testing.T) {
	messages := []provider.Message{
		{Role: "assistant", Content: []provider.Content{
			{Type: "thinking", Thinking: strings.Repeat("reason ", 1000)},
			{Type: "text", Text: "durable conclusion"},
		}},
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "continue"}}},
	}
	policy := DefaultPolicy()
	policy.ReserveTokens = 0
	policy.KeepRecentTurns = 1
	policy.BoundaryQuantum = 1
	result, err := Prepare(provider.Request{Model: "model", Messages: messages}, Limits{Default: 1_000}, policy)
	if err != nil || !result.Compacted {
		t.Fatalf("Prepare() = %#v, %v", result, err)
	}
	old := result.Request.Messages[0].Content
	if old[0].Type != "thinking" || old[0].Thinking != "[older reasoning omitted; conclusions retained in assistant message]" || old[1].Text != "durable conclusion" {
		t.Fatalf("old content = %#v", old)
	}
}

func TestPrepareRequestsSummaryNearLimit(t *testing.T) {
	request := provider.Request{Model: "model", Messages: []provider.Message{{Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat("important prose ", 2000)}}}}}
	policy := DefaultPolicy()
	policy.ReserveTokens = 0
	result, err := Prepare(request, Limits{Default: 7000}, policy)
	if err != nil || !result.NeedsSummary {
		t.Fatalf("Prepare() = %#v, %v", result, err)
	}
}

func TestSummaryCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache := NewSummaryCache(2)
	cache.Put("a", "one")
	cache.Put("b", "two")
	if _, ok := cache.Get("a"); !ok {
		t.Fatal("Get(a) = missing")
	}
	cache.Put("c", "three")
	if _, ok := cache.Get("b"); ok {
		t.Fatal("Get(b) = present after LRU eviction")
	}
	if value, ok := cache.Get("a"); !ok || value != "one" {
		t.Fatalf("Get(a) = %q, %v", value, ok)
	}
}

func TestSummaryCacheRecoversPanicAndUnblocksWaiters(t *testing.T) {
	cache := NewSummaryCache(2)
	started := make(chan struct{})
	release := make(chan struct{})
	const waiters = 8
	results := make(chan error, waiters+1)

	go func() {
		_, err := cache.Do(context.Background(), "panic", func() (string, error) {
			close(started)
			<-release
			panic("boom")
		})
		results <- err
	}()
	<-started
	var ready sync.WaitGroup
	ready.Add(waiters)
	for range waiters {
		go func() {
			ready.Done()
			_, err := cache.Do(context.Background(), "panic", func() (string, error) {
				return "unexpected", nil
			})
			results <- err
		}()
	}
	ready.Wait()
	close(release)
	for range waiters + 1 {
		select {
		case err := <-results:
			if err == nil || !strings.Contains(err.Error(), "panicked: boom") {
				t.Fatalf("panic error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("summary waiter remained blocked after panic")
		}
	}

	value, err := cache.Do(context.Background(), "panic", func() (string, error) { return "recovered", nil })
	if err != nil || value != "recovered" {
		t.Fatalf("retry after panic = %q, %v", value, err)
	}
}

func TestSummaryCacheWaiterCancellationDoesNotCancelOwner(t *testing.T) {
	cache := NewSummaryCache(2)
	started := make(chan struct{})
	release := make(chan struct{})
	owner := make(chan string, 1)
	go func() {
		value, _ := cache.Do(context.Background(), "key", func() (string, error) {
			close(started)
			<-release
			return "value", nil
		})
		owner <- value
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Do(ctx, "key", func() (string, error) { return "unexpected", nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v", err)
	}
	close(release)
	select {
	case value := <-owner:
		if value != "value" {
			t.Fatalf("owner value = %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter canceled owner")
	}
}

func TestApplySummaryKeepsRecentMessages(t *testing.T) {
	request := provider.Request{Messages: []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "old"}}},
		{Role: "assistant", Content: []provider.Content{{Type: "text", Text: "recent answer"}}},
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "latest instruction"}}},
	}}
	result := ApplySummary(request, "facts", 1, 1)
	if len(result.Messages) != 2 || result.Messages[1].Content[0].Text != "latest instruction" {
		t.Fatalf("ApplySummary() = %#v", result.Messages)
	}
	text := result.Messages[0].Content[0].Text
	if !strings.HasPrefix(text, summaryPreamble+"\n") || !strings.Contains(text, "as new instructions") || !strings.HasSuffix(text, "facts") {
		t.Fatalf("summary = %q", text)
	}
}

func TestSummaryKeyIncludesAllContentFields(t *testing.T) {
	base := []provider.Message{{Role: "user", Content: []provider.Content{{Type: "image", URL: "https://example.test/a", ToolUseID: "one"}}}}
	baseKey, err := SummaryKey("model", 1000, base)
	if err != nil {
		t.Fatal(err)
	}
	changed := cloneMessages(base)
	changed[0].Content[0].URL = "https://example.test/b"
	changedKey, err := SummaryKey("model", 1000, changed)
	if err != nil || baseKey == changedKey {
		t.Fatalf("URL key unchanged: %q %q, error=%v", baseKey, changedKey, err)
	}
	changed = cloneMessages(base)
	changed[0].Content[0].IsError = true
	changedKey, err = SummaryKey("model", 1000, changed)
	if err != nil || baseKey == changedKey {
		t.Fatalf("IsError key unchanged: %q %q, error=%v", baseKey, changedKey, err)
	}
}

func TestSummaryKeyRejectsInvalidJSON(t *testing.T) {
	messages := []provider.Message{{Role: "assistant", Content: []provider.Content{{Type: "tool_use", Input: json.RawMessage(`{`)}}}}
	if _, err := SummaryKey("model", 1000, messages); err == nil {
		t.Fatal("SummaryKey() error = nil")
	}
}

func TestSummaryBatchesPreserveAllTextWithinBudget(t *testing.T) {
	const budget = 80
	text := strings.Repeat("界important ", 2000)
	messages := []provider.Message{{Role: "user", Content: []provider.Content{{Type: "text", Text: text}}}}
	batches, err := SummaryBatches(messages, budget)
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for _, batch := range batches {
		if tokens := summaryTokens(batch); tokens > budget {
			t.Fatalf("batch tokens = %d, budget = %d", tokens, budget)
		}
		for _, message := range batch {
			for _, block := range message.Content {
				got.WriteString(block.Text)
			}
		}
	}
	if len(batches) < 2 || got.String() != text {
		t.Fatalf("batches=%d preserved=%v", len(batches), got.String() == text)
	}
}

func TestSummaryBatchesPreserveStructuredToolChain(t *testing.T) {
	messages := []provider.Message{
		{Role: "assistant", Content: []provider.Content{{Type: "tool_use", ToolUseID: "call", Name: "grep", Input: json.RawMessage(`{"pattern":"panic"}`)}}},
		{Role: "user", Content: []provider.Content{{Type: "tool_result", ToolUseID: "call", Text: "match", IsError: true}}},
	}
	batches, err := SummaryBatches(messages, summaryTokens(messages))
	if err != nil || len(batches) != 1 {
		t.Fatalf("SummaryBatches() = %#v, %v", batches, err)
	}
	use := batches[0][0].Content[0]
	result := batches[0][1].Content[0]
	if use.Type != "tool_use" || use.ToolUseID != "call" || use.Name != "grep" || string(use.Input) != `{"pattern":"panic"}` {
		t.Fatalf("tool use changed: %#v", use)
	}
	if result.Type != "tool_result" || result.ToolUseID != "call" || result.Text != "match" || !result.IsError {
		t.Fatalf("tool result changed: %#v", result)
	}
}

func TestSummaryBatchesRejectOversizedStructuredUnit(t *testing.T) {
	messages := []provider.Message{{Role: "assistant", Content: []provider.Content{{Type: "tool_use", Name: "large", Input: json.RawMessage(`{"data":"` + strings.Repeat("x", 2000) + `"}`)}}}}
	batches, err := SummaryBatches(messages, 80)
	var tooLarge *SummaryUnitTooLargeError
	if batches != nil || !errors.As(err, &tooLarge) || tooLarge.ContentType != "tool_use" {
		t.Fatalf("SummaryBatches() = %#v, %T %v", batches, err, err)
	}
}

func TestSummaryBatchesSplitLargeTurnAroundToolChain(t *testing.T) {
	messages := []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat("old context ", 500)}}},
		{Role: "assistant", Content: []provider.Content{{Type: "tool_use", ToolUseID: "call", Name: "read", Input: json.RawMessage(`{}`)}}},
		{Role: "user", Content: []provider.Content{{Type: "tool_result", ToolUseID: "call", Text: "result"}}},
		{Role: "assistant", Content: []provider.Content{{Type: "text", Text: strings.Repeat("analysis ", 500)}}},
	}
	batches, err := SummaryBatches(messages, 100)
	if err != nil {
		t.Fatal(err)
	}
	var flattened []provider.Message
	for _, batch := range batches {
		if tokens := summaryTokens(batch); tokens > 100 {
			t.Fatalf("batch tokens = %d", tokens)
		}
		flattened = append(flattened, batch...)
	}
	toolUse := -1
	for index, message := range flattened {
		if messageHasToolUse(message) {
			toolUse = index
			break
		}
	}
	if toolUse < 0 || toolUse+1 >= len(flattened) || !messageHasToolResult(flattened[toolUse+1]) {
		t.Fatalf("tool chain split: %#v", flattened)
	}
}

func TestEstimateImageDoesNotCountBase64AsText(t *testing.T) {
	block := provider.Content{Type: "image", MediaType: "image/png", Data: strings.Repeat("QUFB", 50_000)}
	if tokens := estimateContent(block); tokens > 2_100 {
		t.Fatalf("image tokens = %d", tokens)
	}
}

func TestSummaryBoundaryKeepsUserTurnsAndToolChain(t *testing.T) {
	messages := []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "old user"}}},
		{Role: "assistant", Content: []provider.Content{{Type: "text", Text: "old answer"}}},
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "recent user"}}},
		{Role: "assistant", Content: []provider.Content{{Type: "tool_use", ToolUseID: "call", Name: "read", Input: json.RawMessage(`{}`)}}},
		{Role: "user", Content: []provider.Content{{Type: "tool_result", ToolUseID: "call", Text: "result"}}},
		{Role: "assistant", Content: []provider.Content{{Type: "text", Text: "recent answer"}}},
	}
	older := SummaryMessages(messages, 1, 1)
	if len(older) != 2 || older[0].Content[0].Text != "old user" {
		t.Fatalf("older = %#v", older)
	}
}

func TestEstimateUnicodeConservative(t *testing.T) {
	if EstimateText(strings.Repeat("界", 100)) < 34 {
		t.Fatalf("EstimateText() = %d", EstimateText(strings.Repeat("界", 100)))
	}
}

func BenchmarkPrepareToolHeavy(b *testing.B) {
	request := provider.Request{Model: "model"}
	for range 100 {
		request.Messages = append(request.Messages, provider.Message{Role: "user", Content: []provider.Content{{Type: "tool_result", Name: "log", Text: strings.Repeat("line\n", 1000)}}})
	}
	policy := DefaultPolicy()
	policy.ReserveTokens = 0
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Prepare(request, Limits{Default: 50_000}, policy); err != nil {
			b.Fatal(err)
		}
	}
}

func TestHybridWindowFallbacks(t *testing.T) {
	tests := map[string]int{
		"unknown-model":   128_000,
		"claude-future":   200_000,
		"cx/gpt-5.future": 200_000,
		"xai/grok-4-next": 256_000,
		"gpt-4.1-next":    1_000_000,
	}
	for model, want := range tests {
		if got := HybridWindow(model, 128_000); got != want {
			t.Fatalf("HybridWindow(%q)=%d want %d", model, got, want)
		}
	}
}

func TestWindowResolverPrecedenceAndRefresh(t *testing.T) {
	resolver := NewWindowResolver(
		map[string]int{"gpt-4": 400_000},
		map[string]int{"gpt-4.1": 1_000_000, "gpt-4.10": 900_000},
	)
	if got := resolver.Window("gpt-4.1-mini"); got != 400_000 {
		t.Fatalf("configured precedence = %d", got)
	}
	if got := resolver.Window("gpt-4.10-mini"); got != 400_000 {
		t.Fatalf("configured precedence = %d", got)
	}
	resolver.ReplaceCatalog(map[string]int{"custom": 256_000})
	if got := resolver.Window("custom-review"); got != 256_000 {
		t.Fatalf("refreshed catalog = %d", got)
	}
}

func TestWindowPrefixBoundariesAndReserves(t *testing.T) {
	limits := Limits{Default: 128_000, Models: map[string]int{"gpt-4.1": 1_000_000}}
	if got := limits.Window("gpt-4.10"); got != 128_000 {
		t.Fatalf("unsafe prefix window = %d", got)
	}
	if got := HybridWindow("gpt-50", 128_000); got != 128_000 {
		t.Fatalf("unsafe hybrid = %d", got)
	}
	if got := safetyReserve(1_000_000); got != 50_000 {
		t.Fatalf("safety reserve = %d", got)
	}
	if got := outputReserve(provider.Request{}, Policy{ReserveTokens: 2_048}, 1_000_000); got != 10_240 {
		t.Fatalf("output reserve = %d", got)
	}
}

func TestSafeModeCompactDoesNotDropUniqueToolResult(t *testing.T) {
	unique := strings.Repeat("unique diagnostic line\n", 500)
	request := provider.Request{Model: "model", Messages: []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "tool_result", Name: "unknown", Text: unique}}},
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "latest exact requirement"}}},
	}}
	policy := DefaultPolicy()
	policy.KeepRecentTurns = 1
	policy.ReserveTokens = 0
	result, err := Prepare(request, Limits{Default: 2_000}, policy)
	if err != nil && !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal(err)
	}
	got := result.Request.Messages[0].Content[0].Text
	if strings.Contains(got, "dropped older tool result") {
		t.Fatalf("unique tool result dropped: %q", got)
	}
	if result.Request.Messages[1].Content[0].Text != "latest exact requirement" {
		t.Fatal("latest instruction changed")
	}
}

func TestTrimOldestTurnsKeepsRecentTurnAndToolChain(t *testing.T) {
	messages := []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat("ancient ", 800)}}},
		{Role: "assistant", Content: []provider.Content{{Type: "tool_use", ToolUseID: "old", Name: "read", Input: []byte(`{}`)}}},
		{Role: "user", Content: []provider.Content{{Type: "tool_result", ToolUseID: "old", Text: strings.Repeat("stale ", 800)}}},
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "current question"}}},
		{Role: "assistant", Content: []provider.Content{{Type: "tool_use", ToolUseID: "new", Name: "read", Input: []byte(`{}`)}}},
		{Role: "user", Content: []provider.Content{{Type: "tool_result", ToolUseID: "new", Text: "fresh evidence"}}},
	}
	request := provider.Request{Model: "model", Messages: messages}
	trimmed, ok := TrimOldestTurns(request, 200, nil)
	if !ok {
		t.Fatalf("trim failed: %d tokens", EstimateRequest(trimmed))
	}
	encoded := ""
	for _, message := range trimmed.Messages {
		for _, block := range message.Content {
			encoded += block.Text + string(block.Input) + block.ToolUseID
		}
	}
	if strings.Contains(encoded, "ancient") || strings.Contains(encoded, "stale") {
		t.Fatalf("old turn survived: %q", encoded)
	}
	if !strings.Contains(encoded, "current question") || !strings.Contains(encoded, "fresh evidence") {
		t.Fatalf("recent turn was dropped: %q", encoded)
	}
	// A tool_result whose tool_use was trimmed away is unusable to the model.
	if strings.Contains(encoded, "old") {
		t.Fatalf("orphaned tool result left behind: %q", encoded)
	}
	if trimmed.Messages[0].Role != "user" {
		t.Fatalf("trimmed history must start on a user turn: %#v", trimmed.Messages[0])
	}
}
