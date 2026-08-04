package pool

import (
	"sort"
	"strings"
	"sync"
	"time"
)

type Account struct {
	ID                string
	Provider          string
	Label             string
	Plan              string
	ResetCredits      int
	ResetCreditsKnown bool
	Enabled           bool
	Weight            int
	// DisabledReason carries why the gateway switched this account off, so the dashboard can
	// say so instead of showing a dark card with no explanation.
	DisabledReason        string
	QuotaRemainingPercent float64
	QuotaExhausted        bool
	QuotaResetAt          time.Time
	QuotaUpdatedAt        time.Time
	ModelQuotas           map[string]QuotaSnapshot
	Models                []string
	MaxRequestsPerMinute  int
	MaxTokensPerHour      int
}

type Pool struct {
	mu       sync.RWMutex
	accounts map[string]Account
}

func New(accounts []Account) *Pool {
	p := &Pool{accounts: make(map[string]Account, len(accounts))}
	for _, account := range accounts {
		if account.QuotaUpdatedAt.IsZero() {
			account.QuotaRemainingPercent = 100
		}
		p.accounts[account.ID] = cloneAccount(account)
	}
	return p
}

func cloneAccount(account Account) Account {
	account.Models = append([]string(nil), account.Models...)
	if account.ModelQuotas != nil {
		quotas := make(map[string]QuotaSnapshot, len(account.ModelQuotas))
		for key, snapshot := range account.ModelQuotas {
			quotas[key] = snapshot
		}
		account.ModelQuotas = quotas
	}
	return account
}

func (p *Pool) Upsert(account Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.accounts[account.ID]; ok {
		if account.QuotaUpdatedAt.IsZero() {
			account.QuotaRemainingPercent = existing.QuotaRemainingPercent
			account.QuotaExhausted = existing.QuotaExhausted
			account.QuotaResetAt = existing.QuotaResetAt
			account.QuotaUpdatedAt = existing.QuotaUpdatedAt
		}
		if account.ModelQuotas == nil {
			account.ModelQuotas = existing.ModelQuotas
		}
		if account.Plan == "" {
			account.Plan = existing.Plan
		}
		if !account.ResetCreditsKnown {
			account.ResetCredits = existing.ResetCredits
			account.ResetCreditsKnown = existing.ResetCreditsKnown
		}
		if account.Label == "" {
			account.Label = existing.Label
		}
	} else if account.QuotaUpdatedAt.IsZero() {
		account.QuotaRemainingPercent = 100
	}
	p.accounts[account.ID] = cloneAccount(account)
}

func (p *Pool) SetEnabled(id string, enabled bool) bool {
	return p.SetDisabled(id, !enabled, "")
}

// SetDisabled switches routing for an account and records why, so the dashboard can explain
// a card it turned off by itself. Enabling always clears the reason: whatever it said has
// been resolved.
func (p *Pool) SetDisabled(id string, disabled bool, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	account, ok := p.accounts[id]
	if !ok {
		return false
	}
	account.Enabled = !disabled
	if disabled {
		account.DisabledReason = reason
	} else {
		account.DisabledReason = ""
	}
	p.accounts[id] = account
	return true
}

func (p *Pool) UpdateMetadata(id, label, plan string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	account, ok := p.accounts[id]
	if !ok {
		return false
	}
	if label != "" {
		account.Label = label
	}
	if plan != "" {
		account.Plan = plan
	}
	p.accounts[id] = account
	return true
}

func (p *Pool) UpdateResetCredits(id string, credits int, known bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	account, ok := p.accounts[id]
	if !ok {
		return false
	}
	account.ResetCredits = credits
	account.ResetCreditsKnown = known
	p.accounts[id] = account
	return true
}

func (p *Pool) Get(id string) (Account, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	account, ok := p.accounts[id]
	return cloneAccount(account), ok
}

func (p *Pool) Remove(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.accounts, id)
}

func (p *Pool) UpdateQuota(id string, remainingPercent float64, exhausted bool, resetAt, updatedAt time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	account, ok := p.accounts[id]
	if !ok {
		return false
	}
	account.QuotaRemainingPercent = remainingPercent
	account.QuotaExhausted = exhausted
	account.QuotaResetAt = resetAt
	account.QuotaUpdatedAt = updatedAt
	p.accounts[id] = account
	return true
}

func (p *Pool) List() []Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	accounts := make([]Account, 0, len(p.accounts))
	for _, account := range p.accounts {
		accounts = append(accounts, cloneAccount(account))
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	return accounts
}

func (p *Pool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

func (p *Pool) RestoreQuota(id string, snapshots []QuotaSnapshot) bool {
	if len(snapshots) == 0 {
		return false
	}
	remaining := 100.0
	modelQuotas := make(map[string]QuotaSnapshot, len(snapshots))
	var exhausted, hasPrepaid bool
	var resetAt, updatedAt time.Time
	for _, snapshot := range snapshots {
		modelQuotas[snapshot.Key] = snapshot
		if snapshot.Key == "prepaid" && snapshot.Remaining > 0 {
			hasPrepaid = true
		}
		ignored := snapshot.Unlimited || snapshot.Key == "prepaid" || strings.HasPrefix(snapshot.Key, "review_")
		if !ignored && snapshot.RemainingPercent < remaining {
			remaining = snapshot.RemainingPercent
		}
		if !ignored && snapshot.Exhausted {
			exhausted = true
		}
		if !ignored && !snapshot.ResetAt.IsZero() && (resetAt.IsZero() || snapshot.ResetAt.Before(resetAt)) {
			resetAt = snapshot.ResetAt
		}
		if snapshot.FetchedAt.After(updatedAt) {
			updatedAt = snapshot.FetchedAt
		}
	}
	if hasPrepaid {
		exhausted = false
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	account, ok := p.accounts[id]
	if !ok {
		return false
	}
	account.ModelQuotas = modelQuotas
	account.QuotaRemainingPercent = remaining
	account.QuotaExhausted = exhausted
	account.QuotaResetAt = resetAt
	account.QuotaUpdatedAt = updatedAt
	p.accounts[id] = account
	return true
}

type QuotaSnapshot struct {
	Key              string
	Remaining        float64
	RemainingPercent float64
	ResetAt          time.Time
	FetchedAt        time.Time
	Unlimited        bool
	Exhausted        bool
}
