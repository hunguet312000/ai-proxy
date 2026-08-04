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
