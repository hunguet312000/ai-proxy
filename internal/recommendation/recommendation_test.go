package recommendation

import (
	"context"
	"errors"
	"testing"
	"time"

	"literouter/internal/pool"
	"literouter/internal/storage"
)

func TestRecommendRanksAvailableModelsAndAppliesFilters(t *testing.T) {
	now := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	service := New(Config{
		Pool: pool.New([]pool.Account{
			{ID: "codex-1", Provider: "codex", Enabled: true, Models: []string{"cx/gpt-5.6-sol"}, QuotaRemainingPercent: 80, QuotaUpdatedAt: now},
			{ID: "claude-1", Provider: "claude", Enabled: true, Models: []string{"claude-sonnet-5"}, QuotaRemainingPercent: 90, QuotaUpdatedAt: now},
		}),
		Catalog: func(context.Context, string) ([]storage.CatalogModel, error) {
			return []storage.CatalogModel{
				{Provider: "codex", ID: "cx/gpt-5.6-sol", ContextWindow: 400_000},
				{Provider: "claude", ID: "claude-sonnet-5", ContextWindow: 1_000_000},
				{Provider: "xai", ID: "xai/grok-4"},
			}, nil
		},
		Now: func() time.Time { return now },
	})

	response, err := service.Recommend(context.Background(), Query{Task: "coding", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 2 || response.Filters.Task != "coding" {
		t.Fatalf("response = %+v", response)
	}
	if response.Data[0].Model != "cx/gpt-5.6-sol" || !response.Data[0].Available {
		t.Fatalf("top recommendation = %+v", response.Data[0])
	}

	response, err = service.Recommend(context.Background(), Query{Provider: "claude", MinContext: 500_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 1 || response.Data[0].Model != "claude-sonnet-5" {
		t.Fatalf("filtered response = %+v", response)
	}
}

func TestRecommendExcludesActiveQuotaAndKeepsExpiredQuotaAvailable(t *testing.T) {
	now := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	service := New(Config{
		Pool: pool.New([]pool.Account{
			{ID: "active", Provider: "codex", Enabled: true, Models: []string{"cx/blocked"}, ModelQuotas: map[string]pool.QuotaSnapshot{
				"cx/blocked": {RemainingPercent: 0, Exhausted: true, ResetAt: now.Add(time.Hour), FetchedAt: now},
			}},
			{ID: "expired", Provider: "codex", Enabled: true, Models: []string{"cx/recovered"}, ModelQuotas: map[string]pool.QuotaSnapshot{
				"cx/recovered": {RemainingPercent: 0, Exhausted: true, ResetAt: now.Add(-time.Hour), FetchedAt: now},
			}},
		}),
		Catalog: func(context.Context, string) ([]storage.CatalogModel, error) {
			return []storage.CatalogModel{
				{Provider: "codex", ID: "cx/blocked"},
				{Provider: "codex", ID: "cx/recovered"},
			}, nil
		},
		Now: func() time.Time { return now },
	})

	response, err := service.Recommend(context.Background(), Query{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	items := map[string]Item{}
	for _, item := range response.Data {
		items[item.Model] = item
	}
	if items["cx/blocked"].Available || !items["cx/recovered"].Available {
		t.Fatalf("quota availability = %+v", items)
	}
}

func TestRecommendValidationAndCatalogErrors(t *testing.T) {
	service := New(Config{Catalog: func(context.Context, string) ([]storage.CatalogModel, error) {
		return nil, errors.New("database unavailable")
	}})
	if _, err := service.Recommend(context.Background(), Query{Limit: 51}); err == nil {
		t.Fatal("limit validation succeeded")
	}
	if _, err := service.Recommend(context.Background(), Query{Task: "unknown"}); err == nil {
		t.Fatal("task validation succeeded")
	}
	if _, err := service.Recommend(context.Background(), Query{}); err == nil {
		t.Fatal("catalog error was ignored")
	}
}
