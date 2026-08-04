package pool

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSelectorStrategies(t *testing.T) {
	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	accounts := []Account{
		{ID: "a", Provider: "codex", Enabled: true, Weight: 1},
		{ID: "b", Provider: "codex", Enabled: true, Weight: 2},
		{ID: "c", Provider: "claude", Enabled: true, Weight: 1},
	}
	tests := []struct {
		name     string
		strategy SelectionStrategy
		prepare  func(*Selector)
		request  SelectRequest
		want     string
	}{
		{"round robin", StrategyRoundRobin, nil, SelectRequest{Provider: "codex"}, "a"},
		{"weighted", StrategyWeighted, nil, SelectRequest{Provider: "codex"}, "b"},
		{"failover", StrategyFailover, nil, SelectRequest{Provider: "codex"}, "b"},
		{"least tokens", StrategyLeastUsed, func(s *Selector) { s.ReportSuccess("a", 100) }, SelectRequest{Provider: "codex"}, "b"},
		{"least rpm", StrategyLeastUsedRPM, func(s *Selector) { _, _ = s.Select(SelectRequest{Provider: "codex"}) }, SelectRequest{Provider: "codex"}, "b"},
		{"smart weight", StrategySmart, nil, SelectRequest{Provider: "codex"}, "b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selector := NewSelector(New(accounts), test.strategy, nil)
			selector.now = func() time.Time { return base }
			if test.prepare != nil {
				test.prepare(selector)
			}
			result, err := selector.Select(test.request)
			if err != nil || result.Account.ID != test.want {
				t.Fatalf("Select() = %#v, %v; want %s", result, err, test.want)
			}
		})
	}
}

func TestSelectorRoundRobinAndWeightedSequences(t *testing.T) {
	accounts := []Account{{ID: "a", Enabled: true, Weight: 1}, {ID: "b", Enabled: true, Weight: 2}}
	for _, test := range []struct {
		strategy SelectionStrategy
		want     []string
	}{
		{StrategyRoundRobin, []string{"a", "b", "a", "b"}},
		{StrategyWeighted, []string{"b", "a", "b", "b", "a", "b"}},
	} {
		selector := NewSelector(New(accounts), test.strategy, nil)
		for index, want := range test.want {
			result, err := selector.Select(SelectRequest{})
			if err != nil || result.Account.ID != want {
				t.Fatalf("%s pick %d = %#v, %v; want %s", test.strategy, index, result, err, want)
			}
		}
	}
}

func TestSelectorStickyAndAliasFallback(t *testing.T) {
	accounts := []Account{
		{ID: "a", Enabled: true, Models: []string{"fallback"}},
		{ID: "b", Enabled: true, Models: []string{"fallback"}},
	}
	selector := NewSelector(New(accounts), StrategySticky, map[string][]string{"alias": {"primary", "fallback"}})
	first, err := selector.Select(SelectRequest{Model: "alias", ConversationID: "conversation"})
	if err != nil || first.ResolvedModel != "fallback" {
		t.Fatalf("Select() = %#v, %v", first, err)
	}
	second, _ := selector.Select(SelectRequest{Model: "alias", ConversationID: "conversation"})
	if second.Account.ID != first.Account.ID {
		t.Fatalf("sticky accounts = %s, %s", first.Account.ID, second.Account.ID)
	}
}

func TestSelectorExcludesFailedAccount(t *testing.T) {
	selector := NewSelector(New([]Account{{ID: "a", Enabled: true}, {ID: "b", Enabled: true}}), StrategySmart, nil)
	result, err := selector.Select(SelectRequest{ExcludeIDs: map[string]struct{}{"a": {}}})
	if err != nil || result.Account.ID != "b" {
		t.Fatalf("Select() = %#v, %v", result, err)
	}
}

