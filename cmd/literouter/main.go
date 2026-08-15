package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"

	"literouter/internal/cache"
	"literouter/internal/clisetup"
	"literouter/internal/config"
	"literouter/internal/contextguard"
	"literouter/internal/gateway"
	"literouter/internal/pool"
	pooloauth "literouter/internal/pool/oauth"
	"literouter/internal/provider"
	"literouter/internal/recommendation"
	"literouter/internal/secret"
	"literouter/internal/storage"
	"literouter/internal/toolstore"
	"literouter/internal/translator"
	"literouter/internal/ui"
	"literouter/internal/usage"
)

const (
	healthcheckTimeout = 2 * time.Second
	// toolstoreDefaultMaxBytes caps how much elided tool output the reference store
	// holds in memory (64 MiB). Enough for the bulk of a long coding session's
	// truncated reads while never becoming a second copy of the whole conversation.
	toolstoreDefaultMaxBytes = 64 << 20
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "", "path to YAML configuration")
	healthcheck := flag.Bool("healthcheck", false, "check the local health endpoint")
	setupCodex := flag.Bool("setup-codex", false, "configure local ~/.codex/config.toml to use LiteRouter")
	flag.Parse()

	if *setupCodex || (len(flag.Args()) > 0 && flag.Args()[0] == "setup-codex") {
		if err := configureCodex(); err != nil {
			fmt.Fprintf(os.Stderr, "setup codex error: %v\n", err)
			return 1
		}
		fmt.Println("Successfully configured ~/.codex/config.toml for LiteRouter")
		return 0
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load configuration: %v\n", err)
		return 1
	}
	if *healthcheck {
		if err := checkHealth(cfg.Server.Addr); err != nil {
			fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
			return 1
		}
		return 0
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.Log.Level),
	}))
	slog.SetDefault(logger)

	key, err := secret.DecodeKey(os.Getenv("LITEROUTER_MASTER_KEY"))
	if err != nil {
		logger.Error("invalid LITEROUTER_MASTER_KEY", "error", err)
		return 1
	}
	box, err := secret.New(key)
	if err != nil {
		logger.Error("initialize credential encryption", "error", err)
		return 1
	}
	store, err := storage.Open(cfg.Storage.Path, box)
	if err != nil {
		logger.Error("open storage", "error", err)
		return 1
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Error("close storage", "error", err)
		}
	}()

	migrationContext, cancelMigration := context.WithTimeout(context.Background(), 10*time.Second)
	if err := store.Migrate(migrationContext); err != nil {
		cancelMigration()
		logger.Error("migrate storage", "error", err)
		return 1
	}
	cancelMigration()

	storedAccounts, err := store.ListAccounts(context.Background())
	if err != nil {
		logger.Error("load account pool", "error", err)
		return 1
	}
	accountPool := pool.New(poolAccounts(storedAccounts))
	if err := restoreQuotaSnapshots(context.Background(), store, accountPool, storedAccounts); err != nil {
		logger.Error("restore quota snapshots", "error", err)
		return 1
	}
	codexProvider := pooloauth.NewCodexProvider(nil)
	claudeProvider := pooloauth.NewClaudeProvider(nil)
	grokProvider := pooloauth.NewGrokProvider(nil)
	antigravityProvider := pooloauth.NewAntigravityProvider(nil)
	// The OAuth app identity comes from LiteRouter's own settings, configured in the UI —
	// never hardcoded and never from env. Restore it here so a restart keeps the login.
	if clientID, storedErr := store.GetSetting(context.Background(), "antigravity.client_id"); storedErr == nil {
		secret, _ := store.GetSetting(context.Background(), "antigravity.client_secret")
		antigravityProvider.SetCredentials(clientID, secret)
	}
	oauthManager, err := pooloauth.NewManager(store, accountPool, key, logger)
	if err != nil {
		logger.Error("initialize OAuth", "error", err)
		return 1
	}
	// The OAuth flows must use the same provider instance that carries the configured
	// credentials; a fresh one inside the manager would authorize with an empty client id.
	oauthManager.SetAntigravityProvider(antigravityProvider)
	credentialManager := pooloauth.NewCredentialManager(store, accountPool, logger, codexProvider, claudeProvider, grokProvider, antigravityProvider)
	usageService := usage.NewService(store, accountPool, credentialManager)
	usageService.SetLogger(logger)
	oauthManager.SetOnAccountConnected(func(ctx context.Context, accountID string) {
		if _, err := usageService.RefreshAccount(ctx, accountID); err != nil {
			logger.Warn("quota refresh after OAuth failed", "account_id", accountID, "error", err)
		}
	})
	windowResolver := contextguard.NewWindowResolver(cfg.Context.ModelWindows, nil)
	strategy := cfg.Router.Strategy
	// Env/config wins when LITEROUTER_ROUTER_STRATEGY is set so stability profiles are not
	// silently overridden by a stale UI setting in the DB.
	if _, envSet := os.LookupEnv("LITEROUTER_ROUTER_STRATEGY"); !envSet {
		if stored, strategyErr := store.GetSetting(context.Background(), "router.strategy"); strategyErr == nil && stored != "" {
			strategy = stored
		}
	}
	selector := pool.NewSelector(accountPool, pool.SelectionStrategy(strategy), cfg.Router.ModelAliases)
	oauthInference := pooloauth.NewInference(credentialManager, selector)
	// User-registered upstreams. The registry is loaded before the gateway so a
	// custom provider serves traffic from the first request after a restart.
	customProviders := gateway.NewCustomProviderRegistry(nil)
	reloadCustomProviders := func() {
		definitions, loadErr := store.LoadCustomProviders(context.Background())
		if loadErr != nil {
			logger.Error("load custom providers", "error", loadErr)
			return
		}
		if reloadErr := customProviders.Reload(definitions); reloadErr != nil {
			// Partial failures are expected when a base URL is edited badly; the
			// providers that did load keep serving.
			logger.Warn("custom providers partially loaded", "error", reloadErr)
		}
		logger.Info("custom providers loaded", "prefixes", customProviders.Prefixes())
	}
	reloadCustomProviders()
	gatewayService, err := newGateway(cfg, windowResolver, oauthInference, customProviders, store)
	if err != nil {
		logger.Error("initialize gateway", "error", err)
		return 1
	}
	// Same precedence as router.strategy: the env var wins so a deliberate deployment
	// setting is not silently overridden by a stale dashboard value. Otherwise a stored
	// setting wins over config.yaml — and a nil error means the row exists even when the
	// value is empty, so an override the user cleared in the UI stays cleared instead of
	// reverting to config.yaml on the next restart.
	if _, envSet := os.LookupEnv("LITEROUTER_ROUTER_PLAN_MODEL"); !envSet {
		if stored, storedErr := store.GetSetting(context.Background(), "router.plan_model"); storedErr == nil {
			gatewayService.SetPlanModel(stored)
		}
	}
	if _, envSet := os.LookupEnv("LITEROUTER_ROUTER_COMPACT_MODEL"); !envSet {
		if stored, storedErr := store.GetSetting(context.Background(), "router.compact_model"); storedErr == nil {
			gatewayService.SetCompactModel(stored)
		}
	}
	if _, envSet := os.LookupEnv("LITEROUTER_ROUTER_FALLBACK_MODEL"); !envSet {
		if stored, storedErr := store.GetSetting(context.Background(), "router.fallback_model"); storedErr == nil {
			gatewayService.SetFallbackModel(stored)
		}
	}
	if _, envSet := os.LookupEnv("LITEROUTER_CONTEXT_SUMMARIZE"); !envSet {
		if stored, storedErr := store.GetSetting(context.Background(), "context.summarize"); storedErr == nil {
			if modeErr := gatewayService.SetSummarizeMode(stored); modeErr != nil {
				logger.Warn("ignoring stored summarize mode", "mode", stored, "error", modeErr)
			}
		}
	}
	// The context mode needs both envs unset before a stored value may apply:
	// enabled=false and mode=off express the same thing, so a deployment that pins
	// either one must not be half-overridden by a stale dashboard row.
	_, contextEnabledEnv := os.LookupEnv("LITEROUTER_CONTEXT_ENABLED")
	_, contextModeEnv := os.LookupEnv("LITEROUTER_CONTEXT_MODE")
	if !contextEnabledEnv && !contextModeEnv {
		if stored, storedErr := store.GetSetting(context.Background(), "context.mode"); storedErr == nil && stored != "" {
			if modeErr := gatewayService.SetContextMode(stored); modeErr != nil {
				logger.Warn("stored context mode is invalid; keeping the boot mode", "value", stored)
			}
		}
	}
	// The long-context rule follows the same precedence, and both halves move together:
	// a stored model with no stored percent must not inherit config.yaml's percent, or the
	// threshold would belong to a rule the user replaced.
	if _, envSet := os.LookupEnv("LITEROUTER_ROUTER_LONG_CONTEXT_MODEL"); !envSet {
		if model, storedErr := store.GetSetting(context.Background(), "router.long_context_model"); storedErr == nil {
			percent := 0
			if raw, percentErr := store.GetSetting(context.Background(), "router.long_context_percent"); percentErr == nil {
				percent, _ = strconv.Atoi(raw)
			}
			gatewayService.SetLongContext(model, percent)
		}
	}
	// Same precedence again for the image rule. Both halves are read together so a stored
	// vision model is never paired with config.yaml's text-only list, which would apply the
	// rule to models the user never declared.
	if _, envSet := os.LookupEnv("LITEROUTER_ROUTER_IMAGE_MODEL"); !envSet {
		if model, storedErr := store.GetSetting(context.Background(), "router.image_model"); storedErr == nil {
			textOnly, _ := store.GetSetting(context.Background(), "router.text_only_models")
			gatewayService.SetImageRoute(model, strings.Split(textOnly, ","))
		}
	}
	// Same precedence for the image-transcription toggle. It is meaningless without the
	// image rule, but restored independently so a stored value is not paired with config's.
	if _, envSet := os.LookupEnv("LITEROUTER_ROUTER_BUILD_IMAGE_PROMPT"); !envSet {
		if stored, storedErr := store.GetSetting(context.Background(), "router.build_image_prompt"); storedErr == nil {
			gatewayService.SetBuildImagePrompt(stored == "true")
		}
	}
	gatewayService.SetOnUsage(func(ev gateway.UsageEvent) {
		usageService.EnqueueGatewayUsage(storage.UsageEvent{
			Provider: ev.Provider, Model: ev.Model, Endpoint: ev.Endpoint, Status: ev.Status,
			PromptTokens: ev.PromptTokens, CompletionTokens: ev.CompletionTokens, CachedTokens: ev.CachedTokens,
			PromptTokensEstimated: ev.PromptTokensEstimated, CompletionTokensEstimated: ev.CompletionTokensEstimated,
			CachedTokensReported: ev.CachedTokensReported, Effort: ev.Effort,
		})
	})
	// Synced from local 9router /v1/models (codex=cx, xai). Claude not present there; keep Anthropic defaults.
	seedByProvider := map[string][]string{
		"codex": {
			"cx/gpt-5.3-codex-spark", "cx/gpt-5.3-codex-spark-review",
			"cx/gpt-5.4", "cx/gpt-5.4-review", "cx/gpt-5.4-mini", "cx/gpt-5.4-mini-review",
			"cx/gpt-5.5", "cx/gpt-5.5-review",
			"cx/gpt-5.6-luna", "cx/gpt-5.6-luna-review",
			"cx/gpt-5.6-sol", "cx/gpt-5.6-sol-review",
			"cx/gpt-5.6-terra", "cx/gpt-5.6-terra-review",
		},
		"claude": {
			"claude-opus-4-8", "claude-sonnet-5", "claude-haiku-4-5",
			"claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5-20251001",
		},
		"xai": {
			"xai/grok-3", "xai/grok-4", "xai/grok-4-fast-reasoning", "xai/grok-4.5", "xai/grok-code-fast-1",
		},
		"antigravity": {
			"ag/claude-opus-4-6-thinking", "ag/gpt-oss-120b-medium", "ag/gemini-3-flash",
			"gemini-default", "gemini-3.5-flash-high", "gemini-3.5-flash-medium",
			"gemini-3.5-flash-low", "gemini-3.5-flash-extra-low",
			"gemini-3.1-pro-high", "gemini-3.1-pro-low",
		},
	}
	// Merge gateway-discovered models under inferred provider.
	for _, id := range gatewayService.Models() {
		provider := "codex"
		lower := strings.ToLower(id)
		switch {
		case strings.HasPrefix(lower, "claude"), strings.HasPrefix(lower, "anthropic"):
			provider = "claude"
		case strings.HasPrefix(lower, "grok"), strings.HasPrefix(lower, "xai/"), lower == "xai":
			provider = "xai"
		case strings.HasPrefix(lower, "cx/"):
			provider = "codex"
		}
		seedByProvider[provider] = append(seedByProvider[provider], id)
	}
	for provider, ids := range seedByProvider {
		// de-dupe while preserving order
		seen := map[string]struct{}{}
		uniq := make([]string, 0, len(ids))
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			uniq = append(uniq, id)
		}
		if err := store.EnsureCatalogModels(context.Background(), provider, uniq); err != nil {
			logger.Warn("seed catalog models", "provider", provider, "error", err)
		}
	}
	refreshContextWindows := func(ctx context.Context) error {
		windows, err := store.CatalogContextWindows(ctx)
		if err != nil {
			return err
		}
		windowResolver.ReplaceCatalog(windows)
		// Effort overrides live in the same table and change on the same edits, so they
		// are refreshed together rather than through a second path that could drift.
		efforts, err := store.CatalogEfforts(ctx)
		if err != nil {
			return err
		}
		if gatewayService != nil {
			gatewayService.ReplaceModelEfforts(efforts)
		}
		return nil
	}
	// Load once at boot: the refresh otherwise runs only on a catalog edit, so a
	// restart would serve every request with the overrides missing until someone
	// happened to touch a model.
	if err := refreshContextWindows(context.Background()); err != nil {
		logger.Error("load catalog context windows", "error", err)
		return 1
	}
	apiToken := os.Getenv("LITEROUTER_API_TOKEN")
	if apiToken == "" {
		logger.Error("LITEROUTER_API_TOKEN is required")
		return 1
	}
	uiService, err := ui.New(accountPool, apiToken, func(ctx context.Context, providerName string) (ui.OAuthResult, error) {
		var result pooloauth.StartResult
		var err error
		switch providerName {
		case "codex":
			result, err = oauthManager.StartCodex(ctx)
		case "claude":
			result, err = oauthManager.StartClaude(ctx)
		case "grok", "xai":
			result, err = oauthManager.StartGrok(ctx)
		case "antigravity":
			result, err = oauthManager.StartAntigravity(ctx)
		default:
			return ui.OAuthResult{}, fmt.Errorf("unsupported provider")
		}
		return ui.OAuthResult{
			AuthURL: result.AuthURL, UserCode: result.UserCode, VerificationURI: result.VerificationURI,
			VerificationURIComplete: result.VerificationURIComplete,
		}, err
	}, func(ctx context.Context, accountID string) error {
		_, err := usageService.RefreshAccount(ctx, accountID)
		return err
	}, store.ListQuotaSnapshots, func(ctx context.Context, accountID string, enabled bool, weight int) error {
		if err := store.UpdateAccountRouting(ctx, accountID, enabled, weight); err != nil {
			return err
		}
		pooled, ok := accountPool.Get(accountID)
		if !ok {
			return fmt.Errorf("account not found")
		}
		pooled.Enabled = enabled
		pooled.Weight = weight
		accountPool.Upsert(pooled)
		return nil
	}, func(ctx context.Context, accountID string) error {
		if err := store.DeleteAccount(ctx, accountID); err != nil {
			return err
		}
		accountPool.Remove(accountID)
		return nil
	}, ui.APIKeyHooks{
		List: store.ListAPIKeys, Create: store.CreateAPIKey,
		SetEnabled: store.SetAPIKeyEnabled, Delete: store.DeleteAPIKey,
		Valid: func(token string) bool { return authorizeToken(context.Background(), apiToken, token, store) },
	}, ui.ModelHooks{
		List: store.ListCatalogModels,
		Add: func(ctx context.Context, providerName, id, label string, contextWindow int) (storage.CatalogModel, error) {
			model, err := store.AddCatalogModel(ctx, providerName, id, label, contextWindow)
			if err == nil {
				err = refreshContextWindows(ctx)
			}
			// A model nobody has a figure for is the one case worth spending tokens on
			// unasked: the alternative is the conservative hybrid fallback, which is a
			// guess, and a wrong window is paid for on every turn afterwards — too low
			// compacts sessions that would have been served, too high lets them grow past
			// the point where any compaction fits. When the curated table or the operator
			// supplied a number, that number stands and the card's Measure button is there
			// for whoever wants to check it.
			//
			// Detached from the request so adding a model stays instant; a search uploads
			// real prompts and takes as long as they take.
			if err == nil && model.ContextWindow <= 0 {
				go func() {
					probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
					defer cancel()
					found := gatewayService.SearchContextWindow(probeCtx, id, 0, 0)
					if found.Window <= 0 {
						slog.Warn("could not measure the context window of a newly added model",
							"model", id, "error", found.Error)
						return
					}
					if setErr := store.SetCatalogContextWindow(probeCtx, providerName, id, found.Window); setErr != nil {
						slog.Warn("persist measured context window", "model", id, "error", setErr)
						return
					}
					if refreshErr := refreshContextWindows(probeCtx); refreshErr != nil {
						slog.Warn("refresh context windows after measuring", "model", id, "error", refreshErr)
					}
					slog.Info("measured the context window of a newly added model",
						"model", id, "context_window", found.Window, "tokens_spent", found.TokensSpent)
				}()
			}
			return model, err
		},
		SetEffort: func(ctx context.Context, providerName, id, effort string) error {
			if err := store.SetCatalogEffort(ctx, providerName, id, effort); err != nil {
				return err
			}
			return refreshContextWindows(ctx)
		},
		SetContextWindow: func(ctx context.Context, providerName, id string, window int) error {
			if err := store.SetCatalogContextWindow(ctx, providerName, id, window); err != nil {
				return err
			}
			return refreshContextWindows(ctx)
		},
		Window: func(ctx context.Context, id string) ui.ModelWindow {
			window, served, refused := gatewayService.ContextWindowEvidence(ctx, id)
			return ui.ModelWindow{Effective: window, Served: served, Refused: refused}
		},
		ProbeContext: func(ctx context.Context, providerName, id string, tokens int, onStep func(ui.ContextProbeStep)) (ui.ContextProbeResult, error) {
			report := func(step gateway.ContextProbe) {
				if onStep == nil {
					return
				}
				onStep(ui.ContextProbeStep{
					Tokens: step.Tokens, Reported: step.Reported, Accepted: step.Accepted, Refused: step.Refused,
					TimedOut: step.TimedOut, Started: step.Started, Attempt: step.Attempt,
					Duration: step.Duration, Error: step.Error,
				})
			}
			out := ui.ContextProbeResult{Model: id}
			if tokens > 0 {
				// One size, one answer. The cheap option, and the only honest way to
				// settle "does this model take N tokens" — everything else is inference.
				// Announced before it is sent, for the same reason the search does: one
				// sized probe against a large window is seconds of silence otherwise.
				report(gateway.ContextProbe{Tokens: tokens, Started: true, Attempt: 1})
				probe := gatewayService.ProbeContextWindow(ctx, id, tokens)
				report(probe)
				out.Steps, out.TokensSpent, out.Error = 1, probe.Effective(), probe.Error
				if !probe.Accepted {
					if probe.Refused {
						out.SmallestRefused = tokens
					}
					return out, nil
				}
				// The upstream's own count, not the size aimed at. They differ — filler
				// never tokenizes exactly like the traffic the calibration was learned
				// from — and only one of them is a fact about this request.
				out.Window, out.LargestAccepted = tokens, tokens
				if probe.Reported > 0 {
					out.Window, out.LargestAccepted, out.TokensSpent = probe.Reported, probe.Reported, probe.Reported
				}
			} else {
				found := gatewayService.SearchContextWindowStreaming(ctx, id, 0, 0, report)
				out.Window, out.LargestAccepted = found.Window, found.LargestAccepted
				out.SmallestRefused, out.Steps = found.SmallestRefused, len(found.Steps)
				out.TokensSpent, out.Error = found.TokensSpent, found.Error
			}
			// Writing it back is the point: a number the dashboard reports but the guard
			// does not budget against is worse than no number, because it reads as
			// settled. The gateway already holds it as a floor; this makes it survive a
			// restart and puts it where the CLI setup card reads from.
			//
			// Only ever upward. What an accepted probe proves is "at least this much",
			// never "no more than this" — and the two are easy to confuse, because the
			// upstream reports the size it actually counted, which is smaller than the
			// size the filler was aimed at. Measured live: a probe aimed at 300,000 was
			// served and counted as 237,766, and writing that back unconditionally
			// lowered a catalogued 256,000 on the strength of a request that had
			// succeeded. A refusal is what bounds a window from above, and a refusal
			// never reaches this branch.
			// An empty provider means "measure only": the add-model form asks before the
			// catalogue row exists, so there is nothing to write to and the number is for
			// the form field.
			if out.Window > 0 && providerName != "" {
				if believed := gatewayService.ContextWindowFor(ctx, id); out.Window <= believed {
					out.Error = fmt.Sprintf("kept the existing %d: the probe only proves %d was served",
						believed, out.Window)
					return out, nil
				}
				if err := store.SetCatalogContextWindow(ctx, providerName, id, out.Window); err != nil {
					return out, err
				}
				if err := refreshContextWindows(ctx); err != nil {
					return out, err
				}
				out.Saved = true
			}
			return out, nil
		},
		Delete: func(ctx context.Context, providerName, id string) error {
			if err := store.DeleteCatalogModel(ctx, providerName, id); err != nil {
				return err
			}
			return refreshContextWindows(ctx)
		},
		Test: func(ctx context.Context, modelID string) (ui.ModelTestResult, error) {
			start := time.Now()
			// A custom provider has no OAuth account, so the pool probe cannot serve it.
			// Without this the test fell through to the Codex default and reported
			// "model is not supported when using Codex with a ChatGPT account" for a
			// model that routes perfectly well.
			if prefix, ok := customProviders.PrefixFor(modelID); ok {
				temp := 0.0
				resp, err := gatewayService.Chat(ctx, translator.OpenAIRequest{
					Model:     modelID,
					Messages:  []translator.OpenAIMessage{{Role: "user", Content: "Reply with exactly: pong"}},
					MaxTokens: 8, Temperature: &temp,
				})
				if err != nil {
					return ui.ModelTestResult{OK: false, Model: modelID,
						Latency: time.Since(start).Round(time.Millisecond).String(),
						Error:   err.Error(), Provider: "custom:" + prefix}, nil
				}
				preview := ""
				if len(resp.Choices) > 0 {
					switch value := resp.Choices[0].Message.Content.(type) {
					case string:
						preview = value
					default:
						preview = fmt.Sprint(value)
					}
				}
				preview = strings.Join(strings.Fields(strings.TrimSpace(preview)), " ")
				if preview == "" {
					preview = "(empty response)"
				}
				return ui.ModelTestResult{OK: true, Model: modelID,
					Latency: time.Since(start).Round(time.Millisecond).String(),
					Preview: preview, Provider: "custom:" + prefix}, nil
			}
			provider := "codex"
			lower := strings.ToLower(modelID)
			switch {
			case strings.HasPrefix(lower, "claude"), strings.HasPrefix(lower, "anthropic"):
				provider = "claude"
			case strings.HasPrefix(lower, "grok"), strings.HasPrefix(lower, "xai/"), lower == "xai":
				provider = "grok"
			case strings.HasPrefix(lower, "cx/"), strings.HasPrefix(lower, "gpt-"), strings.HasPrefix(lower, "o1"), strings.HasPrefix(lower, "o3"), strings.HasPrefix(lower, "o4"):
				provider = "codex"
			}
			// Prefer connected OAuth session from the account pool.
			if accountID, preview, chatUsage, err := credentialManager.ChatTest(ctx, accountPool, provider, modelID); err == nil {
				prov := provider
				if prov == "grok" {
					prov = "xai"
				}
				usageService.RecordGatewayUsage(ctx, storage.UsageEvent{
					Provider: prov, Model: modelID, Endpoint: "/ui/models/test", Status: "ok",
					PromptTokens: chatUsage.PromptTokens, CompletionTokens: chatUsage.CompletionTokens, CachedTokens: chatUsage.CachedTokens,
				})
				return ui.ModelTestResult{
					OK: true, Model: modelID, Latency: time.Since(start).Round(time.Millisecond).String(),
					Preview: preview, Provider: accountID,
				}, nil
			} else {
				oauthErr := err
				// Fallback to API-key gateway when configured.
				upstream := modelID
				switch {
				case strings.HasPrefix(upstream, "cx/"):
					upstream = strings.TrimPrefix(upstream, "cx/")
				case strings.HasPrefix(upstream, "xai/"):
					upstream = strings.TrimPrefix(upstream, "xai/")
				}
				maxTokens := 8
				temp := 0.0
				resp, gerr := gatewayService.Chat(ctx, translator.OpenAIRequest{
					Model: upstream,
					Messages: []translator.OpenAIMessage{{
						Role: "user", Content: "Reply with exactly: pong",
					}},
					MaxTokens: maxTokens, Temperature: &temp,
				})
				latency := time.Since(start).Round(time.Millisecond).String()
				if gerr == nil {
					preview := ""
					if len(resp.Choices) > 0 {
						switch v := resp.Choices[0].Message.Content.(type) {
						case string:
							preview = v
						default:
							preview = fmt.Sprint(v)
						}
					}
					preview = strings.Join(strings.Fields(strings.TrimSpace(preview)), " ")
					if preview == "" {
						preview = "(empty response)"
					}
					return ui.ModelTestResult{OK: true, Model: resp.Model, Latency: latency, Preview: preview}, nil
				}
				// Surface the OAuth error first — that's what the dashboard expects.
				msg := oauthErr.Error()
				if strings.Contains(gerr.Error(), "provider is not configured") {
					// keep oauth message
				} else {
					msg = msg + "; api-key fallback: " + gerr.Error()
				}
				return ui.ModelTestResult{OK: false, Model: modelID, Latency: latency, Error: msg}, nil
			}
		}}, ui.SettingsHooks{
		GetStrategy: func(ctx context.Context) (string, error) {
			value, err := store.GetSetting(ctx, "router.strategy")
			if err != nil || value == "" {
				return cfg.Router.Strategy, nil
			}
			return value, nil
		},
		SetStrategy: func(ctx context.Context, strategy string) error {
			if err := store.SetSetting(ctx, "router.strategy", strategy); err != nil {
				return err
			}
			selector.SetStrategy(pool.SelectionStrategy(strategy))
			return nil
		},
		GetPlanModel: func(context.Context) (string, error) {
			// From the gateway rather than the DB: it is what is actually in force, and
			// it already reflects the config/env precedence applied at startup.
			return gatewayService.PlanModel(), nil
		},
		SetPlanModel: func(ctx context.Context, model string) error {
			if err := store.SetSetting(ctx, "router.plan_model", model); err != nil {
				return err
			}
			gatewayService.SetPlanModel(model)
			return nil
		},
		GetSummarizeMode: func(context.Context) (string, error) {
			return gatewayService.SummarizeMode(), nil
		},
		SetSummarizeMode: func(ctx context.Context, mode string) error {
			if err := gatewayService.SetSummarizeMode(mode); err != nil {
				return err
			}
			return store.SetSetting(ctx, "context.summarize", mode)
		},
		GetFallbackModel: func(context.Context) (string, error) {
			// Same contract as GetPlanModel: the in-force value, already carrying the
			// env-wins precedence applied at startup.
			return gatewayService.FallbackModel(), nil
		},
		SetFallbackModel: func(ctx context.Context, model string) error {
			if err := store.SetSetting(ctx, "router.fallback_model", model); err != nil {
				return err
			}
			gatewayService.SetFallbackModel(model)
			return nil
		},
		GetCompactModel: func(context.Context) (string, error) {
			// Same contract as GetPlanModel: the in-force value, already carrying the
			// env-wins precedence applied at startup.
			return gatewayService.CompactModel(), nil
		},
		SetCompactModel: func(ctx context.Context, model string) error {
			if err := store.SetSetting(ctx, "router.compact_model", model); err != nil {
				return err
			}
			gatewayService.SetCompactModel(model)
			return nil
		},
		GetContextMode: func(context.Context) (string, error) {
			return gatewayService.ContextMode(), nil
		},
		SetContextMode: func(ctx context.Context, mode string) error {
			if err := gatewayService.SetContextMode(mode); err != nil {
				return err
			}
			return store.SetSetting(ctx, "context.mode", mode)
		},
		GetCLIDraft: func(ctx context.Context) (clisetup.Draft, error) {
			raw, err := store.GetSetting(ctx, "clisetup.claude")
			if err != nil || raw == "" {
				// A missing row is the normal state before the first apply, not a failure.
				return clisetup.Draft{}, nil
			}
			var draft clisetup.Draft
			if err := json.Unmarshal([]byte(raw), &draft); err != nil {
				return clisetup.Draft{}, err
			}
			return draft, nil
		},
		SetCLIDraft: func(ctx context.Context, draft clisetup.Draft) error {
			encoded, err := json.Marshal(draft)
			if err != nil {
				return err
			}
			return store.SetSetting(ctx, "clisetup.claude", string(encoded))
		},
		GetLongContext: func(ctx context.Context) (string, int, error) {
			model, err := store.GetSetting(ctx, "router.long_context_model")
			if err != nil {
				// No row yet is the normal state before the rule is first saved.
				return "", 0, nil
			}
			percent := 0
			if raw, percentErr := store.GetSetting(ctx, "router.long_context_percent"); percentErr == nil {
				percent, _ = strconv.Atoi(raw)
			}
			return model, percent, nil
		},
		SetLongContext: func(ctx context.Context, model string, percent int) error {
			if err := store.SetSetting(ctx, "router.long_context_model", model); err != nil {
				return err
			}
			if err := store.SetSetting(ctx, "router.long_context_percent", strconv.Itoa(percent)); err != nil {
				return err
			}
			// Applied to the running gateway, so the rule takes effect without a restart —
			// the same contract SetPlanModel has.
			gatewayService.SetLongContext(model, percent)
			return nil
		},
		GetImageRoute: func(ctx context.Context) (string, string, error) {
			model, err := store.GetSetting(ctx, "router.image_model")
			if err != nil {
				// No row yet is the normal state before the rule is first saved.
				return "", "", nil
			}
			textOnly, _ := store.GetSetting(ctx, "router.text_only_models")
			return model, textOnly, nil
		},
		SetImageRoute: func(ctx context.Context, model, textOnly string) error {
			if err := store.SetSetting(ctx, "router.image_model", model); err != nil {
				return err
			}
			if err := store.SetSetting(ctx, "router.text_only_models", textOnly); err != nil {
				return err
			}
			gatewayService.SetImageRoute(model, strings.Split(textOnly, ","))
			return nil
		},
		GetBuildImagePrompt: func(ctx context.Context) (bool, error) {
			stored, err := store.GetSetting(ctx, "router.build_image_prompt")
			if err != nil {
				return false, nil
			}
			return stored == "true", nil
		},
		SetBuildImagePrompt: func(ctx context.Context, enabled bool) error {
			value := "false"
			if enabled {
				value = "true"
			}
			if err := store.SetSetting(ctx, "router.build_image_prompt", value); err != nil {
				return err
			}
			gatewayService.SetBuildImagePrompt(enabled)
			return nil
		},
		GetAntigravityCredentials: func(ctx context.Context) (string, string, error) {
			clientID, err := store.GetSetting(ctx, "antigravity.client_id")
			logger.Info("GetAntigravityCredentials", "client_id", clientID, "err", err)
			if err != nil {
				return "", "", nil
			}
			secret, _ := store.GetSetting(ctx, "antigravity.client_secret")
			return clientID, secret, nil
		},
		SetAntigravityCredentials: func(ctx context.Context, clientID, secret string) error {
			if err := store.SetSetting(ctx, "antigravity.client_id", clientID); err != nil {
				return err
			}
			if err := store.SetSetting(ctx, "antigravity.client_secret", secret); err != nil {
				return err
			}
			antigravityProvider.SetCredentials(clientID, secret)
			return nil
		},
		ContextCeiling: gatewayService.ClientContextCeiling,
	}, ui.UsageHooks{
		Summary: usageService.UsageSummary,
	}, gatewayService.Models()...)
	if err != nil {
		logger.Error("initialize UI", "error", err)
		return 1
	}
	uiService.SetCustomProviderHooks(ui.CustomProviderHooks{
		List:      store.ListCustomProviders,
		Create:    store.CreateCustomProvider,
		Update:    store.UpdateCustomProvider,
		Delete:    store.DeleteCustomProvider,
		AddKey:    store.AddCustomProviderKey,
		ToggleKey: store.SetCustomProviderKeyEnabled,
		DeleteKey: store.DeleteCustomProviderKey,
		Reload:    reloadCustomProviders,
	})
	uiService.SetResetCodex(func(ctx context.Context, accountID string) error {
		_, err := usageService.ResetCodexSession(ctx, accountID)
		return err
	})
	uiService.SetCompleteOAuth(func(ctx context.Context, provider, raw string) error {
		_, err := oauthManager.CompleteManualCallback(ctx, provider, raw)
		return err
	})
	backgroundContext, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()
	go credentialManager.Run(backgroundContext, cfg.OAuth.RefreshInterval)
	go usageService.Run(backgroundContext, time.Minute)
	go usageService.RunRetention(backgroundContext, cfg.Storage.UsageRetention)
	go store.RunAPIKeyMaintenance(backgroundContext)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	e.HEAD("/", func(c echo.Context) error {
		return c.NoContent(http.StatusNoContent)
	})
	if err := uiService.Register(e); err != nil {
		logger.Error("register UI", "error", err)
		return 1
	}
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			path := c.Request().URL.Path
			// Local dashboard is same-origin protected (CSRF). API routes still need a token.
			if path == "/" && c.Request().Method == http.MethodHead || path == "/health" || path == "/ui" || strings.HasPrefix(path, "/ui/") {
				return next(c)
			}
			provided := strings.TrimSpace(c.Request().Header.Get("X-Api-Key"))
			if authorization := c.Request().Header.Get("Authorization"); provided == "" && strings.HasPrefix(authorization, "Bearer ") {
				provided = strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
			}
			if !authorizeToken(c.Request().Context(), apiToken, provided, store) {
				if strings.HasPrefix(path, "/v1/messages") || strings.HasPrefix(path, "/v1/models") {
					return c.JSON(http.StatusUnauthorized, map[string]any{
						"type": "error", "error": map[string]string{"type": "authentication_error", "message": "unauthorized"},
					})
				}
				return echo.NewHTTPError(http.StatusUnauthorized, "unauthorized")
			}
			return next(c)
		}
	})
	recommendationService := recommendation.New(recommendation.Config{
		Pool:          accountPool,
		Catalog:       store.ListCatalogModels,
		GatewayModels: gatewayService.Models(),
		APIKeyProviders: map[string]bool{
			"codex": os.Getenv("OPENAI_API_KEY") != "",
			"xai":   os.Getenv("XAI_API_KEY") != "",
		},
	})
	gatewayService.Register(e)
	recommendationService.Register(e)
	e.POST("/api/oauth/start", func(c echo.Context) error {
		var request struct {
			Provider string `json:"provider"`
		}
		if err := c.Bind(&request); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
		}
		var (
			result pooloauth.StartResult
			err    error
		)
		switch request.Provider {
		case "codex":
			result, err = oauthManager.StartCodex(c.Request().Context())
		case "claude":
			result, err = oauthManager.StartClaude(c.Request().Context())
		case "grok", "xai":
			result, err = oauthManager.StartGrok(c.Request().Context())
		case "antigravity":
			result, err = oauthManager.StartAntigravity(c.Request().Context())
		default:
			return echo.NewHTTPError(http.StatusBadRequest, "provider must be codex, claude, xai, or antigravity")
		}
		if err != nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, err.Error())
		}
		return c.JSON(http.StatusOK, result)
	})
	e.POST("/api/oauth/codex/import", func(c echo.Context) error {
		var token pooloauth.TokenSet
		if err := c.Bind(&token); err != nil || token.AccessToken == "" || token.IDToken == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "access_token and id_token are required")
		}
		account, err := oauthManager.ImportCodex(c.Request().Context(), token)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"id": account.ID, "provider": account.Provider, "label": account.Label})
	})
	e.POST("/api/oauth/codex/import-cli", func(c echo.Context) error {
		account, err := oauthManager.ImportCodexCLI(c.Request().Context(), defaultCodexAuthPath())
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"id": account.ID, "provider": account.Provider, "label": account.Label})
	})
	e.GET("/api/accounts", func(c echo.Context) error {
		return c.JSON(http.StatusOK, accountPool.List())
	})
	e.GET("/api/accounts/:id/quota", func(c echo.Context) error {
		accountID := c.Param("id")
		if _, ok := accountPool.Get(accountID); !ok {
			return echo.NewHTTPError(http.StatusNotFound, "account not found")
		}
		refresh := c.QueryParam("refresh") == "true"
		if refresh {
			quota, err := usageService.RefreshAccount(c.Request().Context(), accountID)
			if err != nil {
				return echo.NewHTTPError(http.StatusBadGateway, err.Error())
			}
			return c.JSON(http.StatusOK, quota)
		}
		snapshots, err := store.ListQuotaSnapshots(c.Request().Context(), accountID)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, snapshots)
	})

	server := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           e,
		ReadHeaderTimeout: cfg.Server.ReadTimeout,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
	}

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	logger.Info("server started", "addr", cfg.Server.Addr, "accounts", accountPool.Len())

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped unexpectedly", "error", err)
			return 1
		}
		return 0
	case <-signalContext.Done():
		logger.Info("shutdown requested")
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := oauthManager.Close(shutdownContext); err != nil {
		logger.Warn("OAuth callback shutdown failed", "error", err)
	}
	if err := server.Shutdown(shutdownContext); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		_ = server.Close()
		return 1
	}
	if err := usageService.Close(shutdownContext); err != nil {
		logger.Warn("flush usage events", "error", err)
	}
	stopBackground()
	if err := store.FlushAPIKeyUsage(shutdownContext); err != nil {
		logger.Warn("flush API key usage", "error", err)
	}

	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server shutdown failed", "error", err)
		return 1
	}
	logger.Info("server stopped")
	return 0
}

