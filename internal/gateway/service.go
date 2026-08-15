package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"

	"literouter/internal/cache"
	"literouter/internal/contextguard"
	"literouter/internal/provider"
	"literouter/internal/toolstore"
	"literouter/internal/translator"
)

var ErrProviderUnavailable = errors.New("provider is not configured")

type JSONClient interface {
	DoJSON(ctx context.Context, path string, requestBody, responseBody any) error
}

type OAuthInference interface {
	DoJSON(ctx context.Context, request translator.OpenAIRequest, conversationID string) (translator.OpenAIResponse, error)
	DoStream(ctx context.Context, request translator.OpenAIRequest, conversationID string) (io.ReadCloser, error)
	// SupportsAnthropicPassthrough reports whether the model can be served by an
	// Anthropic-native upstream, which lets the gateway skip translation entirely.
	SupportsAnthropicPassthrough(model string) bool
	DoAnthropicStream(ctx context.Context, payload []byte, model, conversationID, betas string) (io.ReadCloser, error)
}

type summaryClient struct{ service *Service }

func (client summaryClient) Summarize(ctx context.Context, input contextguard.SummaryInput) (string, error) {
	model := client.service.resolveSummaryModel(input.Model)
	const instruction = `Compress the older conversation into a loss-minimizing continuation context.
Preserve exactly: user intent, constraints, preferences, acceptance criteria, unresolved tasks, decisions and rationale, file paths, code symbols, APIs, commands, IDs, numbers, URLs, errors, failed attempts, test results, current state, next action, and tool findings.
Never invent or claim unfinished work completed. Distinguish verified facts from assumptions. Use dense structured bullets and exact strings when later work depends on them.`
	budget := client.service.summaryInputBudget(model, input.MaxTokens)
	batches, err := contextguard.SummaryBatches(input.Messages, budget)
	if err != nil {
		return "", err
	}
	if len(batches) == 0 {
		return "", fmt.Errorf("summarizer received no messages")
	}
	summaries, err := client.summarizeBatches(ctx, model, instruction, batches, input.MaxTokens)
	if err != nil {
		return "", err
	}
	for len(summaries) > 1 {
		messages := make([]provider.Message, len(summaries))
		for index, summary := range summaries {
			messages[index] = provider.Message{Role: "user", Content: []provider.Content{{Type: "text", Text: summary}}}
		}
		reductionBatches, err := contextguard.SummaryBatches(messages, budget)
		if err != nil {
			return "", err
		}
		next, err := client.summarizeBatches(ctx, model,
			instruction+" Merge these partial summaries without dropping unique facts.", reductionBatches, input.MaxTokens)
		if err != nil {
			return "", err
		}
		if len(next) >= len(summaries) {
			return strings.Join(next, "\n"), nil
		}
		summaries = next
	}
	return summaries[0], nil
}

// maxConcurrentSummaryBatches bounds how many summarization calls are in flight at once.
// Small, because they all land on the same account: the point is to stop the map phase
// from costing the sum of its batches, not to saturate the upstream.
const maxConcurrentSummaryBatches = 3

// summarizeBatches compresses every batch and returns the results in batch order.
//
// The batches are independent by construction, and running them one after another meant
// the whole map phase had to finish inside a single summaryTimeout — so a backlog needing
// more than one batch spent the timeout and got trimmed anyway. Concurrently, the phase
// costs about what its slowest batch costs.
func (client summaryClient) summarizeBatches(ctx context.Context, model, instruction string,
	batches [][]provider.Message, maxTokens int) ([]string, error) {
	texts := make([]string, len(batches))
	failures := make([]error, len(batches))
	slots := make(chan struct{}, min(len(batches), maxConcurrentSummaryBatches))
	var pending sync.WaitGroup
	for index, batch := range batches {
		pending.Add(1)
		go func() {
			defer pending.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			texts[index], failures[index] = client.summarizeBatch(ctx, model, instruction, batch, maxTokens)
		}()
	}
	pending.Wait()
	// One failed batch means the summary would silently lose that slice of the
	// conversation, so the whole attempt fails and the caller falls back to trimming.
	if err := errors.Join(failures...); err != nil {
		return nil, err
	}
	return texts, nil
}

// summaryEffort is the reasoning effort every summarization call is sent at.
//
// It has to be stated. A summary request carries no Effort of its own, and the Codex
// payload builder defaults an absent effort to "high", so each batch was a
// quarter-million-token call to a reasoning model at full effort — which is where the
// flat 60s on time-to-first-token came from, and why the gate below gave up on
// summarizing at all rather than pay it. Compressing a transcript is not a reasoning
// task: the instruction says what to preserve, and the model has only to rewrite what
// is in front of it.
const summaryEffort = "low"

func (client summaryClient) summarizeBatch(ctx context.Context, model, instruction string, messages []provider.Message, maxTokens int) (string, error) {
	temperature := 0.0
	request := provider.Request{
		Model: model, MaxTokens: maxTokens, Temperature: &temperature, Effort: summaryEffort,
		System: []provider.Content{{Type: "text", Text: instruction}}, Messages: messages,
	}
	upstreamRequest, err := translator.ToOpenAIRequest(request)
	if err != nil {
		return "", err
	}
	var lastErr error
	for _, candidate := range client.service.modelChain(model) {
		upstreamRequest.Model = candidate
		var response translator.OpenAIResponse
		if client.service.oauthInference != nil {
			response, err = client.service.oauthInference.DoJSON(ctx, upstreamRequest, conversationID(ctx))
			if err == nil {
				return summaryResponseText(response)
			}
			lastErr = err
		}
		upstream := client.service.clientForModel(candidate)
		if upstream == nil {
			continue
		}
		upstreamRequest.Model = upstreamModel(candidate)
		if err = upstream.DoJSON(ctx, "/chat/completions", upstreamRequest, &response); err != nil {
			lastErr = err
			if retryableProviderError(err) {
				continue
			}
			return "", err
		}
		return summaryResponseText(response)
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", ErrProviderUnavailable
}

func summaryResponseText(response translator.OpenAIResponse) (string, error) {
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("summarizer returned no choices")
	}
	switch content := response.Choices[0].Message.Content.(type) {
	case string:
		if strings.TrimSpace(content) != "" {
			return content, nil
		}
	case []translator.OpenAIContentPart:
		var text strings.Builder
		for _, part := range content {
			text.WriteString(part.Text)
		}
		if strings.TrimSpace(text.String()) != "" {
			return text.String(), nil
		}
	}
	return "", fmt.Errorf("summarizer returned empty content")
}

func (s *Service) summaryInputBudget(model string, maxTokens int) int {
	window := s.resolveContextWindow(context.Background(), model)
	if window <= 0 {
		window = s.contextLimits.Window(model)
	}
	// The output reserve is a floor of 8k so a real summary has room to be written, but a
	// floor has to stay smaller than the thing it is carved out of: against a 12k window
	// it took two thirds of the budget, leaving batches so small the backlog shattered
	// into dozens of them. Cap it at an eighth of the window so small models degrade
	// proportionally instead of collapsing. Nothing changes above ~64k, which is every
	// model actually in use here.
	reserve := max(maxTokens, min(8_000, window/8))
	return max(window-reserve-max(window/20, 2_048), 1_024)
}

type Service struct {
	openAI          JSONClient
	xai             JSONClient
	openAIStream    StreamClient
	xaiStream       StreamClient
	oauthInference  OAuthInference
	responses       *cache.ResponseCache
	compressionMode cache.CompressionMode
	promptMinBytes  int
	xaiPromptCache  bool
	models          []string
	aliases         map[string][]string
	planModel       atomic.Pointer[string]
	compactModel    atomic.Pointer[string]
	fallbackModel   atomic.Pointer[string]
	longContext     longContextPointer
	imageRoute      atomic.Pointer[imageRoute]
	// buildImagePrompt, when true, transcribes images to text (via the vision model)
	// for text-only serving models instead of rerouting or stripping them.
	buildImagePrompt atomic.Pointer[bool]
	// transcriptionCache remembers vision-model text for repeated images.
	transcriptionCache *transcriptionCache
	transcriptionMu    sync.Mutex
	outputLimits       map[string]int
	learnedLimits      map[string]int
	learnedWindows     map[string]int
	observedWindows    map[string]int
	tokenScales        map[string]tokenScale
	warnedInflated     map[string]bool
	// modelEfforts overrides the reasoning effort per model. Held as a pointer so a
	// catalog edit is visible to in-flight requests without locking the hot path.
	modelEfforts    atomic.Pointer[map[string]string]
	learnedMu       sync.RWMutex
	onOutputLimit   func(model string, limit int)
	onContextWindow func(model string, window int)
	onCalibration   func(TokenCalibration)
	// contextMode is the runtime state of the proxy pipeline: off, safe, or
	// aggressive. Atomic so the dashboard can flip it without a restart.
	contextMode      atomic.Pointer[string]
	summarizeMode    atomic.Pointer[string]
	contextGuard     bool
	contextLimits    contextguard.Limits
	customProviders  *CustomProviderRegistry
	touchCustomKey   func(keyID string)
	contextWindow    func(context.Context, string) (int, error)
	contextPolicy    contextguard.Policy
	summarizer       contextguard.Summarizer
	summaryCache     *contextguard.SummaryCache
	summaryModel     string
	summaryMaxTokens int
	summaryTimeout   time.Duration
	onUsage          func(UsageEvent)
	// toolStore holds full tool-result bodies elided by aggressive truncation, keyed
	// by content hash so a truncated marker can point at them. Served through
	// GET /ref/:id; nil disables the endpoint and keeps truncation hintless.
	toolStore *toolstore.Store
}

