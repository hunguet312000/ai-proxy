package gateway

import (
	"context"
	"testing"

	"literouter/internal/translator"
)

func planTranscript(text string) translator.AnthropicRequest {
	return translator.AnthropicRequest{
		Model: "cheap/small",
		Messages: []translator.AnthropicMessage{{
			Role: "user", Content: []translator.AnthropicContent{{Type: "text", Text: text}},
		}},
	}
}

func routingService(windows map[string]int) *Service {
	return New(Options{
		ContextWindow: func(_ context.Context, model string) (int, error) { return windows[model], nil },
	})
}

func TestLongContextRoutesOnlyOnceTheTurnIsBig(t *testing.T) {
	service := routingService(map[string]int{"cheap/small": 200_000, "big/wide": 1_000_000})
	service.SetLongContext("big/wide", 60)
	request := planTranscript("ordinary turn")

	// 60% of a 200k window is 120k tokens, i.e. 480_000 bytes by the byte estimate.
	if decision, _ := service.routeModel(context.Background(), request, 400_000); decision.overrides() {
		t.Fatal("a turn under the threshold must stay on the requested model")
	}
	decision, _ := service.routeModel(context.Background(), request, 600_000)
	if !decision.overrides() || decision.Model != "big/wide" || decision.Reason != "long context" {
		t.Fatalf("got (%q, %q, %v), want big/wide via long context", decision.Model, decision.Reason, decision.overrides())
	}
}

// Handing a turn to a model with no more room costs the prompt cache and buys nothing, so
// a pair configured the wrong way round is treated as no rule at all.
func TestLongContextIgnoresATargetWithNoMoreRoom(t *testing.T) {
	service := routingService(map[string]int{"cheap/small": 200_000, "narrow/model": 128_000})
	service.SetLongContext("narrow/model", 60)
	if decision, _ := service.routeModel(context.Background(), planTranscript("x"), 900_000); decision.overrides() {
		t.Fatal("routing to a smaller window must not happen")
	}
}

func TestPlanModeWinsOverLongContext(t *testing.T) {
	service := routingService(map[string]int{
		"cheap/small": 200_000, "strong/plan": 400_000, "big/wide": 1_000_000,
	})
	service.SetPlanModel("strong/plan")
	service.SetLongContext("big/wide", 60)
	// Over the requested model's threshold but well inside the plan model's: planning is
	// worth the strong model, and it can hold the turn.
	decision, _ := service.routeModel(context.Background(), planTranscript(planEnteredMarker), 600_000)
	if !decision.overrides() || decision.Model != "strong/plan" || decision.Reason != "plan mode" {
		t.Fatalf("got (%q, %q, %v), want strong/plan via plan mode", decision.Model, decision.Reason, decision.overrides())
	}
}

// The one case where long context overrules plan mode. Forcing a plan turn onto a model
// that cannot hold it produces a rejection and then a plan built on trimmed history,
// which is worse than planning on whatever model can see all of it.
func TestLongContextRescuesAPlanTurnTooBigForThePlanModel(t *testing.T) {
	service := routingService(map[string]int{
		"cheap/small": 200_000, "strong/plan": 400_000, "big/wide": 1_000_000,
	})
	service.SetPlanModel("strong/plan")
	service.SetLongContext("big/wide", 60)
	// 60% of the plan model's 400k window is 240k tokens = 960_000 bytes.
	decision, _ := service.routeModel(context.Background(), planTranscript(planEnteredMarker), 1_200_000)
	if !decision.overrides() || decision.Model != "big/wide" {
		t.Fatalf("got (%q, %q, %v), want big/wide", decision.Model, decision.Reason, decision.overrides())
	}
	if decision.Reason != "plan turn exceeds the plan model's window" {
		t.Fatalf("reason = %q", decision.Reason)
	}
}

func TestSetLongContextRejectsAnUnusableShare(t *testing.T) {
	service := routingService(nil)
	for _, percent := range []int{0, -5, 100, 1_000} {
		service.SetLongContext("big/wide", percent)
		if _, got := service.LongContext(); got != defaultLongContextPercent {
			t.Fatalf("percent %d stored as %d, want the default %d", percent, got, defaultLongContextPercent)
		}
	}
	service.SetLongContext("big/wide", 45)
	if _, got := service.LongContext(); got != 45 {
		t.Fatalf("valid percent stored as %d", got)
	}
}

// Configuring the rule is what lets the client use the room it actually has: without it
// one global setting has to hold for the smallest window in the card.
func TestClientContextCeilingLiftsToTheLongContextModel(t *testing.T) {
	service := routingService(map[string]int{
		"cheap/small": 200_000, "mid/model": 400_000, "big/wide": 1_000_000,
	})
	models := []string{"cheap/small", "mid/model"}
	if got := service.ClientContextCeiling(context.Background(), models); got != 200_000 {
		t.Fatalf("without the rule: got %d, want the smallest window 200000", got)
	}
	service.SetLongContext("big/wide", 60)
	if got := service.ClientContextCeiling(context.Background(), models); got != 1_000_000 {
		t.Fatalf("with the rule: got %d, want 1000000", got)
	}
	// A long-context model that is not actually roomier must not raise the ceiling.
	service.SetLongContext("mid/model", 60)
	if got := service.ClientContextCeiling(context.Background(), models); got != 400_000 {
		t.Fatalf("got %d, want 400000", got)
	}
}

// A window learned from an upstream rejection has to move the threshold with it, or the
// rule keeps measuring against a limit the model was proven not to have.
func TestLongContextThresholdFollowsTheLearnedWindow(t *testing.T) {
	service := routingService(map[string]int{"cheap/small": 400_000, "big/wide": 1_000_000})
	service.SetLongContext("big/wide", 60)
	request := planTranscript("x")
	// 60% of 400k is 240k tokens = 960_000 bytes; 800_000 stays put.
	if decision, _ := service.routeModel(context.Background(), request, 800_000); decision.overrides() {
		t.Fatal("under the threshold at the catalog window")
	}
	service.learnContextWindow("cheap/small", 513_800,
		contextRejection("prompt is too long: 513800 tokens > 200000 tokens"))
	// Now 60% of 200k is 120k tokens = 480_000 bytes, so the same turn routes.
	decision, _ := service.routeModel(context.Background(), request, 800_000)
	if !decision.overrides() || decision.Model != "big/wide" {
		t.Fatalf("got (%q, %v), want big/wide once the window is known to be smaller", decision.Model, decision.overrides())
	}
}
