package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/labstack/echo/v4"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
)

// The context window is the one limit that cannot safely be guessed at.
//
// Guess low and the guard refuses turns the upstream would have served. Guess high
// and the damage is worse, because it is not confined to one turn: Claude Code sizes
// its own compaction off the window it believes the model has, and a compaction
// request re-sends the entire conversation, so it costs roughly twice what the
// conversation costs. Compact a 250k-token session and the request is ~500k. Let the
// session grow past half the real window before anything compacts and no compaction
// can ever fit again — the only way out is discarding the session.
//
// Curated defaults go stale and hand-entered numbers are guesses, so both are
// treated here as opening bids that runtime evidence overrides. Two kinds of
// evidence arrive without spending an extra request:
//
//	a rejection names the real limit  -> an upper bound, authoritative
//	an accepted prompt of N tokens    -> a lower bound, incontrovertible
//
// The upper bound wins when both exist, because exceeding it fails hard while
// underestimating merely guards early. The exception is a contradiction: a bound
// below a prompt the upstream demonstrably accepted was misread, and the observation
// is the survivor.

// minLearnableContextWindow floors what a number inside a rejection may be taken to
// mean. Rejections quote small integers next to the window — an output budget in
// "input length and max_tokens exceed context limit: 500000 + 8192 > 400000", model
// id fragments elsewhere — and reading one as a window would strangle every turn.
const minLearnableContextWindow = 16384

// parseContextWindowRejection extracts the window an upstream just revealed by
// refusing a request.
//
// Providers word it differently but all of them name both the size that was refused
// and the limit it broke:
//
//	prompt is too long: 513800 tokens > 400000 tokens
//	This model's maximum context length is 400000 tokens. However, your messages resulted in 513800 tokens
//	input length and max_tokens exceed context limit: 500000 + 8192 > 400000
//
// so rather than matching each wording: the largest number in the message is the size
// that was refused, and the window is the largest number strictly below it. That reads
// all three orderings, and it survives a trailing "reduce your prompt by 113800
// tokens", which a plain "smallest number wins" rule would learn as the window.
//
// Deliberately independent of LiteRouter's own estimate of the request. The estimate and
// the upstream's tokenizer disagree by tens of percent — measured at 267k against a
// reported 513k on the same payload — so comparing against it discarded correct lessons
// whenever the estimate came in under the real window.
func parseContextWindowRejection(err error) (int, bool) {
	var providerError *provider.ProviderError
	if !errors.As(err, &providerError) || !isUpstreamContextError(providerError) {
		return 0, false
	}
	message := strings.ToLower(providerError.Message + " " + providerError.Code)
	refused, window := 0, 0
	for _, value := range integersIn(message) {
		if value < minLearnableContextWindow {
			continue
		}
		if value > refused {
			refused, window = value, refused
			continue
		}
		if value > window {
			window = value
		}
	}
	// One plausible number alone is ambiguous — it may be the limit or the size that
	// broke it — and guessing wrong in the high direction installs a ceiling above the
	// real window, which is the failure this whole file exists to prevent.
	return window, window > 0
}

// recordContextWindow stores a window the upstream named in a rejection, and persists it.
// Only lower bounds are kept, matching the output-cap rule: a higher one cannot be
// verified without provoking another rejection, and the lowest observed value is the one
// known to be safe.
func (s *Service) recordContextWindow(model string, window int) bool {
	return s.storeContextWindow(model, window, true)
}

// recordContextWindowGuess stores a window that was inferred rather than read, and
// deliberately does not persist it.
//
// The distinction earned itself. A step-down is a heuristic — the upstream said only that
// something was too long — and writing heuristics into the catalog made them permanent
// and cumulative: observed live, a run of numberless refusals walked cx/gpt-5.6-sol from
// a catalogued 400,000 down to 60,789 and left it there across restarts, so every later
// session was guarded, trimmed and reported against a limit the model does not have.
// Keeping guesses in memory bounds the damage to one process lifetime and lets a restart
// recover whatever a human or the catalog actually knows.
func (s *Service) recordContextWindowGuess(model string, window int) bool {
	return s.storeContextWindow(model, window, false)
}

func (s *Service) storeContextWindow(model string, window int, persist bool) bool {
	if model == "" || window <= 0 {
		return false
	}
	s.learnedMu.Lock()
	if existing, ok := s.learnedWindows[model]; ok && existing <= window {
		s.learnedMu.Unlock()
		return false
	}
	if s.learnedWindows == nil {
		s.learnedWindows = map[string]int{}
	}
	s.learnedWindows[model] = window
	s.learnedMu.Unlock()
	slog.Info("learned upstream context window", "model", model, "context_window", window,
		"source", map[bool]string{true: "upstream named it", false: "inferred, not persisted"}[persist])
	if persist && s.onContextWindow != nil {
		s.onContextWindow(model, window)
	}
	return true
}