// SetToolStore wires the reference store that truncation captures into, and that
// GET /ref/:id serves from. Reconfigurable at runtime; a nil store disables capture
// and the endpoint.
func (s *Service) SetToolStore(store *toolstore.Store) {
	if s != nil {
		s.toolStore = store
	}
}

// ToolStore returns the wired reference store, or nil when the feature is disabled.
func (s *Service) ToolStore() *toolstore.Store {
	if s == nil {
		return nil
	}
	return s.toolStore
}

// refHandler serves the full body of an elided tool result by content hash. The
// marker points the model here ("or fetch the full output"), so a truncated block can
// be recovered without re-running the tool.
func (s *Service) refHandler(c echo.Context) error {
	if s.toolStore == nil {
		return echo.NewHTTPError(http.StatusNotFound, "tool reference store is disabled")
	}
	body, ok := s.toolStore.Get(c.Param("id"))
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "tool reference not found")
	}
	// text/plain, not application/octet-stream: the consumer is the model reading the
	// body back into context, and tool output is text.
	c.Response().Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, err := c.Response().Write([]byte(body))
	return err
}

type Options struct {
	OpenAI          JSONClient
	XAI             JSONClient
	OpenAIStream    StreamClient
	XAIStream       StreamClient
	OAuthInference  OAuthInference
	ResponseCache   *cache.ResponseCache
	CompressionMode cache.CompressionMode
	PromptMinBytes  int
	XAIPromptCache  bool
	Models          []string
	Aliases         map[string][]string
	// PlanModel overrides the requested model on /v1/messages turns taken while
	// Claude Code's plan mode is active. Empty disables the override entirely,
	// including the transcript scan it needs.
	PlanModel string
	// CompactModel serves detected Claude Code compact/auto-compact requests, at
	// compactEffort. Empty disables the detection and leaves compaction on the
	// session model.
	CompactModel string
	// FallbackModel serves a turn whose every other candidate failed — most often a
	// model whose provider has no usable account, which would otherwise be reported
	// as a 502. Empty disables it. See withFallbackModel.
	FallbackModel string
	// LongContextModel serves a /v1/messages turn whose prompt is too large a share of
	// the window belonging to the model that would otherwise take it. Empty disables it.
	LongContextModel string
	// LongContextPercent is that share, 1..99. Zero means defaultLongContextPercent.
	LongContextPercent int
	// ImageModel serves a turn carrying an image whose routed model was declared unable to
	// read one. Empty makes such a turn a clear refusal instead.
	ImageModel string
	// TextOnlyModels names the models that cannot read images, by exact id or prefix. It
	// has to be named because no vendor exposes it reliably and no model id carries it.
	TextOnlyModels []string
	// BuildImagePrompt transcribes images to text via ImageModel for text-only serving
	// models instead of rerouting or stripping them.
	BuildImagePrompt bool
	// MaxOutputTokens caps max_tokens per model, keyed by exact id or model prefix.
	// Explicit configuration: it wins over anything the gateway learns at runtime.
	MaxOutputTokens map[string]int
	// LearnedOutputTokens seeds the runtime cache from whatever previous runs observed,
	// so a restart does not re-pay the one rejection it took to discover each cap.
	LearnedOutputTokens map[string]int
	// OnOutputLimit is called when a new cap is learned from an upstream rejection, so
	// it can be persisted. It runs on the request path; keep it cheap and non-blocking.
	OnOutputLimit func(model string, limit int)
	// OnContextWindow is called when an upstream rejection reveals a model's real
	// context window, so the catalog can be corrected. Same contract as OnOutputLimit:
	// it runs on the request path and must not block.
	//
	// A ceiling learned this way needs no seeding counterpart: it is persisted into the
	// same catalog column ContextWindow already reads, so the next boot picks it up as
	// ordinary catalog data. The floor is the half that does — see ObservedPrompts.
	OnContextWindow func(model string, window int)
	// ObservedPrompts seeds, per model, the largest prompt the upstream counted itself
	// and answered. It is a floor under that model's window: resolveContextWindow raises
	// a belief to meet it and never lowers one, so a wrong entry cannot strangle a model.
	//
	// Seeding this is what breaks a trap the runtime path cannot escape on its own. The
	// floor only rises when a served prompt exceeds the current belief, but the guard
	// compacts every request down to fit that belief before sending it — so once the
	// belief is too low, no prompt large enough to correct it is ever sent again, and the
	// mistake becomes permanent. Measured here: 845 turns had been served with
	// upstream-counted prompts above the catalogued 256,000 for cx/gpt-5.6-*, the largest
	// 372,860, and the catalogue still read 256,000 because every one of those turns
	// predated the guard. The evidence was already in usage_events; nothing was reading it.
	ObservedPrompts map[string]int
	// ToolStore wires the reference store that aggressive truncation captures elided
	// tool-result bodies into, and that GET /ref/:id serves back. Nil disables the
	// feature (truncation keeps its re-run hint only).
	ToolStore *toolstore.Store
	// LearnedCalibrations seeds the per-model tokenizer scales measured by previous
	// runs, so budget math starts from evidence instead of the conventional guesses
	// and re-earns nothing after a restart.
	LearnedCalibrations []TokenCalibration
	// OnCalibration is called when a model's measured scale changes materially (or
	// on the periodic heartbeat that carries the confidence counters), so it can be
	// persisted. Runs on the request path; keep it cheap and non-blocking.
	OnCalibration  func(TokenCalibration)
	ContextEnabled bool
	// ContextMode refines ContextEnabled: "safe" keeps the lossless pipeline,
	// "aggressive" adds superseded-result collapse and old-tool-output truncation.
	// Ignored when ContextEnabled is false. Empty means safe.
	ContextMode string
	// SummarizeMode selects llm or trim for reclaiming room. Empty means llm.
	SummarizeMode string
	ContextGuard  bool
	ContextLimits contextguard.Limits
	// CustomProviders resolves user-registered upstreams by model prefix.
	CustomProviders *CustomProviderRegistry
	// TouchCustomKey records that a key served a request, for the UI's last-used
	// column. It is a callback so the gateway keeps no storage dependency.
	TouchCustomKey   func(keyID string)
	ContextWindow    func(context.Context, string) (int, error)
	ContextPolicy    contextguard.Policy
	Summarizer       contextguard.Summarizer
	SummaryCache     *contextguard.SummaryCache
	SummaryModel     string
	SummaryMaxTokens int
	SummaryTimeout   time.Duration
	OnUsage          func(UsageEvent)
}

