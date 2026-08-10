package gateway

import (
	"log/slog"
	"strings"

	"literouter/internal/translator"
)

// Claude Code's /compact and auto-compact re-send the entire conversation to the
// session model with a fixed summarization instruction as the final user message.
// On a large session that is the slowest request the client ever makes — ~180k
// tokens into a max-effort reasoning model took ~5 minutes — and nothing about
// the task needs that model. Detecting the request lets the operator route it to
// a fast model instead.
//
// Detection requires all three signals, ANDed:
//
//   - no tools: every real Claude Code coding turn carries the tool schemas,
//     while the compact request strips them;
//   - a large payload: a compact request re-sends the whole conversation, so
//     anything small is a title generation or an ordinary no-tool API call;
//   - the instruction text in the final user message only: the post-compact
//     continuation turn quotes summary content in its FIRST user message, and a
//     transcript that merely mentions the phrase carries it in earlier messages.
//
// If a client update rewords the prompt, detection quietly returns false and the
// compact runs on the session model — the status quo, slow but correct. The
// debug line in compactRoute is what makes that drift diagnosable.
const (
	// compactRequestMarker is the load-bearing sentence of the summarization
	// instruction, verified against the 2.1.x client.
	compactRequestMarker = "Your task is to create a detailed summary of the conversation so far"
	compactMinBytes      = 32 << 10
	// compactEffort is forced onto detected compact turns. Medium, not low: the
	// summary seeds the continued session, so its quality is worth a real effort
	// budget — the latency win comes from dropping max-effort reasoning, not from
	// racing to the cheapest setting.
	compactEffort = "medium"
)

// isCompactRequest reports whether this turn is a Claude Code compact/auto-compact
// summarization request.
func isCompactRequest(request translator.AnthropicRequest, rawBytes int) bool {
	if len(request.Tools) != 0 || rawBytes < compactMinBytes {
		return false
	}
	for index := len(request.Messages) - 1; index >= 0; index-- {
		message := request.Messages[index]
		if message.Role != "user" {
			continue
		}
		for _, content := range message.Content {
			if content.Type == "text" && strings.Contains(content.Text, compactRequestMarker) {
				return true
			}
		}
		// Only the final user message may carry the instruction; anything earlier
		// is history quoting it.
		return false
	}
	return false
}

// compactRoute reports the model that should serve this turn because it is a
// compact request and a compact model is configured.
//
// Unlike planModeModel it does not bail when the configured model equals the
// requested one: the forced effort must still apply, which is exactly the
// "keep the session model, just stop reasoning at max effort" configuration.
func (s *Service) compactRoute(request translator.AnthropicRequest, rawBytes int) (string, bool) {
	compactModel := s.CompactModel()
	if compactModel == "" {
		return "", false
	}
	if !isCompactRequest(request, rawBytes) {
		if len(request.Tools) == 0 && rawBytes >= compactMinBytes {
			// A large no-tools request that misses the marker is what a client
			// update that reworded the prompt looks like.
			slog.Debug("large no-tools request did not match the compact marker",
				"model", request.Model, "request_bytes", rawBytes)
		}
		return "", false
	}
	return compactModel, true
}

// CompactModel reports the model compact requests are routed to, or empty when
// the override is off.
func (s *Service) CompactModel() string {
	if value := s.compactModel.Load(); value != nil {
		return *value
	}
	return ""
}

// SetCompactModel changes the compact-request override on a running gateway, so
// the dashboard does not need a restart to take effect. Empty turns it off.
// Same atomic-pointer contract as SetPlanModel: read on every /v1/messages
// request, written once in a session.
func (s *Service) SetCompactModel(model string) {
	model = strings.TrimSpace(model)
	s.compactModel.Store(&model)
}
