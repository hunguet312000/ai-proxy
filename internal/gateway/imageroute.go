package gateway

import (
	"errors"
	"strings"

	"literouter/internal/contextguard"
	"literouter/internal/translator"
)

// errImageModelUnavailable marks a turn that carries an image bound for a model declared
// unable to read one, with no vision model configured to take it instead.
//
// Reported rather than forwarded. The upstream would refuse it anyway, after the whole
// conversation had been uploaded, with wording that never mentions the attachment — so the
// user learns nothing and pays for the lesson. This message names the cause and the fix.
var errImageModelUnavailable = errors.New(
	"this request contains an image but the routed model is configured as text-only; " +
		"set an image model in LiteRouter's routing settings, or remove the attachment")

// A text-only model handed a screenshot fails in the least useful way available: the
// upstream rejects the turn with wording of its own choosing, the client sees a generic
// error, and nothing in the transcript says the cause was an attachment. Claude Code
// pastes images freely — every screenshot, every diagram — so on a text-only default this
// is not an edge case, it is every turn the user drags a picture into.
//
// claude-code-router routes on the presence of an image alone. That is wrong here, and
// measurably so: it would move a plan turn carrying a screenshot off a strong multimodal
// model onto whatever cheap vision model was configured, downgrading the plan for no
// reason. The presence of an image says nothing about whether the current model minds.
//
// What matters is the pair: an image in the request AND a serving model that cannot read
// it. The second half cannot be inferred — vendors publish it inconsistently and model ids
// carry no marker — so it is named. Naming a model text-only is a small, one-time act by
// the person who already knows, and it is the only input that makes this decision correct
// rather than a guess.

// imageRoute is the whole rule: where image turns go, and which models cannot take them.
type imageRoute struct {
	model string
	// textOnly holds the ids as given, matched by the same prefix rules the rest of the
	// package uses, so naming "fpt-ai" covers every model that provider serves.
	textOnly []string
}

// ImageRoute reports the vision model and the models declared text-only.
func (s *Service) ImageRoute() (string, []string) {
	route := s.imageRoute.Load()
	if route == nil {
		return "", nil
	}
	return route.model, append([]string(nil), route.textOnly...)
}

// SetImageRoute changes the rule on a running gateway. Either half is useful alone: a
// vision model with nothing declared text-only never fires, and models declared text-only
// with no vision model turn an opaque upstream rejection into a clear refusal.
func (s *Service) SetImageRoute(model string, textOnly []string) {
	cleaned := make([]string, 0, len(textOnly))
	for _, id := range textOnly {
		if id = strings.TrimSpace(id); id != "" {
			cleaned = append(cleaned, id)
		}
	}
	s.imageRoute.Store(&imageRoute{model: strings.TrimSpace(model), textOnly: cleaned})
}

// SetBuildImagePrompt turns image→text transcription on or off. When on, a text-only
// serving model gets every image in its prompt transcribed to text by the vision model
// instead of the turn being rerouted to the vision model or the image stripped.
func (s *Service) SetBuildImagePrompt(enabled bool) {
	if s == nil {
		return
	}
	s.buildImagePrompt.Store(&enabled)
}

// BuildImagePrompt reports whether image→text transcription is enabled.
func (s *Service) BuildImagePrompt() bool {
	if s == nil {
		return false
	}
	if value := s.buildImagePrompt.Load(); value != nil {
		return *value
	}
	return false
}

// requestHasImage reports whether any message carries an image block.
//
// Walked on the parsed request rather than sniffed out of the raw bytes. Byte sniffing
// would be cheaper but it cannot tell an image block from the string `"type":"image"`
// appearing inside a tool result or a code snippet, and a false positive here reroutes a
// turn that had no picture in it.
func requestHasImage(request translator.AnthropicRequest) bool {
	return anyImage(request)
}

