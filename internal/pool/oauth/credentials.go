package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"literouter/internal/pool"
	"literouter/internal/storage"
)

type CredentialManager struct {
	store     *storage.Store
	pool      *pool.Pool
	providers map[string]OAuthProvider
	logger    *slog.Logger

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

func NewCredentialManager(store *storage.Store, accountPool *pool.Pool, logger *slog.Logger, providers ...OAuthProvider) *CredentialManager {
	manager := &CredentialManager{
		store: store, pool: accountPool, logger: logger,
		providers: make(map[string]OAuthProvider, len(providers)), locks: make(map[string]*sync.Mutex),
	}
	for _, provider := range providers {
		manager.providers[provider.Name()] = provider
	}
	return manager
}

// RetireRejectedAccount switches an account off because the upstream refused its
// credentials, recording the upstream's own words so the dashboard can say what happened.
//
// This exists because a rejected credential used to be invisible. Refresh failures were
// handled — LoadFresh disables on a permanent refresh error — but a token the provider
// rejects at request time is not a refresh failure, so nothing recorded it: the account
// stayed enabled, the selector kept picking it, every turn burned a round trip on it
// before falling through, and the only trace was a debug line the default log level does
// not print. Three dead accounts looked exactly like three healthy ones on the dashboard,
// while the whole load funnelled onto the one that still worked and overloaded it.
//
// Disabling rather than a softer flag is deliberate: the selector already skips disabled
// accounts and the dashboard already explains them, so this reuses the one path the rest
// of the system understands. Re-authenticating or re-enabling clears the reason.
func (m *CredentialManager) RetireRejectedAccount(ctx context.Context, accountID, provider, reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "the provider rejected this account's credentials; sign in again"
	}
	if m.store != nil {
		if err := m.store.DisableAccountWithReason(ctx, accountID, reason); err != nil {
			m.log().Warn("could not record a rejected account", "account_id", accountID, "error", err)
		}
	}
	if m.pool != nil {
		m.pool.SetDisabled(accountID, true, reason)
	}
	// Warn, not debug: this is the one event that silently removes capacity while looking
	// like nothing happened, and it needs a human to sign in again before it recovers.
	m.log().Warn("account credentials were rejected upstream; routing disabled until you sign in again",
		"account_id", accountID, "provider", provider, "reason", reason)
}

func (m *CredentialManager) log() *slog.Logger {
	if m.logger != nil {
		return m.logger
	}
	return slog.Default()
}

