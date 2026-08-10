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
	// One unit left. Dropping turns is out of moves, but the unit's own tool
	// output usually is the bulk of it, so keep going inside the unit.
	return trimWithinUnit(request, units[0], dropped, budget)
}

// trimWithinUnit drops the oldest tool chains of a single conversation unit.
//
// A subagent turn is always exactly one unit: summaryUnits only splits on a user
// message that is not a tool_result, and after the opening task every user
// message a subagent produces is a tool_result. So no matter how long a subagent
// runs, len(units) stays 1, the loop above never executes, and trimming used to
// report failure — which the gateway turned into a hard rejection. Every subagent
// that outgrew the budget died on this, while ordinary sessions (real user turns,
// many units) trimmed fine.
//
// The opening messages are kept unconditionally: they carry the task, and a
// request that has lost its instructions is worse than one that is merely shorter.
func trimWithinUnit(request provider.Request, unit []provider.Message, dropped, budget int) (provider.Request, bool) {
	head, chains := toolChains(unit)
	if len(chains) == 0 {
		return request, false
	}
	for len(chains) > 0 {
		chains = chains[1:]
		dropped++
		candidate := trimmedRequest(request, append([][]provider.Message{head}, chains...), dropped)
		if EstimateRequest(candidate) <= budget {
			return candidate, true
		}
	}
	return request, false
}

// toolChains splits a unit into its opening messages and the tool_use/tool_result
// chains that follow. Chains stay whole, so a tool_use never loses its matching
// tool_result — the same invariant TrimOldestTurns relies on between units.
func toolChains(unit []provider.Message) ([]provider.Message, [][]provider.Message) {
	var head []provider.Message
	var chains [][]provider.Message
	for index := 0; index < len(unit); {
		if !messageHasToolUse(unit[index]) {
			// Prose between chains rides along with whatever precedes it rather than
			// being reordered or silently dropped on its own.
			if len(chains) == 0 {
				head = append(head, unit[index])
			} else {
				chains[len(chains)-1] = append(chains[len(chains)-1], unit[index])
			}
			index++
			continue
		}
		end := index + 1
		for end < len(unit) && messageHasToolResult(unit[end]) {
			end++
		}
		chains = append(chains, cloneMessages(unit[index:end]))
		index = end
	}
	return head, chains
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
