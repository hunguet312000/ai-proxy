package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Custom provider kinds and wire protocols. These mirror what the upstream actually
// speaks, because that is what decides which translator the gateway uses.
const (
	CustomKindOpenAI    = "openai"
	CustomKindAnthropic = "anthropic"

	CustomAPITypeChat      = "chat"
	CustomAPITypeResponses = "responses"
	CustomAPITypeMessages  = "messages"
)

// ErrCustomPrefixTaken is returned when two providers would claim the same routing
// prefix. Routing resolves a model to exactly one provider, so the prefix has to be
// unique or requests would silently go to whichever row was read first.
var ErrCustomPrefixTaken = errors.New("routing prefix is already used by another provider")

// reservedCustomPrefixes are the names the built-in routing already answers to. A
// custom provider claiming one of these would shadow it: the gateway checks custom
// prefixes first, so "codex/gpt-5.6-sol" would stop reaching the OAuth pool.
var reservedCustomPrefixes = map[string]struct{}{
	"codex": {}, "cx": {}, "openai": {}, "gpt": {},
	"claude": {}, "anthropic": {},
	"xai": {}, "grok": {},
	"antigravity": {}, "ag": {}, "gemini": {},
}

type CustomProvider struct {
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	Prefix    string              `json:"prefix"`
	Kind      string              `json:"kind"`
	APIType   string              `json:"api_type"`
	BaseURL   string              `json:"base_url"`
	Enabled   bool                `json:"enabled"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
	Keys      []CustomProviderKey `json:"keys,omitempty"`
}

type CustomProviderKey struct {
	ID         string    `json:"id"`
	ProviderID string    `json:"provider_id"`
	Label      string    `json:"label"`
	Enabled    bool      `json:"enabled"`
	Weight     int       `json:"weight"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	// Secret is populated only by the loader the gateway uses. Listing for the UI
	// leaves it empty so a key cannot leak through an API response.
	Secret string `json:"-"`
}

// NormalizeCustomPrefix reduces a user-typed prefix to the form routing compares
// against: lower case, no surrounding slashes or spaces.
func NormalizeCustomPrefix(value string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(value)), "/")
}

func validateCustomProvider(provider *CustomProvider) error {
	provider.Name = strings.TrimSpace(provider.Name)
	provider.Prefix = NormalizeCustomPrefix(provider.Prefix)
	provider.Kind = strings.ToLower(strings.TrimSpace(provider.Kind))
	provider.APIType = strings.ToLower(strings.TrimSpace(provider.APIType))
	provider.BaseURL = strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")

	if provider.Prefix == "" {
		return errors.New("prefix is required")
	}
	if len(provider.Prefix) > 32 {
		return errors.New("prefix must be 32 characters or fewer")
	}
	for _, r := range provider.Prefix {
		valid := r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !valid {
			return fmt.Errorf("prefix may only contain letters, digits, '-', '_' and '.'")
		}
	}
	if _, reserved := reservedCustomPrefixes[provider.Prefix]; reserved {
		return fmt.Errorf("prefix %q is reserved for a built-in provider; choose another", provider.Prefix)
	}
	if len(provider.Name) > 64 {
		return errors.New("name must be 64 characters or fewer")
	}
	switch provider.Kind {
	case CustomKindOpenAI:
		switch provider.APIType {
		case CustomAPITypeChat, CustomAPITypeResponses:
		case "":
			provider.APIType = CustomAPITypeChat
		default:
			return fmt.Errorf("api_type must be %q or %q for an OpenAI-compatible provider",
				CustomAPITypeChat, CustomAPITypeResponses)
		}
	case CustomKindAnthropic:
		// Anthropic-compatible upstreams only speak Messages, so the field is fixed
		// rather than offered as a choice that could be set wrong.
		provider.APIType = CustomAPITypeMessages
	default:
		return fmt.Errorf("kind must be %q or %q", CustomKindOpenAI, CustomKindAnthropic)
	}
	parsed, err := url.Parse(provider.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("base URL must be absolute, e.g. https://api.example.com/v1")
	}
	// Custom providers may intentionally be plain HTTP (for example, a private
	// upstream hosted without TLS). Built-in providers keep their stricter HTTPS
	// policy; this path is explicitly user-configured.
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("base URL must use HTTP or HTTPS")
	}
	return nil
}

