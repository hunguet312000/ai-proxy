package gateway

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
	"literouter/internal/translator"
)

// Claude Code asks for a large max_tokens — tens of thousands, tunable only upward
// through CLAUDE_CODE_MAX_OUTPUT_TOKENS — because every model it was built for
// accepts it. Point it at a model with a smaller output cap and the upstream
// rejects the request outright, on every single turn, before generating anything.
// There is no partial failure to recover from and no retry that helps: the number
// is wrong for that model and the client will keep sending it.
//
// Clamping is only ever applied to models with a configured cap. A blanket default
// would silently shorten answers from models that really do accept the request, and
// truncated output is a far more expensive failure to diagnose than a 400.
// Caps come from two places. Configuration is an explicit human decision and wins
// outright. Everything else is learned from upstream rejections at runtime, which is
// the only way to know a cap without a per-provider table that would go stale and,
// while wrong, truncate silently.
func (s *Service) outputLimit(model string) int {
	if limit := contextguard.LookupByModel(s.outputLimits, model); limit > 0 {
		return limit
	}
	s.learnedMu.RLock()
	defer s.learnedMu.RUnlock()
	return contextguard.LookupByModel(s.learnedLimits, model)
}

// minLearnableOutputLimit floors what a parsed number may be taken to mean. Model
// ids are full of small integers — "claude-opus-4-8", "gpt-5.6" — and mistaking one
// of those for a cap would clamp every response to a few tokens.
const minLearnableOutputLimit = 256

// outputLimitMarkers gate the parse. A rejection must actually claim an output or
// completion limit before any number inside it is trusted as one.
var outputLimitMarkers = []string{
	"max_tokens", "max_completion_tokens", "max_output_tokens",
	"output tokens", "completion tokens",
}

// isOutputLimitRejection reports whether an upstream refused the request because of
// its output budget, regardless of whether it named the number.
func isOutputLimitRejection(err error) bool {
	var providerError *provider.ProviderError
	if !errors.As(err, &providerError) {
		return false
	}
	switch providerError.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
	default:
		return false
	}
	// A context-window rejection also quotes two token counts, and must never be read
	// as an output cap: clamping max_tokens would not shorten the prompt, and the
	// client would stop compacting because the gateway stopped reporting overflow.
	if isUpstreamContextError(providerError) {
		return false
	}
	message := strings.ToLower(providerError.Message + " " + providerError.Code)
	for _, marker := range outputLimitMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// parseOutputLimitRejection extracts the cap an upstream just revealed by refusing
// the request, given the max_tokens that was asked for.
//
// Providers phrase it differently but all of them name the number:
//
//	max_tokens: 64000 > 32000, which is the maximum allowed number of output tokens
//	max_tokens is too large: 32000. This model supports at most 16384 completion tokens
//	max_tokens must be at least 1 and at most 8192
//
// so rather than matching each wording, the largest number that is both plausibly a
// token budget and strictly below what was requested is taken. "Strictly below"
// discards the echoed request value, and the floor discards version fragments.
func parseOutputLimitRejection(err error, requested int) (int, bool) {
	if requested <= minLearnableOutputLimit || !isOutputLimitRejection(err) {
		return 0, false
	}
	var providerError *provider.ProviderError
	if !errors.As(err, &providerError) {
		return 0, false
	}
	message := strings.ToLower(providerError.Message + " " + providerError.Code)
	best := 0
	for _, value := range integersIn(message) {
		if value >= minLearnableOutputLimit && value < requested && value > best {
			best = value
		}
	}
	return best, best > 0
}

// fallbackOutputLimits are the caps tried, in order, when an upstream refuses on
// output budget without naming its limit.
//
// Two rungs rather than a search. Each attempt re-uploads the whole conversation, so
// converging in three probes would cost more input tokens than the turn itself — and
// this only runs for upstreams whose error text omits the number, which the major
// providers all include. 8192 is the value essentially every model accepts; 2048 is a
// floor for the rare small one. Landing under the real cap is deliberate: an
// underestimate costs output headroom Claude Code was never going to use, while an
// overestimate is another rejection.
var fallbackOutputLimits = []int{8192, 2048}

// nextFallbackOutputLimit returns the highest fallback cap strictly below what was
// requested, or reports false when the ladder has nothing lower left.
//
// It is deliberately a function of the request alone rather than of an attempt index.
// Each step down makes the previous rung the new requested value, so an index into the
// ladder and the remaining rungs drift apart immediately; descending from whatever was
// last sent cannot. The attempt counter bounds how many steps are taken, nothing more.
func nextFallbackOutputLimit(requested int) (int, bool) {
	best := 0
	for _, limit := range fallbackOutputLimits {
		if limit < requested && limit > best {
			best = limit
		}
	}
	return best, best > 0
}

