package gateway

import (
	"context"
	"strings"
	"sync/atomic"

	"literouter/internal/translator"
)

// Sending a turn that outgrew its model to one with more room is the third defence
// against a conversation dying of its own length, and the only one that costs nothing
// when it works. The other two are recoveries: the client compacting in time
// (CLAUDE_CODE_MAX_CONTEXT_TOKENS, set from the dashboard) discards history, and the
// gateway trimming after a rejection (retryAfterTrimmingContext) discards more and pays
// a wasted upstream call for it. Routing loses nothing.
//
// Ported from claude-code-router's `longContext`/`longContextThreshold` pair, with the
// threshold expressed as a share of the serving model's window rather than as an
// absolute token count. An absolute number cannot be right for two models at once — 60k
// is most of a 128k window and a quarter of a 400k one — and it would have to be
// re-tuned every time a model in the card changes.

// defaultLongContextPercent is the share of the serving model's window above which a
// turn is handed to the long-context model.
//
// 60 rather than something tighter because the size this is measured against is a byte
// estimate, and measured against the upstream's own counts that estimate ran anywhere
// from 0.55x to 1.3x of the truth. Routing early is cheap — the long-context model
// answers the turn — while routing late means the recovery paths take over.
const defaultLongContextPercent = 60

// longContextRoute is the whole rule, swapped atomically so the dashboard can change it
// on a running gateway.
type longContextRoute struct {
	model   string
	percent int
}

// LongContext reports the long-context model and threshold, or an empty model when the
// rule is off.
func (s *Service) LongContext() (string, int) {
	if route := s.longContext.Load(); route != nil {
		return route.model, route.percent
	}
	return "", defaultLongContextPercent
}

// SetLongContext changes the rule on a running gateway. An empty model turns it off; a
// percent outside 1..99 falls back to the default rather than disabling the rule, since
// a stored zero from an older row should not silently mean "never".
func (s *Service) SetLongContext(model string, percent int) {
	model = strings.TrimSpace(model)
	if percent < 1 || percent > 99 {
		percent = defaultLongContextPercent
	}
	s.longContext.Store(&longContextRoute{model: model, percent: percent})
}

// longContextTarget reports the model that should take over from servingModel because
// the turn is too big for it.
//
// rawBytes rather than a token count, because counting properly means translating the
// whole request — the dominant per-turn CPU cost once a session carries six figures of
// context, which is every turn this decides. The conversion is not a fixed divisor
// though: tokensFromBytes uses the ratio measured from this model's own served turns, so
// the threshold is compared against something close to what the upstream will count.
func (s *Service) longContextTarget(ctx context.Context, servingModel string, rawBytes int) (string, bool) {
	route := s.longContext.Load()
	if route == nil || route.model == "" || strings.EqualFold(route.model, servingModel) {
		return "", false
	}
	window := s.resolveContextWindow(ctx, servingModel)
	if window <= 0 || s.tokensFromBytes(servingModel, rawBytes) <= window*route.percent/100 {
		return "", false
	}
	// Moving a turn to a model with no more room than the one it came from buys nothing
	// and costs the prompt cache, so a misconfigured pair is treated as no rule at all.
	if s.resolveContextWindow(ctx, route.model) <= window {
		return "", false
	}
	return route.model, true
}

// routeModel picks the model for a /v1/messages turn, and reports why.
//
// Plan mode wins, because the point of the plan override is that planning is worth a
// strong model regardless of anything else. The exception is a plan turn the plan model
// cannot hold: forcing it there produces a rejection and then a trimmed plan built on
// half the conversation, which is worse than planning on whatever model can actually
// see all of it.
//
// Image capability is applied last, to whatever the size and plan rules settled on. It is
// a correction rather than a preference: the other two choose between models that could
// each serve the turn, while this one rules out a model that cannot read what it was sent.
// The error it can return means the turn is unservable as routed and must not be forwarded.
func (s *Service) routeModel(ctx context.Context, request translator.AnthropicRequest, rawBytes int) (routeDecision, error) {
	model, effort, reason, overridden := s.sizePlanAndCompactRoute(ctx, request, rawBytes)
	serving := request.Model
	if overridden {
		serving = model
	}
	vision, strip, err := s.imageCapabilityFix(serving, request)
	if err != nil {
		return routeDecision{}, err
	}
	decision := routeDecision{Reason: reason, Effort: effort}
	if overridden {
		decision.Model = model
	}
	switch {
	case vision != "":
		decision.Model, decision.Reason = vision, "image on a text-only model"
	case strip:
		decision.StripImages = true
		if decision.Reason == "" {
			decision.Reason = "images dropped for a text-only model"
		} else {
			decision.Reason += "; images dropped for a text-only model"
		}
	}
	return decision, nil
}

