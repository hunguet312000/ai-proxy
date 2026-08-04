package storage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"literouter/internal/secret"
)

func TestAccountCRUD(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	want := Account{
		ID:          "account-1",
		Provider:    "openai",
		Label:       "Primary",
		Credentials: []byte("encrypted-blob"),
		Enabled:     true,
		Weight:      2,
	}
	if err := store.CreateAccount(ctx, want); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	got, err := store.GetAccount(ctx, want.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.Provider != want.Provider || got.Label != want.Label || !bytes.Equal(got.Credentials, want.Credentials) {
		t.Fatalf("GetAccount() = %#v, want fields from %#v", got, want)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("timestamps were not set")
	}

	got.Label = "Updated"
	if err := store.UpdateAccount(ctx, got); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}
	if err := store.UpdateAccountRouting(ctx, got.ID, false, 3); err != nil {
		t.Fatalf("UpdateAccountRouting() error = %v", err)
	}

	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) != 1 || accounts[0].Label != "Updated" || accounts[0].Enabled || accounts[0].Weight != 3 {
		t.Fatalf("ListAccounts() = %#v", accounts)
	}

	if err := store.DeleteAccount(ctx, want.ID); err != nil {
		t.Fatalf("DeleteAccount() error = %v", err)
	}
	if _, err := store.GetAccount(ctx, want.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAccount() error = %v, want sql.ErrNoRows", err)
	}
}

func TestPartialAccountUpdatesPreserveRoutingState(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account := Account{
		ID: "account-partial", Provider: "codex", Label: "Codex", Plan: "free",
		Credentials: []byte("old-token"), Enabled: false, Weight: 7,
	}
	if err := store.CreateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateAccountPlan(ctx, account.ID, "plus"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateAccountCredentials(ctx, account.ID, []byte("new-token")); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.Weight != account.Weight || got.Plan != "plus" || !bytes.Equal(got.Credentials, []byte("new-token")) {
		t.Fatalf("account = %#v", got)
	}
}

func TestUpdateAccountRoutingPreservesAccountData(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account := Account{
		ID: "account-routing", Provider: "codex", Label: "Primary", Plan: "plus",
		Credentials: []byte("token"), Enabled: true, Weight: 2,
	}
	if err := store.CreateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateAccountRouting(ctx, account.ID, false, 9); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.Weight != 9 || got.Provider != account.Provider || got.Label != account.Label || got.Plan != account.Plan || !bytes.Equal(got.Credentials, account.Credentials) {
		t.Fatalf("account = %#v", got)
	}
}

func TestStaleAccountWritesPreserveRoutingState(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account := Account{
		ID: "account-stale", Provider: "grok", Label: "User", Plan: "supergrok",
		Credentials: []byte("old-token"), Enabled: true, Weight: 4,
	}
	if err := store.CreateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	stale, err := store.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateAccountRouting(ctx, account.ID, false, 9); err != nil {
		t.Fatal(err)
	}

	stale.Credentials = []byte("refreshed-token")
	stale.Enabled = true
	stale.Weight = 1
	if err := store.UpdateAccount(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(ctx, stale); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.Weight != 9 {
		t.Fatalf("stale writes changed routing state: %#v", got)
	}
	if !bytes.Equal(got.Credentials, stale.Credentials) {
		t.Fatalf("credentials = %q", got.Credentials)
	}
}

func TestCredentialsEncryptedAtRest(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	plaintext := []byte("plain-session-token")
	if err := store.CreateAccount(ctx, Account{
		ID: "encrypted", Provider: "openai", Credentials: plaintext, Enabled: true, Weight: 1,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	var persisted []byte
	if err := store.db.QueryRowContext(ctx, "SELECT credentials FROM accounts WHERE id = ?", "encrypted").Scan(&persisted); err != nil {
		t.Fatalf("read persisted credential: %v", err)
	}
	if bytes.Contains(persisted, plaintext) {
		t.Fatal("credentials were stored in plaintext")
	}
}

func TestAccountValidationAndMissingRows(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.CreateAccount(ctx, Account{}); err == nil {
		t.Fatal("CreateAccount() accepted invalid account")
	}
	if err := store.UpdateAccount(ctx, Account{ID: "missing", Provider: "x", Credentials: []byte{1}, Weight: 1}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateAccount() error = %v, want sql.ErrNoRows", err)
	}
	if err := store.DeleteAccount(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("DeleteAccount() error = %v, want sql.ErrNoRows", err)
	}
}

func openTestStore(t testing.TB) *Store {
	t.Helper()
	box, err := secret.New(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatalf("secret.New() error = %v", err)
	}
	store, err := Open(filepath.Join(t.TempDir(), "literouter.db"), box)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return store
}

// The reason has to survive a restart — the dashboard is read after the fact, not during the
// failure — and enabling must clear it, since whatever it said has been resolved.
func TestDisabledReasonPersistsAndClears(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account := Account{ID: "codex:x", Provider: "codex", Label: "x@example.com",
		Credentials: []byte(`{"access_token":"a"}`), Enabled: true, Weight: 1}
	if err := store.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}

	const reason = "Your session has ended. Please log in again."
	if err := store.DisableAccountWithReason(ctx, account.ID, reason); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Enabled || stored.DisabledReason != reason {
		t.Fatalf("enabled=%v reason=%q, want disabled with the reason", stored.Enabled, stored.DisabledReason)
	}

	if err := store.UpdateAccountRouting(ctx, account.ID, true, 1); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Enabled || stored.DisabledReason != "" {
		t.Fatalf("enabled=%v reason=%q, want enabled with no reason", stored.Enabled, stored.DisabledReason)
	}

	// Providers return whole JSON documents in error bodies; the card needs a sentence.
	long := strings.Repeat("y", 500)
	if err := store.DisableAccountWithReason(ctx, account.ID, long); err != nil {
		t.Fatal(err)
	}
	stored, _ = store.GetAccount(ctx, account.ID)
	if len(stored.DisabledReason) != 300 {
		t.Fatalf("reason length = %d, want it capped at 300", len(stored.DisabledReason))
	}
}
