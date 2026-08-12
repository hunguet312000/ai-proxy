package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
	"literouter/internal/storage"
	"literouter/internal/translator"
)

// sizedUpstream answers a probe by size: anything at or below limit is served, anything
// above is refused the way this backend refuses — a 413 carrying no number.
type sizedUpstream struct {
	limit int
	// refuseFirst many attempts are refused whatever their size, reproducing an upstream
	// that turns a payload away and then serves the identical one minutes later.
	refuseFirst int
	seen        []int
	calls       int
}

func (fake *sizedUpstream) DoJSON(_ context.Context, request translator.OpenAIRequest, _ string) (translator.OpenAIResponse, error) {
	fake.calls++
	size := 0
	for _, message := range request.Messages {
		size += len(strings.Fields(fake.textOf(message)))
	}
	fake.seen = append(fake.seen, size)
	if fake.refuseFirst >= fake.calls || size > fake.limit {
		return translator.OpenAIResponse{}, &provider.ProviderError{
			Provider: "codex OAuth", StatusCode: 413, Code: "context_length_exceeded",
			Message: "Your input exceeds the context window of this model.",
		}
	}
	return translator.OpenAIResponse{
		Choices: []translator.OpenAIChoice{{Message: translator.OpenAIMessage{Role: "assistant", Content: "ok"}}},
		Usage:   translator.OpenAIUsage{PromptTokens: size},
	}, nil
}

func (fake *sizedUpstream) textOf(message translator.OpenAIMessage) string {
	switch value := message.Content.(type) {
	case string:
		return value
	case []translator.OpenAIContentPart:
		var text strings.Builder
		for _, part := range value {
			text.WriteString(part.Text)
		}
		return text.String()
	}
	return ""
}

func (fake *sizedUpstream) DoStream(context.Context, translator.OpenAIRequest, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}
func (fake *sizedUpstream) SupportsAnthropicPassthrough(string) bool { return false }
func (fake *sizedUpstream) DoAnthropicStream(context.Context, []byte, string, string, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

// The point of a probe is to measure the model, not the belief about it. The pipeline
// exists to shrink requests to fit that belief, so a probe routed through it would
// compact itself down and report the number it started from.
func TestProbeSendsItsFullSizeEvenWithTheGuardOn(t *testing.T) {
	upstream := &sizedUpstream{limit: 1 << 30}
	service := New(Options{
		OAuthInference: upstream,
		ContextEnabled: true, ContextMode: ContextModeAggressive,
		ContextLimits: contextguard.Limits{Default: 32_000},
		ContextPolicy: contextguard.Policy{SoftRatio: 0.5, SummarizeRatio: 0.6, HardRatio: 0.9, KeepRecentTurns: 1},
	})

	probe := service.ProbeContextWindow(context.Background(), "cx/gpt-5.6-luna", 200_000)

	if !probe.Accepted || probe.Error != "" {
		t.Fatalf("probe = %+v", probe)
	}
	// The filler is sized in tokens and the fake counts words, so this is deliberately a
	// loose floor: what matters is that a 200k-token probe did not arrive as a 32k one.
	if len(upstream.seen) != 1 || upstream.seen[0] < 100_000 {
		t.Fatalf("upstream saw %v; the guard compacted the probe", upstream.seen)
	}
}

// A refusal from this upstream is partly session-scoped: the identical payload was
// measured refused three times in one run and served minutes later. Believing the first
// one installs a ceiling below what the model actually takes, which is the failure the
// whole probe exists to avoid.
func TestOneRefusalIsNotEnoughToRuleASizeOut(t *testing.T) {
	upstream := &sizedUpstream{limit: 1 << 30, refuseFirst: 1}
	service := New(Options{OAuthInference: upstream})

	result := service.SearchContextWindow(context.Background(), "cx/gpt-5.6-luna", 100_000, 200_000)

	if result.SmallestRefused != 0 {
		t.Fatalf("a single refusal ruled out %d: %+v", result.SmallestRefused, result.Steps)
	}
	if result.LargestAccepted == 0 {
		t.Fatalf("nothing accepted: %+v", result.Steps)
	}
}

// Two refusals at one size is the signal, and the search has to act on it.
func TestConfirmedRefusalsBoundTheSearch(t *testing.T) {
	upstream := &sizedUpstream{limit: 0} // refuses everything, twice per size
	service := New(Options{OAuthInference: upstream})

	result := service.SearchContextWindow(context.Background(), "cx/gpt-5.6-luna", 100_000, 200_000)

	if result.SmallestRefused == 0 {
		t.Fatalf("confirmed refusals did not bound the search: %+v", result.Steps)
	}
	if result.LargestAccepted != 0 {
		t.Fatalf("accepted %d against an upstream that refuses everything", result.LargestAccepted)
	}
	// Every size was attempted exactly probeConfirmations times, not once and not forever.
	if upstream.calls != len(result.Steps) || upstream.calls > maxProbeSteps*probeConfirmations {
		t.Fatalf("calls = %d, steps = %d", upstream.calls, len(result.Steps))
	}
}

// A prompt already served is proof, and re-proving it costs real tokens. The search has
// to start above what is already known rather than from the bottom of the range.
func TestSearchStartsAboveWhatIsAlreadyProven(t *testing.T) {
	upstream := &sizedUpstream{limit: 1 << 30}
	service := New(Options{
		OAuthInference:  upstream,
		ObservedPrompts: map[string]int{"cx/gpt-5.6-luna": 370_578},
	})

	service.SearchContextWindow(context.Background(), "cx/gpt-5.6-luna", 0, 0)

	if len(upstream.seen) == 0 {
		t.Fatal("no probe was sent")
	}
	// Word counts run below token counts for this filler, so compare in the same units:
	// the first probe must be comfortably larger than a search that ignored the floor
	// and started from minProbeTokens.
	if upstream.seen[0] < minProbeTokens {
		t.Fatalf("first probe was %d words, i.e. it started from the floor of the range: %v",
			upstream.seen[0], upstream.seen)
	}
}

// What the probe learns has to reach the guard, or the dashboard reports one number while
// every request is still budgeted against another.
func TestAcceptedProbeRaisesTheWindowTheGuardUses(t *testing.T) {
	upstream := &sizedUpstream{limit: 1 << 30}
	service := New(Options{
		OAuthInference: upstream,
		ContextWindow:  func(context.Context, string) (int, error) { return 256_000, nil },
	})
	before := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-luna")

	result := service.SearchContextWindow(context.Background(), "cx/gpt-5.6-luna", 300_000, 400_000)

	if result.Window <= before {
		t.Fatalf("probe concluded %d against a prior belief of %d", result.Window, before)
	}
	if after := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-luna"); after != result.Window {
		t.Fatalf("guard still budgets against %d after the probe found %d", after, result.Window)
	}
}

