package contextguard

import (
	"errors"
	"fmt"
	"strings"

	"literouter/internal/provider"
)

var ErrBudgetExceeded = errors.New("context budget exceeded")

type Policy struct {
	SoftRatio       float64
	SummarizeRatio  float64
	HardRatio       float64
	KeepRecentTurns int
	ReserveTokens   int
	// Aggressive enables the lossy history stages — superseded-result collapse and
	// head/tail truncation of old bulky tool results — on top of the always-safe
	// thinking elision and exact dedup. Recent turns stay byte-for-byte either way.
	Aggressive bool
	// TruncateRatio is the share of the available budget above which old unique
	// tool results are truncated (aggressive mode only). It sits between SoftRatio
	// and SummarizeRatio so mechanical truncation gets a chance before the LLM
	// summary does.
	TruncateRatio float64
	// TruncateThresholdBytes is the size below which an old tool result is never
	// truncated; TruncateHeadBytes/TruncateTailBytes are what survives of a larger
	// one. Zero values fall back to the package defaults.
	TruncateThresholdBytes int
	TruncateHeadBytes      int
	TruncateTailBytes      int
	// BoundaryQuantum quantizes the old/recent boundary to a K-message grid so the
	// compacted prefix stays byte-identical for K consecutive appended messages —
	// without it every new message shifts the boundary and rewrites a prefix the
	// upstream had cached. Zero or one disables quantization.
	BoundaryQuantum int
	// EstimateScale is the learned ratio of estimated tokens to the upstream's real
	// count for this model. Budgets are token-space while estimates are not, so the
	// available budget is multiplied by this before any comparison. Zero means 1.0.
	EstimateScale float64
}

type Limits struct {
	Default int
	Models  map[string]int
}

type CheckResult struct {
	InputTokens      int
	SafeLimit        int
	Window           int
	EstimatedOverage bool
	Exceeded         bool
}

type Result struct {
	Request      provider.Request
	BeforeTokens int
	AfterTokens  int
	SavedTokens  int
	Window       int
	Compacted    bool
	NeedsSummary bool
}

func DefaultPolicy() Policy {
	return Policy{
		SoftRatio: 0.78, SummarizeRatio: 0.88, HardRatio: 0.96, KeepRecentTurns: 6, ReserveTokens: 2048,
		TruncateRatio: 0.82, TruncateThresholdBytes: defaultTruncateThreshold,
		TruncateHeadBytes: defaultTruncateHead, TruncateTailBytes: defaultTruncateTail,
		BoundaryQuantum: defaultBoundaryQuantum,
	}
}

// AggressivePolicy is base with the lossy history stages switched on.
func AggressivePolicy(base Policy) Policy {
	base.Aggressive = true
	if base.TruncateRatio <= 0 {
		base.TruncateRatio = 0.82
	}
	return base
}

func (limits Limits) Window(model string) int {
	if window := limits.Models[model]; window > 0 {
		return window
	}
	bestLength, bestWindow := 0, 0
	for prefix, window := range limits.Models {
		if window > 0 && modelPrefixMatch(model, prefix) && len(prefix) > bestLength {
			bestLength, bestWindow = len(prefix), window
		}
	}
	if bestWindow > 0 {
		return bestWindow
	}
	return HybridWindow(model, limits.Default)
}

