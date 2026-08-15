package gateway

import (
	"context"
	"net/http"
	"testing"

	"literouter/internal/provider"
)

func contextRejection(message string) error {
	return &provider.ProviderError{Provider: "test", StatusCode: http.StatusBadRequest, Message: message}
}

func TestParseContextWindowRejection(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    int
		wantOK  bool
	}{
		{
			name:    "anthropic wording",
			message: "prompt is too long: 513800 tokens > 400000 tokens",
			want:    400_000,
			wantOK:  true,
		},
		{
			name:    "openai wording quotes the window first",
			message: "This model's maximum context length is 400000 tokens. However, your messages resulted in 513800 tokens",
			want:    400_000,
			wantOK:  true,
		},
		{
			// The output budget is quoted beside the window and is the smallest number in
			// the message, so a "smallest wins" reading would learn 8192 as the window.
			name:    "output budget quoted alongside is ignored",
			message: "input length and max_tokens exceed context limit: 500000 + 8192 > 400000",
			want:    400_000,
			wantOK:  true,
		},
		{
			// A trailing remainder is smaller than the window, so a "smallest number wins"
			// reading would install 113800 as the ceiling.
			name:    "a quoted remainder is not the window",
			message: "maximum context length is 400000 tokens, your messages resulted in 513800 tokens; reduce by 113800 tokens",
			want:    400_000,
			wantOK:  true,
		},
		{
			// One number cannot be told apart from the size that broke the limit, and
			// guessing high installs a ceiling above the real window.
			name:    "a single number is too ambiguous to learn from",
			message: "prompt is too long: 513800 tokens",
			want:    0,
			wantOK:  false,
		},
		{
			name:    "not a context rejection",
			message: "max_tokens: 64000 > 32000, which is the maximum allowed number of output tokens",
			want:    0,
			wantOK:  false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := parseContextWindowRejection(contextRejection(testCase.message))
			if ok != testCase.wantOK || got != testCase.want {
				t.Fatalf("got (%d, %v), want (%d, %v)", got, ok, testCase.want, testCase.wantOK)
			}
		})
	}
}

func TestResolveContextWindowPrefersLearnedCeiling(t *testing.T) {
	service := New(Options{
		ContextWindow: func(context.Context, string) (int, error) { return 1_000_000, nil },
	})
	if window := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-sol"); window != 1_000_000 {
		t.Fatalf("catalog window: got %d, want 1000000", window)
	}
	service.learnContextWindow("cx/gpt-5.6-sol", 513_800,
		contextRejection("prompt is too long: 513800 tokens > 400000 tokens"))
	if window := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-sol"); window != 400_000 {
		t.Fatalf("learned window: got %d, want 400000", window)
	}
}

func TestDisableContextLearningKeepsOperatorWindow(t *testing.T) {
	service := New(Options{
		ContextWindow:          func(context.Context, string) (int, error) { return 272_000, nil },
		ObservedPrompts:        map[string]int{"cx/gpt-5.6-sol": 370_000},
		DisableContextLearning: true,
	})
	// A persisted floor from a previous run must not be seeded back in at boot.
	if window := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-sol"); window != 272_000 {
		t.Fatalf("seeded floor overrode operator window: got %d, want 272000", window)
	}
	// A served prompt that would normally raise the floor must not move the window.
	service.recordUsage(UsageEvent{Model: "cx/gpt-5.6-sol", PromptTokens: 370_000})
	// A rejection that would normally lower it must not either.
	service.learnContextWindow("cx/gpt-5.6-sol", 400_000,
		contextRejection("prompt is too long: 400000 tokens > 200000 tokens"))
	if window := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-sol"); window != 272_000 {
		t.Fatalf("window = %d, want the operator-set 272000 to survive", window)
	}
}