func New(options Options) *Service {
	service := &Service{
		openAI: options.OpenAI, xai: options.XAI, openAIStream: options.OpenAIStream, xaiStream: options.XAIStream,
		oauthInference: options.OAuthInference, responses: options.ResponseCache,
		compressionMode: options.CompressionMode, promptMinBytes: options.PromptMinBytes, xaiPromptCache: options.XAIPromptCache,
		models: append([]string(nil), options.Models...), aliases: cloneAliases(options.Aliases),
		outputLimits:    cloneOutputLimits(options.MaxOutputTokens),
		learnedLimits:   cloneOutputLimits(options.LearnedOutputTokens),
		onOutputLimit:   options.OnOutputLimit,
		onContextWindow: options.OnContextWindow,
		onCalibration:   options.OnCalibration,
		contextGuard:    options.ContextGuard, contextLimits: options.ContextLimits, contextWindow: options.ContextWindow, contextPolicy: options.ContextPolicy,
		customProviders: options.CustomProviders, touchCustomKey: options.TouchCustomKey,
		summarizer: options.Summarizer, summaryCache: options.SummaryCache, summaryModel: options.SummaryModel,
		summaryMaxTokens: options.SummaryMaxTokens, summaryTimeout: options.SummaryTimeout,
		onUsage:   options.OnUsage,
		toolStore: options.ToolStore,
	}
	service.SetPlanModel(options.PlanModel)
	service.SetCompactModel(options.CompactModel)
	service.SetFallbackModel(options.FallbackModel)
	service.SetLongContext(options.LongContextModel, options.LongContextPercent)
	service.SetImageRoute(options.ImageModel, options.TextOnlyModels)
	service.SetBuildImagePrompt(options.BuildImagePrompt)
	service.seedTokenScales(options.LearnedCalibrations)
	service.seedObservedPrompts(options.ObservedPrompts)
	mode := ContextModeOff
	if options.ContextEnabled {
		mode = strings.ToLower(strings.TrimSpace(options.ContextMode))
		if mode != ContextModeAggressive {
			mode = ContextModeSafe
		}
	}
	service.contextMode.Store(&mode)
	if err := service.SetSummarizeMode(options.SummarizeMode); err != nil {
		slog.Warn("ignoring invalid summarize mode", "mode", options.SummarizeMode, "error", err)
		_ = service.SetSummarizeMode("")
	}
	// Summarization machinery is initialised regardless of the boot mode: the
	// dashboard can switch the pipeline on at runtime, and a nil summarizer at
	// that moment would silently skip straight to trimming.
	if service.summarizer == nil {
		service.summarizer = summaryClient{service: service}
	}
	if service.summaryCache == nil {
		service.summaryCache = contextguard.NewSummaryCache(128)
	}
	if service.summaryMaxTokens <= 0 {
		service.summaryMaxTokens = 1200
	}
	if service.summaryTimeout <= 0 {
		// Summarization runs inline in the request path against a live upstream.
		// 20s routinely expired on large coding contexts and turned every long
		// session into a stall followed by a rejection.
		service.summaryTimeout = 60 * time.Second
	}
	if service.ContextMode() == ContextModeAggressive && service.compressionMode == cache.CompressionAggressive {
		slog.Warn("both cache compression and the context pipeline are aggressive; the cache pass also mutates recent turns and busts the upstream prompt cache — prefer LITEROUTER_CACHE_COMPRESSION_MODE=safe")
	}
	return service
}

// Context pipeline modes. Off leaves requests untouched (the guard may still
// warn); safe is the lossless compact/summarize/trim pipeline; aggressive adds
// superseded-result collapse and old-tool-output truncation.
const (
	ContextModeOff        = "off"
	ContextModeSafe       = "safe"
	ContextModeAggressive = "aggressive"
)

// How the pipeline reclaims room once the cheap stages are not enough.
//
// llm sends the older backlog to a model and replaces it with the summary: it keeps the
// arc of the conversation, and it costs an upstream call — measured at 15-35s on a
// six-figure-token backlog, which is most of the latency of a large turn.
//
// trim drops whole turns from the middle instead, keeping the opening turn and the recent
// ones (see contextguard.TrimOldestTurns). Milliseconds rather than tens of seconds, at
// the price of the dropped middle; the model is told with a trim notice so it asks instead
// of inventing the gap. Compaction requests always take this path regardless — see
// isCompactTurn.
const (
	SummarizeModeLLM  = "llm"
	SummarizeModeTrim = "trim"
)

// SummarizeMode reports how the pipeline reclaims room, as currently in force.
func (s *Service) SummarizeMode() string {
	if value := s.summarizeMode.Load(); value != nil {
		return *value
	}
	return SummarizeModeLLM
}

// SetSummarizeMode changes it on a running gateway. Empty restores the default.
func (s *Service) SetSummarizeMode(mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = SummarizeModeLLM
	}
	switch mode {
	case SummarizeModeLLM, SummarizeModeTrim:
		s.summarizeMode.Store(&mode)
		return nil
	default:
		return fmt.Errorf("summarize mode must be llm or trim")
	}
}

// ContextMode reports the pipeline mode currently in force.
func (s *Service) ContextMode() string {
	if value := s.contextMode.Load(); value != nil {
		return *value
	}
	return ContextModeOff
}

// SetContextMode changes the pipeline mode on a running gateway.
func (s *Service) SetContextMode(mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case ContextModeOff, ContextModeSafe, ContextModeAggressive:
		s.contextMode.Store(&mode)
		return nil
	default:
		return fmt.Errorf("context mode must be off, safe, or aggressive")
	}
}

// contextPrepEnabled reports whether requests may be mutated to fit their window.
func (s *Service) contextPrepEnabled() bool {
	return s.ContextMode() != ContextModeOff
}

// activeContextPolicy resolves the policy for the mode in force. A zero-valued
// configured policy — the guard switched off in config — falls back to the
// defaults rather than silently disabling every budget.
func (s *Service) activeContextPolicy() contextguard.Policy {
	policy := s.contextPolicy
	if policy.Validate() != nil {
		policy = contextguard.DefaultPolicy()
	}
	if s.ContextMode() == ContextModeAggressive {
		policy = contextguard.AggressivePolicy(policy)
	}
	// Aggressive truncation gets a reference store to capture elided bodies into,
	// so a cut is retrievable instead of gone. Only wired when a store exists.
	if store := s.toolStore; store != nil {
		policy.StoreResult = store.Put
	}
	// Plan-mode markers are protected from trimming: a turn that contains the "exited
	// plan mode" reminder must survive a trim, or the gateway would never learn the
	// session left plan mode and would keep routing every turn to the plan model.
	policy.KeepContaining = append(policy.KeepContaining, planEnteredMarker, planActiveMarker, planExitedMarker)
	return policy
}

func (s *Service) Chat(ctx context.Context, request translator.OpenAIRequest) (translator.OpenAIResponse, error) {
	return s.chat(ctx, request, "/v1/chat/completions")
}

// ReplaceModelEfforts installs the per-model effort overrides from the catalog.
func (s *Service) ReplaceModelEfforts(efforts map[string]string) {
	clone := make(map[string]string, len(efforts))
	for model, effort := range efforts {
		clone[model] = effort
	}
	s.modelEfforts.Store(&clone)
}

// effortFor returns the override for a model, or empty to leave the request alone.
//
// The lookup is on the model actually being called, not the one the client asked for:
// a fallback candidate is a different model with its own cost, and inheriting the
// original's effort would spend a reasoning budget the operator set for something else.
func (s *Service) effortFor(model string) string {
	efforts := s.modelEfforts.Load()
	if efforts == nil {
		return ""
	}
	return (*efforts)[model]
}

