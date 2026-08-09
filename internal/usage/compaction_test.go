package usage

import (
	"strings"
	"testing"

	"literouter/internal/storage"
)

// profile builds a model profile from (floor, requests, hitRate) triples. Cache figures
// are marked reported unless the hit rate is exactly zero and reported is forced off.
func profile(provider, model string, reported bool, bands ...[3]float64) storage.ModelPromptProfile {
	out := storage.ModelPromptProfile{Provider: provider, Model: model}
	for _, band := range bands {
		floor, requests, hit := int(band[0]), int(band[1]), band[2]
		average := float64(floor) + 10_000
		prompt := int64(average * float64(requests))
		bucket := storage.PromptSizeBucket{
			Floor: floor, Requests: requests,
			PromptTokens: prompt, CachedTokens: int64(float64(prompt) * hit),
		}
		if reported {
			bucket.Reported = requests
		}
		out.Buckets = append(out.Buckets, bucket)
	}
	return out
}

func TestAdviseRecommendsTheSizeWhereCacheReuseCollapses(t *testing.T) {
	// Reuse holds around 30% up to 100k and halves past 150k, which is the point where
	// carrying more history starts being billed in full on every turn.
	model := profile("codex", "m", true,
		[3]float64{0, 500, 0.30},
		[3]float64{50_000, 300, 0.29},
		[3]float64{100_000, 200, 0.28},
		[3]float64{150_000, 300, 0.12},
		[3]float64{250_000, 300, 0.08},
	)
	advice := adviseModel(model, providerPrior{}, 400_000)
	if advice.RecommendedWindow != 150_000 {
		t.Fatalf("recommended %d, want 150000 (%s)", advice.RecommendedWindow, advice.Status)
	}
	if advice.EstimatedSaving <= 0 {
		t.Errorf("a recommendation must come with an estimated saving, got %v", advice.EstimatedSaving)
	}
}

func TestAdviseDeclinesWhenTheUpstreamNeverReportsCache(t *testing.T) {
	// Antigravity reports a cache figure on 18 of 2017 requests here. Reading that as a
	// 0% hit rate would recommend aggressive compaction on no evidence at all, throwing
	// away context for a saving that was never measured.
	model := profile("antigravity", "m", false,
		[3]float64{0, 800, 0},
		[3]float64{100_000, 400, 0},
		[3]float64{200_000, 400, 0},
	)
	advice := adviseModel(model, providerPrior{}, 400_000)
	if advice.RecommendedWindow == 0 {
		t.Fatalf("no recommendation despite 1600 requests: %s", advice.Status)
	}
	if !advice.SavingIsUpperBound {
		t.Error("without cache figures the saving must be marked an upper bound")
	}
	if !strings.Contains(advice.Status, "no cache figures from upstream") {
		t.Errorf("status = %q, want it to name what is missing", advice.Status)
	}
	if strings.HasPrefix(advice.Status, "0 ") {
		t.Errorf("status = %q, want it not to open with a zero that reads as no traffic", advice.Status)
	}
}

func TestAdviseDeclinesUntilThereAreEnoughLargeRequests(t *testing.T) {
	// A model can have thousands of small requests and still know nothing about how it
	// behaves at the sizes where the decision is made.
	model := profile("codex", "m", true,
		[3]float64{0, 5_000, 0.30},
		[3]float64{150_000, 10, 0.05},
	)
	advice := adviseModel(model, providerPrior{}, 400_000)
	if advice.RecommendedWindow != 0 {
		t.Fatalf("recommended %d from 10 large requests", advice.RecommendedWindow)
	}
	if !strings.Contains(advice.Status, "learning") {
		t.Errorf("status = %q, want it to report the evidence still missing", advice.Status)
	}
}

func TestAdviseReportsNoTrafficRatherThanGuessing(t *testing.T) {
	advice := adviseModel(storage.ModelPromptProfile{Provider: "codex", Model: "new"}, providerPrior{}, 400_000)
	if advice.RecommendedWindow != 0 || !strings.Contains(advice.Status, "no traffic") {
		t.Fatalf("advice = %+v, want an explicit no-traffic verdict", advice)
	}
}

