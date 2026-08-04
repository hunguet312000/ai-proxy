package recommendation

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"literouter/internal/pool"
	"literouter/internal/storage"
)

const (
	defaultLimit = 5
	maxLimit     = 50
)

// Query controls the recommendation candidate set and ranking.
type Query struct {
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	Task       string `json:"task"`
	MinContext int    `json:"min_context,omitempty"`
	Limit      int    `json:"limit"`
}

// Item is a model recommendation based only on local catalog and pool state.
type Item struct {
	Model                 string    `json:"model"`
	Provider              string    `json:"provider"`
	Label                 string    `json:"label"`
	ContextWindow         int       `json:"context_window,omitempty"`
	Available             bool      `json:"available"`
	Score                 float64   `json:"score"`
	Reasons               []string  `json:"reasons"`
	AccountsAvailable     int       `json:"accounts_available"`
	QuotaRemainingPercent float64   `json:"quota_remaining_percent"`
	QuotaUpdatedAt        time.Time `json:"quota_updated_at,omitempty"`
}

// Response is the stable wire response for the recommendation endpoints.
type Response struct {
	Data        []Item    `json:"data"`
	Filters     Query     `json:"filters"`
	GeneratedAt time.Time `json:"generated_at"`
}

// CatalogLoader loads the current catalog without performing any upstream call.
type CatalogLoader func(context.Context, string) ([]storage.CatalogModel, error)

type Config struct {
	Pool            *pool.Pool
	Catalog         CatalogLoader
	GatewayModels   []string
	APIKeyProviders map[string]bool
	Now             func() time.Time
}

type Service struct {
	pool            *pool.Pool
	catalog         CatalogLoader
	gatewayModels   []string
	apiKeyProviders map[string]bool
	now             func() time.Time
}

func New(config Config) *Service {
	providers := make(map[string]bool, len(config.APIKeyProviders))
	for provider, enabled := range config.APIKeyProviders {
		providers[normalizeProvider(provider)] = enabled
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		pool:            config.Pool,
		catalog:         config.Catalog,
		gatewayModels:   append([]string(nil), config.GatewayModels...),
		apiKeyProviders: providers,
		now:             now,
	}
}

func (s *Service) Recommend(ctx context.Context, query Query) (Response, error) {
	query, err := normalizeQuery(query)
	if err != nil {
		return Response{}, err
	}
	if s == nil || s.catalog == nil {
		return Response{}, fmt.Errorf("recommendation catalog is unavailable")
	}

	catalog, err := s.catalog(ctx, "")
	if err != nil {
		return Response{}, fmt.Errorf("load recommendation catalog: %w", err)
	}
	candidates := mergeCandidates(catalog, s.gatewayModels)
	accounts := []pool.Account(nil)
	if s.pool != nil {
		accounts = s.pool.List()
	}

	items := make([]Item, 0, len(candidates))
	now := s.now().UTC()
	for _, candidate := range candidates {
		if !matchesQuery(candidate, query) {
			continue
		}
		item := s.evaluate(candidate, accounts, query, now)
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Available != items[j].Available {
			return items[i].Available
		}
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		if items[i].Provider != items[j].Provider {
			return items[i].Provider < items[j].Provider
		}
		return items[i].Model < items[j].Model
	})
	if len(items) > query.Limit {
		items = items[:query.Limit]
	}
	if items == nil {
		items = []Item{}
	}
	return Response{Data: items, Filters: query, GeneratedAt: now}, nil
}

func normalizeQuery(query Query) (Query, error) {
	query.Provider = normalizeProvider(query.Provider)
	query.Model = strings.TrimSpace(query.Model)
	query.Task = strings.ToLower(strings.TrimSpace(query.Task))
	if query.Task == "" {
		query.Task = "general"
	}
	if query.Limit == 0 {
		query.Limit = defaultLimit
	}
	if query.Limit < 1 || query.Limit > maxLimit {
		return Query{}, fmt.Errorf("limit must be between 1 and %d", maxLimit)
	}
	if query.MinContext < 0 {
		return Query{}, fmt.Errorf("min_context cannot be negative")
	}
	switch query.Task {
	case "general", "coding", "reasoning", "fast":
	default:
		return Query{}, fmt.Errorf("task must be general, coding, reasoning, or fast")
	}
	return query, nil
}

