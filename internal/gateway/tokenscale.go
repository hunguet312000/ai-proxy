package gateway

import (
	"log/slog"

	"literouter/internal/contextguard"
)

// Every size decision in this package is made against a guess, and the guesses were
// measured wrong by wide margins.
//
// Against the counts upstreams reported for the same payloads, contextguard.EstimateRequest
// — max(bytes/4, runes/3) — read 633,361 for a prompt counted as 350,018 (1.8x high, on
// repetitive prose) and about 245,000 for one counted as 265,802 (0.9x, on hex-dense text).
// A single fixed divisor cannot span that, because it is a property of the content and the
// model's tokenizer, not a constant.
//
// It does not have to be guessed. Every served turn hands over both numbers at once: what
// LiteRouter thought the request was, and what the upstream says it actually was. Learning
// the ratio between them per model turns two coarse heuristics into arithmetic:
//
//	long-context routing  bytes  -> tokens, before translation, to compare against a window
//	trim budgeting        tokens -> estimate space, which is what TrimOldestTurns compares in
//
// Both were previously off by whatever the content happened to do. The trim was the worse
// of the two: it budgets in estimate space against a token-space window, so on prose — where
// the estimate reads high — it cut far more history than the model needed. Observed live, a
// turn was trimmed from 30 messages to 6 when 10 would have fit.

// tokenScale is what one model's traffic has revealed about its tokenizer.
type tokenScale struct {
	// bytesPerToken converts a raw request body length to tokens.
	bytesPerToken float64
	// estimatePerToken converts a token budget into the units EstimateRequest reports.
	estimatePerToken float64
}

// Fallbacks for a model nothing has been learned about yet. 4 bytes per token is the
// conventional English figure, and 1.0 keeps the trim budget behaving exactly as it did
// before this file existed.
const (
	fallbackBytesPerToken    = 4.0
	fallbackEstimatePerToken = 1.0
)

// Bounds on what a sample may claim. Outside these the numbers are not describing a
// tokenizer — a truncated stream, a cached prompt reported as a handful of tokens, a
// mis-parsed usage frame — and folding them in would move the scale away from the truth.
const (
	minBytesPerToken    = 1.0
	maxBytesPerToken    = 24.0
	minEstimatePerToken = 0.25
	maxEstimatePerToken = 6.0
)

// minScaleSampleTokens ignores small turns. Their size is dominated by the fixed cost of
// the system prompt and tool schemas rather than by the conversation, so their ratio does
// not describe the large turns these scales are used to judge.
const minScaleSampleTokens = 5_000

// scaleSmoothing is the weight a new sample carries. Low enough that one unusual turn —
// a screenshot, a wall of base64 — cannot swing the scale, high enough that a session
// which changes character is tracked within a handful of turns.
const scaleSmoothing = 0.3

// observeTokenScale folds one served turn into a model's scales.
//
// reported must be the upstream's own count. An estimate on both sides of the ratio would
// only ever teach it that the estimator agrees with itself.
func (s *Service) observeTokenScale(model string, rawBytes, estimate, reported int) {
	if model == "" || reported < minScaleSampleTokens || rawBytes <= 0 || estimate <= 0 {
		return
	}
	bytesPer := float64(rawBytes) / float64(reported)
	estimatePer := float64(estimate) / float64(reported)
	if bytesPer < minBytesPerToken || bytesPer > maxBytesPerToken ||
		estimatePer < minEstimatePerToken || estimatePer > maxEstimatePerToken {
		return
	}
	s.learnedMu.Lock()
	if s.tokenScales == nil {
		s.tokenScales = map[string]tokenScale{}
	}
	previous, known := s.tokenScales[model]
	scale := tokenScale{bytesPerToken: bytesPer, estimatePerToken: estimatePer}
	if known {
		scale.bytesPerToken = blend(previous.bytesPerToken, bytesPer)
		scale.estimatePerToken = blend(previous.estimatePerToken, estimatePer)
	}
	s.tokenScales[model] = scale
	s.learnedMu.Unlock()
	// Logged on the first sample and whenever it moves materially, at info rather than
	// debug. This value silently decides which model serves a turn and how much history a
	// trim keeps, and until it was surfaced the only way to see it was to infer it from
	// behaviour. A quiet self-tuning number is one nobody can check.
	if !known || movedMaterially(previous, scale) {
		slog.Info("measured tokenizer scale", "model", model,
			"bytes_per_token", round2(scale.bytesPerToken),
			"estimate_per_token", round2(scale.estimatePerToken),
			"sample_tokens", reported, "first", !known)
	}
}

// materialScaleChange is how far a scale must move to be worth a log line. Below this the
// smoothing is just tracking noise between turns.
const materialScaleChange = 0.1

func movedMaterially(previous, current tokenScale) bool {
	return relativeChange(previous.bytesPerToken, current.bytesPerToken) > materialScaleChange ||
		relativeChange(previous.estimatePerToken, current.estimatePerToken) > materialScaleChange
}

func relativeChange(previous, current float64) float64 {
	if previous <= 0 {
		return 1
	}
	delta := (current - previous) / previous
	if delta < 0 {
		return -delta
	}
	return delta
}

func blend(current, sample float64) float64 {
	return current*(1-scaleSmoothing) + sample*scaleSmoothing
}

func round2(value float64) float64 {
	return float64(int(value*100+0.5)) / 100
}

// tokenScaleFor returns what has been measured for a model, falling back to the
// conventional figures. Prefix matching is deliberate: a scale learned for "cx/gpt-5.6-sol"
// describes the family, and the review variants share its tokenizer.
func (s *Service) tokenScaleFor(model string) tokenScale {
	s.learnedMu.RLock()
	defer s.learnedMu.RUnlock()
	if scale, ok := s.tokenScales[model]; ok {
		return scale
	}
	bestLength := 0
	best := tokenScale{bytesPerToken: fallbackBytesPerToken, estimatePerToken: fallbackEstimatePerToken}
	for candidate, scale := range s.tokenScales {
		if len(candidate) > bestLength && modelFamilyMatch(model, candidate) {
			bestLength, best = len(candidate), scale
		}
	}
	return best
}

// modelFamilyMatch reuses the prefix rules the rest of the package keys per-model values
// with, so a scale is found by the same matching that finds a window or an output cap.
func modelFamilyMatch(model, prefix string) bool {
	return contextguard.LookupByModel(map[string]int{prefix: 1}, model) == 1
}

// tokensFromBytes converts a raw body length to tokens for the model that would serve it.
func (s *Service) tokensFromBytes(model string, rawBytes int) int {
	if rawBytes <= 0 {
		return 0
	}
	return int(float64(rawBytes) / s.tokenScaleFor(model).bytesPerToken)
}

// estimateBudgetForTokens converts a budget expressed in tokens into the units
// EstimateRequest — and therefore TrimOldestTurns — works in.
func (s *Service) estimateBudgetForTokens(model string, tokens int) int {
	if tokens <= 0 {
		return 0
	}
	return int(float64(tokens) * s.tokenScaleFor(model).estimatePerToken)
}
