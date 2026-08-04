package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"literouter/internal/secret"
)

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    plan TEXT NOT NULL DEFAULT '',
    credentials BLOB NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    weight INTEGER NOT NULL DEFAULT 1 CHECK (weight > 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_accounts_provider_enabled
    ON accounts(provider, enabled);

CREATE TABLE IF NOT EXISTS oauth_sessions (
    state TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    verifier BLOB NOT NULL,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_oauth_sessions_expires_at
    ON oauth_sessions(expires_at);

CREATE TABLE IF NOT EXISTS quota_snapshots (
    account_id TEXT NOT NULL,
    quota_key TEXT NOT NULL,
    used REAL NOT NULL,
    total REAL NOT NULL,
    remaining REAL NOT NULL,
    remaining_percent REAL NOT NULL,
    reset_at INTEGER,
    unlimited INTEGER NOT NULL DEFAULT 0 CHECK (unlimited IN (0, 1)),
    exhausted INTEGER NOT NULL DEFAULT 0 CHECK (exhausted IN (0, 1)),
    source TEXT NOT NULL,
    fetched_at INTEGER NOT NULL,
    PRIMARY KEY (account_id, quota_key),
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_account_exhausted
    ON quota_snapshots(account_id, exhausted);

CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL DEFAULT '',
    prefix TEXT NOT NULL,
    key_hash BLOB NOT NULL UNIQUE,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at INTEGER NOT NULL,
    revoked_at INTEGER,
    last_used_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_api_keys_enabled ON api_keys(enabled);

CREATE TABLE IF NOT EXISTS catalog_models (
    provider TEXT NOT NULL,
    id TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    context_window INTEGER NOT NULL DEFAULT 0,
    -- The largest max_tokens this model accepted, learned from an upstream rejection.
    -- 0 means "not yet observed"; there is deliberately no default table, because a
    -- guessed cap silently truncates every answer the model would have completed.
    max_output_tokens INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (provider, id)
);

-- A custom provider is a user-registered upstream: a base URL, a wire protocol and
-- a routing prefix. Models are addressed as "<prefix>/<model>", which is what keeps
-- routing unambiguous without hardcoding anything about the vendor.
CREATE TABLE IF NOT EXISTS custom_providers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL DEFAULT '',
    prefix TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL,
    api_type TEXT NOT NULL DEFAULT 'chat',
    base_url TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Keys are separate rows so one provider can hold several credentials and rotate
-- across them, the way the OAuth pool already does for accounts. The secret is
-- encrypted with the same box as accounts.credentials; it is never stored in clear.
CREATE TABLE IF NOT EXISTS custom_provider_keys (
    id TEXT PRIMARY KEY,
    provider_id TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    secret BLOB NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    weight INTEGER NOT NULL DEFAULT 1 CHECK (weight > 0),
    created_at INTEGER NOT NULL,
    last_used_at INTEGER,
    FOREIGN KEY (provider_id) REFERENCES custom_providers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_custom_provider_keys_provider
    ON custom_provider_keys(provider_id, enabled);

CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS usage_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    provider TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    endpoint TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'ok',
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_usage_events_ts ON usage_events(ts DESC);
CREATE INDEX IF NOT EXISTS idx_usage_events_provider ON usage_events(provider);
CREATE INDEX IF NOT EXISTS idx_usage_events_model ON usage_events(model);
`

type Account struct {
	ID          string
	Provider    string
	Label       string
	Plan        string
	Credentials []byte
	Enabled     bool
	Weight      int
	// DisabledReason is why routing stopped using this account, when the gateway is what
	// stopped it. Empty when the account is enabled, or when a human turned it off — they
	// already know why, and inventing a reason on their behalf would be noise.
	//
	// It exists because an account disabling itself is otherwise invisible: the dashboard
	// showed one switched off with nothing to say what happened, and the explanation — an
	// expired session that needs a fresh login — was a single line in a log nobody reads.
	DisabledReason string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Store struct {
	db  *sql.DB
	box *secret.Box

	apiKeyMu    sync.RWMutex
	apiKeys     map[string]string
	apiKeyDirty map[string]time.Time
	apiKeyReady bool
}

func Open(path string, box *secret.Box) (*Store, error) {
	if box == nil {
		return nil, fmt.Errorf("credential encryption is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create storage directory: %w", err)
	}

	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping SQLite: %w", err)
	}

	return &Store{
		db: db, box: box,
		apiKeys: make(map[string]string), apiKeyDirty: make(map[string]time.Time),
	}, nil
}

func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close SQLite: %w", err)
	}
	return nil
}

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate SQLite: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE accounts ADD COLUMN plan TEXT NOT NULL DEFAULT ''`); err != nil {
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "duplicate column") {
			return fmt.Errorf("migrate accounts.plan: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE accounts ADD COLUMN disabled_reason TEXT NOT NULL DEFAULT ''`); err != nil {
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "duplicate column") {
			return fmt.Errorf("migrate accounts.disabled_reason: %w", err)
		}
	}
	if err := s.migrateCatalogModels(ctx); err != nil {
		return err
	}
	for _, column := range []string{
		"prompt_estimated INTEGER NOT NULL DEFAULT 0",
		"completion_estimated INTEGER NOT NULL DEFAULT 0",
		"cached_reported INTEGER NOT NULL DEFAULT 0",
	} {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE usage_events ADD COLUMN `+column); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				return fmt.Errorf("migrate usage_events.%s: %w", strings.Fields(column)[0], err)
			}
		}
	}
	if err := s.loadAPIKeyCache(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) UpsertAccount(ctx context.Context, account Account) error {
	if err := validateAccount(account); err != nil {
		return err
	}
	encrypted, err := s.box.Encrypt(account.Credentials)
	if err != nil {
		return fmt.Errorf("encrypt account %q credentials: %w", account.ID, err)
	}
	now := time.Now().UTC()
	if account.CreatedAt.IsZero() {
		account.CreatedAt = now
	}
	if account.UpdatedAt.IsZero() {
		account.UpdatedAt = now
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO accounts (id, provider, label, plan, credentials, enabled, weight, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  provider = excluded.provider,
  label = excluded.label,
  plan = CASE WHEN excluded.plan != '' THEN excluded.plan ELSE accounts.plan END,
  credentials = excluded.credentials,
  updated_at = excluded.updated_at`,
		account.ID, account.Provider, account.Label, account.Plan, encrypted, account.Enabled, account.Weight,
		account.CreatedAt.UnixMilli(), account.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("upsert account %q: %w", account.ID, err)
	}
	return nil
}

func (s *Store) CreateAccount(ctx context.Context, account Account) error {
	if err := validateAccount(account); err != nil {
		return err
	}
	encrypted, err := s.box.Encrypt(account.Credentials)
	if err != nil {
		return fmt.Errorf("encrypt account %q credentials: %w", account.ID, err)
	}
	now := time.Now().UTC()
	if account.CreatedAt.IsZero() {
		account.CreatedAt = now
	}
	if account.UpdatedAt.IsZero() {
		account.UpdatedAt = now
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO accounts (id, provider, label, plan, credentials, enabled, weight, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		account.ID, account.Provider, account.Label, account.Plan, encrypted, account.Enabled, account.Weight,
		account.CreatedAt.UnixMilli(), account.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("create account %q: %w", account.ID, err)
	}
	return nil
}

func (s *Store) UpdateAccount(ctx context.Context, account Account) error {
	if err := validateAccount(account); err != nil {
		return err
	}
	encrypted, err := s.box.Encrypt(account.Credentials)
	if err != nil {
		return fmt.Errorf("encrypt account %q credentials: %w", account.ID, err)
	}
	account.UpdatedAt = time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE accounts
SET provider = ?, label = ?, plan = ?, credentials = ?, updated_at = ?
WHERE id = ?`,
		account.Provider, account.Label, account.Plan, encrypted,
		account.UpdatedAt.UnixMilli(), account.ID,
	)
	if err != nil {
		return fmt.Errorf("update account %q: %w", account.ID, err)
	}
	return requireChanged(result, account.ID)
}

func (s *Store) UpdateAccountRouting(ctx context.Context, id string, enabled bool, weight int) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("account ID is required")
	}
	if weight < 1 || weight > 100 {
		return errors.New("account weight must be between 1 and 100")
	}
	// The reason is cleared either way. Enabling resolves whatever it said, and a human
	// disabling an account does not need the gateway's last explanation left on the card.
	result, err := s.db.ExecContext(ctx, `
UPDATE accounts SET enabled = ?, weight = ?, disabled_reason = '', updated_at = ? WHERE id = ?`,
		enabled, weight, time.Now().UTC().UnixMilli(), id,
	)
	if err != nil {
		return fmt.Errorf("update account %q routing: %w", id, err)
	}
	return requireChanged(result, id)
}

// DisableAccountWithReason switches routing off and records why, for the cases where the
// gateway decides rather than the user — an expired session, a credential the provider will
// not refresh. The reason is what turns a mysteriously dark card into an instruction.
func (s *Store) DisableAccountWithReason(ctx context.Context, id, reason string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("account ID is required")
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 300 {
		// Providers return whole JSON documents in their error bodies. The card needs the
		// sentence, not the document.
		reason = reason[:300]
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE accounts SET enabled = 0, disabled_reason = ?, updated_at = ? WHERE id = ?`,
		reason, time.Now().UTC().UnixMilli(), id,
	)
	if err != nil {
		return fmt.Errorf("disable account %q: %w", id, err)
	}
	return requireChanged(result, id)
}

func (s *Store) UpdateAccountPlan(ctx context.Context, id, plan string) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE accounts SET plan = ?, updated_at = ? WHERE id = ?`,
		plan, time.Now().UTC().UnixMilli(), id,
	)
	if err != nil {
		return fmt.Errorf("update account %q plan: %w", id, err)
	}
	return requireChanged(result, id)
}

func (s *Store) UpdateAccountCredentials(ctx context.Context, id string, credentials []byte) error {
	encrypted, err := s.box.Encrypt(credentials)
	if err != nil {
		return fmt.Errorf("encrypt account %q credentials: %w", id, err)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE accounts SET credentials = ?, updated_at = ? WHERE id = ?`,
		encrypted, time.Now().UTC().UnixMilli(), id,
	)
	if err != nil {
		return fmt.Errorf("update account %q credentials: %w", id, err)
	}
	return requireChanged(result, id)
}

