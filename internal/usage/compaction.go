package usage

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"literouter/internal/storage"
)

// Compaction advice answers one question per model: at what conversation size does
// carrying more history stop paying for itself?
//
// The reason it can be answered at all is that upstream prompt caches do not decay
// smoothly. Measured on this deployment, cx/gpt-5.6-sol reuses 28.9% of a prompt below
// 50k and 7.4% above 300k — past a point, every extra token of history is billed at
// full price on every subsequent turn. Compacting before that point is the saving;
// compacting after it is what the default (window − 13k) does.
//
// What this deliberately does not do is guess. A model with no evidence keeps its
// declared window, because lowering a window forces earlier compaction, and earlier
// compaction discards context the user cannot get back. The failure mode of a wrong
// recommendation is silent lost work, so the bar for making one is high — the same
// reasoning the catalog already applies to max_output_tokens.
const (
	// minBucketRequests is the evidence needed in a single size band before that band
	// can move a threshold. Small samples in the large bands are common and noisy.
	minBucketRequests = 40
	// MinLargeRequests is the evidence needed above the smallest bands. A model with
	// ten thousand tiny requests knows nothing about how it behaves at 200k. Exported
	// so a caller can explain the bar rather than restate the number.
	MinLargeRequests = 120
	// minReportedFraction is the share of requests that must carry a cache figure
	// before "no cache" can be read as measurement rather than as a reporting gap.
	// Antigravity reports on 18 of 2017 events here; treating that as a 0% hit rate
	// would recommend aggressive compaction on no evidence at all.
	minReportedFraction = 0.25
	// decayFactor is how far cache reuse must fall from the model's own best band
	// before extra history is judged not to pay for itself.
	decayFactor = 0.5
	// shrinkage is the weight given to the provider-level prior, in requests. A model
	// with `shrinkage` requests of its own is trusted half on its own numbers and half
	// on its provider's; the pull fades as its own evidence grows.
	shrinkage = 200
	// deadband keeps a recommendation from flapping between neighbouring bands, and is
	// also what counts as "already there" once one has been applied.
	deadband = 20_000
	// referenceWindow is the size above which a conversation is "carrying history" for
	// the purpose of the heavy-share figure. It sits where recommendations usually land,
	// so the share answers "how much of my cost is history I could compact away".
	referenceWindow = 150_000
	// minMeaningfulSaving is how much input a threshold must cut before it is worth
	// trading context for. Below this the summarisation request and the lost history
	// cost more than the tokens saved.
	minMeaningfulSaving = 0.15
	// floorWindow is the smallest window worth recommending. Below it, compaction runs
	// so often that the summaries themselves dominate the cost.
	floorWindow = 60_000
)

// CompactionAdvice is one model's recommendation.
type CompactionAdvice struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// RecommendedWindow is the context window to advertise so the client compacts at
	// the right point. Zero means "no recommendation" — see Status.
	RecommendedWindow int `json:"recommended_window"`
	CurrentWindow     int `json:"current_window"`
	// Status is why there is or is not a recommendation, in the operator's terms.
	Status string `json:"status"`
	// Reason is the same verdict in a form callers can group on. Grouping on Status
	// would mean matching prose, which changes whenever the wording does.
	Reason Reason `json:"reason"`
	// Requests and LargeRequests are the evidence behind the verdict.
	Requests      int `json:"requests"`
	LargeRequests int `json:"large_requests"`
	// ReportedRequests is how many of them carried a cache figure at all.
	ReportedRequests int `json:"reported_requests"`
	// UnsizedRequests ran but recorded no prompt size, so they could not be analysed.
	UnsizedRequests int `json:"unsized_requests"`
	// SavingIsUpperBound marks a saving computed without cache figures. The token
	// reduction is exact; how much of it turns into money depends on reuse the upstream
	// never reported, so the real saving is at most this.
	SavingIsUpperBound bool `json:"saving_is_upper_bound"`
	// HeavyShare is the share of this model's input tokens that came from requests
	// above referenceWindow. It needs no cache figures, so it is the one thing still
	// measurable when the upstream reports none — and it is what says whether history
	// is where the model's cost actually lives.
	HeavyShare float64 `json:"heavy_share"`
	// BestHitRate and CliffHitRate are the cache reuse at the model's best band and at
	// the band where the recommendation lands.
	BestHitRate  float64 `json:"best_hit_rate"`
	CliffHitRate float64 `json:"cliff_hit_rate"`
	// CliffWindow is where reuse collapses, reported even when it is already applied so
	// the figure stays visible rather than disappearing with the recommendation.
	CliffWindow int `json:"cliff_window"`
	// EstimatedSaving is the share of billed prompt tokens that compacting at the
	// recommended point would have avoided, over the observed traffic.
	EstimatedSaving float64 `json:"estimated_saving"`
}

