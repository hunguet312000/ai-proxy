package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCustomProviderCRUDAndKeyEncryption(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	created, err := store.CreateCustomProvider(ctx, CustomProvider{
		Name: "AI Telegram", Prefix: " AI-Tele/ ", Kind: CustomKindOpenAI,
		APIType: CustomAPITypeChat, BaseURL: "https://apikey.click/v1/", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Prefix != "ai-tele" {
		t.Fatalf("prefix was not normalized: %q", created.Prefix)
	}
	if created.BaseURL != "https://apikey.click/v1" {
		t.Fatalf("base URL keeps a trailing slash: %q", created.BaseURL)
	}

	key, err := store.AddCustomProviderKey(ctx, created.ID, "primary", "sk-secret-value")
	if err != nil {
		t.Fatal(err)
	}

	// Listing is what the UI reads, so it must never carry key material.
	listed, err := store.ListCustomProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || len(listed[0].Keys) != 1 {
		t.Fatalf("listed = %#v", listed)
	}
	if listed[0].Keys[0].Secret != "" {
		t.Fatal("ListCustomProviders leaked the api key")
	}

	// The gateway loader is the only path that decrypts.
	loaded, err := store.LoadCustomProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || len(loaded[0].Keys) != 1 || loaded[0].Keys[0].Secret != "sk-secret-value" {
		t.Fatalf("loaded = %#v", loaded)
	}

	// And the value is not sitting in the database in clear.
	var blob []byte
	if err := store.db.QueryRowContext(ctx,
		`SELECT secret FROM custom_provider_keys WHERE id = ?`, key.ID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "sk-secret-value") {
		t.Fatal("the api key was stored unencrypted")
	}

	updated, err := store.UpdateCustomProvider(ctx, CustomProvider{
		ID: created.ID, Name: "Renamed", Prefix: "ai-tele", Kind: CustomKindOpenAI,
		APIType: CustomAPITypeResponses, BaseURL: "https://apikey.click/v1", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.APIType != CustomAPITypeResponses || updated.Name != "Renamed" {
		t.Fatalf("updated = %#v", updated)
	}

	// Deleting the provider must take its credentials with it.
	if err := store.DeleteCustomProvider(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM custom_provider_keys WHERE provider_id = ?`, created.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d orphaned keys survived the provider", remaining)
	}
}

func TestCustomProviderPrefixMustBeUnique(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := CustomProvider{Kind: CustomKindOpenAI, APIType: CustomAPITypeChat,
		BaseURL: "https://a.example.com/v1", Enabled: true}
	base.Prefix = "acme"
	if _, err := store.CreateCustomProvider(ctx, base); err != nil {
		t.Fatal(err)
	}
	// Two providers on one prefix would make routing depend on row order.
	if _, err := store.CreateCustomProvider(ctx, base); !errors.Is(err, ErrCustomPrefixTaken) {
		t.Fatalf("duplicate prefix error = %v", err)
	}
}

func TestCustomProviderValidation(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for name, provider := range map[string]CustomProvider{
		"no prefix":        {Kind: CustomKindOpenAI, BaseURL: "https://a.example.com/v1"},
		"bad prefix char":  {Prefix: "a b", Kind: CustomKindOpenAI, BaseURL: "https://a.example.com/v1"},
		"unknown kind":     {Prefix: "p", Kind: "sideways", BaseURL: "https://a.example.com/v1"},
		"bad api type":     {Prefix: "p", Kind: CustomKindOpenAI, APIType: "grpc", BaseURL: "https://a.example.com/v1"},
		"relative url":     {Prefix: "p", Kind: CustomKindOpenAI, BaseURL: "/v1"},
		"plaintext remote": {Prefix: "p", Kind: CustomKindOpenAI, BaseURL: "http://api.example.com/v1"},
	} {
		if _, err := store.CreateCustomProvider(ctx, provider); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	// Loopback over plain HTTP is allowed, matching the built-in provider clients.
	if _, err := store.CreateCustomProvider(ctx, CustomProvider{
		Prefix: "local", Kind: CustomKindOpenAI, BaseURL: "http://127.0.0.1:1234/v1", Enabled: true,
	}); err != nil {
		t.Fatalf("loopback base URL rejected: %v", err)
	}
	// An Anthropic-compatible provider is pinned to Messages regardless of input.
	anthropic, err := store.CreateCustomProvider(ctx, CustomProvider{
		Prefix: "anth", Kind: CustomKindAnthropic, APIType: CustomAPITypeChat,
		BaseURL: "https://anth.example.com/v1", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if anthropic.APIType != CustomAPITypeMessages {
		t.Fatalf("api type = %q, want messages", anthropic.APIType)
	}
}

func TestLoadCustomProvidersSkipsUnusable(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Disabled provider: present in the UI list, never offered for routing.
	disabled, err := store.CreateCustomProvider(ctx, CustomProvider{
		Prefix: "off", Kind: CustomKindOpenAI, BaseURL: "https://off.example.com/v1", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddCustomProviderKey(ctx, disabled.ID, "", "sk-off"); err != nil {
		t.Fatal(err)
	}
	// Enabled provider with no key: would accept traffic and fail every request.
	if _, err := store.CreateCustomProvider(ctx, CustomProvider{
		Prefix: "keyless", Kind: CustomKindOpenAI, BaseURL: "https://k.example.com/v1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// Enabled provider whose only key is disabled.
	partly, err := store.CreateCustomProvider(ctx, CustomProvider{
		Prefix: "paused", Kind: CustomKindOpenAI, BaseURL: "https://p.example.com/v1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	pausedKey, err := store.AddCustomProviderKey(ctx, partly.ID, "", "sk-paused")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCustomProviderKeyEnabled(ctx, pausedKey.ID, false); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadCustomProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("unusable providers were offered for routing: %#v", loaded)
	}
	if listed, err := store.ListCustomProviders(ctx); err != nil || len(listed) != 3 {
		t.Fatalf("listed = %d, err = %v; the UI must still see them", len(listed), err)
	}
}

func TestAddCustomProviderKeyRejectsUnknownProvider(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.AddCustomProviderKey(context.Background(), "cp_missing", "", "sk"); err == nil {
		t.Fatal("a key was attached to a provider that does not exist")
	}
}

func TestCustomProviderRejectsReservedPrefixes(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	// Custom prefixes are resolved before the built-in switch, so claiming one of
	// these would silently stop the corresponding pool from ever being reached.
	for _, prefix := range []string{"codex", "cx", "claude", "anthropic", "xai", "grok", "antigravity", "ag", "gemini", "CODEX"} {
		if _, err := store.CreateCustomProvider(ctx, CustomProvider{
			Prefix: prefix, Kind: CustomKindOpenAI, BaseURL: "https://x.example.com/v1", Enabled: true,
		}); err == nil {
			t.Fatalf("reserved prefix %q was accepted", prefix)
		}
	}
	if _, err := store.CreateCustomProvider(ctx, CustomProvider{
		Prefix: "fpt-ai", Kind: CustomKindOpenAI, BaseURL: "https://mkp-api.fptcloud.com/v1", Enabled: true,
	}); err != nil {
		t.Fatalf("a non-reserved prefix was rejected: %v", err)
	}
}
