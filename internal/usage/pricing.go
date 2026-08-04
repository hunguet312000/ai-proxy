package usage

import (
	"strings"
)

// ModelRates are $/1M tokens — aligned with 9router open-sse/providers/pricing.js.
type ModelRates struct {
	Input         float64
	Output        float64
	Cached        float64
	Reasoning     float64
	CacheCreation float64
}

// Default mid-tier fallback when no model match (9router returns null → 0 cost;
// we keep a conservative estimate so the UI never silently shows $0 on unknown models).
var defaultRates = ModelRates{Input: 2.0, Output: 10.0, Cached: 0.2, Reasoning: 10.0, CacheCreation: 2.0}

// Exact model ids (base name, no vendor prefix). First-class 9router MODEL_PRICING subset.
var exactRates = map[string]ModelRates{
	// Claude
	"claude-opus-4-8":           {5, 25, 0.50, 25, 6.25},
	"claude-opus-4-6":           {5, 25, 0.50, 25, 6.25},
	"claude-opus-4-5":           {5, 25, 0.50, 37.50, 5},
	"claude-sonnet-5":           {3, 15, 0.30, 15, 3.75},
	"claude-sonnet-4-6":         {3, 15, 0.30, 15, 3.75},
	"claude-sonnet-4-5":         {3, 15, 0.30, 15, 3.75},
	"claude-haiku-4-5":          {1, 5, 0.10, 5, 1.25},
	"claude-haiku-4-5-20251001": {1, 5, 0.10, 5, 1.25},
	"claude-fable-5":            {10, 50, 1.00, 50, 12.50},
	// GPT / Codex
	"gpt-5":               {1.25, 10, 0.625, 10, 1.25},
	"gpt-5-mini":          {0.25, 2, 0.125, 2, 0.25},
	"gpt-5-codex":         {1.25, 10, 0.625, 10, 1.25},
	"gpt-5.1":             {1.25, 10, 0.625, 10, 1.25},
	"gpt-5.1-codex":       {1.25, 10, 0.625, 10, 1.25},
	"gpt-5.1-codex-mini":  {1.50, 6, 0.75, 9, 1.50},
	"gpt-5.2":             {1.75, 14, 0.175, 14, 1.75},
	"gpt-5.2-codex":       {1.75, 14, 0.175, 14, 1.75},
	"gpt-5.3-codex":       {1.75, 14, 0.175, 14, 1.75},
	"gpt-5.3-codex-spark": {3.00, 12, 0.30, 12, 3.00},
	"gpt-5.4":             {1.75, 14, 0.175, 14, 1.75},
	"gpt-5.4-mini":        {0.40, 1.60, 0.04, 1.60, 0.40},
	"gpt-5.5":             {2.00, 12, 0.20, 12, 2.00},
	"gpt-5.6":             {2.50, 15, 0.25, 15, 2.50},
	"gpt-5.6-luna":        {1.00, 6, 0.10, 6, 1.00},
	"gpt-5.6-terra":       {2.50, 15, 0.25, 15, 2.50},
	"gpt-5.6-sol":         {5.00, 30, 0.50, 30, 5.00},
	// Grok
	"grok-code-fast-1":      {0.50, 2, 0.25, 3, 0.50},
	"grok-4":                {0.50, 2, 0.25, 3, 0.50},
	"grok-4.5":              {0.50, 2, 0.25, 3, 0.50},
	"grok-3":                {0.50, 2, 0.25, 3, 0.50},
	"grok-4-fast-reasoning": {0.50, 2, 0.25, 3, 0.50},
}