// Reason is why a model did or did not get a recommendation.
type Reason string

const (
	ReasonRecommended Reason = "recommended"
	ReasonApplied     Reason = "applied"
	ReasonNoTraffic   Reason = "no-traffic"
	ReasonNoCacheData Reason = "no-cache-reporting"
	ReasonLearning    Reason = "learning"
	ReasonCacheHolds  Reason = "cache-holds"
	ReasonCacheAbsent Reason = "cache-absent"
)

// assumeNoCache lists providers the operator has declared uncached, via
// LITEROUTER_COMPACTION_ASSUME_NO_CACHE=antigravity,openai
//
// It no longer gates the recommendation — an operator cannot be expected to know
// whether an upstream caches, and withholding advice until they say so meant a model
// with 786 requests never got any. All it does now is drop the "at most" caveat from
// the saving, because a declared-uncached provider bills every trimmed token.
func assumeNoCache(provider string) bool {
	declared := strings.TrimSpace(os.Getenv("LITEROUTER_COMPACTION_ASSUME_NO_CACHE"))
	if declared == "" {
		return false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	for _, name := range strings.Split(declared, ",") {
		if strings.ToLower(strings.TrimSpace(name)) == provider && provider != "" {
			return true
		}
	}
	return false
}

// Advisor turns recorded usage into compaction advice.
type Advisor struct {
	store *storage.Store
}

func NewAdvisor(store *storage.Store) *Advisor { return &Advisor{store: store} }

// Advise reports one entry per model seen since the cutoff, including the models it
// declines to advise on and why.
func (a *Advisor) Advise(ctx context.Context, since time.Time, windows map[string]int) ([]CompactionAdvice, error) {
	if a == nil || a.store == nil {
		return nil, fmt.Errorf("compaction advisor has no store")
	}
	profiles, err := a.store.PromptProfiles(ctx, since)
	if err != nil {
		return nil, err
	}
	priors := providerPriors(profiles)

	out := make([]CompactionAdvice, 0, len(profiles))
	for _, profile := range profiles {
		advice := adviseModel(profile, priors[profile.Provider], windows[profile.Model])
		out = append(out, advice)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LargeRequests != out[j].LargeRequests {
			return out[i].LargeRequests > out[j].LargeRequests
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

// providerPrior is the pooled behaviour of every model of one provider, used as the
// starting point for a model that has not yet earned its own numbers.
type providerPrior struct {
	hitByBucket []float64
	requests    int
	reported    int
	total       int
}

func providerPriors(profiles []storage.ModelPromptProfile) map[string]providerPrior {
	pooled := map[string][]storage.PromptSizeBucket{}
	for _, profile := range profiles {
		buckets, ok := pooled[profile.Provider]
		if !ok {
			buckets = make([]storage.PromptSizeBucket, len(profile.Buckets))
		}
		for index, bucket := range profile.Buckets {
			if index >= len(buckets) {
				break
			}
			buckets[index].Floor = bucket.Floor
			buckets[index].Requests += bucket.Requests
			buckets[index].PromptTokens += bucket.PromptTokens
			buckets[index].CachedTokens += bucket.CachedTokens
			buckets[index].Reported += bucket.Reported
		}
		pooled[profile.Provider] = buckets
	}
	out := map[string]providerPrior{}
	for provider, buckets := range pooled {
		prior := providerPrior{hitByBucket: make([]float64, len(buckets))}
		for index, bucket := range buckets {
			prior.hitByBucket[index] = hitRate(bucket)
			prior.requests += bucket.Requests
			prior.reported += bucket.Reported
			prior.total += bucket.Requests
		}
		out[provider] = prior
	}
	return out
}

func hitRate(bucket storage.PromptSizeBucket) float64 {
	if bucket.PromptTokens <= 0 {
		return 0
	}
	return float64(bucket.CachedTokens) / float64(bucket.PromptTokens)
}

// adviseModel is the whole decision for one model.
func adviseModel(profile storage.ModelPromptProfile, prior providerPrior, currentWindow int) CompactionAdvice {
	advice := CompactionAdvice{
		Provider: profile.Provider, Model: profile.Model, CurrentWindow: currentWindow,
	}
	reported := 0
	for index, bucket := range profile.Buckets {
		advice.Requests += bucket.Requests
		reported += bucket.Reported
		if bucket.Floor >= 100_000 {
			advice.LargeRequests += bucket.Requests
		}
		_ = index
	}

	advice.UnsizedRequests = profile.Unsized
	advice.HeavyShare = heavyShare(profile)
	if advice.Requests == 0 {
		if profile.Unsized > 0 {
			advice.Status = fmt.Sprintf("%d requests seen, none with a recorded prompt size — nothing to measure",
				profile.Unsized)
			advice.Reason = ReasonNoCacheData
			return advice
		}
		advice.Status = "no traffic yet"
		advice.Reason = ReasonNoTraffic
		return advice
	}
	// A provider that does not report cache figures makes every model under it look
	// perfectly uncached. That is a measurement gap, not a finding, and acting on it
	// would compact aggressively for no reason.
	coverage := float64(reported) / float64(advice.Requests)
	priorCoverage := 0.0
	if prior.total > 0 {
		priorCoverage = float64(prior.reported) / float64(prior.total)
	}
	if coverage < minReportedFraction && priorCoverage < minReportedFraction {
		// Worded to say what is missing, not what is absent: "(0 of 10 requests)" was
		// read as "this model served no traffic", when it means the traffic carried no
		// cache figures.
		// Cache reuse cannot be measured, but the shape of the traffic still can. Saying
		// only "nothing to measure" throws away the one fact that decides whether this
		// model is worth attention at all.
		measured := fmt.Sprintf("%d requests seen, no cache figures from upstream", advice.Requests)
		if reported > 0 {
			measured = fmt.Sprintf("%d requests seen, only %d carrying a cache figure",
				advice.Requests, reported)
		}
		if profile.Unsized > 0 {
			measured += fmt.Sprintf(" (+%d with no recorded prompt size)", profile.Unsized)
		}
		// Cache reuse decides how much of a trimmed token turns into money, not whether
		// trimming helps: a shorter prompt is fewer tokens either way. So the threshold
		// is still computable from sizes alone, and only the saving carries a caveat.
		return adviseBySize(advice, profile, currentWindow, measured)
		advice.Reason = ReasonNoCacheData
		advice.ReportedRequests = reported
		return advice
	}
	if advice.LargeRequests < MinLargeRequests {
		advice.Status = fmt.Sprintf("learning: %d/%d large requests observed", advice.LargeRequests, MinLargeRequests)
		advice.Reason = ReasonLearning
		return advice
	}

	// Blend each band toward the provider's pooled behaviour, weighted by how much
	// evidence this model has of its own. A new model of a known provider therefore
	// starts from that provider's curve instead of from nothing.
	blended := make([]float64, len(profile.Buckets))
	for index, bucket := range profile.Buckets {
		own := hitRate(bucket)
		weight := float64(bucket.Requests)
		priorHit := 0.0
		if index < len(prior.hitByBucket) {
			priorHit = prior.hitByBucket[index]
		}
		blended[index] = (weight*own + shrinkage*priorHit) / (weight + shrinkage)
	}

	best := 0.0
	for index, bucket := range profile.Buckets {
		if bucket.Requests >= minBucketRequests && blended[index] > best {
			best = blended[index]
		}
	}
	if best <= 0 {
		advice.Status = "cache reuse measured at zero across every size — compaction cannot help"
		advice.Reason = ReasonCacheAbsent
		return advice
	}
	advice.BestHitRate = best

	// The recommendation is the first band where reuse has fallen by decayFactor: past
	// it, history is billed in full on every turn.
	cliff := 0
	for index, bucket := range profile.Buckets {
		if bucket.Floor == 0 || bucket.Requests < minBucketRequests {
			continue
		}
		if blended[index] < best*decayFactor {
			cliff = bucket.Floor
			advice.CliffHitRate = blended[index]
			break
		}
	}
	if cliff == 0 {
		advice.Status = "cache reuse holds at every observed size — no earlier compaction needed"
		advice.Reason = ReasonCacheHolds
		return advice
	}
	if cliff < floorWindow {
		cliff = floorWindow
	}
	advice.CliffWindow = cliff
	if currentWindow > 0 && currentWindow-cliff < deadband {
		// Naming the number matters: "nothing to gain" reads as "this was never worth
		// doing", when the usual reason for landing here is that the recommendation was
		// already applied.
		advice.Status = fmt.Sprintf("already compacting near %dk, where reuse falls to %.0f%%",
			currentWindow/1000, advice.CliffHitRate*100)
		advice.Reason = ReasonApplied
		return advice
	}
	advice.RecommendedWindow = cliff
	advice.EstimatedSaving = estimateSaving(profile, cliff)
	advice.Status = fmt.Sprintf("compact near %dk: reuse falls from %.0f%% to %.0f%%",
		cliff/1000, best*100, advice.CliffHitRate*100)
	advice.Reason = ReasonRecommended
	return advice
}

// adviseBySize recommends from prompt sizes alone, for the upstreams that report no
// cache at all. Nobody can tell from outside whether such a provider caches — asking the
// operator to declare it just moves an unanswerable question — so the recommendation is
// made on what is knowable and the uncertainty is put on the saving instead.
//
// The objective is the largest window that still cuts a meaningful share of input:
// keeping context is the thing being traded away, so it is spent only where the return
// is real.
func adviseBySize(advice CompactionAdvice, profile storage.ModelPromptProfile,
	currentWindow int, measured string) CompactionAdvice {
	// Every band is a candidate, not only the ones this model landed in: the best
	// threshold is often a size the model never ran at, which is exactly the point of
	// moving it there.
	best, bestSaving := 0, 0.0
	for _, floor := range storage.PromptSizeFloors() {
		if floor < floorWindow {
			continue
		}
		saving := estimateSaving(profile, floor)
		if saving >= minMeaningfulSaving && floor > best {
			best, bestSaving = floor, saving
		}
	}
	if best == 0 {
		advice.Status = fmt.Sprintf("%s — no window would cut even %.0f%% of its input, so there is little history to compact",
			measured, minMeaningfulSaving*100)
		advice.Reason = ReasonCacheHolds
		return advice
	}
	advice.CliffWindow = best
	if currentWindow > 0 && currentWindow-best < deadband {
		advice.Status = fmt.Sprintf("already compacting near %dk", currentWindow/1000)
		advice.Reason = ReasonApplied
		return advice
	}
	advice.RecommendedWindow = best
	advice.EstimatedSaving = bestSaving
	advice.SavingIsUpperBound = !assumeNoCache(profile.Provider)
	advice.Reason = ReasonRecommended
	if advice.SavingIsUpperBound {
		advice.Status = fmt.Sprintf("%s — compacting near %dk would cut %.0f%% of its input; the billed saving is at most that, less if this upstream caches",
			measured, best/1000, bestSaving*100)
	} else {
		advice.Status = fmt.Sprintf("declared uncached: compacting near %dk cuts %.0f%% of its input",
			best/1000, bestSaving*100)
	}
	return advice
}

// heavyShare is the fraction of a model's input tokens that arrived in conversations
// past referenceWindow. Unlike cache reuse it is derived from prompt sizes alone, so it
// survives an upstream that reports no cache at all.
func heavyShare(profile storage.ModelPromptProfile) float64 {
	var total, heavy int64
	for _, bucket := range profile.Buckets {
		total += bucket.PromptTokens
		if bucket.Floor >= referenceWindow {
			heavy += bucket.PromptTokens
		}
	}
	if total <= 0 {
		return 0
	}
	return float64(heavy) / float64(total)
}

// estimateSaving is the share of billed prompt tokens that would not have been billed
// had conversations been compacted at the threshold.
//
// It is an upper bound on the traffic that was actually observed, not a forecast: it
// assumes a compacted conversation would have cost the threshold rather than its real
// size, and ignores the cost of the summarisation requests themselves.
func estimateSaving(profile storage.ModelPromptProfile, threshold int) float64 {
	var billed, saved float64
	for _, bucket := range profile.Buckets {
		if bucket.Requests == 0 {
			continue
		}
		bucketBilled := float64(bucket.PromptTokens - bucket.CachedTokens)
		billed += bucketBilled
		if bucket.Floor < threshold {
			continue
		}
		average := float64(bucket.PromptTokens) / float64(bucket.Requests)
		if average <= float64(threshold) {
			continue
		}
		saved += bucketBilled * (1 - float64(threshold)/average)
	}
	if billed <= 0 {
		return 0
	}
	return saved / billed
}