func (s *Service) chat(ctx context.Context, request translator.OpenAIRequest, endpoint string) (translator.OpenAIResponse, error) {
	if request.Model == "" || len(request.Messages) == 0 {
		return translator.OpenAIResponse{}, fmt.Errorf("model and messages are required")
	}
	if request.Stream {
		return translator.OpenAIResponse{}, fmt.Errorf("streaming requires the streaming gateway")
	}
	ctx = withPromptCacheSeed(ctx, request)
	cacheEligible := s.responses != nil && s.cacheableOpenAIRequest(request)
	var response translator.OpenAIResponse
	var lastErr error
	chain := s.modelChain(request.Model)
	// The candidate that actually served, which is not always the one the client asked
	// for: the chain can hand the turn to an alias or to the router's fallback model, and
	// either can belong to a different provider than the requested id does.
	served := request.Model
	// The effort that candidate was actually sent at. Nothing on the response can be used
	// to derive it — it comes from the route, the per-model override, or the client's own
	// ask, and only this loop knows which one won — so it is captured as the turn is won.
	sentEffort := request.Effort
	// Indexed rather than ranged so an output-cap rejection can rewind onto the same
	// candidate, exactly as the streaming and Anthropic paths do.
	clampAttempts := map[string]int{}
	for index := 0; index < len(chain); index++ {
		model := chain[index]
		client := s.clientForModel(model)
		candidate := request
		candidate.Model = model
		// A route-forced effort describes the task, so it wins over the per-model
		// catalog override, which describes the model.
		if forced := forcedEffort(ctx); forced != "" {
			candidate.Effort = forced
		} else if effort := s.effortFor(model); effort != "" {
			candidate.Effort = effort
		}
		// Per candidate, not once per request: an alias chain can mix models whose
		// output caps differ, and the cap that matters is the one for whoever serves
		// the turn.
		s.clampOpenAIOutput(&candidate)
		candidate, err := s.prepareOpenAIRequest(ctx, candidate)
		if err != nil {
			lastErr = err
			continue
		}
		key := ""
		if cacheEligible {
			key, err = responseKey(candidate)
			if err != nil {
				return translator.OpenAIResponse{}, err
			}
			if encoded, ok := s.responses.Get(key); ok && json.Unmarshal(encoded, &response) == nil {
				return response, nil
			}
		}
		s.applyOpenAIPromptCache(ctx, &candidate)
		err = nil
		// A custom provider owns this model outright: neither the OAuth pool nor the
		// built-in client may be tried, since sending a prefixed model to them would
		// either report a bogus outage or hit the wrong upstream entirely.
		custom, isCustom := s.resolveCustomProvider(model)
		if isCustom {
			path, pathErr := customUpstreamPath(custom.APIType)
			if pathErr != nil {
				return translator.OpenAIResponse{}, pathErr
			}
			logCustomTarget(custom, endpoint)
			candidate.Model = custom.Model
			response = translator.OpenAIResponse{}
			if err = custom.Client.DoJSON(ctx, path, candidate, &response); err == nil {
				s.touchCustomProviderKey(custom.KeyID)
			}
		} else if s.oauthInference != nil {
			response, err = s.oauthInference.DoJSON(ctx, candidate, conversationID(ctx))
		}
		if !isCustom && (err != nil || s.oauthInference == nil) {
			if client == nil {
				if err != nil {
					lastErr = err
				} else {
					lastErr = ErrProviderUnavailable
				}
				continue
			}
			candidate.Model = upstreamModel(model)
			err = client.DoJSON(ctx, "/chat/completions", candidate, &response)
		}
		if err != nil {
			lastErr = err
			s.learnContextWindow(model, 0, err)
			if s.learnOutputLimit(model, requestedOutputTokens(candidate), clampAttempts[model], err) {
				clampAttempts[model]++
				index--
				continue
			}
			if !retryableProviderError(err) {
				return translator.OpenAIResponse{}, err
			}
			continue
		}
		if cacheEligible && cacheableOpenAIResponse(response) {
			if encoded, err := json.Marshal(response); err == nil {
				s.responses.Put(key, encoded, false, false)
			}
		}
		lastErr = nil
		served, sentEffort = model, candidate.Effort
		break
	}
	if lastErr != nil {
		return translator.OpenAIResponse{}, lastErr
	}
	usage := response.Usage
	// Each side is estimated on its own. Requiring both to be missing meant a provider
	// that reports only one — Cursor reports completion tokens and no prompt count —
	// recorded a hard zero for the other, which reads as "this request cost nothing"
	// rather than "this was not reported".
	if usage.PromptTokens == 0 {
		if unified, err := translator.FromOpenAIRequest(request); err == nil {
			usage.PromptTokens = contextguard.EstimateRequest(unified)
		}
	}
	if usage.CompletionTokens == 0 && len(response.Choices) > 0 {
		usage.CompletionTokens = contextguard.EstimateText(openAIMessageText(response.Choices[0].Message))
	}
	// "Estimated" means the upstream did not report the figure — whoever filled it in.
	// Deriving it from "the value is zero" instead would mark a provider's own estimate
	// as an authoritative count.
	promptEst, completionEst := !usage.PromptTokensReported, !usage.CompletionTokensReported
	s.recordUsage(UsageEvent{
		Provider: s.providerNameFor(served), Model: response.Model, RequestModel: request.Model, Endpoint: endpoint,
		PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
		CachedTokens:          usage.PromptTokensDetails.CachedTokens,
		CachedTokensReported:  usage.PromptTokensDetails.CachedTokensReported,
		PromptTokensEstimated: promptEst, CompletionTokensEstimated: completionEst,
		Effort: sentEffort,
	})
	return response, nil
}

// prepareStreamCandidate applies compression and the context guard to one
// candidate. When the caller already holds the unified form — the Anthropic
// endpoint always does — it is reused instead of translating the OpenAI request
// back, which halves the number of full-history passes per turn.
// The second return is the estimate of the payload this actually produced, or 0 when
// the request was passed through untouched. Calibration rides on it: pairing the
// caller's pre-pipeline estimate with the count the upstream reported for a compacted
// payload taught the tokenizer ratio the compaction factor instead of the tokenizer.
func (s *Service) prepareStreamCandidate(ctx context.Context, request translator.OpenAIRequest, unified *provider.Request) (translator.OpenAIRequest, int, error) {
	if unified == nil {
		prepared, err := s.prepareOpenAIRequest(ctx, request)
		return prepared, 0, err
	}
	if !s.contextPrepEnabled() && !s.contextGuard && s.compressionMode != cache.CompressionAggressive {
		return request, 0, nil
	}
	candidate := cloneProviderRequest(*unified)
	candidate.Model = request.Model
	candidate.Stream = request.Stream
	// The caller resolved this candidate's effort (route-forced or per-model
	// catalog override) on the OpenAI form. Rebuilding from the unified request
	// silently dropped it, which is how a catalog effort never reached a
	// streaming /v1/messages upstream while context work was enabled.
	if request.Effort != "" {
		candidate.Effort = request.Effort
	}
	if s.compressionMode == cache.CompressionAggressive {
		compressToolResults(&candidate, s.compressionMode)
	}
	var err error
	if s.contextPrepEnabled() {
		candidate, err = s.prepareContext(ctx, candidate)
		if err != nil {
			return translator.OpenAIRequest{}, 0, err
		}
	} else if s.contextGuard {
		if err := s.guardContext(ctx, candidate); err != nil {
			return translator.OpenAIRequest{}, 0, err
		}
	}
	// The image rule runs on the model actually about to be attempted, not just the one the
	// client asked for. A request routed to a vision model can fall back to a text-only model
	// (fallback_model is appended to every chain), and a routing decision made on the asked-for
	// model alone never re-checks the candidate that ends up serving — so a turn with an image
	// in its history would reach that text-only candidate carrying the image_url it cannot read.
	// Strip it here, per candidate, before translation.
	if route := s.imageRoute.Load(); route != nil && route.isTextOnly(candidate.Model) {
		s.stripProviderImages(&candidate)
	}
	result, err := translator.ToOpenAIRequest(candidate)
	if err != nil {
		return translator.OpenAIRequest{}, 0, err
	}
	result.PromptCacheKey = request.PromptCacheKey
	return result, contextguard.EstimateRequest(candidate), nil
}

// stripProviderImages replaces every image block in a provider request with a text
// placeholder. In the unified form, images nested inside a tool result are hoisted into
// standalone image blocks alongside the tool_result (translator anthropicToolResultContent),
// so top-level blocks cover both shapes. It runs before translation, per candidate, when
// the candidate model cannot read images.
func (s *Service) stripProviderImages(request *provider.Request) {
	for messageIndex := range request.Messages {
		message := &request.Messages[messageIndex]
		for contentIndex := range message.Content {
			block := &message.Content[contentIndex]
			if block.Type != "image" {
				continue
			}
			block.Type = "text"
			block.Data, block.MediaType, block.URL = "", "", ""
			block.Text = imagePlaceholder
		}
	}
}

func (s *Service) prepareOpenAIRequest(ctx context.Context, request translator.OpenAIRequest) (translator.OpenAIRequest, error) {
	if !s.contextPrepEnabled() && !s.contextGuard && s.compressionMode != cache.CompressionAggressive {
		return request, nil
	}
	unified, err := translator.FromOpenAIRequest(request)
	if err != nil {
		return translator.OpenAIRequest{}, err
	}
	if s.compressionMode == cache.CompressionAggressive {
		compressToolResults(&unified, s.compressionMode)
	}
	if s.contextPrepEnabled() {
		unified, err = s.prepareContext(ctx, unified)
		if err != nil {
			return translator.OpenAIRequest{}, err
		}
	} else if s.contextGuard {
		if err := s.guardContext(ctx, unified); err != nil {
			return translator.OpenAIRequest{}, err
		}
	}
	result, err := translator.ToOpenAIRequest(unified)
	if err != nil {
		return translator.OpenAIRequest{}, err
	}
	result.PromptCacheKey = request.PromptCacheKey
	return result, nil
}

func (s *Service) Messages(ctx context.Context, request translator.AnthropicRequest) (translator.AnthropicResponse, error) {
	unified, err := translator.FromAnthropicRequest(request)
	if err != nil {
		return translator.AnthropicResponse{}, err
	}
	response, err := s.complete(ctx, unified)
	if err != nil {
		return translator.AnthropicResponse{}, err
	}
	return translator.ToAnthropicResponse(response), nil
}

