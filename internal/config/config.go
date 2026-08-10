package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultAddr                 = "127.0.0.1:8317"
	defaultReadTimeout          = 10 * time.Second
	defaultWriteTimeout         = time.Duration(0)
	defaultIdleTimeout          = 60 * time.Second
	defaultShutdownTimeout      = 15 * time.Second
	defaultStoragePath          = "./data/literouter.db"
	defaultUsageRetention       = 90 * 24 * time.Hour
	defaultRefreshInterval      = time.Minute
	defaultRouterStrategy       = "sticky_soft"
	defaultResponseCacheTTL     = 15 * time.Minute
	defaultResponseCacheEntries = 0 // 0 disables response cache (prefer live upstream)
	defaultCompressionMode      = "safe"
	defaultPromptCacheMinBytes  = 0 // 0 disables injected prompt_cache_key
	// Summarization runs against a live upstream during a request. Large coding
	// contexts routinely need more than the old 20s ceiling before falling back.
	defaultSummarizeTimeout = 60 * time.Second
	defaultOpenAIBaseURL    = "https://api.openai.com/v1"
	defaultXAIBaseURL       = "https://api.x.ai/v1"
	defaultLogLevel         = "info"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Storage   StorageConfig   `yaml:"storage"`
	OAuth     OAuthConfig     `yaml:"oauth"`
	Router    RouterConfig    `yaml:"router"`
	Cache     CacheConfig     `yaml:"cache"`
	Context   ContextConfig   `yaml:"context"`
	Providers ProvidersConfig `yaml:"providers"`
	Log       LogConfig       `yaml:"log"`
}

