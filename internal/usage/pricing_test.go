package usage

import (
	"math"
	"testing"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-12
}

func TestNormalizeModelID(t *testing.T) {
	tests := map[string]string{
		"cx/gpt-5.6-sol":        "gpt-5.6-sol",
		"cx/gpt-5.6-sol-review": "gpt-5.6-sol",
		"xai/grok-4.5":          "grok-4.5",
		"claude-sonnet-5":       "claude-sonnet-5",
		"gpt-5.6-sol (high)":    "gpt-5.6-sol",
	}
	for in, want := range tests {
		if got := NormalizeModelID(in); got != want {
			t.Fatalf("NormalizeModelID(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRatesForModelExactAndPattern(t *testing.T) {
	sol := RatesForModel("cx/gpt-5.6-sol")
	if sol.Input != 5.0 || sol.Output != 30.0 || sol.Cached != 0.5 {
		t.Fatalf("sol rates = %+v", sol)
	}
	// review maps to base
	rev := RatesForModel("cx/gpt-5.6-sol-review")
	if rev != sol {
		t.Fatalf("review rates = %+v want %+v", rev, sol)
	}
	grok := RatesForModel("xai/grok-4.5")
	if grok.Input != 0.5 || grok.Output != 2.0 {
		t.Fatalf("grok rates = %+v", grok)
	}
	// pattern gpt-5.6-*
	lunaLike := RatesForModel("gpt-5.6-unknown-tier")
	if lunaLike.Input != 2.5 || lunaLike.Output != 15 {
		t.Fatalf("gpt-5.6-* pattern = %+v", lunaLike)
	}
	// haiku pattern
	h := RatesForModel("claude-haiku-4-5-20251001")
	if h.Input != 1.0 || h.Output != 5.0 || h.Cached != 0.1 {
		t.Fatalf("haiku = %+v", h)
	}
}

func TestCalculateCostUSDOpenAIStyle(t *testing.T) {
	// gpt-5.6-sol: in 5, out 30, cached 0.5 per 1M
	// prompt=1000 (includes 200 cached), completion=100
	// nonCached=800 → 800*5/1e6 = 0.004
	// cached=200 → 200*0.5/1e6 = 0.0001
	// out=100 → 100*30/1e6 = 0.003
	// total = 0.0071
	u := TokenUsage{PromptTokens: 1000, CompletionTokens: 100, CachedTokens: 200}
	got := CalculateCostUSD("cx/gpt-5.6-sol", u)
	want := 0.0071
	if !almostEqual(got, want) {
		t.Fatalf("cost=%v want %v", got, want)
	}
}

func TestCalculateCostUSDClaudeFold(t *testing.T) {
	// Claude reports prompt excluding cache.
	// input=800, cache_read=200, cache_creation=0, output=100
	// after fold prompt=1000, cached=200
	// sonnet: in 3, out 15, cached 0.3
	// nonCached=800 → 800*3/1e6=0.0024
	// cached=200 → 200*0.3/1e6=0.00006
	// out=100 → 100*15/1e6=0.0015
	// total=0.00396
	got := EstimateCostClaudeUSD("claude-sonnet-5", 800, 100, 200, 0, 0)
	want := 0.00396
	if !almostEqual(got, want) {
		t.Fatalf("claude cost=%v want %v", got, want)
	}
}

func TestEstimateCostUSDMatchesCalculate(t *testing.T) {
	got := EstimateCostUSD("cx/gpt-5.6-sol", 26, 5, 0)
	// 26*5/1e6 + 5*30/1e6 = 0.00013 + 0.00015 = 0.00028
	want := 0.00028
	if !almostEqual(got, want) {
		t.Fatalf("estimate=%v want %v", got, want)
	}
}

func TestMatchGlob(t *testing.T) {
	if !matchGlob("gpt-5.6-*", "gpt-5.6-sol") {
		t.Fatal("expected match gpt-5.6-*")
	}
	if !matchGlob("*-codex", "gpt-5.3-codex") {
		t.Fatal("expected match *-codex")
	}
	if matchGlob("gpt-5.6-*", "gpt-5.3-codex") {
		t.Fatal("should not match")
	}
	if !matchGlob("grok-*", "grok-4.5") {
		t.Fatal("grok match")
	}
}