func TestAdviseUsesTheProviderPriorForAThinModel(t *testing.T) {
	// A new model of a known provider starts from that provider's measured curve. Its
	// own thin numbers pull the estimate as they accumulate rather than replacing it,
	// so one unlucky band cannot move the threshold on its own.
	prior := providerPrior{
		hitByBucket: []float64{0.30, 0.30, 0.30, 0.10, 0.08},
		requests:    5_000, reported: 5_000, total: 5_000,
	}
	thin := profile("codex", "new", true,
		[3]float64{0, 200, 0.05},
		[3]float64{50_000, 200, 0.05},
		[3]float64{100_000, 200, 0.05},
		[3]float64{150_000, 200, 0.05},
		[3]float64{250_000, 200, 0.05},
	)
	advice := adviseModel(thin, prior, 400_000)
	// Blended against the prior, the low early bands do not collapse the model's best
	// band, so the cliff still lands where the provider's curve puts it.
	if advice.RecommendedWindow != 150_000 {
		t.Fatalf("recommended %d, want the provider curve's 150000 (%s)", advice.RecommendedWindow, advice.Status)
	}
}

func TestAdviseSaysNothingWhenReuseHoldsAtEverySize(t *testing.T) {
	model := profile("xai", "m", true,
		[3]float64{0, 500, 0.80},
		[3]float64{100_000, 300, 0.78},
		[3]float64{250_000, 300, 0.76},
	)
	advice := adviseModel(model, providerPrior{}, 200_000)
	if advice.RecommendedWindow != 0 {
		t.Fatalf("recommended %d though reuse never collapsed", advice.RecommendedWindow)
	}
	if !strings.Contains(advice.Status, "holds at every observed size") {
		t.Errorf("status = %q", advice.Status)
	}
}

func TestAdviseRespectsTheDeadbandAndTheDeclaredWindow(t *testing.T) {
	model := profile("codex", "m", true,
		[3]float64{0, 500, 0.30},
		[3]float64{100_000, 300, 0.28},
		[3]float64{150_000, 300, 0.10},
		[3]float64{250_000, 300, 0.08},
	)
	// Already compacting near the cliff: moving a few thousand tokens is noise.
	if advice := adviseModel(model, providerPrior{}, 160_000); advice.RecommendedWindow != 0 {
		t.Errorf("recommended %d inside the deadband (%s)", advice.RecommendedWindow, advice.Status)
	}
	// A window already at or below the cliff has nothing to gain.
	if advice := adviseModel(model, providerPrior{}, 150_000); advice.RecommendedWindow != 0 {
		t.Errorf("recommended %d at or below the cliff (%s)", advice.RecommendedWindow, advice.Status)
	}
}

func TestEstimateSavingIgnoresTrafficBelowTheThreshold(t *testing.T) {
	model := profile("codex", "m", true,
		[3]float64{0, 100, 0.30},
		[3]float64{250_000, 100, 0.10},
	)
	saving := estimateSaving(model, 150_000)
	if saving <= 0 || saving >= 1 {
		t.Fatalf("saving = %v, want a fraction of billed tokens", saving)
	}
	// Compacting at a threshold above everything observed saves nothing.
	if got := estimateSaving(model, 1_000_000); got != 0 {
		t.Errorf("saving = %v above all observed traffic, want 0", got)
	}
}

func TestAdviseSaysTheRecommendationIsAlreadyApplied(t *testing.T) {
	// Landing here usually means the operator just applied the recommendation. Saying
	// "nothing to gain" reads as "this was never worth doing" and hides the number they
	// captured, so the status names the point they are now compacting at.
	model := profile("codex", "m", true,
		[3]float64{0, 500, 0.30},
		[3]float64{100_000, 300, 0.28},
		[3]float64{150_000, 300, 0.12},
		[3]float64{250_000, 300, 0.08},
	)
	advice := adviseModel(model, providerPrior{}, 150_000)
	if advice.RecommendedWindow != 0 {
		t.Fatalf("recommended %d when already at the cliff", advice.RecommendedWindow)
	}
	if !strings.Contains(advice.Status, "already compacting near 150k") {
		t.Errorf("status = %q, want it to name where the model now compacts", advice.Status)
	}
	if advice.CliffWindow != 150_000 {
		t.Errorf("cliff window = %d, want it reported even without a recommendation", advice.CliffWindow)
	}
}

