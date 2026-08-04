package usage

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"literouter/internal/pool"
	"literouter/internal/secret"
	"literouter/internal/storage"
)

func openUsageTestStore(t *testing.T) *storage.Store {
	t.Helper()
	box, err := secret.New(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(t.TempDir(), "usage.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestUsageWriterFlushesByBatchSize(t *testing.T) {
	store := openUsageTestStore(t)
	writer := newUsageWriter(store, func() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }, 8, 2, time.Hour)
	if !writer.enqueue(storage.UsageEvent{Provider: "codex", Model: "a"}) || !writer.enqueue(storage.UsageEvent{Provider: "xai", Model: "b"}) {
		t.Fatal("enqueue failed")
	}
	deadline := time.Now().Add(time.Second)
	for {
		summary, err := store.UsageSummary(context.Background(), time.Time{}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if summary.Requests == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("batch was not flushed: %+v", summary)
		}
		time.Sleep(time.Millisecond)
	}
	if err := writer.close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUsageWriterFlushesOnClose(t *testing.T) {
	store := openUsageTestStore(t)
	service := NewService(store, pool.New(nil), nil)
	if !service.EnqueueGatewayUsage(storage.UsageEvent{Provider: "codex", Model: "cx/model", PromptTokens: 10, CompletionTokens: 2}) {
		t.Fatal("enqueue failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatalf("second Close() failed: %v", err)
	}
	summary, err := store.UsageSummary(context.Background(), time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 1 || summary.PromptTokens != 10 || summary.CompletionTokens != 2 {
		t.Fatalf("flushed summary = %+v", summary)
	}
	if service.EnqueueGatewayUsage(storage.UsageEvent{}) {
		t.Fatal("enqueue succeeded after close")
	}
}

func TestUsageWriterDropsWithoutBlockingWhenQueueFull(t *testing.T) {
	store := openUsageTestStore(t)
	writer := newUsageWriter(store, func() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }, 1, 1, time.Hour)
	writer.lifecycle.Lock()
	writer.started = true // Keep the worker paused so saturation is deterministic.
	writer.lifecycle.Unlock()
	if !writer.enqueue(storage.UsageEvent{Model: "first"}) {
		t.Fatal("first enqueue failed")
	}
	started := time.Now()
	if writer.enqueue(storage.UsageEvent{Model: "dropped"}) {
		t.Fatal("second enqueue should be dropped")
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("full queue blocked for %s", elapsed)
	}
	if writer.dropped.Load() != 1 {
		t.Fatalf("dropped = %d, want 1", writer.dropped.Load())
	}
}

func TestUsageWriterFlushesOnTimer(t *testing.T) {
	store := openUsageTestStore(t)
	writer := newUsageWriter(store, func() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }, 8, 8, 5*time.Millisecond)
	if !writer.enqueue(storage.UsageEvent{Model: "timer"}) {
		t.Fatal("enqueue failed")
	}
	deadline := time.Now().Add(time.Second)
	for {
		summary, err := store.UsageSummary(context.Background(), time.Time{}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if summary.Requests == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timer did not flush usage")
		}
		time.Sleep(time.Millisecond)
	}
	if err := writer.close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
