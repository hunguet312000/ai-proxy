package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"literouter/internal/pool"
	"literouter/internal/provider"
	"literouter/internal/translator"
)

// Finding a model's real context window by asking it.
//
// Every other source in this codebase is second-hand: a curated table, a catalogue sync,
// a number parsed out of a rejection, a size that happened to be served. They disagree —
// the curated table said 400,000 for cx/gpt-5.6-* while the catalogue said 256,000 — and
// the disagreement is expensive in both directions, because the guard compacts against
// whichever one it believes. A probe settles it: send a prompt of a known size and see.
//
// Three properties of this upstream shape the design, all of them measured rather than
// assumed:
//
//   - A refusal is not reproducible. The identical payload was refused three times in one
//     run and served outright minutes later, so one rejection is not a measurement. Every
//     rejection here is confirmed before it is believed.
//   - Codex rejections carry no number, so nothing can be read out of the message. Only
//     the accept/reject outcome is information.
//   - LiteRouter's own estimate is unreliable in both directions — it read 633,361 for a
//     prompt the upstream counted as 350,018 — so the size that matters is the one the
//     upstream reports back, never the one this process computed.

// ContextProbe is one attempt at one size.
type ContextProbe struct {
	// Tokens is the size aimed at, in upstream tokens.
	Tokens int `json:"tokens"`
	// Reported is the count the upstream put on the prompt, or zero when it did not say.
	// This is the authoritative number: Tokens is only what the filler was sized for.
	Reported int  `json:"reported,omitempty"`
	Accepted bool `json:"accepted"`
	// Refused distinguishes a genuine context-window rejection from an upstream error that
	// says nothing about the prompt size. Only a context refusal may move the search ceiling.
	// A plan/usage-limit response such as Cursor's HTTP 400 must surface as Error instead.
	Refused bool `json:"refused,omitempty"`
	// TimedOut separates "the upstream never answered" from "the upstream said no". Only
	// the second bounds a window; treating the first as a refusal would invent a ceiling.
	TimedOut bool `json:"timed_out,omitempty"`
	// Started marks the notification sent before an attempt rather than after it. An
	// attempt can sit for the full two-minute deadline, so a caller told only about
	// finished attempts shows nothing at all for the first two minutes of a search —
	// which is exactly the stretch where a person most wants to know what it is doing.
	Started bool `json:"started,omitempty"`
	// Attempt numbers the retry within one size, since a refusal is probed twice.
	Attempt int `json:"attempt,omitempty"`
	// Reached records whether this response is evidence that the upstream consumed the
	// prompt for context measurement. Plan, quota, auth, and model-availability failures
	// may reach the HTTP endpoint but do not prove prompt tokens were accepted or billed;
	// keeping them false makes the reported cost honest.
	Reached  bool   `json:"reached,omitempty"`
	Error    string `json:"error,omitempty"`
	Duration string `json:"duration,omitempty"`
}

// ContextProbeResult is a completed search.
type ContextProbeResult struct {
	Model string `json:"model"`
	// Window is what the search concluded: the largest prompt the upstream actually
	// answered. It is a floor, not a ceiling — the real limit is somewhere between this
	// and the smallest size that was refused — and a floor is the honest answer, since
	// every other number here would be a guess about the gap.
	Window          int            `json:"window"`
	LargestAccepted int            `json:"largest_accepted"`
	SmallestRefused int            `json:"smallest_refused,omitempty"`
	Steps           []ContextProbe `json:"steps"`
	TokensSpent     int            `json:"tokens_spent"`
	Error           string         `json:"error,omitempty"`
}

const (
	// probeFillerWord is repeated to build the prompt. Ordinary prose rather than random
	// bytes: a tokenizer splits repetitive filler differently from real text, and the
	// point is to reach a token count the upstream agrees with.
	probeFillerWord = "review the following module carefully and note what it does. "
	// maxProbeSteps bounds a search. Each step uploads its whole prompt, so the cost of
	// a search is roughly the sum of its steps — six halvings resolve any realistic range
	// to within a few percent, and paying for more precision than that is not worth it.
	maxProbeSteps = 6
	// probeConfirmations is how many refusals at one size are needed before the size is
	// treated as refused, given that refusals here are partly session-scoped.
	probeConfirmations = 2
	// minProbeTokens keeps a search away from sizes no agent could work in.
	minProbeTokens = 16_384
	// maxProbeTokens caps what a search will ever upload in one step, so a runaway upper
	// bound cannot turn one button press into millions of billed tokens.
	maxProbeTokens = 1_100_000
	// probeResolution stops a binary search once the bracket is this tight. Below it the
	// remaining uncertainty costs less than the request needed to remove it.
	probeResolution = 8_192
	// probeBaseTimeout and probeTimeoutPerToken bound one upstream call.
	//
	// Sized from what a healthy probe actually costs on this backend — about 1.5s plus
	// 15µs per token, measured at 2.6s for 60k, 4.3s for 200k and 6.7s for 340k — with
	// room on top. A flat two minutes was the first attempt and it was far too patient:
	// the failure it exists to catch is an upstream that answers nothing at all, and that
	// looks the same at fifteen seconds as at two minutes. Waiting the extra minute and
	// three quarters bought no information and cost the person at the button four minutes
	// for a search that returned what was already known.
	//
	// Proportional rather than flat because the alternative is choosing between cutting
	// off a legitimate large probe and waiting out a small stuck one: at 380k this allows
	// ~23s against a healthy ~7s, and at 1M it allows ~35s.
	probeBaseTimeout     = 15 * time.Second
	probeTimeoutPerToken = 20 * time.Microsecond
	// probeSearchBudget bounds a whole search, so a run of slow attempts cannot add up to
	// an unbounded wait.
	probeSearchBudget = 3 * time.Minute
)

