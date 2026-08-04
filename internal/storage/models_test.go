package storage

import (
	"context"
	"path/filepath"
	"testing"

	"literouter/internal/secret"
)

func TestCatalogModelsCRUD(t *testing.T) {
	key := make([]byte, secret.KeySize)
	box, _ := secret.New(key)
	store, err := Open(filepath.Join(t.TempDir(), "models.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureCatalogModels(context.Background(), "codex", []string{"gpt-4.1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureCatalogModels(context.Background(), "xai", []string{"grok-4"}); err != nil {
		t.Fatal(err)
	}
	created, err := store.AddCatalogModel(context.Background(), "claude", "claude-sonnet-5", "Sonnet 5")
	if err != nil || created.ID != "claude-sonnet-5" || created.Provider != "claude" {
		t.Fatalf("AddCatalogModel() = %#v, %v", created, err)
	}
	claude, err := store.ListCatalogModels(context.Background(), "claude")
	if err != nil || len(claude) != 1 || claude[0].ID != "claude-sonnet-5" {
		t.Fatalf("ListCatalogModels(claude) = %#v, %v", claude, err)
	}
	codex, err := store.ListCatalogModels(context.Background(), "codex")
	if err != nil || len(codex) != 1 || codex[0].ID != "gpt-4.1" {
		t.Fatalf("ListCatalogModels(codex) = %#v, %v", codex, err)
	}
	all, err := store.ListCatalogModels(context.Background(), "")
	if err != nil || len(all) != 3 {
		t.Fatalf("ListCatalogModels() = %#v, %v", all, err)
	}
	if err := store.DeleteCatalogModel(context.Background(), "xai", "grok-4"); err != nil {
		t.Fatal(err)
	}
	all, _ = store.ListCatalogModels(context.Background(), "")
	if len(all) != 2 {
		t.Fatalf("after delete = %#v", all)
	}
}

func TestInferCatalogProvider(t *testing.T) {
	tests := map[string]string{
		"claude-sonnet-5": "claude",
		"grok-4":          "xai",
		"gpt-4.1":         "codex",
		"cx/gpt-5.6-sol":  "codex",
	}
	for id, want := range tests {
		if got := inferCatalogProvider(id); got != want {
			t.Fatalf("inferCatalogProvider(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestPrettyModelLabel(t *testing.T) {
	tests := map[string]string{
		"cx/gpt-5.6-sol":            "GPT 5.6 Sol",
		"cx/gpt-5.3-codex-spark":    "GPT 5.3 Codex Spark",
		"xai/grok-4-fast-reasoning": "Grok 4 Fast Reasoning",
		"claude-sonnet-5":           "Claude Sonnet 5",
	}
	for id, want := range tests {
		if got := PrettyModelLabel(id); got != want {
			t.Fatalf("PrettyModelLabel(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestCatalogContextWindows(t *testing.T) {
	key := make([]byte, secret.KeySize)
	box, _ := secret.New(key)
	store, err := Open(filepath.Join(t.TempDir(), "contexts.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	created, err := store.AddCatalogModel(ctx, "codex", "cx/gpt-5.6-sol", "", 400_000)
	if err != nil || created.ContextWindow != 400_000 {
		t.Fatalf("created = %+v, %v", created, err)
	}
	// Startup reseed may update labels/defaults but must preserve custom window.
	if err := store.EnsureCatalogModels(ctx, "codex", []string{"cx/gpt-5.6-sol"}); err != nil {
		t.Fatal(err)
	}
	window, err := store.CatalogContextWindow(ctx, "cx/gpt-5.6-sol-review")
	if err != nil || window != 400_000 {
		t.Fatalf("review window = %d, %v", window, err)
	}
	if err := store.SetCatalogContextWindow(ctx, "codex", "cx/gpt-5.6-sol", 512_000); err != nil {
		t.Fatal(err)
	}
	window, _ = store.CatalogContextWindow(ctx, "gpt-5.6-sol")
	if window != 512_000 {
		t.Fatalf("updated window = %d", window)
	}
}

func TestKnownContextWindows(t *testing.T) {
	tests := map[string]int{
		"cx/gpt-5.3-codex-spark":    400_000,
		"cx/gpt-5.4-mini-review":    400_000,
		"cx/gpt-5.5-review":         272_000,
		"cx/gpt-5.6-sol-review":     400_000,
		"claude-opus-4-8":           1_000_000,
		"claude-sonnet-5":           1_000_000,
		"claude-haiku-4-5-20251001": 200_000,
		"xai/grok-3":                131_072,
		"xai/grok-4.5":              500_000,
		"xai/grok-code-fast-1":      256_000,
	}
	for model, want := range tests {
		if got := DefaultContextWindow(model); got != want {
			t.Fatalf("DefaultContextWindow(%q) = %d, want %d", model, got, want)
		}
	}
}

func TestCatalogMaxOutputTokens(t *testing.T) {
	key := make([]byte, secret.KeySize)
	box, _ := secret.New(key)
	store, err := Open(filepath.Join(t.TempDir(), "outputs.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureCatalogModels(ctx, "codex", []string{"gpt-4.1"}); err != nil {
		t.Fatal(err)
	}

	// Nothing is guessed: an unobserved model is absent rather than defaulted.
	limits, err := store.CatalogMaxOutputTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(limits) != 0 {
		t.Fatalf("CatalogMaxOutputTokens() = %v, want empty", limits)
	}

	if err := store.RecordCatalogMaxOutputTokens(ctx, "gpt-4.1", 16384); err != nil {
		t.Fatal(err)
	}
	// A model that was never seeded still gets recorded: custom-provider models reach
	// the gateway without ever being enumerated.
	if err := store.RecordCatalogMaxOutputTokens(ctx, "cc/unlisted", 4096); err != nil {
		t.Fatal(err)
	}
	// Only lower caps win. A higher observation means something else answered under
	// this name, and taking it walks back into rejections.
	if err := store.RecordCatalogMaxOutputTokens(ctx, "gpt-4.1", 32000); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCatalogMaxOutputTokens(ctx, "cc/unlisted", 2048); err != nil {
		t.Fatal(err)
	}

	limits, err = store.CatalogMaxOutputTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if limits["gpt-4.1"] != 16384 || limits["cc/unlisted"] != 2048 || len(limits) != 2 {
		t.Fatalf("CatalogMaxOutputTokens() = %v, want gpt-4.1:16384 cc/unlisted:2048", limits)
	}

	// Recording must not duplicate the row it already updated.
	models, err := store.ListCatalogModels(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, model := range models {
		if model.ID == "gpt-4.1" {
			seen++
			if model.MaxOutputTokens != 16384 {
				t.Fatalf("gpt-4.1 MaxOutputTokens = %d, want 16384", model.MaxOutputTokens)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("gpt-4.1 appears %d times, want 1", seen)
	}

	if err := store.RecordCatalogMaxOutputTokens(ctx, "", 1024); err == nil {
		t.Fatal("an empty model id was accepted")
	}
	if err := store.RecordCatalogMaxOutputTokens(ctx, "gpt-4.1", 0); err == nil {
		t.Fatal("a non-positive limit was accepted")
	}
}
