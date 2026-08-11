package contextguard

import (
	"strings"
	"testing"

	"literouter/internal/provider"
)

// subagentTranscript is the shape Claude Code sends for a subagent: one real user
// turn carrying the task, then nothing but tool_use/tool_result pairs. It is the
// case that used to be untrimmable, because summaryUnits never opens a second unit
// for it.
func subagentTranscript(chains, resultBytes int) []provider.Message {
	messages := []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "audit the inference flow"}}},
	}
	for range chains {
		messages = append(messages,
			provider.Message{Role: "assistant", Content: []provider.Content{
				{Type: "tool_use", ToolUseID: "call", Name: "Read", Input: []byte(`{"file_path":"/a.go"}`)}}},
			provider.Message{Role: "user", Content: []provider.Content{
				{Type: "tool_result", ToolUseID: "call", Text: strings.Repeat("x", resultBytes)}}},
		)
	}
	return messages
}

func TestTrimReducesASubagentTurnThatHasNoSecondUserTurn(t *testing.T) {
	messages := subagentTranscript(40, 8192)
	if units := len(summaryUnits(messages)); units != 1 {
		t.Fatalf("units = %d; the regression only bites at exactly one unit", units)
	}
	request := provider.Request{Model: "model", Messages: messages}
	budget := EstimateRequest(request) / 4

	trimmed, ok := TrimOldestTurns(request, budget)
	if !ok {
		t.Fatal("a single-unit subagent turn must still be trimmable")
	}
	if tokens := EstimateRequest(trimmed); tokens > budget {
		t.Fatalf("trimmed to %d tokens, budget %d", tokens, budget)
	}
	// The task survives: a shorter request is recoverable, one that lost its
	// instructions is not.
	if trimmed.Messages[0].Content[0].Text != "audit the inference flow" {
		t.Fatalf("opening task was dropped: %#v", trimmed.Messages[0])
	}
}

func TestTrimKeepsToolChainsPairedWithinAUnit(t *testing.T) {
	request := provider.Request{Model: "model", Messages: subagentTranscript(30, 8192)}
	trimmed, ok := TrimOldestTurns(request, EstimateRequest(request)/5)
	if !ok {
		t.Fatal("trim failed")
	}
	// Every tool_result that survived must still have its tool_use immediately
	// before it, or the upstream rejects the turn outright.
	for index, message := range trimmed.Messages {
		if !messageHasToolResult(message) {
			continue
		}
		if index == 0 || !messageHasToolUse(trimmed.Messages[index-1]) && !messageHasToolResult(trimmed.Messages[index-1]) {
			t.Fatalf("orphaned tool_result at %d", index)
		}
	}
}

func TestTrimStillPrefersDroppingWholeTurnsWhenThereAreAny(t *testing.T) {
	var messages []provider.Message
	for range 6 {
		messages = append(messages, subagentTranscript(4, 8192)...)
	}
	request := provider.Request{Model: "model", Messages: messages}
	trimmed, ok := TrimOldestTurns(request, EstimateRequest(request)/3)
	if !ok {
		t.Fatal("multi-turn transcript must trim")
	}
	if len(trimmed.Messages) >= len(messages) {
		t.Fatalf("nothing was dropped: %d messages", len(trimmed.Messages))
	}
}

func TestTrimFallsBackToTheOpeningTurnWhenEveryChainIsOversized(t *testing.T) {
	// Two chains that each exceed the budget on their own: the only candidate that
	// can fit is the opening turn with every chain gone. Observed in production as
	// before_tokens=800137 -> after_tokens=100, before_messages=5 -> after_messages=1.
	request := provider.Request{Model: "model", Messages: subagentTranscript(2, 1_200_000)}
	budget := 251_087

	trimmed, ok := TrimOldestTurns(request, budget)
	if !ok {
		t.Fatal("the head-only candidate must be returned, not discarded")
	}
	if len(trimmed.Messages) != 1 {
		t.Fatalf("messages = %d, want only the opening turn", len(trimmed.Messages))
	}
	if tokens := EstimateRequest(trimmed); tokens > budget {
		t.Fatalf("trimmed to %d tokens, budget %d", tokens, budget)
	}
}

// The ordering trimAfterContextRejection relies on: trimming a compacted payload can
// never keep less history than trimming the original, so a compaction that fell short
// of the budget is still worth keeping as the trim base instead of being discarded.
func TestCompactThenTrimNeverKeepsLessThanTrimAlone(t *testing.T) {
	// Bulk in the recent turns, which compaction leaves byte-for-byte, plus older
	// turns it can truncate — the shape where compaction alone cannot reach budget.
	var messages []provider.Message
	for range 6 {
		messages = append(messages, subagentTranscript(2, 60_000)...)
	}
	for range 4 {
		messages = append(messages, subagentTranscript(2, 240_000)...)
	}
	request := provider.Request{Model: "model", Messages: messages}
	budget := EstimateRequest(request) * 3 / 4

	plain, okPlain := TrimOldestTurns(request, budget)
	compacted := AggressiveCompact(request, AggressivePolicy(DefaultPolicy()))
	viaCompact, okCompact := TrimOldestTurns(compacted, budget)

	if !okCompact {
		t.Fatalf("compacted payload did not fit; plain fit = %v", okPlain)
	}
	if okPlain && len(viaCompact.Messages) < len(plain.Messages) {
		t.Fatalf("compact-then-trim kept %d messages, plain trim kept %d",
			len(viaCompact.Messages), len(plain.Messages))
	}
	if tokens := EstimateRequest(viaCompact); tokens > budget {
		t.Fatalf("result is %d tokens over a %d budget", tokens, budget)
	}
}

