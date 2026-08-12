package contextguard

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"literouter/internal/provider"
)

func toolExchange(id, name, input, result string, isError bool) []provider.Message {
	return []provider.Message{
		{Role: "assistant", Content: []provider.Content{{Type: "tool_use", ToolUseID: id, Name: name, Input: json.RawMessage(input)}}},
		{Role: "user", Content: []provider.Content{{Type: "tool_result", ToolUseID: id, Text: result, IsError: isError}}},
	}
}

func TestStickyBoundaryAdvancesOnGrid(t *testing.T) {
	policy := Policy{KeepRecentTurns: 2, BoundaryQuantum: 4}
	// Below the first grid line the raw boundary stands (5 messages, keep 2 → 3):
	// rounding it to zero would switch off every compaction stage, which is what sent
	// short-but-huge transcripts straight to trim. See TestCompactionRunsBelowTheFirstGridLine.
	cases := []struct{ messages, want int }{
		{0, 0}, {2, 0}, {5, 3}, {6, 4}, {9, 4}, {10, 8}, {13, 8}, {14, 12},
	}
	for _, testCase := range cases {
		if got := stickyOldBoundary(testCase.messages, policy); got != testCase.want {
			t.Fatalf("stickyOldBoundary(%d) = %d, want %d", testCase.messages, got, testCase.want)
		}
	}
	// Quantum 0/1 reproduces the raw sliding boundary.
	if got := stickyOldBoundary(10, Policy{KeepRecentTurns: 2}); got != 8 {
		t.Fatalf("unquantized boundary = %d, want 8", got)
	}
}

// The shape that used to fall through the whole ladder: a subagent transcript short
// enough that the boundary grid rounded to zero, but carrying tool output far larger
// than the window. Every stage gates on `index < boundary`, so a zero boundary left
// trim — which deletes whole tool chains — as the only rung that could act.
func TestCompactionRunsBelowTheFirstGridLine(t *testing.T) {
	policy := AggressivePolicy(DefaultPolicy()) // keep 6, quantum 8: the grid line is 14 messages
	messages := []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "audit the inference flow"}}},
	}
	for index := range 6 {
		messages = append(messages, toolExchange(
			fmt.Sprintf("call-%d", index), "Bash", fmt.Sprintf(`{"command":"grep -r x dir%d"}`, index),
			strings.Repeat(fmt.Sprintf("match %d line\n", index), 20_000), false)...)
	}
	if len(messages) != 13 {
		t.Fatalf("fixture has %d messages; the grid line must stay above it", len(messages))
	}
	request := provider.Request{Model: "model", Messages: messages}
	before := EstimateRequest(request)

	boundary := stickyOldBoundary(len(messages), policy)
	if boundary == 0 {
		t.Fatal("boundary rounded to zero, so every compaction stage is switched off")
	}
	compact(&request, policy, 1, 1<<30)

	after := EstimateRequest(request)
	if after >= before*3/5 {
		t.Fatalf("compaction saved almost nothing: %d -> %d tokens", before, after)
	}
	// Old results are truncated in place — the chain itself survives, which is the whole
	// difference from trim — while everything inside the recent window stays verbatim.
	truncated, verbatim := 0, 0
	for index, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type != "tool_result" {
				continue
			}
			switch {
			case index >= boundary:
				if strings.Contains(block.Text, truncateMarker) {
					t.Fatalf("message %d is inside the recent window but was truncated", index)
				}
				verbatim++
			case !strings.Contains(block.Text, truncateMarker):
				t.Fatalf("old tool result at message %d was left at %d bytes", index, len(block.Text))
			default:
				truncated++
				// Head plus tail plus the note, not the original 260 KB.
				if len(block.Text) > policy.TruncateThresholdBytes {
					t.Fatalf("truncated result at message %d is still %d bytes", index, len(block.Text))
				}
			}
		}
	}
	if truncated != 3 || verbatim != 3 {
		t.Fatalf("truncated %d and kept %d verbatim; want 3 old and 3 recent", truncated, verbatim)
	}
	if len(request.Messages) != len(messages) {
		t.Fatalf("compaction dropped messages: %d -> %d", len(messages), len(request.Messages))
	}
}

