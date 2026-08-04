package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// Plain billing exposes monthlyLimit/used for Grok Pro.
	// ?format=credits is a different shape (weekly period + on-demand/prepaid) and often omits monthly included credits.
	grokBillingURL = "https://cli-chat-proxy.grok.com/v1/billing"
	grokCreditsURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	grokUserURL    = "https://cli-chat-proxy.grok.com/v1/user?include=subscription"
)

type GrokFetcher struct {
	client     *http.Client
	billingURL string
	creditsURL string
	userURL    string
}

func NewGrokFetcher(client *http.Client) *GrokFetcher {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &GrokFetcher{
		client: client, billingURL: grokBillingURL, creditsURL: grokCreditsURL, userURL: grokUserURL,
	}
}

func (f *GrokFetcher) Provider() string { return "grok" }

func (f *GrokFetcher) Fetch(ctx context.Context, accessToken, accountID string) (Quota, error) {
	user, userErr := f.get(ctx, f.userURL, accessToken, accountID, "")
	billing, billErr := f.get(ctx, f.billingURL, accessToken, accountID, "")
	credits, _ := f.get(ctx, f.creditsURL, accessToken, accountID, "")
	if billErr != nil && userErr != nil && len(credits) == 0 {
		return Quota{}, fmt.Errorf("fetch Grok quota: billing: %v; user: %v", billErr, userErr)
	}
	if billing == nil {
		billing = []byte(`{}`)
	}
	if user == nil {
		user = []byte(`{}`)
	}
	if credits == nil {
		credits = []byte(`{}`)
	}
	quota, err := ParseGrokQuota(billing, user, credits)
	if err != nil {
		return Quota{}, err
	}
	if len(quota.Windows) == 0 {
		if billErr != nil {
			return Quota{}, billErr
		}
		return Quota{}, fmt.Errorf("grok quota response has no windows")
	}
	quota.FetchedAt = time.Now().UTC()
	return quota, nil
}

func (f *GrokFetcher) get(ctx context.Context, endpoint, token, accountID, email string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "grok-cli/0.2.93 grok-shell/0.2.93")
	request.Header.Set("x-xai-token-auth", "xai-grok-cli")
	request.Header.Set("x-grok-client-identifier", "grok-shell")
	request.Header.Set("x-grok-client-version", "0.2.93")
	request.Header.Set("x-grok-client-mode", "headless")
	if accountID != "" {
		request.Header.Set("x-userid", accountID)
	}
	if email != "" {
		request.Header.Set("x-email", email)
	}
	response, err := f.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch Grok quota: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch Grok quota: HTTP %d", response.StatusCode)
	}
	return body, nil
}

