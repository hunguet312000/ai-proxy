package pool

import (
	"errors"
	"hash/fnv"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

type SelectionStrategy string

const (
	StrategyRoundRobin   SelectionStrategy = "round_robin"
	StrategyWeighted     SelectionStrategy = "weighted"
	StrategyLeastUsed    SelectionStrategy = "least_used"
	StrategyLeastUsedRPM SelectionStrategy = "least_used_rpm"
	StrategySticky       SelectionStrategy = "sticky"
	StrategyStickySoft   SelectionStrategy = "sticky_soft"
	StrategyFailover     SelectionStrategy = "failover"
	StrategySmart        SelectionStrategy = "smart"
)

// Soft-sticky pressure thresholds. Preferred account is kept while healthy; once
// these fire and a healthier peer exists, the session affinity migrates.
const (
	stickySoftQuotaPressure      = 15.0            // percent remaining
	stickySoftLimitPressureRatio = 0.8             // of MaxRequestsPerMinute / MaxTokensPerHour
	stickySoftErrorPressure      = 2               // consecutive errors before soft migrate
	stickySoftMigrateCooldown    = 5 * time.Second // pin through tool-loop bursts
	stickySoftAffinityTTL        = 24 * time.Hour
	stickySoftAffinityMaxEntries = 4096
)

var ErrNoAccount = errors.New("no eligible account")

type SelectRequest struct {
	Provider       string
	Model          string
	ConversationID string
	FirstMessage   string
	ExcludeIDs     map[string]struct{}
}

type SelectResult struct {
	Account       Account
	ResolvedModel string
	ReservationID uint64
}

type requestReservation struct {
	accountID string
}

type Selector struct {
	mu              sync.Mutex
	pool            *Pool
	strategy        SelectionStrategy
	aliases         map[string][]string
	runtime         map[string]*accountRuntime
	affinity        map[string]affinityBinding // session/conversation -> account
	modelCooldown   map[string]time.Time
	reservations    map[uint64]requestReservation
	nextReservation uint64
	rr              uint64
	now             func() time.Time
}

type affinityBinding struct {
	accountID  string
	boundAt    time.Time
	lastUsedAt time.Time
}

// accountRuntime is the in-memory selection state for one account: what it has spent
// recently and whether it is being held back.
//
// There is deliberately no "healthy" flag here. There used to be one, set by a SetHealthy
// method that nothing ever called, so the eligibility check it guarded could not fire and
// the pool looked like it tracked account health when it did not. Credentials the provider
// has rejected are recorded on the account itself instead (Enabled plus DisabledReason,
// persisted by the credential manager), which survives a restart and gives the dashboard
// something to show — neither of which an in-memory flag could do.
type accountRuntime struct {
	tokensHour        rollingCounter
	requestsMinute    rollingCounter
	consecutiveErrors int
	consecutive429s   int
	cooldownUntil     time.Time
	circuitUntil      time.Time
	lastUsed          time.Time
	currentWeight     int
	pendingRequests   int64
}

type rollingCounter struct {
	events []counterEvent
}

type counterEvent struct {
	at    time.Time
	value int64
}

func NewSelector(accountPool *Pool, strategy SelectionStrategy, aliases map[string][]string) *Selector {
	strategy = SelectionStrategy(strings.ToLower(string(strategy)))
	if strategy == "" {
		strategy = StrategySmart
	}
	return &Selector{
		pool: accountPool, strategy: strategy, aliases: cloneAliases(aliases),
		runtime: make(map[string]*accountRuntime), affinity: make(map[string]affinityBinding), modelCooldown: make(map[string]time.Time),
		reservations: make(map[uint64]requestReservation), now: time.Now,
	}
}

func (s *Selector) SetStrategy(strategy SelectionStrategy) {
	strategy = SelectionStrategy(strings.ToLower(strings.TrimSpace(string(strategy))))
	if strategy == "" {
		strategy = StrategySmart
	}
	s.mu.Lock()
	s.strategy = strategy
	s.rr = 0
	for _, state := range s.runtime {
		state.currentWeight = 0
	}
	// Affinity is strategy-specific session memory; clear on switch so behavior is predictable.
	clear(s.affinity)
	s.mu.Unlock()
}

func (s *Selector) Strategy() SelectionStrategy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.strategy
}