func (m *CredentialManager) LoadFresh(ctx context.Context, accountID string) (storage.Account, TokenSet, AccountInfo, error) {
	lock := m.accountLock(accountID)
	lock.Lock()
	defer lock.Unlock()

	account, err := m.store.GetAccount(ctx, accountID)
	if err != nil {
		return storage.Account{}, TokenSet{}, AccountInfo{}, err
	}
	provider := m.providers[account.Provider]
	var token TokenSet
	if err := json.Unmarshal(account.Credentials, &token); err != nil {
		return storage.Account{}, TokenSet{}, AccountInfo{}, fmt.Errorf("decode account credentials: %w", err)
	}
	if provider == nil {
		// Some providers have no authorization endpoint at all — a Cursor session is
		// copied out of the IDE, and there is nothing to refresh or to ask for account
		// info. Those accounts are served straight from what was imported. An expired
		// one is reported here rather than sent upstream, where it would come back as
		// an opaque auth failure with no hint that a re-import is what fixes it.
		if !token.ExpiresAt.IsZero() && time.Now().After(token.ExpiresAt) {
			reason := fmt.Sprintf("the imported %s session expired on %s; import a fresh one",
				account.Provider, token.ExpiresAt.Format("2006-01-02"))
			if updateErr := m.store.DisableAccountWithReason(ctx, account.ID, reason); updateErr != nil {
				return storage.Account{}, TokenSet{}, AccountInfo{}, updateErr
			}
			m.pool.SetDisabled(account.ID, true, reason)
			return storage.Account{}, TokenSet{}, AccountInfo{}, errors.New(reason)
		}
		return account, token, AccountInfo{ID: account.ID, Email: account.Label, Plan: account.Plan}, nil
	}
	if shouldRefresh(provider, &token, time.Now()) {
		refreshed, err := provider.Refresh(ctx, token.RefreshToken)
		if err != nil {
			var providerError *ProviderError
			if errors.As(err, &providerError) && providerError.Permanent {
				account.Enabled = false
				// The provider's own words are recorded, not a paraphrase: "Your session has
				// ended. Please log in again." tells the user exactly what to do, where a
				// generic "credentials expired" would not. Without this the dashboard showed a
				// card switched off with no explanation anywhere the user would look.
				reason := strings.TrimSpace(providerError.Message)
				if reason == "" {
					reason = "the provider refused to refresh this account's credentials"
				}
				if updateErr := m.store.DisableAccountWithReason(ctx, account.ID, reason); updateErr != nil {
					return storage.Account{}, TokenSet{}, AccountInfo{}, errors.Join(err, updateErr)
				}
				m.pool.SetDisabled(account.ID, true, reason)
			}
			return storage.Account{}, TokenSet{}, AccountInfo{}, err
		}
		mergeToken(&token, refreshed)
		credentials, err := json.Marshal(token)
		if err != nil {
			return storage.Account{}, TokenSet{}, AccountInfo{}, fmt.Errorf("encode refreshed credentials: %w", err)
		}
		account.Credentials = credentials
		if err := m.store.UpdateAccountCredentials(ctx, account.ID, credentials); err != nil {
			return storage.Account{}, TokenSet{}, AccountInfo{}, err
		}
	}
	credentialsBeforeInfo, err := json.Marshal(token)
	if err != nil {
		return storage.Account{}, TokenSet{}, AccountInfo{}, fmt.Errorf("encode OAuth credentials: %w", err)
	}
	info, err := provider.AccountInfo(ctx, &token)
	if err != nil {
		return storage.Account{}, TokenSet{}, AccountInfo{}, err
	}
	credentialsAfterInfo, err := json.Marshal(token)
	if err != nil {
		return storage.Account{}, TokenSet{}, AccountInfo{}, fmt.Errorf("encode OAuth credentials: %w", err)
	}
	if string(credentialsAfterInfo) != string(credentialsBeforeInfo) {
		account.Credentials = credentialsAfterInfo
		if err := m.store.UpdateAccountCredentials(ctx, account.ID, credentialsAfterInfo); err != nil {
			return storage.Account{}, TokenSet{}, AccountInfo{}, err
		}
	}
	if info.Plan != "" && account.Plan != info.Plan {
		account.Plan = info.Plan
		if updateErr := m.store.UpdateAccountPlan(ctx, account.ID, info.Plan); updateErr == nil && m.pool != nil {
			m.pool.UpdateMetadata(account.ID, info.Email, info.Plan)
		}
	}
	return account, token, *info, nil
}

func (m *CredentialManager) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	m.refreshDue(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshDue(ctx)
		}
	}
}

func (m *CredentialManager) refreshDue(ctx context.Context) {
	accounts, err := m.store.ListAccounts(ctx)
	if err != nil {
		m.logger.Warn("list accounts for token refresh", "error", err)
		return
	}
	for _, account := range accounts {
		if !account.Enabled || m.providers[account.Provider] == nil {
			continue
		}
		refreshContext, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, _, _, err := m.LoadFresh(refreshContext, account.ID)
		cancel()
		if err != nil {
			m.logger.Warn("proactive OAuth refresh failed", "provider", account.Provider, "account_id", account.ID, "error", err)
		}
	}
}

func (m *CredentialManager) accountLock(id string) *sync.Mutex {
	m.locksMu.Lock()
	defer m.locksMu.Unlock()
	lock := m.locks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[id] = lock
	}
	return lock
}

func shouldRefresh(provider OAuthProvider, token *TokenSet, now time.Time) bool {
	if refreshable, ok := provider.(interface {
		ShouldRefresh(*TokenSet, time.Time) bool
	}); ok {
		return refreshable.ShouldRefresh(token, now)
	}
	return token.RefreshToken != "" && !token.ExpiresAt.IsZero() && !token.ExpiresAt.After(now.Add(5*time.Minute))
}

func mergeToken(current *TokenSet, refreshed *TokenSet) {
	if refreshed.AccessToken != "" {
		current.AccessToken = refreshed.AccessToken
	}
	if refreshed.RefreshToken != "" {
		current.RefreshToken = refreshed.RefreshToken
	}
	if refreshed.IDToken != "" {
		current.IDToken = refreshed.IDToken
	}
	if refreshed.TokenType != "" {
		current.TokenType = refreshed.TokenType
	}
	if refreshed.Scope != "" {
		current.Scope = refreshed.Scope
	}
	if refreshed.ProjectID != "" {
		current.ProjectID = refreshed.ProjectID
	}
	if !refreshed.ExpiresAt.IsZero() {
		current.ExpiresAt = refreshed.ExpiresAt
	}
	if !refreshed.LastRefreshAt.IsZero() {
		current.LastRefreshAt = refreshed.LastRefreshAt
	}
}