type candidate struct {
	provider string
	model    string
	label    string
	context  int
}

func mergeCandidates(catalog []storage.CatalogModel, gatewayModels []string) []candidate {
	seen := make(map[string]struct{}, len(catalog)+len(gatewayModels))
	candidates := make([]candidate, 0, len(catalog)+len(gatewayModels))
	add := func(provider, model, label string, contextWindow int) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		provider = normalizeProvider(provider)
		if provider == "" {
			provider = inferProvider(model)
		}
		key := strings.ToLower(provider + "\x00" + model)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		if label == "" {
			label = storage.PrettyModelLabel(model)
		}
		if contextWindow <= 0 {
			contextWindow = storage.DefaultContextWindow(model)
		}
		candidates = append(candidates, candidate{provider: provider, model: model, label: label, context: contextWindow})
	}
	for _, model := range catalog {
		add(model.Provider, model.ID, model.Label, model.ContextWindow)
	}
	for _, model := range gatewayModels {
		add(inferProvider(model), model, "", 0)
	}
	return candidates
}

func matchesQuery(candidate candidate, query Query) bool {
	if query.Provider != "" && candidate.provider != query.Provider {
		return false
	}
	if query.MinContext > 0 && (candidate.context <= 0 || candidate.context < query.MinContext) {
		return false
	}
	if query.Model != "" {
		want := strings.ToLower(strings.TrimSpace(query.Model))
		model := strings.ToLower(candidate.model)
		base := model
		if index := strings.IndexByte(base, '/'); index >= 0 {
			base = base[index+1:]
		}
		if want != model && want != base && !strings.HasPrefix(model, want) && !strings.HasPrefix(base, want) {
			return false
		}
	}
	return true
}

func (s *Service) evaluate(candidate candidate, accounts []pool.Account, query Query, now time.Time) Item {
	item := Item{
		Model: candidate.model, Provider: candidate.provider, Label: candidate.label,
		ContextWindow: candidate.context, Reasons: []string{},
		QuotaRemainingPercent: 100,
	}
	matchingAccounts := 0
	availableAccounts := 0
	bestQuota := 0.0
	quotaKnown := false
	var newestQuota time.Time
	quotaBlocked := false
	for _, account := range accounts {
		if normalizeProvider(account.Provider) != candidate.provider || !supportsModel(account.Models, candidate.model) {
			continue
		}
		matchingAccounts++
		quota, known := accountQuota(account, candidate.model, now)
		if known {
			quotaKnown = true
			if quota.percent > bestQuota {
				bestQuota = quota.percent
			}
			if quota.updatedAt.After(newestQuota) {
				newestQuota = quota.updatedAt
			}
		}
		if !account.Enabled {
			continue
		}
		if quota.blocked {
			quotaBlocked = true
			continue
		}
		availableAccounts++
	}
	item.AccountsAvailable = availableAccounts
	item.QuotaUpdatedAt = newestQuota
	if quotaKnown {
		item.QuotaRemainingPercent = clampPercent(bestQuota)
	}
	apiKeyAvailable := s.apiKeyProviders[candidate.provider]
	item.Available = availableAccounts > 0 || apiKeyAvailable

	if item.Available {
		item.Reasons = append(item.Reasons, "available")
	}
	if availableAccounts > 0 {
		if quotaKnown {
			item.Reasons = append(item.Reasons, "quota_available")
		} else {
			item.Reasons = append(item.Reasons, "active_account")
		}
	} else if quotaBlocked {
		item.Reasons = append(item.Reasons, "quota_exhausted")
	} else if matchingAccounts > 0 {
		item.Reasons = append(item.Reasons, "no_enabled_account")
	}
	if apiKeyAvailable {
		item.Reasons = append(item.Reasons, "api_key_configured")
	}
	if candidate.context > 0 {
		item.Reasons = append(item.Reasons, "context_known")
	} else {
		item.Reasons = append(item.Reasons, "context_unknown")
	}
	if taskMatches(candidate, query.Task) {
		item.Reasons = append(item.Reasons, query.Task+"_match")
	}

	item.Score = scoreCandidate(candidate, item, query.Task, quotaKnown)
	return item
}