func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, provider, label, plan, credentials, enabled, weight, disabled_reason, created_at, updated_at
FROM accounts WHERE id = ?`, id)
	account, err := s.scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("get account %q: %w", id, sql.ErrNoRows)
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account %q: %w", id, err)
	}
	return account, nil
}

func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, provider, label, plan, credentials, enabled, weight, disabled_reason, created_at, updated_at
FROM accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()

	accounts := make([]Account, 0)
	for rows.Next() {
		account, err := s.scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accounts: %w", err)
	}
	return accounts, nil
}

func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete account %q: %w", id, err)
	}
	return requireChanged(result, id)
}

func validateAccount(account Account) error {
	switch {
	case account.ID == "":
		return fmt.Errorf("account ID is required")
	case account.Provider == "":
		return fmt.Errorf("account provider is required")
	case len(account.Credentials) == 0:
		return fmt.Errorf("encrypted account credentials are required")
	case account.Weight < 1:
		return fmt.Errorf("account weight must be greater than zero")
	}
	return nil
}

func requireChanged(result sql.Result, id string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected rows: %w", err)
	}
	if changed == 0 {
		return fmt.Errorf("account %q: %w", id, sql.ErrNoRows)
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanAccount(row scanner) (Account, error) {
	var (
		account         Account
		encrypted       []byte
		createdAtMillis int64
		updatedAtMillis int64
	)
	if err := row.Scan(
		&account.ID, &account.Provider, &account.Label, &account.Plan, &encrypted,
		&account.Enabled, &account.Weight, &account.DisabledReason, &createdAtMillis, &updatedAtMillis,
	); err != nil {
		return Account{}, err
	}
	credentials, err := s.box.Decrypt(encrypted)
	if err != nil {
		return Account{}, fmt.Errorf("decrypt account %q credentials: %w", account.ID, err)
	}
	account.Credentials = credentials
	account.CreatedAt = time.UnixMilli(createdAtMillis).UTC()
	account.UpdatedAt = time.UnixMilli(updatedAtMillis).UTC()
	return account, nil
}