// The gateway now skips the expensive keep-hunting probe when keep=1 yields no
// backlog. That is only sound if keep=1 is the most permissive setting there is —
// otherwise a summarizable transcript would be sent straight to the trim.
func TestKeepOneYieldsTheLargestSummaryBacklog(t *testing.T) {
	var multiTurn []provider.Message
	for range 5 {
		multiTurn = append(multiTurn, subagentTranscript(3, 512)...)
	}
	for _, messages := range [][]provider.Message{subagentTranscript(20, 512), multiTurn} {
		widest := len(SummaryMessages(messages, 1, 8))
		for keep := 2; keep <= 8; keep++ {
			if got := len(SummaryMessages(messages, keep, 8)); got > widest {
				t.Fatalf("keep=%d backlog %d exceeds keep=1 backlog %d", keep, got, widest)
			}
		}
	}
}

func TestSubagentHasNoSummaryBacklogAtAnyKeep(t *testing.T) {
	messages := subagentTranscript(40, 8192)
	for keep := 1; keep <= 8; keep++ {
		if got := len(SummaryMessages(messages, keep, 8)); got != 0 {
			t.Fatalf("keep=%d yielded %d backlog messages; summarize is supposed to be impossible here", keep, got)
		}
	}
}

// An output ask the size of the window used to leave available < 1, which made
// HardBudget report 0 and TrimOldestTurns decline on the budget alone — a hard
// rejection on a request whose history was entirely droppable.
func TestBudgetSurvivesAnOutputAskTheSizeOfTheWindow(t *testing.T) {
	limits := Limits{Default: 200_000}
	policy := DefaultPolicy()
	request := provider.Request{
		Model: "model", MaxTokens: 200_000, Messages: subagentTranscript(8, 200_000),
	}

	if budget := HardBudget(request, limits, policy); budget <= 0 {
		t.Fatalf("hard budget = %d, want a positive budget to trim against", budget)
	}
	trimmed, ok := TrimOldestTurns(request, HardBudget(request, limits, policy))
	if !ok {
		t.Fatal("a droppable history must still be trimmable under an oversized output ask")
	}
	if len(trimmed.Messages) >= len(request.Messages) {
		t.Fatalf("nothing was dropped: %d messages", len(trimmed.Messages))
	}
}

func TestOutputReserveNeverTakesMoreThanHalfTheWindow(t *testing.T) {
	policy := DefaultPolicy()
	// Asking for more output than the whole window is impossible; the upstream's own
	// cap and router.max_output_tokens handle that. Budget math must not amplify it.
	if reserve := outputReserve(provider.Request{MaxTokens: 500_000}, policy, 200_000); reserve != 100_000 {
		t.Fatalf("reserve = %d, want the window halved", reserve)
	}
	// An ordinary ask is left exactly as it was.
	want := 64_000 + policy.ReserveTokens
	if reserve := outputReserve(provider.Request{MaxTokens: 64_000}, policy, 200_000); reserve != want {
		t.Fatalf("reserve = %d, want %d unchanged", reserve, want)
	}
}

// System content becomes the front of the upstream payload — for Codex the
// `instructions` string, which is what the prompt cache is keyed on. Two trims that
// dropped different amounts must therefore produce byte-identical System content, or
// every trimmed turn re-bills the entire system prompt on top of the trim itself.
func TestTrimKeepsSystemContentIdenticalAcrossDifferentDropCounts(t *testing.T) {
	system := []provider.Content{{Type: "text", Text: "you are a coding assistant"}}
	request := provider.Request{Model: "model", System: system,
		Messages: subagentTranscript(40, 8192)}

	light, ok := TrimOldestTurns(request, EstimateRequest(request)*3/4)
	if !ok {
		t.Fatal("light trim failed")
	}
	heavy, ok := TrimOldestTurns(request, EstimateRequest(request)/8)
	if !ok {
		t.Fatal("heavy trim failed")
	}
	// Different amounts really were dropped, or the comparison below proves nothing.
	if len(light.Messages) <= len(heavy.Messages) {
		t.Fatalf("both trims kept the same amount: %d vs %d",
			len(light.Messages), len(heavy.Messages))
	}
	if len(light.System) != len(heavy.System) {
		t.Fatalf("system block count differs: %d vs %d", len(light.System), len(heavy.System))
	}
	for index := range light.System {
		if light.System[index].Text != heavy.System[index].Text {
			t.Fatalf("system block %d differs between trims:\n%q\n%q",
				index, light.System[index].Text, heavy.System[index].Text)
		}
	}
	// The notice still has to be there — the model needs to know history is missing.
	if !strings.Contains(light.System[len(light.System)-1].Text, trimMarker) {
		t.Fatalf("trim notice missing: %#v", light.System)
	}
}

func TestTrimReportsFailureWhenTheHeadAloneCannotFit(t *testing.T) {
	// No tool chains to give up, and the opening turn is never dropped.
	request := provider.Request{Model: "model", Messages: []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat("x", 40_000)}}},
	}}
	if _, ok := TrimOldestTurns(request, 100); ok {
		t.Fatal("expected failure with nothing droppable")
	}
}
