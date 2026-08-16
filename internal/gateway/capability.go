package gateway

import (
	"errors"
	"log/slog"
	"strings"

	"literouter/internal/provider"
	"literouter/internal/storage"
	"literouter/internal/translator"
)

// Model capability quirks, learned from upstream rejections.
//
// A vLLM server started without --enable-auto-tool-choice rejects
// tool_choice:"auto" outright, and some reasoning templates accept only a
// subset of effort values (Qwen3 accepts xhigh/medium/low but not high, which
// is Claude Code's default). Both are per-model facts the proxy can only learn
// by asking once: after the first rejection the request is re-issued in the
// form the upstream accepts, the fact is remembered for the rest of the
// process, and every later turn is shaped pre-emptively so the failure never
// recurs. The retry is invisible to the caller because recovery happens before
// the error is returned.

// autoToolChoiceMarkers identify a rejection caused by the upstream not
// supporting automatic tool choice.
var autoToolChoiceMarkers = []string{
	// vLLM, on the chat template render: "\"auto\" tool choice requires
	// --enable-auto-tool-choice and --tool-call-parser to be set".
	"--enable-auto-tool-choice",
}

// reasoningEffortMarkers identify a rejection caused by an unsupported
// reasoning effort value.
var reasoningEffortMarkers = []string{
	// Qwen/vLLM chat template: "Unexpected reasoning effort high. Supported
	// types are xhigh (default), medium, and low."
	"unexpected reasoning effort",
}

func isAutoToolChoiceRejection(err error) bool {
	var providerError *provider.ProviderError
	if !errors.As(err, &providerError) {
		return false
	}
	message := strings.ToLower(providerError.Message)
	for _, marker := range autoToolChoiceMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func isReasoningEffortRejection(err error) bool {
	var providerError *provider.ProviderError
	if !errors.As(err, &providerError) {
		return false
	}
	message := strings.ToLower(providerError.Message)
	for _, marker := range reasoningEffortMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// knownSafeReasoningEffort is what a request's effort is rewritten to when the
// value it carried — or the template default an absent field produces — was
// rejected. Qwen3's template accepts xhigh, medium and low; low is the common
// denominator and the same value the proxy's own summarization calls already
// use, so it is the safest thing to put in place of a refused effort.
const knownSafeReasoningEffort = "low"

// recoverIncompatibleCapabilities learns from a rejection that this model's
// upstream does not support something the request carried, and reports whether
// the caller should retry the same candidate in the corrected shape.
//
// request is the payload that was just refused; its tool_choice and
// reasoning_effort name exactly what the upstream compared, which is what gets
// remembered. Only a newly learned fact earns a retry — if the fact was
// already known, the request was already shaped and retrying would loop on an
// error shaping cannot fix.
func (s *Service) recoverIncompatibleCapabilities(model string, request *translator.OpenAIRequest, err error) bool {
	learned := false
	if isAutoToolChoiceRejection(err) {
		learned = s.recordCapability("auto tool choice", model, &s.noAutoToolChoice) || learned
	}
	if isReasoningEffortRejection(err) && request != nil {
		// The rejected value is exactly what the request carried — and when it
		// carried none, the template's default ("high" on Qwen3) is what was
		// rejected, so the empty string is learned as a rejected value too.
		value := request.ReasoningEffort
		s.learnedMu.Lock()
		if s.rejectedReasoningEfforts == nil {
			s.rejectedReasoningEfforts = map[string]map[string]bool{}
		}
		rejected := s.rejectedReasoningEfforts[model]
		if rejected == nil {
			rejected = map[string]bool{}
			s.rejectedReasoningEfforts[model] = rejected
		}
		if !rejected[value] {
			rejected[value] = true
			learned = true
			slog.Warn("learned upstream rejects reasoning effort value", "model", model,
				"value", value, "recovering_by_sending", knownSafeReasoningEffort)
		}
		s.learnedMu.Unlock()
	}
	return learned
}

func (s *Service) recordCapability(what, model string, set *map[string]bool) bool {
	if model == "" {
		return false
	}
	s.learnedMu.Lock()
	if *set == nil {
		*set = map[string]bool{}
	}
	known := (*set)[model]
	(*set)[model] = true
	if !known {
		slog.Warn("learned upstream does not support "+what, "model", model,
			"recovering_by_shaping_future_requests", true)
	}
	s.learnedMu.Unlock()
	return !known
}

// applyModelCapabilities reshapes a request that is about to be sent so it
// only uses capabilities this model's upstream is known to have. The two
// learned quirks:
//
//   - tool_choice:"auto" is rewritten to "none" when the upstream rejects it.
//     Keeping the tools defined is deliberate: a server that gains
//     --enable-auto-tool-choice later starts calling them again with no proxy
//     change, and "none" is the value vLLM treats as plain chat.
//   - reasoning_effort is rewritten to knownSafeReasoningEffort when its value —
//     or the template default an absent field produces — has been rejected. A
//     supported value like "medium" still reaches the model untouched.
func (s *Service) applyModelCapabilities(request *translator.OpenAIRequest, model string) {
	if model == "" {
		return
	}
	s.learnedMu.RLock()
	noAuto := s.noAutoToolChoice[model]
	var rejected map[string]bool
	if s.rejectedReasoningEfforts != nil {
		rejected = s.rejectedReasoningEfforts[model]
	}
	s.learnedMu.RUnlock()
	s.disableThinkingMu.RLock()
	disabled := s.disableThinking[model]
	s.disableThinkingMu.RUnlock()
	if noAuto {
		if choice, ok := request.ToolChoice.(string); ok && choice == "auto" {
			request.ToolChoice = "none"
		}
	}
	// The "off" override is the operator's explicit "send no effort", and it
	// wins over recovery. On the untranslated form the override rides as Effort
	// ("off"); after translation it becomes ForceNoEffort. A model that had its
	// empty value rejected gets "low" only when the request was not told to
	// stay effort-free.
	if request.ForceNoEffort || request.Effort == storage.EffortOff {
		request.ReasoningEffort = ""
		return
	}
	// The empty string is a value like any other: an absent reasoning_effort is
	// defaulted by the template (to "high" on Qwen3), so it can be the very
	// thing that was rejected. Sending "low" in its place is what makes the
	// retry — and every later turn — accepted.
	if rejected[request.ReasoningEffort] {
		request.ReasoningEffort = knownSafeReasoningEffort
	}
	// vLLM Qwen3 with thinking enabled deliberates first and then either calls a
	// tool or ends with a bare "\n\n" — the stall. Only when the operator turned
	// the toggle on for this model is thinking disabled so it answers directly.
	if disabled {
		if request.ChatTemplateKwargs == nil {
			request.ChatTemplateKwargs = map[string]any{}
		}
		if _, ok := request.ChatTemplateKwargs["enable_thinking"]; !ok {
			request.ChatTemplateKwargs["enable_thinking"] = false
		}
	}
}