// probeTimeout is how long one attempt at this size may take before it is called stuck.
func probeTimeout(tokens int) time.Duration {
	return probeBaseTimeout + time.Duration(tokens)*probeTimeoutPerToken
}

// ProbeContextWindow sends one sized prompt straight to the upstream and reports what
// happened. It deliberately bypasses the context pipeline: the pipeline exists to shrink
// requests to fit the believed window, so routing a probe through it would measure the
// belief rather than the model.
func (s *Service) ProbeContextWindow(ctx context.Context, model string, tokens int) ContextProbe {
	probe := ContextProbe{Tokens: tokens}
	if tokens <= 0 {
		probe.Error = "probe size must be positive"
		return probe
	}
	if tokens > maxProbeTokens {
		probe.Error = fmt.Sprintf("probe size %d is above the %d cap", tokens, maxProbeTokens)
		return probe
	}
	deadline := probeTimeout(tokens)
	attemptCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	start := time.Now()
	response, err := s.sendProbe(attemptCtx, model, tokens)
	probe.Duration = time.Since(start).Round(time.Millisecond).String()
	defer func() { logProbe(model, probe) }()
	if err != nil {
		// A provider error is not automatically a context refusal. Cursor, for example,
		// returns HTTP 400 for plan/usage-limit and model-availability failures; treating
		// those as a size boundary fabricates a false ceiling in the dashboard.
		var providerErr *provider.ProviderError
		isProviderError := errors.As(err, &providerErr)
		isContextError := isProviderError && isUpstreamContextError(providerErr)
		probe.Reached = isContextError
		probe.Refused = isContextError
		// A deadline is not a refusal. Reporting it as one would tell the search that this
		// size is too large and quietly install a ceiling that nothing measured.
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			probe.TimedOut = true
			probe.Reached = true
			probe.Error = fmt.Sprintf("no answer within %s", deadline.Round(time.Second))
			return probe
		}
		probe.Error = err.Error()
		return probe
	}
	probe.Accepted = true
	probe.Reached = true
	if response.Usage.PromptTokens > 0 {
		probe.Reported = response.Usage.PromptTokens
	}
	return probe
}

// logProbe records every attempt, on whichever path made it. Logged here rather than in
// the search so a single sized probe — the cheap, precise option, and the one an operator
// reaches for when something looks wrong — leaves the same trail as a full search.
func logProbe(model string, probe ContextProbe) {
	slog.Info("context probe", "model", model, "tokens", probe.Tokens,
		"accepted", probe.Accepted, "refused", probe.Refused, "timed_out", probe.TimedOut, "reported", probe.Reported,
		"duration", probe.Duration, "error", probe.Error)
}

// probeFiller builds roughly targetBytes of text whose token density is close to real
// prose.
//
// The obvious filler — one phrase repeated — is the wrong shape: a tokenizer merges
// repetition far more aggressively than it merges varied text, so the prompt arrives
// denser than its byte count suggests. Measured live against Codex: a probe sized from
// this model's own learned bytes-per-token for 300,000 tokens was counted as 237,766, a
// fifth short, which for a search means every step aims lower than it meant to. Numbering
// each line is enough to stop the merging.
func probeFiller(targetBytes int) string {
	var filler strings.Builder
	filler.Grow(max(targetBytes, 1) + len(probeFillerWord))
	for index := 0; filler.Len() < targetBytes; index++ {
		fmt.Fprintf(&filler, "%d. %s", index, probeFillerWord)
	}
	return filler.String()
}

