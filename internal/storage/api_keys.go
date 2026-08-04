package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const apiKeyUsageFlushInterval = time.Minute

type APIKey struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	RevokedAt  time.Time `json:"revoked_at,omitempty"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	Token      string    `json:"token,omitempty"` // only on create
}

func (s *Store) CreateAPIKey(ctx context.Context, name string) (APIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	if len(name) > 64 {
		return APIKey{}, fmt.Errorf("name must be 64 characters or fewer")
	}
	token, err := newAPIKeyToken()
	if err != nil {
		return APIKey{}, err
	}
	now := time.Now().UTC()
	hash := hashToken(token)
	id := "key_" + hex.EncodeToString(hash[:8])
	key := APIKey{ID: id, Name: name, Prefix: tokenPrefix(token), Enabled: true, CreatedAt: now, Token: token}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO api_keys (id, name, prefix, key_hash, enabled, created_at, revoked_at, last_used_at)
VALUES (?, ?, ?, ?, 1, ?, NULL, NULL)`,
		key.ID, key.Name, key.Prefix, hash, now.UnixMilli(),
	); err != nil {
		return APIKey{}, fmt.Errorf("insert api key: %w", err)
	}
	s.apiKeyMu.Lock()
	s.apiKeys[string(hash)] = key.ID
	s.apiKeyReady = true
	s.apiKeyMu.Unlock()
	return key, nil
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, prefix, enabled, created_at, revoked_at, last_used_at
FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) SetAPIKeyEnabled(ctx context.Context, id string, enabled bool) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("api key id is required")
	}
	var revoked any
	if !enabled {
		revoked = time.Now().UTC().UnixMilli()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE api_keys SET enabled = ?, revoked_at = ? WHERE id = ?`, boolToInt(enabled), revoked, id)
	if err != nil {
		return fmt.Errorf("update api key: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("api key not found")
	}
	if err := s.refreshAPIKeyCacheEntry(ctx, id); err != nil {
		return err
	}
	return nil
}

func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("api key id is required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete api key: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("api key not found")
	}
	s.apiKeyMu.Lock()
	for hash, cachedID := range s.apiKeys {
		if cachedID == id {
			delete(s.apiKeys, hash)
		}
	}
	delete(s.apiKeyDirty, id)
	s.apiKeyMu.Unlock()
	return nil
}

func (s *Store) ValidAPIKey(_ context.Context, token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	hash := hashToken(token)
	s.apiKeyMu.RLock()
	id, ok := s.apiKeys[string(hash)]
	ready := s.apiKeyReady
	s.apiKeyMu.RUnlock()
	if !ready || !ok {
		return false
	}
	s.apiKeyMu.Lock()
	s.apiKeyDirty[id] = time.Now().UTC()
	s.apiKeyMu.Unlock()
	return true
}

func (s *Store) ValidAPIKeyConstantTime(master, provided string) bool {
	if subtle.ConstantTimeCompare([]byte(provided), []byte(master)) == 1 {
		return true
	}
	return s.ValidAPIKey(context.Background(), provided)
}

func (s *Store) loadAPIKeyCache(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, key_hash FROM api_keys WHERE enabled = 1 AND revoked_at IS NULL`)
	if err != nil {
		return fmt.Errorf("load api key cache: %w", err)
	}
	defer rows.Close()
	keys := make(map[string]string)
	for rows.Next() {
		var id string
		var hash []byte
		if err := rows.Scan(&id, &hash); err != nil {
			return fmt.Errorf("scan api key cache: %w", err)
		}
		keys[string(hash)] = id
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load api key cache: %w", err)
	}
	s.apiKeyMu.Lock()
	s.apiKeys = keys
	s.apiKeyReady = true
	s.apiKeyMu.Unlock()
	return nil
}

func (s *Store) refreshAPIKeyCacheEntry(ctx context.Context, id string) error {
	var hash []byte
	var enabled int
	var revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT key_hash, enabled, revoked_at FROM api_keys WHERE id = ?`, id).Scan(&hash, &enabled, &revoked)
	if err != nil {
		return fmt.Errorf("reload api key cache entry: %w", err)
	}
	s.apiKeyMu.Lock()
	for cachedHash, cachedID := range s.apiKeys {
		if cachedID == id {
			delete(s.apiKeys, cachedHash)
		}
	}
	if enabled == 1 && !revoked.Valid {
		s.apiKeys[string(hash)] = id
	}
	if enabled != 1 || revoked.Valid {
		delete(s.apiKeyDirty, id)
	}
	s.apiKeyMu.Unlock()
	return nil
}

func (s *Store) RunAPIKeyMaintenance(ctx context.Context) {
	ticker := time.NewTicker(apiKeyUsageFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.FlushAPIKeyUsage(flushCtx)
			cancel()
			return
		case <-ticker.C:
			_ = s.FlushAPIKeyUsage(ctx)
		}
	}
}

func (s *Store) FlushAPIKeyUsage(ctx context.Context) error {
	s.apiKeyMu.Lock()
	if len(s.apiKeyDirty) == 0 {
		s.apiKeyMu.Unlock()
		return nil
	}
	dirty := s.apiKeyDirty
	s.apiKeyDirty = make(map[string]time.Time)
	s.apiKeyMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.restoreDirtyAPIKeys(dirty)
		return fmt.Errorf("begin api key usage batch: %w", err)
	}
	defer tx.Rollback()
	for id, usedAt := range dirty {
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, usedAt.UnixMilli(), id); err != nil {
			s.restoreDirtyAPIKeys(dirty)
			return fmt.Errorf("update api key usage: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		s.restoreDirtyAPIKeys(dirty)
		return fmt.Errorf("commit api key usage batch: %w", err)
	}
	return nil
}

func (s *Store) restoreDirtyAPIKeys(dirty map[string]time.Time) {
	s.apiKeyMu.Lock()
	defer s.apiKeyMu.Unlock()
	for id, usedAt := range dirty {
		if current, ok := s.apiKeyDirty[id]; !ok || usedAt.After(current) {
			s.apiKeyDirty[id] = usedAt
		}
	}
}

func scanAPIKey(row interface{ Scan(dest ...any) error }) (APIKey, error) {
	var key APIKey
	var enabled int
	var createdAt int64
	var revokedAt, lastUsed sql.NullInt64
	if err := row.Scan(&key.ID, &key.Name, &key.Prefix, &enabled, &createdAt, &revokedAt, &lastUsed); err != nil {
		return APIKey{}, fmt.Errorf("scan api key: %w", err)
	}
	key.Enabled = enabled == 1
	key.CreatedAt = time.UnixMilli(createdAt).UTC()
	if revokedAt.Valid {
		key.RevokedAt = time.UnixMilli(revokedAt.Int64).UTC()
	}
	if lastUsed.Valid {
		key.LastUsedAt = time.UnixMilli(lastUsed.Int64).UTC()
	}
	return key, nil
}

func newAPIKeyToken() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return "sk-lr-" + hex.EncodeToString(raw[:]), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func tokenPrefix(token string) string {
	if len(token) <= 12 {
		return token
	}
	return token[:12]
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