type quotaState struct {
	percent   float64
	blocked   bool
	updatedAt time.Time
}

func accountQuota(account pool.Account, model string, now time.Time) (quotaState, bool) {
	if snapshot, ok := findModelQuota(account.ModelQuotas, model); ok {
		blocked := snapshot.Exhausted && (snapshot.ResetAt.IsZero() || now.Before(snapshot.ResetAt))
		return quotaState{percent: snapshot.RemainingPercent, blocked: blocked, updatedAt: snapshot.FetchedAt}, true
	}
	if account.QuotaUpdatedAt.IsZero() && !account.QuotaExhausted && account.QuotaRemainingPercent == 100 {
		return quotaState{}, false
	}
	blocked := account.QuotaExhausted && (account.QuotaResetAt.IsZero() || now.Before(account.QuotaResetAt))
	return quotaState{percent: account.QuotaRemainingPercent, blocked: blocked, updatedAt: account.QuotaUpdatedAt}, true
}

func findModelQuota(quotas map[string]pool.QuotaSnapshot, model string) (pool.QuotaSnapshot, bool) {
	if snapshot, ok := quotas[model]; ok {
		return snapshot, true
	}
	want := strings.ToLower(model)
	for key, snapshot := range quotas {
		if strings.EqualFold(key, want) {
			return snapshot, true
		}
	}
	return pool.QuotaSnapshot{}, false
}

func supportsModel(models []string, model string) bool {
	if len(models) == 0 {
		return true
	}
	for _, supported := range models {
		if strings.EqualFold(strings.TrimSpace(supported), model) {
			return true
		}
	}
	return false
}

func scoreCandidate(candidate candidate, item Item, task string, quotaKnown bool) float64 {
	score := 0.0
	if item.Available {
		score += 50
	}
	if item.AccountsAvailable > 0 {
		score += 10
	}
	if item.QuotaRemainingPercent > 0 && quotaKnown {
		score += item.QuotaRemainingPercent * 0.25
	}
	if candidate.context > 0 {
		score += 5
	}
	if taskMatches(candidate, task) {
		score += 15
	}
	if score > 100 {
		return 100
	}
	return score
}

func taskMatches(candidate candidate, task string) bool {
	value := strings.ToLower(candidate.provider + " " + candidate.model + " " + candidate.label)
	switch task {
	case "coding":
		return candidate.provider == "codex" || strings.Contains(value, "code") || strings.Contains(value, "codex")
	case "reasoning":
		return strings.Contains(value, "reason") || strings.Contains(value, "think") || strings.Contains(value, "opus") || strings.Contains(value, "o1") || strings.Contains(value, "o3") || strings.Contains(value, "o4")
	case "fast":
		return strings.Contains(value, "mini") || strings.Contains(value, "haiku") || strings.Contains(value, "flash") || strings.Contains(value, "fast") || strings.Contains(value, "spark")
	default:
		return false
	}
}

func inferProvider(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(model, "ag/"), strings.HasPrefix(model, "gemini"), strings.HasPrefix(model, "antigravity"):
		return "antigravity"
	case strings.HasPrefix(model, "claude"), strings.HasPrefix(model, "anthropic"):
		return "claude"
	case strings.HasPrefix(model, "grok"), strings.HasPrefix(model, "xai/"):
		return "xai"
	case strings.HasPrefix(model, "cx/"), strings.HasPrefix(model, "gpt"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"), strings.HasPrefix(model, "codex"):
		return "codex"
	default:
		return "codex"
	}
}

func normalizeProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "grok":
		return "xai"
	case "openai", "cx":
		return "codex"
	default:
		return provider
	}
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