func (s *Service) Models() []string {
	return append([]string(nil), s.models...)
}

func (s *Service) complete(ctx context.Context, request provider.Request) (provider.Response, error) {
	if request.Stream {
		return provider.Response{}, fmt.Errorf("streaming requires the streaming gateway")
	}
	ctx = withPromptCacheSeedValue(ctx, request.User, firstProviderUserMessage(request.Messages))
	cacheEligible := s.responses != nil && s.cacheableProviderRequest(request)
	var raw translator.OpenAIResponse
	var response provider.Response
	var lastErr error
	chain := s.modelChain(request.Model)
	// The candidate that actually served, which is not always the one the client asked
	// for: the chain can hand the turn to an alias or to the router's fallback model, and
	// either can belong to a different provider than the requested id does.
	served := request.Model
	// A response with no content is the non-streaming twin of the empty turn the
	// streaming path retries. Returning it verbatim is what surfaces as an empty
	// response — and on a /compact request that means the summary silently comes
	// back blank — so the same retry-then-replay allowance applies here.
	emptyReplays := 0
	var emptyResponse provider.Response
	var sawEmpty bool
	clampAttempts := map[string]int{}
	trims := 0
	// sentRequest is the payload the winning attempt actually sent. Calibration below
	// needs it rather than the caller's request: the two differ by whatever the context
	// pipeline removed, and learning that difference as a tokenizer ratio is what
	// inflated the scale.
	sentRequest := request
	for index := 0; index < len(chain); index++ {
		model := chain[index]
		client := s.clientForModel(model)
		candidate := cloneProviderRequest(request)
		candidate.Model = model
		if forced := forcedEffort(ctx); forced != "" {
			candidate.Effort = forced
		} else if effort := s.effortFor(model); effort != "" {
			candidate.Effort = effort
		}
		s.clampProviderOutput(&candidate)
		if s.compressionMode == cache.CompressionAggressive {
			compressToolResults(&candidate, s.compressionMode)
		}
		prepared := candidate
		var err error
		if s.contextPrepEnabled() {
			prepared, err = s.prepareContext(ctx, candidate)
		} else if s.contextGuard {
			err = s.guardContext(ctx, candidate)
		}
		if err != nil {
			lastErr = err
			// Same rule as the streaming chain: a turn too large for this candidate may
			// still fit a later one, and this path used to abandon on any prepare error
			// without even advancing.
			if errors.Is(err, contextguard.ErrBudgetExceeded) {
				_, window, _ := s.contextLimitsFor(ctx, model)
				if s.laterCandidateHoldsMore(ctx, chain[index+1:], window) {
					continue
				}
			}
			return provider.Response{}, err
		}
		// Same per-candidate image rule as the streaming path: a fallback to a text-only
		// model must not carry an image the routing decision (made on the asked-for model)
		// left in place.
		if route := s.imageRoute.Load(); route != nil && route.isTextOnly(model) {
			s.stripProviderImages(&prepared)
		}
		sentRequest = prepared
		upstream, err := translator.ToOpenAIRequest(prepared)
		if err != nil {
			return provider.Response{}, err
		}
		key := ""
		if cacheEligible {
			key, err = responseKey(upstream)
			if err != nil {
				return provider.Response{}, err
			}
			if encoded, ok := s.responses.Get(key); ok && json.Unmarshal(encoded, &response) == nil {
				return response, nil
			}
		}
		s.applyOpenAIPromptCache(ctx, &upstream)
		err = nil
		target, isCustom := s.resolveCustomProvider(model)
		if isCustom {
			path, pathErr := customUpstreamPath(target.APIType)
			if pathErr != nil {
				return provider.Response{}, pathErr
			}
			logCustomTarget(target, "/v1/messages")
			upstream.Model = target.Model
			raw = translator.OpenAIResponse{}
			if err = target.Client.DoJSON(ctx, path, upstream, &raw); err == nil {
				s.touchCustomProviderKey(target.KeyID)
			}
		} else if s.oauthInference != nil {
			raw, err = s.oauthInference.DoJSON(ctx, upstream, conversationID(ctx))
		}
		if !isCustom && (err != nil || s.oauthInference == nil) {
			if client == nil {
				if err == nil {
					lastErr = ErrProviderUnavailable
					continue
				}
				// OAuth-only deployments have no fallback client, so this is the only
				// place an oauth rejection can be recovered from. Learn the window and
				// re-attempt the same candidate on the shrunken history; otherwise move
				// on exactly as before.
				lastErr = err
				s.learnContextWindow(model, 0, err)
				if s.trimAfterContextRejection(ctx, model, &request, &trims, err) {
					index--
				}
				continue
			}
			upstream.Model = upstreamModel(model)
			err = client.DoJSON(ctx, "/chat/completions", upstream, &raw)
		}
		if err != nil {
			lastErr = err
			// The rejection teaches the real window first, so the trim retry below
			// budgets against what the upstream just revealed.
			s.learnContextWindow(model, 0, err)
			// An output-cap rejection is not retryable as sent, but it is retryable once
			// the cap it just revealed is applied. Learn it and re-attempt this candidate
			// so the caller never sees the 400.
			if s.learnOutputLimit(model, requestedOutputTokens(upstream), clampAttempts[model], err) {
				clampAttempts[model]++
				index--
				continue
			}
			// A context rejection is recoverable the same way the streaming path
			// recovers: compact or trim the shared request and re-attempt this
			// candidate. The helper mutates the outer request, so the retry's
			// per-candidate clone starts from the smaller history.
			if s.trimAfterContextRejection(ctx, model, &request, &trims, err) {
				index--
				continue
			}
			if !retryableProviderError(err) {
				return provider.Response{}, err
			}
			continue
		}
		response, err = translator.FromOpenAIResponse(raw)
		if err != nil {
			return provider.Response{}, err
		}
		if !providerResponseHasOutput(response, len(request.Tools) > 0) {
			emptyResponse, sawEmpty = response, true
			lastErr = errEmptyUpstreamResponse
			slog.Warn("upstream returned a response with no content", "model", model,
				"endpoint", "/v1/messages", "replay", emptyReplays)
			if index == len(chain)-1 && emptyReplays < maxEmptyTurnReplays {
				// Out of candidates: ask the same one again rather than handing back a
				// blank answer. Bounded, and only reachable in this dead end.
				emptyReplays++
				index--
			}
			continue
		}
		if cacheEligible && cacheableProviderResponse(response) {
			if encoded, err := json.Marshal(response); err == nil {
				s.responses.Put(key, encoded, false, false)
			}
		}
		lastErr = nil
		served = model
		break
	}
	if lastErr != nil {
		// Every attempt came back blank. The caller still needs a well-formed
		// response, and reporting an error here would break the legitimate case of a
		// model that genuinely has nothing left to say.
		if errors.Is(lastErr, errEmptyUpstreamResponse) && sawEmpty {
			return emptyResponse, nil
		}
		return provider.Response{}, lastErr
	}
	usage := raw.Usage
	// A reported prompt count plus the estimate of the same payload is a free
	// calibration sample; the streaming path was the only teacher before this. It has to
	// be the payload that was sent, not the one that arrived — see sentRequest.
	if usage.PromptTokensReported {
		s.observeTokenScale(request.Model, 0, contextguard.EstimateRequest(sentRequest), usage.PromptTokens)
	}
	// As above: estimate each side independently so a partially reporting provider
	// does not record a zero that is indistinguishable from a free request.
	if usage.PromptTokens == 0 {
		usage.PromptTokens = contextguard.EstimateRequest(request)
	}
	if usage.CompletionTokens == 0 && len(response.Content) > 0 {
		var out strings.Builder
		for _, block := range response.Content {
			if block.Type == "text" || block.Type == "thinking" {
				out.WriteString(block.Text)
			}
		}
		usage.CompletionTokens = contextguard.EstimateText(out.String())
	}
	promptEst, completionEst := !usage.PromptTokensReported, !usage.CompletionTokensReported
	// Attribution keys on the candidate that served, not on the echoed name and not on the
	// requested id. The echoed name arrives with the routing prefix already stripped, so
	// resolving that would report a custom provider's traffic as the built-in fallback. The
	// requested id is wrong for a different reason: a turn asked for as claude-opus-5 and
	// served by the fallback on cx/gpt-5.6-luna was filed under the claude provider, so the
	// dashboard read "Claude / GPT 5.6 Luna" and billed an upstream that was never called.
	s.recordUsage(UsageEvent{
		Provider: s.providerNameFor(served), Model: raw.Model, RequestModel: request.Model, Endpoint: "/v1/messages",
		PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
		CachedTokens:          usage.PromptTokensDetails.CachedTokens,
		CachedTokensReported:  usage.PromptTokensDetails.CachedTokensReported,
		PromptTokensEstimated: promptEst, CompletionTokensEstimated: completionEst,
		// sentRequest is the winning attempt's own payload, so its effort is the one that
		// went up — not the caller's ask, which a route or a per-model override may have
		// replaced.
		Effort: sentRequest.Effort,
	})
	return response, nil
}