// freshImage reports an image in the turn being asked now — the final message, or the system
// block, which stands for every turn.
//
// Freshness is the whole basis of the routing decision, and measuring it was worth the
// trouble. An image stays in the transcript forever because the client resends the history
// every turn, so "does this request contain an image" was true for the rest of the session
// after a single screenshot: the expensive vision model served every later turn, and the
// image's base64 was paid for again on each one. Routing on the newest turn instead means a
// picture costs the vision model once.
func freshImage(request translator.AnthropicRequest) bool {
	for _, block := range request.System {
		if block.Type == "image" {
			return true
		}
	}
	if len(request.Messages) == 0 {
		return false
	}
	final := request.Messages[len(request.Messages)-1]
	return contentHasImage(final.Content)
}

// freshAttachment reports whether the fresh image was put there by the caller rather than
// returned by a tool. An attachment is the request, so refusing it is honest; tool output is
// incidental and killing the turn over it is not.
func freshAttachment(request translator.AnthropicRequest) bool {
	for _, block := range request.System {
		if block.Type == "image" {
			return true
		}
	}
	if len(request.Messages) == 0 {
		return false
	}
	for _, block := range request.Messages[len(request.Messages)-1].Content {
		if block.Type == "image" {
			return true
		}
	}
	return false
}

// anyImage reports an image anywhere in the request, fresh or long past.
func anyImage(request translator.AnthropicRequest) bool {
	for _, message := range request.Messages {
		if contentHasImage(message.Content) {
			return true
		}
	}
	return contentHasImage(request.System)
}

// toolResultImages counts the images nested inside tool results.
func toolResultImages(request translator.AnthropicRequest) int {
	count := 0
	for _, message := range request.Messages {
		for _, block := range message.Content {
			count += len(nestedImageIndexes(block))
		}
	}
	return count
}