func (s *Selector) Select(request SelectRequest) (SelectResult, error) {
	accounts := s.pool.List()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	models := []string{request.Model}
	if chain := s.aliases[request.Model]; len(chain) > 0 {
		models = chain
	}
	for _, model := range models {
		eligible := s.eligible(accounts, request.Provider, model, request.ExcludeIDs, now)
		if len(eligible) == 0 {
			continue
		}
		account := s.pick(eligible, accounts, request, now)
		state := s.state(account.ID)
		state.lastUsed = now
		s.nextReservation++
		reservationID := s.nextReservation
		s.reservations[reservationID] = requestReservation{accountID: account.ID}
		state.pendingRequests++
		return SelectResult{Account: account, ResolvedModel: model, ReservationID: reservationID}, nil
	}
	return SelectResult{}, ErrNoAccount
}

func (s *Selector) CommitRequest(reservationID uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	reservation, ok := s.reservations[reservationID]
	if !ok {
		return false
	}
	delete(s.reservations, reservationID)
	state := s.state(reservation.accountID)
	if state.pendingRequests > 0 {
		state.pendingRequests--
	}
	state.requestsMinute.add(s.now().UTC(), 1)
	return true
}

func (s *Selector) CancelRequest(reservationID uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	reservation, ok := s.reservations[reservationID]
	if !ok {
		return false
	}
	delete(s.reservations, reservationID)
	state := s.state(reservation.accountID)
	if state.pendingRequests > 0 {
		state.pendingRequests--
	}
	return true
}

func (s *Selector) ReportSuccess(accountID string, tokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	state := s.state(accountID)
	state.consecutiveErrors = 0
	state.consecutive429s = 0
	state.cooldownUntil = time.Time{}
	state.circuitUntil = time.Time{}
	state.lastUsed = now
	if tokens > 0 {
		state.tokensHour.add(now, int64(tokens))
	}
}

func (s *Selector) ReportRateLimit(accountID string) {
	s.reportRateLimit(accountID, "", 0)
}

// ReportRateLimitAfter is ReportRateLimit for callers holding the upstream's own answer
// to "how long". Zero means it did not say, and the ladder applies.
func (s *Selector) ReportRateLimitAfter(accountID string, retryAfter time.Duration) {
	s.reportRateLimit(accountID, "", retryAfter)
}

func (s *Selector) ReportModelRateLimit(accountID, model string) {
	s.reportRateLimit(accountID, model, 0)
}

// ReportModelRateLimitAfter is ReportModelRateLimit with the upstream's stated wait.
func (s *Selector) ReportModelRateLimitAfter(accountID, model string, retryAfter time.Duration) {
	s.reportRateLimit(accountID, model, retryAfter)
}

// Bounds on a wait the upstream asked for. The floor stops a zero or sub-second value
// from turning the cooldown off, and the ceiling stops one malformed header from parking
// an account for hours — which matters most when it is the only account there is.
const (
	minUpstreamRetryAfter = time.Second
	maxUpstreamRetryAfter = 15 * time.Minute
)

func (s *Selector) reportRateLimit(accountID, model string, retryAfter time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	state := s.state(accountID)
	state.consecutive429s++
	backoff := 30 * time.Second
	if state.consecutive429s == 2 {
		backoff = time.Minute
	} else if state.consecutive429s >= 3 {
		backoff = 2 * time.Minute
	}
	// The ladder is a guess about an upstream that has just told us the answer. Prefer
	// what it said, in either direction: waiting longer than asked idles an account that
	// is ready, and waiting less earns another 429 and pushes the guess further out.
	if retryAfter > 0 {
		backoff = min(max(retryAfter, minUpstreamRetryAfter), maxUpstreamRetryAfter)
	}
	if model != "" {
		s.modelCooldown[accountID+"\x00"+model] = now.Add(backoff)
		return
	}
	state.cooldownUntil = now.Add(backoff)
}

func (s *Selector) ReportError(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state(accountID)
	state.consecutiveErrors++
	if state.consecutiveErrors >= 5 {
		state.circuitUntil = s.now().UTC().Add(3 * time.Minute)
		state.consecutiveErrors = 0
	}
}