func (s *Store) CreateCustomProvider(ctx context.Context, provider CustomProvider) (CustomProvider, error) {
	if err := validateCustomProvider(&provider); err != nil {
		return CustomProvider{}, err
	}
	now := time.Now().UTC()
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return CustomProvider{}, err
	}
	provider.ID = "cp_" + hex.EncodeToString(random)
	provider.CreatedAt, provider.UpdatedAt = now, now
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO custom_providers (id, name, prefix, kind, api_type, base_url, enabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		provider.ID, provider.Name, provider.Prefix, provider.Kind, provider.APIType,
		provider.BaseURL, boolToInt(provider.Enabled), now.UnixMilli(), now.UnixMilli(),
	); err != nil {
		if isUniqueViolation(err) {
			return CustomProvider{}, ErrCustomPrefixTaken
		}
		return CustomProvider{}, fmt.Errorf("insert custom provider: %w", err)
	}
	return provider, nil
}

func (s *Store) UpdateCustomProvider(ctx context.Context, provider CustomProvider) (CustomProvider, error) {
	if strings.TrimSpace(provider.ID) == "" {
		return CustomProvider{}, errors.New("provider id is required")
	}
	if err := validateCustomProvider(&provider); err != nil {
		return CustomProvider{}, err
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE custom_providers SET name = ?, prefix = ?, kind = ?, api_type = ?, base_url = ?, enabled = ?, updated_at = ?
WHERE id = ?`,
		provider.Name, provider.Prefix, provider.Kind, provider.APIType, provider.BaseURL,
		boolToInt(provider.Enabled), now.UnixMilli(), provider.ID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return CustomProvider{}, ErrCustomPrefixTaken
		}
		return CustomProvider{}, fmt.Errorf("update custom provider: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return CustomProvider{}, sql.ErrNoRows
	}
	provider.UpdatedAt = now
	return provider, nil
}

func (s *Store) DeleteCustomProvider(ctx context.Context, id string) error {
	// Keys go with it via ON DELETE CASCADE, so a deleted provider cannot leave
	// orphaned credentials behind in the database.
	result, err := s.db.ExecContext(ctx, `DELETE FROM custom_providers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete custom provider: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListCustomProviders returns providers with their key metadata but never the key
// material itself.
func (s *Store) ListCustomProviders(ctx context.Context) ([]CustomProvider, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, prefix, kind, api_type, base_url, enabled, created_at, updated_at
FROM custom_providers ORDER BY prefix`)
	if err != nil {
		return nil, fmt.Errorf("list custom providers: %w", err)
	}
	defer rows.Close()
	providers := make([]CustomProvider, 0, 8)
	index := map[string]int{}
	for rows.Next() {
		var provider CustomProvider
		var enabled, created, updated int64
		if err := rows.Scan(&provider.ID, &provider.Name, &provider.Prefix, &provider.Kind,
			&provider.APIType, &provider.BaseURL, &enabled, &created, &updated); err != nil {
			return nil, err
		}
		provider.Enabled = enabled == 1
		provider.CreatedAt = time.UnixMilli(created).UTC()
		provider.UpdatedAt = time.UnixMilli(updated).UTC()
		index[provider.ID] = len(providers)
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	keyRows, err := s.db.QueryContext(ctx, `
SELECT id, provider_id, label, enabled, weight, created_at, last_used_at
FROM custom_provider_keys ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list custom provider keys: %w", err)
	}
	defer keyRows.Close()
	for keyRows.Next() {
		var key CustomProviderKey
		var enabled, created int64
		var lastUsed sql.NullInt64
		if err := keyRows.Scan(&key.ID, &key.ProviderID, &key.Label, &enabled, &key.Weight,
			&created, &lastUsed); err != nil {
			return nil, err
		}
		key.Enabled = enabled == 1
		key.CreatedAt = time.UnixMilli(created).UTC()
		if lastUsed.Valid {
			key.LastUsedAt = time.UnixMilli(lastUsed.Int64).UTC()
		}
		if position, ok := index[key.ProviderID]; ok {
			providers[position].Keys = append(providers[position].Keys, key)
		}
	}
	return providers, keyRows.Err()
}

// LoadCustomProviders returns enabled providers with their decrypted keys. This is
// the gateway's entry point; nothing else should decrypt key material.
func (s *Store) LoadCustomProviders(ctx context.Context) ([]CustomProvider, error) {
	providers, err := s.ListCustomProviders(ctx)
	if err != nil {
		return nil, err
	}
	secrets, err := s.db.QueryContext(ctx, `SELECT id, secret FROM custom_provider_keys`)
	if err != nil {
		return nil, fmt.Errorf("load custom provider keys: %w", err)
	}
	defer secrets.Close()
	plaintext := map[string]string{}
	for secrets.Next() {
		var id string
		var blob []byte
		if err := secrets.Scan(&id, &blob); err != nil {
			return nil, err
		}
		decrypted, err := s.box.Decrypt(blob)
		if err != nil {
			return nil, fmt.Errorf("decrypt custom provider key %s: %w", id, err)
		}
		plaintext[id] = string(decrypted)
	}
	if err := secrets.Err(); err != nil {
		return nil, err
	}
	enabled := make([]CustomProvider, 0, len(providers))
	for _, provider := range providers {
		if !provider.Enabled {
			continue
		}
		keys := make([]CustomProviderKey, 0, len(provider.Keys))
		for _, key := range provider.Keys {
			if !key.Enabled {
				continue
			}
			key.Secret = plaintext[key.ID]
			if key.Secret == "" {
				continue
			}
			keys = append(keys, key)
		}
		if len(keys) == 0 {
			// A provider with no usable key would accept traffic and then fail every
			// request, so it is not offered for routing at all.
			continue
		}
		provider.Keys = keys
		enabled = append(enabled, provider)
	}
	return enabled, nil
}

func (s *Store) AddCustomProviderKey(ctx context.Context, providerID, label, apiKey string) (CustomProviderKey, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return CustomProviderKey{}, errors.New("api key is required")
	}
	label = strings.TrimSpace(label)
	if len(label) > 64 {
		return CustomProviderKey{}, errors.New("label must be 64 characters or fewer")
	}
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM custom_providers WHERE id = ?`, providerID).Scan(&exists); err != nil {
		return CustomProviderKey{}, err
	}
	if exists == 0 {
		return CustomProviderKey{}, sql.ErrNoRows
	}
	encrypted, err := s.box.Encrypt([]byte(apiKey))
	if err != nil {
		return CustomProviderKey{}, err
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return CustomProviderKey{}, err
	}
	now := time.Now().UTC()
	key := CustomProviderKey{
		ID: "cpk_" + hex.EncodeToString(random), ProviderID: providerID, Label: label,
		Enabled: true, Weight: 1, CreatedAt: now,
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO custom_provider_keys (id, provider_id, label, secret, enabled, weight, created_at, last_used_at)
VALUES (?, ?, ?, ?, 1, 1, ?, NULL)`,
		key.ID, key.ProviderID, key.Label, encrypted, now.UnixMilli(),
	); err != nil {
		return CustomProviderKey{}, fmt.Errorf("insert custom provider key: %w", err)
	}
	return key, nil
}

func (s *Store) SetCustomProviderKeyEnabled(ctx context.Context, id string, enabled bool) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE custom_provider_keys SET enabled = ? WHERE id = ?`, boolToInt(enabled), id)
	if err != nil {
		return fmt.Errorf("update custom provider key: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteCustomProviderKey(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM custom_provider_keys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete custom provider key: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) TouchCustomProviderKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE custom_provider_keys SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().UnixMilli(), id)
	return err
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}