// An accepted probe proves "at least this much was served" and nothing more. The two are
// easy to confuse because the upstream reports the size it counted, which runs below the
// size the filler was aimed at — measured live, a probe aimed at 300,000 came back
// counted as 237,766. Treating that as the window lowered a catalogued 256,000 on the
// strength of a request that had succeeded.
func TestAnAcceptedProbeNeverLowersTheWindow(t *testing.T) {
	upstream := &sizedUpstream{limit: 1 << 30}
	service := New(Options{
		OAuthInference: upstream,
		ContextWindow:  func(context.Context, string) (int, error) { return 256_000, nil },
	})

	// A size the upstream happily serves, but smaller than what is already believed.
	probe := service.ProbeContextWindow(context.Background(), "cx/gpt-5.6-luna", 100_000)
	if !probe.Accepted {
		t.Fatalf("probe = %+v", probe)
	}
	if window := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-luna"); window != 256_000 {
		t.Fatalf("window moved to %d after a smaller prompt was served", window)
	}
	// And a search over a range entirely below the belief leaves it alone too.
	service.SearchContextWindow(context.Background(), "cx/gpt-5.6-luna", 20_000, 60_000)
	if window := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-luna"); window != 256_000 {
		t.Fatalf("window moved to %d after a search below the belief", window)
	}
}

// Repetition tokenizes denser than prose, so a filler built by repeating one phrase
// arrives smaller than its byte count implies and every search step aims low.
func TestProbeFillerDoesNotRepeatItself(t *testing.T) {
	filler := probeFiller(4_000)
	if len(filler) < 4_000 {
		t.Fatalf("filler is %d bytes, want at least 4000", len(filler))
	}
	// The same window of text must not appear twice: that is what a tokenizer merges.
	const window = 120
	seen := map[string]bool{}
	for offset := 0; offset+window <= len(filler); offset += window {
		chunk := filler[offset : offset+window]
		if seen[chunk] {
			t.Fatalf("filler repeats the block %q", chunk)
		}
		seen[chunk] = true
	}
}