// eligible lists the accounts that may serve this provider/model right now.
//
// Every exclusion here is something the upstream imposed — disabled credentials, a 429
// cooldown, an exhausted quota, a configured rate limit — except one. The circuit breaker
// is this process's own guess that an account is unwell, and applying it like the rest
// meant five faults could take the last account out and leave the pool empty for three
// minutes, failing every request with "no eligible account". That is worse than the
// condition it protects against, and it is the live configuration: the codex provider is
// down to a single enabled account. So circuit-broken accounts are held back only while
// something else can serve.
func (s *Selector) eligible(accounts []Account, provider, model string, excluded map[string]struct{}, now time.Time) []Account {
	eligible := make([]Account, 0, len(accounts))
	var circuitBroken []Account
	for _, account := range accounts {
		if _, skip := excluded[account.ID]; skip {
			continue
		}
		state := s.state(account.ID)
		state.requestsMinute.prune(now.Add(-time.Minute))
		state.tokensHour.prune(now.Add(-time.Hour))
		if !account.Enabled || (provider != "" && account.Provider != provider) || !supportsModel(account.Models, model) {
			continue
		}
		if now.Before(state.cooldownUntil) {
			continue
		}
		if cooldownUntil := s.modelCooldown[account.ID+"\x00"+model]; now.Before(cooldownUntil) {
			continue
		}
		if modelQuota, known := account.ModelQuotas[model]; known {
			if modelQuota.Exhausted && (modelQuota.ResetAt.IsZero() || now.Before(modelQuota.ResetAt)) {
				continue
			}
		} else if account.QuotaExhausted && (account.QuotaResetAt.IsZero() || now.Before(account.QuotaResetAt)) {
			continue
		}
		if account.MaxRequestsPerMinute > 0 && state.requestsMinute.sum(now, time.Minute)+state.pendingRequests >= int64(account.MaxRequestsPerMinute) {
			continue
		}
		if account.MaxTokensPerHour > 0 && state.tokensHour.sum(now, time.Hour) >= int64(account.MaxTokensPerHour) {
			continue
		}
		// Last, so that an account failing any check above never reaches the fallback.
		if now.Before(state.circuitUntil) {
			circuitBroken = append(circuitBroken, account)
			continue
		}
		eligible = append(eligible, account)
	}
	if len(eligible) == 0 && len(circuitBroken) > 0 {
		slog.Warn("serving from a circuit-broken account because no other account can take the request",
			"provider", provider, "model", model, "accounts", len(circuitBroken))
		eligible = circuitBroken
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].ID < eligible[j].ID })
	return eligible
}

// pick chooses among the accounts that may serve this request. all is every account in
// the pool, eligible or not — sticky_soft needs it to tell an account that is merely
// unavailable this instant from one that is gone for good.
func (s *Selector) pick(accounts, all []Account, request SelectRequest, now time.Time) Account {
	switch s.strategy {
	case StrategyRoundRobin:
		account := accounts[s.rr%uint64(len(accounts))]
		s.rr++
		return account
	case StrategyWeighted:
		return s.pickWeighted(accounts)
	case StrategyLeastUsed:
		return s.pickLeast(accounts, now, false)
	case StrategyLeastUsedRPM:
		return s.pickLeast(accounts, now, true)
	case StrategySticky:
		key := affinityKey(request)
		if key != "" {
			hash := fnv.New64a()
			_, _ = hash.Write([]byte(key))
			return accounts[hash.Sum64()%uint64(len(accounts))]
		}
		return s.pickSmart(accounts, now)
	case StrategyStickySoft:
		return s.pickStickySoft(accounts, all, request, now)
	case StrategyFailover:
		best := accounts[0]
		for _, account := range accounts[1:] {
			if account.Weight > best.Weight {
				best = account
			}
		}
		return best
	default:
		return s.pickSmart(accounts, now)
	}
}

func (s *Selector) pickWeighted(accounts []Account) Account {
	total := 0
	best := accounts[0]
	for _, account := range accounts {
		weight := max(account.Weight, 1)
		state := s.state(account.ID)
		state.currentWeight += weight
		total += weight
		bestState := s.state(best.ID)
		if state.currentWeight > bestState.currentWeight {
			best = account
		}
	}
	s.state(best.ID).currentWeight -= total
	return best
}