// observeContextWindow records that a prompt of promptTokens was served, which puts a
// floor under whatever the model's window is.
//
// Only counts the upstream itself reported are accepted. LiteRouter's estimate is
// derived from byte length and runs both high and low; treating a high one as proof
// would raise the floor above the real window and turn the guard off exactly when it
// is needed. A reported count is the upstream's own arithmetic on a request it chose
// to answer.
func (s *Service) observeContextWindow(model string, promptTokens int) bool {
	if model == "" || promptTokens <= minLearnableContextWindow {
		return false
	}
	// Read-locked fast path first. This runs on every served request, and in the steady
	// state the answer is "already know a bigger one", so taking the write lock to find
	// that out would serialise every response through it.
	s.learnedMu.RLock()
	existing, ok := s.observedWindows[model]
	s.learnedMu.RUnlock()
	if ok && existing >= promptTokens {
		return false
	}
	// Read before the floor is stored, or the comparison below is against this very
	// observation — resolveContextWindow returns the floor once it is in place, so
	// promptTokens > believed would never be true and nothing would ever be corrected.
	believed := s.resolveContextWindow(context.Background(), model)
	s.learnedMu.Lock()
	// Re-checked under the write lock: another response for the same model may have
	// raised the floor past this one while the read lock was released.
	if existing, ok := s.observedWindows[model]; ok && existing >= promptTokens {
		s.learnedMu.Unlock()
		return false
	}
	if s.observedWindows == nil {
		s.observedWindows = map[string]int{}
	}
	s.observedWindows[model] = promptTokens
	s.learnedMu.Unlock()
	slog.Debug("observed accepted prompt size", "model", model, "prompt_tokens", promptTokens)
	// A served prompt larger than the catalogued window proves that window wrong, so it is
	// worth persisting — otherwise the correction is re-discovered every restart, and until
	// it is, the client is told a smaller ceiling than the model has. Measured live:
	// cx/gpt-5.4-mini served 267,873 tokens against a catalogued 200,000.
	//
	// Persisting a floor as the window still understates it, but understating by a proven
	// amount beats understating by a guessed one. Only ever on the rare turn that raises
	// the floor past the belief, so the request path pays nothing in the normal case.
	if s.onContextWindow != nil && promptTokens > believed {
		slog.Info("prompt served past the catalogued window; correcting it",
			"model", model, "prompt_tokens", promptTokens)
		s.onContextWindow(model, promptTokens)
	}
	return true
}

// contextWindowStepDown is how far the believed window drops when an upstream refuses on
// context without naming its limit.
//
// Measured necessity, not a hypothetical: the Codex/ChatGPT backend answers
//
//	HTTP 413 context_length_exceeded: "Your input exceeds the context window of this
//	model. Please adjust your input and try again."
//
// which carries no number at all, so parsing can never improve the belief for it. Without
// a step-down the catalog's guess is the only value there will ever be, and if that guess
// is high every long session walks into the same wall repeatedly.
const contextWindowStepDown = 4 // believed × 3/4

// minContextStepDownRatio is how far the cascade may descend: never below this fraction
// of what configuration and the catalog say.
//
// A bound is not optional. The gate above stops a step-down when the refused request was
// smaller than the belief, but that gate cannot stop a cascade — as the belief shrinks a
// large client keeps sending requests bigger than it, so every refusal qualifies. Observed
// live: seven numberless refusals took a 400,000 window to roughly 53,000, at which point
// the client was told to compact against a limit eight times smaller than the truth. Half
// the curated value is the floor because the curated value is the only figure here that
// somebody actually verified.
const minContextStepDownRatio = 2

