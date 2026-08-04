package usage

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"literouter/internal/pool"
	"literouter/internal/storage"
)

type Window struct {
	Key              string    `json:"key"`
	Used             float64   `json:"used"`
	Total            float64   `json:"total"`
	Remaining        float64   `json:"remaining"`
	RemainingPercent float64   `json:"remaining_percent"`
	ResetAt          time.Time `json:"reset_at,omitempty"`
	Unlimited        bool      `json:"unlimited"`
	Exhausted        bool      `json:"exhausted"`
}

type Quota struct {
	Provider     string    `json:"provider"`
	Plan         string    `json:"plan,omitempty"`
	LimitReached bool      `json:"limit_reached"`
	ResetCredits int       `json:"reset_credits,omitempty"`
	Windows      []Window  `json:"windows"`
	FetchedAt    time.Time `json:"fetched_at"`
}

type Fetcher interface {
	Provider() string
	Fetch(ctx context.Context, accessToken, accountID string) (Quota, error)
}

type Tracker struct {
	store    *storage.Store
	pool     *pool.Pool
	fetchers map[string]Fetcher
}

func NewTracker(store *storage.Store, accountPool *pool.Pool, fetchers ...Fetcher) *Tracker {
	tracker := &Tracker{store: store, pool: accountPool, fetchers: make(map[string]Fetcher, len(fetchers))}
	for _, fetcher := range fetchers {
		tracker.fetchers[fetcher.Provider()] = fetcher
	}
	return tracker
}

func (t *Tracker) Refresh(ctx context.Context, account storage.Account, accessToken, providerAccountID string) (Quota, error) {
	fetcher := t.fetchers[account.Provider]
	if fetcher == nil {
		return Quota{}, fmt.Errorf("quota tracking is not supported for provider %q", account.Provider)
	}
	quota, err := fetcher.Fetch(ctx, accessToken, providerAccountID)
	if err != nil {
		return Quota{}, err
	}
	if len(quota.Windows) == 0 {
		return Quota{}, fmt.Errorf("%s quota response has no windows", account.Provider)
	}
	snapshots := make([]storage.QuotaSnapshot, 0, len(quota.Windows))
	for _, window := range quota.Windows {
		snapshots = append(snapshots, storage.QuotaSnapshot{
			AccountID: account.ID, Key: window.Key, Used: window.Used, Total: window.Total,
			Remaining: window.Remaining, RemainingPercent: window.RemainingPercent,
			ResetAt: window.ResetAt, Unlimited: window.Unlimited, Exhausted: window.Exhausted,
			Source: fetcher.Provider(), FetchedAt: quota.FetchedAt,
		})
	}
	if err := t.store.ReplaceQuotaSnapshots(ctx, account.ID, snapshots); err != nil {
		return Quota{}, err
	}
	if plan := strings.TrimSpace(quota.Plan); plan != "" && plan != account.Plan {
		if err := t.store.UpdateAccountPlan(ctx, account.ID, plan); err != nil {
			return Quota{}, err
		}
	}
	if t.pool != nil {
		poolSnapshots := make([]pool.QuotaSnapshot, 0, len(snapshots))
		for _, snapshot := range snapshots {
			poolSnapshots = append(poolSnapshots, pool.QuotaSnapshot{
				Key: snapshot.Key, Remaining: snapshot.Remaining, RemainingPercent: snapshot.RemainingPercent,
				ResetAt: snapshot.ResetAt, FetchedAt: snapshot.FetchedAt, Unlimited: snapshot.Unlimited, Exhausted: snapshot.Exhausted,
			})
		}
		t.pool.RestoreQuota(account.ID, poolSnapshots)
		t.pool.UpdateMetadata(account.ID, "", strings.TrimSpace(quota.Plan))
		t.pool.UpdateResetCredits(account.ID, quota.ResetCredits, true)
	}
	return quota, nil
}

func summarize(quota Quota) (remaining float64, exhausted bool, resetAt time.Time) {
	return summarizeWindows(quota.LimitReached, quota.Windows)
}

func summarizeWindows(limitReached bool, windows []Window) (remaining float64, exhausted bool, resetAt time.Time) {
	remaining = 100
	exhausted = limitReached
	var hasPrepaid bool
	for _, window := range windows {
		if window.Key == "prepaid" && window.Remaining > 0 {
			hasPrepaid = true
		}
		if window.Unlimited || window.Key == "prepaid" || strings.HasPrefix(window.Key, "review_") || strings.HasPrefix(window.Key, "weekly_") {
			continue
		}
		if window.RemainingPercent < remaining {
			remaining = window.RemainingPercent
		}
		if window.Exhausted {
			exhausted = true
		}
		if !window.ResetAt.IsZero() && (resetAt.IsZero() || window.ResetAt.Before(resetAt)) {
			resetAt = window.ResetAt
		}
	}
	if hasPrepaid {
		exhausted = false
	}
	return remaining, exhausted, resetAt
}

func percentageWindow(key string, usedPercent float64, resetAt time.Time) Window {
	used := math.Max(0, math.Min(100, usedPercent))
	remaining := 100 - used
	return Window{
		Key: key, Used: used, Total: 100, Remaining: remaining,
		RemainingPercent: remaining, ResetAt: resetAt, Exhausted: remaining <= 0,
	}
}
