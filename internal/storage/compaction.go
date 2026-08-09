package storage

import (
	"context"
	"fmt"
	"time"
)

// PromptSizeBucket is how a model behaved over one band of prompt sizes. The band is
// the unit the compaction advisor reasons in: cache reuse does not decay smoothly, it
// falls off a cliff once a conversation stops fitting whatever the upstream keeps warm,
// and that cliff is only visible when requests are grouped by size.
type PromptSizeBucket struct {
	// Floor is the inclusive lower bound of the band, in prompt tokens.
	Floor int
	// Requests counts events in the band. A band with too few is not evidence.
	Requests int
	// PromptTokens and CachedTokens are the sums the upstream reported.
	PromptTokens int64
	CachedTokens int64
	// Reported counts events where the upstream reported any cache at all. A provider
	// that never reports it produces CachedTokens == 0, which must not be read as "the
	// cache never hits" — it means nothing was measured.
	Reported int
}

// ModelPromptProfile is the measured behaviour of one model.
type ModelPromptProfile struct {
	Provider string
	Model    string
	Buckets  []PromptSizeBucket
	// Unsized counts successful requests with no recorded prompt size. They cannot join
	// a size band, but they are the difference between what the usage table shows and
	// what this analysis could use — reporting zero of them where 21 exist reads as
	// "this model barely ran".
	Unsized int
}

// promptSizeFloors are the band edges, in tokens. They are denser where the decision
// lives: between 50k and 200k is where the advisor has to place a threshold, and a
// coarser grid there would hide the cliff it is looking for.
var promptSizeFloors = []int{0, 25_000, 50_000, 75_000, 100_000, 125_000, 150_000, 200_000, 250_000, 300_000}

// PromptProfiles reports per-model prompt-size behaviour since a point in time.
// Only successful events are counted: a rejected request tells you nothing about how
// the model would have used its cache.
func (s *Store) PromptProfiles(ctx context.Context, since time.Time) ([]ModelPromptProfile, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(provider,''), COALESCE(model,''), prompt_tokens, cached_tokens
FROM usage_events
WHERE ts >= ? AND status = 'ok'`, since.UTC().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("prompt profiles: %w", err)
	}
	defer rows.Close()

	type key struct{ provider, model string }
	collected := map[key][]PromptSizeBucket{}
	unsized := map[key]int{}
	for rows.Next() {
		var provider, model string
		var prompt, cached int64
		if err := rows.Scan(&provider, &model, &prompt, &cached); err != nil {
			return nil, err
		}
		if model == "" {
			continue
		}
		id := key{provider, model}
		if prompt <= 0 {
			unsized[id]++
			continue
		}
		buckets, ok := collected[id]
		if !ok {
			buckets = make([]PromptSizeBucket, len(promptSizeFloors))
			for index, floor := range promptSizeFloors {
				buckets[index].Floor = floor
			}
		}
		index := bucketIndex(int(prompt))
		buckets[index].Requests++
		buckets[index].PromptTokens += prompt
		buckets[index].CachedTokens += cached
		if cached > 0 {
			buckets[index].Reported++
		}
		collected[id] = buckets
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]ModelPromptProfile, 0, len(collected))
	for id, buckets := range collected {
		out = append(out, ModelPromptProfile{
			Provider: id.provider, Model: id.model, Buckets: buckets, Unsized: unsized[id],
		})
		delete(unsized, id)
	}
	// A model whose every request lacked a prompt size still deserves a row: silence
	// would look like it had never run.
	for id, count := range unsized {
		out = append(out, ModelPromptProfile{Provider: id.provider, Model: id.model, Unsized: count})
	}
	return out, nil
}

// PromptSizeFloors are the candidate thresholds, in tokens. A caller choosing a window
// has to weigh every band, not only the ones a given model happened to land in.
func PromptSizeFloors() []int {
	out := make([]int, len(promptSizeFloors))
	copy(out, promptSizeFloors)
	return out
}

func bucketIndex(prompt int) int {
	index := 0
	for i, floor := range promptSizeFloors {
		if prompt >= floor {
			index = i
		}
	}
	return index
}