// learnContextWindow turns an upstream context rejection into a window for this model.
//
// sent is LiteRouter's estimate of what was uploaded, or zero when the caller has no way
// to know. It gates the numberless step-down only — never the parse, which reads the
// upstream's own numbers and must not be second-guessed by an estimator that was measured
// reading 633k for a prompt the upstream counted as 350k.
func (s *Service) learnContextWindow(model string, sent int, err error) {
	if window, ok := parseContextWindowRejection(err); ok {
		s.recordContextWindow(model, window)
		return
	}
	if !isContextOverflow(err) {
		return
	}
	believed := s.resolveContextWindow(context.Background(), model)
	if believed <= minLearnableContextWindow {
		return
	}
	// Refusing something smaller than the model is believed to accept says nothing about
	// the window, so it must not lower it. Observed live: a payload refused twice was
	// served moments later unchanged, and without this gate each of those refusals dropped
	// the believed window another quarter — telling the client to compact against a limit
	// the model does not have. Attempts after a trim are the common case here, being
	// smaller by construction.
	if sent > 0 && sent < believed {
		return
	}
	stepped := believed * (contextWindowStepDown - 1) / contextWindowStepDown
	// The descent stops at whichever floor is higher: half of what configuration and the
	// catalog claim, or a prompt the upstream has actually served. The first bounds the
	// cascade, the second is a fact — descending past a size known to work would compact
	// sessions that were succeeding.
	floor := s.baseContextWindow(context.Background(), model) / minContextStepDownRatio
	if _, served := s.learnedContextBounds(model); served > floor {
		floor = served
	}
	if stepped < floor {
		stepped = floor
	}
	if stepped >= believed || stepped <= minLearnableContextWindow {
		return
	}
	s.recordContextWindowGuess(model, stepped)
}

// baseContextWindow is the window before any runtime evidence: configuration and the
// catalog only. It is what the step-down floor is measured against, since a value a human
// or the curated table supplied is the only one here that was not inferred.
func (s *Service) baseContextWindow(ctx context.Context, model string) int {
	if s.contextWindow != nil {
		if window, err := s.contextWindow(ctx, model); err == nil && window > 0 {
			return window
		}
	}
	return s.contextLimits.Window(model)
}

// learnedContextBounds returns the bounds runtime evidence has established for a
// model: the lowest window a rejection named, and the largest prompt known to have
// been served.
func (s *Service) learnedContextBounds(model string) (ceiling, floor int) {
	s.learnedMu.RLock()
	defer s.learnedMu.RUnlock()
	return contextguard.LookupByModel(s.learnedWindows, model),
		contextguard.LookupByModel(s.observedWindows, model)
}

// SmallestContextWindow reports the smallest window among the given models, skipping
// blanks. A model with no catalogued window still contributes the conservative hybrid
// fallback rather than nothing, which is the safe direction: too small a belief costs
// early compaction, too large a one costs the session.
//
// Exported for the dashboard, which configures the client's global
// CLAUDE_CODE_MAX_CONTEXT_TOKENS from a card naming several models. One number has to
// hold for whichever of them serves a turn, so the smallest is the only safe answer:
// handing over the largest would leave every other model compacting too late, which is
// exactly the failure the setting exists to prevent.
func (s *Service) SmallestContextWindow(ctx context.Context, models []string) int {
	smallest := 0
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		window := s.resolveContextWindow(ctx, model)
		if window <= 0 {
			continue
		}
		if smallest == 0 || window < smallest {
			smallest = window
		}
	}
	return smallest
}

// longContextBeta is the header value Claude Code sends when the 1M-context beta is
// active — which is what selecting a `[1m]` model variant turns on.
const longContextBeta = "context-1m-2025-08-07"

// warnOnInflatedWindowBelief reports the one misconfiguration no proxy-side setting can
// correct.
//
// Picking a `[1m]` variant in /model makes the client size its compaction against a
// 1,000,000-token window, and it checks that marker before it consults
// CLAUDE_CODE_MAX_CONTEXT_TOKENS, so the dashboard's max-context setting is bypassed
// entirely. The suffix is stripped from the model id before the request is sent, so the
// only trace on this side is the beta header.
//
// It warns rather than rejects. The turn is servable, and the trim backstop keeps it
// servable as the conversation grows — but it will need trimming on every turn from here
// on, quietly losing history, and that is worth a line in the log rather than silence.
func (s *Service) warnOnInflatedWindowBelief(c echo.Context, model string) {
	if !strings.Contains(strings.ToLower(c.Request().Header.Get("anthropic-beta")), longContextBeta) {
		return
	}
	window := s.resolveContextWindow(c.Request().Context(), model)
	if window <= 0 || window >= 1_000_000 {
		return
	}
	s.learnedMu.Lock()
	if s.warnedInflated == nil {
		s.warnedInflated = map[string]bool{}
	}
	first := !s.warnedInflated[model]
	s.warnedInflated[model] = true
	s.learnedMu.Unlock()
	if !first {
		return
	}
	slog.Warn("client is using the 1M-context beta for a model that does not have it; "+
		"it will compact far too late — avoid the [1m] variant in /model for this model",
		"model", model, "real_context_window", window, "client_believes", 1_000_000)
}

// estimateSentTokens reports the prompt size of a request as LiteRouter counts it,
// for the one purpose of reading a rejection against it.
func estimateSentTokens(request provider.Request) int {
	return contextguard.EstimateRequest(request)
}