// sendProbe dispatches without the model chain. A fallback alias would answer for a
// different model than the one being measured, which is the one thing a probe must not do.
func (s *Service) sendProbe(ctx context.Context, model string, tokens int) (translator.OpenAIResponse, error) {
	scale := s.tokenScaleFor(model)
	bytesPerToken := scale.bytesPerToken
	if bytesPerToken <= 0 {
		bytesPerToken = fallbackBytesPerToken
	}
	temperature := 0.0
	request := provider.Request{
		Model: model, MaxTokens: 16, Temperature: &temperature, Effort: "low",
		Messages: []provider.Message{{Role: "user", Content: []provider.Content{
			{Type: "text", Text: probeFiller(int(float64(tokens)*bytesPerToken)) + "\nReply with exactly: ok"},
		}}},
	}
	upstream, err := translator.ToOpenAIRequest(request)
	if err != nil {
		return translator.OpenAIResponse{}, err
	}
	// A model claimed by a custom provider goes to that upstream and nowhere else, the same
	// rule the serving paths follow. Without this the probe fell through to the OAuth pool,
	// which has no account for a prefixed model, and then to the built-in OpenAI client,
	// which does not know the model either — so every size came back refused in a few
	// hundred milliseconds while the plain Test button on the same card said OK. Sizes
	// "refused" faster than a 22k prompt can be uploaded are not refusals.
	if target, ok := s.resolveCustomProvider(model); ok {
		path, pathErr := customUpstreamPath(target.APIType)
		if pathErr != nil {
			return translator.OpenAIResponse{}, pathErr
		}
		upstream.Model = target.Model
		var response translator.OpenAIResponse
		if err := target.Client.DoJSON(ctx, path, upstream, &response); err != nil {
			return translator.OpenAIResponse{}, err
		}
		s.touchCustomProviderKey(target.KeyID)
		return response, nil
	}
	if s.oauthInference != nil {
		response, oauthErr := s.oauthInference.DoJSON(ctx, upstream, "")
		if oauthErr == nil {
			return response, nil
		}
		// Only an empty pool means the question went unanswered. Every other failure is
		// the answer, and must not be retried against an API-key upstream — that would
		// measure a different backend than the one that serves this model.
		if !errors.Is(oauthErr, pool.ErrNoAccount) {
			return translator.OpenAIResponse{}, oauthErr
		}
	}
	client := s.clientForModel(model)
	if client == nil {
		return translator.OpenAIResponse{}, ErrProviderUnavailable
	}
	upstream.Model = upstreamModel(model)
	var response translator.OpenAIResponse
	if err := client.DoJSON(ctx, "/chat/completions", upstream, &response); err != nil {
		return translator.OpenAIResponse{}, err
	}
	return response, nil
}

// ContextWindowFor exposes the window the guard is currently budgeting against, so a
// caller deciding whether a measurement is worth persisting compares against the same
// number every request is served under rather than against the catalogue alone.
func (s *Service) ContextWindowFor(ctx context.Context, model string) int {
	return s.resolveContextWindow(ctx, model)
}

// ContextWindowEvidence reports the window in force for a model together with the
// runtime evidence behind it: the largest prompt the upstream has actually served, and
// the smallest window a rejection named.
//
// The dashboard needs all three because they routinely disagree, and until now it showed
// only the catalogue figure. A card reading 256k while every request was budgeted against
// 372,860 is not a cosmetic gap: it is the number an operator reasons about, and there
// was nowhere in the interface the real one appeared.
func (s *Service) ContextWindowEvidence(ctx context.Context, model string) (window, served, refused int) {
	refused, served = s.learnedContextBounds(model)
	return s.resolveContextWindow(ctx, model), served, refused
}

// SearchContextWindow finds the largest prompt a model will answer, between low and high.
//
// Seeded rather than started from nothing: the largest prompt already served is a proven
// lower bound, so the search begins where the evidence ends instead of rediscovering it.
// For a model with history that turns a six-step search into one or two steps.
func (s *Service) SearchContextWindow(ctx context.Context, model string, low, high int) ContextProbeResult {
	return s.SearchContextWindowStreaming(ctx, model, low, high, nil)
}

