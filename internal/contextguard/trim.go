package contextguard

import (
	"literouter/internal/provider"
)

const trimMarker = "[literouter:trim-v1"

// trimNotice is the block appended to System when turns were dropped.
//
// It carries no count on purpose. System content is translated into the front of the
// upstream payload — for Codex it becomes the `instructions` string, which is the
// byte range the prompt cache is keyed on — so a notice that reads "dropped_turns=7"
// and then "dropped_turns=8" moves the cacheable prefix on every trimmed turn and the
// whole system prompt is re-billed each time. Constant text costs one prefix change
// when trimming first engages and none afterwards. How much was dropped is still
// recorded, in the log line the gateway writes next to the trim.
const trimNotice = trimMarker + "] Older conversation turns were removed to fit the" +
	" context window. Ask the user to restate anything you need that is no longer visible."

// TrimOldestTurns drops whole conversation turns until the request fits budget. It is
// the deterministic alternative to summarization: dropping complete turns is lossy but
// keeps the request servable, which a hard rejection does not, and it costs milliseconds
// where an LLM summary of the same backlog costs tens of seconds. Tool chains stay intact
// because summaryUnits only splits on user turns that are not tool results, so a tool_use
// never loses its matching tool_result.
//
// The opening turn is kept for as long as possible, and that is the whole reason this is
// usable in place of a summary. A plain front-to-back trim drops it first, and the opening
// turn is where the task, the constraints and the file paths were stated — losing it is the
// most damaging single thing this function can do to answer quality, and keeping it costs
// nothing. So the middle goes first: oldest-but-one, forward.
func TrimOldestTurns(request provider.Request, budget int) (provider.Request, bool) {
	if budget <= 0 || len(request.Messages) == 0 {
		return request, false
	}
	if EstimateRequest(request) <= budget {
		return request, true
	}
	units := summaryUnits(request.Messages)
	dropped := 0
	fits := func(candidate provider.Request) bool { return EstimateRequest(candidate) <= budget }

	// Keep index 0, drop index 1 repeatedly: the middle of the conversation shrinks while
	// both ends survive. Stops with the opening turn and the latest turn still present.
	for len(units) > 2 {
		units = append(units[:1], units[2:]...)
		dropped++
		if candidate := trimmedRequest(request, units, dropped); fits(candidate) {
			return candidate, true
		}
	}
	// The opening turn plus the latest one is still too large, so the opening turn has to
	// go too. Being servable beats being well-grounded; a rejected turn is worth nothing.
	for len(units) > 1 {
		units = units[1:]
		dropped++
		if candidate := trimmedRequest(request, units, dropped); fits(candidate) {
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
	result.System = append(result.System, provider.Content{Type: "text", Text: trimNotice})
	return result
}
