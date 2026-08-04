package storage

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestOAuthSessionOneTimeEncrypted(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	want := OAuthSession{
		State: "signed-state", Provider: "codex", Verifier: "secret-verifier", ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := store.PutOAuthSession(ctx, want); err != nil {
		t.Fatalf("PutOAuthSession() error = %v", err)
	}
	var persisted []byte
	if err := store.db.QueryRowContext(ctx, "SELECT verifier FROM oauth_sessions WHERE state = ?", want.State).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if string(persisted) == want.Verifier {
		t.Fatal("OAuth verifier stored in plaintext")
	}
	got, err := store.TakeOAuthSession(ctx, want.State, want.Provider)
	if err != nil || got.Verifier != want.Verifier {
		t.Fatalf("TakeOAuthSession() = %#v, %v", got, err)
	}
	if _, err := store.TakeOAuthSession(ctx, want.State, want.Provider); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second TakeOAuthSession() error = %v", err)
	}
}

func TestTakeLatestOAuthSession(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	old := OAuthSession{State: "old-state", Provider: "grok", Verifier: "old-verifier", ExpiresAt: time.Now().Add(time.Minute)}
	newest := OAuthSession{State: "new-state", Provider: "grok", Verifier: "new-verifier", ExpiresAt: time.Now().Add(2 * time.Minute)}
	if err := store.PutOAuthSession(ctx, old); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := store.PutOAuthSession(ctx, newest); err != nil {
		t.Fatal(err)
	}
	got, err := store.TakeLatestOAuthSession(ctx, "grok")
	if err != nil || got.State != newest.State || got.Verifier != newest.Verifier {
		t.Fatalf("TakeLatestOAuthSession() = %#v, %v", got, err)
	}
	if _, err := store.TakeOAuthSession(ctx, newest.State, "grok"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("newest should be consumed: %v", err)
	}
	// old session still present until taken
	if got, err := store.TakeOAuthSession(ctx, old.State, "grok"); err != nil || got.Verifier != old.Verifier {
		t.Fatalf("old session = %#v, %v", got, err)
	}
}
