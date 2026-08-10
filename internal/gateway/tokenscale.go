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
	// samples counts the estimate-bearing measurements folded in; spread is the
	// smoothed relative deviation between successive samples. Together they say
	// whether the measurement has earned trust: many samples that agree with each
	// other are evidence, three that disagree are noise.
	samples int
	spread  float64
}

// TokenCalibration is the exported shape of a learned scale, for persisting
// across restarts. Options.LearnedCalibrations seeds it back at boot.
type TokenCalibration struct {
	Model            string
	BytesPerToken    float64
	EstimatePerToken float64
	Spread           float64
	Samples          int
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

// initialSpread is where an unmeasured model starts: maximal disagreement, so
// nothing is trusted until the samples themselves earn it. Each consistent sample
// blends it down; a dozen agreeing samples reach calibStableSpread, while samples
// that keep disagreeing hold it high indefinitely.
const initialSpread = 1.0

// Confidence gate: with at least this many estimate-bearing samples whose spread
// has settled below this bound, the measured estimate ratio is applied to budget
// math as-is instead of through the conservative clamp. This is what replaces
// hand-tuned ratio knobs over time — the measurement takes over from the guess,
// but only once it has the evidence to.
const (
	calibConfidentSamples = 12
	calibStableSpread     = 0.05
)

// persistEvery bounds how often a converged calibration is re-persisted, so the
// samples/spread counters survive a restart without writing on every turn.
const persistEvery = 8

// observeTokenScale folds one served turn into a model's scales.
//
// reported must be the upstream's own count. An estimate on both sides of the ratio would
// only ever teach it that the estimator agrees with itself.
//
// Either side of the sample may be missing: the non-streaming candidate loops
// never held the raw body (rawBytes 0, estimate-only), and the Anthropic
// passthrough never translates so it has no estimate (estimate 0, bytes-only).
// Each side updates only its own ratio; the confidence counters follow the
// estimate side, which is what budget decisions ride on.
func (s *Service) observeTokenScale(model string, rawBytes, estimate, reported int) {
	if model == "" || reported < minScaleSampleTokens || (estimate <= 0 && rawBytes <= 0) {
		return
	}
	estimatePer := 0.0
	if estimate > 0 {
		estimatePer = float64(estimate) / float64(reported)
		if estimatePer < minEstimatePerToken || estimatePer > maxEstimatePerToken {
			return
		}
	}
	bytesPer := 0.0
	if rawBytes > 0 {
		bytesPer = float64(rawBytes) / float64(reported)
		if bytesPer < minBytesPerToken || bytesPer > maxBytesPerToken {
			return
		}
	}
	s.learnedMu.Lock()
	if s.tokenScales == nil {
		s.tokenScales = map[string]tokenScale{}
	}
	previous, known := s.tokenScales[model]
	scale := previous
	if !known {
		scale = tokenScale{bytesPerToken: fallbackBytesPerToken, estimatePerToken: fallbackEstimatePerToken, spread: initialSpread}
	}
	if bytesPer > 0 {
		if known && previous.bytesPerToken > 0 {
			scale.bytesPerToken = blend(previous.bytesPerToken, bytesPer)
		} else {
			scale.bytesPerToken = bytesPer
		}
	}
	if estimatePer > 0 {
		if scale.samples > 0 {
			// How far this sample sits from the running value is what spread tracks:
			// consistent traffic blends it toward zero, contradictory traffic holds
			// it high and keeps the conservative clamp in charge.
			scale.spread = blend(scale.spread, relativeChange(scale.estimatePerToken, estimatePer))
			scale.estimatePerToken = blend(scale.estimatePerToken, estimatePer)
		} else {
			scale.estimatePerToken = estimatePer
		}
		scale.samples++
	}
	s.tokenScales[model] = scale
	s.learnedMu.Unlock()
	// Logged on the first sample and whenever it moves materially, at info rather than
	// debug. This value silently decides which model serves a turn and how much history a
	// trim keeps, and until it was surfaced the only way to see it was to infer it from
	// behaviour. A quiet self-tuning number is one nobody can check.
	moved := !known || movedMaterially(previous, scale)
	if moved {
		slog.Info("measured tokenizer scale", "model", model,
			"bytes_per_token", round2(scale.bytesPerToken),
			"estimate_per_token", round2(scale.estimatePerToken),
			"spread", round2(scale.spread), "samples", scale.samples,
			"sample_tokens", reported, "first", !known)
	}
	// Persisted on material moves plus a periodic heartbeat: the heartbeat is what
	// carries the samples/spread confidence counters across a restart once the
	// ratios themselves have converged and stopped moving.
	if s.onCalibration != nil && (moved || (scale.samples > 0 && scale.samples%persistEvery == 0)) {
		s.onCalibration(TokenCalibration{
			Model: model, BytesPerToken: scale.bytesPerToken, EstimatePerToken: scale.estimatePerToken,
			Spread: scale.spread, Samples: scale.samples,
		})
	}
}

// seedTokenScales restores calibrations persisted by previous runs.
func (s *Service) seedTokenScales(calibrations []TokenCalibration) {
	if len(calibrations) == 0 {
		return
	}
	s.learnedMu.Lock()
	defer s.learnedMu.Unlock()
	if s.tokenScales == nil {
		s.tokenScales = map[string]tokenScale{}
	}
	for _, cal := range calibrations {
		if cal.Model == "" || cal.BytesPerToken <= 0 || cal.EstimatePerToken <= 0 {
			continue
		}
		s.tokenScales[cal.Model] = tokenScale{
			bytesPerToken:    cal.BytesPerToken,
			estimatePerToken: cal.EstimatePerToken,
			spread:           cal.Spread,
			samples:          cal.Samples,
		}
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