// ParseGrokQuota merges plain billing + optional credits billing + user subscription.
// billingJSON: /v1/billing (monthlyLimit/used)
// creditsJSON: /v1/billing?format=credits (weekly period, on-demand, prepaid)
func ParseGrokQuota(billingJSON, userJSON, creditsJSON []byte) (Quota, error) {
	billingCfg := grokConfig(billingJSON)
	creditsCfg := grokConfig(creditsJSON)

	var user map[string]any
	_ = json.Unmarshal(userJSON, &user)
	plan := extractGrokPlan(user)
	if plan == "" {
		plan = extractGrokPlan(map[string]any{"config": billingCfg})
	}
	if plan == "" {
		plan = extractGrokPlan(map[string]any{"config": creditsCfg})
	}
	if plan == "" {
		plan = "xAI"
	}
	quota := Quota{Provider: "grok", Plan: plan}

	// Monthly included credits from plain billing.
	monthlyReset := parseReset(firstValue(billingCfg, "billingPeriodEnd", "billing_period_end", "periodEnd"))
	if monthlyReset.IsZero() {
		monthlyReset = parseReset(firstValue(creditsCfg, "billingPeriodEnd", "billing_period_end", "periodEnd"))
	}
	monthly := unwrapNumber(firstValue(billingCfg, "monthlyLimit", "monthly_limit"))
	includedUsed := unwrapNumber(firstValue(billingCfg, "includedUsed", "included_used", "used", "totalUsed", "total_used"))
	if monthly <= 0 {
		// fallback to credits payload if present
		monthly = unwrapNumber(firstValue(creditsCfg, "monthlyLimit", "monthly_limit"))
		if includedUsed <= 0 {
			includedUsed = unwrapNumber(firstValue(creditsCfg, "includedUsed", "included_used", "used", "totalUsed", "total_used"))
		}
	}
	if monthly > 0 {
		quota.Windows = append(quota.Windows, absoluteWindow("monthly", includedUsed, monthly, monthlyReset))
	}

	// Weekly SuperGrok window from credits currentPeriod (matches grok.com "Giới hạn SuperGrok hàng tuần").
	// Live SuperGrok credits payload exposes percent usage, not absolute credits:
	//   creditUsagePercent + productUsage[{Api|GrokChat}.usagePercent]
	if period, ok := object(creditsCfg["currentPeriod"]); ok {
		end := parseReset(firstValue(period, "end"))
		periodType := strings.ToUpper(firstString(period, "type"))
		weeklyUsed := unwrapNumber(firstValue(creditsCfg, "weeklyUsed", "weekly_used", "periodUsed", "period_used"))
		weeklyTotal := unwrapNumber(firstValue(creditsCfg, "weeklyLimit", "weekly_limit", "periodLimit", "period_limit", "includedLimit", "included_limit"))
		// Some payloads nest usage under period.
		if weeklyTotal <= 0 {
			weeklyTotal = unwrapNumber(firstValue(period, "limit", "total", "cap", "amount"))
		}
		if weeklyUsed <= 0 {
			weeklyUsed = unwrapNumber(firstValue(period, "used", "spent", "consumed"))
		}
		usedPct, hasPct := grokWeeklyUsedPercent(creditsCfg)
		if strings.Contains(periodType, "WEEKLY") || !end.IsZero() || hasPct {
			if weeklyTotal > 0 {
				quota.Windows = append(quota.Windows, absoluteWindow("weekly", weeklyUsed, weeklyTotal, end))
			} else if isPaidGrokPlan(plan) || hasPct {
				// Primary weekly only — API/Chat split is useful for debugging but too noisy on cards.
				quota.Windows = append(quota.Windows, percentageWindow("weekly", usedPct, end))
			}
		}
	}

	// On-demand from either payload (credits is usually authoritative).
	cap := unwrapNumber(firstValue(creditsCfg, "onDemandCap", "on_demand_cap"))
	used := unwrapNumber(firstValue(creditsCfg, "onDemandUsed", "on_demand_used"))
	if cap <= 0 {
		cap = unwrapNumber(firstValue(billingCfg, "onDemandCap", "on_demand_cap"))
		used = unwrapNumber(firstValue(billingCfg, "onDemandUsed", "on_demand_used"))
	}
	if cap > 0 {
		quota.Windows = append(quota.Windows, absoluteWindow("on_demand", used, cap, monthlyReset))
	} else if !isPaidGrokPlan(plan) && cap == 0 && used > 0 {
		// 9router: free tier with zero cap and usage marks on-demand exhausted.
		quota.Windows = append(quota.Windows, absoluteWindow("on_demand", 1, 1, monthlyReset))
	}

	prepaid := unwrapNumber(firstValue(creditsCfg, "prepaidBalance", "prepaid_balance"))
	if prepaid <= 0 {
		prepaid = unwrapNumber(firstValue(billingCfg, "prepaidBalance", "prepaid_balance"))
	}
	if prepaid > 0 {
		quota.Windows = append(quota.Windows, Window{
			Key: "prepaid", Total: prepaid, Remaining: prepaid, RemainingPercent: 100,
		})
	}

	// Nested credits objects (9router walks credits/creditBalance/usage...).
	appendGrokCreditObjects(&quota, billingCfg, monthlyReset)
	appendGrokCreditObjects(&quota, creditsCfg, monthlyReset)

	quota.Windows = sortGrokWindows(quota.Windows)
	return quota, nil
}

func sortGrokWindows(windows []Window) []Window {
	order := map[string]int{
		"weekly": 1, "weekly_api": 2, "weekly_chat": 3, "session": 4, "monthly": 5, "on_demand": 6, "credits": 7, "prepaid": 8, "subscription": 9,
	}
	out := append([]Window(nil), windows...)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			oi, oj := order[out[i].Key], order[out[j].Key]
			if oi == 0 {
				oi = 50
			}
			if oj == 0 {
				oj = 50
			}
			if oj < oi || (oj == oi && out[j].Key < out[i].Key) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func grokConfig(raw []byte) map[string]any {
	var root map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &root) != nil {
		return map[string]any{}
	}
	if cfg, ok := object(root["config"]); ok {
		return cfg
	}
	return root
}

func isPaidGrokPlan(plan string) bool {
	p := strings.ToLower(strings.TrimSpace(plan))
	if p == "" || p == "xai" || p == "free" || p == "none" || p == "null" {
		return false
	}
	return true
}

func appendGrokCreditObjects(quota *Quota, cfg map[string]any, resetAt time.Time) {
	if cfg == nil {
		return
	}
	// Avoid duplicate monthly/on_demand keys.
	has := map[string]bool{}
	for _, w := range quota.Windows {
		has[w.Key] = true
	}
	for _, key := range []string{"credits", "creditBalance", "usage", "includedCredits", "subscriptionCredits"} {
		obj, ok := object(cfg[key])
		if !ok {
			continue
		}
		total := unwrapNumber(firstValue(obj, "total", "limit", "cap", "allocation", "amount"))
		used := unwrapNumber(firstValue(obj, "used", "spent", "consumed"))
		remaining := unwrapNumber(firstValue(obj, "remaining", "balance", "left"))
		winReset := parseReset(firstValue(obj, "resetAt", "resetsAt", "end"))
		if winReset.IsZero() {
			winReset = resetAt
		}
		if total > 0 {
			if used <= 0 && remaining > 0 {
				used = total - remaining
				if used < 0 {
					used = 0
				}
			}
			if !has["credits"] {
				quota.Windows = append(quota.Windows, absoluteWindow("credits", used, total, winReset))
				has["credits"] = true
			}
		} else if remaining > 0 && !has["credits"] {
			quota.Windows = append(quota.Windows, Window{
				Key: "credits", Total: remaining, Remaining: remaining, RemainingPercent: 100, ResetAt: winReset,
			})
			has["credits"] = true
		}
	}
}