func TestAdviceAccountsForRequestsWithNoRecordedPromptSize(t *testing.T) {
	// The usage table showed 31 Cursor requests while the advisor said "10 requests",
	// because rows recorded before the input estimate landed carry no prompt size and
	// cannot join a size band. Hiding them made the model look barely used.
	model := profile("cursor", "composer", false, [3]float64{0, 10, 0})
	model.Unsized = 21

	advice := adviseModel(model, providerPrior{}, 200_000)
	if advice.UnsizedRequests != 21 {
		t.Fatalf("unsized = %d, want 21", advice.UnsizedRequests)
	}
	if !strings.Contains(advice.Status, "+21 with no recorded prompt size") {
		t.Errorf("status = %q, want the skipped requests accounted for", advice.Status)
	}
}

func TestAdviceStillReportsAModelWhoseRequestsAllLackAPromptSize(t *testing.T) {
	// Silence would be indistinguishable from "never ran".
	advice := adviseModel(storage.ModelPromptProfile{Provider: "cursor", Model: "m", Unsized: 7}, providerPrior{}, 0)
	if strings.Contains(advice.Status, "no traffic") {
		t.Fatalf("status = %q, want it to distinguish unmeasurable traffic from none", advice.Status)
	}
	if !strings.Contains(advice.Status, "7 requests seen") {
		t.Errorf("status = %q, want the request count reported", advice.Status)
	}
}

func TestAdviseReportsTheHeavyShareWhenCacheCannotBeMeasured(t *testing.T) {
	// 786 requests with no cache figures is not a dead end: prompt sizes still say how
	// much of the cost is history, which is what decides whether the model deserves
	// attention at all.
	model := profile("antigravity", "gemini", false,
		[3]float64{0, 400, 0},
		[3]float64{200_000, 386, 0},
	)
	advice := adviseModel(model, providerPrior{}, 0)
	if advice.HeavyShare <= 0.5 {
		t.Errorf("heavy share = %.2f, want most of the input attributed to large conversations", advice.HeavyShare)
	}
	// The recommendation is still made — an operator cannot know whether an upstream
	// caches, and withholding advice on that basis leaves a busy model with nothing
	// forever. What the missing figure changes is the saving's status, not the threshold.
	if advice.RecommendedWindow == 0 {
		t.Fatalf("no recommendation for a model with 786 requests: %s", advice.Status)
	}
	if !advice.SavingIsUpperBound {
		t.Error("a saving computed without cache figures must be marked an upper bound")
	}
	if !strings.Contains(advice.Status, "at most") {
		t.Errorf("status = %q, want the saving qualified", advice.Status)
	}
}

func TestAdviseRecommendsOnceTheOperatorDeclaresAProviderUncached(t *testing.T) {
	// The operator can turn "not reported" into "not cached". That is a claim only they
	// can make, so it is stated in configuration rather than inferred from silence.
	t.Setenv("LITEROUTER_COMPACTION_ASSUME_NO_CACHE", "antigravity")
	model := profile("antigravity", "gemini", false,
		[3]float64{0, 400, 0},
		[3]float64{200_000, 386, 0},
	)
	advice := adviseModel(model, providerPrior{}, 400_000)
	if advice.RecommendedWindow == 0 {
		t.Fatalf("no recommendation (%s)", advice.Status)
	}
	if advice.SavingIsUpperBound {
		t.Error("a declared-uncached provider bills every trimmed token, so the saving is exact")
	}
	if !strings.Contains(advice.Status, "declared uncached") {
		t.Errorf("status = %q, want the assumption attributed to the operator", advice.Status)
	}
}

func TestAdviseIgnoresADeclarationForADifferentProvider(t *testing.T) {
	t.Setenv("LITEROUTER_COMPACTION_ASSUME_NO_CACHE", "openai")
	model := profile("antigravity", "gemini", false,
		[3]float64{0, 400, 0},
		[3]float64{200_000, 386, 0},
	)
	// A declaration for one provider must not silently make another's saving look exact.
	advice := adviseModel(model, providerPrior{}, 400_000)
	if !advice.SavingIsUpperBound {
		t.Fatal("a provider that was not declared must keep the caveat on its saving")
	}
}

func TestAdviseDeclaredUncachedButSmallConversationsGetsNothing(t *testing.T) {
	// Nothing to compact is a real answer, and better than a threshold that would only
	// churn summaries.
	t.Setenv("LITEROUTER_COMPACTION_ASSUME_NO_CACHE", "antigravity")
	model := profile("antigravity", "gemini", false, [3]float64{0, 900, 0})
	advice := adviseModel(model, providerPrior{}, 400_000)
	if advice.RecommendedWindow != 0 || !strings.Contains(advice.Status, "little history to compact") {
		t.Fatalf("advice = %+v, want no recommendation for small conversations", advice)
	}
}