// HybridWindow is the conservative fallback for models without catalog metadata.
// It avoids assuming oversized vendor windows and preserves quality by compacting later.
func HybridWindow(model string, fallback int) int {
	model = strings.ToLower(strings.TrimSpace(model))
	if fallback <= 0 {
		fallback = 128_000
	}
	switch {
	// Current Claude generations ship 1M windows. Assuming 200k here made LiteRouter
	// summarize and reject long coding sessions the model could serve, so prefer the
	// real window and let the upstream tokenizer make the authoritative rejection.
	case modelPrefixMatch(model, "claude-opus-4"), modelPrefixMatch(model, "claude-opus-5"),
		modelPrefixMatch(model, "claude-sonnet-4-6"), modelPrefixMatch(model, "claude-sonnet-5"),
		modelPrefixMatch(model, "claude-fable"), modelPrefixMatch(model, "claude-mythos"):
		return max(fallback, 1_000_000)
	case modelPrefixMatch(model, "claude"):
		return max(fallback, 200_000)
	case modelPrefixMatch(model, "gemini"), modelPrefixMatch(model, "ag"), modelPrefixMatch(model, "antigravity"):
		return max(fallback, 1_000_000)
	case modelPrefixMatch(model, "gpt-4.1"):
		return max(fallback, 1_000_000)
	case modelPrefixMatch(model, "gpt-5"), modelPrefixMatch(model, "cx/gpt-5"), modelPrefixMatch(model, "o1"), modelPrefixMatch(model, "o3"), modelPrefixMatch(model, "o4"):
		return max(fallback, 200_000)
	case modelPrefixMatch(model, "grok-4"), modelPrefixMatch(model, "xai/grok-4"), modelPrefixMatch(model, "grok-code"), modelPrefixMatch(model, "xai/grok-code"):
		return max(fallback, 256_000)
	default:
		return fallback
	}
}

func modelPrefixMatch(model, prefix string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if model == prefix {
		return true
	}
	if prefix == "" || !strings.HasPrefix(model, prefix) {
		return false
	}
	if strings.ContainsRune("-._/:@ (", rune(prefix[len(prefix)-1])) {
		return true
	}
	return len(model) > len(prefix) && strings.ContainsRune("-._/:@ (", rune(model[len(prefix)]))
}

func (policy Policy) Validate() error {
	if policy.SoftRatio <= 0 || policy.SoftRatio >= policy.SummarizeRatio || policy.SummarizeRatio >= policy.HardRatio || policy.HardRatio > 1 {
		return fmt.Errorf("context ratios must satisfy 0 < soft < summarize < hard <= 1")
	}
	if policy.KeepRecentTurns < 1 || policy.ReserveTokens < 0 {
		return fmt.Errorf("recent turns must be positive and reserve tokens cannot be negative")
	}
	if policy.Aggressive && policy.TruncateRatio > 0 &&
		(policy.TruncateRatio < policy.SoftRatio || policy.TruncateRatio >= policy.SummarizeRatio) {
		return fmt.Errorf("truncate ratio must satisfy soft <= truncate < summarize")
	}
	if head, tail, threshold := policy.truncateSizes(); head+tail >= threshold {
		return fmt.Errorf("truncate head+tail must stay under the truncate threshold")
	}
	if policy.BoundaryQuantum < 0 {
		return fmt.Errorf("boundary quantum cannot be negative")
	}
	if policy.EstimateScale != 0 && (policy.EstimateScale < 0.25 || policy.EstimateScale > 6) {
		return fmt.Errorf("estimate scale must be 0 or within [0.25, 6]")
	}
	return nil
}

// scaledAvailable converts a token-space budget into estimate space using the
// learned per-model ratio, so preflight decisions and the upstream tokenizer agree
// about how much room is left. Without it the byte heuristic ran 1.8x high on
// prose (compacting 40% early) and 0.9x low on dense text (missing real overflows).
func scaledAvailable(available int, policy Policy) int {
	if policy.EstimateScale <= 0 {
		return available
	}
	return int(float64(available) * policy.EstimateScale)
}

func Check(request provider.Request, limits Limits, policy Policy) (CheckResult, error) {
	if err := policy.Validate(); err != nil {
		return CheckResult{}, err
	}
	window := limits.Window(request.Model)
	inputTokens := EstimateRequest(request)
	safeLimit := scaledAvailable(window-outputReserve(request, policy, window)-safetyReserve(window), policy)
	// EstimateRequest is intentionally conservative and is not tokenizer-authoritative.
	// Crossing the policy threshold is telemetry only; upstream must make the hard
	// rejection with its exact tokenizer and actual model context window.
	estimatedOverage := safeLimit < 1 || float64(inputTokens) >= float64(safeLimit)*policy.HardRatio
	return CheckResult{
		InputTokens:      inputTokens,
		SafeLimit:        safeLimit,
		Window:           window,
		EstimatedOverage: estimatedOverage,
		Exceeded:         false,
	}, nil
}