func TestSelectorFiltersAndRecovery(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	p := New([]Account{
		{ID: "a", Enabled: true, MaxRequestsPerMinute: 1},
		{ID: "b", Enabled: true, QuotaExhausted: true, QuotaResetAt: now.Add(time.Hour)},
	})
	selector := NewSelector(p, StrategySmart, nil)
	selector.now = func() time.Time { return now }
	selected, err := selector.Select(SelectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !selector.CommitRequest(selected.ReservationID) {
		t.Fatal("CommitRequest() = false")
	}
	selector.ReportSuccess("a", 10)
	if _, err := selector.Select(SelectRequest{}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("Select() error = %v", err)
	}
	now = now.Add(time.Minute + time.Second)
	result, err := selector.Select(SelectRequest{})
	if err != nil || result.Account.ID != "a" {
		t.Fatalf("Select() = %#v, %v", result, err)
	}
}

func TestSelectorFiltersAntigravityQuotaByModel(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	selector := NewSelector(New([]Account{
		{ID: "a", Provider: "antigravity", Enabled: true, ModelQuotas: map[string]QuotaSnapshot{
			"gemini-pro-agent":     {Key: "gemini-pro-agent", Exhausted: true, ResetAt: now.Add(time.Hour)},
			"gemini-3-flash-agent": {Key: "gemini-3-flash-agent", RemainingPercent: 80},
		}},
		{ID: "b", Provider: "antigravity", Enabled: true, ModelQuotas: map[string]QuotaSnapshot{
			"gemini-pro-agent": {Key: "gemini-pro-agent", RemainingPercent: 50},
		}},
	}), StrategyFailover, nil)
	selector.now = func() time.Time { return now }

	result, err := selector.Select(SelectRequest{Provider: "antigravity", Model: "gemini-pro-agent"})
	if err != nil || result.Account.ID != "b" {
		t.Fatalf("pro Select() = %#v, %v", result, err)
	}
	result, err = selector.Select(SelectRequest{Provider: "antigravity", Model: "gemini-3-flash-agent"})
	if err != nil || result.Account.ID != "a" {
		t.Fatalf("flash Select() = %#v, %v", result, err)
	}
}

func TestSelectorAntigravityRateLimitIsModelScoped(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	selector := NewSelector(New([]Account{{ID: "a", Provider: "antigravity", Enabled: true}}), StrategySmart, nil)
	selector.now = func() time.Time { return now }
	selector.ReportModelRateLimit("a", "gemini-pro-agent")
	if _, err := selector.Select(SelectRequest{Provider: "antigravity", Model: "gemini-pro-agent"}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("rate-limited model error = %v", err)
	}
	if _, err := selector.Select(SelectRequest{Provider: "antigravity", Model: "gemini-3-flash-agent"}); err != nil {
		t.Fatalf("other model error = %v", err)
	}
	now = now.Add(30 * time.Second)
	if _, err := selector.Select(SelectRequest{Provider: "antigravity", Model: "gemini-pro-agent"}); err != nil {
		t.Fatalf("post-cooldown model error = %v", err)
	}
}

func TestSelectorRateLimitAndCircuitBreaker(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	selector := NewSelector(New([]Account{{ID: "a", Enabled: true}}), StrategySmart, nil)
	selector.now = func() time.Time { return now }
	selector.ReportRateLimit("a")
	if _, err := selector.Select(SelectRequest{}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("cooldown Select() error = %v", err)
	}
	now = now.Add(30 * time.Second)
	if _, err := selector.Select(SelectRequest{}); err != nil {
		t.Fatalf("post-cooldown Select() error = %v", err)
	}
	selector.ReportSuccess("a", 0)
	for range 5 {
		selector.ReportError("a")
	}
	if _, err := selector.Select(SelectRequest{}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("open-circuit Select() error = %v", err)
	}
	now = now.Add(3 * time.Minute)
	if _, err := selector.Select(SelectRequest{}); err != nil {
		t.Fatalf("post-circuit Select() error = %v", err)
	}
}

func TestSelectorUsesQuotaAndNormalizesStrategy(t *testing.T) {
	selector := NewSelector(New([]Account{
		{ID: "low", Enabled: true, Weight: 1, QuotaRemainingPercent: 1, QuotaUpdatedAt: time.Now()},
		{ID: "high", Enabled: true, Weight: 1, QuotaRemainingPercent: 99, QuotaUpdatedAt: time.Now()},
	}), "SMART", nil)
	result, err := selector.Select(SelectRequest{})
	if err != nil || result.Account.ID != "high" {
		t.Fatalf("Select() = %#v, %v", result, err)
	}
}

func TestSelectorCircuitNeedsFiveNewErrorsAfterRecovery(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	selector := NewSelector(New([]Account{{ID: "a", Enabled: true}}), StrategySmart, nil)
	selector.now = func() time.Time { return now }
	for range 5 {
		selector.ReportError("a")
	}
	now = now.Add(3 * time.Minute)
	selector.ReportError("a")
	if _, err := selector.Select(SelectRequest{}); err != nil {
		t.Fatalf("Select() after one new error = %v", err)
	}
}

func TestSelectorSetStrategyAppliesImmediately(t *testing.T) {
	selector := NewSelector(New([]Account{{ID: "a", Enabled: true, Weight: 1}, {ID: "b", Enabled: true, Weight: 2}}), StrategySmart, nil)
	first, err := selector.Select(SelectRequest{})
	if err != nil || first.Account.ID != "b" {
		t.Fatalf("smart Select() = %#v, %v", first, err)
	}
	selector.SetStrategy(StrategyRoundRobin)
	if selector.Strategy() != StrategyRoundRobin {
		t.Fatalf("Strategy() = %q", selector.Strategy())
	}
	for index, want := range []string{"a", "b", "a"} {
		result, err := selector.Select(SelectRequest{})
		if err != nil || result.Account.ID != want {
			t.Fatalf("round robin pick %d = %#v, %v; want %s", index, result, err, want)
		}
	}
}

func TestSelectorConcurrent(t *testing.T) {
	selector := NewSelector(New([]Account{{ID: "a", Enabled: true}, {ID: "b", Enabled: true}}), StrategyRoundRobin, nil)
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := selector.Select(SelectRequest{})
			if err == nil {
				selector.ReportSuccess(result.Account.ID, 1)
			}
		}()
	}
	wait.Wait()
}

