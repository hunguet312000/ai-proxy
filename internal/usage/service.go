package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"literouter/internal/pool"
	"literouter/internal/pool/oauth"
	"literouter/internal/storage"
)

type Service struct {
	tracker     *Tracker
	credentials *oauth.CredentialManager
	pool        *pool.Pool
	logger      *slog.Logger
	writer      *usageWriter
	backoffMu   sync.Mutex
	backoff     map[string]refreshBackoff
}

// refreshBackoff spaces out health checks for an account whose provider keeps refusing
// them, and remembers how many times in a row it has.
type refreshBackoff struct {
	failures int
	nextAt   time.Time
}

// ErrCredentialsRejected marks a quota failure that was the provider refusing the account's
// credentials rather than refusing the quota call. A fetcher wraps it so the refresh loop can
// retire the account, which matters for an account no traffic reaches: it is never rejected at
// request time, so the health check is the only place its death can be noticed.
var ErrCredentialsRejected = errors.New("the provider rejected this account's credentials; sign in again")

// A quota endpoint can fail permanently while inference keeps working, and then a
// once-a-minute health check is a once-a-minute warning forever. Measured on a live
// instance: three accounts whose provider returned 403 PERMISSION_DENIED for the quota API
// produced 111 of the 128 warnings in the log, at which point the log stops being useful —
// the genuinely interesting lines were buried, repeatedly, while diagnosing something else.
//
// So the check backs off per account and the warning quiets down. Neither gives up: a
// permission can be granted later, and the ceiling keeps the check running at a rate that
// notices when it is.
const (
	refreshBackoffBase = 2 * time.Minute
	refreshBackoffMax  = 30 * time.Minute
	// maxRefreshWarnings is how many consecutive failures are worth a warning. The same
	// failure for the twentieth time is not new information.
	maxRefreshWarnings = 3
)

func NewService(store *storage.Store, accountPool *pool.Pool, credentials *oauth.CredentialManager) *Service {
	service := &Service{
		tracker:     NewTracker(store, accountPool, NewCodexFetcher(nil), NewClaudeFetcher(nil), NewGrokFetcher(nil), NewAntigravityFetcher(nil)),
		credentials: credentials,
		pool:        accountPool,
		logger:      slog.Default(),
	}
	service.writer = newUsageWriter(store, func() *slog.Logger { return service.logger }, 0, 0, 0)
	return service
}

func (s *Service) SetLogger(logger *slog.Logger) {
	if logger != nil {
		s.logger = logger
	}
}

func (s *Service) RefreshAccount(ctx context.Context, accountID string) (Quota, error) {
	account, token, info, err := s.credentials.LoadFresh(ctx, accountID)
	if err != nil {
		return Quota{}, err
	}
	providerAccountID := info.ID
	if account.Provider == "antigravity" {
		providerAccountID = token.ProjectID
	}
	quota, err := s.tracker.Refresh(ctx, account, token.AccessToken, providerAccountID)
	if err == nil {
		// Cleared here rather than in the polling loop so a refresh the user asked for from
		// the dashboard also restores the normal cadence — otherwise a fixed permission would
		// stay throttled by the history of it being broken.
		s.clearRefreshBackoff(accountID)
	}
	return quota, err
}

// ResetCodexSession consumes one Codex session-reset credit then refreshes quota.
func (s *Service) ResetCodexSession(ctx context.Context, accountID string) (Quota, error) {
	account, token, info, err := s.credentials.LoadFresh(ctx, accountID)
	if err != nil {
		return Quota{}, err
	}
	if account.Provider != "codex" {
		return Quota{}, fmt.Errorf("reset credits are only available for Codex accounts")
	}
	// Ensure credits remain before spending.
	available, err := FetchCodexResetCredits(ctx, nil, token.AccessToken, info.ID)
	if err != nil {
		return Quota{}, err
	}
	if available <= 0 {
		return Quota{}, fmt.Errorf("no Codex reset credits available")
	}
	redeemID := fmt.Sprintf("literouter-%d", time.Now().UnixNano())
	if err := ConsumeCodexResetCredit(ctx, nil, token.AccessToken, info.ID, redeemID); err != nil {
		return Quota{}, err
	}
	return s.tracker.Refresh(ctx, account, token.AccessToken, info.ID)
}