func restoreQuotaSnapshots(ctx context.Context, store *storage.Store, accountPool *pool.Pool, accounts []storage.Account) error {
	for _, account := range accounts {
		snapshots, err := store.ListQuotaSnapshots(ctx, account.ID)
		if err != nil {
			return err
		}
		poolSnapshots := make([]pool.QuotaSnapshot, 0, len(snapshots))
		for _, snapshot := range snapshots {
			poolSnapshots = append(poolSnapshots, pool.QuotaSnapshot{
				Key:              snapshot.Key,
				Remaining:        snapshot.Remaining,
				RemainingPercent: snapshot.RemainingPercent,
				ResetAt:          snapshot.ResetAt,
				FetchedAt:        snapshot.FetchedAt,
				Unlimited:        snapshot.Unlimited,
				Exhausted:        snapshot.Exhausted,
			})
		}
		accountPool.RestoreQuota(account.ID, poolSnapshots)
	}
	return nil
}

func poolAccounts(accounts []storage.Account) []pool.Account {
	result := make([]pool.Account, 0, len(accounts))
	for _, account := range accounts {
		result = append(result, pool.Account{
			ID:             account.ID,
			Provider:       account.Provider,
			Label:          account.Label,
			Plan:           account.Plan,
			Enabled:        account.Enabled,
			Weight:         account.Weight,
			DisabledReason: account.DisabledReason,
		})
	}
	return result
}

func defaultCodexAuthPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex/auth.json"
	}
	return filepath.Join(home, ".codex", "auth.json")
}

func configureCodex() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("user home dir: %w", err)
	}
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0755); err != nil {
		return fmt.Errorf("mkdir .codex: %w", err)
	}
	configPath := filepath.Join(codexDir, "config.toml")

	content := ""
	if b, err := os.ReadFile(configPath); err == nil {
		content = string(b)
	}

	lines := strings.Split(content, "\n")
	var newLines []string
	inBlock := false
	skipSection := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "# BEGIN LITEROUTER" || trimmed == "# BEGIN LITEROUTER PROVIDER" {
			inBlock = true
			continue
		}
		if trimmed == "# END LITEROUTER" || trimmed == "# END LITEROUTER PROVIDER" {
			inBlock = false
			continue
		}
		if inBlock {
			continue
		}

		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if trimmed == "[model_providers.literouter]" || trimmed == "[agents.subagent]" {
				skipSection = true
				continue
			} else {
				skipSection = false
			}
		}

		if skipSection {
			continue
		}

		if strings.HasPrefix(trimmed, "model =") || strings.HasPrefix(trimmed, "model_provider =") || strings.HasPrefix(trimmed, "model_reasoning_effort =") {
			continue
		}

		newLines = append(newLines, line)
	}

	filteredContent := strings.TrimSpace(strings.Join(newLines, "\n"))

	literouterGlobal := `model = "cx/gpt-5.6-sol"
model_provider = "literouter"
model_reasoning_effort = "medium"`

	literouterProvider := `[model_providers.literouter]
name = "LiteRouter"
base_url = "http://127.0.0.1:8317/v1"
wire_api = "responses"

[agents.subagent]
model = "opencode/deepseek-v4-flash"`

	var result string
	if filteredContent == "" {
		result = fmt.Sprintf("# BEGIN LITEROUTER\n%s\n# END LITEROUTER\n\n# BEGIN LITEROUTER PROVIDER\n%s\n# END LITEROUTER PROVIDER\n", literouterGlobal, literouterProvider)
	} else {
		firstSectionIdx := -1
		fLines := strings.Split(filteredContent, "\n")
		for idx, l := range fLines {
			tr := strings.TrimSpace(l)
			if strings.HasPrefix(tr, "[") && strings.HasSuffix(tr, "]") {
				firstSectionIdx = idx
				break
			}
		}

		if firstSectionIdx != -1 {
			before := strings.Join(fLines[:firstSectionIdx], "\n")
			after := strings.Join(fLines[firstSectionIdx:], "\n")
			cleanBefore := strings.TrimSpace(before)
			if cleanBefore != "" {
				result = fmt.Sprintf("# BEGIN LITEROUTER\n%s\n# END LITEROUTER\n\n%s\n\n%s\n\n# BEGIN LITEROUTER PROVIDER\n%s\n# END LITEROUTER PROVIDER\n",
					literouterGlobal, cleanBefore, after, literouterProvider)
			} else {
				result = fmt.Sprintf("# BEGIN LITEROUTER\n%s\n# END LITEROUTER\n\n%s\n\n# BEGIN LITEROUTER PROVIDER\n%s\n# END LITEROUTER PROVIDER\n",
					literouterGlobal, after, literouterProvider)
			}
		} else {
			result = fmt.Sprintf("# BEGIN LITEROUTER\n%s\n# END LITEROUTER\n\n%s\n\n# BEGIN LITEROUTER PROVIDER\n%s\n# END LITEROUTER PROVIDER\n",
				literouterGlobal, filteredContent, literouterProvider)
		}
	}

	return os.WriteFile(configPath, []byte(strings.TrimSpace(result)+"\n"), 0644)
}