func TestCompactPrefixStableWithinQuantum(t *testing.T) {
	policy := AggressivePolicy(Policy{
		SoftRatio: 0.78, SummarizeRatio: 0.88, HardRatio: 0.96,
		KeepRecentTurns: 2, BoundaryQuantum: 4, TruncateRatio: 0.82,
	})
	build := func(exchanges, extraTexts int) provider.Request {
		var messages []provider.Message
		for index := range exchanges {
			messages = append(messages, toolExchange(
				fmt.Sprintf("call-%d", index), "Bash", `{"command":"go test"}`,
				strings.Repeat("identical output line\n", 300), false)...)
		}
		for range extraTexts {
			messages = append(messages, provider.Message{Role: "user", Content: []provider.Content{{Type: "text", Text: "continue"}}})
		}
		return provider.Request{Model: "model", Messages: messages}
	}
	serializePrefix := func(request provider.Request, boundary int) string {
		encoded, err := json.Marshal(request.Messages[:boundary])
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}

	first := build(6, 0) // 12 messages: raw boundary 10, grid 8
	compact(&first, policy, 1, 1<<30)
	second := build(6, 1) // 13 messages appended within the quantum: raw 11, grid still 8
	compact(&second, policy, 1, 1<<30)
	boundary := stickyOldBoundary(12, policy)
	if boundary != stickyOldBoundary(13, policy) {
		t.Fatalf("boundary moved inside the quantum: %d vs %d", boundary, stickyOldBoundary(13, policy))
	}
	if serializePrefix(first, boundary) != serializePrefix(second, boundary) {
		t.Fatal("compacted prefix changed while the boundary was stable")
	}
}

func TestCompactIdempotentOnOwnOutput(t *testing.T) {
	policy := AggressivePolicy(DefaultPolicy())
	policy.KeepRecentTurns = 1
	policy.BoundaryQuantum = 1
	var messages []provider.Message
	messages = append(messages, toolExchange("a", "Read", `{"file":"a.go"}`, strings.Repeat("body line\n", 800), false)...)
	messages = append(messages, toolExchange("b", "Read", `{"file":"a.go"}`, strings.Repeat("body line v2\n", 800), false)...)
	messages = append(messages, provider.Message{Role: "user", Content: []provider.Content{{Type: "text", Text: "continue"}}})
	request := provider.Request{Model: "model", Messages: messages}
	compact(&request, policy, 1, 1<<30)
	once, err := json.Marshal(request.Messages)
	if err != nil {
		t.Fatal(err)
	}
	compact(&request, policy, 1, 1<<30)
	twice, err := json.Marshal(request.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Fatal("second compact pass changed already-compacted output")
	}
}

func TestCollapseSupersededKeepsNewestAndSkipsErrors(t *testing.T) {
	var messages []provider.Message
	messages = append(messages, toolExchange("old", "Read", `{"file":"a.go"}`, "stale content", false)...)
	messages = append(messages, toolExchange("failed", "Read", `{"file":"a.go"}`, "permission denied", true)...)
	messages = append(messages, toolExchange("new", "Read", `{"file":"a.go"}`, "current content", false)...)
	request := provider.Request{Model: "model", Messages: messages}
	collapseSuperseded(&request, len(messages))

	if text := request.Messages[1].Content[0].Text; !strings.HasPrefix(text, supersedeMarker) || !strings.Contains(text, "tool=Read") {
		t.Fatalf("stale result not collapsed: %q", text)
	}
	// An old error followed by a successful retry is real signal about what was fixed.
	if text := request.Messages[3].Content[0].Text; text != "permission denied" {
		t.Fatalf("error result was touched: %q", text)
	}
	if text := request.Messages[5].Content[0].Text; text != "current content" {
		t.Fatalf("newest result was touched: %q", text)
	}
}

func TestCollapseSupersededRequiresIdenticalInput(t *testing.T) {
	var messages []provider.Message
	messages = append(messages, toolExchange("one", "Read", `{"file":"a.go","offset":0}`, "first slice", false)...)
	messages = append(messages, toolExchange("two", "Read", `{"file":"a.go","offset":100}`, "second slice", false)...)
	request := provider.Request{Model: "model", Messages: messages}
	collapseSuperseded(&request, len(messages))
	if request.Messages[1].Content[0].Text != "first slice" || request.Messages[3].Content[0].Text != "second slice" {
		t.Fatalf("different arguments were treated as superseded: %#v", request.Messages)
	}
}