// Run periodically refreshes quota/health for every enabled account.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	s.refreshAll(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshAll(ctx)
		}
	}
}

func (s *Service) RunRetention(ctx context.Context, retention time.Duration) {
	if s == nil || s.tracker == nil || s.tracker.store == nil || retention <= 0 {
		return
	}
	purge := func() {
		deleted, err := s.tracker.store.DeleteUsageEventsBefore(ctx, time.Now().UTC().Add(-retention), 1000)
		if err != nil {
			if ctx.Err() == nil && s.logger != nil {
				s.logger.Warn("purge expired usage events", "error", err)
			}
			return
		}
		if deleted > 0 && s.logger != nil {
			s.logger.Info("purged expired usage events", "events", deleted)
		}
	}
	purge()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purge()
		}
	}
}

func (s *Service) refreshAll(ctx context.Context) {
	if s.pool == nil {
		return
	}
	now := time.Now()
	for _, account := range s.pool.List() {
		if !account.Enabled {
			continue
		}
		if !s.refreshDue(account.ID, now) {
			continue
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := s.RefreshAccount(refreshCtx, account.ID)
		cancel()
		if err == nil {
			continue
		}
		if errors.Is(err, ErrCredentialsRejected) && s.credentials != nil {
			// Not backed off and not quieted: this one does not resolve itself, and the account
			// is dead capacity until someone signs in. Retiring it here is what makes an idle
			// account's death visible at all — nothing routes to it, so nothing else would ever
			// find out.
			s.credentials.RetireRejectedAccount(ctx, account.ID, account.Provider, "")
			continue
		}
		failures, next := s.recordRefreshFailure(account.ID, now)
		if failures <= maxRefreshWarnings {
			s.logger.Warn("account health refresh failed", "account_id", account.ID,
				"provider", account.Provider, "error", err,
				"consecutive_failures", failures, "next_attempt_in", next.Sub(now).Round(time.Second).String())
			continue
		}
		s.logger.Debug("account health refresh still failing", "account_id", account.ID,
			"provider", account.Provider, "error", err,
			"consecutive_failures", failures, "next_attempt_in", next.Sub(now).Round(time.Second).String())
	}
}

// refreshDue reports whether an account's health check is allowed to run yet.
func (s *Service) refreshDue(accountID string, now time.Time) bool {
	s.backoffMu.Lock()
	defer s.backoffMu.Unlock()
	state, known := s.backoff[accountID]
	return !known || !now.Before(state.nextAt)
}

// recordRefreshFailure extends an account's backoff and reports the new failure count and
// when the next attempt is allowed.
func (s *Service) recordRefreshFailure(accountID string, now time.Time) (int, time.Time) {
	s.backoffMu.Lock()
	defer s.backoffMu.Unlock()
	if s.backoff == nil {
		s.backoff = map[string]refreshBackoff{}
	}
	state := s.backoff[accountID]
	state.failures++
	delay := refreshBackoffBase << min(state.failures-1, 16)
	if delay > refreshBackoffMax || delay <= 0 {
		delay = refreshBackoffMax
	}
	state.nextAt = now.Add(delay)
	s.backoff[accountID] = state
	return state.failures, state.nextAt
}

// clearRefreshBackoff forgets an account's failures, so one success restores the normal
// cadence rather than leaving it throttled by history.
func (s *Service) clearRefreshBackoff(accountID string) {
	s.backoffMu.Lock()
	defer s.backoffMu.Unlock()
	delete(s.backoff, accountID)
}
