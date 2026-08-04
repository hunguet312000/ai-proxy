package usage

import (
	"testing"
	"time"
)

// A quota endpoint that fails permanently used to produce a warning a minute forever:
// measured on a live instance, three accounts refused with 403 PERMISSION_DENIED accounted
// for 111 of the 128 warnings in the log, burying everything else.
func TestRefreshBackoffGrowsAndIsCapped(t *testing.T) {
	service := &Service{}
	now := time.Now()

	// The first check is always allowed; nothing is known about the account yet.
	if !service.refreshDue("a", now) {
		t.Fatal("a fresh account must be checked")
	}
	failures, next := service.recordRefreshFailure("a", now)
	if failures != 1 || next.Sub(now) != refreshBackoffBase {
		t.Fatalf("first failure: %d failures, delay %v, want 1 and %v", failures, next.Sub(now), refreshBackoffBase)
	}
	if service.refreshDue("a", now.Add(refreshBackoffBase-time.Second)) {
		t.Fatal("checked again before the backoff elapsed")
	}
	if !service.refreshDue("a", now.Add(refreshBackoffBase)) {
		t.Fatal("not checked once the backoff elapsed")
	}

	// Doubling, then flat at the ceiling — which keeps the check running often enough to
	// notice a permission being granted later.
	_, next = service.recordRefreshFailure("a", now)
	if next.Sub(now) != 2*refreshBackoffBase {
		t.Fatalf("second failure delay = %v, want %v", next.Sub(now), 2*refreshBackoffBase)
	}
	for attempt := 0; attempt < 40; attempt++ {
		_, next = service.recordRefreshFailure("a", now)
	}
	if next.Sub(now) != refreshBackoffMax {
		t.Fatalf("delay = %v, want it capped at %v", next.Sub(now), refreshBackoffMax)
	}
}

// One success restores the normal cadence, rather than leaving the account throttled by the
// history of having been broken.
func TestRefreshBackoffClearsOnSuccess(t *testing.T) {
	service := &Service{}
	now := time.Now()
	for attempt := 0; attempt < 5; attempt++ {
		service.recordRefreshFailure("a", now)
	}
	if service.refreshDue("a", now) {
		t.Fatal("still due while backed off")
	}
	service.clearRefreshBackoff("a")
	if !service.refreshDue("a", now) {
		t.Fatal("a cleared account must be checked again immediately")
	}
	// And the count restarts, so the warnings come back for a genuinely new outage.
	failures, _ := service.recordRefreshFailure("a", now)
	if failures != 1 {
		t.Fatalf("failures = %d after clearing, want 1", failures)
	}
}

// Backoff is per account: one broken provider must not silence the checks for the others.
func TestRefreshBackoffIsPerAccount(t *testing.T) {
	service := &Service{}
	now := time.Now()
	service.recordRefreshFailure("broken", now)
	if service.refreshDue("broken", now) {
		t.Fatal("the failing account should be waiting")
	}
	if !service.refreshDue("healthy", now) {
		t.Fatal("an unrelated account was caught by another's backoff")
	}
}
