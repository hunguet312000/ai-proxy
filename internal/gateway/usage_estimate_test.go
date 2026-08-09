package gateway

import (
	"testing"

	"literouter/internal/contextguard"
	"literouter/internal/translator"
)

// estimateSides mirrors the two independent estimates the recording paths make, so the
// rule can be exercised without standing up a whole upstream.
func estimateSides(usage translator.OpenAIUsage, request translator.OpenAIRequest, answer string) (int, bool, int, bool) {
	promptEst, completionEst := false, false
	if usage.PromptTokens == 0 {
		if unified, err := translator.FromOpenAIRequest(request); err == nil {
			usage.PromptTokens = contextguard.EstimateRequest(unified)
			promptEst = true
		}
	}
	if usage.CompletionTokens == 0 && answer != "" {
		usage.CompletionTokens = contextguard.EstimateText(answer)
		completionEst = true
	}
	return usage.PromptTokens, promptEst, usage.CompletionTokens, completionEst
}

func requestWithText(text string) translator.OpenAIRequest {
	return translator.OpenAIRequest{
		Model:     "cursor/composer-2.5-fast",
		MaxTokens: 64,
		Messages:  []translator.OpenAIMessage{{Role: "user", Content: text}},
	}
}

func TestUsageEstimatesPromptEvenWhenCompletionIsReported(t *testing.T) {
	// Cursor's agent stream reports completion tokens and no prompt count. Requiring
	// both to be missing before estimating recorded a hard zero for input, which reads
	// as a free request rather than an unreported one.
	usage := translator.OpenAIUsage{CompletionTokens: 20, CompletionTokensReported: true}
	prompt, promptEst, completion, completionEst := estimateSides(usage, requestWithText("a fairly long question about the codebase"), "pong")

	if prompt <= 0 || !promptEst {
		t.Fatalf("prompt = %d estimated=%v, want a positive estimate", prompt, promptEst)
	}
	if completion != 20 || completionEst {
		t.Errorf("completion = %d estimated=%v, want the reported 20 left untouched", completion, completionEst)
	}
}

func TestUsageKeepsReportedPromptAndEstimatesMissingCompletion(t *testing.T) {
	usage := translator.OpenAIUsage{PromptTokens: 1234, PromptTokensReported: true}
	prompt, promptEst, completion, completionEst := estimateSides(usage, requestWithText("hi"), "a generated answer")

	if prompt != 1234 || promptEst {
		t.Errorf("prompt = %d estimated=%v, want the reported 1234 left untouched", prompt, promptEst)
	}
	if completion <= 0 || !completionEst {
		t.Errorf("completion = %d estimated=%v, want a positive estimate", completion, completionEst)
	}
}

func TestUsageLeavesFullyReportedUsageAlone(t *testing.T) {
	usage := translator.OpenAIUsage{
		PromptTokens: 900, PromptTokensReported: true,
		CompletionTokens: 100, CompletionTokensReported: true,
	}
	prompt, promptEst, completion, completionEst := estimateSides(usage, requestWithText("hi"), "answer")
	if prompt != 900 || completion != 100 || promptEst || completionEst {
		t.Fatalf("usage = %d/%d estimated=%v/%v, want it untouched", prompt, completion, promptEst, completionEst)
	}
}
