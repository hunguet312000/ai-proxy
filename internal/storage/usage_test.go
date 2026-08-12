package storage

import (
	"context"
	"testing"
	"time"
)

func TestUsageEventsSummary(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if err := store.InsertUsageEvent(ctx, UsageEvent{
			Timestamp: now.Add(-time.Duration(i) * time.Minute),
			Provider:  "codex", Model: "cx/gpt-5.6-sol", Endpoint: "/v1/chat/completions", Status: "ok",
			PromptTokens: 100, CompletionTokens: 50, CachedTokens: 10, CostUSD: 0.01,
			PromptTokensEstimated: i == 0, CachedTokensReported: i != 1,
			Effort: "max",
		}); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := store.UsageSummary(ctx, now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 3 || sum.PromptTokens != 300 || len(sum.ByProvider) != 1 || len(sum.Recent) != 3 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.ByProvider[0].Provider != "codex" || sum.ByModel[0].Model != "cx/gpt-5.6-sol" {
		t.Fatalf("breakdown = %+v %+v", sum.ByProvider, sum.ByModel)
	}
	if !sum.Recent[0].PromptTokensEstimated || !sum.Recent[0].CachedTokensReported || sum.Recent[1].CachedTokensReported {
		t.Fatalf("usage provenance = %+v", sum.Recent)
	}
	// The effort has to survive the round trip, because it is the only record of what a
	// turn was actually sent at — the dashboard's column reads straight off this row.
	if sum.Recent[0].Effort != "max" {
		t.Fatalf("recorded effort = %q, want max", sum.Recent[0].Effort)
	}
}

// The floor is only as good as what it excludes. Both exclusions cost real quota to
// learn: an estimated count read 1.8x high on one measured payload, and a failed turn is
// recorded with an estimated prompt size too, which is how apparent successes past 600k
// once appeared in this table and nearly became the believed window.
func TestLargestServedPromptsIgnoresEstimatesAndFailures(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	events := []UsageEvent{
		{Model: "cx/gpt-5.6-luna", Status: "ok", PromptTokens: 300_000},
		{Model: "cx/gpt-5.6-luna", Status: "ok", PromptTokens: 372_860},
		{Model: "cx/gpt-5.6-luna", Status: "ok", PromptTokens: 900_000, PromptTokensEstimated: true},
		{Model: "cx/gpt-5.6-luna", Status: "context_overflow", PromptTokens: 800_000},
		{Model: "cx/gpt-5.6-sol", Status: "ok", PromptTokens: 120_000},
		{Model: "", Status: "ok", PromptTokens: 500_000},
	}
	for index, event := range events {
		event.Timestamp = now.Add(-time.Duration(index) * time.Minute)
		if err := store.InsertUsageEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}

	largest, err := store.LargestServedPrompts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := largest["cx/gpt-5.6-luna"]; got != 372_860 {
		t.Fatalf("luna floor = %d, want the largest upstream-counted success", got)
	}
	if got := largest["cx/gpt-5.6-sol"]; got != 120_000 {
		t.Fatalf("sol floor = %d, want 120000", got)
	}
	if _, ok := largest[""]; ok {
		t.Fatal("a blank model produced a floor")
	}
}

func TestDeleteUsageEventsBeforeBatchesAndPreservesBoundary(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	cutoff := time.Now().UTC().Truncate(time.Millisecond)
	for _, timestamp := range []time.Time{
		cutoff.Add(-3 * time.Hour), cutoff.Add(-2 * time.Hour), cutoff.Add(-time.Hour),
		cutoff, cutoff.Add(time.Hour),
	} {
		if err := store.InsertUsageEvent(ctx, UsageEvent{Timestamp: timestamp, Model: timestamp.String()}); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := store.DeleteUsageEventsBefore(ctx, cutoff, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 3 {
		t.Fatalf("deleted = %d, want 3", deleted)
	}
	summary, err := store.UsageSummary(ctx, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 2 {
		t.Fatalf("remaining requests = %d, want 2", summary.Requests)
	}
	for _, event := range summary.Recent {
		if event.Timestamp.Before(cutoff) {
			t.Fatalf("expired event remained: %s", event.Timestamp)
		}
	}
}

func TestDeleteUsageEventsBeforeHonorsCanceledContext(t *testing.T) {
	store := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deleted, err := store.DeleteUsageEventsBefore(ctx, time.Now().UTC(), 1)
	if err == nil || deleted != 0 {
		t.Fatalf("DeleteUsageEventsBefore() = %d, %v", deleted, err)
	}
}

func TestInsertUsageEventsEmptyBatch(t *testing.T) {
	store := openTestStore(t)
	if err := store.InsertUsageEvents(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}