// Pattern rates — order matters, first match wins (9router PATTERN_PRICING).
var patternRates = []struct {
	pattern string
	rates   ModelRates
}{
	{"*-codex-xhigh", ModelRates{10, 40, 5, 60, 10}},
	{"*-codex-high", ModelRates{8, 32, 4, 48, 8}},
	{"*-codex-max", ModelRates{8, 32, 4, 48, 8}},
	{"*-codex-mini-*", ModelRates{1.50, 6, 0.75, 9, 1.50}},
	{"*-codex-mini", ModelRates{1.50, 6, 0.75, 9, 1.50}},
	{"*-codex-spark", ModelRates{3, 12, 0.30, 12, 3}},
	{"*-codex-low", ModelRates{1.75, 14, 0.175, 14, 1.75}},
	{"*-codex-none", ModelRates{1.75, 14, 0.175, 14, 1.75}},
	{"codex-*", ModelRates{1.75, 14, 0.175, 14, 1.75}},
	{"*-codex", ModelRates{1.75, 14, 0.175, 14, 1.75}},

	{"claude-opus-*", ModelRates{5, 25, 0.50, 25, 6.25}},
	{"claude-sonnet-*", ModelRates{3, 15, 0.30, 15, 3.75}},
	{"claude-haiku-*", ModelRates{1, 5, 0.10, 5, 1.25}},
	{"claude-*", ModelRates{3, 15, 0.30, 15, 3.75}},

	{"gpt-5.6-*", ModelRates{2.50, 15, 0.25, 15, 2.50}},
	{"gpt-5.5-*", ModelRates{2.00, 12, 0.20, 12, 2.00}},
	{"gpt-5.4-*", ModelRates{1.75, 14, 0.175, 14, 1.75}},
	{"gpt-5.3-*", ModelRates{1.75, 14, 0.175, 14, 1.75}},
	{"gpt-5.2-*", ModelRates{1.75, 14, 0.175, 14, 1.75}},
	{"gpt-5.1-*", ModelRates{1.25, 10, 0.625, 10, 1.25}},
	{"gpt-5-*", ModelRates{1.25, 10, 0.625, 10, 1.25}},
	{"gpt-5*", ModelRates{1.25, 10, 0.625, 10, 1.25}},
	{"gpt-4o-*", ModelRates{0.15, 0.60, 0.075, 0.90, 0.15}},
	{"gpt-4o", ModelRates{2.50, 10, 1.25, 15, 2.50}},
	{"gpt-4*", ModelRates{2.50, 10, 1.25, 15, 2.50}},

	{"o1-*", ModelRates{3, 12, 1.50, 18, 3}},
	{"o1", ModelRates{15, 60, 7.50, 90, 15}},
	{"o3-*", ModelRates{10, 40, 5, 60, 10}},
	{"o4-*", ModelRates{2, 8, 1, 12, 2}},

	{"grok-code-*", ModelRates{0.50, 2, 0.25, 3, 0.50}},
	{"grok-*", ModelRates{0.50, 2, 0.25, 3, 0.50}},
}

// NormalizeModelID strips vendor prefixes and review/effort suffixes for pricing lookup.
func NormalizeModelID(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	// strip vendor prefix: cx/gpt-5.6-sol → gpt-5.6-sol
	if i := strings.IndexByte(model, '/'); i >= 0 && i+1 < len(model) {
		model = model[i+1:]
	}
	// drop trailing " (high)" effort markers
	if i := strings.LastIndex(model, " ("); i >= 0 && strings.HasSuffix(model, ")") {
		model = strings.TrimSpace(model[:i])
	}
	// review virtual models bill as base
	if strings.HasSuffix(model, "-review") {
		model = strings.TrimSuffix(model, "-review")
	}
	return strings.ToLower(model)
}

// RatesForModel resolves rates via exact → pattern → default (9router getPricingForModel).
func RatesForModel(model string) ModelRates {
	base := NormalizeModelID(model)
	if base == "" {
		return defaultRates
	}
	if r, ok := exactRates[base]; ok {
		return r
	}
	// also try original lower without normalize side-effects beyond prefix
	if r, ok := exactRates[strings.ToLower(strings.TrimSpace(model))]; ok {
		return r
	}
	for _, p := range patternRates {
		if matchGlob(p.pattern, base) {
			return p.rates
		}
	}
	return defaultRates
}

