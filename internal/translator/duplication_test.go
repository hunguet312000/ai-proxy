package translator

import "testing"

func toolResult(text string) OpenAIMessage {
	return OpenAIMessage{Role: "tool", Content: text}
}

func TestMeasurePromptDuplicationCountsOnlyEarlierExactRepeats(t *testing.T) {
	// The last occurrence is the one the model would keep, so the earlier copies are
	// the redundant ones — counting the wrong end would suggest deleting current data.
	request := OpenAIRequest{Messages: []OpenAIMessage{
		{Role: "user", Content: "read the file"},
		toolResult("package main"),
		{Role: "user", Content: "read it again"},
		toolResult("package main"),
	}}
	measured := MeasurePromptDuplication(request)
	if measured.DuplicateResults != 1 {
		t.Fatalf("duplicate results = %d, want 1", measured.DuplicateResults)
	}
	if measured.DuplicateToolBytes != len("package main") {
		t.Errorf("duplicate bytes = %d, want %d", measured.DuplicateToolBytes, len("package main"))
	}
}

func TestMeasurePromptDuplicationIgnoresChangedContent(t *testing.T) {
	// A file read twice with different content is history, not duplication. Treating
	// it as duplication would erase the fact that the file changed.
	request := OpenAIRequest{Messages: []OpenAIMessage{
		toolResult("version one"),
		toolResult("version two"),
	}}
	if measured := MeasurePromptDuplication(request); measured.DuplicateToolBytes != 0 {
		t.Fatalf("duplicate bytes = %d, want 0 for differing results", measured.DuplicateToolBytes)
	}
}

func TestMeasurePromptDuplicationIgnoresNonToolMessages(t *testing.T) {
	// Two identical user turns are the user repeating themselves, which the model is
	// meant to see.
	request := OpenAIRequest{Messages: []OpenAIMessage{
		{Role: "user", Content: "same"},
		{Role: "user", Content: "same"},
	}}
	if measured := MeasurePromptDuplication(request); measured.DuplicateToolBytes != 0 {
		t.Fatalf("duplicate bytes = %d, want 0 outside tool results", measured.DuplicateToolBytes)
	}
}

func TestMeasurePromptDuplicationReadsBlockContent(t *testing.T) {
	block := []any{map[string]any{"type": "text", "text": "chunk"}}
	request := OpenAIRequest{Messages: []OpenAIMessage{
		{Role: "tool", Content: block},
		{Role: "tool", Content: block},
	}}
	measured := MeasurePromptDuplication(request)
	if measured.DuplicateToolBytes != len("chunk") {
		t.Fatalf("duplicate bytes = %d, want %d from block content", measured.DuplicateToolBytes, len("chunk"))
	}
	if ratio := measured.Ratio(); ratio <= 0 || ratio > 1 {
		t.Errorf("ratio = %v, want a fraction", ratio)
	}
}

func TestMeasurePromptDuplicationHandlesAnEmptyRequest(t *testing.T) {
	if measured := MeasurePromptDuplication(OpenAIRequest{}); measured.Ratio() != 0 {
		t.Fatal("an empty request must not report duplication")
	}
}

func TestUsageMarksAProxySuppliedPromptCountAsUnreported(t *testing.T) {
	// A provider that reports no prompt count may supply the proxy's own measurement of
	// what it sent. That number is useful, but it is not the upstream's, so it must not
	// be presented as an authoritative figure.
	var usage OpenAIUsage
	if err := usage.UnmarshalJSON([]byte(`{"prompt_tokens":320,"prompt_tokens_estimated":true,"completion_tokens":40}`)); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if usage.PromptTokens != 320 {
		t.Errorf("prompt tokens = %d, want the supplied 320", usage.PromptTokens)
	}
	if usage.PromptTokensReported {
		t.Error("a proxy-supplied count must not be flagged as reported by the upstream")
	}
	if !usage.CompletionTokensReported {
		t.Error("the upstream's own completion count must stay flagged as reported")
	}
}

func TestUsageKeepsUpstreamCountsAuthoritative(t *testing.T) {
	var usage OpenAIUsage
	if err := usage.UnmarshalJSON([]byte(`{"prompt_tokens":100,"completion_tokens":20}`)); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !usage.PromptTokensReported || !usage.CompletionTokensReported {
		t.Fatal("counts sent without the estimated marker are the upstream's own")
	}
}