func cloneProviderRequest(request provider.Request) provider.Request {
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

func (s *Service) guardContext(ctx context.Context, request provider.Request) error {
	limits, window, err := s.contextLimitsFor(ctx, request.Model)
	if err != nil {
		// This check cannot do anything but log — it defers every real decision to the
		// upstream tokenizer — and the lookup hands back a serviceable window from
		// configuration even when it failed. Returning the error surfaced as a 502 on
		// every turn for the duration of a transient catalog read, which is the whole
		// session, over a number this function was only ever going to warn about.
		slog.Warn("context window lookup failed; guarding against the fallback window",
			"model", request.Model, "fallback_window", window, "error", err)
	}
	result, err := contextguard.Check(request, limits, s.requestContextPolicy(request.Model))
	if err != nil {
		return err
	}
	if result.EstimatedOverage {
		slog.Warn("context estimate exceeds policy threshold; deferring to upstream tokenizer",
			"model", request.Model, "estimated_input_tokens", result.InputTokens,
			"safe_limit", result.SafeLimit, "context_window", result.Window)
	}
	return nil
}

// requestContextPolicy resolves the policy for one request: the mode in force,
// plus the estimate scale this model's own served turns have taught.
//
// While the measurement is young it passes through a conservative clamp — a bad
// early sample applied to the budget could either erase the safety margin or
// starve the request, and 0.6–1.5 covers both measured extremes (0.9 and 1.8)
// with room. Once enough samples agree (calibConfidentSamples at or under
// calibStableSpread), the measured value is applied as-is: the evidence takes
// over from the guess, which is exactly what makes the ratio knobs stop needing
// hand-tuning.
func (s *Service) requestContextPolicy(model string) contextguard.Policy {
	policy := s.activeContextPolicy()
	scale := s.tokenScaleFor(model)
	low, high := 0.6, 1.5
	if scale.samples >= calibConfidentSamples && scale.spread <= calibStableSpread {
		low, high = minEstimatePerToken, maxEstimatePerToken
	}
	policy.EstimateScale = min(max(scale.estimatePerToken, low), high)
	return policy
}

// contextPrepOutcome is what prepareContext reports about one mutated request.
type contextPrepOutcome struct {
	stage        string
	beforeTokens int
	afterTokens  int
	window       int
	summaryModel string
}

func (s *Service) prepareContext(ctx context.Context, request provider.Request) (provider.Request, error) {
	if !s.contextPrepEnabled() {
		return request, nil
	}
	start := time.Now()
	prepared, outcome, err := s.prepareContextStages(ctx, request)
	if err != nil {
		return provider.Request{}, err
	}
	if outcome.stage != "" {
		// One line per mutated request — the success ledger. The Warn lines inside
		// the stages remain the failure diagnostics.
		slog.Info("context pipeline mutated request",
			"model", request.Model, "stage", outcome.stage,
			"before_tokens", outcome.beforeTokens, "after_tokens", outcome.afterTokens,
			"window", outcome.window, "summary_model", outcome.summaryModel,
			"duration", time.Since(start).Round(time.Millisecond))
	}
	return prepared, nil
}

func (s *Service) prepareContextStages(ctx context.Context, request provider.Request) (provider.Request, contextPrepOutcome, error) {
	policy := s.requestContextPolicy(request.Model)
	// Same resolution the guard uses. A lookup failure is not actionable here — the
	// window handed back is serviceable and the pipeline has nothing to say about it.
	limits, window, _ := s.contextLimitsFor(ctx, request.Model)
	outcome := contextPrepOutcome{window: window}
	prepared, err := contextguard.Prepare(request, limits, policy)
	outcome.beforeTokens, outcome.afterTokens = prepared.BeforeTokens, prepared.AfterTokens
	if err != nil {
		if errors.Is(err, contextguard.ErrBudgetExceeded) {
			return s.trimStage(request, limits, policy, outcome)
		}
		return provider.Request{}, outcome, err
	}
	if prepared.Compacted {
		outcome.stage = "compact"
	}
	if !prepared.NeedsSummary || skipPreemptiveTrimFor(request.Model, window) {
		// A client that believes a larger window than the model has sends every turn
		// past the soft ratio, and trimming on that belief loses history the upstream
		// would have taken — the Cursor path measured 45 messages dropped per turn
		// while the model never refused. For such models the trim runs only after the
		// upstream actually refuses, where trimAfterContextRejection is the backstop.
		return prepared.Request, outcome, nil
	}
	// summaryRecentTurns probes by rebuilding and re-estimating the whole request once
	// per candidate keep value, which is milliseconds on a six-figure-token turn. Ask
	// the cheap question first: keep=1 yields the largest backlog any keep value can,
	// so an empty one there means no summary is possible at all. Every subagent lands
	// here — its only non-tool_result user message is the opening task — and used to
	// pay for five full probes before the same fallback ran anyway.
	// A compaction request is a payload whose entire purpose is to be summarized by the
	// model it is sent to, so summarizing it here first is work done twice — and the model
	// then summarizes a summary. Measured on one turn of this session: the LLM pass took
	// 35.2s to go from 645,471 tokens to 63,059, while the deterministic trim took 1.1s to
	// reach 154,651, which already fits. Two summarize rounds per compaction is most of why
	// a compact sat at 95% for minutes.
	//
	// The same double-work applies to any turn that already carries a proxy summary in
	// its first user message (the post-compact continuation, or a session the proxy's
	// own summarize stage previously rewrote). That summary is the older conversation in
	// compressed form; summarizing it again here adds a second compress-and-replace pass
	// over a transcript that is already the output of one, for the same reason the
	// compaction request itself is skipped. Measured on a real post-compact turn, the
	// second pass cost another ~35s before the deterministic trim ran anyway.
	//
	// The trade is real and worth stating: trim drops the oldest turn-units outright, so the
	// summary the client ends up with covers less of the beginning than a compressed-then-
	// summarized history would. Latency was the priority; reverting is deleting one clause.
	if s.summarizer == nil || isCompactTurn(ctx) || requestAlreadySummarized(prepared.Request) || s.SummarizeMode() == SummarizeModeTrim || len(contextguard.SummaryMessages(prepared.Request.Messages, 1, policy.BoundaryQuantum)) == 0 {
		if contextguard.ExceedsHardLimit(prepared, policy) {
			return s.trimStage(prepared.Request, limits, policy, outcome)
		}
		return prepared.Request, outcome, nil
	}
	keepRecentTurns := s.summaryRecentTurns(prepared.Request, limits, policy)
	older := contextguard.SummaryMessages(prepared.Request.Messages, keepRecentTurns, policy.BoundaryQuantum)
	if len(older) == 0 {
		if contextguard.ExceedsHardLimit(prepared, policy) {
			return s.trimStage(prepared.Request, limits, policy, outcome)
		}
		return prepared.Request, outcome, nil
	}
	model := s.resolveSummaryModel(prepared.Request.Model)
	outcome.summaryModel = model
	// Summarizing means sending the backlog to a model, so the summary call is
	// itself bound by the context window. Once the backlog alone overflows, the
	// call cannot succeed — it just burns the whole summary timeout before the
	// deterministic trim runs anyway, which showed up as a flat 60s added to
	// time-to-first-token on every oversized turn.
	if !s.summaryInputFits(older, model) {
		if contextguard.ExceedsHardLimit(prepared, policy) {
			slog.Warn("summary backlog exceeds the context window; trimming oldest turns without summarizing",
				"model", prepared.Request.Model, "summary_model", model, "backlog_messages", len(older))
			return s.trimStage(prepared.Request, limits, policy, outcome)
		}
		return prepared.Request, outcome, nil
	}
	key, keyErr := contextguard.SummaryKey(model, s.summaryMaxTokens, older)
	summaryContext, cancel := context.WithTimeout(ctx, s.summaryTimeout)
	summarize := func() (string, error) {
		return s.summarizer.Summarize(summaryContext, contextguard.SummaryInput{
			Model: model, Messages: older, MaxTokens: s.summaryMaxTokens,
		})
	}
	var summary string
	if keyErr != nil {
		summary, err = summarize()
	} else {
		summary, err = s.summaryCache.Do(summaryContext, key, summarize)
	}
	cancel()
	if err != nil || strings.TrimSpace(summary) == "" {
		if contextguard.ExceedsHardLimit(prepared, policy) {
			slog.Warn("summarization unavailable; trimming oldest turns", "model", prepared.Request.Model, "error", err)
			return s.trimStage(prepared.Request, limits, policy, outcome)
		}
		return prepared.Request, outcome, nil
	}
	summarized := contextguard.ApplySummary(prepared.Request, summary, keepRecentTurns, policy.BoundaryQuantum)
	outcome.stage = "summarize"
	verified, verifyErr := contextguard.Prepare(summarized, limits, policy)
	if verifyErr != nil || contextguard.ExceedsHardLimit(verified, policy) {
		return s.trimStage(summarized, limits, policy, outcome)
	}
	outcome.afterTokens = verified.AfterTokens
	return verified.Request, outcome, nil
}

// requestAlreadySummarized reports whether the request already carries a proxy
// summary as its oldest message — the marker a prior summarize stage wrote, or
// the summary the client's post-compact continuation quotes back as its first
// user message. Such a request is the output of a previous summarization, so
// summarizing it again here would compress an already-compressed conversation.
//
// The marker is the proxy's own (ProxySummaryMarker), checked only in the oldest
// user message: that is where ApplySummary writes it and where the client's
// post-compact continuation quotes it. A marker anywhere else is history quoting
// a summary, not the request being one.
func requestAlreadySummarized(request provider.Request) bool {
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		for _, block := range message.Content {
			if block.Type == "text" && strings.Contains(block.Text, contextguard.ProxySummaryMarker) {
				return true
			}
		}
		// The summary lives in the oldest user message; nothing after it can be one.
		break
	}
	return false
}