func authorizeToken(ctx context.Context, master, provided string, store *storage.Store) bool {
	if provided == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(master)) == 1 {
		return true
	}
	return store != nil && store.ValidAPIKey(ctx, provided)
}

func newGateway(cfg config.Config, windowResolver *contextguard.WindowResolver, oauthInference gateway.OAuthInference,
	customProviders *gateway.CustomProviderRegistry, store *storage.Store) (*gateway.Service, error) {
	var openAIClient, xaiClient gateway.JSONClient
	var openAIStream, xaiStream gateway.StreamClient
	if apiKey := os.Getenv("OPENAI_API_KEY"); apiKey != "" {
		client, err := provider.NewOpenAICompatibleClient("openai", cfg.Providers.OpenAI.BaseURL, apiKey, nil)
		if err != nil {
			return nil, err
		}
		openAIClient, openAIStream = client, client
	}
	if apiKey := os.Getenv("XAI_API_KEY"); apiKey != "" {
		client, err := provider.NewOpenAICompatibleClient("xai", cfg.Providers.XAI.BaseURL, apiKey, nil)
		if err != nil {
			return nil, err
		}
		xaiClient, xaiStream = client, client
	}
	models := make([]string, 0, len(cfg.Router.ModelAliases))
	for alias := range cfg.Router.ModelAliases {
		models = append(models, alias)
	}
	sort.Strings(models)
	var responseCache *cache.ResponseCache
	if cfg.Cache.ResponseMaxEntries > 0 {
		responseCache = cache.NewResponseCache(cfg.Cache.ResponseMaxEntries, cfg.Cache.ResponseTTL)
	}
	// Reference store for tool results elided by aggressive context truncation. The
	// full bodies are kept in memory (capped), addressed by content hash, and served
	// back through GET /ref/<hash> so the model can recover a truncated block without
	// re-running the tool. Always-on: it is small, bounded, and the fetch path is the
	// only consumer.
	toolRefStore := toolstore.New(toolstoreDefaultMaxBytes)
	// Output caps discovered by earlier runs. A failure here only costs the gateway the
	// one rejection it takes to rediscover them, so it must not stop startup.
	learnedOutputTokens, err := store.CatalogMaxOutputTokens(context.Background())
	if err != nil {
		slog.Warn("load learned output limits", "error", err)
		learnedOutputTokens = nil
	}
	// Tokenizer calibrations measured by earlier runs. Same contract: losing them
	// only means the ratios are re-measured from live traffic.
	var learnedCalibrations []gateway.TokenCalibration
	if stored, calErr := store.ListModelCalibrations(context.Background()); calErr != nil {
		slog.Warn("load model calibrations", "error", calErr)
	} else {
		for _, cal := range stored {
			learnedCalibrations = append(learnedCalibrations, gateway.TokenCalibration{
				Model: cal.Model, BytesPerToken: cal.BytesPerToken, EstimatePerToken: cal.EstimatePerToken,
				Spread: cal.Spread, Samples: cal.Samples,
			})
		}
		if len(learnedCalibrations) > 0 {
			slog.Info("seeded tokenizer calibrations from previous runs", "models", len(learnedCalibrations))
		}
	}
	// The largest prompt each model has actually been served, as the upstream counted it.
	// Same contract again: losing it only means the guard starts from the catalogue's
	// figure, which is where it started before this existed.
	observedPrompts, err := store.LargestServedPrompts(context.Background())
	if err != nil {
		slog.Warn("load largest served prompts", "error", err)
		observedPrompts = nil
	}
	return gateway.New(gateway.Options{
		OpenAI: openAIClient, XAI: xaiClient, OpenAIStream: openAIStream, XAIStream: xaiStream,
		OAuthInference:  oauthInference,
		CustomProviders: customProviders,
		TouchCustomKey: func(keyID string) {
			// Best effort: a failed bookkeeping write must not fail the request it
			// belongs to, so the error is dropped rather than propagated.
			_ = store.TouchCustomProviderKey(context.Background(), keyID)
		},
		ResponseCache:   responseCache,
		CompressionMode: cache.CompressionMode(cfg.Cache.CompressionMode), PromptMinBytes: cfg.Cache.PromptCacheMinBytes,
		XAIPromptCache: cfg.Cache.XAIPromptCache,
		Models:         models, Aliases: cfg.Router.ModelAliases,
		ToolStore:           toolRefStore,
		PlanModel:           cfg.Router.PlanModel,
		CompactModel:        cfg.Router.CompactModel,
		FallbackModel:       cfg.Router.FallbackModel,
		LongContextModel:    cfg.Router.LongContextModel,
		LongContextPercent:  cfg.Router.LongContextPercent,
		ImageModel:          cfg.Router.ImageModel,
		TextOnlyModels:      cfg.Router.TextOnlyModels,
		BuildImagePrompt:    cfg.Router.BuildImagePrompt,
		MaxOutputTokens:     cfg.Router.MaxOutputTokens,
		LearnedOutputTokens: learnedOutputTokens,
		LearnedCalibrations: learnedCalibrations,
		ObservedPrompts:     observedPrompts,
		OnCalibration: func(cal gateway.TokenCalibration) {
			// Runs on the request path; the in-memory scale is already in effect, so
			// this write only decides whether the next start remembers it.
			if err := store.UpsertModelCalibration(context.Background(), storage.ModelCalibration{
				Model: cal.Model, BytesPerToken: cal.BytesPerToken, EstimatePerToken: cal.EstimatePerToken,
				Spread: cal.Spread, Samples: cal.Samples,
			}); err != nil {
				slog.Warn("persist model calibration", "model", cal.Model, "error", err)
			}
		},
		OnOutputLimit: func(model string, limit int) {
			// Runs on the request path. The in-memory cap is already in effect, so this
			// write only decides whether the next process start remembers it.
			if err := store.RecordCatalogMaxOutputTokens(context.Background(), model, limit); err != nil {
				slog.Warn("persist learned output limit", "model", model, "max_tokens", limit, "error", err)
			}
		},
		OnContextWindow: func(model string, window int) {
			// Same contract as OnOutputLimit: the gateway is already using the value, so
			// this write decides whether it survives a restart. Refreshing the resolver
			// after it is what lets the dashboard and the guard agree without a reboot.
			if err := store.RecordCatalogContextWindow(context.Background(), model, window); err != nil {
				slog.Warn("persist learned context window", "model", model, "context_window", window, "error", err)
				return
			}
			windows, err := store.CatalogContextWindows(context.Background())
			if err != nil {
				slog.Warn("refresh context windows after learning", "model", model, "error", err)
				return
			}
			windowResolver.ReplaceCatalog(windows)
		},
		ContextEnabled: cfg.Context.Enabled,
		ContextMode:    cfg.Context.Mode,
		SummarizeMode:  cfg.Context.Summarize,
		ContextGuard:   cfg.Context.GuardEnabled,
		ContextLimits:  contextguard.Limits{Default: cfg.Context.DefaultWindow, Models: cfg.Context.ModelWindows},
		ContextWindow: func(_ context.Context, model string) (int, error) {
			if windowResolver == nil {
				return 0, nil
			}
			return windowResolver.Window(model), nil
		},
		ContextPolicy: contextguard.Policy{
			SoftRatio: cfg.Context.SoftRatio, SummarizeRatio: cfg.Context.SummarizeRatio, HardRatio: cfg.Context.HardRatio,
			KeepRecentTurns: cfg.Context.KeepRecentTurns, ReserveTokens: cfg.Context.ReserveTokens,
			// The quantized boundary keeps the compacted prefix byte-stable between
			// grid advances; there is no config knob because turning it off is never
			// the better trade.
			BoundaryQuantum: contextguard.DefaultPolicy().BoundaryQuantum,
		},
		SummaryModel: cfg.Context.SummarizeModel, SummaryMaxTokens: cfg.Context.SummarizeMaxTokens,
		SummaryTimeout: cfg.Context.SummarizeTimeout,
	}), nil
}

func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func checkHealth(addr string) error {
	healthURL, err := healthURL(addr)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: healthcheckTimeout}
	response, err := client.Get(healthURL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	return nil
}

func healthURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("parse server address: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	return (&url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, port),
		Path:   "/health",
	}).String(), nil
}