// grokWeeklyUsedPercent reads SuperGrok weekly consumption from credits billing.
// Prefer creditUsagePercent; fall back to summing productUsage percentages.
func grokWeeklyUsedPercent(cfg map[string]any) (float64, bool) {
	if cfg == nil {
		return 0, false
	}
	if raw := firstValue(cfg, "creditUsagePercent", "credit_usage_percent", "usagePercent", "usage_percent"); raw != nil {
		return clampPercent(unwrapNumber(raw)), true
	}
	if total, ok := sumGrokProductUsage(cfg); ok {
		return total, true
	}
	return 0, false
}

func sumGrokProductUsage(cfg map[string]any) (float64, bool) {
	raw, ok := cfg["productUsage"]
	if !ok {
		raw, ok = cfg["product_usage"]
	}
	if !ok {
		return 0, false
	}
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return 0, false
	}
	var total float64
	found := false
	for _, item := range items {
		obj, ok := object(item)
		if !ok {
			continue
		}
		if raw := firstValue(obj, "usagePercent", "usage_percent", "percent", "usedPercent", "used_percent"); raw != nil {
			total += unwrapNumber(raw)
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return clampPercent(total), true
}

func grokProductUsageWindows(cfg map[string]any, resetAt time.Time) []Window {
	raw, ok := cfg["productUsage"]
	if !ok {
		raw, ok = cfg["product_usage"]
	}
	if !ok {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []Window
	for _, item := range items {
		obj, ok := object(item)
		if !ok {
			continue
		}
		product := strings.TrimSpace(firstString(obj, "product", "name", "id"))
		rawPct := firstValue(obj, "usagePercent", "usage_percent", "percent", "usedPercent", "used_percent")
		if product == "" || rawPct == nil {
			continue
		}
		key := grokProductWindowKey(product)
		if key == "" {
			continue
		}
		out = append(out, percentageWindow(key, unwrapNumber(rawPct), resetAt))
	}
	return out
}

func grokProductWindowKey(product string) string {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(product), "_", "")) {
	case "api":
		return "weekly_api"
	case "grokchat", "chat", "grok":
		return "weekly_chat"
	default:
		return ""
	}
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func unwrapNumber(value any) float64 {
	switch v := value.(type) {
	case nil:
		return 0
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	case string:
		var f float64
		fmt.Sscanf(v, "%f", &f)
		return f
	case map[string]any:
		if raw, ok := v["val"]; ok {
			return unwrapNumber(raw)
		}
		if raw, ok := v["value"]; ok {
			return unwrapNumber(raw)
		}
	}
	return 0
}

func absoluteWindow(key string, used, total float64, resetAt time.Time) Window {
	if used < 0 {
		used = 0
	}
	if total < 0 {
		total = 0
	}
	remaining := total - used
	if remaining < 0 {
		remaining = 0
	}
	percentage := 0.0
	if total > 0 {
		percentage = remaining / total * 100
	}
	return Window{
		Key: key, Used: used, Total: total, Remaining: remaining,
		RemainingPercent: percentage, ResetAt: resetAt, Exhausted: total > 0 && remaining <= 0,
	}
}

func extractGrokPlan(root map[string]any) string {
	if root == nil {
		return ""
	}
	candidates := []map[string]any{root}
	if user, ok := object(root["user"]); ok {
		candidates = append(candidates, user)
	}
	if sub, ok := object(root["subscription"]); ok {
		candidates = append(candidates, sub)
	}
	if cfg, ok := object(root["config"]); ok {
		candidates = append(candidates, cfg)
	}
	for _, node := range candidates {
		for _, key := range []string{"subscriptionTier", "subscription_tier", "tier", "plan", "planName", "plan_name"} {
			if v := firstString(node, key); v != "" {
				if strings.EqualFold(v, "user") || strings.EqualFold(v, "ok") {
					continue
				}
				return normalizeGrokPlan(v)
			}
		}
		if sub, ok := object(node["subscription"]); ok {
			if v := firstString(sub, "tier", "name", "plan", "subscriptionTier"); v != "" {
				return normalizeGrokPlan(v)
			}
			if cur, ok := object(sub["currentTier"]); ok {
				if v := firstString(cur, "name", "tier", "id"); v != "" {
					return normalizeGrokPlan(v)
				}
			}
		}
		if cur, ok := object(node["currentTier"]); ok {
			if v := firstString(cur, "name", "tier", "id"); v != "" {
				return normalizeGrokPlan(v)
			}
		}
	}
	return ""
}

func normalizeGrokPlan(v string) string {
	v = strings.TrimSpace(v)
	switch strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(v, "_", ""), "-", "")) {
	case "supergrok":
		return "supergrok"
	case "supergrokheavy":
		return "supergrok heavy"
	case "grokpro":
		return "grokpro"
	default:
		return v
	}
}
