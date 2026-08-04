package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestPoolCRUD(t *testing.T) {
	p := New([]Account{{ID: "b"}, {ID: "a"}})
	if got := p.List(); len(got) != 2 || got[0].ID != "a" {
		t.Fatalf("List() = %#v", got)
	}

	p.Upsert(Account{ID: "a", Label: "updated"})
	account, ok := p.Get("a")
	if !ok || account.Label != "updated" {
		t.Fatalf("Get() = %#v, %v", account, ok)
	}

	p.Remove("a")
	if _, ok := p.Get("a"); ok || p.Len() != 1 {
		t.Fatal("Remove() did not remove account")
	}
}

func TestPoolDefaultsAndPreservesQuota(t *testing.T) {
	p := New([]Account{{ID: "a", Enabled: true}})
	account, _ := p.Get("a")
	if account.QuotaRemainingPercent != 100 {
		t.Fatalf("initial quota = %#v", account)
	}
	updatedAt := time.Now().UTC()
	p.UpdateQuota("a", 25, true, updatedAt.Add(time.Hour), updatedAt)
	p.Upsert(Account{ID: "a", Enabled: false, Weight: 1})
	account, _ = p.Get("a")
	if account.Enabled || account.QuotaRemainingPercent != 25 || !account.QuotaExhausted || !account.QuotaUpdatedAt.Equal(updatedAt) {
		t.Fatalf("updated account = %#v", account)
	}
}

func TestPoolRestoreQuotaIgnoresReviewAndUsesPrepaid(t *testing.T) {
	p := New([]Account{{ID: "a", Enabled: true}})
	now := time.Now().UTC()
	if !p.RestoreQuota("a", []QuotaSnapshot{
		{Key: "monthly", RemainingPercent: 0, Exhausted: true, ResetAt: now.Add(time.Hour), FetchedAt: now},
		{Key: "review_session", RemainingPercent: 0, Exhausted: true, FetchedAt: now},
		{Key: "prepaid", Remaining: 10, RemainingPercent: 100, FetchedAt: now},
	}) {
		t.Fatal("RestoreQuota() = false")
	}
	account, _ := p.Get("a")
	if account.QuotaExhausted || account.QuotaRemainingPercent != 0 || account.QuotaResetAt.IsZero() {
		t.Fatalf("restored account = %#v", account)
	}
}

func TestPoolRestoreQuotaKeepsHealthyReset(t *testing.T) {
	p := New([]Account{{ID: "a", Enabled: true}})
	now := time.Now().UTC()
	reset := now.Add(3*24*time.Hour + 17*time.Hour)
	if !p.RestoreQuota("a", []QuotaSnapshot{
		{Key: "session", RemainingPercent: 51, Exhausted: false, ResetAt: reset, FetchedAt: now},
		{Key: "weekly", RemainingPercent: 80, Exhausted: false, ResetAt: reset.Add(24 * time.Hour), FetchedAt: now},
	}) {
		t.Fatal("RestoreQuota() = false")
	}
	account, _ := p.Get("a")
	if account.QuotaRemainingPercent != 51 || account.QuotaExhausted || !account.QuotaResetAt.Equal(reset) {
		t.Fatalf("restored account = %#v", account)
	}
}

func TestPoolClonesMutableAccountFields(t *testing.T) {
	input := Account{
		ID: "a", Models: []string{"model-a"},
		ModelQuotas: map[string]QuotaSnapshot{"model-a": {Key: "model-a", RemainingPercent: 75}},
	}
	p := New([]Account{input})
	input.Models[0] = "mutated"
	input.ModelQuotas["model-a"] = QuotaSnapshot{Key: "model-a", RemainingPercent: 0}

	account, _ := p.Get("a")
	account.Models[0] = "returned-mutated"
	account.ModelQuotas["model-a"] = QuotaSnapshot{Key: "model-a", RemainingPercent: 1}
	listed := p.List()
	listed[0].Models[0] = "listed-mutated"
	listed[0].ModelQuotas["model-a"] = QuotaSnapshot{Key: "model-a", RemainingPercent: 2}

	account, _ = p.Get("a")
	if account.Models[0] != "model-a" || account.ModelQuotas["model-a"].RemainingPercent != 75 {
		t.Fatalf("mutable account fields leaked through pool boundary: %#v", account)
	}
}

func TestPoolAtomicMutationsPreserveIndependentState(t *testing.T) {
	p := New([]Account{{ID: "a", Enabled: true, Label: "old", Plan: "free"}})
	now := time.Now().UTC()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for range 100 {
			p.SetEnabled("a", false)
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			p.UpdateMetadata("a", "new", "pro")
			p.UpdateResetCredits("a", 7, true)
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			p.RestoreQuota("a", []QuotaSnapshot{{Key: "model-a", RemainingPercent: 25, FetchedAt: now}})
		}
	}()
	wg.Wait()

	account, _ := p.Get("a")
	if account.Enabled || account.Label != "new" || account.Plan != "pro" || account.ResetCredits != 7 || !account.ResetCreditsKnown {
		t.Fatalf("independent state was overwritten: %#v", account)
	}
	if account.QuotaRemainingPercent != 25 || len(account.ModelQuotas) != 1 {
		t.Fatalf("quota state is inconsistent: %#v", account)
	}
}

func TestPoolConcurrentAccess(t *testing.T) {
	p := New(nil)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprint(id)
			p.Upsert(Account{ID: key})
			p.Get(key)
			p.List()
		}(i)
	}
	wg.Wait()
	if p.Len() != 100 {
		t.Fatalf("Len() = %d, want 100", p.Len())
	}
}