func TestTruncateKeepsHeadTailAndMarker(t *testing.T) {
	var lines []string
	for index := range 500 {
		lines = append(lines, fmt.Sprintf("src/pkg/file.go:%d: diagnostic detail", index))
	}
	text := strings.Join(lines, "\n")
	policy := Policy{}
	replacement, ok := truncateToolResult(resultSource{name: "mcp__custom__analyze"}, "use-1", text, false, policy)
	if !ok {
		t.Fatalf("large unique result was not truncated (%d bytes)", len(text))
	}
	if !strings.HasPrefix(replacement, truncateMarker) || !strings.Contains(replacement, "tool_use_id=use-1") {
		t.Fatalf("marker missing: %q", replacement[:120])
	}
	if !strings.Contains(replacement, "src/pkg/file.go:0:") || !strings.Contains(replacement, "src/pkg/file.go:499:") {
		t.Fatal("head or tail lines lost")
	}
	// Naming the call is the point: "re-run the tool" left the model to work out which
	// one from a tool_use_id, which is not something a model acts on.
	if !strings.Contains(replacement, "re-run mcp__custom__analyze") {
		t.Fatalf("recovery hint does not name the tool: %q", replacement)
	}
	if len(replacement) >= len(text) {
		t.Fatalf("truncation did not shrink: %d >= %d", len(replacement), len(text))
	}
}

func TestTruncateKeepsErrorResultsIntact(t *testing.T) {
	text := strings.Repeat("panic: goroutine stack trace line\n", 300) // ~10KB, under the 16KB error allowance
	if _, ok := truncateToolResult(resultSource{name: "Bash"}, "use-err", text, true, Policy{}); ok {
		t.Fatal("error result under the allowance was truncated")
	}
	huge := strings.Repeat("panic: goroutine stack trace line\n", 1000)
	replacement, ok := truncateToolResult(resultSource{name: "Bash"}, "use-err", huge, true, Policy{})
	if !ok {
		t.Fatal("oversized error result was not truncated")
	}
	if !strings.HasPrefix(replacement, truncateMarker) {
		t.Fatalf("marker missing: %q", replacement[:80])
	}
}

func TestTruncateGateNotCrossedLeavesUniqueResults(t *testing.T) {
	policy := AggressivePolicy(DefaultPolicy())
	policy.KeepRecentTurns = 1
	policy.BoundaryQuantum = 1
	unique := strings.Repeat("unique diagnostic line\n", 400)
	var messages []provider.Message
	messages = append(messages, toolExchange("a", "Bash", `{"command":"make"}`, unique, false)...)
	messages = append(messages, provider.Message{Role: "user", Content: []provider.Content{{Type: "text", Text: "continue"}}})
	request := provider.Request{Model: "model", Messages: messages}
	// beforeTokens below the truncate gate: supersede may run, truncation must not.
	compact(&request, policy, 100_000, 100)
	if request.Messages[1].Content[0].Text != unique {
		t.Fatalf("unique result truncated below the gate: %q", request.Messages[1].Content[0].Text[:80])
	}
}

func TestAggressiveCompactShrinksWithoutDroppingTurns(t *testing.T) {
	var messages []provider.Message
	for index := range 8 {
		messages = append(messages, toolExchange(
			fmt.Sprintf("call-%d", index), "Read", `{"file":"main.go"}`,
			strings.Repeat(fmt.Sprintf("content v%d\n", index), 600), false)...)
	}
	messages = append(messages, provider.Message{Role: "user", Content: []provider.Content{{Type: "text", Text: "latest instruction"}}})
	request := provider.Request{Model: "model", Messages: messages}
	before := EstimateRequest(request)
	compacted := AggressiveCompact(request, DefaultPolicy())
	after := EstimateRequest(compacted)
	if after >= before {
		t.Fatalf("AggressiveCompact saved nothing: %d -> %d", before, after)
	}
	if len(compacted.Messages) != len(request.Messages) {
		t.Fatalf("messages dropped: %d -> %d", len(request.Messages), len(compacted.Messages))
	}
	if last := compacted.Messages[len(compacted.Messages)-1].Content[0].Text; last != "latest instruction" {
		t.Fatalf("latest instruction changed: %q", last)
	}
	// The input is untouched; only the clone is rewritten.
	if request.Messages[1].Content[0].Text == compacted.Messages[1].Content[0].Text {
		t.Fatal("expected the oldest superseded result to be rewritten in the clone")
	}
}

