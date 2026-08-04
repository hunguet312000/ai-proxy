package storage

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"literouter/internal/secret"
)

func TestAPIKeyLifecycle(t *testing.T) {
	key := make([]byte, secret.KeySize)
	box, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(t.TempDir(), "keys.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	created, err := store.CreateAPIKey(context.Background(), "ci")
	if err != nil || created.Token == "" || created.Prefix == "" {
		t.Fatalf("CreateAPIKey() = %#v, %v", created, err)
	}
	if !store.ValidAPIKey(context.Background(), created.Token) {
		t.Fatal("created token should validate")
	}
	keys, err := store.ListAPIKeys(context.Background())
	if err != nil || len(keys) != 1 || keys[0].Token != "" || keys[0].Name != "ci" {
		t.Fatalf("ListAPIKeys() = %#v, %v", keys, err)
	}
	if err := store.SetAPIKeyEnabled(context.Background(), created.ID, false); err != nil {
		t.Fatal(err)
	}
	if store.ValidAPIKey(context.Background(), created.Token) {
		t.Fatal("paused token should not validate")
	}
	if err := store.SetAPIKeyEnabled(context.Background(), created.ID, true); err != nil {
		t.Fatal(err)
	}
	if !store.ValidAPIKey(context.Background(), created.Token) {
		t.Fatal("resumed token should validate")
	}
	if err := store.DeleteAPIKey(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	if store.ValidAPIKey(context.Background(), created.Token) {
		t.Fatal("deleted token should not validate")
	}
}

func TestAPIKeyCacheWarmLoadsAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	box, err := secret.New(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	first, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	created, err := first.CreateAPIKey(context.Background(), "restart")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if err := second.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !second.ValidAPIKey(context.Background(), created.Token) {
		t.Fatal("warm-loaded token should validate")
	}
}

func TestAPIKeyUsageIsCoalescedUntilFlush(t *testing.T) {
	store := openTestStore(t)
	created, err := store.CreateAPIKey(context.Background(), "coalesced")
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if !store.ValidAPIKey(context.Background(), created.Token) {
			t.Fatal("token should validate")
		}
	}
	keys, err := store.ListAPIKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !keys[0].LastUsedAt.IsZero() {
		t.Fatalf("last_used_at was persisted before flush: %s", keys[0].LastUsedAt)
	}
	if err := store.FlushAPIKeyUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	keys, err = store.ListAPIKeys(context.Background())
	if err != nil || keys[0].LastUsedAt.IsZero() {
		t.Fatalf("last_used_at was not flushed: %#v, %v", keys, err)
	}
}

func TestAPIKeyCacheConcurrentValidateAndRevoke(t *testing.T) {
	store := openTestStore(t)
	created, err := store.CreateAPIKey(context.Background(), "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				store.ValidAPIKey(context.Background(), created.Token)
			}
		}()
	}
	if err := store.SetAPIKeyEnabled(context.Background(), created.ID, false); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if store.ValidAPIKey(context.Background(), created.Token) {
		t.Fatal("revoked token remained cached")
	}
}

func BenchmarkValidAPIKeyCachedParallel(b *testing.B) {
	store := openTestStore(b)
	created, err := store.CreateAPIKey(context.Background(), "benchmark")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if !store.ValidAPIKey(context.Background(), created.Token) {
				b.Fatal("token should validate")
			}
		}
	})
}