// skipPreemptiveTrimFor reports whether a model should skip the pipeline's
// estimate-based trim and rely on trim-after-refusal only. The client may believe a
// larger window than the model has (the 1M beta on a ~194k model), which puts every
// turn past the soft ratio — trimming on the estimate then loses history the
// upstream would have served. The refusal path is the ground truth and still works.
func skipPreemptiveTrimFor(model string, window int) bool {
	return false
}

// resolveSummaryModel picks who writes proxy summaries: the explicit configured
// summarizer, else the compact model — the operator already chose it as the fast
// summarizer for client compacts — else the request's own model.
func (s *Service) resolveSummaryModel(requestModel string) string {
	if s.summaryModel != "" {
		return s.summaryModel
	}
	if compact := s.CompactModel(); compact != "" {
		return compact
	}
	return requestModel
}

// maxSummaryBatches bounds how large a backlog is worth summarizing. Three covers a
// conversation roughly three times its own window, which is every overflow seen in the
// logs; past that the summary costs more than the session it is trying to save, and the
// deterministic trim is the better answer.
const maxSummaryBatches = 3

// summaryInputFits reports whether this backlog is worth attempting to summarize.
//
// It used to ask whether the backlog fit a single window. That was the right question for
// a single-shot summarizer and the wrong one here: Summarize map-reduces through
// SummaryBatches, so it handles a backlog of any size, and the check switched it off in
// exactly the cases it exists for. Every trim in the production log came through this
// gate — eight trims, eight refusals to summarize, no summary ever attempted.
//
// What does need bounding is the work, which is why this counts batches instead of
// allowing everything. The batches run concurrently and at low effort now, so the bound
// is about cost rather than the timeout it used to be about.
func (s *Service) summaryInputFits(older []provider.Message, model string) bool {
	budget := s.summaryInputBudget(model, s.summaryMaxTokens)
	if budget <= 0 {
		return false
	}
	tokens := contextguard.EstimateRequest(provider.Request{Model: model, Messages: older})
	return tokens <= budget*maxSummaryBatches
}

// trimStage runs the deterministic trim and stamps the outcome accordingly.
func (s *Service) trimStage(request provider.Request, limits contextguard.Limits, policy contextguard.Policy, outcome contextPrepOutcome) (provider.Request, contextPrepOutcome, error) {
	trimmed, err := s.trimToBudget(request, limits, policy)
	if err != nil {
		return provider.Request{}, outcome, err
	}
	if outcome.stage == "summarize" {
		outcome.stage = "summarize+trim"
	} else {
		outcome.stage = "trim"
	}
	outcome.afterTokens = contextguard.EstimateRequest(trimmed)
	return trimmed, outcome, nil
}

// trimToBudget is the deterministic last resort before rejecting a request.
// Dropping whole older turns is lossy, but it keeps the session alive; a hard
// rejection ends the turn and leaves the caller with nothing to continue from.
func (s *Service) trimToBudget(request provider.Request, limits contextguard.Limits, policy contextguard.Policy) (provider.Request, error) {
	budget := contextguard.HardBudget(request, limits, policy)
	trimmed, ok := contextguard.TrimOldestTurns(request, budget, policy.KeepContaining)
	if !ok {
		return provider.Request{}, contextguard.ErrBudgetExceeded
	}
	slog.Warn("trimmed oldest turns to fit context window", "model", request.Model, "budget_tokens", budget,
		"before_messages", len(request.Messages), "after_messages", len(trimmed.Messages))
	return trimmed, nil
}

func (s *Service) summaryRecentTurns(request provider.Request, limits contextguard.Limits, policy contextguard.Policy) int {
	keep := policy.KeepRecentTurns
	for keep > 1 {
		probe := contextguard.ApplySummary(request, strings.Repeat("s", max(s.summaryMaxTokens, 1)*4), keep, policy.BoundaryQuantum)
		prepared, err := contextguard.Prepare(probe, limits, policy)
		if err == nil && !contextguard.ExceedsHardLimit(prepared, policy) {
			break
		}
		keep--
	}
	return keep
}

func (s *Service) applyOpenAIPromptCache(ctx context.Context, request *translator.OpenAIRequest) {
	// xAI only accepts prompt_cache_key when explicitly opted in.
	if isXAIModel(request.Model) && !s.xaiPromptCache {
		request.PromptCacheKey = ""
		return
	}
	if request.PromptCacheKey != "" {
		return
	}
	// promptMinBytes <= 0 disables injection (pass-through only). Claude CLI stability
	// prefers no synthetic keys that can break provider caching contracts.
	if s.promptMinBytes <= 0 {
		return
	}
	encoded, err := json.Marshal(request.Messages)
	if err != nil || len(encoded) < s.promptMinBytes {
		return
	}
	firstUser := promptCacheSeed(ctx)
	if firstUser == "" {
		firstUser = firstUserMessage(request.Messages)
	}
	request.PromptCacheKey = cache.StickyPromptCacheKey(request.Model, conversationID(ctx), request.User, firstUser)
}

func firstProviderUserMessage(messages []provider.Message) string {
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		for _, block := range message.Content {
			if block.Type == "text" && block.Text != "" {
				return block.Text
			}
		}
	}
	return ""
}

func openAIMessageText(message translator.OpenAIMessage) string {
	switch content := message.Content.(type) {
	case string:
		return content
	case []translator.OpenAIContentPart:
		var b strings.Builder
		for _, part := range content {
			b.WriteString(part.Text)
		}
		return b.String()
	default:
		return ""
	}
}

func firstUserMessage(messages []translator.OpenAIMessage) string {
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		switch content := message.Content.(type) {
		case string:
			if content != "" {
				return content
			}
		case []translator.OpenAIContentPart:
			for _, part := range content {
				if (part.Type == "text" || part.Type == "input_text") && part.Text != "" {
					return part.Text
				}
			}
		case []any:
			encoded, err := json.Marshal(content)
			if err != nil {
				continue
			}
			var parts []translator.OpenAIContentPart
			if json.Unmarshal(encoded, &parts) == nil {
				for _, part := range parts {
					if (part.Type == "text" || part.Type == "input_text") && part.Text != "" {
						return part.Text
					}
				}
			}
		}
	}
	return ""
}

func (s *Service) cacheableOpenAIRequest(request translator.OpenAIRequest) bool {
	return request.Temperature != nil && *request.Temperature == 0 && len(request.Tools) == 0 && request.ToolChoice == nil && len(s.aliases[request.Model]) == 0
}

func (s *Service) cacheableProviderRequest(request provider.Request) bool {
	return request.Temperature != nil && *request.Temperature == 0 && len(request.Tools) == 0 && request.ToolChoice.Type == "" && len(s.aliases[request.Model]) == 0
}