func TestObservedPromptRaisesWindowAboveCatalogGuess(t *testing.T) {
	service := New(Options{
		ContextWindow: func(context.Context, string) (int, error) { return 128_000, nil },
	})
	// A count the upstream reported for a request it served: proof the window is at
	// least this large, whatever the catalog guessed.
	service.recordUsage(UsageEvent{Model: "custom/big", PromptTokens: 250_000})
	if window := service.resolveContextWindow(context.Background(), "custom/big"); window != 250_000 {
		t.Fatalf("got %d, want the observed floor 250000", window)
	}
	// An estimate is not proof and must not move the floor.
	service.recordUsage(UsageEvent{Model: "custom/big", PromptTokens: 900_000, PromptTokensEstimated: true})
	if window := service.resolveContextWindow(context.Background(), "custom/big"); window != 250_000 {
		t.Fatalf("estimated count moved the floor: got %d, want 250000", window)
	}
	// Neither does a failed turn, whatever it claims to have sent.
	service.recordUsage(UsageEvent{Model: "custom/big", PromptTokens: 900_000, Status: "context_overflow"})
	if window := service.resolveContextWindow(context.Background(), "custom/big"); window != 250_000 {
		t.Fatalf("failed turn moved the floor: got %d, want 250000", window)
	}
}

// The floor only ever rises when a served prompt beats the current belief — but the guard
// shrinks every request to fit that belief before sending it, so once the belief is too
// low nothing large enough to correct it is ever sent again. Live consequence: 845 turns
// had been served above a catalogued 256,000, the largest at 372,860, and the catalogue
// still read 256,000 because all of them predated the guard. Seeding is the way out; the
// evidence was already in usage_events and nothing read it.
func TestSeededFloorCorrectsAWindowTheGuardCanNoLongerDisprove(t *testing.T) {
	catalogued := New(Options{
		ContextWindow: func(context.Context, string) (int, error) { return 256_000, nil },
	})
	if window := catalogued.resolveContextWindow(context.Background(), "cx/gpt-5.6-luna"); window != 256_000 {
		t.Fatalf("without seeding: got %d, want the catalogued 256000", window)
	}

	seeded := New(Options{
		ContextWindow:   func(context.Context, string) (int, error) { return 256_000, nil },
		ObservedPrompts: map[string]int{"cx/gpt-5.6-luna": 372_860},
	})
	if window := seeded.resolveContextWindow(context.Background(), "cx/gpt-5.6-luna"); window != 372_860 {
		t.Fatalf("seeded floor: got %d, want 372860", window)
	}
}

// A floor is a claim about one model, and it may only raise a belief. Anything that could
// lower one would strangle a model on start-up, which is worse than the guess it replaced.
func TestSeededFloorsOnlyRaiseAndIgnoreImplausibleEntries(t *testing.T) {
	service := New(Options{
		ContextWindow: func(context.Context, string) (int, error) { return 400_000, nil },
		ObservedPrompts: map[string]int{
			"kept":   500_000,
			"under":  120_000, // below the catalogue: must not lower it
			"tiny":   1_024,   // below what a window may plausibly be
			"":       900_000,
			"other":  0,
			"raised": 260_000,
		},
	})
	cases := map[string]int{"kept": 500_000, "under": 400_000, "tiny": 400_000, "other": 400_000, "raised": 400_000}
	for model, want := range cases {
		if window := service.resolveContextWindow(context.Background(), model); window != want {
			t.Fatalf("%s window = %d, want %d", model, window, want)
		}
	}
	// And a later observation still wins over a seeded one, exactly as within a run.
	service.recordUsage(UsageEvent{Model: "raised", PromptTokens: 610_000})
	if window := service.resolveContextWindow(context.Background(), "raised"); window != 610_000 {
		t.Fatalf("observation after seeding = %d, want 610000", window)
	}
}

func TestObservationOverridesMisreadCeiling(t *testing.T) {
	service := New(Options{})
	service.recordContextWindow("custom/model", 20_000)
	service.recordUsage(UsageEvent{Model: "custom/model", PromptTokens: 180_000})
	// A ceiling below a prompt the upstream actually served cannot be right.
	if window := service.resolveContextWindow(context.Background(), "custom/model"); window != 180_000 {
		t.Fatalf("got %d, want the observation 180000 to win", window)
	}
}