// routeDecision is what the routing rules concluded about one turn.
//
// A struct rather than a widening list of returns: there are three rules now and they can
// combine — a long-context handover whose target also cannot read the tool output it was
// handed is a real combination, not a hypothetical.
type routeDecision struct {
	// Model is the model to serve the turn, or empty to keep the one the client asked for.
	Model string
	// Effort forces the reasoning effort for the turn, or empty to leave it alone.
	// Only the compact route sets it today; it wins over the per-model catalog
	// override because it describes the task, not the model.
	Effort string
	// Reason is for the log; it is what makes an unexpected handover explainable later.
	Reason string
	// StripImages means the turn is servable only once its images are replaced with a
	// placeholder, because the model taking it cannot read them. It covers two situations:
	// a fresh image from a tool with no vision model configured, and images left over in the
	// history of a turn that is not about them — the second is what keeps a single screenshot
	// from moving the whole rest of a session onto the vision model.
	StripImages bool
}

// overrides reports whether the decision changes anything about the turn.
func (decision routeDecision) overrides() bool {
	return decision.Model != "" || decision.StripImages || decision.Effort != ""
}

// sizePlanAndCompactRoute resolves the rules that pick between models any of which
// could serve the turn.
//
// Compact beats plan: a /compact issued while plan mode is active is a
// summarization task, not a plan turn, and the plan markers are still in the
// transcript. Compact also beats long-context, with the same escape hatch plan
// mode has — a compact request is the largest request a session ever produces, so
// one the compact model cannot hold goes to the long-context model instead,
// keeping the forced effort because it is still a summarization turn.
func (s *Service) sizePlanAndCompactRoute(ctx context.Context, request translator.AnthropicRequest, rawBytes int) (model, effort, reason string, overridden bool) {
	if compactModel, ok := s.compactRoute(request, rawBytes); ok {
		if long, tooBig := s.longContextTarget(ctx, compactModel, rawBytes); tooBig {
			return long, compactEffort, "compact request exceeds the compact model's window", true
		}
		return compactModel, compactEffort, "compact request", true
	}
	if plan, ok := s.planModeModel(request); ok {
		if long, tooBig := s.longContextTarget(ctx, plan, rawBytes); tooBig {
			return long, "", "plan turn exceeds the plan model's window", true
		}
		return plan, "", "plan mode", true
	}
	if long, ok := s.longContextTarget(ctx, request.Model, rawBytes); ok {
		return long, "", "long context", true
	}
	return "", "", "", false
}

// ClientContextCeiling reports the window a client should be told it has, given the
// models it is configured with and the long-context rule.
//
// Without the rule it is the smallest window among them, because one global client
// setting has to hold for whichever model serves a turn. With the rule it is the
// long-context model's window: every turn that outgrows a smaller model is handed over
// before it gets there, so the smaller windows stop being the binding constraint. That
// is the payoff for configuring the rule — the client is allowed to use the room it
// actually has instead of the least room any of its models has.
func (s *Service) ClientContextCeiling(ctx context.Context, models []string) int {
	ceiling := s.SmallestContextWindow(ctx, models)
	route := s.longContext.Load()
	if route == nil || route.model == "" {
		return ceiling
	}
	if window := s.resolveContextWindow(ctx, route.model); window > ceiling {
		return window
	}
	return ceiling
}

// longContextPointer is the field type, named here so Service stays readable.
type longContextPointer = atomic.Pointer[longContextRoute]
