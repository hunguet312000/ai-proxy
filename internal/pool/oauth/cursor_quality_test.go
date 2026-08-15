package oauth

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"literouter/internal/translator"
)

// TestCursorBudgetQualityComparison measures the quality trade-off at different
// trim budgets. It builds a ~150k-token transcript with three facts planted at
// the start, middle and end, then asks the model to recall each one. Running
// the same transcript at 25k / 50k / 100k budgets shows how much context loss
// actually hurts, and at what latency cost.
//
// Skipped under -short: it needs the live Cursor CLI and makes 3 model calls
// per budget.
func TestCursorBudgetQualityComparison(t *testing.T) {
	if testing.Short() {
		t.Skip("live Cursor CLI test")
	}
	if !cursorACPAvailable() {
		t.Skip("cursor CLI agent not available")
	}

	// The planted facts. Each is unique and hard to guess.
	factEarly := "THE SECRET PORT IS 47291"
	factMiddle := "THE SECRET COLOR IS CERULEAN"
	factLate := "THE SECRET ANIMAL IS OKAPI"

	// Build a ~150k-token transcript: filler turns with the facts planted at the
	// start, middle and end.
	var messages []translator.OpenAIMessage
	messages = append(messages, translator.OpenAIMessage{
		Role: "system", Content: "You are a helpful assistant. Answer the final question using ONLY what is stated in the conversation history. If the information is not present, say NOT_IN_CONTEXT.",
	})
	plant := func(text string) {
		messages = append(messages, translator.OpenAIMessage{Role: "user", Content: text})
		messages = append(messages, translator.OpenAIMessage{Role: "assistant", Content: "Understood, I recorded that. " + strings.Repeat("confirmed ", 5)})
	}
	// Filler density: each user turn ~2.4k tokens, assistant ~0.7k. 110 turns
	// each side ≈ 200k+ tokens total.
	filler := func(i int) string {
		return fmt.Sprintf("Task %d: implement feature %d with details. ", i, i) +
			strings.Repeat("filler file content and diff lines describing the implementation ", 40)
	}
	assistantFiller := func(i int) string {
		return fmt.Sprintf("Completed task %d with changes to files. ", i) +
			strings.Repeat("code changes ", 25)
	}
	plant(factEarly) // very early
	for i := 0; i < 52; i++ {
		messages = append(messages,
			translator.OpenAIMessage{Role: "user", Content: filler(i)},
			translator.OpenAIMessage{Role: "assistant", Content: assistantFiller(i)},
		)
	}
	plant(factMiddle) // middle
	for i := 52; i < 108; i++ {
		messages = append(messages,
			translator.OpenAIMessage{Role: "user", Content: filler(i)},
			translator.OpenAIMessage{Role: "assistant", Content: assistantFiller(i)},
		)
	}
	plant(factLate) // very late

	request := translator.OpenAIRequest{Model: "composer-2.5", Messages: messages}
	totalTokens := estimateRequestTokens(request)
	t.Logf("transcript: %d tokens, %d messages", totalTokens, len(messages))

	// The three questions, each needing a fact at a different depth.
	questions := []struct {
		name string
		q    string
		want string
	}{
		{"early", "What is THE SECRET PORT? Reply with the number only.", "47291"},
		{"middle", "What is THE SECRET COLOR? Reply with the color only.", "CERULEAN"},
		{"late", "What is THE SECRET ANIMAL? Reply with the animal only.", "OKAPI"},
	}

	budgets := []int{25_000, 50_000, 100_000}
	for _, budget := range budgets {
		t.Run(fmt.Sprintf("budget-%dk", budget/1000), func(t *testing.T) {
			trimmed := trimCursorFoldToBudget(request, budget)
			trimmedTokens := estimateRequestTokens(trimmed)
			t.Logf("  trimmed to %d tokens (%d%% of %d), %d messages",
				trimmedTokens, trimmedTokens*100/totalTokens, totalTokens, len(trimmed.Messages))

			// One conversation per budget so pool reuse is fair within it.
			conv := fmt.Sprintf("quality-%d", budget)
			ctx := context.Background()
			// Warm the pool once.
			warm, err := runCursorACPTurn(ctx, conv, ".", "Reply with exactly: WARM", "composer-2.5")
			if err != nil {
				t.Fatalf("warm: %v", err)
			}
			_, _ = collectOpenAIStream(warm, "composer-2.5")
			warm.Close()

			for _, q := range questions {
				prompt := acpPromptFromRequest(trimmed) + "\n\n" + q.q
				start := time.Now()
				reader, err := runCursorACPTurn(ctx, conv, ".", prompt, "composer-2.5")
				if err != nil {
					t.Fatalf("%s: %v", q.name, err)
				}
				resp, err := collectOpenAIStream(reader, "composer-2.5")
				reader.Close()
				if err != nil {
					t.Fatalf("%s stream: %v", q.name, err)
				}
				content, _ := resp.Choices[0].Message.Content.(string)
				elapsed := time.Since(start).Round(time.Millisecond)

				ok := strings.Contains(strings.ToUpper(content), strings.ToUpper(q.want))
				status := "OK"
				if !ok {
					status = "MISS"
				}
				t.Logf("  [%s] %-7s: %s (%s)", status, q.name, strings.TrimSpace(content), elapsed)
				if q.name == "late" && !ok {
					t.Logf("    late fact should be present at every budget — check whether trim dropped it")
				}
			}
		})
	}
}