func TestRecordContextWindowKeepsTheLowestAndNotifiesOnce(t *testing.T) {
	var notified []int
	service := New(Options{OnContextWindow: func(_ string, window int) { notified = append(notified, window) }})
	if !service.recordContextWindow("m", 400_000) {
		t.Fatal("first observation should be new information")
	}
	if service.recordContextWindow("m", 500_000) {
		t.Fatal("a higher window is unverified and must be ignored")
	}
	if !service.recordContextWindow("m", 300_000) {
		t.Fatal("a lower window is stronger evidence and must be kept")
	}
	if len(notified) != 2 || notified[0] != 400_000 || notified[1] != 300_000 {
		t.Fatalf("notifications: got %v, want [400000 300000]", notified)
	}
}

func TestSmallestContextWindowIgnoresBlanks(t *testing.T) {
	windows := map[string]int{"cheap": 200_000, "big": 1_000_000}
	service := New(Options{
		ContextWindow: func(_ context.Context, model string) (int, error) { return windows[model], nil },
	})
	got := service.SmallestContextWindow(context.Background(), []string{"big", "", "cheap"})
	if got != 200_000 {
		t.Fatalf("got %d, want the smallest window 200000", got)
	}
	// An uncatalogued model resolves to the conservative hybrid fallback, and that lower
	// number is the one a global client setting has to respect.
	got = service.SmallestContextWindow(context.Background(), []string{"big", "uncatalogued"})
	if got != 128_000 {
		t.Fatalf("got %d, want the hybrid fallback 128000", got)
	}
	if got := service.SmallestContextWindow(context.Background(), []string{"", "  "}); got != 0 {
		t.Fatalf("nothing named: got %d, want 0", got)
	}
}

// The Codex/ChatGPT backend refuses with no number in the message, verified against the
// live upstream. Without a step-down the catalog's guess would be the only value there
// ever is, so a session that overflows once overflows identically forever.
func TestNumberlessRejectionStepsTheWindowDown(t *testing.T) {
	numberless := &provider.ProviderError{
		Provider: "codex OAuth", StatusCode: http.StatusRequestEntityTooLarge,
		Code:    "context_length_exceeded",
		Message: "Your input exceeds the context window of this model. Please adjust your input and try again.",
	}
	service := New(Options{
		ContextWindow: func(context.Context, string) (int, error) { return 400_000, nil },
	})
	service.learnContextWindow("cx/gpt-5.6-sol", 900_000, numberless)
	if window := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-sol"); window != 300_000 {
		t.Fatalf("after one numberless rejection: got %d, want 300000", window)
	}
	service.learnContextWindow("cx/gpt-5.6-sol", 900_000, numberless)
	if window := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-sol"); window != 225_000 {
		t.Fatalf("after two: got %d, want 225000", window)
	}
}

// A prompt the upstream actually served is a fact, and stepping past it would compact
// sessions that were working. The belief converges there instead of walking to zero.
func TestStepDownStopsAtTheLargestServedPrompt(t *testing.T) {
	numberless := &provider.ProviderError{
		Provider: "codex OAuth", StatusCode: http.StatusRequestEntityTooLarge,
		Code: "context_length_exceeded", Message: "Your input exceeds the context window of this model.",
	}
	service := New(Options{
		ContextWindow: func(context.Context, string) (int, error) { return 400_000, nil },
	})
	// The size the live upstream reported for a request it served.
	service.recordUsage(UsageEvent{Model: "cx/gpt-5.6-sol", PromptTokens: 350_018})
	for attempt := 0; attempt < 5; attempt++ {
		service.learnContextWindow("cx/gpt-5.6-sol", 900_000, numberless)
	}
	if window := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-sol"); window != 350_018 {
		t.Fatalf("got %d, want it pinned at the served size 350018", window)
	}
}

// A refusal of something smaller than the model is believed to accept is not evidence
// about the window. Observed live: the same payload was refused twice then served
// unchanged, and each post-trim refusal used to knock another quarter off the belief.
func TestStepDownIgnoresRefusalsSmallerThanTheBelievedWindow(t *testing.T) {
	numberless := &provider.ProviderError{
		Provider: "codex OAuth", StatusCode: http.StatusRequestEntityTooLarge,
		Code: "context_length_exceeded", Message: "Your input exceeds the context window of this model.",
	}
	service := New(Options{
		ContextWindow: func(context.Context, string) (int, error) { return 300_000, nil },
	})
	service.learnContextWindow("cx/gpt-5.6-sol", 271_602, numberless)
	if window := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-sol"); window != 300_000 {
		t.Fatalf("got %d, want the belief left at 300000", window)
	}
	// Unknown size still steps down: a caller with no estimate to offer — the byte
	// passthrough — is no reason to stop learning.
	service.learnContextWindow("cx/gpt-5.6-sol", 0, numberless)
	if window := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-sol"); window != 225_000 {
		t.Fatalf("got %d, want 225000", window)
	}
}

