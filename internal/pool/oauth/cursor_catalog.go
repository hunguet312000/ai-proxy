package oauth

// Cursor's model catalog is separate from the shared one because Cursor is a
// different animal:
//
//   - The model list is not static — it comes from GetUsableModels per account,
//     and the ids (cursor-grok-4.6-low, composer-2.5) do not match any public
//     naming convention.
//   - The window is a latency budget, not a maximum. Live measurements on
//     cursor-grok-4.6-low: 27k tokens answered in 5s, 133k in 13s, 267k in 26s,
//     534k in 60s, 801k in 110s. The upstream accepts 800k; answering in under
//     5s means keeping the fold around 25-30k tokens. A shared catalog row that
//     says "800k window" is actively harmful here: it invites sending the whole
//     session and waiting two minutes.
//   - The shared context pipeline (summarize/trim against a window) does not
//     apply: Cursor folds the transcript into one flat prompt, and the fold is
//     bounded here by the per-model budget below.
//
// This is why cursorTrimBudget lives here rather than in the shared window
// resolver.

import (
	"strings"
)

// cursorLatencyBudget returns the token budget a cache-miss fold may send, chosen
// so the upstream answers in interactive time (measured ~5s per 25k tokens on
// grok-4.6-low; the budget scales the other way: smaller fold → faster answer).
//
// These are the defaults. The dashboard can override per model via the Cursor
// catalog, but the defaults are what make a fresh setup fast without measuring.
func cursorLatencyBudget(model string) int {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "xhigh"):
		// Highest reasoning tier: give it more room to think, accept ~8-10s.
		return 60_000
	case strings.Contains(lower, "high"):
		return 45_000
	case strings.Contains(lower, "medium"):
		return 35_000
	case strings.Contains(lower, "low"):
		// Measured: 27k → 5s. 30k keeps a useful tail of history at ~5-6s.
		return 30_000
	case strings.Contains(lower, "composer"):
		return 25_000
	default:
		return 30_000
	}
}