func Prepare(request provider.Request, limits Limits, policy Policy) (Result, error) {
	if err := policy.Validate(); err != nil {
		return Result{}, err
	}
	result := Result{Request: cloneRequest(request), Window: limits.Window(request.Model)}
	result.BeforeTokens = EstimateRequest(result.Request)
	result.AfterTokens = result.BeforeTokens
	available := scaledAvailable(result.Window-outputReserve(request, policy, result.Window)-safetyReserve(result.Window), policy)
	if available < 1 {
		return result, ErrBudgetExceeded
	}
	if float64(result.BeforeTokens) < float64(available)*policy.SoftRatio {
		return result, nil
	}
	compact(&result.Request, policy, available, result.BeforeTokens)
	result.AfterTokens = EstimateRequest(result.Request)
	result.SavedTokens = max(result.BeforeTokens-result.AfterTokens, 0)
	result.Compacted = result.SavedTokens > 0
	result.NeedsSummary = float64(result.AfterTokens) >= float64(available)*policy.SummarizeRatio
	if float64(result.AfterTokens) >= float64(available)*policy.HardRatio && !result.NeedsSummary {
		return result, ErrBudgetExceeded
	}
	return result, nil
}

// HardBudget is the largest estimated input that still stays under the hard ratio.
// Callers use it to trim deterministically instead of failing the request.
func HardBudget(request provider.Request, limits Limits, policy Policy) int {
	window := limits.Window(request.Model)
	available := scaledAvailable(window-outputReserve(request, policy, window)-safetyReserve(window), policy)
	if available < 1 {
		return 0
	}
	return max(int(float64(available)*policy.HardRatio)-1, 0)
}

func ExceedsHardLimit(result Result, policy Policy) bool {
	available := scaledAvailable(result.Window-outputReserve(result.Request, policy, result.Window)-safetyReserve(result.Window), policy)
	return available < 1 || float64(result.AfterTokens) >= float64(available)*policy.HardRatio
}

// safetyReserve absorbs tokenizer-estimation error and provider envelope overhead.
// Five percent scales with modern 200k–1M windows instead of becoming negligible.
func safetyReserve(window int) int {
	if window <= 0 {
		return 0
	}
	return max(window/20, 64)
}

func outputReserve(request provider.Request, policy Policy, window int) int {
	output := request.MaxTokens
	if request.MaxCompletionTokens > output {
		output = request.MaxCompletionTokens
	}
	if output == 0 {
		// Small custom/test windows cannot reserve more output than input space.
		output = min(8_192, max(window/10, 1))
	}
	reserve := output + policy.ReserveTokens
	if window <= 0 {
		return reserve
	}
	// A caller's max_tokens is an ask, not a fact: the upstream caps output itself and
	// LiteRouter learns that cap separately (router.max_output_tokens). Letting the ask
	// consume the whole window left no input budget at all — available fell below 1, so
	// HardBudget returned 0 and TrimOldestTurns declined on the budget before it ever
	// looked at the history, hard-rejecting a request it could have fitted. Half the
	// window always stays with the input.
	return min(reserve, window/2)
}

func cloneRequest(request provider.Request) provider.Request {
	result := request
	result.System = append([]provider.Content(nil), request.System...)
	result.Tools = append([]provider.Tool(nil), request.Tools...)
	result.Messages = make([]provider.Message, len(request.Messages))
	for index, message := range request.Messages {
		result.Messages[index] = message
		result.Messages[index].Content = append([]provider.Content(nil), message.Content...)
	}
	return result
}