// A served prompt larger than the catalogued window proves that window wrong, and the
// correction has to outlive the process — otherwise every restart re-discovers it, and
// until it does the client is told a smaller ceiling than the model has. Measured live:
// cx/gpt-5.4-mini served 267,873 tokens against a catalogued 200,000.
func TestServedPromptPastTheWindowIsPersisted(t *testing.T) {
	var persisted []int
	service := New(Options{
		ContextWindow:   func(context.Context, string) (int, error) { return 200_000, nil },
		OnContextWindow: func(_ string, window int) { persisted = append(persisted, window) },
	})
	service.recordUsage(UsageEvent{Model: "cx/gpt-5.4-mini", PromptTokens: 267_873})
	if len(persisted) != 1 || persisted[0] != 267_873 {
		t.Fatalf("persisted = %v, want [267873]", persisted)
	}
	// Inside the believed window there is nothing to correct, so the request path must not
	// pay a write for it.
	service.recordUsage(UsageEvent{Model: "cx/gpt-5.4-mini", PromptTokens: 267_900})
	service.recordUsage(UsageEvent{Model: "other/model", PromptTokens: 100_000})
	if len(persisted) != 2 || persisted[1] != 267_900 {
		t.Fatalf("persisted = %v, want the higher floor and nothing for the small turn", persisted)
	}
}

// Regression for a live incident: a run of numberless refusals walked cx/gpt-5.6-sol from
// a catalogued 400,000 down to roughly 53,000 and persisted it, so every later session was
// guarded, trimmed and reported against a limit eight times smaller than the truth.
//
// The gate on `sent` cannot prevent this on its own — as the belief shrinks a large client
// keeps sending requests bigger than it, so every refusal qualifies for another step.
func TestNumberlessRejectionsCannotCollapseTheWindow(t *testing.T) {
	numberless := &provider.ProviderError{
		Provider: "codex OAuth", StatusCode: http.StatusRequestEntityTooLarge,
		Code: "context_length_exceeded", Message: "Your input exceeds the context window of this model.",
	}
	var persisted []int
	service := New(Options{
		ContextWindow:   func(context.Context, string) (int, error) { return 400_000, nil },
		OnContextWindow: func(_ string, window int) { persisted = append(persisted, window) },
	})
	// Twenty refusals, each of a request far larger than any belief they produce.
	for attempt := 0; attempt < 20; attempt++ {
		service.learnContextWindow("cx/gpt-5.6-sol", 670_000, numberless)
	}
	window := service.resolveContextWindow(context.Background(), "cx/gpt-5.6-sol")
	if window != 200_000 {
		t.Fatalf("window = %d, want it floored at half the catalogued 400000", window)
	}
	// And none of it reaches the catalog: a step-down is inferred, not read, so it must not
	// outlive the process and become the value a later run starts from.
	if len(persisted) != 0 {
		t.Fatalf("persisted = %v, want nothing — step-downs are guesses", persisted)
	}
}

// A number the upstream actually named is a different kind of evidence and does persist,
// including below the step-down floor.
func TestANamedWindowPersistsAndIgnoresTheStepDownFloor(t *testing.T) {
	var persisted []int
	service := New(Options{
		ContextWindow:   func(context.Context, string) (int, error) { return 400_000, nil },
		OnContextWindow: func(_ string, window int) { persisted = append(persisted, window) },
	})
	service.learnContextWindow("m", 200_000,
		contextRejection("prompt is too long: 190000 tokens > 100000 tokens"))
	if window := service.resolveContextWindow(context.Background(), "m"); window != 100_000 {
		t.Fatalf("window = %d, want the named 100000", window)
	}
	if len(persisted) != 1 || persisted[0] != 100_000 {
		t.Fatalf("persisted = %v, want [100000]", persisted)
	}
}