func (s *Selector) pickLeast(accounts []Account, now time.Time, requests bool) Account {
	best := accounts[0]
	bestUsage := s.usage(best.ID, now, requests)
	for _, account := range accounts[1:] {
		usage := s.usage(account.ID, now, requests)
		if usage < bestUsage {
			best, bestUsage = account, usage
		}
	}
	return best
}

func (s *Selector) pickSmart(accounts []Account, now time.Time) Account {
	best := accounts[0]
	bestScore := s.smartScore(best, now)
	for _, account := range accounts[1:] {
		score := s.smartScore(account, now)
		if score > bestScore {
			best, bestScore = account, score
		}
	}
	return best
}

func (s *Selector) smartScore(account Account, now time.Time) float64 {
	state := s.state(account.ID)
	idle := 24 * time.Hour
	if !state.lastUsed.IsZero() {
		idle = now.Sub(state.lastUsed)
		if idle < 0 {
			idle = 0
		}
	}
	return float64(max(account.Weight, 1))*100 + account.QuotaRemainingPercent -
		float64(state.tokensHour.sum(now, time.Hour))*0.01 -
		float64(state.requestsMinute.sum(now, time.Minute)+state.pendingRequests)*5 -
		float64(state.consecutiveErrors)*50 + idle.Seconds()*0.1
}

func (s *Selector) usage(id string, now time.Time, requests bool) int64 {
	state := s.state(id)
	if requests {
		return state.requestsMinute.sum(now, time.Minute) + state.pendingRequests
	}
	return state.tokensHour.sum(now, time.Hour)
}

func (s *Selector) state(id string) *accountRuntime {
	state := s.runtime[id]
	if state == nil {
		state = &accountRuntime{}
		s.runtime[id] = state
	}
	return state
}

func (c *rollingCounter) add(at time.Time, value int64) {
	c.events = append(c.events, counterEvent{at: at, value: value})
}

func (c *rollingCounter) sum(now time.Time, window time.Duration) int64 {
	c.prune(now.Add(-window))
	var total int64
	for _, event := range c.events {
		total += event.value
	}
	return total
}

func (c *rollingCounter) prune(cutoff time.Time) {
	first := 0
	for first < len(c.events) && !c.events[first].at.After(cutoff) {
		first++
	}
	if first > 0 {
		c.events = append([]counterEvent(nil), c.events[first:]...)
	}
}

func affinityKey(request SelectRequest) string {
	key := strings.TrimSpace(request.ConversationID)
	if key != "" {
		return key
	}
	return strings.TrimSpace(request.FirstMessage)
}

func findAccount(accounts []Account, id string) (Account, bool) {
	for _, account := range accounts {
		if account.ID == id {
			return account, true
		}
	}
	return Account{}, false
}

// pickStickySoft keeps a session on one account while it stays healthy, then
// migrates affinity to a healthier peer under pressure. Hard ineligibility is
// handled by eligible(); this path only sees accounts already allowed to serve.
func (s *Selector) pickStickySoft(accounts, all []Account, request SelectRequest, now time.Time) Account {
	s.pruneAffinity(now)
	key := affinityKey(request)
	if key == "" {
		return s.pickLeastPressured(accounts, now)
	}
	if bound, ok := s.affinity[key]; ok {
		if preferred, found := findAccount(accounts, bound.accountID); found {
			// Recently rebound sessions stay pinned so a multi-call tool loop does not
			// soft-migrate mid-turn. Hard ineligibility still drops preferred via eligible().
			pinned := now.Sub(bound.boundAt) < stickySoftMigrateCooldown
			if pinned || !s.underPressure(preferred, now) {
				s.touchAffinity(key, preferred.ID, now, false)
				return preferred
			}
			// Soft pressure: migrate only when a clearly healthier peer exists.
			if next, ok := s.pickHealthierPeer(accounts, preferred, now); ok {
				s.touchAffinity(key, next.ID, now, true)
				return next
			}
			s.touchAffinity(key, preferred.ID, now, false)
			return preferred
		}
		// The bound account cannot serve this instant. When it still exists and is enabled
		// that is temporary — excluded after a fault on this very turn, in a 429 cooldown,
		// at its per-minute limit — and moving the binding would be permanent: the next
		// turn finds the new account bound and never comes back. A whole session's warm
		// prompt cache is then stranded on an account it will not use again, and the
		// upstream re-reads the entire history at full price on the account it moved to.
		// Serve this one turn elsewhere and leave the binding where it is.
		if preferred, exists := findAccount(all, bound.accountID); exists && preferred.Enabled {
			fallback := s.pickLeastPressured(accounts, now)
			s.touchAffinity(key, bound.accountID, now, false)
			return fallback
		}
	}
	chosen := s.pickLeastPressured(accounts, now)
	s.touchAffinity(key, chosen.ID, now, true)
	return chosen
}