// nestedImageIndexes reports where the image entries sit inside a tool result's content.
func nestedImageIndexes(block translator.AnthropicContent) []int {
	if block.Type != "tool_result" {
		return nil
	}
	nested, ok := block.Content.([]any)
	if !ok {
		return nil
	}
	var indexes []int
	for index, item := range nested {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := entry["type"].(string); kind == "image" {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

// stripSystemImages replaces every image in a system block list with imagePlaceholder,
// reporting whether anything changed (so callers can leave the slice untouched when not).
func stripSystemImages(system []translator.AnthropicContent) ([]translator.AnthropicContent, bool) {
	changed := false
	cleaned := make([]translator.AnthropicContent, len(system))
	copy(cleaned, system)
	for index, block := range system {
		if block.Type == "image" {
			cleaned[index] = translator.AnthropicContent{Type: "text", Text: imagePlaceholder}
			changed = true
		}
	}
	if !changed {
		return system, false
	}
	return cleaned, true
}

// imagePlaceholder replaces an image a text-only model cannot be shown.
//
// Explicit rather than silent, and worded so the model reports the gap instead of guessing
// around it. A dropped image with no trace is what produced confident answers about pictures
// that were never seen; a model told plainly that it cannot see one says so.
const imagePlaceholder = "[an image was here. The model serving this turn cannot read images, so it was " +
	"omitted. If it was described earlier in this conversation, rely on that description; otherwise say it " +
	"could not be viewed. Do not guess at its contents.]"

// stripUnreadableImages replaces every image in the request with imagePlaceholder and reports
// how many it replaced, covering both blocks the caller attached and images nested in tool
// results. A screenshot pasted twenty turns ago is exactly as unreadable to a text-only model
// as one a tool just returned, and leaving it in place costs its base64 on every turn.
//
// The touched messages are copied rather than edited in place: the caller's request shares
// its slices with the passthrough path, which forwards the caller's original bytes.
func stripUnreadableImages(request translator.AnthropicRequest) (translator.AnthropicRequest, int) {
	stripped := 0
	// System blocks can carry an image too (a screenshot the client embedded in the
	// preamble). anyImage/freshImage already look there; the pass that removes them
	// must as well, or a text-only model is handed an image_url it cannot read.
	if system, changed := stripSystemImages(request.System); changed {
		request.System = system
		for _, block := range request.System {
			if block.Type == "image" {
				stripped++
			}
		}
	}
	messages := make([]translator.AnthropicMessage, len(request.Messages))
	copy(messages, request.Messages)
	for messageIndex, message := range messages {
		var content []translator.AnthropicContent
		ensure := func() {
			if content == nil {
				content = make([]translator.AnthropicContent, len(message.Content))
				copy(content, message.Content)
			}
		}
		for blockIndex, block := range message.Content {
			if block.Type == "image" {
				ensure()
				content[blockIndex] = translator.AnthropicContent{Type: "text", Text: imagePlaceholder}
				stripped++
				continue
			}
			indexes := nestedImageIndexes(block)
			if len(indexes) == 0 {
				continue
			}
			ensure()
			nested, _ := block.Content.([]any)
			replaced := make([]any, len(nested))
			copy(replaced, nested)
			for _, at := range indexes {
				replaced[at] = map[string]any{"type": "text", "text": imagePlaceholder}
				stripped++
			}
			block.Content = replaced
			content[blockIndex] = block
		}
		if content != nil {
			messages[messageIndex].Content = content
		}
	}
	if stripped == 0 {
		return request, 0
	}
	request.Messages = messages
	return request, stripped
}

// contentHasImage checks a block list, looking inside tool results as well as at the blocks
// themselves.
//
// The nested case is the common one, not the exotic one: an image reaches a transcript when
// Read opens an image file or an MCP tool returns a screenshot, and both arrive nested in a
// tool result. Checking only top-level blocks missed every one of them, which would have
// left this rule firing for pasted images and silently not firing for tool output.
func contentHasImage(blocks []translator.AnthropicContent) bool {
	for _, block := range blocks {
		switch block.Type {
		case "image":
			return true
		case "tool_result":
			nested, ok := block.Content.([]any)
			if !ok {
				continue
			}
			for _, item := range nested {
				entry, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if kind, _ := entry["type"].(string); kind == "image" {
					return true
				}
			}
		}
	}
	return false
}

// isTextOnly reports whether a model has been declared unable to read images.
//
// Every Cursor agent model (composer, grok-*) is text-only by construction on this
// proxy: the agent path folds the request into a single flat text prompt and a
// base64 data URI in that text is not decoded by the service — measured live, a
// transcription through a Cursor model cogitates and hangs. They are treated as
// text-only regardless of the declared list so an image turn is always transcribed
// first (through the vision model) instead of reaching a Cursor model raw.
func (route *imageRoute) isTextOnly(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(lower, "cursor/") || strings.HasPrefix(lower, "cu/") {
		return true
	}
	for _, id := range route.textOnly {
		if contextguard.LookupByModel(map[string]int{id: 1}, model) == 1 {
			return true
		}
	}
	return false
}

// imageCapabilityFix corrects a routing decision that would send an image to a model
// declared unable to read it.
//
// With a vision model configured the turn simply goes there. Without one the answer depends
// on where the image came from, because the two cases fail differently:
//
//   - An attachment is the request. Refusing names the cause and the fix, and the user is
//     right there to act on it — far better than an upload the upstream will reject with
//     wording that never mentions the picture.
//   - Tool output is incidental. A subagent that read one image file among fifty should not
//     die of it, so the image is replaced with an explicit placeholder and the turn
//     continues. Degraded and honest beats dead.
func (s *Service) imageCapabilityFix(servingModel string, request translator.AnthropicRequest) (string, bool, error) {
	route := s.imageRoute.Load()
	if route == nil || len(route.textOnly) == 0 || !route.isTextOnly(servingModel) {
		return "", false, nil
	}
	if !freshImage(request) {
		// Nothing is being asked about a picture right now. Any image still in the transcript
		// is unreadable here and costs its base64 on every turn, so it goes — and the session
		// stays on the model it was configured to run on. What the vision model said about it
		// earlier is still in the history as text, which is the part a text model can use.
		if anyImage(request) {
			return "", true, nil
		}
		return "", false, nil
	}
	// Declaring the vision model text-only is contradictory, and following it would bounce
	// the turn straight back here. Treat the explicit vision role as the stronger signal.
	if route.model != "" && !strings.EqualFold(route.model, servingModel) {
		return route.model, false, nil
	}
	if route.model != "" {
		return "", false, nil
	}
	if freshAttachment(request) {
		return "", false, errImageModelUnavailable
	}
	return "", true, nil
}
