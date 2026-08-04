package gateway

import (
	"context"
	"testing"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
	"literouter/internal/translator"
)

func TestOutputLimitMatchesExactAndPrefix(t *testing.T) {
	service := New(Options{MaxOutputTokens: map[string]int{
		"cc/small":   4096,
		"cc":         8192,
		"zero-limit": 0,
	}})

	cases := map[string]int{
		"cc/small":       4096, // exact
		"cc/small-turbo": 4096, // longest matching prefix wins over the shorter "cc"
		"cc/other":       8192,
		"ccx/other":      0, // "cc" only matches up to a separator, so "ccx" is not a hit
		"gpt-4.1":        0,
		"zero-limit":     0, // a non-positive limit is dropped, not honoured as zero
	}
	for model, want := range cases {
		if got := service.outputLimit(model); got != want {
			t.Errorf("outputLimit(%q) = %d, want %d", model, got, want)
		}
	}
	if got := New(Options{}).outputLimit("cc/small"); got != 0 {
		t.Errorf("outputLimit with no configuration = %d, want 0", got)
	}
}

func TestClampOutputOnlyLowers(t *testing.T) {
	service := New(Options{MaxOutputTokens: map[string]int{"cheap": 4096}})

	openAI := translator.OpenAIRequest{Model: "cheap", MaxTokens: 32000, MaxCompletionTokens: 32000}
	service.clampOpenAIOutput(&openAI)
	if openAI.MaxTokens != 4096 || openAI.MaxCompletionTokens != 4096 {
		t.Fatalf("max tokens = %d/%d, want 4096/4096", openAI.MaxTokens, openAI.MaxCompletionTokens)
	}

	// Already inside the cap: a clamp that raised the request would invent output
	// budget the caller never asked for.
	modest := translator.OpenAIRequest{Model: "cheap", MaxTokens: 512}
	service.clampOpenAIOutput(&modest)
	if modest.MaxTokens != 512 {
		t.Fatalf("MaxTokens = %d, want 512", modest.MaxTokens)
	}

	// Unlisted models are passed through untouched.
	untouched := translator.OpenAIRequest{Model: "expensive", MaxTokens: 32000}
	service.clampOpenAIOutput(&untouched)
	if untouched.MaxTokens != 32000 {
		t.Fatalf("MaxTokens = %d, want 32000", untouched.MaxTokens)
	}

	unified := provider.Request{Model: "cheap", MaxTokens: 32000, MaxCompletionTokens: 100}
	service.clampProviderOutput(&unified)
	if unified.MaxTokens != 4096 || unified.MaxCompletionTokens != 100 {
		t.Fatalf("unified max tokens = %d/%d, want 4096/100", unified.MaxTokens, unified.MaxCompletionTokens)
	}
}

// TestStreamClampSurvivesContextPreparation guards the ordering bug this clamp is
// easy to reintroduce: prepareStreamCandidate rebuilds the candidate from the
// unified request, so clamping before it silently restores the caller's max_tokens.
func TestStreamClampSurvivesContextPreparation(t *testing.T) {
	// ContextEnabled is what makes prepareStreamCandidate take the rebuilding branch.
	service := New(Options{
		ContextEnabled:  true,
		ContextLimits:   contextguard.Limits{Default: 128_000},
		ContextPolicy:   contextguard.Policy{SoftRatio: 0.78, SummarizeRatio: 0.88, HardRatio: 0.96, KeepRecentTurns: 6, ReserveTokens: 2048},
		MaxOutputTokens: map[string]int{"claude-cheap": 4096},
	})

	unified := provider.Request{Model: "claude-cheap", MaxTokens: 32000,
		Messages: []provider.Message{{Role: "user", Content: []provider.Content{{Type: "text", Text: "hi"}}}}}
	request, err := translator.ToOpenAIRequest(unified)
	if err != nil {
		t.Fatalf("ToOpenAIRequest: %v", err)
	}
	request.Stream = true

	candidate := request
	candidate.Model = "claude-cheap"
	prepared, err := service.prepareStreamCandidate(context.Background(), candidate, &unified)
	if err != nil {
		t.Fatalf("prepareStreamCandidate: %v", err)
	}
	if prepared.MaxTokens != 32000 {
		t.Fatalf("preparation was expected to restore max_tokens, got %d", prepared.MaxTokens)
	}
	service.clampOpenAIOutput(&prepared)
	if prepared.MaxTokens != 4096 {
		t.Fatalf("MaxTokens after clamp = %d, want 4096", prepared.MaxTokens)
	}
}