func TestProxyMarkersNeverReprocessed(t *testing.T) {
	summaryText := summaryMarker + " earlier turns summarized"
	messages := []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "tool_result", ToolUseID: "x", Text: summaryText}}},
		{Role: "user", Content: []provider.Content{{Type: "tool_result", ToolUseID: "y", Text: summaryText}}},
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "continue"}}},
	}
	request := provider.Request{Model: "model", Messages: messages}
	policy := AggressivePolicy(DefaultPolicy())
	policy.KeepRecentTurns = 1
	policy.BoundaryQuantum = 1
	compact(&request, policy, 1, 1<<30)
	if request.Messages[0].Content[0].Text != summaryText || request.Messages[1].Content[0].Text != summaryText {
		t.Fatalf("proxy marker block was reprocessed: %#v", request.Messages)
	}
}

func TestCheckAppliesEstimateScaleBothDirections(t *testing.T) {
	request := provider.Request{Model: "model", Messages: []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat("word ", 300)}}},
	}}
	limits := Limits{Default: 1_000}
	base := Policy{SoftRatio: 0.78, SummarizeRatio: 0.88, HardRatio: 0.96, KeepRecentTurns: 6}

	neutral, err := Check(request, limits, base)
	if err != nil || neutral.EstimatedOverage {
		t.Fatalf("neutral scale flagged overage: %#v, %v", neutral, err)
	}
	dense := base
	dense.EstimateScale = 0.4 // estimates run low on dense text: shrink the budget
	flagged, err := Check(request, limits, dense)
	if err != nil || !flagged.EstimatedOverage {
		t.Fatalf("low scale did not flag overage: %#v, %v", flagged, err)
	}
	if flagged.SafeLimit >= neutral.SafeLimit {
		t.Fatalf("scaled safe limit did not shrink: %d >= %d", flagged.SafeLimit, neutral.SafeLimit)
	}
	prose := base
	prose.EstimateScale = 1.5
	relaxed, err := Check(request, limits, prose)
	if err != nil || relaxed.SafeLimit <= neutral.SafeLimit {
		t.Fatalf("high scale did not grow the budget: %#v, %v", relaxed, err)
	}
}

func TestSummaryBoundaryQuantizedIsStableAndLandsOnTurnStart(t *testing.T) {
	build := func(turns int) []provider.Message {
		var messages []provider.Message
		for range turns {
			messages = append(messages,
				provider.Message{Role: "user", Content: []provider.Content{{Type: "text", Text: "ask"}}},
				provider.Message{Role: "assistant", Content: []provider.Content{{Type: "text", Text: "answer"}}},
			)
		}
		return messages
	}
	first := summaryBoundary(build(12), 2, 6)
	if first == 0 {
		t.Fatal("expected a non-zero quantized boundary")
	}
	if message := build(12)[first]; message.Role != "user" || messageHasToolResult(message) {
		t.Fatalf("boundary %d does not land on a turn start", first)
	}
	// One more turn appended: the raw boundary moved by two messages, the grid did not.
	second := summaryBoundary(build(13), 2, 6)
	if first != second {
		t.Fatalf("quantized summary boundary churned: %d -> %d", first, second)
	}
	keyFirst, err := SummaryKey("model", 1000, SummaryMessages(build(12), 2, 6))
	if err != nil {
		t.Fatal(err)
	}
	keySecond, err := SummaryKey("model", 1000, SummaryMessages(build(13), 2, 6))
	if err != nil {
		t.Fatal(err)
	}
	if keyFirst != keySecond {
		t.Fatal("summary cache key churned inside the quantum")
	}
}

func TestPolicyValidateNewFields(t *testing.T) {
	valid := AggressivePolicy(DefaultPolicy())
	if err := valid.Validate(); err != nil {
		t.Fatalf("default aggressive policy invalid: %v", err)
	}
	badRatio := valid
	badRatio.TruncateRatio = 0.5 // below soft
	if badRatio.Validate() == nil {
		t.Fatal("truncate ratio below soft accepted")
	}
	badSizes := valid
	badSizes.TruncateHeadBytes = 4_000
	badSizes.TruncateTailBytes = 4_000
	badSizes.TruncateThresholdBytes = 4_096
	if badSizes.Validate() == nil {
		t.Fatal("head+tail over threshold accepted")
	}
	badScale := valid
	badScale.EstimateScale = 10
	if badScale.Validate() == nil {
		t.Fatal("estimate scale out of range accepted")
	}
}