// matchGlob supports 9router-style * wildcards, case-insensitive.
func matchGlob(pattern, model string) bool {
	pattern = strings.ToLower(pattern)
	model = strings.ToLower(model)
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == model
	}
	// must start with first literal (unless pattern starts with *)
	if parts[0] != "" && !strings.HasPrefix(model, parts[0]) {
		return false
	}
	pos := 0
	if parts[0] != "" {
		pos = len(parts[0])
	}
	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if part == "" {
			if i == len(parts)-1 {
				return true
			}
			continue
		}
		idx := strings.Index(model[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}
	// if pattern doesn't end with *, tail must be exact (pos already advanced through last part)
	if !strings.HasSuffix(pattern, "*") && pos != len(model) {
		return false
	}
	return true
}

// TokenUsage is the canonical cost input (9router canonicalizeUsage shape).
// PromptTokens is cache-inclusive: includes CachedTokens + CacheCreationTokens.
type TokenUsage struct {
	PromptTokens        int
	CompletionTokens    int
	CachedTokens        int // cache read (subset of prompt)
	CacheCreationTokens int
	ReasoningTokens     int
}

// CanonicalizeUsage mirrors 9router canonicalizeUsage.
// Claude path: prompt excludes cache → fold cache_read + cache_creation into prompt.
// OpenAI path: prompt already includes cached → pass through.
// claudeStyle=true when caller saw Anthropic-style separate cache fields without cached_tokens.
func CanonicalizeUsage(prompt, completion, cached, cacheCreation, reasoning int, claudeStyle bool) TokenUsage {
	if claudeStyle {
		// fold cache into prompt (cache-exclusive prompt from Anthropic)
		prompt = prompt + cached + cacheCreation
	}
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	if cached < 0 {
		cached = 0
	}
	if cacheCreation < 0 {
		cacheCreation = 0
	}
	if reasoning < 0 {
		reasoning = 0
	}
	// cached cannot exceed prompt
	if cached > prompt {
		cached = prompt
	}
	return TokenUsage{
		PromptTokens:        prompt,
		CompletionTokens:    completion,
		CachedTokens:        cached,
		CacheCreationTokens: cacheCreation,
		ReasoningTokens:     reasoning,
	}
}

// CalculateCostUSD mirrors 9router calculateCostFromTokens.
// prompt_tokens is cache-inclusive; non-cached = prompt - cached - cache_creation.
func CalculateCostUSD(model string, u TokenUsage) float64 {
	r := RatesForModel(model)
	nonCached := u.PromptTokens - u.CachedTokens - u.CacheCreationTokens
	if nonCached < 0 {
		nonCached = 0
	}
	cost := 0.0
	cost += float64(nonCached) * (r.Input / 1e6)
	if u.CachedTokens > 0 {
		cachedRate := r.Cached
		if cachedRate == 0 {
			cachedRate = r.Input
		}
		cost += float64(u.CachedTokens) * (cachedRate / 1e6)
	}
	cost += float64(u.CompletionTokens) * (r.Output / 1e6)
	if u.ReasoningTokens > 0 {
		rr := r.Reasoning
		if rr == 0 {
			rr = r.Output
		}
		cost += float64(u.ReasoningTokens) * (rr / 1e6)
	}
	if u.CacheCreationTokens > 0 {
		cr := r.CacheCreation
		if cr == 0 {
			cr = r.Input
		}
		cost += float64(u.CacheCreationTokens) * (cr / 1e6)
	}
	return cost
}

// EstimateCostUSD is the public entry used by RecordGatewayUsage.
// Assumes OpenAI-style tokens (prompt already includes cached).
func EstimateCostUSD(model string, prompt, completion, cached int) float64 {
	u := CanonicalizeUsage(prompt, completion, cached, 0, 0, false)
	return CalculateCostUSD(model, u)
}

// EstimateCostClaudeUSD for Anthropic-style tokens where prompt excludes cache read/write.
func EstimateCostClaudeUSD(model string, prompt, completion, cacheRead, cacheCreation, reasoning int) float64 {
	u := CanonicalizeUsage(prompt, completion, cacheRead, cacheCreation, reasoning, true)
	return CalculateCostUSD(model, u)
}
