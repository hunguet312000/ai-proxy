package contextguard

import (
	"fmt"

	"literouter/internal/provider"
)

const trimMarker = "[literouter:trim-v1"

// TrimOldestTurns drops whole leading conversation turns until the request fits
// budget. It is the deterministic fallback for when summarization fails or times
// out: dropping complete turns is lossy but keeps the request servable, which a
// hard rejection does not. Tool chains stay intact because summaryUnits only
// splits on user turns that are not tool results, so a tool_use never loses its
// matching tool_result.
func TrimOldestTurns(request provider.Request, budget int) (provider.Request, bool) {
	if budget <= 0 || len(request.Messages) == 0 {
		return request, false
	}
	if EstimateRequest(request) <= budget {
		return request, true
	}
	units := summaryUnits(request.Messages)
	dropped := 0
	for len(units) > 1 {
		units = units[1:]
		dropped++
		candidate := trimmedRequest(request, units, dropped)
		if EstimateRequest(candidate) <= budget {
			return candidate, true
		}
	}
	if dropped == 0 {
		return request, false
	}
	candidate := trimmedRequest(request, units, dropped)
	return candidate, EstimateRequest(candidate) <= budget
}

func trimmedRequest(request provider.Request, units [][]provider.Message, dropped int) provider.Request {
	result := cloneRequest(request)
	messages := make([]provider.Message, 0, len(request.Messages))
	for _, unit := range units {
		messages = append(messages, unit...)
	}
	result.Messages = messages
	result.System = append(result.System, provider.Content{
		Type: "text",
		Text: fmt.Sprintf("%s dropped_turns=%d] Older conversation turns were removed to fit the context window. Ask the user to restate anything you need that is no longer visible.", trimMarker, dropped),
	})
	return result
}
