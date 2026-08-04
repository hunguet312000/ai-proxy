package gateway

import (
	"context"
	"testing"
)

func TestTokenScaleFallsBackBeforeAnythingIsMeasured(t *testing.T) {
	service := New(Options{})
	// 4 bytes per token and an estimate taken at face value: exactly what the code did
	// before scales existed, so an unmeasured decision.Model behaves as it always has.
	if got := service.tokensFromBytes("unknown/decision.Model", 400_000); got != 100_000 {
		t.Fatalf("tokensFromBytes = %d, want 100000", got)
	}
	if got := service.estimateBudgetForTokens("unknown/decision.Model", 250_000); got != 250_000 {
		t.Fatalf("estimateBudgetForTokens = %d, want 250000", got)
	}
}

// The numbers here are the ones measured against the live upstream: a 1.9MB body that
// contextguard.EstimateRequest read as 633,361 and the upstream counted as 350,018.
func TestTokenScaleLearnsFromARealTurn(t *testing.T) {
	service := New(Options{})
	service.observeTokenScale("cx/gpt-5.6-sol", 1_900_108, 633_361, 350_018)

	// 1_900_108 / 350_018 ≈ 5.43 bytes per token, not 4.
	if got := service.tokensFromBytes("cx/gpt-5.6-sol", 1_900_108); got < 348_000 || got > 352_000 {
		t.Fatalf("tokensFromBytes = %d, want ≈350018", got)
	}
	// 633_361 / 350_018 ≈ 1.81, so a 200k-token budget is 362k of estimate.
	got := service.estimateBudgetForTokens("cx/gpt-5.6-sol", 200_000)
	if got < 355_000 || got > 370_000 {
		t.Fatalf("estimateBudgetForTokens = %d, want ≈362000", got)
	}
	// The family shares a tokenizer, so the review variant inherits the measurement.
	if got := service.tokensFromBytes("cx/gpt-5.6-sol-review", 1_900_108); got < 348_000 || got > 352_000 {
		t.Fatalf("review variant = %d, want the family scale", got)
	}
}

func TestTokenScaleRejectsUnusableSamples(t *testing.T) {
	service := New(Options{})
	cases := []struct {
		name                         string
		rawBytes, estimate, reported int
	}{
		// Dominated by the fixed cost of system prompt and tool schemas, so its ratio says
		// nothing about the large turns these scales are used to judge.
		{"too small to be representative", 40_000, 12_000, 1_200},
		// 100 bytes per token is not a tokenizer; something truncated or mis-parsed.
		{"impossible bytes per token", 2_000_000, 600_000, 20_000},
		{"no reported count", 2_000_000, 600_000, 0},
		{"empty body", 0, 600_000, 300_000},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service.observeTokenScale("m", testCase.rawBytes, testCase.estimate, testCase.reported)
			if got := service.tokensFromBytes("m", 400_000); got != 100_000 {
				t.Fatalf("sample was accepted: tokensFromBytes = %d, want the 100000 fallback", got)
			}
		})
	}
}

// One unusual turn — a screenshot, a wall of base64 — must not swing the scale, but a
// session that genuinely changes character has to be tracked.
func TestTokenScaleSmoothsSuccessiveSamples(t *testing.T) {
	service := New(Options{})
	service.observeTokenScale("m", 1_000_000, 250_000, 250_000) // 4.0 bytes/token
	service.observeTokenScale("m", 1_000_000, 250_000, 125_000) // 8.0 bytes/token
	// 4.0 blended 30% toward 8.0 is 5.2, not 8.0.
	scale := service.tokenScaleFor("m")
	if scale.bytesPerToken < 5.0 || scale.bytesPerToken > 5.4 {
		t.Fatalf("bytesPerToken = %.2f, want ≈5.2", scale.bytesPerToken)
	}
	for attempt := 0; attempt < 12; attempt++ {
		service.observeTokenScale("m", 1_000_000, 250_000, 125_000)
	}
	if scale := service.tokenScaleFor("m"); scale.bytesPerToken < 7.7 {
		t.Fatalf("bytesPerToken = %.2f, want it converged near 8.0", scale.bytesPerToken)
	}
}

// The payoff: a measured scale changes which decision.Model serves a turn, because the threshold is
// finally compared against something close to what the upstream will count.
func TestMeasuredScaleChangesTheRoutingDecision(t *testing.T) {
	service := routingService(map[string]int{"cheap/small": 200_000, "big/wide": 1_000_000})
	service.SetLongContext("big/wide", 60)
	request := planTranscript("x")
	// 60% of 200k is 120k tokens. At the 4-bytes fallback a 600KB body reads as 150k and
	// routes away.
	if decision, _ := service.routeModel(context.Background(), request, 600_000); !decision.overrides() {
		t.Fatal("at the fallback scale this turn should route")
	}
	// Measured at 5.43 bytes per token the same body is only ~110k tokens, under the
	// threshold, and belongs on the model the client asked for.
	service.observeTokenScale("cheap/small", 1_900_108, 633_361, 350_018)
	if decision, _ := service.routeModel(context.Background(), request, 600_000); decision.overrides() {
		t.Fatalf("routed to %q; the measured scale puts this turn under the threshold", decision.Model)
	}
}