type ServerConfig struct {
	Addr            string        `yaml:"addr"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	IdleTimeout     time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

type StorageConfig struct {
	Path           string        `yaml:"path"`
	UsageRetention time.Duration `yaml:"usage_retention"`
}

type OAuthConfig struct {
	RefreshInterval time.Duration `yaml:"refresh_interval"`
}

type RouterConfig struct {
	Strategy     string              `yaml:"strategy"`
	ModelAliases map[string][]string `yaml:"model_aliases"`
	// PlanModel serves /v1/messages turns taken while Claude Code's plan mode is
	// active, whatever model the client asked for. It lets a session rest on a cheap
	// model for implementation and still plan on a strong one, without the 200k
	// prompt-token gate that disables the client's own opusplan upgrade.
	PlanModel string `yaml:"plan_model"`
	// CompactModel serves detected Claude Code compact/auto-compact requests — the
	// slowest request a session makes, and one nothing about needs the session
	// model. Routed at medium effort. Empty disables the detection entirely.
	CompactModel string `yaml:"compact_model"`
	// LongContextModel serves a turn whose prompt is too large a share of the window
	// belonging to the model that would otherwise take it — the routing equivalent of
	// claude-code-router's `longContext`. Empty disables the rule.
	//
	// It is a routing decision, not a recovery: the alternatives are the client
	// compacting or the gateway trimming, and both of those lose conversation.
	LongContextModel string `yaml:"long_context_model"`
	// LongContextPercent is that share, 1..99. Zero takes the gateway's default.
	//
	// A share rather than claude-code-router's absolute longContextThreshold, because one
	// token count cannot be right for two models at once: 60,000 is most of a 128k window
	// and a quarter of a 400k one.
	LongContextPercent int `yaml:"long_context_percent"`
	// ImageModel serves a turn carrying an image when the model that would otherwise take
	// it appears in TextOnlyModels. Empty turns such a turn into a clear refusal instead of
	// an upload that the upstream is certain to reject.
	ImageModel string `yaml:"image_model"`
	// TextOnlyModels names the models that cannot read images, by exact id or prefix.
	//
	// Named rather than detected. Vendors publish vision support inconsistently and no
	// model id carries it, so any inference would be a guess — and unlike a wrong window,
	// a wrong guess here either breaks working image turns or silently downgrades good
	// models. Whoever adds a text-only upstream already knows.
	TextOnlyModels []string `yaml:"text_only_models"`
	// MaxOutputTokens caps the max_tokens LiteRouter forwards, per model, keyed by
	// exact id or model prefix like context.model_windows. Claude Code asks for tens
	// of thousands of output tokens on every turn; a model with a smaller cap rejects
	// the request outright rather than answering shorter. Models absent here are not
	// clamped — shortening a model that would have complied is worse than a 400.
	MaxOutputTokens map[string]int `yaml:"max_output_tokens"`
}

type CacheConfig struct {
	ResponseTTL         time.Duration `yaml:"response_ttl"`
	ResponseMaxEntries  int           `yaml:"response_max_entries"`
	CompressionMode     string        `yaml:"compression_mode"`
	PromptCacheMinBytes int           `yaml:"prompt_cache_min_bytes"`
	XAIPromptCache      bool          `yaml:"xai_prompt_cache_key"`
}

type ContextConfig struct {
	Enabled bool `yaml:"enabled"`
	// Mode refines Enabled: "safe" (default) keeps the lossless pipeline,
	// "aggressive" adds superseded-result collapse and head/tail truncation of old
	// bulky tool output. Ignored while Enabled is false.
	Mode               string         `yaml:"mode"`
	GuardEnabled       bool           `yaml:"guard_enabled"`
	DefaultWindow      int            `yaml:"default_window"`
	SoftRatio          float64        `yaml:"soft_ratio"`
	SummarizeRatio     float64        `yaml:"summarize_ratio"`
	HardRatio          float64        `yaml:"hard_ratio"`
	KeepRecentTurns    int            `yaml:"keep_recent_turns"`
	ReserveTokens      int            `yaml:"reserve_tokens"`
	SummarizeModel     string         `yaml:"summarize_model"`
	SummarizeMaxTokens int            `yaml:"summarize_max_tokens"`
	SummarizeTimeout   time.Duration  `yaml:"summarize_timeout"`
	ModelWindows       map[string]int `yaml:"model_windows"`
}

type ProvidersConfig struct {
	OpenAI ProviderConfig `yaml:"openai"`
	XAI    ProviderConfig `yaml:"xai"`
}

type ProviderConfig struct {
	BaseURL string `yaml:"base_url"`
}

type LogConfig struct {
	Level string `yaml:"level"`
}

func Default() Config {
	return Config{
		Server: ServerConfig{
			Addr:            defaultAddr,
			ReadTimeout:     defaultReadTimeout,
			WriteTimeout:    defaultWriteTimeout,
			IdleTimeout:     defaultIdleTimeout,
			ShutdownTimeout: defaultShutdownTimeout,
		},
		Storage: StorageConfig{Path: defaultStoragePath, UsageRetention: defaultUsageRetention},
		OAuth:   OAuthConfig{RefreshInterval: defaultRefreshInterval},
		Router: RouterConfig{Strategy: defaultRouterStrategy, ModelAliases: map[string][]string{},
			MaxOutputTokens: map[string]int{}},
		Cache: CacheConfig{
			ResponseTTL: defaultResponseCacheTTL, ResponseMaxEntries: defaultResponseCacheEntries,
			CompressionMode: defaultCompressionMode, PromptCacheMinBytes: defaultPromptCacheMinBytes,
		},
		Context: ContextConfig{
			Enabled: true, GuardEnabled: true, DefaultWindow: 128_000, SoftRatio: 0.78, SummarizeRatio: 0.88, HardRatio: 0.96,
			KeepRecentTurns: 6, ReserveTokens: 2048, SummarizeMaxTokens: 4000,
			SummarizeTimeout: defaultSummarizeTimeout, ModelWindows: map[string]int{},
		},
		Providers: ProvidersConfig{
			OpenAI: ProviderConfig{BaseURL: defaultOpenAIBaseURL},
			XAI:    ProviderConfig{BaseURL: defaultXAIBaseURL},
		},
		Log: LogConfig{Level: defaultLogLevel},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		if err := loadYAML(path, &cfg); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

func loadYAML(path string, cfg *Config) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config %q: %w", path, err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode config %q: %w", path, err)
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("decode config %q: %w", path, err)
		}
		return fmt.Errorf("decode config %q: multiple YAML documents are not allowed", path)
	}

	return nil
}

func applyEnv(cfg *Config) error {
	if value, ok := os.LookupEnv("LITEROUTER_SERVER_ADDR"); ok {
		cfg.Server.Addr = value
	}

	durations := []struct {
		name   string
		target *time.Duration
	}{
		{"LITEROUTER_SERVER_READ_TIMEOUT", &cfg.Server.ReadTimeout},
		{"LITEROUTER_SERVER_WRITE_TIMEOUT", &cfg.Server.WriteTimeout},
		{"LITEROUTER_SERVER_IDLE_TIMEOUT", &cfg.Server.IdleTimeout},
		{"LITEROUTER_SERVER_SHUTDOWN_TIMEOUT", &cfg.Server.ShutdownTimeout},
		{"LITEROUTER_OAUTH_REFRESH_INTERVAL", &cfg.OAuth.RefreshInterval},
		{"LITEROUTER_STORAGE_USAGE_RETENTION", &cfg.Storage.UsageRetention},
		{"LITEROUTER_CACHE_RESPONSE_TTL", &cfg.Cache.ResponseTTL},
		{"LITEROUTER_CONTEXT_SUMMARIZE_TIMEOUT", &cfg.Context.SummarizeTimeout},
	}
	for _, item := range durations {
		value, ok := os.LookupEnv(item.name)
		if !ok {
			continue
		}
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("parse %s: %w", item.name, err)
		}
		*item.target = duration
	}

	if value, ok := os.LookupEnv("LITEROUTER_STORAGE_PATH"); ok {
		cfg.Storage.Path = value
	}
	if value, ok := os.LookupEnv("LITEROUTER_ROUTER_STRATEGY"); ok {
		cfg.Router.Strategy = value
	}
	if value, ok := os.LookupEnv("LITEROUTER_ROUTER_PLAN_MODEL"); ok {
		cfg.Router.PlanModel = value
	}
	if value, ok := os.LookupEnv("LITEROUTER_ROUTER_COMPACT_MODEL"); ok {
		cfg.Router.CompactModel = value
	}
	if value, ok := os.LookupEnv("LITEROUTER_ROUTER_LONG_CONTEXT_MODEL"); ok {
		cfg.Router.LongContextModel = value
	}
	if value, ok := os.LookupEnv("LITEROUTER_CONTEXT_MODE"); ok {
		cfg.Context.Mode = value
	}
	if value, ok := os.LookupEnv("LITEROUTER_CONTEXT_SUMMARIZE_MODEL"); ok {
		cfg.Context.SummarizeModel = value
	}
	if value, ok := os.LookupEnv("LITEROUTER_ROUTER_IMAGE_MODEL"); ok {
		cfg.Router.ImageModel = value
	}
	if value, ok := os.LookupEnv("LITEROUTER_ROUTER_TEXT_ONLY_MODELS"); ok {
		cfg.Router.TextOnlyModels = splitModelList(value)
	}
	integers := []struct {
		name   string
		target *int
	}{
		{"LITEROUTER_CACHE_RESPONSE_MAX_ENTRIES", &cfg.Cache.ResponseMaxEntries},
		{"LITEROUTER_CACHE_PROMPT_MIN_BYTES", &cfg.Cache.PromptCacheMinBytes},
		{"LITEROUTER_ROUTER_LONG_CONTEXT_PERCENT", &cfg.Router.LongContextPercent},
		{"LITEROUTER_CONTEXT_KEEP_RECENT_TURNS", &cfg.Context.KeepRecentTurns},
		{"LITEROUTER_CONTEXT_SUMMARIZE_MAX_TOKENS", &cfg.Context.SummarizeMaxTokens},
	}
	for _, item := range integers {
		if value, ok := os.LookupEnv(item.name); ok {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("parse %s: %w", item.name, err)
			}
			*item.target = parsed
		}
	}
	floats := []struct {
		name   string
		target *float64
	}{
		{"LITEROUTER_CONTEXT_SOFT_RATIO", &cfg.Context.SoftRatio},
		{"LITEROUTER_CONTEXT_SUMMARIZE_RATIO", &cfg.Context.SummarizeRatio},
		{"LITEROUTER_CONTEXT_HARD_RATIO", &cfg.Context.HardRatio},
	}
	for _, item := range floats {
		if value, ok := os.LookupEnv(item.name); ok {
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("parse %s: %w", item.name, err)
			}
			*item.target = parsed
		}
	}
	if value, ok := os.LookupEnv("LITEROUTER_CACHE_COMPRESSION_MODE"); ok {
		cfg.Cache.CompressionMode = value
	}
	if value, ok := os.LookupEnv("LITEROUTER_CACHE_XAI_PROMPT_CACHE_KEY"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("parse LITEROUTER_CACHE_XAI_PROMPT_CACHE_KEY: %w", err)
		}
		cfg.Cache.XAIPromptCache = parsed
	}
	booleans := []struct {
		name   string
		target *bool
	}{
		{"LITEROUTER_CONTEXT_ENABLED", &cfg.Context.Enabled},
		{"LITEROUTER_CONTEXT_GUARD_ENABLED", &cfg.Context.GuardEnabled},
	}
	for _, item := range booleans {
		if value, ok := os.LookupEnv(item.name); ok {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("parse %s: %w", item.name, err)
			}
			*item.target = parsed
		}
	}
	if value, ok := os.LookupEnv("LITEROUTER_OPENAI_BASE_URL"); ok {
		cfg.Providers.OpenAI.BaseURL = value
	}
	if value, ok := os.LookupEnv("LITEROUTER_XAI_BASE_URL"); ok {
		cfg.Providers.XAI.BaseURL = value
	}
	if value, ok := os.LookupEnv("LITEROUTER_LOG_LEVEL"); ok {
		cfg.Log.Level = value
	}

	return nil
}

func (cfg *Config) Validate() error {
	_, portText, err := net.SplitHostPort(cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("server.addr must use host:port format: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("server.addr port must be between 1 and 65535")
	}

	timeouts := []struct {
		name  string
		value time.Duration
	}{
		{"server.read_timeout", cfg.Server.ReadTimeout},
		{"server.idle_timeout", cfg.Server.IdleTimeout},
		{"server.shutdown_timeout", cfg.Server.ShutdownTimeout},
		{"oauth.refresh_interval", cfg.OAuth.RefreshInterval},
		{"cache.response_ttl", cfg.Cache.ResponseTTL},
	}
	for _, timeout := range timeouts {
		if timeout.value <= 0 {
			return fmt.Errorf("%s must be greater than zero", timeout.name)
		}
	}
	if cfg.Server.WriteTimeout < 0 {
		return fmt.Errorf("server.write_timeout cannot be negative")
	}
	if strings.TrimSpace(cfg.Storage.Path) == "" {
		return fmt.Errorf("storage.path is required")
	}
	if cfg.Storage.UsageRetention < 0 {
		return fmt.Errorf("storage.usage_retention cannot be negative")
	}

	cfg.Router.Strategy = strings.ToLower(cfg.Router.Strategy)
	switch cfg.Router.Strategy {
	case "round_robin", "weighted", "least_used", "least_used_rpm", "sticky", "sticky_soft", "failover", "smart":
	default:
		return fmt.Errorf("router.strategy is invalid")
	}
	cfg.Router.PlanModel = strings.TrimSpace(cfg.Router.PlanModel)
	cfg.Router.CompactModel = strings.TrimSpace(cfg.Router.CompactModel)
	cfg.Router.LongContextModel = strings.TrimSpace(cfg.Router.LongContextModel)
	cfg.Router.ImageModel = strings.TrimSpace(cfg.Router.ImageModel)
	cfg.Router.TextOnlyModels = trimModelList(cfg.Router.TextOnlyModels)
	if cfg.Router.LongContextPercent != 0 && (cfg.Router.LongContextPercent < 1 || cfg.Router.LongContextPercent > 99) {
		return fmt.Errorf("router.long_context_percent must be 0 (default) or between 1 and 99")
	}
	for alias, chain := range cfg.Router.ModelAliases {
		if strings.TrimSpace(alias) == "" || len(chain) == 0 {
			return fmt.Errorf("router.model_aliases must use non-empty aliases and fallback chains")
		}
		for _, model := range chain {
			if strings.TrimSpace(model) == "" {
				return fmt.Errorf("router.model_aliases.%s contains an empty model", alias)
			}
		}
	}
	for model, limit := range cfg.Router.MaxOutputTokens {
		if strings.TrimSpace(model) == "" || limit <= 0 {
			return fmt.Errorf("router.max_output_tokens must use non-empty models and positive limits")
		}
	}
	for name, baseURL := range map[string]string{
		"providers.openai.base_url": cfg.Providers.OpenAI.BaseURL,
		"providers.xai.base_url":    cfg.Providers.XAI.BaseURL,
	} {
		parsed, err := url.Parse(baseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("%s must be an absolute URL", name)
		}
		if parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" {
			return fmt.Errorf("%s must use HTTPS", name)
		}
	}
	if cfg.Context.DefaultWindow <= 0 || cfg.Context.KeepRecentTurns <= 0 || cfg.Context.ReserveTokens < 0 || cfg.Context.SummarizeMaxTokens <= 0 {
		return fmt.Errorf("context windows, recent turns, and summarize max tokens must be positive; reserve cannot be negative")
	}
	if cfg.Context.SummarizeTimeout <= 0 {
		return fmt.Errorf("context.summarize_timeout must be positive")
	}
	if cfg.Context.SoftRatio <= 0 || cfg.Context.SoftRatio >= cfg.Context.SummarizeRatio || cfg.Context.SummarizeRatio >= cfg.Context.HardRatio || cfg.Context.HardRatio > 1 {
		return fmt.Errorf("context ratios must satisfy 0 < soft < summarize < hard <= 1")
	}
	cfg.Context.Mode = strings.ToLower(strings.TrimSpace(cfg.Context.Mode))
	switch cfg.Context.Mode {
	case "", "safe", "aggressive":
	default:
		return fmt.Errorf("context.mode must be safe or aggressive")
	}
	for model, window := range cfg.Context.ModelWindows {
		if strings.TrimSpace(model) == "" || window <= 0 {
			return fmt.Errorf("context.model_windows must use non-empty models and positive windows")
		}
	}
	if cfg.Cache.ResponseMaxEntries < 0 {
		return fmt.Errorf("cache.response_max_entries cannot be negative")
	}
	if cfg.Cache.PromptCacheMinBytes < 0 {
		return fmt.Errorf("cache.prompt_cache_min_bytes cannot be negative")
	}
	cfg.Cache.CompressionMode = strings.ToLower(cfg.Cache.CompressionMode)
	if cfg.Cache.CompressionMode != "safe" && cfg.Cache.CompressionMode != "aggressive" {
		return fmt.Errorf("cache.compression_mode must be safe or aggressive")
	}

	switch strings.ToLower(cfg.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level must be one of debug, info, warn, or error")
	}

	return nil
}

// splitModelList reads a comma-separated model list from an environment variable, which is
// how a list has to arrive when there is no YAML to carry it.
func splitModelList(value string) []string {
	return trimModelList(strings.Split(value, ","))
}

// trimModelList drops blanks and surrounding space so an empty or sloppily written list
// behaves as no list rather than as a list containing "".
func trimModelList(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