// integersIn collects the unsigned integer runs in text. Digit groups split by any
// separator are read apart, so a version like "5.6" contributes 5 and 6 — both far
// below the floor — rather than a spurious 56.
func integersIn(text string) []int {
	var values []int
	current, active := 0, false
	for index := 0; index <= len(text); index++ {
		if index < len(text) && text[index] >= '0' && text[index] <= '9' {
			// Saturate rather than wrap on absurdly long digit runs; the value is
			// discarded by the "below requested" test either way.
			if current < 1<<40 {
				current = current*10 + int(text[index]-'0')
			}
			active = true
			continue
		}
		if active {
			values = append(values, current)
		}
		current, active = 0, false
	}
	return values
}

// recordOutputLimit stores a learned cap and reports whether it is new information.
// Only lower caps are accepted: a higher observation means something else answered
// under this name, and believing it walks straight back into rejections.
func (s *Service) recordOutputLimit(model string, limit int) bool {
	if model == "" || limit <= 0 {
		return false
	}
	s.learnedMu.Lock()
	if existing, ok := s.learnedLimits[model]; ok && existing <= limit {
		s.learnedMu.Unlock()
		return false
	}
	if s.learnedLimits == nil {
		s.learnedLimits = map[string]int{}
	}
	s.learnedLimits[model] = limit
	s.learnedMu.Unlock()
	slog.Info("learned upstream output limit", "model", model, "max_tokens", limit)
	if s.onOutputLimit != nil {
		s.onOutputLimit(model, limit)
	}
	return true
}

// maxOutputLimitAttempts bounds how many caps one model may be given during a single
// turn. Each attempt re-uploads the conversation, so this tracks the ladder length
// rather than exceeding it.
var maxOutputLimitAttempts = len(fallbackOutputLimits)

// learnOutputLimit turns a rejection into a cap for this model. attempt is how many
// caps this model has already been given during this turn. It reports true when the
// caller should re-attempt the same candidate, which will now be clamped.
//
// A named number is always preferred. Falling back to the ladder when there is none —
// or when the named number failed to lower anything — is what keeps an upstream whose
// error text omits the limit from being a permanent hard failure.
func (s *Service) learnOutputLimit(model string, requested, attempt int, err error) bool {
	if attempt >= maxOutputLimitAttempts {
		return false
	}
	if limit, ok := parseOutputLimitRejection(err, requested); ok && s.recordOutputLimit(model, limit) {
		return true
	}
	if !isOutputLimitRejection(err) {
		return false
	}
	limit, ok := nextFallbackOutputLimit(requested)
	if !ok {
		return false
	}
	slog.Warn("upstream refused on output budget without naming its limit; stepping down",
		"model", model, "requested", requested, "trying", limit, "attempt", attempt+1)
	return s.recordOutputLimit(model, limit)
}

// cloneOutputLimits copies the configured caps and drops non-positive entries, so
// outputLimit never has to distinguish "no cap" from "a cap of zero" — the latter
// would clamp every response to nothing.
func cloneOutputLimits(limits map[string]int) map[string]int {
	if len(limits) == 0 {
		return nil
	}
	result := make(map[string]int, len(limits))
	for model, limit := range limits {
		if limit > 0 {
			result[model] = limit
		}
	}
	return result
}

// clampOpenAIOutput caps the output budget on a translated candidate. Both fields
// are clamped because which one the upstream honours depends on how new its API is,
// and sending a too-large value in either is what gets rejected.
func (s *Service) clampOpenAIOutput(request *translator.OpenAIRequest) {
	limit := s.outputLimit(request.Model)
	if limit <= 0 {
		return
	}
	if request.MaxTokens > limit {
		slog.Debug("clamped max_tokens", "model", request.Model, "requested", request.MaxTokens, "limit", limit)
		request.MaxTokens = limit
	}
	if request.MaxCompletionTokens > limit {
		slog.Debug("clamped max_completion_tokens", "model", request.Model, "requested", request.MaxCompletionTokens, "limit", limit)
		request.MaxCompletionTokens = limit
	}
}

// clampProviderOutput is the same cap applied on the non-streaming path, where the
// candidate is still in the unified shape.
func (s *Service) clampProviderOutput(request *provider.Request) {
	limit := s.outputLimit(request.Model)
	if limit <= 0 {
		return
	}
	if request.MaxTokens > limit {
		slog.Debug("clamped max_tokens", "model", request.Model, "requested", request.MaxTokens, "limit", limit)
		request.MaxTokens = limit
	}
	if request.MaxCompletionTokens > limit {
		slog.Debug("clamped max_completion_tokens", "model", request.Model, "requested", request.MaxCompletionTokens, "limit", limit)
		request.MaxCompletionTokens = limit
	}
}
