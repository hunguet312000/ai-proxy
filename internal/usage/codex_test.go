package usage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"literouter/internal/pool"
	"literouter/internal/secret"
	"literouter/internal/storage"
)

func TestParseCodexQuotaIncludesResetCredits(t *testing.T) {
	quota, err := ParseCodexQuota([]byte(`{
	  "plan_type":"plus",
	  "rate_limit":{"primary_window":{"used_percent":10,"reset_at":"2026-08-01T00:00:00Z"}},
	  "rate_limit_reset_credits":{"available_count":2}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if quota.ResetCredits != 2 {
		t.Fatalf("ResetCredits = %d", quota.ResetCredits)
	}
}

func TestParseCodexQuota(t *testing.T) {
	quota, err := ParseCodexQuota([]byte(`{
  "plan_type": "plus",
  "rate_limit": {
    "limit_reached": false,
    "primary_window": {"used_percent": 25, "reset_at": 1700000000},
    "secondary_window": {"used_percent": 80, "reset_at": "2026-08-01T00:00:00Z"}
  },
  "code_review_rate_limit": {
    "primary_window": {"used_percent": 100, "reset_at": 1700000100}
  }
}`))
	if err != nil {
		t.Fatalf("ParseCodexQuota() error = %v", err)
	}
	if quota.Plan != "plus" || quota.LimitReached || len(quota.Windows) != 3 {
		t.Fatalf("ParseCodexQuota() = %#v", quota)
	}
	if quota.Windows[0].Key != "session" || quota.Windows[0].RemainingPercent != 75 {
		t.Fatalf("session window = %#v", quota.Windows[0])
	}
	if quota.Windows[2].Key != "review_session" || !quota.Windows[2].Exhausted {
		t.Fatalf("review window = %#v", quota.Windows[2])
	}
}

func TestTrackerPersistsAndUpdatesPool(t *testing.T) {
	box, err := secret.New(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(t.TempDir(), "tracker.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	account := storage.Account{ID: "codex:account", Provider: "codex", Credentials: []byte(`{}`), Enabled: true, Weight: 1}
	if err := store.CreateAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	accountPool := pool.New([]pool.Account{{ID: account.ID, Provider: account.Provider}})
	tracker := NewTracker(store, accountPool, fakeFetcher{quota: Quota{
		Provider: "codex", FetchedAt: time.Now().UTC(),
		Windows: []Window{percentageWindow("session", 100, time.Now().Add(time.Hour))},
	}})
	if _, err := tracker.Refresh(context.Background(), account, "token", "account"); err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.ListQuotaSnapshots(context.Background(), account.ID)
	if err != nil || len(snapshots) != 1 || !snapshots[0].Exhausted {
		t.Fatalf("snapshots = %#v, error = %v", snapshots, err)
	}
	pooled, _ := accountPool.Get(account.ID)
	if !pooled.QuotaExhausted || pooled.QuotaRemainingPercent != 0 {
		t.Fatalf("pool account = %#v", pooled)
	}
}

func TestTrackerRejectsEmptyQuotaWithoutDeletingSnapshots(t *testing.T) {
	box, err := secret.New(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(t.TempDir(), "empty.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	account := storage.Account{ID: "codex:account", Provider: "codex", Credentials: []byte(`{}`), Enabled: true, Weight: 1}
	if err := store.CreateAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	existing := storage.QuotaSnapshot{AccountID: account.ID, Key: "session", Source: "codex", RemainingPercent: 50, FetchedAt: time.Now().UTC()}
	if err := store.ReplaceQuotaSnapshots(context.Background(), account.ID, []storage.QuotaSnapshot{existing}); err != nil {
		t.Fatal(err)
	}
	tracker := NewTracker(store, nil, fakeFetcher{quota: Quota{Provider: "codex", FetchedAt: time.Now().UTC()}})
	if _, err := tracker.Refresh(context.Background(), account, "token", "account"); err == nil {
		t.Fatal("Refresh() error = nil")
	}
	snapshots, err := store.ListQuotaSnapshots(context.Background(), account.ID)
	if err != nil || len(snapshots) != 1 || snapshots[0].RemainingPercent != 50 {
		t.Fatalf("snapshots = %#v, error = %v", snapshots, err)
	}
}

func TestSummarizeIgnoresReviewOnlyExhaustion(t *testing.T) {
	remaining, exhausted, _ := summarize(Quota{Windows: []Window{
		{Key: "session", RemainingPercent: 75},
		{Key: "review_session", RemainingPercent: 0, Exhausted: true},
	}})
	if remaining != 75 || exhausted {
		t.Fatalf("summarize() = %v, %v", remaining, exhausted)
	}
}

func TestSummarizePrepaidOverridesExhaustedWindows(t *testing.T) {
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	remaining, exhausted, resetAt := summarize(Quota{Windows: []Window{
		{Key: "monthly", RemainingPercent: 0, Exhausted: true, ResetAt: reset},
		{Key: "prepaid", Remaining: 10, RemainingPercent: 100},
	}})
	if remaining != 0 || exhausted || !resetAt.Equal(reset) {
		t.Fatalf("summarize() = %v, %v, %v", remaining, exhausted, resetAt)
	}
}

func TestSummarizeKeepsResetWhenHealthy(t *testing.T) {
	reset := time.Now().Add(3*24*time.Hour + 17*time.Hour + 25*time.Minute).UTC().Truncate(time.Second)
	remaining, exhausted, resetAt := summarize(Quota{Windows: []Window{
		{Key: "session", RemainingPercent: 51, Used: 49, Total: 100, ResetAt: reset},
		{Key: "weekly", RemainingPercent: 80, Used: 20, Total: 100, ResetAt: reset.Add(24 * time.Hour)},
	}})
	if remaining != 51 || exhausted || !resetAt.Equal(reset) {
		t.Fatalf("summarize() = %v, %v, %v", remaining, exhausted, resetAt)
	}
}

type fakeFetcher struct{ quota Quota }

func (f fakeFetcher) Provider() string                                     { return "codex" }
func (f fakeFetcher) Fetch(context.Context, string, string) (Quota, error) { return f.quota, nil }

func TestCodexFetcherHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("ChatGPT-Account-ID") != "account" {
			t.Errorf("headers = %#v", r.Header)
		}
		_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":5}}}`)
	}))
	defer server.Close()
	fetcher := NewCodexFetcher(server.Client())
	fetcher.url = server.URL

	quota, err := fetcher.Fetch(context.Background(), "token", "account")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(quota.Windows) != 1 || time.Since(quota.FetchedAt) > time.Second {
		t.Fatalf("Fetch() = %#v", quota)
	}
}