func cacheableOpenAIResponse(response translator.OpenAIResponse) bool {
	if len(response.Choices) == 0 {
		return false
	}
	for _, choice := range response.Choices {
		if choice.FinishReason != "stop" || len(choice.Message.ToolCalls) > 0 || !openAIContentPresent(choice.Message.Content) {
			return false
		}
	}
	return true
}

func openAIContentPresent(content any) bool {
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case []translator.OpenAIContentPart:
		return len(value) > 0
	case []any:
		return len(value) > 0
	default:
		return content != nil
	}
}

func cacheableProviderResponse(response provider.Response) bool {
	return response.StopReason == "end_turn" && len(response.Content) > 0 && !hasToolCalls(response)
}

// modelChain is the ordered list of models one turn may be attempted on. It is the single
// place the list is built — the summarizer, both non-streaming paths, the stream opener and
// the Anthropic passthrough all call it — which is why the fallback is appended here rather
// than at each of those five call sites.
func (s *Service) modelChain(model string) []string {
	chain := []string{model}
	if aliases := s.aliases[model]; len(aliases) > 0 {
		chain = append([]string(nil), aliases...)
	}
	return s.withFallbackModel(chain)
}

// errEmptyUpstreamResponse marks a non-streaming reply that carried no content and
// no tool call.
var errEmptyUpstreamResponse = errors.New("upstream returned a response with no content")

// providerResponseHasOutput reports whether a response carries anything the caller
// can use. Whitespace-only text counts as nothing: it is what an upstream emits
// when it has produced no real output at all.
func providerResponseHasOutput(response provider.Response, toolsAllowed bool) bool {
	for _, block := range response.Content {
		switch block.Type {
		case "tool_use":
			// A tool call is only output if the caller can act on it. Offered no tools,
			// the client has no schema for the call and sees a turn with no answer.
			if toolsAllowed {
				return true
			}
		default:
			if strings.TrimSpace(block.Text) != "" || strings.TrimSpace(block.Thinking) != "" {
				return true
			}
		}
	}
	return false
}

func retryableProviderError(err error) bool {
	if errors.Is(err, ErrProviderUnavailable) {
		return true
	}
	var providerError *provider.ProviderError
	return errors.As(err, &providerError) && (providerError.StatusCode == 429 || providerError.StatusCode >= 500)
}

func cloneAliases(aliases map[string][]string) map[string][]string {
	result := make(map[string][]string, len(aliases))
	for alias, chain := range aliases {
		result[alias] = append([]string(nil), chain...)
	}
	return result
}

func isXAIModel(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(lower, "grok") || strings.HasPrefix(lower, "xai/") || lower == "xai"
}

func upstreamModel(model string) string {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(model), "xai/") {
		return model[len("xai/"):]
	}
	return model
}

// resolveCustomProvider reports the custom upstream for a model, if one claims it.
func (s *Service) resolveCustomProvider(model string) (CustomTarget, bool) {
	if s.customProviders == nil {
		return CustomTarget{}, false
	}
	return s.customProviders.Resolve(model)
}

func (s *Service) touchCustomProviderKey(keyID string) {
	if s.touchCustomKey != nil && keyID != "" {
		s.touchCustomKey(keyID)
	}
}

func (s *Service) clientForModel(model string) JSONClient {
	if isXAIModel(model) {
		return s.xai
	}
	return s.openAI
}

func responseKey(request translator.OpenAIRequest) (string, error) {
	marshal := func(value any) (json.RawMessage, error) {
		encoded, err := json.Marshal(value)
		return encoded, err
	}
	messages, err := marshal(request.Messages)
	if err != nil {
		return "", err
	}
	tools, err := marshal(request.Tools)
	if err != nil {
		return "", err
	}
	toolChoice, err := marshal(request.ToolChoice)
	if err != nil {
		return "", err
	}
	temperature, _ := marshal(request.Temperature)
	topP, _ := marshal(request.TopP)
	seed, _ := marshal(request.Seed)
	stop, _ := marshal(request.Stop)
	presencePenalty, _ := marshal(request.PresencePenalty)
	frequencyPenalty, _ := marshal(request.FrequencyPenalty)
	return cache.BuildResponseKey(cache.ResponseKey{
		Model: request.Model, Temperature: temperature, TopP: topP, Seed: seed, Stop: stop,
		ResponseFormat: request.ResponseFormat, N: request.N,
		PresencePenalty: presencePenalty, FrequencyPenalty: frequencyPenalty,
		MaxTokens: request.MaxTokens, MaxCompletionTokens: request.MaxCompletionTokens,
		Messages: messages, Tools: tools, ToolChoice: toolChoice,
	})
}

func promptMessages(request provider.Request) []cache.PromptMessage {
	messages := make([]cache.PromptMessage, 0, len(request.System)+len(request.Messages))
	for _, block := range request.System {
		if block.Type == "text" {
			messages = append(messages, cache.PromptMessage{Role: "system", Content: block.Text})
		}
	}
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "text" || block.Type == "tool_result" {
				messages = append(messages, cache.PromptMessage{Role: message.Role, Content: block.Text})
			}
		}
	}
	return messages
}

func compressToolResults(request *provider.Request, mode cache.CompressionMode) {
	names := make(map[string]string)
	for messageIndex := range request.Messages {
		message := &request.Messages[messageIndex]
		for contentIndex := range message.Content {
			block := &message.Content[contentIndex]
			if block.Type == "tool_use" {
				names[block.ToolUseID] = block.Name
				continue
			}
			if block.Type != "tool_result" {
				continue
			}
			result := cache.CompressToolResultMode(names[block.ToolUseID], block.Text, mode)
			block.Text = result.Compressed
		}
	}
}

func hasToolCalls(response provider.Response) bool {
	for _, block := range response.Content {
		if block.Type == "tool_use" {
			return true
		}
	}
	return false
}

// UsageEvent is a lightweight gateway request metric (no prompt bodies).
type UsageEvent struct {
	Provider                  string
	Model                     string
	Endpoint                  string
	Status                    string
	PromptTokens              int
	CompletionTokens          int
	CachedTokens              int
	PromptTokensEstimated     bool
	CompletionTokensEstimated bool
	CachedTokensReported      bool
	// Effort is the reasoning effort sent upstream for this turn.
	Effort string
	// RequestModel is the id the client asked for, when it differs from Model.
	//
	// Model carries what the upstream reported, which is right for usage attribution but
	// wrong for keying anything the gateway later looks up by requested id: an upstream
	// answering "gpt-5.6-sol" for a request routed as "cx/gpt-5.6-sol" would file the
	// lesson under a name no later lookup can reach, because prefix matching only
	// extends a requested id, never strips from it. Optional — Model is used when empty.
	RequestModel string
}

func (s *Service) recordUsage(ev UsageEvent) {
	if s == nil {
		return
	}
	if ev.Status == "" {
		ev.Status = "ok"
	}
	// Every path funnels through here on the way out, which makes it the one place
	// that sees a served prompt together with the count the upstream put on it — the
	// lower bound on that model's window, for free. Estimated counts are skipped:
	// only the upstream's own arithmetic proves what it accepted.
	if ev.Status == "ok" && !ev.PromptTokensEstimated {
		model := ev.RequestModel
		if model == "" {
			model = ev.Model
		}
		s.observeContextWindow(model, ev.PromptTokens)
	}
	if s.onUsage == nil {
		return
	}
	// The callback only enqueues into a bounded writer, so the request path stays non-blocking.
	s.onUsage(ev)
}

func (s *Service) SetOnUsage(fn func(UsageEvent)) {
	if s != nil {
		s.onUsage = fn
	}
}

// providerNameFor attributes usage to the provider that actually served the model.
// A custom provider is reported under its own prefix; without this it fell through
// to the built-in switch and every custom request was recorded as "openai", which
// made the usage table and the routing map wrong.
func (s *Service) providerNameFor(model string) string {
	// PrefixFor, not Resolve: naming the provider must not consume a rotation slot.
	if s.customProviders != nil {
		if prefix, ok := s.customProviders.PrefixFor(model); ok {
			return "custom:" + prefix
		}
	}
	return providerNameForModel(model)
}

func providerNameForModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "ag/"), strings.HasPrefix(m, "antigravity/"), strings.HasPrefix(m, "gemini-"), strings.HasPrefix(m, "gemini/"):
		return "antigravity"
	case strings.HasPrefix(m, "claude"), strings.HasPrefix(m, "anthropic"):
		return "claude"
	case strings.HasPrefix(m, "grok"), strings.HasPrefix(m, "xai/"):
		return "xai"
	case strings.HasPrefix(m, "cx/"), strings.Contains(m, "codex"), strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return "codex"
	default:
		return "openai"
	}
}