func TestSelectorStickySoftAffinityAndPressureMigrate(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	accounts := []Account{
		{ID: "a", Provider: "codex", Enabled: true, Weight: 1, QuotaRemainingPercent: 90, QuotaUpdatedAt: now},
		{ID: "b", Provider: "codex", Enabled: true, Weight: 1, QuotaRemainingPercent: 90, QuotaUpdatedAt: now},
	}
	selector := NewSelector(New(accounts), StrategyStickySoft, nil)
	selector.now = func() time.Time { return now }

	first, err := selector.Select(SelectRequest{Provider: "codex", ConversationID: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := selector.Select(SelectRequest{Provider: "codex", ConversationID: "sess-1"})
	if err != nil || second.Account.ID != first.Account.ID {
		t.Fatalf("affinity broke: first=%s second=%#v err=%v", first.Account.ID, second, err)
	}

	// Other session can bind a different account without disturbing sess-1.
	other, err := selector.Select(SelectRequest{Provider: "codex", ConversationID: "sess-2"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := selector.Select(SelectRequest{Provider: "codex", ConversationID: "sess-1"})
	if err != nil || again.Account.ID != first.Account.ID {
		t.Fatalf("sess-1 migrated unexpectedly to %s (other=%s)", again.Account.ID, other.Account.ID)
	}

	// Drive preferred account into soft quota pressure; peer stays healthy.
	preferred := first.Account.ID
	peer := "b"
	if preferred == "b" {
		peer = "a"
	}
	p := selector.pool
	acc, _ := p.Get(preferred)
	acc.QuotaRemainingPercent = 10
	acc.QuotaUpdatedAt = now
	p.Upsert(acc)
	peerAcc, _ := p.Get(peer)
	peerAcc.QuotaRemainingPercent = 95
	peerAcc.QuotaUpdatedAt = now
	p.Upsert(peerAcc)

	// Soft migrate is suppressed during the post-bind cooldown (tool-loop pin).
	now = now.Add(stickySoftMigrateCooldown + time.Second)
	migrated, err := selector.Select(SelectRequest{Provider: "codex", ConversationID: "sess-1"})
	if err != nil || migrated.Account.ID != peer {
		t.Fatalf("expected migrate to %s, got %#v err=%v", peer, migrated, err)
	}
	// Affinity must stick on the new account after migration.
	after, err := selector.Select(SelectRequest{Provider: "codex", ConversationID: "sess-1"})
	if err != nil || after.Account.ID != peer {
		t.Fatalf("post-migrate affinity = %#v err=%v", after, err)
	}
}

func TestSelectorStickySoftKeepsPreferredWithoutHealthierPeer(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	accounts := []Account{
		{ID: "a", Enabled: true, Weight: 1, QuotaRemainingPercent: 10, QuotaUpdatedAt: now},
		{ID: "b", Enabled: true, Weight: 1, QuotaRemainingPercent: 5, QuotaUpdatedAt: now},
	}
	selector := NewSelector(New(accounts), StrategyStickySoft, nil)
	selector.now = func() time.Time { return now }
	first, err := selector.Select(SelectRequest{ConversationID: "solo"})
	if err != nil {
		t.Fatal(err)
	}
	// Both under pressure — stay on bound account rather than thrashing.
	second, err := selector.Select(SelectRequest{ConversationID: "solo"})
	if err != nil || second.Account.ID != first.Account.ID {
		t.Fatalf("thrash under mutual pressure: first=%s second=%#v err=%v", first.Account.ID, second, err)
	}
}

func TestSelectorStickySoftHardFailExcludesAndRebinds(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	selector := NewSelector(New([]Account{
		{ID: "a", Enabled: true, Weight: 1, QuotaRemainingPercent: 80, QuotaUpdatedAt: now},
		{ID: "b", Enabled: true, Weight: 1, QuotaRemainingPercent: 80, QuotaUpdatedAt: now},
	}), StrategyStickySoft, nil)
	selector.now = func() time.Time { return now }
	first, err := selector.Select(SelectRequest{ConversationID: "loop"})
	if err != nil {
		t.Fatal(err)
	}
	selector.ReportRateLimit(first.Account.ID)
	second, err := selector.Select(SelectRequest{ConversationID: "loop"})
	if err != nil || second.Account.ID == first.Account.ID {
		t.Fatalf("hard fail should rebind, got %#v err=%v first=%s", second, err, first.Account.ID)
	}
	third, err := selector.Select(SelectRequest{ConversationID: "loop"})
	if err != nil || third.Account.ID != second.Account.ID {
		t.Fatalf("rebind affinity lost: %#v err=%v", third, err)
	}
}

func TestSelectorReservationCommitCancelIdempotent(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	selector := NewSelector(New([]Account{{ID: "a", Enabled: true, MaxRequestsPerMinute: 1}}), StrategySmart, nil)
	selector.now = func() time.Time { return now }

	first, err := selector.Select(SelectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selector.Select(SelectRequest{}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("pending reservation did not enforce RPM: %v", err)
	}
	if !selector.CancelRequest(first.ReservationID) || selector.CancelRequest(first.ReservationID) || selector.CommitRequest(first.ReservationID) {
		t.Fatal("cancel was not idempotent")
	}

	second, err := selector.Select(SelectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !selector.CommitRequest(second.ReservationID) || selector.CommitRequest(second.ReservationID) || selector.CancelRequest(second.ReservationID) {
		t.Fatal("commit was not idempotent")
	}
	if _, err := selector.Select(SelectRequest{}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("committed request did not consume RPM: %v", err)
	}
}

func TestSelectorConcurrentReservationResolution(t *testing.T) {
	selector := NewSelector(New([]Account{{ID: "a", Enabled: true}, {ID: "b", Enabled: true}}), StrategyLeastUsedRPM, nil)
	var wait sync.WaitGroup
	for index := range 200 {
		wait.Add(1)
		go func(commit bool) {
			defer wait.Done()
			selected, err := selector.Select(SelectRequest{})
			if err != nil {
				return
			}
			if commit {
				selector.CommitRequest(selected.ReservationID)
			} else {
				selector.CancelRequest(selected.ReservationID)
			}
		}(index%2 == 0)
	}
	wait.Wait()
	selector.mu.Lock()
	defer selector.mu.Unlock()
	if len(selector.reservations) != 0 || selector.state("a").pendingRequests != 0 || selector.state("b").pendingRequests != 0 {
		t.Fatalf("reservation leak: reservations=%d a=%d b=%d", len(selector.reservations), selector.state("a").pendingRequests, selector.state("b").pendingRequests)
	}
}