// Cost is dominated by the first steps, which sit at the top of the range, so a search
// that opens far above a proven floor pays most of its bill narrowing back down. Measured
// live: a floor of 370,578 opened onto 555,867 and spent roughly 1.8M tokens on two
// refusals before it got near the answer.
func TestSearchDoesNotReachFarAboveAProvenFloor(t *testing.T) {
	const floor = 370_578
	upstream := &sizedUpstream{limit: 1 << 30}
	service := New(Options{
		OAuthInference:  upstream,
		ObservedPrompts: map[string]int{"cx/gpt-5.6-luna": floor},
	})

	result := service.SearchContextWindow(context.Background(), "cx/gpt-5.6-luna", 0, 0)

	for _, step := range result.Steps {
		if step.Tokens > floor*5/4 {
			t.Fatalf("probed %d, more than a quarter above the proven floor of %d", step.Tokens, floor)
		}
	}
	if result.TokensSpent > floor*3 {
		t.Fatalf("search spent %d tokens against a floor of %d", result.TokensSpent, floor)
	}
}

// The deadline exists to catch an upstream that answers nothing, and that failure looks
// identical at fifteen seconds and at two minutes — so it has to be short. It also has to
// stay above what a healthy probe of the same size actually costs, measured on this
// backend at about 1.5s plus 15µs per token, or a large legitimate probe gets cut off and
// reported as stuck.
func TestProbeTimeoutIsShortButStaysAboveTheRealCost(t *testing.T) {
	for _, size := range []int{60_000, 200_000, 340_000, 381_052, 1_000_000} {
		healthy := 1500*time.Millisecond + time.Duration(size)*15*time.Microsecond
		deadline := probeTimeout(size)
		if deadline < healthy*2 {
			t.Fatalf("%d tokens: deadline %s leaves no room over a healthy %s", size, deadline, healthy)
		}
		if deadline > 45*time.Second {
			t.Fatalf("%d tokens: deadline %s is back to being too patient", size, deadline)
		}
	}
	// And the smallest probes are not made to wait on the per-token term alone.
	if probeTimeout(1) < probeBaseTimeout {
		t.Fatal("a tiny probe got less than the base deadline")
	}
}

// A model belonging to a custom provider has to be probed through that provider. Sent to
// the OAuth pool it finds no account, and to a built-in client an unknown model — so every
// size came back "refused" in a few hundred milliseconds while the plain Test button on the
// same card reported OK. Sizes refused faster than the prompt could be uploaded are not
// refusals.
func TestProbeReachesCustomProviderModels(t *testing.T) {
	var seen struct {
		calls int
		model string
		words int
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen.calls++
		seen.model = body.Model
		for _, message := range body.Messages {
			seen.words += len(strings.Fields(message.Content))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":%d}}`, seen.words)
	}))
	defer upstream.Close()

	registry := NewCustomProviderRegistry(upstream.Client())
	// A provider with no keys is skipped by Reload, so the fixture needs one to be claimed
	// at all.
	if err := registry.Reload([]storage.CustomProvider{{
		ID: "fpt", Prefix: "fpt-ai", Name: "FPT AI", BaseURL: upstream.URL,
		APIType: "chat", Enabled: true,
		Keys: []storage.CustomProviderKey{{
			ID: "k1", ProviderID: "fpt", Label: "test", Enabled: true, Weight: 1, Secret: "secret",
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	oauth := &sizedUpstream{limit: 1 << 30}
	service := New(Options{OAuthInference: oauth, CustomProviders: registry})

	probe := service.ProbeContextWindow(context.Background(), "fpt-ai/GLM-5.2", 40_000)

	if !probe.Accepted || probe.Error != "" {
		t.Fatalf("probe = %+v", probe)
	}
	if seen.calls != 1 {
		t.Fatalf("custom upstream calls = %d, want 1", seen.calls)
	}
	if oauth.calls != 0 {
		t.Fatalf("the OAuth pool was asked about a custom-provider model %d times", oauth.calls)
	}
	// The prefix is stripped before the request leaves, and the prompt is full size.
	if seen.model != "GLM-5.2" {
		t.Fatalf("upstream received model %q, want the unprefixed id", seen.model)
	}
	if seen.words < 20_000 {
		t.Fatalf("upstream received only %d words; the probe was not sent at full size", seen.words)
	}
}

// A cost that was not paid must not be reported as one. Every attempt of one observed
// search failed before reaching any upstream, and the result still claimed 628,000 tokens.
func TestSpentTokensExcludeAttemptsThatNeverReachedTheUpstream(t *testing.T) {
	service := New(Options{}) // no OAuth pool, no client: nothing can be reached
	result := service.SearchContextWindow(context.Background(), "cx/gpt-5.6-luna", 100_000, 200_000)

	if result.TokensSpent != 0 {
		t.Fatalf("claimed %d tokens spent while nothing left the process: %+v", result.TokensSpent, result.Steps)
	}
	for _, step := range result.Steps {
		if step.Reached {
			t.Fatalf("step %+v claims it reached an upstream", step)
		}
	}
}