// SearchContextWindowStreaming is SearchContextWindow with a callback fired as each step
// finishes.
//
// A search can run for minutes — one measured attempt sat for the full two-minute deadline
// twice over — and a caller that can only report the end of it leaves a person watching a
// spinner with no idea whether anything is happening or what it has cost so far. onStep may
// be nil.
func (s *Service) SearchContextWindowStreaming(ctx context.Context, model string, low, high int, onStep func(ContextProbe)) ContextProbeResult {
	result := ContextProbeResult{Model: model}
	if strings.TrimSpace(model) == "" {
		result.Error = "model is required"
		return result
	}
	_, served := s.learnedContextBounds(model)
	low = max(low, served, minProbeTokens)
	if high <= 0 {
		high = s.resolveContextWindow(ctx, model)
	}
	// Room above the belief, or a probe can only ever confirm what is already assumed.
	// Two resolutions, not one: the loop stops as soon as the bracket is down to a single
	// resolution, so opening exactly that wide leaves nothing to probe at all.
	high = max(high, low+2*probeResolution)
	if served > 0 {
		// Reach beyond a proven floor is capped, because the cost of a search is dominated
		// by its first few steps and those sit near the top of the range. Measured on this
		// upstream: a floor of 370,578 opened onto a 555,867 ceiling, whose first two
		// bracketing steps were refusals at 463,222 and 416,900 — and a refusal is probed
		// twice, so ~1.8M tokens went on narrowing from above. Climbing by a quarter at a
		// time costs a fraction of that and converges over a few runs, and a model that
		// takes far more than it has already served is not a case worth paying to find in
		// one press.
		high = min(high, max(served*5/4, low+2*probeResolution))
	}
	high = min(high, maxProbeTokens)
	if low >= high {
		result.Error = fmt.Sprintf("nothing to search: floor %d is already at the %d cap", low, high)
		return result
	}

	result.LargestAccepted = 0
	searchCtx, cancelSearch := context.WithTimeout(ctx, probeSearchBudget)
	defer cancelSearch()
	for step := 0; step < maxProbeSteps && high-low > probeResolution; step++ {
		if searchCtx.Err() != nil {
			result.Error = searchCtx.Err().Error()
			break
		}
		size := low + (high-low)/2
		probe := s.probeWithConfirmation(searchCtx, model, size, &result, onStep)
		if probe.TimedOut {
			// Neither bound moved, so continuing would re-ask the same question and wait
			// out the same deadline again.
			result.Error = probe.Error
			break
		}
		if probe.Accepted {
			low = size
			result.LargestAccepted = max(result.LargestAccepted, probe.effective())
			continue
		}
		if !probe.Refused {
			// An auth, plan, quota, model-availability, or transport error says nothing about
			// the context window. Abort instead of turning it into a false upper bound.
			result.Error = probe.Error
			break
		}
		high = size
		if result.SmallestRefused == 0 || size < result.SmallestRefused {
			result.SmallestRefused = size
		}
	}

	result.Window = result.LargestAccepted
	if result.Window == 0 {
		// Nothing new was accepted, so the best answer is still whatever was already
		// proven. Reporting zero would read as "this model has no window".
		_, result.Window = s.learnedContextBounds(model)
	}
	if result.Window > 0 {
		// Feed it back the same way a served turn does, so the guard, the dashboard and
		// the client setting all move together instead of only this report knowing.
		s.observeContextWindow(model, result.Window)
	}
	slog.Info("probed context window", "model", model, "window", result.Window,
		"largest_accepted", result.LargestAccepted, "smallest_refused", result.SmallestRefused,
		"steps", len(result.Steps), "tokens_spent", result.TokensSpent)
	return result
}

// probeWithConfirmation treats a refusal as provisional until it repeats. A single
// rejection from this upstream is not a measurement: the identical payload was refused
// three times in one run and served minutes later, so believing the first one would
// install a ceiling below what the model actually takes.
func (s *Service) probeWithConfirmation(ctx context.Context, model string, size int, result *ContextProbeResult, onStep func(ContextProbe)) ContextProbe {
	var probe ContextProbe
	for attempt := 1; attempt <= probeConfirmations; attempt++ {
		if onStep != nil {
			onStep(ContextProbe{Tokens: size, Started: true, Attempt: attempt})
		}
		probe = s.ProbeContextWindow(ctx, model, size)
		probe.Attempt = attempt
		result.Steps = append(result.Steps, probe)
		result.TokensSpent += probe.effective()
		if onStep != nil {
			onStep(probe)
		}
		if probe.Accepted {
			return probe
		}
		if !probe.Refused {
			// A deterministic failure — a model id the upstream does not know, a plan gate,
			// an auth error — will repeat identically. Probing it again only doubles the
			// loop the confirmation exists to catch, which is session-scoped refusals.
			return probe
		}
		if ctx.Err() != nil {
			return probe
		}
	}
	return probe
}

// Effective is the size to credit this probe with: what the upstream counted when it said
// so, else what the filler was aimed at — and nothing at all when the prompt never left,
// because a cost that was not paid must not be reported as one.
func (probe ContextProbe) Effective() int {
	if probe.Reported > 0 {
		return probe.Reported
	}
	if !probe.Reached {
		return 0
	}
	return probe.Tokens
}

func (probe ContextProbe) effective() int { return probe.Effective() }

// EstimateProbeCost reports roughly how many input tokens a search will upload, so the
// dashboard can say what a button press costs before it is pressed.
func EstimateProbeCost(low, high int) int {
	if high <= low {
		return 0
	}
	total, size := 0, low+(high-low)/2
	for step := 0; step < maxProbeSteps && size > 0; step++ {
		total += size
		size /= 2
	}
	return total
}
