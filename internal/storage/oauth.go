package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type OAuthSession struct {
	State     string
	Provider  string
	Verifier  string
	ExpiresAt time.Time
}

func (s *Store) PutOAuthSession(ctx context.Context, session OAuthSession) error {
	if session.State == "" || session.Provider == "" || session.Verifier == "" || session.ExpiresAt.IsZero() {
		return fmt.Errorf("OAuth session is incomplete")
	}
	encryptedVerifier, err := s.box.Encrypt([]byte(session.Verifier))
	if err != nil {
		return fmt.Errorf("encrypt OAuth verifier: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO oauth_sessions (state, provider, verifier, expires_at, created_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(state) DO UPDATE SET
  provider = excluded.provider,
  verifier = excluded.verifier,
  expires_at = excluded.expires_at,
  created_at = excluded.created_at`,
		session.State, session.Provider, encryptedVerifier, session.ExpiresAt.UnixMilli(), time.Now().UTC().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("store OAuth session: %w", err)
	}
	return nil
}

func (s *Store) TakeOAuthSession(ctx context.Context, state, provider string) (OAuthSession, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OAuthSession{}, fmt.Errorf("begin OAuth session transaction: %w", err)
	}
	defer tx.Rollback()

	var encryptedVerifier []byte
	var expiresAtMillis int64
	now := time.Now().UTC()
	err = tx.QueryRowContext(ctx, `
DELETE FROM oauth_sessions
WHERE state = ? AND provider = ? AND expires_at > ?
RETURNING verifier, expires_at`, state, provider, now.UnixMilli()).Scan(&encryptedVerifier, &expiresAtMillis)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthSession{}, fmt.Errorf("OAuth session not found or already used: %w", sql.ErrNoRows)
	}
	if err != nil {
		return OAuthSession{}, fmt.Errorf("take OAuth session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OAuthSession{}, fmt.Errorf("commit OAuth session: %w", err)
	}

	expiresAt := time.UnixMilli(expiresAtMillis).UTC()
	verifier, err := s.box.Decrypt(encryptedVerifier)
	if err != nil {
		return OAuthSession{}, fmt.Errorf("decrypt OAuth verifier: %w", err)
	}
	return OAuthSession{State: state, Provider: provider, Verifier: string(verifier), ExpiresAt: expiresAt}, nil
}

// TakeLatestOAuthSession consumes the newest unexpired session for a provider.
// Used when the user pastes a bare authorization code without the state param.
func (s *Store) TakeLatestOAuthSession(ctx context.Context, provider string) (OAuthSession, error) {
	if provider == "" {
		return OAuthSession{}, fmt.Errorf("OAuth provider is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OAuthSession{}, fmt.Errorf("begin OAuth session transaction: %w", err)
	}
	defer tx.Rollback()

	var state string
	var encryptedVerifier []byte
	var expiresAtMillis int64
	now := time.Now().UTC()
	err = tx.QueryRowContext(ctx, `
DELETE FROM oauth_sessions
WHERE state = (
  SELECT state FROM oauth_sessions
  WHERE provider = ? AND expires_at > ?
  ORDER BY created_at DESC
  LIMIT 1
)
RETURNING state, verifier, expires_at`, provider, now.UnixMilli()).Scan(&state, &encryptedVerifier, &expiresAtMillis)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthSession{}, fmt.Errorf("no active OAuth session for %s — open Connect again: %w", provider, sql.ErrNoRows)
	}
	if err != nil {
		return OAuthSession{}, fmt.Errorf("take latest OAuth session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OAuthSession{}, fmt.Errorf("commit OAuth session: %w", err)
	}
	expiresAt := time.UnixMilli(expiresAtMillis).UTC()
	verifier, err := s.box.Decrypt(encryptedVerifier)
	if err != nil {
		return OAuthSession{}, fmt.Errorf("decrypt OAuth verifier: %w", err)
	}
	return OAuthSession{State: state, Provider: provider, Verifier: string(verifier), ExpiresAt: expiresAt}, nil
}

func (s *Store) DeleteExpiredOAuthSessions(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM oauth_sessions WHERE expires_at <= ?", now.UnixMilli()); err != nil {
		return fmt.Errorf("delete expired OAuth sessions: %w", err)
	}
	return nil
}
