package oauth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"literouter/internal/pool"
	"literouter/internal/provider"
	"literouter/internal/secret"
	"literouter/internal/storage"
)

func testCredentialManager(t *testing.T) (*CredentialManager, *storage.Store, *pool.Pool) {
	t.Helper()
	box, err := secret.New(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(t.TempDir(), "accounts.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(context.Background(), storage.Account{
		ID: "codex:dead", Provider: "codex", Label: "dead@example.com",
		Credentials: []byte(`{"access_token":"stale"}`), Enabled: true, Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	accountPool := pool.New([]pool.Account{
		{ID: "codex:dead", Provider: "codex", Enabled: true, Weight: 1},
		{ID: "codex:live", Provider: "codex", Enabled: true, Weight: 1},
	})
	manager := NewCredentialManager(store, accountPool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return manager, store, accountPool
}

// An account whose credentials the upstream refuses has to become visible. Before this,
// nothing recorded the refusal: the card stayed indistinguishable from a healthy one, the
// selector kept choosing the account, and every turn paid a wasted round trip on it.
func TestRejectedAccountIsRetiredWithTheUpstreamReason(t *testing.T) {
	manager, store, accountPool := testCredentialManager(t)
	const reason = "Your authentication token has been invalidated. Please try signing in again."

	manager.RetireRejectedAccount(context.Background(), "codex:dead", "codex", reason)

	// The dashboard reads the pool, so the reason has to be there for the card to explain
	// itself, and Enabled false is what makes the selector skip it.
	live, ok := accountPool.Get("codex:dead")
	if !ok || live.Enabled || live.DisabledReason != reason {
		t.Fatalf("pool account = %+v, ok = %v", live, ok)
	}
	// And in storage, or a restart brings the dead account back as healthy.
	stored, err := store.GetAccount(context.Background(), "codex:dead")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Enabled || stored.DisabledReason != reason {
		t.Fatalf("stored account enabled = %v, reason = %q", stored.Enabled, stored.DisabledReason)
	}
	// Untouched accounts stay untouched: one refusal must not empty the pool.
	if other, ok := accountPool.Get("codex:live"); !ok || !other.Enabled {
		t.Fatalf("the healthy account was affected: %+v", other)
	}
}

func TestRejectedAccountFallsBackToAReasonWhenTheUpstreamGaveNone(t *testing.T) {
	manager, _, accountPool := testCredentialManager(t)
	manager.RetireRejectedAccount(context.Background(), "codex:dead", "codex", "   ")
	account, _ := accountPool.Get("codex:dead")
	if account.Enabled || account.DisabledReason == "" {
		t.Fatalf("account = %+v, want disabled with some reason", account)
	}
}

// Only a refusal of the credential retires an account. A rate limit and a server fault are
// about this request, not the token, and retiring on those would drain the pool during an
// upstream incident.
func TestOnlyUnauthorizedRetiresAnAccount(t *testing.T) {
	cases := []struct {
		status int
		retire bool
	}{
		{status: http.StatusUnauthorized, retire: true},
		{status: http.StatusTooManyRequests, retire: false},
		{status: http.StatusInternalServerError, retire: false},
		{status: http.StatusBadGateway, retire: false},
		{status: http.StatusForbidden, retire: false},
	}
	for _, testCase := range cases {
		err := error(&provider.ProviderError{StatusCode: testCase.status, Message: "upstream said so"})
		if got := retiresAccount(err); got != testCase.retire {
			t.Fatalf("status %d: retires = %v, want %v", testCase.status, got, testCase.retire)
		}
	}
	if retiresAccount(nil) {
		t.Fatal("a nil error retired an account")
	}
	if retiresAccount(io.EOF) {
		t.Fatal("an untyped error retired an account")
	}
}
