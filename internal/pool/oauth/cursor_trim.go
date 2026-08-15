package oauth

// Cursor has its own trim rules, deliberately separate from the shared
// contextguard pipeline:
//
//   - The window is per-model and measured, not assumed: grok variants accepted
//     800k tokens in live probes, composer sits around 194k, and a wrong number
//     either trims history the model would have taken (window too small) or sends
//     a payload the upstream crawls on (window too large).
//   - The shared trim trims by message turns against a budget the window resolver
//     provides. Cursor folds the transcript into one flat prompt on a cache miss,
//     so trimming here means bounding that fold — tokens, not turns.
//   - Latency is the deciding factor, not the window. Measured live on
//     cursor-grok-4.6-low: 27k tokens answered in 5s, 133k in 13s, 267k in 26s,
//     534k in 60s, 801k in 110s. The upstream *accepts* 800k; it just takes two
//     minutes to answer, which makes the max window useless for interactive work.
//     The budget below is the largest context that still answers fast enough.

import (

	"literouter/internal/contextguard"
	"literouter/internal/translator"
)

// trimCursorFold bounds a cache-miss fold to the model's trim budget. The system
// preamble and the most recent messages survive; older turns are dropped. The
// conversation id is unaffected — it is derived from the full transcript in the
// caller, so two sessions that start differently still differ.
func trimCursorFold(request translator.OpenAIRequest, model string) translator.OpenAIRequest {
	budget := cursorLatencyBudget(model)
	if estimateRequestTokens(request) <= budget {
		return request
	}
	var preamble []translator.OpenAIMessage
	var tail []translator.OpenAIMessage
	for _, message := range request.Messages {
		if message.Role == "system" || message.Role == "developer" {
			preamble = append(preamble, message)
			continue
		}
		tail = append(tail, message)
	}
	// Drop oldest tail messages until the rest fits the budget. A single message
	// can be huge (a tool result), so the loop stops on the first fit rather than
	// assuming each drop is a constant saving.
	for len(tail) > 1 {
		trimmed := translator.OpenAIRequest{Model: request.Model, Messages: append(preamble, tail...)}
		if estimateRequestTokens(trimmed) <= budget {
			return trimmed
		}
		tail = tail[1:]
	}
	// Even the last message overflows alone (a giant tool result): send it anyway
	// — dropping everything would leave the model with no evidence at all.
	return translator.OpenAIRequest{Model: request.Model, Messages: append(preamble, tail...)}
}

// estimateRequestTokens estimates the token count of a request the way the shared
// pipeline does, so the budget above and the trim decision agree.
func estimateRequestTokens(request translator.OpenAIRequest) int {
	return contextguard.EstimateText(agentPromptFromRequest(request))
}