func (s *Selector) touchAffinity(key, accountID string, now time.Time, rebind bool) {
	if key == "" || accountID == "" {
		return
	}
	bound, ok := s.affinity[key]
	if !ok || rebind {
		bound = affinityBinding{accountID: accountID, boundAt: now, lastUsedAt: now}
	} else {
		bound.accountID = accountID
		bound.lastUsedAt = now
	}
	s.affinity[key] = bound
}

func (s *Selector) pruneAffinity(now time.Time) {
	for key, bound := range s.affinity {
		if now.Sub(bound.lastUsedAt) > stickySoftAffinityTTL {
			delete(s.affinity, key)
		}
	}
	if len(s.affinity) <= stickySoftAffinityMaxEntries {
		return
	}
	// Drop oldest lastUsedAt entries until back under cap.
	type item struct {
		key string
		at  time.Time
	}
	items := make([]item, 0, len(s.affinity))
	for key, bound := range s.affinity {
		items = append(items, item{key: key, at: bound.lastUsedAt})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
	overflow := len(s.affinity) - stickySoftAffinityMaxEntries
	for i := 0; i < overflow; i++ {
		delete(s.affinity, items[i].key)
	}
}

func (s *Selector) underPressure(account Account, now time.Time) bool {
	state := s.state(account.ID)
	if !account.QuotaUpdatedAt.IsZero() && account.QuotaRemainingPercent < stickySoftQuotaPressure {
		return true
	}
	if account.MaxRequestsPerMinute > 0 {
		limit := float64(account.MaxRequestsPerMinute)
		if float64(state.requestsMinute.sum(now, time.Minute)) >= limit*stickySoftLimitPressureRatio {
			return true
		}
	}
	if account.MaxTokensPerHour > 0 {
		limit := float64(account.MaxTokensPerHour)
		if float64(state.tokensHour.sum(now, time.Hour)) >= limit*stickySoftLimitPressureRatio {
			return true
		}
	}
	return state.consecutiveErrors >= stickySoftErrorPressure
}

func (s *Selector) pickLeastPressured(accounts []Account, now time.Time) Account {
	calm := make([]Account, 0, len(accounts))
	for _, account := range accounts {
		if !s.underPressure(account, now) {
			calm = append(calm, account)
		}
	}
	if len(calm) > 0 {
		return s.pickSmart(calm, now)
	}
	return s.pickSmart(accounts, now)
}

// pickHealthierPeer returns a non-pressured account that scores meaningfully better
// than the preferred account. No match keeps the preferred sticky binding.
func (s *Selector) pickHealthierPeer(accounts []Account, preferred Account, now time.Time) (Account, bool) {
	best, found := Account{}, false
	bestScore := s.smartScore(preferred, now)
	// Require a clear improvement so tiny score noise does not thrash sessions.
	const minDelta = 25.0
	for _, account := range accounts {
		if account.ID == preferred.ID || s.underPressure(account, now) {
			continue
		}
		score := s.smartScore(account, now)
		if !found || score > bestScore {
			best, bestScore, found = account, score, true
		}
	}
	if !found || bestScore < s.smartScore(preferred, now)+minDelta {
		return Account{}, false
	}
	return best, true
}

func supportsModel(models []string, model string) bool {
	if model == "" || len(models) == 0 {
		return true
	}
	for _, supported := range models {
		if supported == model {
			return true
		}
	}
	return false
}

func cloneAliases(aliases map[string][]string) map[string][]string {
	result := make(map[string][]string, len(aliases))
	for alias, models := range aliases {
		result[alias] = append([]string(nil), models...)
	}
	return result
}
